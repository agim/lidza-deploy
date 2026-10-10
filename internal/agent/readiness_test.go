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

func TestFailedCandidateDiagnosticsSavedBeforeRemoval(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("database unavailable: runtime-secret"))
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	os.Mkdir(bin, 0755)
	// Simulated Docker only: never touches real containers or deployment hosts.
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HOME/actions\"\ncase \"$1\" in\ncreate) echo candidate-id;;\nstart) echo candidate;;\nport) echo 127.0.0.1:" + port + ";;\ninspect) echo '{\"Status\":\"restarting\",\"ExitCode\":1,\"OOMKilled\":false}' ;;\nlogs) echo 'FATAL database authentication failed: runtime-secret';;\ncontainer) echo candidate-id;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	m := testManager(t, &fakeRuntime{})
	app := testApp("one")
	app.Env = map[string]string{"DATABASE_PASSWORD": "runtime-secret"}
	app.Current = &Release{ID: "previous", Container: "previous-container", Image: "lidza/one:previous"}
	m.data.Deployments = []Deployment{{ID: "readiness-failure"}}
	ctx := m.deploymentDiagnostics(context.Background(), job{app: app, deployment: "readiness-failure"})
	docker := &Docker{Root: t.TempDir(), readinessTimeout: 80 * time.Millisecond}
	if _, err := docker.runImage(ctx, app, "candidate", "lidza/one:candidate", "commit"); err == nil || !strings.Contains(err.Error(), "previous release retained") {
		t.Fatal("unready candidate accepted", err)
	}
	log := m.Deployments()[0].Log
	for _, want := range []string{"HTTP 503", "database unavailable", "FATAL database authentication failed", "OOMKilled", "[redacted]"} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q: %s", want, log)
		}
	}
	if strings.Contains(log, "runtime-secret") {
		t.Fatal("secret exposed in candidate diagnostics")
	}
	actions, _ := os.ReadFile(filepath.Join(home, "actions"))
	text := string(actions)
	if strings.Index(text, "logs --tail") < 0 || strings.Index(text, "logs --tail") > strings.Index(text, "rm --force") {
		t.Fatalf("logs collected after deletion: %s", text)
	}
	if strings.Contains(text, "previous-container") || strings.Contains(text, "lidza/one:previous") {
		t.Fatal("previous release changed")
	}
}

func TestReadinessLogsSuccessfulTransitionAndBoundsFailureBody(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.data.Deployments = []Deployment{{ID: "probe"}}
	app := testApp("one")
	app.Env = map[string]string{"TOKEN": "runtime-secret"}
	ctx := m.deploymentDiagnostics(context.Background(), job{app: app, deployment: "probe"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("runtime-secret")) }))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	docker := &Docker{}
	if err := docker.waitReady(ctx, &Release{Port: port}); err != nil {
		t.Fatal(err)
	}
	log := m.Deployments()[0].Log
	if !strings.Contains(log, "Candidate passed /readyz") || strings.Contains(log, "runtime-secret") {
		t.Fatal("missing success or leaked success body", log)
	}
	failure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(strings.Repeat("x", 10000)))
	}))
	defer failure.Close()
	_, port, _ = net.SplitHostPort(strings.TrimPrefix(failure.URL, "http://"))
	body, err := docker.probeReady(context.Background(), &Release{Port: port})
	if err == nil || len(body) != 4096 || strings.Contains(err.Error(), strings.Repeat("x", 10)) {
		t.Fatal("response unbounded or included in returned error", len(body), err)
	}
}

func TestCancelledDeploymentStillCapturesStartupLogs(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, "docker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncase \"$1\" in inspect) echo '{\"OOMKilled\":true}';; logs) echo 'startup failed';;esac\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", home+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var log string
	ctx = context.WithValue(ctx, diagnosticKey{}, func(line string) { log += line })
	(&Docker{}).captureCandidateFailure(ctx, &Release{Container: "candidate"})
	if !strings.Contains(log, "startup failed") || !strings.Contains(log, "OOMKilled") {
		t.Fatal("cancelled build lost diagnostics", log)
	}
}
