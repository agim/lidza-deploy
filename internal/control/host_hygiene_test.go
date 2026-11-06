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

func TestBundlerRemovalProxiesValidatedSelections(t *testing.T) {
	if routePermission("POST /api/control/servers/{server}/host/ruby/bundlers/remove") != "infrastructure.manage" {
		t.Fatal("Bundler removal must be restricted to infrastructure managers")
	}
	fx := newMoveFixture(t)
	var mu sync.Mutex
	var received []agent.BundlerRemoval
	fx.b.mux.HandleFunc("GET /v1/host/ruby/bundlers", func(w http.ResponseWriter, r *http.Request) {
		agent.JSON(w, 200, agent.BundlerCleanupStatus{Groups: []agent.BundlerGroup{{User: "deploy", Ruby: "3.3.0", RubyGems: "3.6.9", Default: "2.6.9", Apps: []string{"shop"}, Remove: []string{"2.3.5"}}}})
	})
	fx.b.mux.HandleFunc("POST /v1/host/ruby/bundlers/remove", func(w http.ResponseWriter, r *http.Request) {
		var in agent.BundlerRemoval
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		mu.Lock()
		received = append(received, in)
		mu.Unlock()
		agent.JSON(w, 202, agent.BundlerCleanupStatus{Groups: []agent.BundlerGroup{}, Job: &agent.HostJob{Kind: "bundler-cleanup", Status: "running"}})
	})
	call := func(method, target, body string) (int, agent.BundlerCleanupStatus) {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.SetPathValue("server", "beta")
		w := httptest.NewRecorder()
		fx.c.hostAdmin(w, req)
		var out agent.BundlerCleanupStatus
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, out := call("GET", "/api/control/servers/beta/host/ruby/bundlers", ""); code != 200 || len(out.Groups) != 1 || out.Groups[0].Remove[0] != "2.3.5" {
		t.Fatalf("plan: %d %+v", code, out)
	}
	for _, bad := range []string{`{"remove":[]}`, `{"remove":[{"user":"deploy","ruby":"3.3.0","version":"2.3.5 && reboot"}]}`, `{"other":1}`} {
		if code, _ := call("POST", "/api/control/servers/beta/host/ruby/bundlers/remove", bad); code != 400 {
			t.Fatalf("%s: %d", bad, code)
		}
	}
	if len(received) != 0 {
		t.Fatal("an invalid selection reached the agent")
	}
	code, out := call("POST", "/api/control/servers/beta/host/ruby/bundlers/remove", `{"remove":[{"user":"deploy","ruby":"3.3.0","version":"2.3.5"}]}`)
	if code != 202 || out.Job == nil || len(received) != 1 || received[0].Remove[0].Version != "2.3.5" {
		t.Fatalf("remove: %d %+v %+v", code, out, received)
	}
}

func TestCleanupPreviewReachesTheAgent(t *testing.T) {
	fx := newMoveFixture(t)
	fx.b.mux.HandleFunc("GET /v1/maintenance", func(w http.ResponseWriter, r *http.Request) {
		state := agent.MaintenanceState{}
		if r.URL.Query().Get("preview") == "1" {
			state.Preview = &agent.CleanupPreview{Items: []agent.CleanupItem{{Kind: "image", Name: "lidza/shop:d1", Bytes: 1000}}, Bytes: 1000}
		}
		agent.JSON(w, 200, state)
	})
	for _, target := range []string{"/api/control/servers/beta/maintenance?preview=1", "/api/control/servers/beta/maintenance"} {
		req := httptest.NewRequest("GET", target, nil)
		req.SetPathValue("server", "beta")
		w := httptest.NewRecorder()
		fx.c.serverMaintenance(w, req)
		var out agent.MaintenanceState
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal(target, w.Code, w.Body)
		}
		if wantPreview := strings.Contains(target, "preview"); (out.Preview != nil) != wantPreview || wantPreview && out.Preview.Bytes != 1000 {
			t.Fatalf("%s: %+v", target, out.Preview)
		}
	}
}
