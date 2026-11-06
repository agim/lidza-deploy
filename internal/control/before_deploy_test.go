package control

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

// beforeDeployAgent answers the before-deploy route for two databases: a
// secondary one that accepts any valid policy and a primary one that
// refuses never, as the real agent does.
type beforeDeployAgent struct {
	*httptest.Server
	mu    sync.Mutex
	calls []string
}

func newBeforeDeployAgent(t *testing.T) *beforeDeployAgent {
	a := &beforeDeployAgent{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.calls = append(a.calls, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
		a.mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "PATCH /v1/databases/reports/backups/before-deploy":
			agent.JSON(w, 200, map[string]string{"status": "saved"})
		case "PATCH /v1/databases/shop/backups/before-deploy":
			agent.Fail(w, 409, errors.New("a primary database is backed up before every deploy; choose always or reuse"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *beforeDeployAgent) received() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func beforeDeployControl(t *testing.T, a *beforeDeployAgent) *Control {
	t.Helper()
	c, err := New(Config{User: "operator@example.com", Password: "fixture-password-16-chars", PublicURL: "http://localhost:3000", DataDir: t.TempDir(), Key: []byte(strings.Repeat("r", 32)),
		Servers: []Server{{ID: "one", Name: "Server one", URL: a.URL, Token: strings.Repeat("a", 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDatabaseBeforeDeployValidatesThenProxies(t *testing.T) {
	a := newBeforeDeployAgent(t)
	c := beforeDeployControl(t, a)
	call := func(server, id, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body))
		r.SetPathValue("server", server)
		r.SetPathValue("id", id)
		c.databaseBeforeDeploy(w, r)
		return w
	}
	for _, tc := range []struct {
		id, body string
		code     int
	}{
		{"reports", `{"mode":"reuse","max_age_hours":48}`, 200},
		{"reports", `{"mode":"never"}`, 200},
		{"reports", `{"mode":"always"}`, 200},
		{"reports", `{"mode":"sometimes"}`, 400},
		{"reports", `{"mode":"reuse"}`, 400},
		{"reports", `{"mode":"reuse","max_age_hours":9000}`, 400},
		{"reports", `not json`, 400},
		{"Bad ID", `{"mode":"never"}`, 404},
		{"shop", `{"mode":"never"}`, 409}, // the agent's refusal for a primary database is passed through
	} {
		if w := call("one", tc.id, tc.body); w.Code != tc.code {
			t.Fatalf("%s %s: %d %s", tc.id, tc.body, w.Code, w.Body.String())
		}
	}
	got := a.received()
	want := []string{
		`PATCH /v1/databases/reports/backups/before-deploy {"mode":"reuse","max_age_hours":48}`,
		`PATCH /v1/databases/reports/backups/before-deploy {"mode":"never"}`,
		`PATCH /v1/databases/reports/backups/before-deploy {"mode":"always"}`,
		`PATCH /v1/databases/shop/backups/before-deploy {"mode":"never"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("agent calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if w := call("missing", "reports", `{"mode":"never"}`); w.Code < 400 {
		t.Fatal("unknown server accepted", w.Code)
	}
	// A mutation of a server's database needs the administrator role, like
	// the other backup settings.
	if p := routePermission("PATCH /api/control/servers/{server}/databases/{id}/backups/before-deploy"); p != "infrastructure.manage" {
		t.Fatal(p)
	}
}

// A move sets the source's policy on the destination database; the default
// (always) needs no call because new databases start there.
func TestMoveCarriesTheBeforeDeployPolicyToTheDestination(t *testing.T) {
	a := newBeforeDeployAgent(t)
	c := beforeDeployControl(t, a)
	job := MoveJob{ID: "m1", AppID: "shop", TargetServer: "one"}
	c.moveCarryBeforeDeploy(context.Background(), job, MoveDatabase{SourceID: "shop-old"}, "reports")
	c.moveCarryBeforeDeploy(context.Background(), job, MoveDatabase{SourceID: "shop-old", BeforeDeploy: agent.BeforeDeployPolicy{Mode: "always"}}, "reports")
	c.moveCarryBeforeDeploy(context.Background(), job, MoveDatabase{SourceID: "shop-old", BeforeDeploy: agent.BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 24}}, "")
	if calls := a.received(); len(calls) != 0 {
		t.Fatal("default policy or missing target produced calls", calls)
	}
	c.moveCarryBeforeDeploy(context.Background(), job, MoveDatabase{SourceID: "shop-old", BeforeDeploy: agent.BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 24}}, "reports")
	// A refusal on the destination (never on a primary) is not fatal.
	c.moveCarryBeforeDeploy(context.Background(), job, MoveDatabase{SourceID: "shop-old", BeforeDeploy: agent.BeforeDeployPolicy{Mode: "never"}}, "shop")
	got := a.received()
	want := []string{
		`PATCH /v1/databases/reports/backups/before-deploy {"mode":"reuse","max_age_hours":24}`,
		`PATCH /v1/databases/shop/backups/before-deploy {"mode":"never"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("agent calls:\n%s", strings.Join(got, "\n"))
	}
	// The plan row names the policy that will be carried.
	if row := (MovePlanDatabase{BeforeDeploy: agent.BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 24}.String()}); row.BeforeDeploy != "reuse within 24h" {
		t.Fatal(row.BeforeDeploy)
	}
}
