package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestRedirectDomainsControlValidationConflictsAndRecords(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	stored := []agent.RedirectDomain{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPut && r.URL.Path == "/v1/apps/portal/redirects" {
			calls++
			var in agent.RedirectRequest
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			stored = in.Redirects
		}
		agent.JSON(w, 200, agent.RedirectView{Redirects: stored, Hostnames: []string{"portal.example.com"}, Ingress: "caddy", IPv4: moveTestTargetIP, IPv6: moveTestTargetIP6, Note: "Redirects: published to Caddy."})
	}))
	defer remote.Close()
	c := &Control{cfg: Config{DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32))}, client: remote.Client(), registry: []Server{{ID: "local", URL: remote.URL, Token: strings.Repeat("a", 32)}}, data: saved{Apps: map[string]Application{
		"portal": {ID: "portal", ServerID: "local", Repository: "acme/portal", Branch: "main", Domain: "portal.example.com"},
		"other":  {ID: "other", ServerID: "remote", Domain: "other.example.com", AdditionalDomains: []string{"alias.example.com"}, RedirectDomains: []agent.RedirectDomain{{Host: "taken.example.com", Status: 301}}},
	}}}
	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
		r.SetPathValue("id", "portal")
		w := httptest.NewRecorder()
		c.updateRedirects(w, r)
		return w
	}
	for body, code := range map[string]int{
		`{"redirects":[{"host":"bad host"}]}`:                     400,
		`{"redirects":[{"host":"portal.example.com"}]}`:           400,
		`{"redirects":[{"host":"old.example.com","status":303}]}`: 400,
		`{"redirects":[{"host":"alias.example.com"}]}`:            409,
		`{"redirects":[{"host":"taken.example.com"}]}`:            409,
	} {
		if w := put(body); w.Code != code || calls != 0 {
			t.Fatalf("%s: %d %s (agent calls %d)", body, w.Code, w.Body.String(), calls)
		}
	}
	w := put(`{"redirects":[{"host":"Old.example.com"},{"host":"legacy.example.com","target":"elsewhere.example.org","status":302,"keep_path":false}]}`)
	if w.Code != 200 || calls != 1 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	var view redirectsView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	want := []agent.RedirectDomain{{Host: "old.example.com", Status: 301, KeepPath: true}, {Host: "legacy.example.com", Target: "elsewhere.example.org", Status: 302}}
	if !slices.Equal(stored, want) || !slices.Equal(c.data.Apps["portal"].RedirectDomains, want) {
		t.Fatalf("forwarded %+v, stored %+v", stored, c.data.Apps["portal"].RedirectDomains)
	}
	if len(view.Records) != 4 || view.Records[0] != (MoveDNSRecord{Hostname: "old.example.com", Type: "A", Value: moveTestTargetIP, Note: "redirect (301 to portal.example.com)"}) || view.Records[3].Type != "AAAA" {
		t.Fatalf("records %+v", view.Records)
	}
	if len(view.Warnings) != 1 || !strings.Contains(view.Warnings[0], "elsewhere.example.org") {
		t.Fatalf("external target not warned: %v", view.Warnings)
	}
	// Application hostnames cannot take over another application's redirect.
	c.data.Apps["other"] = Application{ID: "other", ServerID: "local", Repository: "acme/other", Branch: "main", Domain: "other.example.com"}
	r := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"branch":"main","domain":"other.example.com","additional_domains":["old.example.com"]}`))
	r.SetPathValue("id", "other")
	rec := httptest.NewRecorder()
	c.updateSettings(rec, r)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "redirect domain of portal") {
		t.Fatalf("hostname took over a redirect: %d %s", rec.Code, rec.Body.String())
	}
	// Clearing removes them.
	if w := put(`{"redirects":[]}`); w.Code != 200 || c.data.Apps["portal"].RedirectDomains != nil {
		t.Fatalf("clear: %d %+v", w.Code, c.data.Apps["portal"].RedirectDomains)
	}
	if p := routePermission("PUT /api/control/apps/{id}/redirects"); p != "deploy.write" {
		t.Fatalf("permission %s", p)
	}
}

func TestMoveCarriesRedirectDomains(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	redirects := []agent.RedirectDomain{{Host: "old.example.com", Status: 301, KeepPath: true}, {Host: "legacy.example.com", Status: 308}}
	app := fx.c.data.Apps["shop"]
	app.RedirectDomains = redirects
	fx.c.data.Apps["shop"] = app
	fx.a.source.RedirectDomains = redirects
	fx.resolver.set("old.example.com", moveTestSourceIP)
	fx.resolver.set("legacy.example.com", moveTestSourceIP)
	var mu sync.Mutex
	var carried []agent.RedirectDomain
	fx.b.mux.HandleFunc("PUT /v1/apps/{id}/redirects", func(w http.ResponseWriter, r *http.Request) {
		var in agent.RedirectRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		carried = in.Redirects
		mu.Unlock()
		agent.JSON(w, 200, agent.RedirectView{Redirects: in.Redirects, Note: "Redirects: published to Caddy."})
	})

	req := httptest.NewRequest("GET", "/", nil)
	plan, _, err := fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.RedirectDomains, redirects) || !strings.Contains(plan.RedirectNote, "2 redirect domains (old.example.com, legacy.example.com) are recreated") {
		t.Fatalf("plan: %+v %q", plan.RedirectDomains, plan.RedirectNote)
	}
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatalf("start: %d %+v", status, plan.Blockers)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	mu.Lock()
	got := slices.Clone(carried)
	mu.Unlock()
	if !slices.Equal(got, redirects) || !slices.Equal(job.RedirectHostnames, []string{"old.example.com", "legacy.example.com"}) {
		t.Fatalf("carried %+v, hostnames %v", got, job.RedirectHostnames)
	}
	records := moveRecords(job)
	redirectRecords := slices.DeleteFunc(slices.Clone(records), func(r MoveDNSRecord) bool { return !strings.HasPrefix(r.Note, "redirect") })
	if len(records) != 8 || len(redirectRecords) != 4 || redirectRecords[0] != (MoveDNSRecord{Hostname: "old.example.com", Type: "A", Value: moveTestTargetIP, Note: "redirect (301 to shop.example.com)"}) {
		t.Fatalf("records %+v", records)
	}
	// The cutover waits for the redirect domains' DNS too.
	fx.resolver.set("shop.example.com", moveTestTargetIP)
	fx.resolver.set("www.shop.example.com", moveTestTargetIP)
	fx.probe.set(true)
	fx.c.checkMoveDNS(context.Background(), view.ID, false)
	job, _ = fx.c.moveJob(view.ID)
	if job.State != moveStateCutover || len(job.DNS) != 4 || !job.DNS[2].Redirect || job.DNS[2].Ready {
		t.Fatalf("cutover before redirect DNS moved: %s %+v", job.State, job.DNS)
	}
	fx.resolver.set("old.example.com", moveTestTargetIP)
	fx.resolver.set("legacy.example.com", moveTestTargetIP)
	fx.c.checkMoveDNS(context.Background(), view.ID, false)
	job, _ = fx.c.moveJob(view.ID)
	if job.State != moveStateReady {
		t.Fatalf("redirect DNS moved but not ready: %s %+v", job.State, job.DNS)
	}
	if code := moveAction(t, fx.c, view.ID, "finish", nil); code != 202 {
		t.Fatalf("finish: %d", code)
	}
	waitMove(t, fx.c, view.ID, moveStateFinished)
	if live := fx.c.data.Apps["shop"]; live.ServerID != "beta" || !slices.Equal(live.RedirectDomains, redirects) {
		t.Fatalf("live record lost the redirects: %+v", live)
	}
}
