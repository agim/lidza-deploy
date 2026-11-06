package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const nativeArchiveSocket = "/var/run/postgresql"

func validateNativeArchiveDatabase(id string, p NativeDatabasePolicy) error {
	if !strings.HasPrefix(id, "native-archive-") || p.AppID != "" || p.EnvironmentKey != "" || len(p.Roles) != 0 ||
		p.SQLitePath != "" || p.MaintenancePort != "" || p.Host != "127.0.0.1" || p.Port != "5432" ||
		p.PostgresMajor < 16 || p.PostgresMajor > 18 || !nativePostgresDatabasePath.MatchString("/"+p.ArchiveDatabase) ||
		p.ConfigSHA256 != nativeArchivePeerDigest(p.ArchiveDatabase) || !p.Backup.Offsite || p.Backup.Keep != 1 {
		return errors.New("standalone native backup requires its exact local peer database and off-site policy")
	}
	return p.Backup.Validate()
}

func nativeArchivePeerDigest(database string) string {
	sum := sha256.Sum256([]byte("postgres\x00" + nativeArchiveSocket + "\x005432\x00" + database))
	return hex.EncodeToString(sum[:])
}

func nativeArchiveMetadataURL(database string) string {
	return (&url.URL{Scheme: "postgresql", User: url.User("postgres"), Host: "127.0.0.1:5432", Path: "/" + database}).String()
}

func nativeArchivePeerCredential() (*syscall.Credential, error) {
	account, err := user.Lookup("postgres")
	if err != nil || os.Geteuid() != 0 {
		return nil, errors.New("standalone native backup requires its trusted local PostgreSQL account")
	}
	uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return nil, errors.New("standalone native backup PostgreSQL account identity is invalid")
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}

// runNativeArchiveQuery runs one read-only statement on an archive database
// over the local socket as the postgres account, with the reviewed major's
// psql, and returns its trimmed output. nativeArchiveQuery is what callers
// use; tests replace it.
func runNativeArchiveQuery(ctx context.Context, p NativeDatabasePolicy, sql string) (string, error) {
	if !nativePostgresDatabasePath.MatchString("/"+p.ArchiveDatabase) || p.PostgresMajor < 16 || p.PostgresMajor > 18 {
		return "", errors.New("standalone native backup peer probe requires its reviewed database and toolchain")
	}
	credential, err := nativeArchivePeerCredential()
	if err != nil {
		return "", err
	}
	path := filepath.Join("/usr/lib/postgresql", strconv.Itoa(p.PostgresMajor), "bin/psql")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return "", errors.New("standalone native backup probe tool is unavailable")
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); !ok || owner.Uid != 0 {
		return "", errors.New("standalone native backup probe tool is untrusted")
	}
	command := exec.CommandContext(ctx, path, "-XAtw", "-h", nativeArchiveSocket, "-p", "5432", "-U", "postgres", "-d", p.ArchiveDatabase, "-c", sql)
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8", "PGPASSFILE=/nonexistent", "PGCONNECT_TIMEOUT=5",
		"PGOPTIONS=-c default_transaction_read_only=on -c statement_timeout=5000 -c lock_timeout=1000"}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	var stdout nativeBoundedBuffer
	command.Stdout, command.Stderr = &stdout, &limitedBuffer{}
	if err = command.Run(); err != nil || stdout.exceeded {
		return "", errors.New("standalone native backup read-only peer probe failed")
	}
	return strings.TrimSpace(stdout.String()), nil
}

var nativeArchiveQuery = runNativeArchiveQuery

func probeNativeArchiveDatabase(ctx context.Context, p NativeDatabasePolicy) error {
	out, err := nativeArchiveQuery(ctx, p, "SELECT current_database(), current_setting('server_version_num'), current_setting('transaction_read_only')")
	if err != nil {
		return err
	}
	fields := strings.Split(out, "|")
	if len(fields) != 3 || fields[0] != p.ArchiveDatabase || fields[2] != "on" {
		return errors.New("standalone native backup peer database or read-only state changed")
	}
	if version, err := strconv.Atoi(fields[1]); err != nil || version/10000 != p.PostgresMajor {
		return errors.New("standalone native backup peer PostgreSQL major changed")
	}
	return nil
}

func nativeArchiveMatchesURL(p NativeDatabasePolicy, raw string) bool {
	if p.ArchiveDatabase == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Path != "/"+p.ArchiveDatabase {
		return false
	}
	return u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()
}

// nativeBoundedBuffer keeps at most 512 KiB of command output and records
// whether anything was dropped.
type nativeBoundedBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (buffer *nativeBoundedBuffer) ReadFrom(reader io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{buffer}, reader)
}

func (buffer *nativeBoundedBuffer) Write(raw []byte) (int, error) {
	count := len(raw)
	remaining := 524288 - buffer.Len()
	if count > remaining {
		buffer.exceeded = true
		raw = raw[:remaining]
	}
	_, _ = buffer.Buffer.Write(raw)
	return count, nil
}
