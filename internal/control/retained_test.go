package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestRetainedResourceRoutesNeedInfrastructureManagement(t *testing.T) {
	fx := newMoveFixture(t)
	var patterns []string
	fx.c.routes(func(pattern string, _ http.HandlerFunc) { patterns = append(patterns, pattern) })
	for _, pattern := range []string{"GET /api/control/servers/{server}/retained-resources", "DELETE /api/control/servers/{server}/retained-resources/{id}"} {
		if !slices.Contains(patterns, pattern) {
			t.Fatalf("%s is not registered", pattern)
		}
		if routePermission(pattern) != "infrastructure.manage" {
			t.Fatalf("%s is not restricted to infrastructure managers", pattern)
		}
	}
}

func TestRetainedResourcesShowMovesAndProxyRemoval(t *testing.T) {
	fx := newMoveFixture(t)
	rows := []agent.RetainedResourceView{
		{ID: "r-shop", AppID: "shop", Kind: "application", Created: time.Now().UTC(), Components: []agent.RetainedComponent{{Kind: "database", Name: "lidza-db-shop-data", SizeBytes: 2048, Measured: true}}, SizeBytes: 2048},
		{ID: "database-old", AppID: "old", Kind: "database", Components: []agent.RetainedComponent{}, FinalBackupRequired: true},
	}
	fx.a.with(func() { fx.a.retained = slices.Clone(rows) })
	// shop moved from alpha to beta and now runs there.
	fx.c.mu.Lock()
	fx.c.data.Apps["shop"] = Application{ID: "shop", ServerID: "beta", Repository: "acme/shop", Domain: "shop.example.com"}
	if fx.c.data.Moves == nil {
		fx.c.data.Moves = map[string]MoveJob{}
	}
	fx.c.data.Moves["m1"] = MoveJob{ID: "m1", AppID: "shop", SourceServer: "alpha", TargetServer: "beta", State: moveStateFinished, Updated: time.Now()}
	fx.c.mu.Unlock()

	call := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.SetPathValue("server", "alpha")
		if _, id, ok := strings.Cut(strings.TrimPrefix(target, "/api/control/servers/alpha/retained-resources"), "/"); ok {
			req.SetPathValue("id", id)
		}
		w := httptest.NewRecorder()
		if method == http.MethodGet {
			fx.c.serverRetained(w, req)
		} else {
			fx.c.removeRetained(w, req)
		}
		return w
	}
	w := call("GET", "/api/control/servers/alpha/retained-resources", "")
	var list []fleetRetainedResource
	if err := json.Unmarshal(w.Body.Bytes(), &list); w.Code != 200 || err != nil || len(list) != 2 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if list[1].ID != "r-shop" || list[1].Reason != retainedReasonMoved || list[1].RunsOn != "beta" || list[1].RunsOnName != "Beta" || list[1].ServerID != "alpha" {
		t.Fatalf("moved resource: %+v", list[1])
	}
	if list[0].Reason != retainedReasonRemoved || list[0].RunsOn != "" {
		t.Fatalf("removed resource: %+v", list[0])
	}

	if w := call("DELETE", "/api/control/servers/alpha/retained-resources/r-shop", `{"confirm":"r-sho"}`); w.Code != 400 {
		t.Fatalf("unconfirmed: %d %s", w.Code, w.Body)
	}
	if w := call("DELETE", "/api/control/servers/alpha/retained-resources/nope", `{"confirm":"nope"}`); w.Code != 404 {
		t.Fatalf("unknown: %d %s", w.Code, w.Body)
	}
	if w := call("DELETE", "/api/control/servers/alpha/retained-resources/database-old", `{"confirm":"database-old"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "final backup") {
		t.Fatalf("agent refusal: %d %s", w.Code, w.Body)
	}
	if w := call("DELETE", "/api/control/servers/alpha/retained-resources/r-shop", `{"confirm":"r-shop","final_backup":true}`); w.Code != 202 {
		t.Fatalf("removal: %d %s", w.Code, w.Body)
	}
	fx.a.with(func() {
		if removed := fx.a.retainedRemovals; len(removed) != 1 || removed[0].Confirm != "r-shop" || !removed[0].FinalBackup {
			t.Fatalf("agent requests: %+v", removed)
		}
		fx.a.retained = slices.Clone(rows)
	})

	// A running move of the same application blocks removal on its servers.
	fx.c.mu.Lock()
	fx.c.data.Moves["m2"] = MoveJob{ID: "m2", AppID: "shop", SourceServer: "beta", TargetServer: "alpha", State: "running", Updated: time.Now()}
	fx.c.mu.Unlock()
	if w := call("DELETE", "/api/control/servers/alpha/retained-resources/r-shop", `{"confirm":"r-shop"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "move") {
		t.Fatalf("removal during a move: %d %s", w.Code, w.Body)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

func TestRetainedResourcesNameServersAndDateMoves(t *testing.T) {
	fx := newMoveFixture(t)
	finished := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	fx.c.mu.Lock()
	fx.c.data.Apps["analytics"] = Application{ID: "analytics", ServerID: "beta", Repository: "acme/analytics", Domain: "analytics.example.com"}
	fx.c.data.Moves = map[string]MoveJob{"m1": {ID: "m1", AppID: "analytics", SourceServer: "alpha", TargetServer: "beta", State: moveStateFinished, Updated: finished}}
	fx.c.mu.Unlock()
	row := fx.c.retainedFor("alpha", agent.RetainedResourceView{ID: "r", AppID: "analytics", Origin: &agent.RetainedOrigin{MoveID: "m1", Switched: true}})
	if row.ServerName != "Alpha" || row.Reason != retainedReasonMoved || row.RunsOnName != "Beta" || !row.MovedAt.Equal(finished) {
		t.Fatalf("row = %+v", row)
	}
	// A move the panel no longer remembers is still a move, by its origin.
	fx.c.mu.Lock()
	fx.c.data.Moves = nil
	fx.c.mu.Unlock()
	row = fx.c.retainedFor("alpha", agent.RetainedResourceView{ID: "r", AppID: "analytics", Origin: &agent.RetainedOrigin{MoveID: "m0", Switched: true}})
	if row.Reason != retainedReasonMoved || row.RunsOn != "beta" {
		t.Fatalf("forgotten move = %+v", row)
	}
}
