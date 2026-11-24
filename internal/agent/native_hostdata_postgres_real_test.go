package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realPostgres starts a throwaway PostgreSQL cluster from the server
// binaries installed on this machine, or skips. The provisioning and release
// scripts are what the agent runs on a host, so they are proven against the
// real server: fakes cannot tell whether a role can be dropped.
func realPostgres(t *testing.T, bin string) (psql func(script string) (string, error)) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lidza-pg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	data := filepath.Join(dir, "data")
	if out, err := exec.Command(filepath.Join(bin, "initdb"), "-D", data, "-A", "trust", "-U", "postgres", "--no-sync").CombinedOutput(); err != nil {
		t.Skipf("initdb is not usable here: %v\n%s", err, out)
	}
	start := exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-w", "-t", "60", "-o", "-c listen_addresses='' -c unix_socket_directories="+dir+" -c fsync=off", "-l", filepath.Join(dir, "log"), "start")
	if out, err := start.CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "log"))
		t.Skipf("PostgreSQL did not start here: %v\n%s\n%s", err, out, log)
	}
	t.Cleanup(func() { _ = exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-m", "immediate", "stop").Run() })
	return func(script string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, filepath.Join(bin, "psql"), "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-h", dir, "-U", "postgres", "-d", "postgres")
		cmd.Stdin = strings.NewReader(script)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return strings.TrimSpace(out.String()), err
	}
}

// What the agent provisions it must be able to remove again: the role holds
// a grant on a configuration parameter, and a role that holds any grant
// cannot be dropped until it is revoked.
func TestProvisionedPostgresDatabaseAndRoleAreRemovedOnARealServer(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("PostgreSQL refuses to run as root")
	}
	candidates, _ := filepath.Glob("/usr/lib/postgresql/*/bin/initdb")
	if len(candidates) == 0 {
		t.Skip("no PostgreSQL server binaries on this machine")
	}
	// Every installed major version: parameter grants exist from 15 on.
	for _, initdb := range candidates {
		bin := filepath.Dir(initdb)
		t.Run("PostgreSQL "+filepath.Base(filepath.Dir(bin)), func(t *testing.T) { provisionedPostgresIsRemoved(t, realPostgres(t, bin)) })
	}
}

func provisionedPostgresIsRemoved(t *testing.T, psql func(script string) (string, error)) {
	provision, err := renderNativePGProvisionSQL("app_shop", "SCRAM-SHA-256$4096:c2FsdHNhbHRzYWx0c2FsdA==$"+strings.Repeat("A", 43)+"=:"+strings.Repeat("B", 43)+"=")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := psql(provision); err != nil {
		t.Fatalf("provisioning: %v\n%s", err, out)
	}
	state := func() string {
		out, err := psql("SELECT (SELECT count(*) FROM pg_roles WHERE rolname = 'app_shop') || ' roles, ' || (SELECT count(*) FROM pg_database WHERE datname = 'app_shop') || ' databases';")
		if err != nil {
			t.Fatal(err, out)
		}
		return out
	}
	if got := state(); got != "1 roles, 1 databases" {
		t.Fatalf("after provisioning: %s", got)
	}
	// The grant that used to keep the role from being dropped is really there.
	if out, err := psql("SELECT count(*) FROM pg_parameter_acl WHERE paracl::text LIKE '%app_shop%';"); err != nil || out != "1" {
		t.Fatalf("the role's parameter grant: %q %v", out, err)
	}
	if out, err := psql("DROP ROLE \"app_shop\";"); err == nil || !strings.Contains(out, "cannot be dropped") {
		t.Fatalf("a plain DROP ROLE was expected to be refused: %q %v", out, err)
	}
	release, err := renderNativePGReleaseSQL("app_shop")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := psql(release); err != nil {
		t.Fatalf("removing what was provisioned failed: %v\n%s", err, out)
	}
	if got := state(); got != "0 roles, 0 databases" {
		t.Fatalf("after the removal: %s", got)
	}
	// What an earlier, failed removal left: the database is gone and the
	// role, still holding its grant, is not. A second removal finishes it.
	if out, err := psql(provision + "\\connect postgres\nDROP DATABASE \"app_shop\" WITH (FORCE);\n"); err != nil {
		t.Fatalf("recreating the leftover: %v\n%s", err, out)
	}
	if got := state(); got != "1 roles, 0 databases" {
		t.Fatalf("the leftover: %s", got)
	}
	if out, err := psql(release); err != nil {
		t.Fatalf("removing a leftover role failed: %v\n%s", err, out)
	}
	if got := state(); got != "0 roles, 0 databases" {
		t.Fatalf("after removing the leftover: %s", got)
	}
	// And a removal of nothing is not an error.
	if out, err := psql(release); err != nil {
		t.Fatalf("removing what is already gone failed: %v\n%s", err, out)
	}
}
