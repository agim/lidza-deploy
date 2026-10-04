package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/agim/lidza/pkg/credentials"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRuntime struct {
	mu      sync.Mutex
	fail    bool
	removed int
	gate    chan struct{}
}

func (f *fakeRuntime) Deploy(ctx context.Context, a App, id, token string) (*Release, error) {
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("readiness failed")
	}
	return &Release{ID: id, Commit: id, Port: "12345", Container: id}, nil
}
func (f *fakeRuntime) Remove(context.Context, *Release) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed++
	return nil
}
func (f *fakeRuntime) Ready(context.Context, *Release) error          { return nil }
func (f *fakeRuntime) Logs(context.Context, *Release) (string, error) { return "test logs", nil }
func testManager(t *testing.T, f Runtime) *Manager {
	t.Helper()
	m, err := NewManager(context.Background(), Config{DataDir: t.TempDir(), APIKey: strings.Repeat("a", 32)}, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}
func testApp(id string) App {
	return App{ID: id, Repository: "acme/" + id, Branch: "main", Domain: id + ".example.com", Env: map[string]string{"APP_SECRET": "runtime-secret"}}
}
func waitDeployment(t *testing.T, m *Manager, id string) Deployment {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range m.Deployments() {
			if d.ID == id && (d.Status == "live" || d.Status == "failed") {
				return d
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("deployment did not finish")
	return Deployment{}
}
func TestDeploymentLifecycle(t *testing.T) {
	f := &fakeRuntime{}
	m := testManager(t, f)
	if err := m.Upsert(testApp("first")); err != nil {
		t.Fatal(err)
	}
	d, err := m.Enqueue("first", DeployRequest{Token: "private-secret", Key: "delivery-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitDeployment(t, m, d.ID); got.Status != "live" {
		t.Fatal(got)
	}
	one := m.Current("first").ID
	duplicate, err := m.Enqueue("first", DeployRequest{Key: "delivery-1"})
	if err != nil || duplicate.ID != d.ID {
		t.Fatal("delivery not deduplicated")
	}
	d, err = m.Enqueue("first", DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waitDeployment(t, m, d.ID)
	two := m.Current("first").ID
	if one == two {
		t.Fatal("redeploy reused release")
	}
	if err = m.Rollback(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if m.Current("first").ID != one {
		t.Fatal("rollback did not restore first release")
	}
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	d, err = m.Enqueue("first", DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if waitDeployment(t, m, d.ID).Status != "failed" {
		t.Fatal("failed candidate accepted")
	}
	if m.Current("first").ID != one {
		t.Fatal("failed candidate displaced healthy release")
	}
	data, err := os.ReadFile(m.path())
	if err != nil {
		t.Fatal(err)
	}
	var sealed string
	if err := json.Unmarshal(data, &sealed); err != nil {
		t.Fatal(err)
	}
	data, err = credentials.Decrypt(m.key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-secret") {
		t.Fatal("GitHub token persisted")
	}
	if m.Apps()[0].Env != nil {
		t.Fatal("runtime secrets exposed in list")
	}
	m.Close()
	restored, err := NewManager(context.Background(), m.cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.Current("first").ID != one {
		t.Fatal("active release lost across restart")
	}
}
func TestQueueAndDomainIsolation(t *testing.T) {
	gate := make(chan struct{})
	m := testManager(t, &fakeRuntime{gate: gate})
	if err := m.Upsert(testApp("one")); err != nil {
		t.Fatal(err)
	}
	other := testApp("two")
	other.Domain = "one.example.com"
	if err := m.Upsert(other); err == nil {
		t.Fatal("duplicate domain accepted")
	}
	other.Domain = "two.example.com"
	if err := m.Upsert(other); err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for i := 0; i < 30; i++ {
		if _, err := m.Enqueue("one", DeployRequest{}); err == nil {
			accepted++
		}
	}
	if accepted < 16 || accepted > 17 {
		t.Fatalf("unbounded queue: %d", accepted)
	}
	if err := m.Upsert(testApp("one")); err == nil {
		t.Fatal("configuration changed during deployment")
	}
	close(gate)
}
func TestAuthTLSAndProxyIsolation(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if err := m.Upsert(testApp("one")); err != nil {
		t.Fatal(err)
	}
	h := Handler(m)
	for _, token := range []string{"", "Bearer ", "Bearer wrong"} {
		r := httptest.NewRequest("GET", "/v1/apps", nil)
		r.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("unauthorized status %d", w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/v1/apps", nil)
	r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "runtime-secret") {
		t.Fatal(w.Body.String())
	}
	for _, tc := range []struct {
		host, remote string
		code         int
	}{{"one.example.com", "127.0.0.1:5000", 200}, {"unknown.example.com", "127.0.0.1:5000", 403}, {"one.example.com", "192.0.2.1:5000", 403}} {
		r := httptest.NewRequest("GET", "/tls/allow?domain="+tc.host, nil)
		r.RemoteAddr = tc.remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("TLS authorization: %d", w.Code)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "one: "+r.Host) }))
	defer upstream.Close()
	m.mu.Lock()
	a := m.data.Apps["one"]
	a.Current = &Release{Port: strings.TrimPrefix(upstream.URL, "http://127.0.0.1:")}
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	proxy := Proxy(m)
	r = httptest.NewRequest("GET", "http://one.example.com/", nil)
	w = httptest.NewRecorder()
	proxy.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "one: one.example.com" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, url := range []string{"http://other.example.com/", "http://one.example.com/metrics", "http://one.example.com/readyz"} {
		r = httptest.NewRequest("GET", url, nil)
		w = httptest.NewRecorder()
		proxy.ServeHTTP(w, r)
		if w.Code == 200 {
			t.Fatal("unregistered domain or internal endpoint exposed")
		}
	}
}
func TestValidation(t *testing.T) {
	for _, mutate := range []func(*App){func(a *App) { a.ID = "../x" }, func(a *App) { a.Repository = "https://user:token@github.com/a/b" }, func(a *App) { a.Branch = "--upload-pack=evil" }, func(a *App) { a.Domain = "foo.-bad.com" }, func(a *App) { a.Domain = "127.0.0.1" }, func(a *App) { a.Env["X"] = "value\nOTHER=bad" }, func(a *App) { a.Env["LIDZA_TLS_DOMAINS"] = "evil.example.com" }} {
		a := testApp("app")
		mutate(&a)
		if a.Validate() == nil {
			t.Fatalf("accepted invalid app: %+v", a)
		}
	}
}

func TestGitCredentialsAreEphemeralAndErrorsRedacted(t *testing.T) {
	dir := t.TempDir()
	git := filepath.Join(dir, "git")
	script := `#!/bin/sh
[ "$GIT_TERMINAL_PROMPT" = 0 ] || exit 10
[ "$GIT_CONFIG_VALUE_0" = "" ] || exit 11
[ "$GIT_CONFIG_VALUE_1" = false ] || exit 12
[ "$("$GIT_ASKPASS" Username)" = x-access-token ] || exit 13
[ "$("$GIT_ASKPASS" Password)" = "$LIDZA_CLONE_TOKEN" ] || exit 14
[ -z "$CONTROL_PASSWORD" ] || exit 15
case "$*" in *private-fixture-token*) exit 16 ;; esac
case "$*" in *https://github.com/acme/app.git*) ;; *) exit 17 ;; esac
exit 0
`
	if err := os.WriteFile(git, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("CONTROL_PASSWORD", "control-secret")
	a := testApp("app")
	if err := clone(context.Background(), a, filepath.Join(dir, "source"), "private-fixture-token"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "askpass"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-fixture-token") {
		t.Fatal("credential persisted in helper")
	}
	if err = os.WriteFile(git, []byte("#!/bin/sh\nprintf '%s' \"$LIDZA_CLONE_TOKEN\" >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	err = clone(context.Background(), a, filepath.Join(dir, "source"), "private-fixture-token")
	if err == nil || strings.Contains(err.Error(), "private-fixture-token") {
		t.Fatal("command error exposed credential")
	}
}
