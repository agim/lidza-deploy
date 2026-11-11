package agent

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// retiredPanelCaddyConfig is the old panel host: the panel hostname has its
// own route to the stopped panel port, and shares another with a second
// hostname.
func retiredPanelCaddyConfig() map[string]any {
	config := operatorCaddyConfig()
	server := config["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["https"].(map[string]any)
	server["routes"] = append(server["routes"].([]any),
		map[string]any{"match": []any{map[string]any{"host": []any{"deploy.example.com"}}}, "terminal": true,
			"handle": []any{map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": "127.0.0.1:3000"}}}}},
		map[string]any{"match": []any{map[string]any{"host": []any{"www.example.com", "deploy.example.com"}}}, "terminal": true,
			"handle": []any{map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": "127.0.0.1:3000"}}}}},
	)
	return config
}

func stubControlForward(t *testing.T, now *time.Time) {
	t.Helper()
	oldNow, oldOwn := controlForwardNow, controlOwnAddress
	controlForwardNow = func() time.Time { return *now }
	controlOwnAddress = func(ip netip.Addr) bool { return ip.String() == "198.51.100.7" }
	t.Cleanup(func() { controlForwardNow, controlOwnAddress = oldNow, oldOwn })
}

func routesFor(admin *fakeCaddyAdmin, host string) []map[string]any {
	out := []map[string]any{}
	list, _ := admin.at("/config/apps/http/servers/https/routes").([]any)
	for _, raw := range list {
		route := raw.(map[string]any)
		encoded, _ := json.Marshal(route)
		var parsed caddyRoute
		json.Unmarshal(encoded, &parsed)
		for _, h := range caddyRouteHosts(parsed) {
			if h == host {
				out = append(out, route)
			}
		}
	}
	return out
}

func TestControlForwardReplacesTheHostnameRoute(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	stubControlForward(t, &now)
	admin := newFakeCaddyAdmin(t, retiredPanelCaddyConfig())
	m := testManager(t, &fakeRuntime{})
	m.data.HostIngress = &HostIngressState{Caddy: &NativeCaddyConfig{Admin: admin.URL(), Server: "https"}}
	ctx := context.Background()

	if _, err := m.installControlForward(ctx, "deploy.example.com", "198.51.100.7"); err == nil || !strings.Contains(err.Error(), "own address") {
		t.Fatalf("a forward to this host: %v", err)
	}
	if _, err := m.installControlForward(ctx, "deploy.example.com", "127.0.0.1"); err == nil {
		t.Fatal("a forward to loopback was accepted")
	}
	f, err := m.installControlForward(ctx, "deploy.example.com", "203.0.113.20")
	if err != nil {
		t.Fatal(err)
	}
	if f.Route != "caddy" || !f.Expires.Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("forward %+v", f)
	}
	routes := routesFor(admin, "deploy.example.com")
	if len(routes) != 1 {
		t.Fatalf("routes for the hostname: %v", routes)
	}
	// It took the old route's place (after the operator's own route).
	if second, _ := admin.at("/config/apps/http/servers/https/routes/1").(map[string]any); second["@id"] != controlForwardRouteID {
		t.Fatalf("route 1 %v", second)
	}
	raw, _ := json.Marshal(routes[0])
	if !jsonEqual(raw, renderControlForwardRoute("deploy.example.com", "203.0.113.20")) {
		t.Fatalf("route %s", raw)
	}
	for _, want := range []string{`"dial":"203.0.113.20:443"`, `"server_name":"deploy.example.com"`, `"Host":["{http.request.host}"]`, `"handler":"reverse_proxy"`, `"protocol":"http"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("route lacks %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "127.0.0.1:3000") {
		t.Fatal("the forward still dials the stopped panel")
	}
	if www := routesFor(admin, "www.example.com"); len(www) != 1 || strings.Contains(mustJSONString(www[0]), "deploy.example.com") {
		t.Fatalf("the shared route %v", www)
	}
	if ops := routesFor(admin, "ops.example.com"); len(ops) != 1 {
		t.Fatalf("the operator's route %v", ops)
	}
	if got := m.controlForward(); got == nil || got.Address != "203.0.113.20" {
		t.Fatalf("recorded %+v", got)
	}
	if note := controlForwardNote(f); !strings.Contains(note, "203.0.113.20") || !strings.Contains(note, "2026-11-13") {
		t.Fatalf("note %q", note)
	}

	// The loop leaves a forward in place alone.
	admin.reset()
	m.controlForwardTick(ctx)
	if admin.called("PATCH") != 0 || admin.called("PUT") != 0 || admin.called("DELETE") != 0 {
		t.Fatal("an unchanged forward was rewritten")
	}
	// A Caddy reload brings the old route back: the loop replaces it again.
	admin.mu.Lock()
	server := admin.config["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["https"].(map[string]any)
	server["routes"] = retiredPanelCaddyConfig()["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["https"].(map[string]any)["routes"]
	admin.mu.Unlock()
	m.controlForwardTick(ctx)
	if routes := routesFor(admin, "deploy.example.com"); len(routes) != 1 || routes[0]["@id"] != controlForwardRouteID {
		t.Fatalf("after a reload: %v", routes)
	}
}

func TestControlForwardExpires(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	stubControlForward(t, &now)
	admin := newFakeCaddyAdmin(t, retiredPanelCaddyConfig())
	m := testManager(t, &fakeRuntime{})
	m.data.HostIngress = &HostIngressState{Caddy: &NativeCaddyConfig{Admin: admin.URL(), Server: "https"}}
	ctx := context.Background()
	if _, err := m.installControlForward(ctx, "deploy.example.com", "203.0.113.20"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(7*24*time.Hour - time.Minute)
	m.controlForwardTick(ctx)
	if m.controlForward() == nil {
		t.Fatal("removed before it expired")
	}
	now = now.Add(2 * time.Minute)
	m.controlForwardTick(ctx)
	if m.controlForward() != nil {
		t.Fatal("the expired forward stayed recorded")
	}
	if found, _ := findCaddyID(admin.snapshot(), controlForwardRouteID, nil); found != nil {
		t.Fatal("the expired forward route stayed")
	}
	if ops := routesFor(admin, "ops.example.com"); len(ops) != 1 {
		t.Fatalf("the operator's route %v", ops)
	}
}

func TestControlForwardCaddyfileBody(t *testing.T) {
	original := "deploy.example.com {\n\treverse_proxy 127.0.0.1:3000\n}\n"
	next := controlCaddyfileSite(original, "deploy.example.com", controlForwardBody("deploy.example.com", "2001:db8::20"))
	for _, want := range []string{"reverse_proxy https://[2001:db8::20]:443 {", "header_up Host {host}", "tls_server_name deploy.example.com"} {
		if !strings.Contains(next, want) {
			t.Fatalf("lacks %q:\n%s", want, next)
		}
	}
	if strings.Contains(next, "127.0.0.1:3000") {
		t.Fatalf("old upstream stayed:\n%s", next)
	}
}

func mustJSONString(v any) string { return string(mustJSON(v)) }
