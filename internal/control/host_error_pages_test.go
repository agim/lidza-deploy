package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestErrorPageAdoptionIsProxiedToTheAgent(t *testing.T) {
	pattern := "POST /api/control/servers/{server}/host/ingress/error-pages"
	if routePermission(pattern) != "infrastructure.manage" {
		t.Fatal("adopting error pages is not restricted to infrastructure managers")
	}
	fx := newMoveFixture(t)
	var patterns []string
	fx.c.routes(func(p string, _ http.HandlerFunc) { patterns = append(patterns, p) })
	if !slices.Contains(patterns, pattern) {
		t.Fatal("the error-page route is not registered")
	}
	adopted := 0
	fx.b.mux.HandleFunc("POST /v1/host/ingress/error-pages", func(w http.ResponseWriter, r *http.Request) {
		adopted++
		agent.JSON(w, 200, agent.HostIngressStatus{Mode: agent.HostIngressCaddy, ErrorPages: &agent.HostErrorPages{Dir: "/etc/lidza/pages", Ready: true, Managed: 2}})
	})
	req := httptest.NewRequest("POST", "/api/control/servers/beta/host/ingress/error-pages", nil)
	req.SetPathValue("server", "beta")
	w := httptest.NewRecorder()
	fx.c.adoptErrorPages(w, req)
	var out agent.HostIngressStatus
	if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != 200 || err != nil || adopted != 1 || out.ErrorPages == nil || out.ErrorPages.Managed != 2 {
		t.Fatalf("adopt: %d %s", w.Code, w.Body)
	}
}
