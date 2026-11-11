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

func TestAccessLogSettingRoundTripsThroughTheHostingAgent(t *testing.T) {
	if routePermission("PUT /api/control/apps/{id}/access-log") != "deploy.write" || routePermission("GET /api/control/apps/{id}/access-log") != "fleet.read" {
		t.Fatal("the access log setting is an application setting")
	}
	var mu sync.Mutex
	stored := agent.AppAccessLogStatus{Supported: true, Enabled: true, Active: true, Path: "/var/log/lidza/apps/portal/access.log", Env: "LIDZA_ACCESS_LOG"}
	reached := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/v1/apps/portal/access-log" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			reached++
			var in agent.AppAccessLogSettings
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Enabled == nil {
				agent.Fail(w, 400, err)
				return
			}
			stored.Enabled, stored.Active = *in.Enabled, *in.Enabled
		}
		agent.JSON(w, 200, stored)
	}))
	defer remote.Close()
	c := &Control{cfg: Config{DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32))}, client: remote.Client(), registry: []Server{{ID: "local", URL: remote.URL, Token: strings.Repeat("a", 32)}}, data: saved{Apps: map[string]Application{
		"portal": {ID: "portal", ServerID: "local", Repository: "acme/portal", Branch: "main", Domain: "portal.example.com"},
	}}}
	call := func(method, body string) (int, agent.AppAccessLogStatus) {
		t.Helper()
		r := httptest.NewRequest(method, "/", strings.NewReader(body))
		r.SetPathValue("id", "portal")
		w := httptest.NewRecorder()
		if method == http.MethodGet {
			c.accessLog(w, r)
		} else {
			c.updateAccessLog(w, r)
		}
		var out agent.AppAccessLogStatus
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, out := call(http.MethodGet, ""); code != 200 || !out.Enabled || out.Path != "/var/log/lidza/apps/portal/access.log" {
		t.Fatalf("read: %d %+v", code, out)
	}
	for _, bad := range []string{`{}`, `{"enabled":"no"}`, `{"other":true}`} {
		if code, _ := call(http.MethodPut, bad); code != 400 {
			t.Fatalf("%s: %d", bad, code)
		}
	}
	if reached != 0 {
		t.Fatal("an invalid setting reached the agent")
	}
	if code, out := call(http.MethodPut, `{"enabled":false}`); code != 200 || out.Enabled || out.Active || reached != 1 {
		t.Fatalf("save: %d %+v", code, out)
	}
}
