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

// A push to a native application deploys through the agent only while the
// agent reports a reviewed release policy for it.
func TestNativePushDeploysOnlyReleaseManagedApplications(t *testing.T) {
	for _, managed := range []bool{false, true} {
		native := &agent.NativeApplication{ID: "rails-app", Name: "app", Repository: "acme/app", Branch: "main", Domain: "app.example.com", Runtime: "rails", Services: []string{"app-server.service"}}
		calls := []string{}
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, r.Method+" "+r.URL.Path)
			switch {
			case r.Method == "GET" && r.URL.Path == "/v1/native/apps/rails-app":
				status := agent.NativeStatus{Application: *native, DeploymentSupported: managed, ReleaseManaged: managed}
				json.NewEncoder(w).Encode(status)
			case r.Method == "POST" && r.URL.Path == "/v1/native/apps/rails-app/deploy":
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), `"key":"github:delivery-1"`) {
					t.Error("deploy request lacks the delivery key", string(body))
				}
				w.WriteHeader(202)
				io.WriteString(w, `{"id":"d1","app_id":"rails-app","status":"queued"}`)
			default:
				t.Error("unexpected agent call", r.Method, r.URL.Path)
				w.WriteHeader(500)
			}
		}))
		c := &Control{client: remote.Client(), cfg: Config{PublicURL: "http://127.0.0.1:3000"}, registry: []Server{{ID: "host", URL: remote.URL}},
			data: saved{Apps: map[string]Application{"rails-app": {ID: "rails-app", ServerID: "host", Repository: "acme/app", Branch: "main", AutoDeploy: true, Generation: "g1", Native: native}}},
			credentialsFixture: func(_ context.Context, _ Application, key string) (agent.DeployRequest, error) {
				return agent.DeployRequest{Key: key, Token: "short-lived"}, nil
			}}
		push, _ := json.Marshal(pushJob{AppID: "rails-app", Generation: "g1", Delivery: "delivery-1"})
		if err := c.dispatchPush(context.Background(), push); err != nil {
			t.Fatal(managed, err)
		}
		remote.Close()
		deployed := false
		for _, call := range calls {
			if call == "POST /v1/native/apps/rails-app/deploy" {
				deployed = true
			}
		}
		if deployed != managed {
			t.Fatalf("release_managed=%t but deploy called=%t: %v", managed, deployed, calls)
		}
	}
}

func TestNativeContainerGuardAllowsReleaseManagedDeployAndRollback(t *testing.T) {
	native := &agent.NativeApplication{ID: "rails-app", Name: "app", Repository: "acme/app", Branch: "main", Domain: "app.example.com", Runtime: "rails", Services: []string{"app-server.service"}}
	for _, managed := range []bool{false, true} {
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(agent.NativeStatus{Application: *native, DeploymentSupported: managed, ReleaseManaged: managed})
		}))
		c := &Control{client: remote.Client(), registry: []Server{{ID: "host", URL: remote.URL}},
			data: saved{Apps: map[string]Application{"rails-app": {ID: "rails-app", ServerID: "host", Native: native}}}}
		for _, pattern := range []string{"POST /api/control/apps/{id}/deploy", "POST /api/control/apps/{id}/rollback", "POST /api/control/apps/{id}/webhook"} {
			reached := false
			handler := c.nativeContainerGuard(pattern, func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(200) })
			r := httptest.NewRequest("POST", "/", nil)
			r.SetPathValue("id", "rails-app")
			w := httptest.NewRecorder()
			handler(w, r)
			want := managed && pattern != "POST /api/control/apps/{id}/webhook" // webhook also needs the GitHub App pairing
			if reached != want || (want && w.Code != 200) || (!want && w.Code != 409) {
				t.Fatalf("managed=%t %s reached=%t code=%d", managed, pattern, reached, w.Code)
			}
		}
		remote.Close()
	}
}
