package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/agim/lidza"
	"github.com/agim/lidza/packs/audit"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/packs/db"
	"github.com/agim/lidza/packs/jobs"
	"github.com/agim/lidza/pkg/router"
)

func TestTeamAndAuditSurviveRestart(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("PostgreSQL integration")
	}
	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("JOBS_WORKERS", "0")
	t.Setenv("AUTH_SECRET", strings.Repeat("s", 64))
	t.Setenv("AUTH_COOKIE_SECURE", "false")
	t.Setenv("LIDZA_MASTER_KEY", strings.Repeat("ab", 32))
	t.Setenv("AUTH_CONNECT", "")
	t.Setenv("APP_URL", "http://127.0.0.1:3000")
	cfg := Config{PublicURL: "http://127.0.0.1:3000", User: "owner-" + random() + "@example.com", Password: "a-long-fixture-password-123", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir()}
	start := func() (*Control, *lidza.Booted, context.Context) {
		t.Helper()
		c, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		h := c.Handler(http.NotFoundHandler())
		b, err := lidza.Boot(context.Background(), lidza.App{Name: "team-persistence", Packs: []lidza.Pack{db.Pack(), auth.Pack(), audit.Pack(), jobs.Pack()}, OnStart: c.Start, Frontend: h, Routes: func(r *router.Router) { r.Handle("/api/control/", h) }})
		if err != nil {
			t.Fatal(err)
		}
		return c, b, lidza.WithServices(context.Background(), b.Services)
	}
	c, first, ctx := start()
	p, err := auth.From(ctx).CreateUser(ctx, "member-"+random()+"@example.com", "Member", "a-long-fixture-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := fleetRoles.Grant(ctx, p.Subject, fleetScope, "viewer"); err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.From(ctx).Login(ctx, p.Subject, nil)
	if err != nil {
		t.Fatal(err)
	}
	cookies := auth.From(ctx).Cookies(tokens)
	actorCtx := auth.WithUser(ctx, &auth.User{ID: c.operatorID})
	if err := audit.From(actorCtx).Record(actorCtx, audit.Event{Action: "team.persistence", Resource: "member/" + p.Subject, Scope: fleetScope}); err != nil {
		t.Fatal(err)
	}
	if err := c.auditedJob("deploy.task", func(context.Context, json.RawMessage) error { return nil })(ctx, []byte(`{"app_id":"fixture"}`)); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.Password = "" // An upgrade must not require a new bootstrap password.
	_, second, restarted := start()
	defer second.Close(context.Background())
	allowed, err := fleetRoles.Can(restarted, p.Subject, fleetScope, "fleet.read")
	if err != nil || !allowed {
		t.Fatal("membership lost across restart", err)
	}
	page, err := audit.From(restarted).List(restarted, audit.Query{Resource: "member/" + p.Subject, Action: "team.persistence"})
	if err != nil || len(page.Records) != 1 || page.Records[0].Actor != c.operatorID {
		t.Fatal("audit lost across restart", err)
	}
	system, err := audit.From(restarted).List(restarted, audit.Query{Actor: "system:deploy.task", Resource: "app/fixture", Action: "deploy.task", Limit: 1})
	if err != nil || len(system.Records) != 1 || system.Records[0].Outcome != audit.OK {
		t.Fatal("background dispatch not audited as system", err)
	}
	request := func() int {
		r := httptest.NewRequest("GET", "/api/control/status", nil)
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		second.Handler.ServeHTTP(w, r)
		return w.Code
	}
	if code := request(); code != 200 {
		t.Fatal("existing session lost after restart", code)
	}
	r := httptest.NewRequest("POST", "/api/control/apps/fixture/owner-claim", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	second.Handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("viewer allowed to reveal setup tokens", w.Code)
	}
	if routePermission("POST /api/control/apps/{id}/owner-claim") != "deploy.secrets" {
		t.Fatal("setup token permission is too broad")
	}
	if err := fleetRoles.RevokeAll(restarted, p.Subject, fleetScope); err != nil {
		t.Fatal(err)
	}
	if code := request(); code != 403 {
		t.Fatal("restarted session survived revocation", code)
	}
}
