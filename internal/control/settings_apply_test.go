package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

// A settings save answers what it applied: the agent's restart as it is, a
// deployment the panel starts for build-affecting changes.
func TestSettingsSaveStartsTheDeploymentABuildChangeNeeds(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		agent   any // the agent's PATCH answer
		applied string
		deploys int
	}{
		{"restart", map[string]any{"status": "saved", "apply": agent.SettingsApplyResult{Applied: agent.SettingsAppliedRestart, DeploymentID: "reload-1", Deployment: &agent.Deployment{ID: "reload-1", Kind: "reload"}, Message: "Settings saved. Restarting the application with them."}}, agent.SettingsAppliedRestart, 0},
		{"deploy", map[string]any{"status": "saved", "apply": agent.SettingsApplyResult{Applied: agent.SettingsAppliedDeploy, Message: "Settings saved. A deployment builds and releases them."}}, agent.SettingsAppliedDeploy, 1},
		{"immediate", map[string]any{"status": "saved", "apply": agent.SettingsApplyResult{Applied: agent.SettingsAppliedImmediate, Message: "Settings saved."}}, agent.SettingsAppliedImmediate, 0},
		{"older agent", map[string]string{"status": "saved"}, agent.SettingsAppliedRestartRequired, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			deploys := 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "PATCH" && r.URL.Path == "/v1/apps/portal/settings":
					json.NewEncoder(w).Encode(scenario.agent)
				case r.Method == "POST" && r.URL.Path == "/v1/apps/portal/deploy":
					deploys++
					var req agent.DeployRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token != "checkout-token" {
						t.Error("deployment lost its checkout credentials", err)
					}
					w.WriteHeader(202)
					json.NewEncoder(w).Encode(agent.Deployment{ID: "deploy-1", AppID: "portal", Status: "queued", Kind: "deploy"})
				default:
					t.Error("unexpected agent call", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 409)
				}
			}))
			defer remote.Close()
			c := &Control{cfg: Config{DataDir: t.TempDir(), Key: make([]byte, 32)}, client: remote.Client(), registry: []Server{{ID: "host", URL: remote.URL, Token: "fixture"}},
				data: saved{Apps: map[string]Application{"portal": {ID: "portal", ServerID: "host", Repository: "acme/portal", Branch: "main", Domain: "portal.example.com", Secret: "must-stay-private"}}}}
			c.credentialsFixture = func(context.Context, Application, string) (agent.DeployRequest, error) {
				return agent.DeployRequest{Token: "checkout-token"}, nil
			}
			r := httptest.NewRequest("PATCH", "/", strings.NewReader(`{"branch":"next","domain":"portal.example.com","env_changes":{}}`))
			r.SetPathValue("id", "portal")
			w := httptest.NewRecorder()
			c.updateSettings(w, r)
			var out SettingsSaved
			if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != 200 || err != nil {
				t.Fatal(w.Code, w.Body.String(), err)
			}
			if out.Apply.Applied != scenario.applied || deploys != scenario.deploys || strings.Contains(w.Body.String(), "must-stay-private") || out.Branch != "next" {
				t.Fatalf("apply %+v, %d deploys: %s", out.Apply, deploys, w.Body.String())
			}
			if scenario.deploys == 1 && (out.Apply.DeploymentID != "deploy-1" || out.Apply.Deployment == nil || out.Apply.Error != "") {
				t.Fatalf("deployment not returned %+v", out.Apply)
			}
			if scenario.name == "restart" && out.Apply.DeploymentID != "reload-1" {
				t.Fatalf("restart not passed through %+v", out.Apply)
			}
		})
	}
}
