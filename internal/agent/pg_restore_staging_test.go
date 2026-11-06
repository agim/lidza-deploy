package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// pgStagingFixture records the psql scripts and pg_restore runs of a host
// database restore.
type pgStagingFixture struct {
	mu       sync.Mutex
	scripts  []string
	restores [][]string
	envs     [][]string
	failRun  bool
	failSwap bool
}

func newPGStagingFixture(t *testing.T) (*Manager, *pgStagingFixture, NativeHostPostgres) {
	t.Helper()
	fx := &pgStagingFixture{}
	m := &Manager{cfg: Config{DataDir: t.TempDir()}, key: bytes.Repeat([]byte{7}, 32), ctx: context.Background()}
	sealed, err := m.sealHostSecret("role-password")
	if err != nil {
		t.Fatal(err)
	}
	previousHost, previousTool, previousRun := nativeHostDataHost, movePGRestoreTool, pgRestoreRun
	// The restore refreshes the managed pg_hba.conf block first; the host
	// answers those probes like a real cluster and keeps them out of the
	// recorded SQL scripts.
	root := t.TempDir()
	hba := filepath.Join(root, "etc/postgresql/17/main/pg_hba.conf")
	if err := os.MkdirAll(filepath.Dir(hba), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hba, []byte(nativeHBAFixture), 0o640); err != nil {
		t.Fatal(err)
	}
	nativeHostDataHost = func(*Manager) nativeHostData {
		return nativeHostData{root: root, run: func(_ context.Context, stdin, name string, args ...string) (string, error) {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			switch {
			case strings.HasPrefix(stdin, "SHOW hba_file"):
				return "/etc/postgresql/17/main/pg_hba.conf\n", nil
			case strings.Contains(stdin, "pg_hba_file_rules"):
				return "0\n", nil
			case strings.Contains(stdin, "pg_reload_conf"):
				return "t\n", nil
			}
			fx.scripts = append(fx.scripts, stdin)
			if fx.failSwap && strings.Contains(stdin, "RENAME TO") {
				return "ERROR: database is being accessed by other users", errors.New("exit status 3")
			}
			return "", nil
		}}
	}
	movePGRestoreTool = func(int) (string, error) { return "/usr/lib/postgresql/17/bin/pg_restore", nil }
	pgRestoreRun = func(_ context.Context, _ string, args, env []string, dump io.Reader, line func(string)) error {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		if slices.Contains(args, "-l") {
			for _, entry := range []string{";", "; Archive created", "215; 1259 16390 TABLE public events app", "216; 0 16390 TABLE DATA public events app", "217; 1259 16391 INDEX public events_pkey app"} {
				line(entry)
			}
			return nil
		}
		_, _ = io.Copy(io.Discard, dump)
		fx.restores, fx.envs = append(fx.restores, args), append(fx.envs, env)
		line("pg_restore: creating TABLE \"public.events\"")
		line("pg_restore: processing data for table \"public.events\"")
		line("pg_restore: creating INDEX \"public.events_pkey\"")
		if fx.failRun {
			line("pg_restore: error: connection to server at \"127.0.0.1\", port 5432 failed: FATAL:  pg_hba.conf rejects connection for host \"127.0.0.1\"")
			return errors.New("exit status 1")
		}
		return nil
	}
	t.Cleanup(func() { nativeHostDataHost, movePGRestoreTool, pgRestoreRun = previousHost, previousTool, previousRun })
	return m, fx, NativeHostPostgres{Name: "app_shop", Resource: "shop-db0001", Port: 5432, Major: 17, Password: sealed}
}

func pgDumpFile(t *testing.T) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dump")
	if err := os.WriteFile(path, []byte("PGDMP custom archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestHostPostgresRestoreGoesThroughStagingAndSwaps(t *testing.T) {
	m, fx, pg := newPGStagingFixture(t)
	tracker := NewProgressTracker("restore")
	ctx := withProgress(context.Background(), tracker)
	if err := moveRestoreDump(ctx, m, moveDatabaseTarget{ID: pg.Resource, Host: &pg}, pgDumpFile(t), true, nil); err != nil {
		t.Fatal(err)
	}
	if len(fx.scripts) != 2 || !strings.Contains(fx.scripts[0], `CREATE DATABASE "app_shop_lidza_staging" OWNER "app_shop"`) || !strings.Contains(fx.scripts[0], `GRANT CONNECT, TEMPORARY ON DATABASE "app_shop_lidza_staging" TO "app_shop"`) {
		t.Fatalf("staging script %q", fx.scripts)
	}
	swap := fx.scripts[1]
	for _, want := range []string{`ALTER DATABASE "app_shop" WITH ALLOW_CONNECTIONS false`, "BEGIN;\nALTER DATABASE \"app_shop\" RENAME TO \"app_shop_lidza_previous\";\nALTER DATABASE \"app_shop_lidza_staging\" RENAME TO \"app_shop\";\nCOMMIT;", `DROP DATABASE IF EXISTS "app_shop_lidza_previous" WITH (FORCE)`} {
		if !strings.Contains(swap, want) {
			t.Fatalf("swap script lacks %q:\n%s", want, swap)
		}
	}
	args, env := fx.restores[0], fx.envs[0]
	if !slices.Contains(args, "--single-transaction") || !slices.Contains(args, "--dbname=app_shop_lidza_staging") || slices.Contains(args, "--clean") || slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "-j") || strings.HasPrefix(a, "--jobs") }) {
		t.Fatalf("pg_restore %v", args)
	}
	if !slices.Contains(env, "PGOPTIONS=-c maintenance_work_mem=1GB -c synchronous_commit=off") || slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, "role-password") }) {
		t.Fatalf("environment %v", env)
	}
	if p := tracker.Snapshot(); p.TotalObjects != 3 || p.DoneObjects != 3 {
		t.Fatalf("progress %+v", p)
	}
}

func TestHostPostgresRestoreFailureLeavesTheDatabase(t *testing.T) {
	m, fx, pg := newPGStagingFixture(t)
	fx.failRun = true
	err := moveRestoreDump(context.Background(), m, moveDatabaseTarget{ID: pg.Resource, Host: &pg}, pgDumpFile(t), true, nil)
	if err == nil || !strings.Contains(err.Error(), "left unchanged") || !strings.Contains(err.Error(), "pg_hba.conf rejects connection") {
		t.Fatalf("failed restore must name the cause: %v", err)
	}
	for _, script := range fx.scripts {
		if strings.Contains(script, "RENAME TO") {
			t.Fatal("a failed restore renamed databases")
		}
	}
	if last := fx.scripts[len(fx.scripts)-1]; !strings.Contains(last, `DROP DATABASE IF EXISTS "app_shop_lidza_staging"`) || !strings.Contains(last, `"app_shop" WITH ALLOW_CONNECTIONS true`) {
		t.Fatalf("cleanup %q", last)
	}
	// A swap that fails lets the application connect again.
	fx.failRun, fx.failSwap, fx.scripts = false, true, nil
	if err := moveRestoreDump(context.Background(), m, moveDatabaseTarget{ID: pg.Resource, Host: &pg}, pgDumpFile(t), true, nil); err == nil {
		t.Fatal("a failed swap succeeded")
	}
	if last := fx.scripts[len(fx.scripts)-1]; !strings.Contains(last, "ALLOW_CONNECTIONS true") {
		t.Fatalf("undo %q", fx.scripts)
	}
}
