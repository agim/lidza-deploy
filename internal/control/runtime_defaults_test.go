package control

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestRuntimeDefaultsReadSaveAndValidate(t *testing.T) {
	cfg := Config{PublicURL: "http://127.0.0.1:3000", User: "operator@example.com", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir()}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, body string) (*httptest.ResponseRecorder, defaultsView) {
		r := httptest.NewRequest(method, "/", strings.NewReader(body))
		w := httptest.NewRecorder()
		c.applicationDefaults(w, r)
		var view defaultsView
		json.Unmarshal(w.Body.Bytes(), &view)
		return w, view
	}
	w, view := call("GET", "")
	if w.Code != 200 || len(view.Runtimes) != 4 {
		t.Fatal(w.Code, w.Body)
	}
	if !maps.Equal(view.Values, view.Runtimes["go"].Values) || view.Revision != view.Runtimes["go"].Revision || view.Values["DB_MIGRATE"] != "true" {
		t.Fatal("top level is not the Go set", view)
	}
	if view.Runtimes["rails"].Values["RAILS_ENV"] != "production" || view.Runtimes["rails"].Values["RAILS_SERVE_STATIC_FILES"] != "1" || view.Runtimes["astro"].Values["ASTRO_TELEMETRY_DISABLED"] != "1" || !maps.Equal(view.Runtimes["node"].Values, map[string]string{"NODE_ENV": "production"}) {
		t.Fatal("runtime sets not seeded", view.Runtimes)
	}
	goRevision := view.Revision
	for _, body := range []string{
		`{"runtime":"php","values":{}}`,
		`{"runtime":"rails"}`,
		`{"runtime":"rails","values":{"SECRET_KEY_BASE":"shared"}}`,
		`{"runtime":"rails","values":{"DATABASE_URL":"postgres://shared"}}`,
		`{"runtime":"node","values":{"SENTRY_DSN":"https://shared"}}`,
		`{"runtime":"node","values":{"PATH":"/usr/bin"}}`,
		`{"runtime":"node","values":{"NODE_OPTIONS":"--inspect"}}`,
		`{"runtime":"astro","values":{"LIDZA_ANYTHING":"1"}}`,
		`{"runtime":"astro","values":{"GREETING":"it's"}}`,
		`{"runtime":"astro","values":{"bad-name":"1"}}`,
		`{"runtime":"go","values":{"RAILS_ENV":"production"}}`,
		`{"values":{"NODE_ENV":"production"}}`,
	} {
		if w, _ := call("PUT", body); w.Code != 400 {
			t.Fatal("accepted", body, w.Code)
		}
	}
	many := map[string]string{}
	for i := range agent.MaxRuntimeDefaults + 1 {
		many[fmt.Sprintf("SETTING_%d", i)] = "1"
	}
	raw, _ := json.Marshal(map[string]any{"runtime": "rails", "values": many})
	if w, _ := call("PUT", string(raw)); w.Code != 400 {
		t.Fatal("accepted more than the maximum number of defaults")
	}
	w, saved := call("PUT", `{"runtime":"rails","values":{"RAILS_ENV":"production","TZ":" Europe/Riga ","EMPTY":""}}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if !maps.Equal(saved.Runtimes["rails"].Values, map[string]string{"RAILS_ENV": "production", "TZ": "Europe/Riga"}) || saved.Revision != goRevision || saved.Runtimes["rails"].Revision == view.Runtimes["rails"].Revision {
		t.Fatal("rails save", saved.Runtimes["rails"], saved.Revision)
	}
	// Equal values in two runtimes never share a revision.
	if w, _ := call("PUT", `{"runtime":"astro","values":{"NODE_ENV":"production"}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w, _ := call("PUT", `{"runtime":"node","values":{}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	_, view = call("GET", "")
	if view.Runtimes["astro"].Revision == runtimeDefaultsResponse("node", map[string]string{"NODE_ENV": "production"}).Revision {
		t.Fatal("revision not bound to the runtime")
	}
	// Saved sets, including a deliberately empty one, survive a restart and
	// the Go set is untouched.
	c2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c2.mu.Lock()
	restarted := c2.defaultsViewLocked()
	c2.mu.Unlock()
	if restarted.Runtimes["rails"].Values["TZ"] != "Europe/Riga" || len(restarted.Runtimes["node"].Values) != 0 || restarted.Runtimes["astro"].Values["ASTRO_TELEMETRY_DISABLED"] != "" || restarted.Revision != goRevision {
		t.Fatal("runtime defaults not persisted", restarted.Runtimes)
	}
	// Unchanged Go behaviour without a runtime.
	if w, _ := call("PUT", `{"values":{"LOG_LEVEL":"debug"}}`); w.Code != 200 || c.applicationDefaultsSnapshot()["LOG_LEVEL"] != "debug" {
		t.Fatal("go save", w.Code, w.Body)
	}
}

func TestApplyDefaultsRoutesByApplicationRuntime(t *testing.T) {
	nodeApp := agent.NativeApplication{ID: "native-node", Name: "api", Repository: "acme/api", Branch: "main", Domain: "api.example.com", Runtime: "rails", Services: []string{"api-server.service"}}
	astroApp := agent.NativeApplication{ID: "native-astro", Name: "site", Repository: "acme/site", Branch: "main", Domain: "site.example.com", Runtime: "astro", DocumentRoot: "public"}
	railsApp := agent.NativeApplication{ID: "native-rails", Name: "shop", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", Runtime: "rails", Services: []string{"shop-server.service"}}
	natives := map[string]agent.NativeApplication{nodeApp.ID: nodeApp, astroApp.ID: astroApp, railsApp.ID: railsApp}
	managed := map[string]string{nodeApp.ID: "node"} // release-managed apps and their policy runtime
	patches := map[string]agent.SettingsPatch{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.Split(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1/native/apps/"), "/v1/apps/"), "/")[0]
		native, isNative := natives[id]
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/native/apps/"+id && isNative:
			_, release := managed[id]
			json.NewEncoder(w).Encode(agent.NativeStatus{Application: native, DeploymentSupported: release, ReleaseManaged: release})
		case r.Method == "GET" && r.URL.Path == "/v1/native/apps/"+id+"/settings" && isNative:
			_, release := managed[id]
			editable := release || native.Runtime == "astro"
			json.NewEncoder(w).Encode(agent.Settings{Native: &native, ID: id, Capabilities: &agent.ManagementCapabilities{Configure: editable, Environment: editable}, ReleaseRuntime: managed[id]})
		case r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "/settings"):
			var p agent.SettingsPatch
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &p); err != nil {
				t.Error(err)
			}
			patches[r.URL.Path] = p
			io.WriteString(w, `{"status":"saved"}`)
		default:
			t.Error("unexpected agent call", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer remote.Close()
	c := &Control{client: remote.Client(), registry: []Server{{ID: "host", URL: remote.URL}}, data: saved{
		RuntimeDefaults: map[string]map[string]string{"node": {"NODE_ENV": "production", "TZ": "UTC"}},
		Apps: map[string]Application{
			"go-app":    {ID: "go-app", ServerID: "host", Branch: "main", Domain: "go.example.com", Generation: "g"},
			"rails-app": {ID: "rails-app", ServerID: "host", Runtime: "rails", Branch: "main", Domain: "rails.example.com", Generation: "g"},
			nodeApp.ID:  {ID: nodeApp.ID, ServerID: "host", Runtime: "rails", Native: &nodeApp, Generation: "g"},
			astroApp.ID: {ID: astroApp.ID, ServerID: "host", Runtime: "astro", Native: &astroApp, Generation: "g"},
			railsApp.ID: {ID: railsApp.ID, ServerID: "host", Runtime: "rails", Native: &railsApp, Generation: "g"},
		}}}
	c.mu.Lock()
	profiles := c.defaultsViewLocked().Runtimes
	c.mu.Unlock()
	handler := c.nativeContainerGuard("POST /api/control/apps/{id}/apply-defaults", c.applyDefaults)
	apply := func(id, runtime string, keys []string, replace bool) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]any{"keys": keys, "revision": profiles[runtime].Revision, "replace": replace})
		r := httptest.NewRequest("POST", "/", strings.NewReader(string(raw)))
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	cases := []struct {
		id, runtime, path string
		keys              []string
	}{
		{"go-app", "go", "/v1/apps/go-app/settings", []string{"LOG_LEVEL"}},
		{"rails-app", "rails", "/v1/apps/rails-app/settings", []string{"RAILS_ENV", "RACK_ENV"}},
		{nodeApp.ID, "node", "/v1/native/apps/native-node/settings", []string{"NODE_ENV", "TZ"}},
		{astroApp.ID, "astro", "/v1/native/apps/native-astro/settings", []string{"ASTRO_TELEMETRY_DISABLED"}},
	}
	for _, tc := range cases {
		// A revision reviewed for another runtime's set is refused.
		for _, other := range agent.DefaultsRuntimes {
			if other == tc.runtime {
				continue
			}
			if w := apply(tc.id, other, tc.keys[:1], false); w.Code != 409 {
				t.Fatal(tc.id, "accepted the", other, "revision", w.Code, w.Body)
			}
		}
		if len(patches) != 0 {
			t.Fatal("refused apply reached the agent", patches)
		}
		w := apply(tc.id, tc.runtime, tc.keys, tc.runtime == "astro")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"runtime":"`+tc.runtime+`"`) {
			t.Fatal(tc.id, w.Code, w.Body)
		}
		p, ok := patches[tc.path]
		if !ok || !p.FromDefaults || p.OnlyMissing == (tc.runtime == "astro") || len(p.EnvChanges) != len(tc.keys) {
			t.Fatalf("%s patch %+v", tc.id, patches)
		}
		for _, key := range tc.keys {
			if v := p.EnvChanges[key]; v == nil || *v != profiles[tc.runtime].Values[key] {
				t.Fatal(tc.id, "wrong value for", key)
			}
		}
		if (p.Branch == "") != (natives[tc.id].ID != "") {
			t.Fatal(tc.id, "branch sent to the wrong kind of application", p.Branch)
		}
		delete(patches, tc.path)
	}
	// A native Rails application without a release policy is refused by the
	// guard before any settings change.
	if w := apply(railsApp.ID, "rails", []string{"RAILS_ENV"}, false); w.Code != 409 || len(patches) != 0 {
		t.Fatal("unmanaged native application accepted defaults", w.Code, patches)
	}
	// The settings view tells the console which set applies.
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("id", nodeApp.ID)
	w := httptest.NewRecorder()
	c.settings(w, r)
	if !strings.Contains(w.Body.String(), `"defaults_runtime":"node"`) {
		t.Fatal("settings do not report the defaults runtime", w.Body)
	}
	// First deployments carry the runtime's set; native publishers keep the
	// Go set they ignore.
	for id, want := range map[string]string{"go-app": "", "rails-app": "rails", nodeApp.ID: ""} {
		values, runtime := c.deployDefaults(c.data.Apps[id])
		if runtime != want || (want == "rails") != (values["RAILS_ENV"] == "production") {
			t.Fatal(id, runtime, values)
		}
	}
}
