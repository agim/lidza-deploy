package control

import (
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeAppsCannotEnterContainerMutationRoutes(t *testing.T) {
	c := &Control{data: saved{Apps: map[string]Application{"rails": {Native: &agent.NativeApplication{Name: "Rails"}}, "go": {}}}}
	for _, pattern := range []string{"POST /api/control/apps/{id}/deploy", "PATCH /api/control/apps/{id}/settings", "POST /api/control/apps/{id}/reload", "PUT /api/control/apps/{id}/{feature}"} {
		called := false
		handler := c.nativeContainerGuard(pattern, func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) })
		r := httptest.NewRequest("POST", "/", nil)
		r.SetPathValue("id", "rails")
		w := httptest.NewRecorder()
		handler(w, r)
		if called || w.Code != 409 {
			t.Fatal("native container mutation not blocked", pattern)
		}
		r.SetPathValue("id", "go")
		w = httptest.NewRecorder()
		handler(w, r)
		if !called || w.Code != 200 {
			t.Fatal("Go route behavior changed", pattern)
		}
	}
	if routePermission("POST /api/control/servers/{server}/native/apps") != "infrastructure.manage" || routePermission("POST /api/control/native-apps/{id}/services/{unit}/{action}") != "infrastructure.manage" {
		t.Fatal("native mutations must remain administrator infrastructure actions")
	}
}

// Diagnostics are runtime-agnostic: a native application's errors, security
// activity, logs, processes and settings are read like a container
// application's, and the refusal of a container-only action names the
// runtime instead of asking for a change plan.
func TestNativeAppsReadDiagnosticsWithoutContainerGuard(t *testing.T) {
	c := &Control{data: saved{Apps: map[string]Application{"shop": {ID: "shop", ServerID: "host", Runtime: "rails", Native: &agent.NativeApplication{ID: "shop", Name: "shop", Runtime: "rails", Services: []string{"lidza-app-shop-web.service"}}}}}}
	for _, pattern := range []string{"GET /api/control/apps/{id}/errors", "GET /api/control/apps/{id}/security", "GET /api/control/apps/{id}/analytics-errors", "GET /api/control/apps/{id}/logs", "GET /api/control/apps/{id}/processes", "GET /api/control/apps/{id}/settings"} {
		called := false
		handler := c.nativeContainerGuard(pattern, func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) })
		r := httptest.NewRequest("GET", "/", nil)
		r.SetPathValue("id", "shop")
		w := httptest.NewRecorder()
		handler(w, r)
		if !called || w.Code != 200 {
			t.Fatal("native diagnostics blocked", pattern, w.Code, w.Body.String())
		}
	}
	handler := c.nativeContainerGuard("POST /api/control/apps/{id}/reload", func(w http.ResponseWriter, r *http.Request) { t.Fatal("container reload reached a native application") })
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("id", "shop")
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "does not apply to a native application") || strings.Contains(w.Body.String(), "change plan") {
		t.Fatal("container-only refusal not explained", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/api/control/apps/shop/analytics-errors", nil)
	r.SetPathValue("id", "shop")
	w = httptest.NewRecorder()
	c.analyticsErrors(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "console collector") {
		t.Fatal("optional analytics for a native application not explained", w.Code, w.Body.String())
	}
}
