package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerApplicationStopStart(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name := "lidza-stop-test-" + newID()
	worker := name + "-worker"
	image := name + ":fixture"
	source := t.TempDir()
	binary := filepath.Join(source, "app")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../tests/fixture")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if err := os.WriteFile(filepath.Join(source, "Dockerfile"), []byte("FROM scratch\nCOPY --chmod=755 app /app\nENTRYPOINT [\"/app\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, "", nil, "docker", "build", "-t", image, source); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = command(context.Background(), "", nil, "docker", "image", "rm", "-f", image) })
	for _, container := range []string{name, worker} {
		args := []string{"run", "-d", "--name", container, "--restart", "unless-stopped"}
		if container == name {
			args = append(args, "--publish", "127.0.0.1::3000")
		}
		if container == name {
			args = append(args, "--env", "LIDZA_ADDR=0.0.0.0:3000", "--env", "LIDZA_MODE=production", "--env", "RELEASE_TEXT=before", image)
		} else {
			args = append(args, "caddy:2.10.2-alpine", "caddy", "respond", "--listen", ":3000", "--body", "app running")
		}
		if _, err := command(ctx, "", nil, "docker", args...); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = command(context.Background(), "", nil, "docker", "rm", "-f", container) })
	}
	d := &Docker{Root: t.TempDir()}
	ports, err := d.PublishedPorts(ctx, []string{name})
	if err != nil {
		t.Fatal(err)
	}
	m := testManager(t, d)
	a := testApp("stop-test")
	a.Current = &Release{ID: "one", Container: name, Port: ports[name], Image: image}
	a.Previous = &Release{ID: "missing", Container: name + "-missing"}
	m.mu.Lock()
	m.data.Apps[a.ID] = a
	m.data.Tasks[taskKey(a.ID, "worker")] = Task{ID: "worker", AppID: a.ID, Mode: "worker", Enabled: true, Running: true, Container: worker}
	m.mu.Unlock()
	stopped := true
	if err := m.setApplicationState(a.ID, ApplicationState{&stopped}); err != nil {
		t.Fatal(err)
	}
	for _, container := range []string{name, worker} {
		output, err := command(ctx, "", nil, "docker", "inspect", "--format", "{{.State.Running}} {{.HostConfig.RestartPolicy.Name}}", container)
		if err != nil || strings.TrimSpace(output) != "false unless-stopped" {
			t.Fatalf("container did not stop: %q %v", output, err)
		}
	}
	value := "after"
	if err := m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, EnvChanges: map[string]*string{"RELEASE_TEXT": &value}}); err != nil {
		t.Fatal(err)
	}
	m.Close()
	restarted, err := NewManager(ctx, m.cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	defer func() {
		for _, a := range restarted.Apps() {
			_ = d.Remove(context.Background(), a.Current)
			_ = d.Remove(context.Background(), a.Previous)
		}
	}()
	if restarted.Target(a.Domain) != nil {
		t.Fatal("agent restart exposed stopped app")
	}
	stopped = false
	if err := restarted.setApplicationState(a.ID, ApplicationState{&stopped}); err != nil {
		t.Fatal(err)
	}
	for _, deployment := range restarted.Deployments() {
		if deployment.Kind == "start" {
			result := waitDeployment(t, restarted, deployment.ID)
			if result.Status != "live" {
				t.Fatal(result.Error)
			}
		}
	}
	w := httptest.NewRecorder()
	Proxy(restarted).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://"+a.Domain+"/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "after") {
		t.Fatal("restarted app not available", w.Code, w.Body)
	}
	if restarted.taskList(a.ID)[0].Paused || !restarted.taskList(a.ID)[0].Enabled {
		t.Fatal("lost worker resume policy")
	}
}
