package control

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestMigrationPlanUsesLegacyInventoryOnlyWhenRouteIsMissing(t *testing.T) {
	const token = "fixture-agent-token-01234567890123456"
	const planRoute = "/v1/apps/agim-dev/migration-plan"
	for _, scenario := range []struct {
		name          string
		status        int
		response      string
		inventory     string
		wantStatus    int
		wantInventory bool
	}{
		{"modern", 200, `{"app_id":"agim-dev","commit":"modern"}`, "", 200, false},
		{"legacy", 404, "404 page not found", `[{"id":"agim-dev","domain":"agim.dev","additional_domains":["www.agim.dev"],"current":{"commit":"legacy"},"env":{"SECRET":"never-return-this"}}]`, 200, true},
		{"removed", 404, "404 page not found", `[{"id":"another-app"}]`, 502, true},
		{"modern missing app", 404, `{"error":"application unavailable"}`, "", 502, false},
		{"authorization failure", 401, "unauthorized", "", 502, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			requests := make(chan string, 4)
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.URL.Path
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("inventory must use an authenticated, read-only request")
				}
				switch r.URL.Path {
				case planRoute:
					w.WriteHeader(scenario.status)
					io.WriteString(w, scenario.response)
				case "/v1/apps":
					io.WriteString(w, scenario.inventory)
				default:
					t.Error("unexpected agent endpoint", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer remote.Close()
			control, err := New(Config{PublicURL: "https://deploy.example.com", User: "operator@example.com",
				Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(),
				Servers: []Server{{ID: "source", URL: remote.URL, Token: token}}})
			if err != nil {
				t.Fatal(err)
			}
			control.data.Apps["agim-dev"] = Application{ID: "agim-dev", ServerID: "source", Domain: "stale.example.com"}
			req := httptest.NewRequest(http.MethodGet, "/api/control/apps/agim-dev/migration-plan", nil)
			req.SetPathValue("id", "agim-dev")
			w := httptest.NewRecorder()
			control.migrationPlan(w, req)
			if w.Code != scenario.wantStatus {
				t.Fatalf("status %d, body %s", w.Code, w.Body.String())
			}
			want := []string{planRoute}
			if scenario.wantInventory {
				want = append(want, "/v1/apps")
			}
			var paths []string
			for len(requests) > 0 {
				paths = append(paths, <-requests)
			}
			if !slices.Equal(paths, want) {
				t.Fatalf("unexpected agent requests %v, want %v", paths, want)
			}
			if scenario.name == "legacy" {
				var plan agent.MigrationPlan
				if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
					t.Fatal(err)
				}
				if !plan.InventoryLimited || plan.ExecutionSupported || plan.Commit != "legacy" ||
					!slices.Equal(plan.Domains, []string{"agim.dev", "www.agim.dev"}) || len(plan.Blockers) == 0 ||
					strings.Contains(w.Body.String(), "never-return-this") || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("legacy plan must report limited, verified inventory only", w.Body.String())
				}
			}
		})
	}
}
