package agent

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerStackArchitectureAndCrashDiagnostics(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for isolated Docker acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	host, err := nativeStackPlatform(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStackImages(ctx, StackManifest{Services: []StackService{{Name: "native"}}}, map[string]string{"native": "valkey/valkey:8-alpine"}); err != nil {
		t.Fatal("native image rejected", err)
	}
	other := "linux/arm64"
	if host == other {
		other = "linux/amd64"
	}
	tag := "lidza/architecture-fixture:" + newID()
	imported := exec.CommandContext(ctx, "docker", "import", "--platform", other, "-", tag)
	imported.Stdin = bytes.NewReader(make([]byte, 1024))
	if out, err := imported.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	t.Cleanup(func() { _, _ = command(context.Background(), "", nil, "docker", "image", "rm", "--force", tag) })
	err = validateStackImages(ctx, StackManifest{Services: []StackService{{Name: "office"}}}, map[string]string{"office": tag})
	if err == nil || !strings.Contains(err.Error(), "incompatible with Docker host") {
		t.Fatal("foreign actual image accepted", err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "Dockerfile"), []byte("FROM "+tag+"\nLABEL fixture=architecture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	derived := tag + "-derived"
	t.Cleanup(func() { _, _ = command(context.Background(), "", nil, "docker", "image", "rm", "--force", derived) })
	err = buildStackImage(context.WithValue(ctx, diagnosticKey{}, func(line string) { t.Log(line) }), source, "office", []string{"build", "--progress=plain", "--platform", host, "--tag", derived, "."})
	if err == nil {
		t.Fatal("foreign FROM accepted for native target")
	}
	t.Log("Native build rejected foreign FROM:", err)
	name := "lidza-readiness-fixture-" + newID()
	if _, err := command(ctx, "", nil, "docker", "run", "--detach", "--network", "none", "--restart", "unless-stopped", "--name", name, "valkey/valkey:8-alpine", "sh", "-c", "echo 'fatal fixture-secret: exec format error'; exit 1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = command(context.Background(), "", nil, "docker", "rm", "--force", name) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("Server setup is required"))
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	m := testManager(t, &fakeRuntime{})
	a := testApp("one")
	a.Env = map[string]string{"TOKEN": "fixture-secret"}
	m.data.Deployments = []Deployment{{ID: "probe"}}
	probe := m.deploymentDiagnostics(ctx, job{app: a, deployment: "probe"})
	started := time.Now()
	err = (&Docker{readinessTimeout: 30 * time.Second}).waitStackReady(probe, &Release{Port: port, Stack: &StackRelease{Containers: []StackContainer{{Container: name, Service: StackService{Name: "office", HealthCommand: []string{"true"}}}}}})
	if err == nil || time.Since(started) > 15*time.Second {
		t.Fatal("actual crash loop did not fail promptly", err, time.Since(started))
	}
	output := m.Deployments()[0].Log
	for _, want := range []string{"Service office; container ", "exit=1", "OOMKilled=false", "exec format error", "[redacted]"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %s: %s", want, output)
		}
	}
	if strings.Contains(output, "fixture-secret") {
		t.Fatal("secret leaked")
	}
	t.Log("Actual foreign image and foreign FROM rejected; actual crashing candidate identified promptly with bounded redacted exit/OOM/restart/runtime evidence")
}
