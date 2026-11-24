package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// realCluster starts one throwaway PostgreSQL cluster and returns its
// socket directory and a psql for it.
func realCluster(t *testing.T, bin, extra string) (socket string, psql func(database, script string) (string, error)) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "lidza-pg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	data := filepath.Join(base, "data")
	if out, err := exec.Command(filepath.Join(bin, "initdb"), "-D", data, "-A", "trust", "-U", "postgres", "--no-sync").CombinedOutput(); err != nil {
		t.Skipf("initdb is not usable here: %v\n%s", err, out)
	}
	start := exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-w", "-t", "60", "-o", "-c listen_addresses='' -c unix_socket_directories="+base+" -c fsync=off "+extra, "-l", filepath.Join(base, "log"), "start")
	if out, err := start.CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(base, "log"))
		t.Skipf("PostgreSQL did not start here: %v\n%s\n%s", err, out, log)
	}
	t.Cleanup(func() { _ = exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-m", "immediate", "stop").Run() })
	return base, func(database, script string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, filepath.Join(bin, "psql"), "-X", "-q", "-A", "-t", "-F", "\t", "-v", "ON_ERROR_STOP=1", "-h", base, "-U", "postgres", "-d", database)
		cmd.Stdin = strings.NewReader(script)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return strings.TrimSpace(out.String()), err
	}
}

// A move copies PostgreSQL by logical replication. The statements the agent
// runs for it, on two real servers: publish on the source, subscribe on the
// destination, rows copied and then followed, the status queries the move
// reads, and a cleanup that leaves no slot, publication or role on the
// source however often it runs. A slot left behind holds the source's WAL
// back until its disk is full.
func TestMoveReplicationStatementsOnRealServers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("PostgreSQL refuses to run as root")
	}
	candidates, _ := filepath.Glob("/usr/lib/postgresql/*/bin/initdb")
	ran := 0
	for _, initdb := range candidates {
		bin := filepath.Dir(initdb)
		if major, _ := strconv.Atoi(filepath.Base(filepath.Dir(bin))); major < nativePGMinMajor || major > nativePGMaxMajor {
			continue
		}
		ran++
		t.Run("PostgreSQL "+filepath.Base(filepath.Dir(bin)), func(t *testing.T) {
			sourceSocket, source := realCluster(t, bin, "-c wal_level=logical")
			_, target := realCluster(t, bin, "")
			schema := "CREATE TABLE orders (id bigint PRIMARY KEY, note text);\nCREATE TABLE audit (id bigint PRIMARY KEY, line text);\n"
			for name, psql := range map[string]func(string, string) (string, error){"source": source, "destination": target} {
				if out, err := psql("postgres", "CREATE DATABASE app_shop;\n"); err != nil {
					t.Fatal(name, err, out)
				}
				if out, err := psql("app_shop", schema); err != nil {
					t.Fatal(name, err, out)
				}
			}
			if out, err := source("app_shop", "INSERT INTO orders SELECT g, 'bulk' FROM generate_series(1, 5000) g;\nINSERT INTO audit VALUES (1, 'kept on the source only');\n"); err != nil {
				t.Fatal(err, out)
			}
			const role, publication, slot, subscription = "lidza_move_abc", "lidza_move_abc", "lidza_move_abc", "lidza_move_abc"
			verifier := "SCRAM-SHA-256$4096:c2FsdHNhbHRzYWx0c2FsdA==$" + strings.Repeat("A", 43) + "=:" + strings.Repeat("B", 43) + "="

			// One table is left out of the publication, as a move does for
			// tables it copies another way.
			publish, err := renderMovePublicationSQL(role, publication, "app_shop", verifier, []string{"public.orders", "public.audit"}, []string{"public.audit"})
			if err != nil {
				t.Fatal(err)
			}
			if out, err := source("app_shop", publish); err != nil {
				t.Fatalf("publishing: %v\n%s", err, out)
			}
			if out, err := source("app_shop", publish); err != nil {
				t.Fatalf("publishing a second time: %v\n%s", err, out)
			}
			subscribe, err := renderMoveSubscriptionSQL(subscription, publication, slot, "host="+sourceSocket+" dbname=app_shop user="+role)
			if err != nil {
				t.Fatal(err)
			}
			if out, err := target("app_shop", subscribe); err != nil {
				t.Fatalf("subscribing: %v\n%s", err, out)
			}
			wait := func(what string, done func() bool) {
				t.Helper()
				for deadline := time.Now().Add(40 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
					if done() {
						return
					}
				}
				t.Fatalf("timed out waiting for %s", what)
			}
			count := func() string {
				out, _ := target("app_shop", "SELECT count(*) FROM orders;\n")
				return out
			}
			wait("the bulk copy", func() bool { return count() == "5000" })
			// The subscription's status, as the move reads it.
			var lastStatus string
			defer func() {
				if t.Failed() {
					t.Logf("last subscription status:\n%s", lastStatus)
				}
			}()
			wait("the subscription to report its table ready", func() bool {
				out, err := target("app_shop", renderMoveSubscriptionStatusSQL(subscription)+";\n")
				lastStatus = out
				status := parseMoveSubscriptionStatus(subscription, out)
				return err == nil && status.Exists && status.Enabled && status.Total == 1 && status.Ready == 1 && status.SyncErrors == 0 && status.ApplyErrors == 0
			})
			// Rows written after the copy follow.
			if out, err := source("app_shop", "INSERT INTO orders VALUES (5001, 'late');\nUPDATE orders SET note = 'changed' WHERE id = 1;\nDELETE FROM orders WHERE id = 2;\n"); err != nil {
				t.Fatal(err, out)
			}
			wait("the later changes", func() bool {
				out, _ := target("app_shop", "SELECT count(*) || ' ' || (SELECT note FROM orders WHERE id = 1) || ' ' || (SELECT count(*) FROM orders WHERE id = 5001) FROM orders;\n")
				return out == "5000 changed 1"
			})
			if out, _ := target("app_shop", "SELECT count(*) FROM audit;\n"); out != "0" {
				t.Fatalf("the excluded table was replicated: %s rows", out)
			}
			// The source's status, as the move reads it.
			out, err := source("app_shop", renderMoveReplicationStatusSQL(slot, true)+";\n")
			status := parseMoveReplicationStatus(out)
			if err != nil || !status.SlotExists || !status.SlotActive || status.WALLevel != "logical" || status.CurrentLSN == "" {
				t.Fatalf("source status: %v %+v\n%s", err, status, out)
			}

			// The end of the move: the destination lets go, the source removes
			// everything, and both can be asked again.
			unsubscribe, _ := renderMoveUnsubscribeSQL(subscription)
			for attempt := 1; attempt <= 2; attempt++ {
				if out, err := target("app_shop", unsubscribe); err != nil {
					t.Fatalf("unsubscribing, attempt %d: %v\n%s", attempt, err, out)
				}
			}
			unpublish, _ := renderMoveUnpublishSQL(role, publication, slot)
			for attempt := 1; attempt <= 2; attempt++ {
				if out, err := source("app_shop", unpublish); err != nil {
					t.Fatalf("cleaning up the source, attempt %d: %v\n%s", attempt, err, out)
				}
			}
			left, err := source("app_shop", "SELECT (SELECT count(*) FROM pg_replication_slots) || ' slots, ' || (SELECT count(*) FROM pg_publication) || ' publications, ' || (SELECT count(*) FROM pg_roles WHERE rolname = '"+role+"') || ' roles';\n")
			if err != nil || left != "0 slots, 0 publications, 0 roles" {
				t.Fatalf("left on the source: %s %v", left, err)
			}
			if out, _ := target("app_shop", "SELECT count(*) FROM pg_subscription;\n"); out != "0" {
				t.Fatalf("subscriptions left on the destination: %s", out)
			}
			if got := count(); got != "5000" {
				t.Fatalf("the destination lost rows at the end: %s", got)
			}
		})
	}
	if ran == 0 {
		t.Skip("no PostgreSQL version the agent supports is installed on this machine")
	}
}
