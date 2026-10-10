package agent

import (
	"context"
	"github.com/agim/lidza-deploy/internal/platform/state"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceIsolationAndEscaping(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	for _, id := range []string{"one", "two"} {
		if err := m.Upsert(testApp(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.setMaintenance("one", Maintenance{true, "<script>bad</script>"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host string
		code int
	}{{"one.example.com", 503}, {"two.example.com", 503}} {
		w := httptest.NewRecorder()
		Proxy(m).ServeHTTP(w, httptest.NewRequest("GET", "http://"+tc.host+"/", nil))
		if w.Code != tc.code {
			t.Fatal(w.Code)
		}
		if tc.host == "one.example.com" {
			if strings.Contains(w.Body.String(), "<script>") || !strings.Contains(w.Body.String(), "&lt;script&gt;") || w.Header().Get("Retry-After") == "" {
				t.Fatal("unsafe maintenance response")
			}
		} else if strings.Contains(w.Body.String(), "We’ll be back") {
			t.Fatal("other app affected")
		}
	}
	if err := m.setMaintenance("one", Maintenance{}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	Proxy(m).ServeHTTP(w, httptest.NewRequest("GET", "http://one.example.com/", nil))
	if strings.Contains(w.Body.String(), "We’ll be back") {
		t.Fatal("maintenance did not end")
	}
}
func TestRestoreIntegrityAndNoOverwrite(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if err := m.Upsert(testApp("one")); err != nil {
		t.Fatal(err)
	}
	source := Database{AppID: "source", Ready: true, Backup: BackupPolicy{Keep: 1}, Backups: []BackupRecord{{ID: "copy", Size: 3, SHA256: "incorrect"}}}
	m.data.Databases["source"] = source
	m.data.Databases["existing"] = Database{AppID: "existing"}
	if err := m.restoreDatabase("one", RestoreRequest{Source: "source", Backup: "copy", Target: "existing"}); err == nil {
		t.Fatal("allowed overwriting existing database")
	}
	if err := os.MkdirAll(m.backupDir("source"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.backupDir("source"), "copy.dump"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.restoreInto(context.Background(), "source", source.Backups[0], Database{}); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatal("corrupt backup was not rejected before provisioning", err)
	}
}
func TestHistoryRedactsSecretsAndTracksKeys(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("one")
	m.data.Deployments = []Deployment{{ID: "build"}}
	ctx := m.deploymentDiagnostics(context.Background(), job{deployment: "build", app: a, token: "private-token"})
	commandDiagnostic(ctx, "docker", "private-token runtime-secret")
	if strings.Contains(m.data.Deployments[0].Log, "private-token") || strings.Contains(m.data.Deployments[0].Log, "runtime-secret") {
		t.Fatal("secret in history")
	}
	b := a
	b.Domain = "new.example.com"
	b.Env = map[string]string{"NEW": "secret-value"}
	keys := strings.Join(changeKeys(a, b), ",")
	if !strings.Contains(keys, "domain") || !strings.Contains(keys, "env:NEW") || strings.Contains(keys, "secret-value") {
		t.Fatal(keys)
	}
}
func TestTaskScheduleValidation(t *testing.T) {
	for _, task := range []Task{{ID: "work", Mode: "worker", Command: []string{"/app/app", "worker"}}, {ID: "daily", Mode: "schedule", Command: []string{"/app/app", "cleanup"}, Daily: "02:30", Timezone: "Europe/Tirane"}, {ID: "hourly", Mode: "schedule", Command: []string{"/app/app"}, EveryMinutes: 60}} {
		if err := task.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range []Task{{ID: "../oops", Mode: "worker", Command: []string{"x"}}, {ID: "invalid", Mode: "schedule", Command: []string{"x"}, Daily: "25:99"}, {ID: "invalid", Mode: "schedule", Command: []string{"x"}, EveryMinutes: 0}, {ID: "invalid", Mode: "worker", Command: []string{""}}} {
		if err := task.Validate(); err == nil {
			t.Fatal("invalid task accepted")
		}
	}
}

func TestTaskRevisionAndPriorRunFence(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("one")
	a.Current = &Release{ID: "release", Image: "fixture"}
	m.data.Apps[a.ID] = a
	key := taskKey(a.ID, "cleanup")
	m.data.Tasks[key] = Task{ID: "cleanup", AppID: a.ID, Mode: "schedule", Enabled: true, Command: []string{"/app/app"}, EveryMinutes: 60, Revision: "new-definition", SeenKeys: []string{"already-ran"}}
	if err := m.runTask(a.ID, "cleanup", "queued-old", "old-definition"); err != nil {
		t.Fatal(err)
	}
	if err := m.runTask(a.ID, "cleanup", "already-ran", "new-definition"); err != nil {
		t.Fatal(err)
	}
	if task := m.data.Tasks[key]; task.Running || task.LastRun != nil {
		t.Fatal("stale definition or replay started a command")
	}
}

func TestUpgradeBlocksNewOperationsUntilHealthy(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if err := m.Upsert(testApp("one")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.cfg.DataDir, "upgrade", "status.json")
	if err := state.Save(path, UpgradeStatus{State: "upgrading"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Enqueue("one", DeployRequest{}); err == nil {
		t.Fatal("deployment started during upgrade")
	}
	if err := m.configureDatabase("one", DatabaseRequest{Mode: "local", Backup: BackupPolicy{Keep: 1}}); err == nil {
		t.Fatal("database operation started during upgrade")
	}
	if len(m.data.Databases) != 0 {
		t.Fatal("blocked operation mutated database bindings")
	}
	if err := state.Save(path, UpgradeStatus{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if m.upgradePending() {
		t.Fatal("healthy upgrade left operation gate closed")
	}
}

func TestDeploymentOutputAppearsBeforeCommandFinishes(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("one")
	m.mu.Lock()
	m.data.Deployments = []Deployment{{ID: "streaming"}}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = m.deploymentDiagnostics(ctx, job{deployment: "streaming", app: a, token: "private-token"})
	marker := filepath.Join(t.TempDir(), "continue")
	done := make(chan error, 1)
	go func() {
		_, err := command(ctx, "", nil, "sh", "-c", `printf 'phase-one\nprivate-'; while [ ! -f "$1" ]; do sleep 0.01; done; printf 'token\nphase-two\n'`, "fixture", marker)
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		log := m.Deployments()[0].Log
		if strings.Contains(log, "phase-one") {
			if strings.Contains(log, "private-") {
				t.Fatal("partial secret leaked before command completion")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("output was buffered until command completion")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatal("command completed before live output was checked", err)
	default:
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	log := m.Deployments()[0].Log
	if !strings.Contains(log, "phase-two") || strings.Contains(log, "private-token") || !strings.Contains(log, "[redacted]") {
		t.Fatal("final output missing or secret exposed")
	}
}

func TestCommandCopyKeepsOutputBoundedAndPublishesChunks(t *testing.T) {
	writes := 0
	b := limitedBuffer{diagnostic: func(string) { writes++ }}
	n, err := io.Copy(&b, io.LimitReader(strings.NewReader(strings.Repeat("x", 70000)), 70000))
	if err != nil || n != 70000 || b.Len() != 65536 || writes < 2 {
		t.Fatalf("copy bypassed bounded live writes: bytes=%d retained=%d writes=%d err=%v", n, b.Len(), writes, err)
	}
}

func TestDeploymentOutputRetainsFailureTail(t *testing.T) {
	b := limitedBuffer{}
	b.Write([]byte(strings.Repeat("successful build step\n", 5000)))
	b.Write([]byte("compiler: final build failure\n"))
	if b.Len() > 65536 || !strings.HasSuffix(b.String(), "compiler: final build failure\n") {
		t.Fatal("final command failure was discarded")
	}
	text := deploymentLogTail(strings.Repeat("previous command\n", 5000) + b.String())
	if len(text) > 65536 || !strings.Contains(text, "Earlier deployment output omitted") || !strings.HasSuffix(text, "compiler: final build failure\n") {
		t.Fatal("deployment history discarded failure tail")
	}
	long := deploymentLogTail(strings.Repeat("x", 70000) + "final failure")
	if len(long) > 65536 || !strings.HasSuffix(long, "final failure") {
		t.Fatal("long line discarded failure")
	}
}

func TestRollingLogRedactsSecretAtBufferBoundary(t *testing.T) {
	secret := "secret-database-password"
	output := scrubLiveOutput(secret[7:]+"\ncompiler failed\n", []string{secret})
	if strings.Contains(output, secret[7:]) || !strings.Contains(output, "compiler failed") {
		t.Fatalf("boundary secret not redacted: %q", output)
	}
}
