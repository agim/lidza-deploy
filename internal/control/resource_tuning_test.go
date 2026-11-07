package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestResourceTuningRoutesProxyToTheAgent(t *testing.T) {
	for _, pattern := range []string{
		"PUT /api/control/servers/{server}/host/resources/overrides",
		"POST /api/control/servers/{server}/host/resources/apply",
		"POST /api/control/servers/{server}/host/resources/redis/{app}/migrate",
	} {
		if routePermission(pattern) != "infrastructure.manage" {
			t.Fatalf("%s is not restricted to infrastructure managers", pattern)
		}
	}
	if routePermission("GET /api/control/servers/{server}/host/resources") != "fleet.read" {
		t.Fatal("reading tuning is fleet access")
	}
	fx := newMoveFixture(t)
	var mu sync.Mutex
	stored := agent.TuningOverrides{}
	calls := []string{}
	view := func() agent.ResourceTuningView {
		return agent.ResourceTuningView{Supported: true, Overrides: stored, PendingRestart: []string{"shared_buffers"}}
	}
	fx.b.mux.HandleFunc("GET /v1/host/resources", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, "GET "+r.URL.RawQuery)
		agent.JSON(w, 200, view())
	})
	fx.b.mux.HandleFunc("PUT /v1/host/resources/overrides", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&stored)
		calls = append(calls, "PUT")
		agent.JSON(w, 200, view())
	})
	fx.b.mux.HandleFunc("POST /v1/host/resources/apply", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var in agent.ResourceTuningApply
		_ = json.NewDecoder(r.Body).Decode(&in)
		calls = append(calls, "APPLY dry="+map[bool]string{true: "1", false: "0"}[in.DryRun])
		agent.JSON(w, 200, view())
	})
	fx.b.mux.HandleFunc("POST /v1/host/resources/redis/{app}/migrate", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, "MIGRATE "+r.PathValue("app"))
		agent.JSON(w, 202, view())
	})
	call := func(method, target, app, body string) (int, agent.ResourceTuningView) {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.SetPathValue("server", "beta")
		if app != "" {
			req.SetPathValue("app", app)
		}
		w := httptest.NewRecorder()
		fx.c.hostAdmin(w, req)
		var out agent.ResourceTuningView
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, out := call("GET", "/api/control/servers/beta/host/resources?refresh=1", "", ""); code != 200 || !out.Supported || out.PendingRestart[0] != "shared_buffers" {
		t.Fatalf("read: %d %+v", code, out)
	}
	// Invalid overrides never reach the agent.
	if code, _ := call("PUT", "/api/control/servers/beta/host/resources/overrides", "", `{"postgres":{"wal_level":"minimal"}}`); code != 400 {
		t.Fatalf("invalid override: %d", code)
	}
	if code, out := call("PUT", "/api/control/servers/beta/host/resources/overrides", "", `{"postgres":{"shared_buffers":"4GB"},"connection_caps":{"shop":40}}`); code != 200 || out.Overrides.Postgres["shared_buffers"] != "4GB" {
		t.Fatalf("override: %d %+v", code, out)
	}
	if code, _ := call("POST", "/api/control/servers/beta/host/resources/apply", "", `{"dry_run":true}`); code != 200 {
		t.Fatalf("dry run: %d", code)
	}
	if code, _ := call("POST", "/api/control/servers/beta/host/resources/apply", "", `{}`); code != 202 {
		t.Fatalf("apply: %d", code)
	}
	if code, _ := call("POST", "/api/control/servers/beta/host/resources/redis/shop/migrate", "shop", `{"confirm":"other"}`); code != 400 {
		t.Fatalf("unconfirmed move: %d", code)
	}
	if code, _ := call("POST", "/api/control/servers/beta/host/resources/redis/shop/migrate", "shop", `{"confirm":"shop"}`); code != 202 {
		t.Fatalf("move: %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"GET refresh=1", "PUT", "APPLY dry=1", "APPLY dry=0", "MIGRATE shop"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("agent calls %v, want %v", calls, want)
	}
}
