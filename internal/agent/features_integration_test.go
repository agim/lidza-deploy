package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerRestoreIntoNewDatabaseAndPreviewCleanup(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1")
	}
	m := testManager(t, &fakeRuntime{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id := "restore-" + newID()[:10]
	target := id + "-copy"
	ids := []string{id, target}
	t.Cleanup(func() {
		m.Close()
		cleanup, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		for _, db := range ids {
			network := "lidza-db-" + db
			for _, args := range [][]string{{"rm", "-f", network}, {"volume", "rm", network + "-data"}, {"network", "rm", network}} {
				_, _ = command(cleanup, "", nil, "docker", args...)
			}
		}
	})
	a := testApp(id)
	a.Preview = true
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if err := m.configureDatabase(id, DatabaseRequest{Mode: "local", Backup: BackupPolicy{Keep: 1}}); err != nil {
		t.Fatal(err)
	}
	d := waitDatabase(t, m, id)
	if !d.Ready {
		t.Fatal(d.Error)
	}
	network := "lidza-db-" + id
	if _, err := command(ctx, "", nil, "docker", "exec", network, "psql", "-U", "postgres", "-d", "app", "-c", "CREATE TABLE proof(value text); ALTER TABLE proof OWNER TO app; INSERT INTO proof VALUES('original');"); err != nil {
		t.Fatal(err)
	}
	if err := m.databaseAction(id, "backup"); err != nil {
		t.Fatal(err)
	}
	d = waitDatabase(t, m, id)
	if len(d.Backups) != 1 {
		t.Fatal(d.Error)
	}
	if _, err := command(ctx, "", nil, "docker", "exec", network, "psql", "-U", "postgres", "-d", "app", "-c", "UPDATE proof SET value='after-backup';"); err != nil {
		t.Fatal(err)
	}
	if err := m.restoreDatabase(id, RestoreRequest{Source: id, Backup: d.Backups[0].ID, Target: target}); err != nil {
		t.Fatal(err)
	}
	restored := waitDatabase(t, m, target)
	if !restored.Ready || restored.Error != "" {
		t.Fatal("new database restore", restored.Error)
	}
	m.mu.Lock()
	bound := m.data.Apps[id].Bindings["DATABASE_URL"]
	oldReady := m.data.Databases[id].Ready
	m.mu.Unlock()
	if bound != target || !oldReady {
		t.Fatal("connection switch lost original")
	}
	for _, test := range []struct{ name, want string }{{id, "after-backup"}, {target, "original"}} {
		value, err := command(ctx, "", nil, "docker", "exec", "lidza-db-"+test.name, "psql", "-U", "postgres", "-d", "app", "-tAc", "SELECT value FROM proof")
		if err != nil || value != test.want {
			t.Fatal("restore changed source or wrong target", value, err)
		}
	}
	// The source is an ephemeral preview DB; the restored target is ordinary retained data.
	if err := m.removePreview(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := m.removePreview(ctx, id); err != nil {
		t.Fatal("cleanup not idempotent", err)
	}
	found, err := command(ctx, "", nil, "docker", "volume", "ls", "--filter", "name=^lidza-db-"+id+"-data$", "--format", "{{.Name}}")
	if err != nil || found != "" {
		t.Fatal("preview volume remained", found, err)
	}
	m.mu.Lock()
	_, retained := m.data.Databases[target]
	m.mu.Unlock()
	if !retained {
		t.Fatal("cleanup deleted restored database")
	}
}
func TestDockerTaskRunRestartAndEnvironmentIsolation(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1")
	}
	m := testManager(t, &fakeRuntime{})
	id := "tasks-" + newID()[:10]
	a := testApp(id)
	a.Env["SECRET"] = "fixture-private-value"
	a.Current = &Release{ID: "release-one", Image: databaseImage}
	if err := m.Upsert(testApp(id)); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.data.Apps[id] = a
	m.mu.Unlock()
	t.Cleanup(func() {
		m.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, task := range m.taskList(id) {
			if task.Container != "" {
				_, _ = command(ctx, "", nil, "docker", "rm", "-f", task.Container)
			}
		}
	})
	task := Task{ID: "once", Mode: "schedule", Command: []string{"/bin/sh", "-c", "printf '%s' \"$SECRET\""}, EveryMinutes: 60, Enabled: true}
	if err := m.putTask(id, task); err != nil {
		t.Fatal(err)
	}
	if err := m.runTask(id, "once", "unique-run"); err != nil {
		t.Fatal(err)
	}
	wait := func(taskID string, worker bool) Task {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			for _, task := range m.taskList(id) {
				if task.ID == taskID && ((worker && task.Container != "") || (!worker && !task.Running && task.LastRun != nil)) {
					return task
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("task timed out")
		return Task{}
	}
	done := wait("once", false)
	if done.Error != "" || strings.Contains(done.Log, "fixture-private-value") || !strings.Contains(done.Log, "[redacted]") {
		t.Fatal("task execution or secret masking", done.Error, done.Log)
	}
	if err := m.runTask(id, "once", "unique-run"); err != nil {
		t.Fatal(err)
	}
	if m.taskList(id)[0].Running {
		t.Fatal("duplicate run repeated command")
	}
	if err := m.runTask(id, "once", "manual-second"); err != nil {
		t.Fatal(err)
	}
	wait("once", false)
	if err := m.runTask(id, "once", "unique-run"); err != nil {
		t.Fatal(err)
	}
	for _, task := range m.taskList(id) {
		if task.ID == "once" && (task.Running || task.RunKey != "manual-second") {
			t.Fatal("prior scheduled run replayed after manual command")
		}
	}
	worker := Task{ID: "worker", Mode: "worker", Command: []string{"/bin/sh", "-c", "while true; do echo alive; sleep 1; done"}, Enabled: true}
	if err := m.putTask(id, worker); err != nil {
		t.Fatal(err)
	}
	if err := m.runTask(id, "worker", ""); err != nil {
		t.Fatal(err)
	}
	running := wait("worker", true)
	if running.Error != "" {
		t.Fatal(running.Error)
	}
	if err := m.runTask(id, "worker", "restart"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		next := wait("worker", true)
		if next.Container != running.Container {
			worker = next
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if worker.Container == "" || worker.Container == running.Container {
		t.Fatal("restart did not replace worker")
	}
	worker.Enabled = false
	if err := m.putTask(id, worker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := command(ctx, "", nil, "docker", "inspect", worker.Container); err == nil {
		t.Fatal("disabled worker still exists")
	}
}
func TestRestoreFailedDumpPreservesConnection(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	id := "one"
	a := testApp(id)
	a.Bindings = map[string]string{"DATABASE_URL": "source"}
	a.Env["DATABASE_URL"] = "postgres://original"
	m.data.Apps[id] = a
	hash := sha256.Sum256([]byte("valid"))
	m.data.Databases["source"] = Database{AppID: "source", Ready: true, Backup: BackupPolicy{Keep: 1}, Backups: []BackupRecord{{ID: "copy", Size: 5, SHA256: hex.EncodeToString(hash[:])}}}
	if err := os.MkdirAll(m.backupDir("source"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.backupDir("source"), "copy.dump"), []byte("wrong"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.restoreDatabase(id, RestoreRequest{Source: "source", Backup: "copy", Target: "new"}); err != nil {
		t.Fatal(err)
	}
	d := waitDatabase(t, m, "new")
	if d.Ready || !strings.Contains(d.Error, "integrity") {
		t.Fatal("corrupt restore ready", d.Error)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.Apps[id].Bindings["DATABASE_URL"] != "source" || m.data.Apps[id].Restoring || m.data.Databases["source"].Operation != "" {
		t.Fatal("failed restore affected active connection")
	}
}
