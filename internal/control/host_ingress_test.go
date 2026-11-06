package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestHostIngressSettingRoundTripsThroughTheAgent(t *testing.T) {
	for _, pattern := range []string{"PUT /api/control/servers/{server}/host/ingress"} {
		if routePermission(pattern) != "infrastructure.manage" {
			t.Fatalf("%s is not restricted to infrastructure managers", pattern)
		}
	}
	fx := newMoveFixture(t)
	var mu sync.Mutex
	stored := agent.HostIngressSettings{}
	refreshed := false
	status := func() agent.HostIngressStatus {
		out := agent.HostIngressStatus{Mode: agent.HostIngressAgent, Source: "detected", Detected: agent.HostIngressAgent}
		if stored.Caddy != nil {
			override := *stored.Caddy
			out.Mode, out.Source, out.CaddyAdmin, out.CaddyServer, out.Override = agent.HostIngressCaddy, "setting", override.Admin, override.Server, &override
		}
		return out
	}
	fx.b.mux.HandleFunc("GET /v1/host/ingress", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		refreshed = refreshed || r.URL.Query().Get("refresh") == "1"
		agent.JSON(w, 200, status())
	})
	fx.b.mux.HandleFunc("PUT /v1/host/ingress", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		agent.JSON(w, 200, status())
	})
	call := func(method, target, body string) (int, agent.HostIngressStatus) {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.SetPathValue("server", "beta")
		w := httptest.NewRecorder()
		fx.c.hostAdmin(w, req)
		var out agent.HostIngressStatus
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, out := call("GET", "/api/control/servers/beta/host/ingress?refresh=1", ""); code != 200 || out.Mode != agent.HostIngressAgent || !refreshed {
		t.Fatalf("read: %d %+v refreshed=%v", code, out, refreshed)
	}
	// The panel validates before anything reaches the agent.
	for _, bad := range []string{`{"caddy":{"admin":"http://192.0.2.1:2019"}}`, `{"caddy":{"admin":"http://127.0.0.1:2019","server":"a b"}}`, `{"caddy":{"admin":"http://127.0.0.1:2019","upstream":"127.0.0.1:9"}}`, `{"caddy":{}}`, `{"other":1}`} {
		if code, _ := call("PUT", "/api/control/servers/beta/host/ingress", bad); code != 400 {
			t.Fatalf("%s: %d", bad, code)
		}
	}
	if stored.Caddy != nil {
		t.Fatal("an invalid setting reached the agent")
	}
	code, out := call("PUT", "/api/control/servers/beta/host/ingress", `{"caddy":{"admin":"http://127.0.0.1:2019","server":"srv0"}}`)
	if code != 200 || out.Mode != agent.HostIngressCaddy || out.Source != "setting" || out.Override == nil || out.Override.Admin != "http://127.0.0.1:2019" || out.CaddyServer != "srv0" {
		t.Fatalf("save: %d %+v", code, out)
	}
	if code, out = call("PUT", "/api/control/servers/beta/host/ingress", `{"caddy":null}`); code != 200 || out.Mode != agent.HostIngressAgent || out.Override != nil {
		t.Fatalf("clear: %d %+v", code, out)
	}
}
