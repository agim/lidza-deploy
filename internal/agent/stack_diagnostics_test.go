package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stackDockerFixture(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}
func TestStackImageMismatchPreservesPreviousWriters(t *testing.T) {
	file := filepath.Join(t.TempDir(), "commands")
	stackDockerFixture(t, `echo "$*" >> '`+file+`'
case "$1" in
info) echo linux/x86_64;;
image) echo '{"os":"linux","architecture":"arm64"}';;
*) exit 99;;
esac
`)
	a := testApp("one")
	a.RuntimeProfile = "minimum"
	a.Current = &Release{ID: "previous", Container: "previous-writer"}
	_, err := (&Docker{}).runStack(context.Background(), a, "candidate", "commit", fixtureStackManifest(), map[string]string{"app": "foreign"})
	if err == nil || !strings.Contains(err.Error(), "service app image architecture linux/arm64 is incompatible with Docker host linux/amd64") {
		t.Fatal(err)
	}
	commands, _ := os.ReadFile(file)
	if strings.Contains(string(commands), "stop") || strings.Contains(string(commands), "create") {
		t.Fatal("mutated writers", string(commands))
	}
}
func TestStackImageArchitectureAliasesAndInvalidMetadata(t *testing.T) {
	for _, tc := range []struct {
		host, image string
		ok          bool
	}{
		{"linux/aarch64", `{"os":"linux","architecture":"arm64"}`, true},
		{"linux/amd64/v3", `{"os":"linux","architecture":"amd64"}`, true},
		{"linux/x86_64", `{"os":"windows","architecture":"amd64"}`, false},
		{"linux/x86_64", `{}`, false},
		{"unknown", `{}`, false},
	} {
		t.Run(tc.host+tc.image, func(t *testing.T) {
			stackDockerFixture(t, "case \"$1\" in info) echo '"+tc.host+"';; image) echo '"+tc.image+"';; esac\n")
			err := validateStackImages(context.Background(), StackManifest{Services: []StackService{{Name: "office"}}}, map[string]string{"office": "fixture"})
			if (err == nil) != tc.ok {
				t.Fatal(err)
			}
		})
	}
}
func TestStackBuildRejectsForeignBaseWarning(t *testing.T) {
	stackDockerFixture(t, "echo 'WARN InvalidBaseImagePlatform: linux/arm64 base, expected linux/amd64'\nexit 0\n")
	if err := buildStackImage(context.Background(), "", "office", []string{"build", "."}); err == nil || !strings.Contains(err.Error(), "office Dockerfile base image architecture") {
		t.Fatal(err)
	}
}
func TestStackDiagnosticsExposeSidecarsBehindSetupGate(t *testing.T) {
	stackDockerFixture(t, `case "$1" in
inspect) echo '{"status":"running","exit":0,"oom":false}';;
exec) case "$2" in mail) echo 'clamd socket unavailable: diagnostic-secret';exit 1;; office) echo 'office discovery unavailable';exit 1;; esac;;
logs) echo 'runtime details diagnostic-secret';;
esac
`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(503)
			w.Write([]byte("Server setup is required. Open /setup"))
		}
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	m := testManager(t, &fakeRuntime{})
	a := testApp("one")
	a.Env = map[string]string{"TOKEN": "diagnostic-secret"}
	m.data.Deployments = []Deployment{{ID: "probe"}}
	ctx := m.deploymentDiagnostics(context.Background(), job{app: a, deployment: "probe"})
	r := &Release{Port: port, Stack: &StackRelease{Bootstrap: true, Manifest: StackManifest{BootstrapReadiness: &StackBootstrapReadiness{Path: "/api/v1/health", ReadyzBodyContains: "Server setup is required"}}, Containers: []StackContainer{
		{Container: "app", Service: StackService{Name: "app", HealthCommand: []string{"probe"}}},
		{Container: "mail", Service: StackService{Name: "mail", HealthCommand: []string{"probe"}}},
		{Container: "office", Service: StackService{Name: "office", HealthCommand: []string{"probe"}}},
	}}}
	err := (&Docker{readinessTimeout: 250 * time.Millisecond}).waitStackReady(ctx, r)
	if err == nil || !strings.Contains(err.Error(), "mail") || !strings.Contains(err.Error(), "office") {
		t.Fatal(err)
	}
	output := m.Deployments()[0].Log
	for _, want := range []string{"Service mail; container mail", "Service office; container office", "clamd socket unavailable", "runtime details", "[redacted]", "Server setup is required"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %s in %s", want, output)
		}
	}
	if strings.Contains(output, "diagnostic-secret") {
		t.Fatal("leaked secret", output)
	}
}
func TestStackRestartLoopFailsBeforeDeadline(t *testing.T) {
	stackDockerFixture(t, `case "$1" in inspect) echo '{"status":"restarting","exit":1,"oom":false}';;logs) echo 'exec format error';;esac
`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	r := &Release{Port: port, Stack: &StackRelease{Containers: []StackContainer{{Container: "office", Service: StackService{Name: "office"}}}}}
	start := time.Now()
	err := (&Docker{readinessTimeout: 30 * time.Second}).waitStackReady(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "office") || !strings.Contains(err.Error(), "restarting, exit=1") || time.Since(start) > 10*time.Second {
		t.Fatal(err, time.Since(start))
	}
}
func TestLiveDiagnosticsPersistBeforeDeploymentCompletes(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.data.Deployments = []Deployment{{ID: "probe", Status: "building"}}
	ctx := m.deploymentDiagnostics(context.Background(), job{app: testApp("one"), deployment: "probe"})
	commandDiagnostic(ctx, "build", "still compiling")
	snapshot := &Manager{cfg: m.cfg, key: m.key}
	if err := snapshot.load(); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.data.Deployments) != 1 || !strings.Contains(snapshot.data.Deployments[0].Log, "still compiling") {
		t.Fatal("live log not durable")
	}
	m.lastDiagnosticSave = time.Now().Add(-6 * time.Second)
	stream := ctx.Value(diagnosticStreamKey{}).(func(string) func(string))("docker")
	stream("still building")
	if err := snapshot.load(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot.data.Deployments[0].Log, "still building") {
		t.Fatal("stream not durable")
	}
}
