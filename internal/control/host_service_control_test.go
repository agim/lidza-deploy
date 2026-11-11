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

func TestServiceControlSettingRoundTripsThroughTheAgent(t *testing.T) {
	if routePermission("PUT /api/control/servers/{server}/host/service-control") != "infrastructure.manage" {
		t.Fatal("the service control setting is not restricted to infrastructure managers")
	}
	fx := newMoveFixture(t)
	var mu sync.Mutex
	stored := agent.ServiceControlStatus{Supported: true, Source: "config", Account: "deploy"}
	reached := 0
	fx.b.mux.HandleFunc("GET /v1/host/service-control", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		agent.JSON(w, 200, stored)
	})
	fx.b.mux.HandleFunc("PUT /v1/host/service-control", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		reached++
		var in agent.ServiceControlSettings
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Enabled == nil {
			agent.Fail(w, 400, err)
			return
		}
		stored.Enabled, stored.Source = *in.Enabled, "setting"
		agent.JSON(w, 200, stored)
	})
	call := func(method, body string) (int, agent.ServiceControlStatus) {
		t.Helper()
		req := httptest.NewRequest(method, "/api/control/servers/beta/host/service-control", strings.NewReader(body))
		req.SetPathValue("server", "beta")
		w := httptest.NewRecorder()
		fx.c.hostAdmin(w, req)
		var out agent.ServiceControlStatus
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, out := call("GET", ""); code != 200 || !out.Supported || out.Enabled || out.Account != "deploy" {
		t.Fatalf("read: %d %+v", code, out)
	}
	for _, bad := range []string{`{}`, `{"enabled":"yes"}`, `{"other":true}`} {
		if code, _ := call("PUT", bad); code != 400 {
			t.Fatalf("%s: %d", bad, code)
		}
	}
	if reached != 0 {
		t.Fatal("an invalid setting reached the agent")
	}
	if code, out := call("PUT", `{"enabled":true}`); code != 200 || !out.Enabled || out.Source != "setting" {
		t.Fatalf("save: %d %+v", code, out)
	}
}
