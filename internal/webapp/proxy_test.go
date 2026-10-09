package webapp

import (
	"context"
	"github.com/agim/lidza"
	"github.com/agim/lidza/pkg/middleware"
	"github.com/agim/lidza/pkg/router"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestControlPanelProxyIdentityAndIndependentLimits(t *testing.T) {
	t.Setenv("LIDZA_TRUSTED_PROXIES", "loopback")
	limit := middleware.RateLimit(middleware.RateLimitOptions{RPS: 0.0001, Burst: 1})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(middleware.ClientIP(r))) }))
	b, err := lidza.Boot(context.Background(), lidza.App{Name: "proxy-compatibility", Routes: func(r *router.Router) { r.Handle("/api/identity", limit) }})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background())
	for _, tc := range []struct {
		peer, forwarded, want string
		status                int
	}{
		{"127.0.0.1:1234", "198.51.100.1", "198.51.100.1", 200},
		{"127.0.0.1:1234", "203.0.113.99, 198.51.100.1", "", 429},
		{"127.0.0.1:1234", "198.51.100.2", "198.51.100.2", 200},
		{"198.51.100.3:1234", "198.51.100.1", "198.51.100.3", 200},
	} {
		r := httptest.NewRequest("GET", "http://localhost/api/identity", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("X-Forwarded-For", tc.forwarded)
		w := httptest.NewRecorder()
		b.Handler.ServeHTTP(w, r)
		if w.Code != tc.status || (tc.want != "" && w.Body.String() != tc.want) {
			t.Fatal("proxy/rate-limit compatibility failure", w.Code, w.Body.String())
		}
	}
}
