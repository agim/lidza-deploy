package control

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

// The panel lists a database's tables and saves the tables whose data its
// backups skip through the agent; the agent's refusals reach the operator.
func TestDatabaseExcludedTablesProxies(t *testing.T) {
	var mu sync.Mutex
	calls := []string{}
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
		mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/databases/shop/tables":
			agent.JSON(w, 200, agent.DatabaseTables{Tables: []agent.DatabaseTable{{Name: "public.events", Rows: 52000, Bytes: 81920000, Excludable: true}}, Total: 1, Excluded: []string{}})
		case r.Method == "PATCH" && r.URL.Path == "/v1/databases/shop/backups/excluded-tables" && strings.Contains(string(body), "public.missing"):
			agent.Fail(w, 409, errors.New("no such table in shop: public.missing"))
		case r.Method == "PATCH" && r.URL.Path == "/v1/databases/shop/backups/excluded-tables":
			agent.JSON(w, 200, map[string]string{"status": "saved"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.Close)
	c, err := New(Config{User: "operator@example.com", Password: "fixture-password-16-chars", PublicURL: "http://localhost:3000", DataDir: t.TempDir(), Key: []byte(strings.Repeat("r", 32)),
		Servers: []Server{{ID: "one", Name: "Server one", URL: a.URL, Token: strings.Repeat("a", 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(handler http.HandlerFunc, method, id, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/", strings.NewReader(body))
		r.SetPathValue("server", "one")
		r.SetPathValue("id", id)
		handler(w, r)
		return w
	}
	w := call(c.databaseTables, "GET", "shop", "")
	var listing agent.DatabaseTables
	if json.Unmarshal(w.Body.Bytes(), &listing); w.Code != 200 || len(listing.Tables) != 1 || listing.Tables[0].Rows != 52000 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("listing: %d %s", w.Code, w.Body.String())
	}
	if w := call(c.databaseExcludedTables, "PATCH", "shop", `{"tables":["public.events"]}`); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if w := call(c.databaseExcludedTables, "PATCH", "shop", `{}`); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	if w := call(c.databaseExcludedTables, "PATCH", "shop", `{"tables":["public.missing"]}`); w.Code != 409 || !strings.Contains(w.Body.String(), "public.missing") {
		t.Fatalf("refusal: %d %s", w.Code, w.Body.String())
	}
	for _, bad := range []struct {
		handler          http.HandlerFunc
		method, id, body string
	}{
		{c.databaseTables, "GET", "../shop", ""},
		{c.databaseExcludedTables, "PATCH", "Shop", `{"tables":[]}`},
		{c.databaseExcludedTables, "PATCH", "shop", `{"tables":["public.events"],"extra":1}`},
	} {
		if w := call(bad.handler, bad.method, bad.id, bad.body); w.Code != 404 && w.Code != 400 {
			t.Fatalf("%s %s: %d", bad.id, bad.body, w.Code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"GET /v1/databases/shop/tables ", `PATCH /v1/databases/shop/backups/excluded-tables {"tables":["public.events"]}`, `PATCH /v1/databases/shop/backups/excluded-tables {"tables":[]}`, `PATCH /v1/databases/shop/backups/excluded-tables {"tables":["public.missing"]}`}
	if !slices.Equal(calls, want) {
		t.Fatalf("agent calls %q", calls)
	}
}

func TestDatabaseExcludedTablesRequireInfrastructureManage(t *testing.T) {
	for _, pattern := range []string{"GET /api/control/servers/{server}/databases/{id}/tables", "PATCH /api/control/servers/{server}/databases/{id}/backups/excluded-tables"} {
		if routePermission(pattern) != "infrastructure.manage" {
			t.Fatal(pattern, routePermission(pattern))
		}
	}
}

// The plan offers the choice for every excluded table before the move
// starts, copying by default, with estimated rows and sizes measured from
// the source's table listing when the inventory has none; an unknown row
// estimate is never shown on its own.
func TestMovePlanShowsExcludedTableChoicesWithEstimates(t *testing.T) {
	fx := newMoveFixture(t)
	tables := fx.excludedSource()
	fx.a.source.Databases[0].ExcludedTables = []agent.MoveExcludedTable{{Name: "public.events", Rows: -1, Bytes: -1}, {Name: "public.audit_log", Rows: -1, Bytes: 1 << 20}}
	plan, _, err := fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	got := plan.Databases[0].ExcludedTables
	if len(got) != 2 || got[0].Choice != moveTableCopy || got[1].Choice != moveTableCopy || got[0].Rows != 41_000 || got[0].Bytes != 28<<20 || got[1].Rows != 700 || got[1].Bytes != 1<<20 {
		t.Fatalf("plan tables: %+v", got)
	}
	if !slices.ContainsFunc(plan.Notes, func(n string) bool {
		return strings.Contains(n, "public.events (about 41k rows, 28.0 MiB: data copied)") && strings.Contains(n, "public.audit_log (about 700 rows, 1.0 MiB: data copied)")
	}) {
		t.Fatalf("notes %q", plan.Notes)
	}
	fx.a.with(func() {
		if tables.listed != 1 {
			t.Fatalf("listed %d times", tables.listed)
		}
	})
	for _, n := range plan.Notes {
		if strings.Contains(n, "rows unknown") {
			t.Fatal(n)
		}
	}
	// A choice made in the plan is part of the start request.
	plan, _, _ = fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta", ExcludedTables: map[string]map[string]string{"shop": {"public.audit_log": moveTableLeave}}})
	if got := plan.Databases[0].ExcludedTables; got[0].Choice != moveTableCopy || got[1].Choice != moveTableLeave {
		t.Fatalf("choices: %+v", got)
	}
}

func TestMoveTableEstimateNeverShowsUnknownRowsAlone(t *testing.T) {
	for _, tc := range []struct {
		table MoveExcludedTable
		want  string
	}{
		{MoveExcludedTable{Rows: 52_000, Bytes: 80 << 20}, "about 52k rows, 80.0 MiB"},
		{MoveExcludedTable{Rows: -1, Bytes: 8192}, "8.0 KiB"},
		{MoveExcludedTable{Rows: 12, Bytes: -1}, "about 12 rows"},
		{MoveExcludedTable{Rows: -1, Bytes: -1}, "not measured"},
	} {
		if got := moveTableEstimate(tc.table); got != tc.want {
			t.Fatalf("%+v: %q, want %q", tc.table, got, tc.want)
		}
	}
}
