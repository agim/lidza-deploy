package control

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// An application without a move answers 204: the panel asks on every card
// expand, and browsers log every 4xx response as a console error.
func TestAppMoveRouteAnswersNoContentWithoutAMove(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	req := httptest.NewRequest("GET", "/api/control/apps/shop/move", nil)
	req.SetPathValue("id", "shop")
	w := httptest.NewRecorder()
	fx.c.appMoveRoute(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d, want %d: %s", w.Code, http.StatusNoContent, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Fatalf("unexpected body %q", w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control %q, want no-store", got)
	}
}
