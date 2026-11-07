package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestRedirectDomainDefaultsKeepPathAndStatus(t *testing.T) {
	var in RedirectRequest
	if err := json.Unmarshal([]byte(`{"redirects":[{"host":"Old.Example.com"},{"host":"legacy.example.com","keep_path":false,"status":308,"target":"www.example.com"}]}`), &in); err != nil {
		t.Fatal(err)
	}
	got, err := NormalizeRedirectDomains([]string{"new.example.com", "www.example.com"}, in.Redirects)
	if err != nil {
		t.Fatal(err)
	}
	want := []RedirectDomain{{Host: "old.example.com", Status: 301, KeepPath: true}, {Host: "legacy.example.com", Target: "www.example.com", Status: 308}}
	if !slices.Equal(got, want) {
		t.Fatalf("normalized %+v", got)
	}
	if err := json.Unmarshal([]byte(`{"host":"a.example.com","unknown":1}`), &RedirectDomain{}); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestRedirectDomainValidation(t *testing.T) {
	own := []string{"new.example.com", "www.new.example.com"}
	many := make([]RedirectDomain, 51)
	for i := range many {
		many[i] = RedirectDomain{Host: "r" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + ".example.com"}
	}
	for name, list := range map[string][]RedirectDomain{
		"invalid host":     {{Host: "not a host"}},
		"ip":               {{Host: "10.0.0.1"}},
		"duplicate":        {{Host: "old.example.com"}, {Host: "OLD.example.com"}},
		"own hostname":     {{Host: "www.new.example.com"}},
		"self target":      {{Host: "old.example.com", Target: "old.example.com"}},
		"bad target":       {{Host: "old.example.com", Target: "https://x"}},
		"bad status":       {{Host: "old.example.com", Status: 303}},
		"chained redirect": {{Host: "a.example.com", Target: "b.example.com"}, {Host: "b.example.com"}},
		"too many":         many,
	} {
		if _, err := NormalizeRedirectDomains(own, list); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// External targets are allowed.
	if _, err := NormalizeRedirectDomains(own, []RedirectDomain{{Host: "old.example.com", Target: "elsewhere.example.org", Status: 302}}); err != nil {
		t.Fatal(err)
	}
}

func TestRedirectCaddyRouteJSON(t *testing.T) {
	raw, err := renderRedirectRoute("web", "new.example.com", []RedirectDomain{
		{Host: "old.example.com", Status: 301, KeepPath: true},
		{Host: "legacy.example.com", Status: 301, KeepPath: true},
		{Host: "promo.example.com", Target: "www.new.example.com", Status: 302},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"@id":"lidza-redirect-web","handle":[{"handler":"subroute","routes":[` +
		`{"handle":[{"handler":"static_response","headers":{"Location":["https://new.example.com{http.request.uri}"]},"status_code":301}],"match":[{"host":["old.example.com","legacy.example.com"]}]},` +
		`{"handle":[{"handler":"static_response","headers":{"Location":["https://www.new.example.com/"]},"status_code":302}],"match":[{"host":["promo.example.com"]}]}]}],` +
		`"match":[{"host":["old.example.com","legacy.example.com","promo.example.com"]}],"terminal":true}`
	if string(raw) != want {
		t.Fatalf("route:\n%s\nwant:\n%s", raw, want)
	}
	if _, err := renderRedirectRoute("web", "new.example.com", nil); err == nil {
		t.Fatal("empty route rendered")
	}
}

func TestRedirectCaddyPublishTLSSubjectsAndRemoval(t *testing.T) {
	admin := newFakeCaddyAdmin(t, operatorCaddyConfig())
	m := caddyManager(admin, "https")
	m.data.Apps = map[string]App{"web": {ID: "web", Domain: "new.example.com"}}
	m.data.Redirects = map[string][]RedirectDomain{"web": {{Host: "old.example.com", Status: 301, KeepPath: true}, {Host: "legacy.example.com", Status: 308}}}
	ctx := context.Background()

	note, err := m.syncRedirectIngress(ctx, "web")
	if err != nil || !strings.HasPrefix(note, "Redirects: published to Caddy.") {
		t.Fatalf("sync: %q %v", note, err)
	}
	route, _ := admin.at("/config/apps/http/servers/https/routes/0").(map[string]any)
	if route["@id"] != "lidza-redirect-web" {
		t.Fatalf("redirect route not first: %v", admin.at("/config/apps/http/servers/https/routes"))
	}
	if got := admin.strings(subjectsPath); !slices.Equal(got, []string{"ops.example.com", "old.example.com", "legacy.example.com"}) {
		t.Fatalf("subjects: %v", got)
	}
	// Unchanged redirects write nothing.
	admin.reset()
	if _, err := m.syncRedirectIngress(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	for _, write := range []string{"PATCH ", "PUT ", "POST ", "DELETE "} {
		if admin.called(write) != 0 {
			t.Fatalf("resync wrote to Caddy: %v", admin.calls)
		}
	}
	// Dropping a host releases its subject only.
	m.data.Redirects["web"] = []RedirectDomain{{Host: "old.example.com", Status: 301, KeepPath: true}}
	if _, err := m.syncRedirectIngress(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if got := admin.strings(subjectsPath); !slices.Equal(got, []string{"ops.example.com", "old.example.com"}) {
		t.Fatalf("subjects after removing legacy: %v", got)
	}
	// A removed application's route and subjects go with the reconcile.
	delete(m.data.Redirects, "web")
	delete(m.data.Apps, "web")
	m.reconcileRedirects(ctx)
	if got := admin.strings(subjectsPath); !slices.Equal(got, []string{"ops.example.com"}) {
		t.Fatalf("subjects after removal: %v", got)
	}
	routes, _ := admin.at("/config/apps/http/servers/https/routes").([]any)
	if len(routes) != 1 {
		t.Fatalf("routes after removal: %v", routes)
	}
}

func TestRedirectProxyAnswersWhileTheAppIsStopped(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if err := m.Upsert(testApp("web")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetRedirects(context.Background(), "web", []RedirectDomain{
		{Host: "old.example.com", KeepPath: true},
		{Host: "legacy.example.com", Target: "www.example.org", Status: 308},
	}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a := m.data.Apps["web"]
	a.Stopped, a.Maintenance.Enabled = true, true
	m.data.Apps["web"] = a
	m.mu.Unlock()
	proxy := Proxy(m)
	for _, c := range []struct {
		host, uri, location string
		status              int
	}{
		{"old.example.com", "/blog/post?id=7", "https://web.example.com/blog/post?id=7", 301},
		{"OLD.example.com:443", "/", "https://web.example.com/", 301},
		{"legacy.example.com", "/deep/path?q=1", "https://www.example.org/", 308},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+c.host+c.uri, nil)
		req.Host = c.host
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)
		if w.Code != c.status || w.Header().Get("Location") != c.location {
			t.Fatalf("%s%s: %d %q", c.host, c.uri, w.Code, w.Header().Get("Location"))
		}
	}
	// The application's own hostname is not redirected.
	req := httptest.NewRequest(http.MethodGet, "http://web.example.com/", nil)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)
	if w.Header().Get("Location") != "" {
		t.Fatalf("own hostname redirected: %d %v", w.Code, w.Header())
	}
	// Certificates: the on-demand allow endpoint approves redirect hosts.
	if !m.domainAllowed(context.Background(), "old.example.com") || m.domainAllowed(context.Background(), "unknown.example.com") {
		t.Fatal("tls allow does not follow the redirect domains")
	}
	apps := m.Apps()
	if len(apps) != 1 || len(apps[0].RedirectDomains) != 2 {
		t.Fatalf("view: %+v", apps)
	}
}

func TestRedirectDomainsDoNotOverlapAnyHostname(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.cfg.ReservedDomains = []string{"*.reserved.example.com"}
	for _, id := range []string{"web", "blog"} {
		if err := m.Upsert(testApp(id)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if _, err := m.SetRedirects(ctx, "web", []RedirectDomain{{Host: "blog.example.com"}}); err == nil || !strings.Contains(err.Error(), "hostname of blog") {
		t.Fatalf("another app's hostname accepted: %v", err)
	}
	if _, err := m.SetRedirects(ctx, "web", []RedirectDomain{{Host: "x.reserved.example.com"}}); err == nil {
		t.Fatal("reserved hostname accepted")
	}
	if _, err := m.SetRedirects(ctx, "missing", []RedirectDomain{{Host: "old.example.com"}}); err == nil {
		t.Fatal("unknown application accepted")
	}
	if _, err := m.SetRedirects(ctx, "web", []RedirectDomain{{Host: "old.example.com"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetRedirects(ctx, "blog", []RedirectDomain{{Host: "old.example.com"}}); err == nil || !strings.Contains(err.Error(), "redirect domain of web") {
		t.Fatalf("another app's redirect accepted: %v", err)
	}
	// Hostnames cannot take over a redirect domain either.
	blog := testApp("blog")
	blog.AdditionalDomains = []string{"old.example.com"}
	if err := m.Upsert(blog); err == nil {
		t.Fatal("hostname overlapping a redirect domain accepted")
	}
	if err := m.PatchSettings("blog", SettingsPatch{Branch: "main", Domain: "old.example.com"}); err == nil {
		t.Fatal("settings took over a redirect domain")
	}
	// Replacing the own list may keep its hosts.
	if _, err := m.SetRedirects(ctx, "web", []RedirectDomain{{Host: "old.example.com", Status: 302}}); err != nil {
		t.Fatal(err)
	}
	// Removing the application drops its redirects.
	if err := m.RetireWith(ctx, "web", AppRemoveRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := m.Redirects("web"); len(got) != 0 || m.redirectHost("old.example.com") {
		t.Fatalf("redirects survived removal: %v", got)
	}
}
