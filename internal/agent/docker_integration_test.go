package agent

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDockerTwoAppsRedeployRollback(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for real container smoke test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "app")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../tests/fixture")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	var checkouts atomic.Int32
	rt := &Docker{Root: t.TempDir()}
	rt.checkout = func(ctx context.Context, a App, dir, token string) error {
		checkouts.Add(1)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		b, err := os.ReadFile(binary)
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, "app"), b, 0755); err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, "lidza.json"), []byte(`{"name":"fixture"}`), 0600); err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY --chmod=755 app /app\nENTRYPOINT [\"/app\"]\n"), 0600); err != nil {
			return err
		}
		// The checkout seam is local; all subsequent build/run/health/proxy operations are real.
		for _, args := range [][]string{{"init", "-b", "main"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "fixture"}} {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %v %s", args, err, out)
			}
		}
		return nil
	}
	m := testManager(t, rt)
	t.Cleanup(func() {
		for _, a := range m.Apps() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = rt.Remove(cleanup, a.Current)
			_ = rt.Remove(cleanup, a.Previous)
			cancel()
		}
	})
	deploy := func(a App) string {
		t.Helper()
		if err := m.Upsert(a); err != nil {
			t.Fatal(err)
		}
		d, err := m.Enqueue(a.ID, DeployRequest{})
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			for _, result := range m.Deployments() {
				if result.ID == d.ID {
					if result.Status == "failed" {
						t.Fatal(result.Error)
					}
					if result.Status == "live" {
						return result.ID
					}
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("docker deployment timeout")
		return ""
	}
	request := func(domain string) string {
		t.Helper()
		r := httptest.NewRequest("GET", "http://"+domain+"/", nil)
		w := httptest.NewRecorder()
		Proxy(m).ServeHTTP(w, r)
		b, _ := io.ReadAll(w.Result().Body)
		if w.Code != 200 {
			t.Fatalf("proxy: %d %s", w.Code, b)
		}
		return string(b)
	}
	one := testApp("smoke-one")
	one.Env = map[string]string{"RELEASE_TEXT": "one-v1"}
	two := testApp("smoke-two")
	two.Env = map[string]string{"RELEASE_TEXT": "two-v1"}
	if err := m.Upsert(one); err != nil {
		t.Fatal(err)
	}
	dbIDs := []string{"db-smoke-" + newID()[:10], "db-smoke-" + newID()[:10]}
	t.Cleanup(func() {
		m.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, a := range m.Apps() {
			_ = rt.Remove(cleanup, a.Current)
			_ = rt.Remove(cleanup, a.Previous)
		}
		for _, id := range dbIDs {
			for _, args := range [][]string{{"rm", "-f", "lidza-db-" + id}, {"volume", "rm", "lidza-db-" + id + "-data"}, {"network", "rm", "lidza-db-" + id}} {
				_, _ = command(cleanup, "", nil, "docker", args...)
			}
		}
	})
	for i, key := range []string{"DATABASE_URL", "ANALYTICS_DATABASE_URL"} {
		if err := m.configureDatabase(one.ID, DatabaseRequest{ID: dbIDs[i], EnvKey: key, Mode: "local", Backup: BackupPolicy{Keep: 10}}); err != nil {
			t.Fatal(err)
		}
		if d := waitDatabase(t, m, dbIDs[i]); !d.Ready || d.Error != "" {
			t.Fatal("database provisioning", d.Error)
		}
	}
	one.Env = nil
	first := deploy(one)
	assertDatabases := func() {
		t.Helper()
		req := httptest.NewRequest("GET", "http://"+one.Domain+"/check-databases", nil)
		w := httptest.NewRecorder()
		Proxy(m).ServeHTTP(w, req)
		if w.Code != 200 || w.Body.String() != "ok" {
			t.Fatal("attached database connections inaccessible", w.Code)
		}
	}
	assertDatabases()
	for _, d := range m.databaseViews() {
		if len(d.Backups) != 1 {
			t.Fatal("deployment did not back up each database")
		}
	}
	deploy(two)
	if request(one.Domain) != "one-v1" || request(two.Domain) != "two-v1" {
		t.Fatal("FQDN routing crossed applications")
	}
	oldDomain := one.Domain
	one.Domain = "changed-smoke.example.com"
	value := "one-v2"
	if err := m.PatchSettings(one.ID, SettingsPatch{Branch: one.Branch, Domain: one.Domain, EnvChanges: map[string]*string{"RELEASE_TEXT": &value}}); err != nil {
		t.Fatal(err)
	}
	// Saving settings reloads the current image without calling checkout again.
	second := m.Deployments()[len(m.Deployments())-1].ID
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if m.Current(one.ID).ID == second {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if first == second || request(one.Domain) != "one-v2" {
		t.Fatal("redeploy not serving new release")
	}
	if m.Target(oldDomain) != nil {
		t.Fatal("old FQDN still routes")
	}
	for host, code := range map[string]int{oldDomain: 403, one.Domain: 200} {
		req := httptest.NewRequest("GET", "/tls/allow?domain="+host, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		Handler(m).ServeHTTP(w, req)
		if w.Code != code {
			t.Fatal("TLS domain authorization did not follow FQDN edit")
		}
	}
	if checkouts.Load() != 2 {
		t.Fatal("runtime reload cloned source")
	}
	env, err := command(ctx, "", nil, "docker", "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", m.Current(one.ID).Container)
	if err != nil || !strings.Contains(env, "APP_URL=https://"+one.Domain) {
		t.Fatal("new runtime origin not applied")
	}
	if err := m.Rollback(ctx, one.ID); err != nil {
		t.Fatal(err)
	}
	if request(one.Domain) != "one-v1" || request(two.Domain) != "two-v1" {
		t.Fatal("rollback failed or affected other app")
	}
	// Switching primary to a second database reloads and retains both named variables.
	if err := m.configureDatabase(one.ID, DatabaseRequest{Mode: "existing", ID: dbIDs[1], EnvKey: "DATABASE_URL"}); err != nil {
		t.Fatal(err)
	}
	reloadID := m.Deployments()[len(m.Deployments())-1].ID
	deadline = time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if m.Current(one.ID).ID == reloadID {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if m.Current(one.ID).ID != reloadID {
		t.Fatal("database switch did not reload")
	}
	assertDatabases()
	m.mu.Lock()
	a := m.data.Apps[one.ID]
	same := a.Env["DATABASE_URL"] == a.Env["ANALYTICS_DATABASE_URL"]
	m.mu.Unlock()
	if !same {
		t.Fatal("primary did not switch")
	}
	// Verify actual runtime hardening, not just argument construction.
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.HostConfig.ReadonlyRootfs}} {{.Config.User}} {{.HostConfig.Memory}}", m.Current(one.ID).Container)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "true 65532:65532 536870912") {
		t.Fatal(err, string(out))
	}
	retained := m.Current(one.ID)
	if err := m.Retire(ctx, one.ID); err != nil {
		t.Fatal(err)
	}
	if err := rt.Remove(ctx, retained); err != nil {
		t.Fatal("Docker cleanup not idempotent", err)
	}
	if m.Target(one.Domain) != nil || request(two.Domain) != "two-v1" {
		t.Fatal("retirement affected other application")
	}
	containers, err := command(ctx, "", nil, "docker", "container", "ls", "--all", "--filter", "name=^/lidza-smoke-one-", "--format", "{{.ID}}")
	if err != nil || containers != "" {
		t.Fatal("retired containers remain", err, containers)
	}

}
