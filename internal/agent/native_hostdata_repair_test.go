package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// pgHost is a host with one PostgreSQL 17 cluster whose data directory lies
// under /mnt/data, with switches for what is wrong with it.
type pgHost struct {
	mu       sync.Mutex
	status   string // pg_lsclusters status column
	owner    string
	unit     string // systemd ActiveState
	closed   map[string]bool
	startErr string
	commands []string
}

func (p *pgHost) run(_ context.Context, _ string, name string, args ...string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	p.commands = append(p.commands, line)
	switch {
	case name == "/usr/bin/pg_lsclusters":
		return "17 main 5432 " + p.status + " " + p.owner + " /mnt/data/pg /var/log/postgresql/postgresql-17-main.log\n", nil
	case name == "/usr/bin/systemctl" && args[0] == "show":
		return p.unit + "\n", nil
	case name == "/usr/sbin/runuser" && slices_contains(args, "/usr/bin/test"):
		if p.closed[args[len(args)-1]] {
			return "", errors.New("exit status 1")
		}
	case name == "/usr/bin/setfacl" || name == "/usr/bin/chmod":
		delete(p.closed, args[len(args)-1])
	case name == "/usr/bin/systemctl" && args[0] == "start":
		if p.startErr != "" {
			return p.startErr, errors.New("exit status 1")
		}
		if len(p.closed) == 0 {
			p.status, p.unit = "online", "active"
		}
	case name == "/usr/bin/journalctl":
		return "FATAL: could not open file \"global/pg_control\"\n", nil
	}
	return "", nil
}

func slices_contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func (p *pgHost) ran(prefix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.commands {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func newPGHost(t *testing.T, setfacl bool) (nativeHostData, *pgHost) {
	t.Helper()
	root := t.TempDir()
	tools := []string{"usr/bin/pg_lsclusters"}
	if setfacl {
		tools = append(tools, "usr/bin/setfacl")
	}
	for _, name := range tools {
		path := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755)
	}
	p := &pgHost{status: "down", owner: "postgres", unit: "failed", closed: map[string]bool{}}
	return nativeHostData{root: root, run: p.run}, p
}

func TestPostgresWithAClosedDataDirectoryIsRepairedBeforeItIsNeeded(t *testing.T) {
	ctx := context.Background()
	h, p := newPGHost(t, true)
	p.closed["/mnt/data"] = true // the mount point was closed to everyone but its owner
	problem := h.diagnosePostgres(ctx)
	if problem == nil || problem.Fix != "access" || !strings.Contains(problem.Summary, `PostgreSQL 17 cluster "main" (port 5432) cannot start: the postgres account cannot enter /mnt/data, which leads to its data directory /mnt/data/pg`) {
		t.Fatalf("diagnosis %+v", problem)
	}
	// Creating an application's database repairs it instead of failing.
	cluster, err := h.ensurePostgres(ctx)
	if err != nil || !cluster.Online || cluster.Major != 17 {
		t.Fatalf("ensurePostgres: %v %+v", err, cluster)
	}
	// Only the postgres account is let in, and only into that directory.
	if p.ran("/usr/bin/setfacl -m u:postgres:x /mnt/data") != 1 || p.ran("/usr/bin/chmod") != 0 || p.ran("/usr/bin/systemctl start postgresql@17-main.service") != 1 {
		t.Fatalf("repair commands %v", p.commands)
	}
	if h.diagnosePostgres(ctx) != nil {
		t.Fatal("a repaired cluster is still reported")
	}
	// Without the ACL tool the search bit for others is the fallback.
	h, p = newPGHost(t, false)
	p.closed["/mnt/data"] = true
	if _, err := h.ensurePostgres(ctx); err != nil || p.ran("/usr/bin/chmod o+x /mnt/data") != 1 {
		t.Fatalf("fallback: %v %v", err, p.commands)
	}
}

func TestPostgresProblemsTheAgentMustNotDecideAreNamedAndLeftAlone(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		edit func(*pgHost)
		want string
	}{
		"stopped on purpose": {func(p *pgHost) { p.unit = "inactive" }, "is stopped and its service did not fail, so it was stopped on purpose"},
		"standby":            {func(p *pgHost) { p.status, p.unit = "online,recovery", "active" }, "is a standby (in recovery)"},
		"another owner":      {func(p *pgHost) { p.owner = "admin" }, "is owned by admin, not by postgres"},
	} {
		h, p := newPGHost(t, true)
		tc.edit(p)
		problem := h.diagnosePostgres(ctx)
		if problem == nil || problem.Fix != "" || !strings.Contains(problem.Summary, tc.want) || !strings.Contains(problem.Summary, `cluster "main"`) {
			t.Fatalf("%s: diagnosis %+v", name, problem)
		}
		if _, err := h.ensurePostgres(ctx); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: ensurePostgres %v", name, err)
		}
		if p.ran("/usr/bin/systemctl start") != 0 || p.ran("/usr/bin/setfacl") != 0 || p.ran("/usr/bin/chmod") != 0 || p.ran("/usr/bin/apt-get") != 0 {
			t.Fatalf("%s: the cluster was touched: %v", name, p.commands)
		}
	}
	// A failed service that still does not start says so with the service's own words.
	h, p := newPGHost(t, true)
	p.startErr = "Job for postgresql@17-main.service failed"
	if _, err := h.ensurePostgres(ctx); err == nil || !strings.Contains(err.Error(), "did not start") || !strings.Contains(err.Error(), "pg_control") {
		t.Fatalf("failed start: %v", err)
	}
}
