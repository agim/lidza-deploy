package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/web"
)

func TestAlertLinksOpenTheMatchingConsoleView(t *testing.T) {
	for key, want := range map[string]string{
		"server:one":                 "/console/servers/one",
		"disk:one:/var/lib":          "/console/servers/one/cleanup",
		"memory:one":                 "/console/servers/one/containers",
		"load:one":                   "/console/servers/one/containers",
		"storage:one":                "/console/remote-backups",
		"database:one:portal":        "/console/databases/portal",
		"backup-age:one:portal":      "/console/databases/portal",
		"health:one:portal":          "/console/applications/portal/logs",
		"deploy:one:portal":          "/console/deployments",
		"security-burst:one:portal":  "/console/security/portal",
		"security-review:one:portal": "/console/security/portal",
		"task:one:portal:nightly":    "/console/applications/portal/settings/processes",
		"server:a b/c":               "/console/servers/a%20b%2Fc",
		"unknown":                    "/console/applications",
		"health:one":                 "/console/applications",
	} {
		if got := alertPath(key); got != want {
			t.Errorf("alertPath(%q) = %q, want %q", key, got, want)
		}
		if !web.IsConsolePath(want) {
			t.Errorf("%s is not served as a console view", want)
		}
	}
}

func TestControlServesConsoleViewsAndKeepsAPI(t *testing.T) {
	c, err := New(Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	var frontend []string
	static := web.Handler()
	h := c.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frontend = append(frontend, r.URL.Path)
		static.ServeHTTP(w, r)
	}))
	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest("GET", "/console.html", nil))
	if page.Code != 200 {
		t.Fatal("console page", page.Code)
	}
	for _, path := range []string{"/console/applications", "/console/applications/portal/settings/environment", "/console/servers/one/console"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Body.String() != page.Body.String() || w.Header().Get("Cache-Control") != page.Header().Get("Cache-Control") {
			t.Fatalf("%s: status=%d, want the console page", path, w.Code)
		}
	}
	// API paths stay with the API handlers (here the auth guard, which needs
	// the framework's services this unit test does not boot).
	before := len(frontend)
	for _, path := range []string{"/api/control/status", "/api/control/apps/portal/settings"} {
		func() {
			defer func() { _ = recover() }()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
		}()
	}
	if len(frontend) != before {
		t.Fatal("API path reached the console frontend", frontend[before:])
	}
}
