package control

import (
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNativeAppsCannotEnterContainerMutationRoutes(t *testing.T) {
	c := &Control{data: saved{Apps: map[string]Application{"rails": {Native: &agent.NativeApplication{Name: "Rails"}}, "go": {}}}}
	for _, pattern := range []string{"POST /api/control/apps/{id}/deploy", "PATCH /api/control/apps/{id}/settings", "DELETE /api/control/apps/{id}", "PUT /api/control/apps/{id}/{feature}"} {
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
