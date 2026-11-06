package onboarding

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza/pkg/credentials"
)

// fakeDocker installs a docker command that logs every invocation and
// answers from the given shell case body. The log path is baked in because
// docker() passes only a fixed set of environment variables on.
func fakeDocker(t *testing.T, cases string) string {
	t.Helper()
	bin := t.TempDir()
	logPath := filepath.Join(bin, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\ncase \"$1\" in\n" + cases + "\nesac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return logPath
}

func dockerCalls(t *testing.T, logPath string) []string {
	t.Helper()
	raw, _ := os.ReadFile(logPath)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	return port
}

func narrowPortRange(t *testing.T) {
	t.Helper()
	previous := managedDatabasePorts
	first, _ := strconv.Atoi(closedPort(t))
	managedDatabasePorts = [2]int{first, first + 3}
	t.Cleanup(func() { managedDatabasePorts = previous })
}

func TestManagedDatabasePortIsFixedAndSaved(t *testing.T) {
	cleanSetupEnv(t)
	dir := t.TempDir()
	if _, err := credentials.Generate(dir); err != nil {
		t.Fatal(err)
	}
	narrowPortRange(t)
	s := &Setup{opts: Options{Dir: dir}}
	// The first port of the range is busy: the next free one is chosen and saved.
	busy, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(managedDatabasePorts[0])))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port, err := s.managedDatabasePort("")
	if err != nil || port != strconv.Itoa(managedDatabasePorts[0]+1) {
		t.Fatalf("port = %q, %v", port, err)
	}
	saved, _ := credentials.Read(dir)
	if saved["SETUP_DB_PORT"] != port {
		t.Fatalf("SETUP_DB_PORT = %q, want %q", saved["SETUP_DB_PORT"], port)
	}
	// A saved port is kept while it is free, even outside the range.
	outside := closedPort(t)
	if port, err := s.managedDatabasePort(outside); err != nil || port != outside {
		t.Fatalf("saved port not kept: %q, %v", port, err)
	}
	// A saved port that is now busy gives way to a free one.
	if port, err := s.managedDatabasePort(strconv.Itoa(managedDatabasePorts[0])); err != nil || port != strconv.Itoa(managedDatabasePorts[0]+1) {
		t.Fatalf("busy saved port kept: %q, %v", port, err)
	}
}

func TestManagedDatabaseRunsOnFixedLoopbackPort(t *testing.T) {
	cleanSetupEnv(t)
	dir := t.TempDir()
	if _, err := credentials.Generate(dir); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Set(dir, map[string]string{"SETUP_DB_PASSWORD": "sealed-password"}); err != nil {
		t.Fatal(err)
	}
	narrowPortRange(t)
	unreachable := closedPort(t)
	logPath := fakeDocker(t, `
  ps) echo "" ;;
  run) echo 0123456789ab ;;
  port) echo "127.0.0.1:`+unreachable+`" ;;
  exec) exit 0 ;;`)
	s := &Setup{opts: Options{Dir: dir}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Nothing listens on the fake address, so the run ends at the
	// connection check; the container command is what this test is about.
	if _, err := s.managedDatabase(ctx); err == nil || !strings.Contains(err.Error(), "could not connect to PostgreSQL") {
		t.Fatalf("expected the connection check to fail, got %v", err)
	}
	saved, _ := credentials.Read(dir)
	port := saved["SETUP_DB_PORT"]
	if port != strconv.Itoa(managedDatabasePorts[0]) {
		t.Fatalf("SETUP_DB_PORT = %q", port)
	}
	var run string
	for _, call := range dockerCalls(t, logPath) {
		if strings.HasPrefix(call, "run ") {
			run = call
		}
	}
	if !strings.Contains(run, "--publish 127.0.0.1:"+port+":5432 ") || strings.Contains(run, "127.0.0.1::5432") {
		t.Fatalf("container must publish a fixed loopback port: %s", run)
	}
	if !strings.Contains(run, "io.lidza.control.database=") || !strings.Contains(run, managedDatabaseImage) {
		t.Fatalf("run command = %s", run)
	}
}

func TestManagedDatabaseRecreatesEphemeralPortContainer(t *testing.T) {
	cleanSetupEnv(t)
	dir := t.TempDir()
	if _, err := credentials.Generate(dir); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Set(dir, map[string]string{"SETUP_DB_PASSWORD": "sealed-password"}); err != nil {
		t.Fatal(err)
	}
	narrowPortRange(t)
	id, name := managedDatabaseName(dir)
	unreachable := closedPort(t)
	logPath := fakeDocker(t, `
  ps) echo 0123456789ab ;;
  inspect) case "$*" in *io.lidza.control.database*) echo `+id+` ;; *) echo "" ;; esac ;;
  run) echo fedcba987654 ;;
  port) echo "127.0.0.1:`+unreachable+`" ;;
  exec) exit 0 ;;`)
	s := &Setup{opts: Options{Dir: dir}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := s.managedDatabase(ctx); err == nil || !strings.Contains(err.Error(), "could not connect to PostgreSQL") {
		t.Fatalf("expected the connection check to fail, got %v", err)
	}
	calls := strings.Join(dockerCalls(t, logPath), "\n")
	for _, want := range []string{"stop --time 60 " + name, "rm " + name, "--publish 127.0.0.1:" + strconv.Itoa(managedDatabasePorts[0]) + ":5432", "type=volume,src=" + name + ",dst=/var/lib/postgresql/data"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("docker calls lack %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "start "+name) {
		t.Fatalf("an ephemeral-port container is replaced, not started:\n%s", calls)
	}
	if strings.Contains(calls, "volume rm") || strings.Contains(calls, "rm -f") || strings.Contains(calls, "rm -v") {
		t.Fatalf("the data volume must survive:\n%s", calls)
	}
}

func TestManagedDatabaseKeepsFixedPortContainer(t *testing.T) {
	cleanSetupEnv(t)
	dir := t.TempDir()
	if _, err := credentials.Generate(dir); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Set(dir, map[string]string{"SETUP_DB_PASSWORD": "sealed-password"}); err != nil {
		t.Fatal(err)
	}
	id, name := managedDatabaseName(dir)
	unreachable := closedPort(t)
	logPath := fakeDocker(t, `
  ps) echo 0123456789ab ;;
  inspect) case "$*" in *io.lidza.control.database*) echo `+id+` ;; *) echo 5433 ;; esac ;;
  port) echo "127.0.0.1:`+unreachable+`" ;;
  exec) exit 0 ;;`)
	s := &Setup{opts: Options{Dir: dir}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := s.managedDatabase(ctx); err == nil || !strings.Contains(err.Error(), "could not connect to PostgreSQL") {
		t.Fatalf("expected the connection check to fail, got %v", err)
	}
	calls := strings.Join(dockerCalls(t, logPath), "\n")
	if !strings.Contains(calls, "start "+name) || strings.Contains(calls, "rm "+name) || strings.Contains(calls, "run ") {
		t.Fatalf("a fixed-port container is started as it is:\n%s", calls)
	}
}

func TestDatabaseWatchFollowsContainerPort(t *testing.T) {
	cleanSetupEnv(t)
	dir := t.TempDir()
	if _, err := credentials.Generate(dir); err != nil {
		t.Fatal(err)
	}
	stale := "postgres://lidza:secret@127.0.0.1:40001/lidza_control?sslmode=disable"
	if err := credentials.Set(dir, map[string]string{"SETUP_DB_PASSWORD": "sealed-password", "DATABASE_URL": stale}); err != nil {
		t.Fatal(err)
	}
	id, _ := managedDatabaseName(dir)
	fakeDocker(t, `
  ps) echo 0123456789ab ;;
  inspect) echo `+id+` ;;
  port) echo "127.0.0.1:40777" ;;`)
	restarted := make(chan struct{})
	var logs []string
	w := databaseWatch{dir: dir, interval: 5 * time.Millisecond, threshold: 2,
		probe:   func(context.Context, string) error { return errors.New("connection refused") },
		restart: func() { close(restarted) },
		logf:    func(format string, args ...any) { logs = append(logs, format) }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go w.run(ctx)
	select {
	case <-restarted:
	case <-ctx.Done():
		t.Fatal("the watch did not restart the panel onto the new port")
	}
	saved, _ := credentials.Read(dir)
	u, err := url.Parse(saved["DATABASE_URL"])
	if err != nil || u.Host != "127.0.0.1:40777" || u.User.Username() != "lidza" || u.Path != "/lidza_control" {
		t.Fatalf("DATABASE_URL = %q", saved["DATABASE_URL"])
	}
	if password, _ := u.User.Password(); password != "secret" {
		t.Fatal("the password must survive the address change")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "moved from") {
		t.Fatalf("logs = %v", logs)
	}
}

func TestDatabaseWatchWaitsWhenAddressIsUnchanged(t *testing.T) {
	cleanSetupEnv(t)
	dir := t.TempDir()
	if _, err := credentials.Generate(dir); err != nil {
		t.Fatal(err)
	}
	dsn := "postgres://lidza:secret@127.0.0.1:40777/lidza_control?sslmode=disable"
	if err := credentials.Set(dir, map[string]string{"SETUP_DB_PASSWORD": "sealed-password", "DATABASE_URL": dsn}); err != nil {
		t.Fatal(err)
	}
	id, _ := managedDatabaseName(dir)
	fakeDocker(t, `
  ps) echo 0123456789ab ;;
  inspect) echo `+id+` ;;
  port) echo "127.0.0.1:40777" ;;`)
	restarted := false
	var logs []string
	w := databaseWatch{dir: dir, interval: 5 * time.Millisecond, threshold: 2,
		probe:   func(context.Context, string) error { return errors.New("connection refused") },
		restart: func() { restarted = true },
		logf:    func(format string, args ...any) { logs = append(logs, format) }}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	w.run(ctx)
	if restarted {
		t.Fatal("a database that is down on its usual address is waited for, not restarted onto")
	}
	saved, _ := credentials.Read(dir)
	if saved["DATABASE_URL"] != dsn {
		t.Fatalf("DATABASE_URL changed to %q", saved["DATABASE_URL"])
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "waiting for it") {
		t.Fatalf("the wait is logged once: %v", logs)
	}
}

func TestDatabaseWatchIgnoresExternalDatabases(t *testing.T) {
	cleanSetupEnv(t)
	dir := t.TempDir()
	if _, err := credentials.Generate(dir); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Set(dir, map[string]string{"DATABASE_URL": "postgres://external.example/control"}); err != nil {
		t.Fatal(err)
	}
	probes := 0
	w := databaseWatch{dir: dir, interval: 5 * time.Millisecond, threshold: 1,
		probe:   func(context.Context, string) error { probes++; return errors.New("down") },
		restart: func() { t.Fatal("external databases are never restarted onto") },
		logf:    func(string, ...any) {}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	w.run(ctx)
	if probes != 0 {
		t.Fatal("external databases are not probed")
	}
}
