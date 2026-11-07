package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

type lifecycleAgent struct {
	mu     sync.Mutex
	calls  []string
	legacy bool
}

func (a *lifecycleAgent) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.calls = append(a.calls, r.Method+" "+r.URL.Path)
		legacy := a.legacy
		a.mu.Unlock()
		switch {
		case legacy && r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/stop") || strings.HasSuffix(r.URL.Path, "/start") || strings.HasSuffix(r.URL.Path, "/restart")):
			http.NotFound(w, r)
		case r.Method == http.MethodPost:
			var req agent.AppLifecycleRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			agent.JSON(w, 200, agent.AppLifecycleResult{AppID: "x", Status: map[string]string{"stop": "stopped", "start": "starting", "restart": "restarting"}[action], Message: req.By})
		default:
			agent.JSON(w, 200, map[string]string{"status": "ok"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (a *lifecycleAgent) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		return ""
	}
	return a.calls[len(a.calls)-1]
}

func lifecycleRequest(c *Control, action, id string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/control/apps/"+id+"/"+action, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	c.appLifecycle(action)(w, r)
	return w
}

func TestAppLifecycleRoutesForwardForEveryRuntime(t *testing.T) {
	remote := &lifecycleAgent{}
	srv := remote.server(t)
	c := &Control{client: srv.Client(), registry: []Server{{ID: "alpha", URL: srv.URL}}, data: saved{Apps: map[string]Application{
		"portal": {ID: "portal", ServerID: "alpha"},
		"rails":  {ID: "rails", ServerID: "alpha", Native: &agent.NativeApplication{ID: "rails", Name: "Rails", Runtime: "rails"}},
	}}}
	for _, id := range []string{"portal", "rails"} {
		for _, action := range agentLifecycleActions {
			w := lifecycleRequest(c, action, id)
			if w.Code != 200 || remote.last() != "POST /v1/apps/"+id+"/"+action {
				t.Fatalf("%s %s: %d %s (%s)", action, id, w.Code, w.Body, remote.last())
			}
			var result agent.AppLifecycleResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Status == "" {
				t.Fatalf("%s %s: %s", action, id, w.Body)
			}
		}
	}
	if w := lifecycleRequest(c, "stop", "missing"); w.Code != 404 {
		t.Fatalf("missing app: %d", w.Code)
	}
	// Every lifecycle route passes the native runtime guard.
	for pattern := range appLifecyclePatterns {
		called := false
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.SetPathValue("id", "rails")
		c.nativeContainerGuard(pattern, func(http.ResponseWriter, *http.Request) { called = true })(httptest.NewRecorder(), r)
		if !called {
			t.Fatalf("%s is refused for native applications", pattern)
		}
	}
}

func TestAppLifecycleIsRefusedDuringAnyMove(t *testing.T) {
	remote := &lifecycleAgent{}
	srv := remote.server(t)
	c := &Control{client: srv.Client(), registry: []Server{{ID: "alpha", URL: srv.URL}, {ID: "beta", Name: "Beta", URL: srv.URL}}, data: saved{
		Apps:  map[string]Application{"portal": {ID: "portal", ServerID: "alpha"}},
		Moves: map[string]MoveJob{"m1": {ID: "m1", AppID: "portal", SourceServer: "alpha", TargetServer: "beta", State: moveStateRunning, Created: time.Now().UTC()}},
	}}
	for _, action := range agentLifecycleActions {
		w := lifecycleRequest(c, action, "portal")
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "moving to Beta") || !strings.Contains(w.Body.String(), "finish or abort the move") {
			t.Fatalf("%s during a move: %d %s", action, w.Code, w.Body)
		}
	}
	if remote.last() != "" {
		t.Fatalf("the agent was called during a move: %s", remote.last())
	}
}

func TestAppLifecycleFallsBackForOlderAgents(t *testing.T) {
	remote := &lifecycleAgent{legacy: true}
	srv := remote.server(t)
	c := &Control{client: srv.Client(), registry: []Server{{ID: "alpha", URL: srv.URL}}, data: saved{Apps: map[string]Application{
		"portal": {ID: "portal", ServerID: "alpha"},
		"rails":  {ID: "rails", ServerID: "alpha", Native: &agent.NativeApplication{ID: "rails", Name: "Rails"}},
	}}}
	if w := lifecycleRequest(c, "stop", "portal"); w.Code != 200 || remote.last() != "PUT /v1/apps/portal/state" {
		t.Fatalf("legacy stop: %d %s %s", w.Code, w.Body, remote.last())
	}
	if w := lifecycleRequest(c, "restart", "portal"); w.Code != 200 || remote.last() != "POST /v1/apps/portal/reload" {
		t.Fatalf("legacy restart: %d %s %s", w.Code, w.Body, remote.last())
	}
	if w := lifecycleRequest(c, "stop", "rails"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "update it first") {
		t.Fatalf("legacy native stop: %d %s", w.Code, w.Body)
	}
}
