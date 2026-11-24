package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// realPostgresHost wires the agent's host-data code to a throwaway real
// PostgreSQL cluster: the agent's own psql calls run against it, and the
// cluster's own pg_hba.conf is the file the agent edits. Only the cluster
// listing is answered by the test.
func realPostgresHost(t *testing.T, bin string) (m *Manager, psql func(database, script string) (string, error)) {
	t.Helper()
	const port = 5432
	base, err := os.MkdirTemp("/tmp", "lidza-pg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	data := filepath.Join(base, "data")
	if out, err := exec.Command(filepath.Join(bin, "initdb"), "-D", data, "-A", "trust", "-U", "postgres", "--no-sync").CombinedOutput(); err != nil {
		t.Skipf("initdb is not usable here: %v\n%s", err, out)
	}
	start := exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-w", "-t", "60", "-o", "-c listen_addresses='' -c unix_socket_directories="+base+" -c fsync=off -p "+strconv.Itoa(port), "-l", filepath.Join(base, "log"), "start")
	if out, err := start.CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(base, "log"))
		t.Skipf("PostgreSQL did not start here: %v\n%s\n%s", err, out, log)
	}
	t.Cleanup(func() { _ = exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-m", "immediate", "stop").Run() })
	psql = func(database, script string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, filepath.Join(bin, "psql"), "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-h", base, "-p", strconv.Itoa(port), "-U", "postgres", "-d", database)
		cmd.Stdin = strings.NewReader(script)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return strings.TrimSpace(out.String()), err
	}
	// The agent addresses host files under a root; the cluster's real
	// directory is reachable under it at the same path.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(base)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, filepath.Join(root, base)); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755)
	os.WriteFile(filepath.Join(root, "usr/bin/pg_lsclusters"), []byte("#!/bin/sh\n"), 0o755)
	major, _ := strconv.Atoi(filepath.Base(filepath.Dir(bin)))
	run := func(ctx context.Context, stdin, name string, args ...string) (string, error) {
		switch name {
		case "/usr/bin/pg_lsclusters":
			return fmt.Sprintf("%d main %d online postgres %s %s\n", major, port, data, filepath.Join(base, "log")), nil
		case "/usr/sbin/runuser":
			// runuser -u postgres -- /usr/bin/psql ... -h <socket dir> ...
			for i, arg := range args {
				if arg != "--" || i+1 >= len(args) || filepath.Base(args[i+1]) != "psql" {
					continue
				}
				real := append([]string{}, args[i+2:]...)
				for j := range real {
					if real[j] == "-h" && j+1 < len(real) {
						real[j+1] = base
					}
				}
				cmd := exec.CommandContext(ctx, filepath.Join(bin, "psql"), append(real, "-U", "postgres")...)
				cmd.Stdin = strings.NewReader(stdin)
				var out bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &out
				err := cmd.Run()
				return out.String(), err
			}
		}
		return "", nil
	}
	previous, savedCount := nativeHostDataHost, moveTableCount
	nativeHostDataHost = func(*Manager) nativeHostData {
		return nativeHostData{root: root, run: run, digest: func(string) (string, error) { return strings.Repeat("ab", 32), nil }}
	}
	m = &Manager{cfg: Config{DataDir: t.TempDir()}, key: bytes.Repeat([]byte{7}, 32), ctx: context.Background(), databaseSlots: make(chan struct{}, 1), data: diskState{Apps: map[string]App{}}}
	// The table count normally goes through runuser on the host; here the
	// same query runs against the cluster.
	moveTableCount = func(ctx context.Context, m *Manager, id string) (int, error) {
		m.mu.Lock()
		target, err := m.moveDatabaseTargetLocked(id)
		m.mu.Unlock()
		if err != nil || target.Host == nil {
			return 0, fmt.Errorf("unknown database: %v", err)
		}
		out, err := psql(target.Host.Name, moveTableQuery+";\n")
		if err != nil {
			return 0, fmt.Errorf("the database could not be measured: %s", out)
		}
		return strconv.Atoi(out)
	}
	t.Cleanup(func() { nativeHostDataHost, moveTableCount = previous, savedCount })
	return m, psql
}

// The cycle that failed on a real server, through the agent's own code and
// a real PostgreSQL: give an application a database, take it away, meet
// what an interrupted removal leaves behind, and create the application's
// database again. Stand-ins for psql cannot fail the way PostgreSQL does.
func TestAnApplicationsDatabaseCanBeCreatedRemovedAndCreatedAgainOnARealServer(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("PostgreSQL refuses to run as root")
	}
	candidates, _ := filepath.Glob("/usr/lib/postgresql/*/bin/initdb")
	if len(candidates) == 0 {
		t.Skip("no PostgreSQL server binaries on this machine")
	}
	ran := 0
	defer func() {
		if ran == 0 {
			t.Skip("no PostgreSQL version the agent supports is installed on this machine")
		}
	}()
	for _, initdb := range candidates {
		bin := filepath.Dir(initdb)
		if major, _ := strconv.Atoi(filepath.Base(filepath.Dir(bin))); major < nativePGMinMajor || major > nativePGMaxMajor {
			continue
		}
		ran++
		t.Run("PostgreSQL "+filepath.Base(filepath.Dir(bin)), func(t *testing.T) {
			m, psql := realPostgresHost(t, bin)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			account := NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}
			state := func() string {
				out, err := psql("postgres", "SELECT (SELECT count(*) FROM pg_roles WHERE rolname = 'app_shop') || ' roles, ' || (SELECT count(*) FROM pg_database WHERE datname = 'app_shop') || ' databases';")
				if err != nil {
					t.Fatal(err, out)
				}
				return out
			}
			provision := func() NativeDataGrant {
				t.Helper()
				grant, err := provisionNativeData(ctx, m, "shop", account, NativeDataRequest{Postgres: true})
				if err != nil || len(grant.Databases) != 1 || grant.Env["DATABASE_URL"] == "" {
					t.Fatalf("provisioning: %v %+v", err, grant)
				}
				if got := state(); got != "1 roles, 1 databases" {
					t.Fatalf("after provisioning: %s", got)
				}
				return grant
			}

			// Created, then removed with its application: nothing stays.
			provision()
			// (No final backup: the application never released, as when a
			// creation is undone. Backups have their own tests.)
			if err := releaseNativeHostDataRecord(ctx, m, "shop", false); err != nil {
				t.Fatalf("removing the application's database: %v", err)
			}
			if _, left := m.hostDataRecord("shop"); left || state() != "0 roles, 0 databases" {
				t.Fatalf("after the removal: record %v, %s", left, state())
			}

			// What an interrupted removal left on a real server: the database
			// gone, its role and the agent's record still there. Creating the
			// application again finds it empty and clears it.
			provision()
			if out, err := psql("postgres", "DROP DATABASE \"app_shop\" WITH (FORCE);\n"); err != nil {
				t.Fatal(err, out)
			}
			if kind, _ := m.leftoverNativeData("shop"); kind != "unfinished" || !m.nativeDataEmpty("shop") {
				t.Fatalf("the leftover was not recognised: %q, empty %v", kind, m.nativeDataEmpty("shop"))
			}
			if note, err := m.clearLeftoverNativeData(ctx, "shop"); err != nil || note == "" {
				t.Fatalf("clearing the leftover: %q %v", note, err)
			}
			if _, left := m.hostDataRecord("shop"); left || state() != "0 roles, 0 databases" {
				t.Fatalf("after clearing the leftover: record %v, %s", left, state())
			}

			// The same leftover once it is listed as retained with a backup of
			// its empty state, as the repair pass records it.
			grant := provision()
			m.mu.Lock()
			d := m.data.Databases[grant.Databases[0]]
			d.Backups = []BackupRecord{{ID: "empty", Created: time.Now().UTC(), Size: 938, SHA256: strings.Repeat("ab", 32)}}
			m.data.Databases[grant.Databases[0]] = d
			m.mu.Unlock()
			if out, err := psql("postgres", "DROP DATABASE \"app_shop\" WITH (FORCE);\n"); err != nil {
				t.Fatal(err, out)
			}
			if _, err := retainNativeHostData(ctx, m, "shop", nil); err != nil {
				t.Fatal(err)
			}
			if !m.nativeDataEmpty("shop") || m.retainedNativeItem("shop") == "" {
				t.Fatalf("the listed leftover: empty %v, item %q", m.nativeDataEmpty("shop"), m.retainedNativeItem("shop"))
			}
			if note, err := m.clearLeftoverNativeData(ctx, "shop"); err != nil || !strings.Contains(note, "held no tables") {
				t.Fatalf("clearing the listed leftover: %q %v", note, err)
			}
			m.wg.Wait()
			if _, left := m.hostDataRecord("shop"); left || state() != "0 roles, 0 databases" || m.retainedNativeItem("shop") != "" {
				t.Fatalf("after clearing the listed leftover: record %v, %s, item %q", left, state(), m.retainedNativeItem("shop"))
			}

			// A database that holds a table is data: it is not cleared.
			provision()
			if out, err := psql("app_shop", "CREATE TABLE orders (id int);\n"); err != nil {
				t.Fatal(err, out)
			}
			if m.nativeDataEmpty("shop") {
				t.Fatal("a database with a table was called empty")
			}
			if _, err := retainNativeHostData(ctx, m, "shop", nil); err != nil {
				t.Fatal(err)
			}
			if note, err := m.clearLeftoverNativeData(ctx, "shop"); err != nil || note != "" {
				t.Fatalf("data was touched: %q %v", note, err)
			}
			if got := state(); got != "1 roles, 1 databases" {
				t.Fatalf("data was removed: %s", got)
			}
		})
	}
}
