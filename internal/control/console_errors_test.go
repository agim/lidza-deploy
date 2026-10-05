package control

import (
	"context"
	"encoding/json"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/analytics"
	"github.com/agim/lidza/packs/db"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func exerciseConsoleIngest(t *testing.T, c *Control, ctx context.Context, call func(string, string, string, []*http.Cookie, string) *httptest.ResponseRecorder, cookies []*http.Cookie) {
	t.Helper()
	a, _ := c.app("portal")
	event := agent.ConsoleError{StoredError: analytics.StoredError{ID: strings.Repeat("a", 64), Source: "server", Message: "captured console failure", Fingerprint: "console-group", CreatedAt: time.Now().UTC()}, AppID: a.ID, Generation: a.Generation, Container: "console-container", Release: "release-one"}
	send := func(key string, events []agent.ConsoleError) *httptest.ResponseRecorder {
		data, _ := json.Marshal(agent.ConsoleBatch{Errors: events})
		r := httptest.NewRequest("POST", "/api/agent/errors", strings.NewReader(string(data))).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("X-Lidza-Server", "one")
		w := httptest.NewRecorder()
		c.ingestErrors(w, r)
		return w
	}
	if w := send("wrong", []agent.ConsoleError{event}); w.Code != 401 {
		t.Fatal("unauthenticated collector accepted")
	}
	for i := 0; i < 2; i++ {
		if w := send(strings.Repeat("a", 32), []agent.ConsoleError{event}); w.Code != 200 {
			t.Fatal("console ingestion failed", w.Code, w.Body)
		}
	}
	w := call("GET", "/api/control/apps/portal/errors", "", cookies, "")
	if w.Code != 200 || strings.Count(w.Body.String(), "captured console failure") != 1 {
		t.Fatal("durable console read or dedup failed", w.Code, w.Body)
	}
	event.Generation = "stale-generation"
	event.ID = strings.Repeat("b", 64)
	if w := send(strings.Repeat("a", 32), []agent.ConsoleError{event}); w.Code != 200 {
		t.Fatal("stale record not acknowledged")
	}
	var count int
	if err := db.From(ctx).QueryRow(ctx, `SELECT count(*) FROM app_error WHERE extra->>'deploy_app'='portal' AND extra->>'deploy_generation'=$1`, a.Generation).Scan(&count); err != nil || count != 1 {
		t.Fatal("stale incarnation attributed to current app", err, count)
	}
	event.Generation = a.Generation
	event.AppID = "another-server-app"
	send(strings.Repeat("a", 32), []agent.ConsoleError{event})
	if w := call("GET", "/api/control/apps/portal/errors", "", cookies, ""); strings.Contains(w.Body.String(), "another-server-app") {
		t.Fatal("foreign app attribution accepted")
	}
}
