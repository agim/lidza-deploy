package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerAutomaticCacheDeployReloadAndPersistence(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for real cache lifecycle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "app")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../tests/cachefixture")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	rt := &Docker{Root: t.TempDir()}
	rt.checkout = func(ctx context.Context, a App, dir, token string) error {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		b, err := os.ReadFile(binary)
		if err != nil {
			return err
		}
		if err = os.Mkdir(filepath.Join(dir, "mail"), 0755); err != nil {
			return err
		}
		for name, data := range map[string][]byte{"app": b, "mail/auth_reset.txt.tmpl": []byte("Reset your account: {{.URL}}"), "lidza.json": []byte(`{"name":"fixture","frontend":{"template":"htmx"},"packs":["lidza/db","lidza/auth","lidza/mail","lidza/cache","lidza/storage"]}`), "Dockerfile": []byte("FROM scratch\nWORKDIR /app\nCOPY --chmod=755 app /app/app\nCOPY --chown=65532:65532 mail /app/mail\nENTRYPOINT [\"/app/app\"]\n")} {
			if err = os.WriteFile(filepath.Join(dir, name), data, 0755); err != nil {
				return err
			}
		}
		for _, args := range [][]string{{"init", "-b", "main"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "cache fixture"}} {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir = dir
			if out, e := cmd.CombinedOutput(); e != nil {
				return fmt.Errorf("fixture git: %w %s", e, out)
			}
		}
		return nil
	}
	m := testManager(t, rt)
	a := testApp(fmt.Sprintf("cache-%d", time.Now().UnixNano()))
	a.Env["MAIL_PROVIDER"] = "smtp"
	a.Env["MAIL_FROM"] = "fixture@example.com"
	a.Env["MAIL_SMTP_URL"] = "smtp://fixture:fixture-password@smtp.example.com:587"
	// As with the GUI, the application's master key is supplied explicitly.
	a.Env["LIDZA_MASTER_KEY"] = strings.Repeat("ab", 32)
	// Reproduce the production failure: local was previously rejected as dev-only.
	a.Env["STORAGE_PROVIDER"] = "local"
	name := "lidza-cache-" + a.ID
	t.Cleanup(func() {
		m.Close()
		for _, app := range m.Apps() {
			_ = rt.Remove(context.Background(), app.Current)
			_ = rt.Remove(context.Background(), app.Previous)
		}
		for _, args := range [][]string{{"rm", "-f", name}, {"volume", "rm", name + "-data"}, {"network", "rm", name}} {
			_ = exec.Command("docker", args...).Run()
		}
		dbName := "lidza-db-" + a.ID
		for _, args := range [][]string{{"rm", "-f", dbName}, {"volume", "rm", dbName + "-data"}, {"network", "rm", dbName}} {
			_ = exec.Command("docker", args...).Run()
		}
		_ = exec.Command("docker", "volume", "rm", "lidza-storage-"+a.ID).Run()
	})
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if err := m.configureDatabase(a.ID, DatabaseRequest{Mode: "local", Backup: BackupPolicy{Hours: 24, Keep: 1}}); err != nil {
		t.Fatal(err)
	}
	if database := waitDatabase(t, m, a.ID); !database.Ready || database.Error != "" {
		t.Fatal("full-stack database provisioning failed", database.Error)
	}
	deploy := func(reload bool) {
		t.Helper()
		var d Deployment
		var err error
		if reload {
			d, err = m.Reload(a.ID)
		} else {
			d, err = m.Enqueue(a.ID, DeployRequest{})
		}
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			for _, result := range m.Deployments() {
				if result.ID == d.ID {
					if result.Status == "failed" {
						t.Fatalf("cache app failed: %s\n%s", result.Error, result.Log)
					}
					if result.Status == "live" {
						return
					}
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("cache deployment timed out")
	}
	request := func(path string) {
		t.Helper()
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("http://127.0.0.1:" + m.Current(a.ID).Port + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != "persisted" {
			t.Fatalf("cache request %s: %d %s", path, resp.StatusCode, body)
		}
	}
	deploy(false)
	request("/write")
	request("/storage-write")
	m.mu.Lock()
	resource := m.data.Caches[a.ID]
	m.mu.Unlock()
	inspect, err := command(ctx, "", nil, "docker", "inspect", "--format", `{{.Config.User}} {{.HostConfig.ReadonlyRootfs}} {{json .HostConfig.PortBindings}} {{json .Config.Env}}`, name)
	if err != nil || !strings.HasPrefix(inspect, "65532:65532 true {} ") || strings.Contains(inspect, resource.LocalPassword) {
		t.Fatal("cache isolation/credential controls failed", err, inspect)
	}
	unauthorized, err := command(ctx, "", nil, "docker", "exec", name, "valkey-cli", "PING")
	if err != nil || !strings.Contains(unauthorized, "NOAUTH") {
		t.Fatal("unauthenticated cache accepted", err, unauthorized)
	}
	ip, err := command(ctx, "", nil, "docker", "inspect", "--format", `{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}`, name)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(resource.URL)
	u.Host = ip + ":6379"
	if err = m.provisionCache(ctx, CacheResource{Mode: "external", URL: u.String()}); err != nil {
		t.Fatal("external framework client could not authenticate", err)
	}
	u.User = url.UserPassword("", "wrong-password")
	if err = m.provisionCache(ctx, CacheResource{Mode: "external", URL: u.String()}); err == nil || strings.Contains(err.Error(), "wrong-password") {
		t.Fatal("external auth failure was accepted or exposed password")
	}
	deploy(true)
	request("/read")
	request("/storage-read")
	if _, err = command(ctx, "", nil, "docker", "stop", name); err != nil {
		t.Fatal(err)
	}
	deploy(false)
	request("/read")
	request("/storage-read")
	if err = m.Rollback(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	request("/storage-read")
	// Scheduled commands mount the same volume with the same unprivileged UID.
	m.mu.Lock()
	taskApp := m.data.Apps[a.ID]
	m.mu.Unlock()
	_, out, err := m.startTaskContainer(ctx, taskApp, Task{ID: "storage-reader", Mode: "schedule", Command: []string{"/app/app", "storage-task"}}, "")
	if err != nil || !strings.Contains(out, "persisted") {
		t.Fatal("scheduled command did not receive lasting storage", err, out)
	}
	m.mu.Lock()
	stable := m.data.Caches[a.ID]
	m.mu.Unlock()
	if resource.URL != stable.URL {
		t.Fatal("redeploy rotated credentials")
	}
	for _, d := range m.Deployments() {
		if strings.Contains(d.Log, resource.LocalPassword) || strings.Contains(d.Error, resource.LocalPassword) {
			t.Fatal("cache secret leaked into deploy logs")
		}
	}
}
