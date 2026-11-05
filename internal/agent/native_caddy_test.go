package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestNativeCaddyPublishInsertsThenReplacesAndWithdraws(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	exists := false
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/id/lidza-app-shop" && !exists:
			http.NotFound(w, r)
		case r.Method == http.MethodPut && r.URL.Path == "/config/apps/http/servers/srv0/routes/0":
			if !strings.Contains(string(body), `"@id":"lidza-app-shop"`) {
				http.Error(w, "missing id", 400)
				return
			}
			exists = true
		case r.Method == http.MethodDelete:
			exists = false
		}
	}))
	defer admin.Close()
	m := &Manager{cfg: Config{ProxyListen: "127.0.0.1:18081", Native: &NativeHostConfig{Caddy: &NativeCaddyConfig{Admin: admin.URL}}}}
	ctx := context.Background()
	if err := m.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", []string{"www.shop.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := m.withdrawNativeCaddyRoute(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	if err := m.withdrawNativeCaddyRoute(ctx, "shop"); err != nil {
		t.Fatal("withdrawing a missing route failed:", err)
	}
	want := "PATCH /id/lidza-app-shop,PUT /config/apps/http/servers/srv0/routes/0,PATCH /id/lidza-app-shop,DELETE /id/lidza-app-shop,DELETE /id/lidza-app-shop"
	if got := strings.Join(calls, ","); got != want {
		t.Fatalf("calls\n got %s\nwant %s", got, want)
	}
	unconfigured := &Manager{cfg: Config{Native: &NativeHostConfig{}}}
	if err := unconfigured.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", nil); err != nil {
		t.Fatal("hosts without a fronting Caddy must not fail:", err)
	}
}
