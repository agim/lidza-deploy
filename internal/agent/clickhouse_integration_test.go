package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerAutomaticClickHouseDeployReloadAndPersistence(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for real service lifecycle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "app")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../tests/clickhousefixture")
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
		for name, data := range map[string][]byte{"app": b, "lidza.json": []byte(`{"name":"fixture","frontend":{"template":"htmx"},"deploy":{"env":{"CLICKHOUSE_MANAGED":"true"}}}`), "Dockerfile": []byte("FROM scratch\nCOPY --chmod=755 app /app/app\nENTRYPOINT [\"/app/app\"]\n")} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0755); err != nil {
				return err
			}
		}
		for _, args := range [][]string{{"init", "-b", "main"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "fixture"}} {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("fixture git: %w %s", err, out)
			}
		}
		return nil
	}
	m := testManager(t, rt)
	a := testApp(fmt.Sprintf("ch-%d", time.Now().UnixNano()))
	name := "lidza-clickhouse-" + a.ID
	t.Cleanup(func() {
		m.Close()
		for _, app := range m.Apps() {
			_ = rt.Remove(context.Background(), app.Current)
			_ = rt.Remove(context.Background(), app.Previous)
		}
		// Only remove resources whose ownership was established by this fixture.
		for _, r := range []struct{ kind, name string }{{"container", name}, {"volume", name + "-data"}, {"volume", name + "-logs"}, {"network", name}} {
			format := `{{index .Labels "io.lidza.clickhouse"}}`
			if r.kind == "container" {
				format = `{{index .Config.Labels "io.lidza.clickhouse"}}`
			}
			out, err := exec.Command("docker", r.kind, "inspect", "--format", format, r.name).Output()
			if err == nil && strings.TrimSpace(string(out)) == name {
				args := []string{r.kind, "rm", r.name}
				if r.kind == "container" {
					args = []string{"rm", "-f", r.name}
				}
				_ = exec.Command("docker", args...).Run()
			}
		}
	})
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
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
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			for _, result := range m.Deployments() {
				if result.ID == d.ID {
					if result.Status == "failed" {
						t.Fatalf("deployment failed: %s\n%s", result.Error, result.Log)
					}
					if result.Status == "live" {
						return
					}
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("deployment timed out")
	}
	request := func(path string) {
		t.Helper()
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://127.0.0.1:" + m.Current(a.ID).Port + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != "persisted" {
			t.Fatalf("private service request %s: %d %s", path, resp.StatusCode, body)
		}
	}
	deploy(false)
	request("/write")
	m.mu.Lock()
	stable := m.data.ClickHouses[a.ID]
	m.mu.Unlock()
	if ports, err := command(ctx, "", nil, "docker", "port", name); err != nil || ports != "" {
		t.Fatal("service ports exposed", err)
	}
	deploy(true)
	request("/read")
	// A stopped service must restart on code redeploy with its original data/key.
	if _, err := command(ctx, "", nil, "docker", "stop", name); err != nil {
		t.Fatal(err)
	}
	deploy(false)
	request("/read")
	m.mu.Lock()
	after := m.data.ClickHouses[a.ID]
	m.mu.Unlock()
	if after != stable {
		t.Fatal("redeploy rotated credentials")
	}
	current := *m.Current(a.ID)
	m.clickhouseProvision = func(context.Context, ClickHouseResource) error { return fmt.Errorf("private diagnostic") }
	d, err := m.Enqueue(a.ID, DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	failed := false
	for time.Now().Before(deadline) && !failed {
		for _, result := range m.Deployments() {
			if result.ID == d.ID && result.Status == "failed" {
				failed = true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !failed || m.Current(a.ID).ID != current.ID {
		t.Fatal("service failure activated a candidate or lost the live release")
	}
	request("/read")
	wrongKeyContext, cancelWrongKey := context.WithTimeout(ctx, 5*time.Second)
	defer cancelWrongKey()
	if err := provisionLocalClickHouse(wrongKeyContext, ClickHouseResource{Network: name, Password: strings.Repeat("a", 64)}); err == nil {
		t.Fatal("wrong retained credentials accepted")
	}
}

func TestDockerClickHouseUnmanagedNameRefused(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for ownership checks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	name := fmt.Sprintf("lidza-clickhouse-occupied-%d", time.Now().UnixNano())
	if _, err := command(ctx, "", nil, "docker", "network", "create", "--label", "io.codex.fixture="+name, name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		out, err := exec.Command("docker", "network", "inspect", "--format", `{{index .Labels "io.codex.fixture"}}`, name).Output()
		if err == nil && strings.TrimSpace(string(out)) == name {
			_ = exec.Command("docker", "network", "rm", name).Run()
		}
	})
	if err := provisionLocalClickHouse(ctx, ClickHouseResource{Network: name, Password: strings.Repeat("b", 64)}); err == nil {
		t.Fatal("unmanaged network taken over")
	}
	if out, err := command(ctx, "", nil, "docker", "volume", "ls", "--filter", "name=^"+name+"-data$", "--format", "{{.Name}}"); err != nil || out != "" {
		t.Fatal("provisioning continued after ownership failure", err)
	}
}
