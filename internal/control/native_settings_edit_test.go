package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestNativeSettingsUseApprovedNativeEndpointAndPreserveHostIdentity(t *testing.T) {
	n := agent.NativeApplication{ID: "astro-site", Name: "site", Runtime: "astro", Repository: "org/site", Branch: "main", Domain: "example.com", DocumentRoot: "public"}
	writes := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/native/apps/astro-site/settings" {
			t.Error("native edit used a container endpoint", r.URL.Path)
			http.Error(w, "wrong adapter", 409)
			return
		}
		if r.Method == "PATCH" {
			writes++
			var p agent.SettingsPatch
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Branch != "preview" {
				t.Error("lost settings patch", err)
			}
		}
		branch := "main"
		if r.Method == "PATCH" {
			branch = "preview"
		}
		json.NewEncoder(w).Encode(agent.Settings{ID: n.ID, Native: &n, Branch: branch, Domain: n.Domain, Astro: &agent.AstroConfig{Mode: "static", OutputDir: "dist"}, Capabilities: &agent.ManagementCapabilities{Configure: true, Environment: true}})
	}))
	defer remote.Close()
	c := &Control{cfg: Config{DataDir: t.TempDir(), Key: make([]byte, 32)}, client: remote.Client(), registry: []Server{{ID: "host", URL: remote.URL, Token: "fixture"}}, data: saved{Apps: map[string]Application{n.ID: {ID: n.ID, Runtime: "astro", Native: &n, ServerID: "host", Repository: n.Repository, Domain: n.Domain, Branch: n.Branch, AutoDeploy: true, GitHubInstallation: 42, Secret: "must-stay-private", Generation: "stable"}}}}
	r := httptest.NewRequest("PATCH", "/", strings.NewReader(`{"branch":"preview","domain":"example.com","env_changes":{}}`))
	r.SetPathValue("id", n.ID)
	w := httptest.NewRecorder()
	c.nativeContainerGuard("PATCH /api/control/apps/{id}/settings", c.updateSettings)(w, r)
	if w.Code != 200 || writes != 1 || strings.Contains(w.Body.String(), "must-stay-private") {
		t.Fatal("native settings failed", w.Code, w.Body.String())
	}
	saved := c.data.Apps[n.ID]
	if saved.Branch != "preview" || saved.Native.Branch != "main" || !saved.AutoDeploy || saved.GitHubInstallation != 42 || saved.Secret != "must-stay-private" {
		t.Fatal("edit changed deployment ownership or registration")
	}
}
