package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

type updateTransport func(*http.Request) (*http.Response, error)

func (f updateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestFleetUpdatesPersistRetryAndUpgradePanelLast(t *testing.T) {
	token := strings.Repeat("a", 32)
	cfg := Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32)), Servers: []Server{{ID: "local", Name: "GUI", URL: "http://127.0.0.1:9090", Token: token}, {ID: "remote", Name: "Remote", URL: "https://agent.example", Token: token}}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]agent.UpgradeStatus{"local": {Current: "v0.2.14", State: "idle", Supported: true}, "remote": {Current: "v0.2.14", State: "idle", Supported: true}}
	queued := []string{}
	busy := true
	client := &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("agent token not forwarded")
		}
		id := "remote"
		if strings.HasPrefix(r.URL.Host, "127.") {
			id = "local"
		}
		code := 200
		var out any = states[id]
		if r.Method == "POST" {
			var in struct{ Version string }
			json.NewDecoder(r.Body).Decode(&in)
			if in.Version != "v0.2.15" {
				t.Error("wrong release")
			}
			if busy {
				code = 409
				busy = false
				out = map[string]string{"error": "deployment running"}
			} else {
				queued = append(queued, id)
				status := states[id]
				status.State = "queued"
				states[id] = status
				code = 202
				out = map[string]string{"status": "queued"}
			}
		}
		body, _ := json.Marshal(out)
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	attach := func() {
		c.client = client
		c.releaseCheck = func(*http.Request) (string, error) { return "v0.2.15", nil }
	}
	attach()
	r := httptest.NewRequest("POST", "/api/control/updates/install", strings.NewReader(`{}`))
	r.SetPathValue("action", "install")
	w := httptest.NewRecorder()
	c.updates(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	if len(queued) != 0 {
		t.Fatal("request restarted GUI before durable dispatch")
	}
	tick := func() {
		t.Helper()
		if err := c.updateTick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if c.updateSettingsSnapshot().Targets[0].State != "waiting" {
		t.Fatal("busy server was not deferred")
	}
	tick()
	if len(queued) != 1 || queued[0] != "remote" {
		t.Fatal("remote must update first", queued)
	}
	// A panel restart must preserve progress and not queue the remote twice.
	c, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	attach()
	tick()
	if len(queued) != 1 {
		t.Fatal("queued update duplicated", queued)
	}
	status := states["remote"]
	status.Current = "v0.2.15"
	status.State = "succeeded"
	states["remote"] = status
	tick()
	if len(queued) != 2 || queued[1] != "local" {
		t.Fatal("panel host was not last", queued)
	}
	status = states["local"]
	status.Current = "v0.2.15"
	status.State = "succeeded"
	states["local"] = status
	tick()
	if updateInProgress(c.updateSettingsSnapshot()) {
		t.Fatal("completed plan remained active")
	}
	// Failed remote upgrades stop local dispatch and permit a manual retry.
	v := c.updateSettingsSnapshot()
	v.Targets[0].State = "updating"
	v.Targets[1].State = "waiting"
	c.saveUpdateSettings(v)
	status = states["remote"]
	status.Current = "v0.2.14"
	status.State = "failed"
	status.Message = "previous binaries restored"
	states["remote"] = status
	tick()
	if updateInProgress(c.updateSettingsSnapshot()) || len(queued) != 2 {
		t.Fatal("failed batch did not stop")
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/control/updates/install", strings.NewReader(`{}`))
	r.SetPathValue("action", "install")
	c.updates(w, r)
	if w.Code != 202 {
		t.Fatal("retry blocked", w.Code, w.Body)
	}
}
func TestRetiredSelfUpdateHookAndPermissions(t *testing.T) {
	if routePermission("POST /api/control/updates/install") != "infrastructure.manage" || routePermission("PATCH /api/control/updates") != "infrastructure.manage" {
		t.Fatal("fleet update must require administrator")
	}
	c, err := New(Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c.Handler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest("POST", "/hooks/self-update", nil))
	if w.Code != 404 {
		t.Fatal("retired hook still available", w.Code)
	}
}

func TestHourlyChecksArePublicAndAutomaticPolicyPersists(t *testing.T) {
	cfg := Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32))}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	c.releaseCheck = func(*http.Request) (string, error) { checks++; return "v0.2.15", nil }
	if err = c.updateTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if checks != 1 || c.updateSettingsSnapshot().Latest != "v0.2.15" || len(c.updateSettingsSnapshot().Targets) != 0 {
		t.Fatal("notify-only check queued installation")
	}
	if err = c.updateTick(context.Background()); err != nil || checks != 1 {
		t.Fatal("release API polled more often than hourly")
	}
	w := httptest.NewRecorder()
	c.updates(w, httptest.NewRequest("PATCH", "/api/control/updates", strings.NewReader(`{"automatic":true}`)))
	c, err = New(cfg)
	if err != nil || !c.updateSettingsSnapshot().Automatic {
		t.Fatal("automatic preference lost after restart")
	}
}

func TestAutomaticModeQueuesWithoutAnOpenBrowser(t *testing.T) {
	token := strings.Repeat("a", 32)
	c, err := New(Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32)), Servers: []Server{{ID: "local", Name: "GUI", URL: "http://127.0.0.1:9090", Token: token}}})
	if err != nil {
		t.Fatal(err)
	}
	c.releaseCheck = func(*http.Request) (string, error) { return "v0.2.15", nil }
	posts := 0
	c.client = &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"current":"v0.2.14","state":"idle","supported":true}`
		if r.Method == "POST" {
			posts++
			body = `{"status":"queued"}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if err = c.saveUpdateSettings(UpdateSettings{Automatic: true}); err != nil {
		t.Fatal(err)
	}
	if err = c.updateTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := c.updateSettingsSnapshot()
	if posts != 1 || v.Version != "v0.2.15" || len(v.Targets) != 1 || v.Targets[0].State != "updating" {
		t.Fatal("automatic durable tick did not queue CLI upgrade", posts, v)
	}
}
