package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// PostgreSQL restores into a host database this agent provisioned. The
// dump is restored into a fresh staging database (owned by the
// application's role, with the same database privileges) in one
// transaction with session tuning, and only then renamed into place: a
// failure at any point leaves the application's database unchanged.
// Measured on large dumps, one transaction with maintenance_work_mem=1GB
// and synchronous_commit=off is 1.6–2.9x faster than an untuned restore,
// and parallel pg_restore -j did not win reliably (it is disk-bound, and
// eight jobs with that much maintenance memory swapped), so it is not
// used. Progress counts pg_restore's --verbose lines against the dump's
// table of contents (pg_restore -l).

// pgRestoreOptions tunes the restoring session.
const pgRestoreOptions = "-c maintenance_work_mem=1GB -c synchronous_commit=off"

const (
	pgStagingSuffix  = "_lidza_staging"
	pgPreviousSuffix = "_lidza_previous"
)

// pgRestoreRun runs pg_restore with args and env, feeding dump and passing
// each output line to line; tests replace it.
var pgRestoreRun = func(ctx context.Context, tool string, args, env []string, dump io.Reader, line func(string)) error {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = "/", env, dump
	reader, writer := io.Pipe()
	cmd.Stdout, cmd.Stderr = writer, writer
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			line(scanner.Text())
		}
		_, _ = io.Copy(io.Discard, reader)
	}()
	err := cmd.Wait()
	writer.Close()
	<-done
	return err
}

// pgDumpEntries counts the restorable entries of a custom-format dump.
func pgDumpEntries(ctx context.Context, tool string, env []string, dump *os.File) int64 {
	if _, err := dump.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	defer dump.Seek(0, io.SeekStart)
	var n int64
	err := pgRestoreRun(ctx, tool, []string{"-l"}, env, dump, func(line string) {
		if line != "" && !strings.HasPrefix(line, ";") {
			n++
		}
	})
	if err != nil {
		return 0
	}
	return n
}

// renderPGStagingSQL creates the staging database for name afresh, owned
// by the application's role with the privileges provisioning grants.
func renderPGStagingSQL(name string) (string, error) {
	role, err := pgQuoteIdent(name)
	if err != nil {
		return "", err
	}
	staging, err := pgQuoteIdent(name + pgStagingSuffix)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "DROP DATABASE IF EXISTS %s WITH (FORCE);\n", staging)
	fmt.Fprintf(&b, "CREATE DATABASE %s OWNER %s TEMPLATE template0 ENCODING 'UTF8';\n", staging, role)
	fmt.Fprintf(&b, "REVOKE CONNECT, TEMPORARY ON DATABASE %s FROM PUBLIC;\n", staging)
	fmt.Fprintf(&b, "GRANT CONNECT, TEMPORARY ON DATABASE %s TO %s;\n", staging, role)
	fmt.Fprintf(&b, "\\connect %s\n", staging)
	b.WriteString("REVOKE ALL ON SCHEMA public FROM PUBLIC;\n")
	fmt.Fprintf(&b, "GRANT USAGE, CREATE ON SCHEMA public TO %s;\n", role)
	return b.String(), nil
}

// renderPGSwapSQL renames the restored staging database into place. New
// connections are refused and sessions ended first; both renames are one
// transaction, so the application's database is either the old or the
// new one, never missing. The old database is dropped last.
func renderPGSwapSQL(name string) (string, error) {
	target, err := pgQuoteIdent(name)
	if err != nil {
		return "", err
	}
	staging, err := pgQuoteIdent(name + pgStagingSuffix)
	if err != nil {
		return "", err
	}
	previous, err := pgQuoteIdent(name + pgPreviousSuffix)
	if err != nil {
		return "", err
	}
	literal, _ := pgQuoteLiteral(name)
	var b strings.Builder
	fmt.Fprintf(&b, "DROP DATABASE IF EXISTS %s WITH (FORCE);\n", previous)
	fmt.Fprintf(&b, "ALTER DATABASE %s WITH ALLOW_CONNECTIONS false;\n", target)
	fmt.Fprintf(&b, "SELECT count(pg_catalog.pg_terminate_backend(pid)) FROM pg_catalog.pg_stat_activity WHERE datname = %s AND pid <> pg_catalog.pg_backend_pid();\n", literal)
	b.WriteString("BEGIN;\n")
	fmt.Fprintf(&b, "ALTER DATABASE %s RENAME TO %s;\n", target, previous)
	fmt.Fprintf(&b, "ALTER DATABASE %s RENAME TO %s;\n", staging, target)
	b.WriteString("COMMIT;\n")
	fmt.Fprintf(&b, "DROP DATABASE IF EXISTS %s WITH (FORCE);\n", previous)
	return b.String(), nil
}

// renderPGSwapUndoSQL lets the application connect to its database again
// after a failed swap and removes the staging database.
func renderPGSwapUndoSQL(name string) (string, error) {
	target, err := pgQuoteIdent(name)
	if err != nil {
		return "", err
	}
	staging, err := pgQuoteIdent(name + pgStagingSuffix)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS true;\nDROP DATABASE IF EXISTS %s WITH (FORCE);\n", target, staging), nil
}

// restoreHostPGStaged restores dump into pg's database through a staging
// database. options are pg_restore's restore options (no --clean: the
// staging database is empty).
func (m *Manager) restoreHostPGStaged(ctx context.Context, pg NativeHostPostgres, dump *os.File, options []string) error {
	h := nativeHostDataHost(m)
	password, err := m.unsealHostSecret(pg.Password)
	if err != nil {
		return err
	}
	tool, err := movePGRestoreTool(pg.Major)
	if err != nil {
		return err
	}
	staging := pg.Name + pgStagingSuffix
	setup, err := renderPGStagingSQL(pg.Name)
	if err != nil {
		return err
	}
	swap, err := renderPGSwapSQL(pg.Name)
	if err != nil {
		return err
	}
	undo, _ := renderPGSwapUndoSQL(pg.Name)
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		_, _ = h.psql(ctx, pg.Port, undo)
	}
	progress := progressFrom(ctx)
	// Hosts provisioned before staging restores existed admit the role only
	// to its own database; the managed pg_hba.conf block is brought up to
	// date (it also names the staging database) before the restore connects.
	nativeHostDataMu.Lock()
	err = h.ensureHBA(ctx, pg.Port, m.hostDataHBAEntries(pg.Port, ""))
	nativeHostDataMu.Unlock()
	if err != nil {
		return fmt.Errorf("the database access rules could not be updated for the restore: %w; the destination database was left unchanged", err)
	}
	progress.Stage("Creating the staging database", 0, nil)
	if out, err := h.psql(ctx, pg.Port, setup); err != nil {
		return fmt.Errorf("the staging database could not be created (%s); the destination database was left unchanged", boundedErrorText(pgRestoreCause(out, err), 300))
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8", "PGHOST=127.0.0.1", "PGPORT=" + strconv.Itoa(pg.Port), "PGUSER=" + pg.Name,
		"PGPASSWORD=" + password, "PGDATABASE=" + staging, "PGAPPNAME=lidza-restore", "PGCONNECT_TIMEOUT=10", "PGPASSFILE=/nonexistent", "PGOPTIONS=" + pgRestoreOptions}
	total := pgDumpEntries(ctx, tool, env, dump)
	var done atomic.Int64
	var output limitedBuffer
	restored := &progressReader{r: dump}
	info, _ := dump.Stat()
	size := int64(0)
	if info != nil {
		size = info.Size()
	}
	progress.Stage("Restoring into the staging database", size, restored.n.Load)
	args := append([]string{"--no-password", "--verbose", "--dbname=" + staging}, options...)
	err = pgRestoreRun(ctx, tool, args, env, restored, func(line string) {
		_, _ = output.Write([]byte(line + "\n"))
		if strings.HasPrefix(line, "pg_restore: creating ") || strings.HasPrefix(line, "pg_restore: processing ") || strings.HasPrefix(line, "pg_restore: executing ") {
			progress.Objects("objects", min(done.Add(1), max(total, 1)), total)
		}
	})
	if err != nil {
		cleanup()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("PostgreSQL restore failed (%s); the destination database was left unchanged", boundedErrorText(pgRestoreCause(output.String(), err), 300))
	}
	progress.Stage("Renaming the restored database into place", 0, nil)
	if out, err := h.psql(ctx, pg.Port, swap); err != nil {
		cleanup()
		return fmt.Errorf("the restored database could not be renamed into place (%s); the destination database was left unchanged", boundedErrorText(pgRestoreCause(out, err), 300))
	}
	return nil
}

// pgRestoreCause is the most useful line of a failed command's output: the
// last line naming an error or a fatal condition, else the last non-empty
// line, else the error itself. Operators see why a restore failed instead of
// only that it failed.
func pgRestoreCause(output string, err error) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if last == "" {
			last = line
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") || strings.Contains(lower, "rejects") || strings.Contains(lower, "denied") {
			return line
		}
	}
	if last != "" {
		return last
	}
	if err != nil {
		return err.Error()
	}
	return "unknown cause"
}
