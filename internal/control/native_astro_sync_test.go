package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestNativeAstroRegistersMetadataAutomaticallyOnceAndPreservesExistingGo(t *testing.T) {
	c, server, _ := nativeInventoryFixture(t)
	goApp := Application{ID: "go", Runtime: "", Repository: "org/go", Domain: "go.example.com", ServerID: "source", AutoDeploy: true, Generation: "preserve"}
	c.data.Apps[goApp.ID] = goApp
	a := agent.NativeApplication{ID: "astro-marketing", Name: "marketing", Runtime: "astro", DocumentRoot: "public", Repository: "org/marketing", Branch: "main", Domain: "example.com", AdditionalDomains: []string{"www.example.com"}, HealthPath: "/", Services: []string{}}
	registered := false
	posts := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/native/capabilities":
			json.NewEncoder(w).Encode(map[string]bool{"status_metadata_only": true, "automatic_registration_metadata_only": true})
		case "GET /v1/native/discovery":
			json.NewEncoder(w).Encode([]agent.NativeApplication{a})
		case "GET /v1/apps":
			apps := []agent.App{}
			if registered {
				apps = append(apps, agent.App{ID: a.ID, Native: &a, Runtime: "astro", Repository: a.Repository, Branch: a.Branch, Domain: a.Domain, AdditionalDomains: a.AdditionalDomains})
			}
			json.NewEncoder(w).Encode(apps)
		case "POST /v1/native/apps":
			posts++
			var in agent.NativeApplication
			json.NewDecoder(r.Body).Decode(&in)
			if !sameNativeApplication(in, a) {
				t.Error("registration metadata changed")
			}
			registered = true
			json.NewEncoder(w).Encode(a)
		default:
			t.Error("unexpected host operation", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	server.URL = remote.URL
	c.registry = []Server{server}
	c.data.Servers = []Server{server}
	for range 2 {
		c.syncNativeInventory(context.Background())
	}
	if posts != 1 || len(c.data.Apps) != 2 || len(c.data.ObservedApps) != 0 || c.nativeSync[server.ID].State != "synced" {
		t.Fatal("automatic registration duplicated, was not durable, or failed", posts, c.nativeSync)
	}
	app := c.data.Apps[a.ID]
	if app.Runtime != "astro" || app.AutoDeploy || app.HookID != 0 || c.data.Apps[goApp.ID].Generation != "preserve" || !c.data.Apps[goApp.ID].AutoDeploy {
		t.Fatal("Astro imported with incorrect runtime or changed Go deployment preferences")
	}
}

func TestNativeProcessAdapterShowsDisabledStateWithoutContainerOrActionCalls(t *testing.T) {
	c, server, a := nativeInventoryFixture(t)
	c.data.Apps[a.ID] = Application{ID: a.ID, Native: &a, ServerID: server.ID}
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/v1/native/apps/"+a.ID {
			t.Error("native process view called container or mutation API", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(agent.NativeStatus{Application: a, Services: []agent.NativeService{{Unit: a.Services[0], Active: "inactive", State: "dead", UnitFileState: "disabled"}}})
	}))
	defer remote.Close()
	server.URL = remote.URL
	c.registry = []Server{server}
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	c.applicationProcesses(w, r)
	var out ApplicationProcessInventory
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || calls != 1 || len(out.Processes) != 1 || out.Processes[0].Startup != "disabled" || out.Processes[0].State != "inactive" {
		t.Fatal("native process metadata contract", w.Code, w.Body.String())
	}
}
