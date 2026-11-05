package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Dumper produces and restores one database's logical dump. The agent's
// existing backup pipeline can satisfy this interface so that migration
// dumps are retained and verified off-site like scheduled backups.
type Dumper interface {
	// Dump writes a complete dump of the database at url to path.
	Dump(ctx context.Context, url, path string) error
	// Restore loads the dump at path into the (existing, empty or
	// disposable) database at url, replacing objects that already exist.
	Restore(ctx context.Context, url, path string) error
}

// Postgres dumps with pg_dump in custom format and restores with pg_restore.
type Postgres struct {
	Bin string // directory holding pg_dump and pg_restore; "" uses PATH
}

func (p Postgres) tool(name string) string {
	if p.Bin == "" {
		return name
	}
	return filepath.Join(p.Bin, name)
}

func validDatabaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return errors.New("migrate: database URL must be postgres://user@host:port/name")
	}
	return nil
}

func (p Postgres) run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, p.tool(name), args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "PGCONNECT_TIMEOUT=15"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("migrate: %s: %w: %s", name, err, tail(string(out)))
	}
	return nil
}

// Dump writes a custom-format archive. The connection URL is passed as a
// --dbname argument rather than through the environment so that no shell
// or log sees it expanded.
func (p Postgres) Dump(ctx context.Context, url, path string) error {
	if err := validDatabaseURL(url); err != nil {
		return err
	}
	temporary := path + ".partial"
	if err := p.run(ctx, "pg_dump", "--format=custom", "--compress=6", "--no-owner", "--no-privileges",
		"--file="+temporary, "--dbname="+url); err != nil {
		os.Remove(temporary)
		return err
	}
	// pg_restore --list parses the archive table of contents; a truncated
	// or corrupt dump fails here instead of at restore time on the target.
	if err := p.run(ctx, "pg_restore", "--list", temporary); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("migrate: dump verification failed: %w", err)
	}
	return os.Rename(temporary, path)
}

// Restore loads the archive into url. Existing objects are dropped first so
// a rehearsal restore can be repeated into the same target database.
func (p Postgres) Restore(ctx context.Context, url, path string) error {
	if err := validDatabaseURL(url); err != nil {
		return err
	}
	if err := p.run(ctx, "pg_restore", "--list", path); err != nil {
		return fmt.Errorf("migrate: refusing to restore an unreadable dump: %w", err)
	}
	return p.run(ctx, "pg_restore", "--clean", "--if-exists", "--no-owner", "--no-privileges",
		"--exit-on-error", "--single-transaction", "--dbname="+url, path)
}

// Digest returns the SHA-256 of a dump file, recorded in the bundle manifest
// and checked again before restore.
func Digest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
