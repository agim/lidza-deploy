package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestUIAssetsRevalidate(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/console.html", "/login.html", "/setup.html", "/app.js", "/style.css", "/help.js"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("ETag") == "" || w.Header().Get("X-Lidza-Deploy-Version") == "" {
			t.Fatalf("%s: status=%d headers=%v", path, w.Code, w.Header())
		}
		if w.Header().Get("Clear-Site-Data") != "" || w.Header().Get("Set-Cookie") != "" {
			t.Fatal("asset cache policy changes session storage")
		}
	}
}

func TestUIUpgradeInvalidatesWarmAssetValidators(t *testing.T) {
	for _, path := range []string{"/console.html", "/app.js", "/style.css"} {
		name := strings.TrimPrefix(path, "/")
		first := staticHandler(fstest.MapFS{name: {Data: []byte("first release")}}, "v0.2.41")
		second := staticHandler(fstest.MapFS{name: {Data: []byte("updated release")}}, "v0.2.42")
		w := httptest.NewRecorder()
		first.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		etag := w.Header().Get("ETag")
		if etag == "" {
			t.Fatal("missing asset validator")
		}
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set("If-None-Match", etag)
		unchanged := httptest.NewRecorder()
		first.ServeHTTP(unchanged, request)
		if unchanged.Code != http.StatusNotModified {
			t.Fatal("unchanged asset did not revalidate", path, unchanged.Code)
		}
		changed := httptest.NewRecorder()
		second.ServeHTTP(changed, request)
		if changed.Code != 200 || changed.Body.String() != "updated release" || changed.Header().Get("ETag") == etag || changed.Header().Get("X-Lidza-Deploy-Version") != "v0.2.42" {
			t.Fatal("old cache validator reused after upgrade", path, changed.Code, changed.Header(), changed.Body)
		}
		request.Method = "HEAD"
		head := httptest.NewRecorder()
		second.ServeHTTP(head, request)
		if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("ETag") != changed.Header().Get("ETag") {
			t.Fatal("HEAD did not describe updated asset", path, head.Code, head.Header())
		}
	}
}

func TestConsoleViewsServeConsolePage(t *testing.T) {
	h := Handler()
	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest("GET", "/console.html", nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), `<div id="root">`) {
		t.Fatal("console page", page.Code)
	}
	for _, path := range []string{"/console", "/console/", "/console/applications", "/console/applications/acme-analytics/settings/environment", "/console/applications/a%2Fb/logs", "/console/deployments/build-one", "/console/servers/local/console", "/console/new-application?step=repository&server=local", "/console/unknown/deep/link"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Body.String() != page.Body.String() {
			t.Fatalf("%s: status=%d, want the console page", path, w.Code)
		}
		for _, header := range []string{"Cache-Control", "Content-Type", "ETag", "X-Lidza-Deploy-Version", "Content-Security-Policy"} {
			if w.Header().Get(header) != page.Header().Get(header) {
				t.Fatalf("%s: %s=%q, console.html has %q", path, header, w.Header().Get(header), page.Header().Get(header))
			}
		}
		head := httptest.NewRecorder()
		h.ServeHTTP(head, httptest.NewRequest("HEAD", path, nil))
		if head.Code != 200 || head.Body.Len() != 0 {
			t.Fatalf("HEAD %s: status=%d", path, head.Code)
		}
	}
	for _, path := range []string{"/consoles", "/console-old", "/assets/console/missing.js", "/missing.html"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code == 200 && w.Body.String() == page.Body.String() {
			t.Fatalf("%s served the console page", path)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != ConsoleHome {
		t.Fatal("root does not open the console", w.Code, w.Header().Get("Location"))
	}
}
