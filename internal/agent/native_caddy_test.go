package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestNativeCaddyConfigRequiresLoopback(t *testing.T) {
	for _, ok := range []NativeCaddyConfig{{Admin: "http://127.0.0.1:2019"}, {Admin: "http://localhost:2019", Server: "srv1", Upstream: "127.0.0.1:18081"}} {
		if err := ok.validate(); err != nil {
			t.Fatalf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []NativeCaddyConfig{{Admin: "http://10.0.0.5:2019"}, {Admin: "https://127.0.0.1:2019"}, {Admin: "http://127.0.0.1:2019/config"}, {Admin: "http://127.0.0.1:2019", Server: "srv 0"}, {Admin: "http://127.0.0.1:2019", Upstream: "203.0.113.1:80"}} {
		if err := bad.validate(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestNativeCaddyRouteProxiesHostnamesToTheAgent(t *testing.T) {
	raw, err := renderNativeCaddyRoute("shop", []string{"shop.example.com", "www.shop.example.com"}, "127.0.0.1:18081")
	if err != nil {
		t.Fatal(err)
	}
	var route struct {
		ID       string `json:"@id"`
		Terminal bool   `json:"terminal"`
		Match    []struct {
			Host []string `json:"host"`
		} `json:"match"`
	}
	if err := json.Unmarshal(raw, &route); err != nil || route.ID != "lidza-app-shop" || !route.Terminal || len(route.Match) != 1 || strings.Join(route.Match[0].Host, ",") != "shop.example.com,www.shop.example.com" {
		t.Fatalf("%s %v", raw, err)
	}
	if !strings.Contains(string(raw), `"dial":"127.0.0.1:18081"`) || !strings.Contains(string(raw), `"handler":"reverse_proxy"`) {
		t.Fatalf("route does not proxy to the agent: %s", raw)
	}
	if _, err := renderNativeCaddyRoute("Bad Id", []string{"a.example.com"}, "127.0.0.1:1"); err == nil {
		t.Fatal("invalid id accepted")
	}
}

// caddyManager is a manager whose configuration names the fake Caddy.
func caddyManager(admin *fakeCaddyAdmin, server string) *Manager {
	return &Manager{cfg: Config{ProxyListen: "127.0.0.1:18081", Native: &NativeHostConfig{Caddy: &NativeCaddyConfig{Admin: admin.URL(), Server: server}}}}
}

const subjectsPath = "/config/apps/tls/automation/policies/0/subjects"

func TestNativeCaddyPublishAddsRouteAndTLSSubjectsOnce(t *testing.T) {
	admin := newFakeCaddyAdmin(t, operatorCaddyConfig())
	m := caddyManager(admin, "https")
	ctx := context.Background()

	note, err := m.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", nil)
	if err != nil || note != "TLS: hostnames added to Caddy automation; certificate issuance follows DNS." {
		t.Fatalf("publish: %q %v", note, err)
	}
	if got := admin.strings(subjectsPath); !slices.Equal(got, []string{"ops.example.com", "shop.example.com"}) {
		t.Fatalf("subjects after publish: %v", got)
	}
	if route, _ := admin.at("/config/apps/http/servers/https/routes/0").(map[string]any); route["@id"] != "lidza-app-shop" {
		t.Fatalf("route not inserted first: %v", admin.at("/config/apps/http/servers/https/routes"))
	}
	if admin.called("GET "+subjectsPath) == 0 {
		t.Fatal("the subjects were not verified after the update")
	}

	// Publishing again changes nothing and writes nothing.
	admin.reset()
	if note, err := m.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", nil); err != nil || note != "TLS: hostnames are in Caddy automation; certificate issuance follows DNS." {
		t.Fatalf("republish: %q %v", note, err)
	}
	for _, write := range []string{"PATCH ", "PUT ", "POST ", "DELETE "} {
		if admin.called(write) != 0 {
			t.Fatalf("republish wrote to Caddy: %v", admin.calls)
		}
	}

	// New hostnames extend the route and the subjects; a wildcard subject
	// already covering a name is respected.
	if _, err := m.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", []string{"www.shop.example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := admin.strings(subjectsPath); !slices.Equal(got, []string{"ops.example.com", "shop.example.com", "www.shop.example.com"}) {
		t.Fatalf("subjects after www: %v", got)
	}
	if _, err := m.publishNativeCaddyRoute(ctx, "blog", "blog.example.com", []string{"www.shop.example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := admin.strings(subjectsPath); !slices.Equal(got, []string{"ops.example.com", "shop.example.com", "www.shop.example.com", "blog.example.com"}) {
		t.Fatalf("subjects after blog: %v", got)
	}

	// Withdrawing removes the route and only the subjects no other
	// published application serves; the operator's subject stays.
	if err := m.withdrawNativeCaddyRoute(ctx, "shop", []string{"shop.example.com", "www.shop.example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := admin.strings(subjectsPath); !slices.Equal(got, []string{"ops.example.com", "www.shop.example.com", "blog.example.com"}) {
		t.Fatalf("subjects after withdrawing shop: %v", got)
	}
	if routes, _ := admin.at("/config/apps/http/servers/https/routes").([]any); len(routes) != 2 {
		t.Fatalf("routes after withdrawing shop: %v", routes)
	}
	if err := m.withdrawNativeCaddyRoute(ctx, "shop", []string{"shop.example.com"}); err != nil {
		t.Fatal("withdrawing a missing route failed:", err)
	}
	if err := m.withdrawNativeCaddyRoute(ctx, "blog", nil); err != nil {
		t.Fatal(err)
	}
	if got := admin.strings(subjectsPath); !slices.Equal(got, []string{"ops.example.com"}) {
		t.Fatalf("subjects after withdrawing blog: %v", got)
	}
	if routes, _ := admin.at("/config/apps/http/servers/https/routes").([]any); len(routes) != 1 {
		t.Fatalf("routes after withdrawing blog: %v", routes)
	}
}

func TestNativeCaddyPublishCreatesMissingTLSAutomation(t *testing.T) {
	config := operatorCaddyConfig()
	delete(config["apps"].(map[string]any), "tls")
	admin := newFakeCaddyAdmin(t, config)
	m := caddyManager(admin, "https")
	ctx := context.Background()
	note, err := m.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", []string{"www.shop.example.com"})
	if err != nil || !strings.HasPrefix(note, "TLS: hostnames added") {
		t.Fatalf("publish without automation: %q %v", note, err)
	}
	if got := admin.strings(subjectsPath); !slices.Equal(got, []string{"shop.example.com", "www.shop.example.com"}) {
		t.Fatalf("created policy subjects: %v", got)
	}
	if issuers, _ := admin.at("/config/apps/tls/automation/policies/0/issuers").([]any); len(issuers) != 1 || issuers[0].(map[string]any)["module"] != "acme" {
		t.Fatalf("created policy issuers: %v", issuers)
	}
	admin.reset()
	if _, err := m.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", []string{"www.shop.example.com"}); err != nil || admin.called("POST ") != 0 || admin.called("PATCH ") != 0 {
		t.Fatalf("republish after creation wrote again: %v %v", err, admin.calls)
	}

	// Automation whose policies are all restricted and non-ACME gets a new policy appended.
	config = operatorCaddyConfig()
	config["apps"].(map[string]any)["tls"] = map[string]any{"automation": map[string]any{"policies": []any{
		map[string]any{"subjects": []any{"internal.example.com"}, "issuers": []any{map[string]any{"module": "internal"}}},
	}}}
	admin = newFakeCaddyAdmin(t, config)
	m = caddyManager(admin, "https")
	if _, err := m.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", nil); err != nil {
		t.Fatal(err)
	}
	if got := admin.strings("/config/apps/tls/automation/policies/1/subjects"); !slices.Equal(got, []string{"shop.example.com"}) {
		t.Fatalf("appended policy: %v", admin.at("/config/apps/tls/automation/policies"))
	}
	if got := admin.strings("/config/apps/tls/automation/policies/0/subjects"); !slices.Equal(got, []string{"internal.example.com"}) {
		t.Fatalf("other policy touched: %v", got)
	}
}

func TestNativeCaddyPublishSkipsSubjectsForOnDemandAndCatchAllPolicies(t *testing.T) {
	ctx := context.Background()
	// On-demand TLS needs no subjects.
	onDemand := operatorCaddyConfig()
	onDemand["apps"].(map[string]any)["tls"] = map[string]any{"automation": map[string]any{
		"on_demand": map[string]any{"permission": map[string]any{"module": "http", "endpoint": "http://127.0.0.1:9090/tls/allow"}},
		"policies":  []any{map[string]any{"on_demand": true}},
	}}
	admin := newFakeCaddyAdmin(t, onDemand)
	m := caddyManager(admin, "https")
	note, err := m.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", nil)
	if err != nil || !strings.Contains(note, "on demand") {
		t.Fatalf("on-demand: %q %v", note, err)
	}
	if _, present := admin.at("/config/apps/tls/automation/policies/0").(map[string]any)["subjects"]; present {
		t.Fatal("subjects were added to an on-demand policy")
	}
	// A policy without subjects already covers every hostname and must not be restricted.
	catchAll := operatorCaddyConfig()
	catchAll["apps"].(map[string]any)["tls"] = map[string]any{"automation": map[string]any{"policies": []any{
		map[string]any{"issuers": []any{map[string]any{"module": "acme", "email": "ops@example.com"}}},
	}}}
	admin = newFakeCaddyAdmin(t, catchAll)
	m = caddyManager(admin, "https")
	note, err = m.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", nil)
	if err != nil || !strings.HasPrefix(note, "TLS: hostnames are in") {
		t.Fatalf("catch-all: %q %v", note, err)
	}
	if _, present := admin.at("/config/apps/tls/automation/policies/0").(map[string]any)["subjects"]; present {
		t.Fatal("a catch-all policy was restricted")
	}
	if err := m.withdrawNativeCaddyRoute(ctx, "shop", []string{"shop.example.com"}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCaddyFollowsTheDetectedIngress(t *testing.T) {
	ctx := context.Background()
	// Agent layout: nothing is published.
	stubHostIngress(t, []int{80, 443}, map[string]map[string]any{hostIngressDefaultAdmin: installerCaddyConfig("127.0.0.1:8081")})
	agentHost := &Manager{cfg: Config{ProxyListen: "127.0.0.1:8081", Native: &NativeHostConfig{Root: "/srv/apps", User: "app"}}}
	if note, err := agentHost.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", nil); err != nil || note != "" {
		t.Fatalf("agent layout published: %q %v", note, err)
	}
	if err := agentHost.withdrawNativeCaddyRoute(ctx, "shop", nil); err != nil {
		t.Fatal(err)
	}
	// Detected operator Caddy on the default admin address: published there,
	// into the server that listens on :443, without any configuration.
	admin := newFakeCaddyAdmin(t, operatorCaddyConfig())
	oldGet := hostIngressAdminGet
	hostIngressAdminGet = func(ctx context.Context, requested string) ([]byte, error) {
		if requested == hostIngressDefaultAdmin {
			return caddyAdminConfig(ctx, admin.URL())
		}
		return oldGet(ctx, requested)
	}
	detected := &Manager{cfg: Config{ProxyListen: "127.0.0.1:8081"}}
	cfg, ok := detected.hostIngressCaddy(ctx)
	if !ok || cfg.Admin != hostIngressDefaultAdmin || cfg.Server != "https" {
		t.Fatalf("detected ingress: %+v %v", cfg, ok)
	}
	// Publishing goes to the detected admin address; the stored override
	// redirects it to the fake for the write.
	detected.data.HostIngress = &HostIngressState{Caddy: &NativeCaddyConfig{Admin: admin.URL(), Server: "https"}}
	if note, err := detected.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", nil); err != nil || !strings.HasPrefix(note, "TLS: hostnames added") {
		t.Fatalf("publish to detected Caddy: %q %v", note, err)
	}
	if got := admin.strings(subjectsPath); !slices.Equal(got, []string{"ops.example.com", "shop.example.com"}) {
		t.Fatalf("subjects: %v", got)
	}
	if !strings.Contains(string(mustJSON(admin.at("/config/apps/http/servers/https/routes/0"))), `"dial":"127.0.0.1:8081"`) {
		t.Fatal("the route must reach the agent proxy")
	}
}
