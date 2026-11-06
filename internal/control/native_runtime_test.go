package control

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

// nativeRuntimeFixture is a panel with one created native application whose
// agent answers status, usage, logs, data and removal.
func nativeRuntimeFixture(t *testing.T, removal int) (*Control, *[]string) {
	t.Helper()
	native := agent.NativeApplication{ID: "shop", Name: "shop", Runtime: "go", Domain: "shop.example.com", Repository: "acme/shop", Branch: "main", Services: []string{"lidza-app-shop-web.service", "lidza-app-shop-jobs.service"}}
	var mu sync.Mutex
	calls := []string{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/native/apps/shop":
			_ = json.NewEncoder(w).Encode(agent.NativeStatus{Application: native, Services: []agent.NativeService{{Unit: native.Services[0], Active: "active", State: "running", UnitFileState: "enabled"}, {Unit: native.Services[1], Active: "inactive", State: "dead", UnitFileState: "disabled"}}})
		case "GET /v1/native/apps/shop/processes":
			io.WriteString(w, `{"services":[{"unit":"lidza-app-shop-web.service","active":"active","state":"running","memory_bytes":104857600,"memory_limit_bytes":536870912,"cpu_percent":12.5,"tasks":7,"restarts":1,"restartable":true,"since":"2026-11-05T10:00:00Z"},{"unit":"lidza-app-shop-jobs.service","active":"inactive","state":"dead","restarts":0,"preserved":true,"restartable":true}]}`)
		case "GET /v1/native/apps/shop/logs":
			io.WriteString(w, `{"log":"journal for `+r.URL.Query().Get("unit")+`"}`)
		case "GET /v1/native/apps/shop/data":
			io.WriteString(w, `{"app_id":"shop","postgres":{"engine":"postgresql","database":"shop","ready":true,"size_bytes":2048}}`)
		case "DELETE /v1/native/apps/shop":
			if removal != 200 {
				http.Error(w, `{"error":"only applications created by this agent can be removed here"}`, removal)
				return
			}
			io.WriteString(w, `{"status":"removed"}`)
		default:
			t.Errorf("unexpected agent call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	c := &Control{cfg: Config{DataDir: t.TempDir(), Key: make([]byte, 32)}, client: remote.Client(), registry: []Server{{ID: "host", URL: remote.URL, Token: "fixture"}},
		data: saved{Apps: map[string]Application{"shop": {ID: "shop", ServerID: "host", Runtime: "go", Native: &native, Domain: native.Domain, Repository: native.Repository, Branch: native.Branch}}}}
	return c, &calls
}

// A native application's processes are its units with their cgroup usage;
// units the agent may restart (service control enabled) get a restart route.
func TestNativeProcessesCarryUsageAndRestartRoute(t *testing.T) {
	c, calls := nativeRuntimeFixture(t, 200)
	r := httptest.NewRequest("GET", "/api/control/apps/shop/processes", nil)
	r.SetPathValue("id", "shop")
	w := httptest.NewRecorder()
	c.applicationProcesses(w, r)
	var out ApplicationProcessInventory
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || len(out.Processes) != 2 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	web, jobs := out.Processes[0], out.Processes[1]
	if web.Runtime != "native" || web.MemoryBytes == nil || *web.MemoryBytes != 100<<20 || web.CPUPercent == nil || *web.CPUPercent != 12.5 || web.Tasks == nil || *web.Tasks != 7 || web.Restarts == nil || *web.Restarts != 1 || web.Since.IsZero() {
		t.Fatalf("web = %+v", web)
	}
	if web.RestartPath != "control/native-apps/shop/services/lidza-app-shop-web.service/restart" || web.Confirm != "shop" || !strings.Contains(web.LogPath, "unit=lidza-app-shop-web.service") {
		t.Fatalf("web control = %+v", web)
	}
	// Deploys leave a preserved unit alone; the operator can restart it.
	if jobs.RestartPath == "" || !jobs.Preserved || jobs.Startup != "disabled" {
		t.Fatalf("a preserved unit is restartable explicitly: %+v", jobs)
	}
	for _, call := range *calls {
		if !strings.HasPrefix(call, "GET ") {
			t.Fatal("reading processes changed a service", call)
		}
	}
}

// Runtime logs of a native application are its units' journal in the
// container log reply shape, optionally for one unit.
func TestNativeRuntimeLogsUseTheContainerReplyShape(t *testing.T) {
	c, _ := nativeRuntimeFixture(t, 200)
	for query, want := range map[string]string{"": "journal for ", "?unit=lidza-app-shop-jobs.service": "journal for lidza-app-shop-jobs.service"} {
		r := httptest.NewRequest("GET", "/api/control/apps/shop/logs"+query, nil)
		r.SetPathValue("id", "shop")
		w := httptest.NewRecorder()
		c.logs(w, r)
		var out struct {
			Logs  string   `json:"logs"`
			Units []string `json:"units"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || out.Logs != want || len(out.Units) != 2 {
			t.Fatalf("%q: %d %s", query, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest("GET", "/api/control/apps/shop/logs?unit=sshd.service", nil)
	r.SetPathValue("id", "shop")
	w := httptest.NewRecorder()
	c.logs(w, r)
	if w.Code != 400 {
		t.Fatalf("a unit outside the application must be refused: %d", w.Code)
	}
}

func TestNativeDataRouteReadsHostDataServices(t *testing.T) {
	c, _ := nativeRuntimeFixture(t, 200)
	r := httptest.NewRequest("GET", "/api/control/native-apps/shop/data", nil)
	r.SetPathValue("id", "shop")
	r.SetPathValue("action", "data")
	w := httptest.NewRecorder()
	c.nativeAppRoute(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"engine":"postgresql"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// Removing a created native application goes to the agent's native
// removal; an adopted application the agent refuses stays registered and
// working, not half-removed.
func TestNativeRemovalUsesNativeAgentRoute(t *testing.T) {
	for _, code := range []int{200, 409} {
		c, calls := nativeRuntimeFixture(t, code)
		handler := c.nativeContainerGuard("DELETE /api/control/apps/{id}", c.retire)
		r := httptest.NewRequest("DELETE", "/api/control/apps/shop", strings.NewReader(`{"confirm":"shop"}`))
		r.SetPathValue("id", "shop")
		w := httptest.NewRecorder()
		handler(w, r)
		if !strings.Contains(strings.Join(*calls, ","), "DELETE /v1/native/apps/shop") {
			t.Fatalf("removal did not reach the native route: %v", *calls)
		}
		a, found := c.app("shop")
		if code == 200 && (w.Code != 200 || found) {
			t.Fatalf("created application not removed: %d %s", w.Code, w.Body)
		}
		if code != 200 && (w.Code == 200 || !found || a.Retiring) {
			t.Fatalf("refused removal must leave the application unchanged: %d %+v", w.Code, a)
		}
	}
}

// Actions that only exist for containers name why and where the native
// counterpart is.
func TestNativeGuardExplainsContainerOnlyActions(t *testing.T) {
	c, _ := nativeRuntimeFixture(t, 200)
	for pattern, want := range map[string]string{
		"POST /api/control/apps/{id}/reload":  "Processes tab",
		"GET /api/control/apps/{id}/cache":    "Redis on the server",
		"POST /api/control/apps/{id}/restore": "creates a container database",
		"GET /api/control/apps/{id}/ports":    "container publications",
	} {
		handler := c.nativeContainerGuard(pattern, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("container action reached a native application", pattern)
		})
		r := httptest.NewRequest("POST", "/", nil)
		r.SetPathValue("id", "shop")
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != 409 || !strings.Contains(w.Body.String(), want) || !strings.Contains(w.Body.String(), "does not apply to a native application") {
			t.Fatalf("%s: %d %s", pattern, w.Code, w.Body)
		}
	}
	handler := c.nativeContainerGuard("PUT /api/control/apps/{id}/{feature}", func(w http.ResponseWriter, r *http.Request) { t.Fatal("state change reached a native application") })
	r := httptest.NewRequest("PUT", "/", nil)
	r.SetPathValue("id", "shop")
	r.SetPathValue("feature", "state")
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "services from the Processes tab") {
		t.Fatalf("state: %d %s", w.Code, w.Body)
	}
}
