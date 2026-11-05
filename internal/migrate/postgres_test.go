package migrate

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const postgresBin = "/usr/lib/postgresql/18/bin"

// scratchPostgres starts a disposable cluster owned by the test user on a
// free loopback port and stops it when the test ends.
func scratchPostgres(t *testing.T) (port int) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(postgresBin, "initdb")); err != nil {
		t.Skip("PostgreSQL 18 tools are not installed")
	}
	if os.Geteuid() == 0 {
		t.Skip("scratch PostgreSQL cannot run as root")
	}
	base := os.Getenv("LIDZA_SCRATCH_DIR")
	if base == "" {
		base = t.TempDir()
	}
	dir, err := os.MkdirTemp(base, "pg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	data := filepath.Join(dir, "data")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(filepath.Join(postgresBin, name), args...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("initdb", "-D", data, "--auth=trust", "--username=proof", "--encoding=UTF8", "--locale=C.UTF-8")
	run("pg_ctl", "-D", data, "-l", filepath.Join(dir, "log"), "-w", "-o",
		// Loopback only: a Unix socket under a deep scratch path exceeds the
		// socket path limit, and the tests connect over TCP anyway.
		fmt.Sprintf("-h 127.0.0.1 -p %d -c unix_socket_directories= -c shared_buffers=16MB -c max_connections=10", port), "start")
	t.Cleanup(func() {
		cmd := exec.Command(filepath.Join(postgresBin, "pg_ctl"), "-D", data, "-m", "fast", "-w", "stop")
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		cmd.Run()
	})
	return port
}

func psql(t *testing.T, url, sql string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(postgresBin, "psql"), "--no-psqlrc", "--tuples-only", "--no-align", "--quiet",
		"-v", "ON_ERROR_STOP=1", "--dbname="+url, "--command="+sql)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("psql %q: %v\n%s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestPostgresDumpVerifiesAndRestores(t *testing.T) {
	port := scratchPostgres(t)
	admin := fmt.Sprintf("postgres://proof@127.0.0.1:%d/postgres", port)
	psql(t, admin, "CREATE DATABASE source_app")
	psql(t, admin, "CREATE DATABASE target_app")
	source := fmt.Sprintf("postgres://proof@127.0.0.1:%d/source_app", port)
	target := fmt.Sprintf("postgres://proof@127.0.0.1:%d/target_app", port)
	psql(t, source, "CREATE TABLE puzzles(id serial PRIMARY KEY, word text NOT NULL); INSERT INTO puzzles(word) SELECT 'w'||g FROM generate_series(1,250) g; CREATE INDEX ON puzzles(word)")
	// Stale objects on the target must be replaced, not merged.
	psql(t, target, "CREATE TABLE puzzles(id int PRIMARY KEY); INSERT INTO puzzles VALUES (999)")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tools := Postgres{Bin: postgresBin}
	dump := filepath.Join(t.TempDir(), "primary.dump")
	if err := tools.Dump(ctx, source, dump); err != nil {
		t.Fatal(err)
	}
	if exists(dump + ".partial") {
		t.Fatal("partial dump left behind")
	}
	digest, err := Digest(dump)
	if err != nil || len(digest) != 64 {
		t.Fatal(err)
	}
	if err := tools.Restore(ctx, target, dump); err != nil {
		t.Fatal(err)
	}
	if got := psql(t, target, "SELECT count(*), min(word), max(id) FROM puzzles"); got != "250|w1|250" {
		t.Fatalf("restored data %q", got)
	}
	// A second restore (rehearsal repeated) succeeds and yields the same rows.
	if err := tools.Restore(ctx, target, dump); err != nil {
		t.Fatal("repeat restore", err)
	}
	if got := psql(t, target, "SELECT count(*) FROM puzzles"); got != "250" {
		t.Fatalf("repeat restore data %q", got)
	}
	os.WriteFile(dump, []byte("not a dump"), 0o600)
	if err := tools.Restore(ctx, target, dump); err == nil {
		t.Fatal("corrupt dump was restored")
	}
	if err := tools.Dump(ctx, "mysql://x/y", dump); err == nil {
		t.Fatal("non-postgres URL accepted")
	}
}
