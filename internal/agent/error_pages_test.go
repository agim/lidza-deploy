package agent

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// withErrorPagesDir points the managed pages at a fresh readable directory.
func withErrorPagesDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for dir := root; dir != filepath.Dir(dir) && strings.HasPrefix(dir, os.TempDir()) && dir != filepath.Clean(os.TempDir()); dir = filepath.Dir(dir) {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	previous := errorPagesDir
	errorPagesDir = filepath.Join(root, "lidza", "pages")
	t.Cleanup(func() { errorPagesDir = previous })
	return errorPagesDir
}

func TestErrorPagesAreNeutralReadableAndWrittenOnce(t *testing.T) {
	dir := withErrorPagesDir(t)
	old := syscall.Umask(0o077) // the agent's umask
	defer syscall.Umask(old)
	changed, err := ensureErrorPages()
	if err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	for _, path := range []string{filepath.Dir(dir), dir} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("%s: %v %v", path, info.Mode(), err)
		}
	}
	for name, want := range map[string]string{"error.html": "This site is temporarily unavailable", "maintenance.html": "Down for maintenance"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("%s: %v %v", name, info.Mode(), err)
		}
		page, _ := os.ReadFile(path)
		text := string(page)
		if !strings.Contains(text, want) || !strings.Contains(text, `lang="en"`) || !strings.Contains(text, "prefers-color-scheme:dark") || !strings.Contains(text, "<h1>") {
			t.Fatalf("%s is not the neutral page: %s", name, text)
		}
		for _, banned := range []string{"src=", "href=", "url(", "http", "Līdza", "Lidza", "Caddy"} {
			if strings.Contains(text, banned) {
				t.Fatalf("%s must be self-contained and neutral; found %q", name, banned)
			}
		}
	}
	if err := errorPagesReadable(); err != nil {
		t.Fatal(err)
	}
	if changed, err := ensureErrorPages(); err != nil || changed {
		t.Fatalf("second write: %v %v", changed, err)
	}
	// A page changed by hand or made private is restored.
	_ = os.WriteFile(filepath.Join(dir, "error.html"), []byte("edited"), 0o600)
	_ = os.Chmod(filepath.Join(dir, "error.html"), 0o600)
	if changed, err := ensureErrorPages(); err != nil || !changed {
		t.Fatalf("repair: %v %v", changed, err)
	}
	if page, _ := os.ReadFile(filepath.Join(dir, "error.html")); !bytes.Equal(page, managedErrorPages["error.html"]) {
		t.Fatal("the edited page was not restored")
	}
	if info, _ := os.Stat(filepath.Join(dir, "error.html")); info.Mode().Perm() != 0o644 {
		t.Fatalf("mode after repair: %v", info.Mode())
	}
	// A private ancestor is reported.
	_ = os.Chmod(filepath.Dir(dir), 0o700)
	if err := errorPagesReadable(); err == nil || !strings.Contains(err.Error(), "not traversable") {
		t.Fatalf("private parent: %v", err)
	}
}

func TestPublishedApplicationsGetErrorRoutes(t *testing.T) {
	dir := withErrorPagesDir(t)
	admin := newFakeCaddyAdmin(t, operatorCaddyConfig())
	m := caddyManager(admin, "https")
	ctx := context.Background()
	if _, err := m.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", []string{"www.shop.example.com"}); err != nil {
		t.Fatal(err)
	}
	route, _ := admin.at("/config/apps/http/servers/https/errors/routes/0").(map[string]any)
	if route["@id"] != "lidza-errors-shop" || strings.Join(caddyMatchHosts(route["match"]), ",") != "shop.example.com,www.shop.example.com" {
		t.Fatalf("error route: %v", admin.at("/config/apps/http/servers/https/errors"))
	}
	handlers := admin.at("/config/apps/http/servers/https/errors/routes/0/handle/0/routes").([]any)
	vars := handlers[0].(map[string]any)["handle"].([]any)[0].(map[string]any)
	if vars["handler"] != "vars" || vars["root"] != dir {
		t.Fatalf("pages root: %v", vars)
	}
	raw, _ := renderNativeCaddyErrorRoute("shop", []string{"shop.example.com"}, dir)
	for _, want := range []string{`"/maintenance.html"`, `"{http.error.status_code}":["503"]`, `"try_files":["/{http.error.status_code}.html","/error.html"]`, `"uri":"{http.matchers.file.relative}"`, `"handler":"file_server"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("error route lacks %s: %s", want, raw)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "maintenance.html")); err != nil {
		t.Fatal("publishing must write the pages")
	}

	// A second application's route goes first; republishing writes nothing.
	if _, err := m.publishNativeCaddyRoute(ctx, "blog", "blog.example.com", nil); err != nil {
		t.Fatal(err)
	}
	if first, _ := admin.at("/config/apps/http/servers/https/errors/routes/0").(map[string]any); first["@id"] != "lidza-errors-blog" {
		t.Fatalf("routes: %v", admin.at("/config/apps/http/servers/https/errors/routes"))
	}
	admin.reset()
	if _, err := m.publishNativeCaddyRoute(ctx, "shop", "shop.example.com", []string{"www.shop.example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, write := range []string{"PATCH ", "PUT ", "POST ", "DELETE "} {
		if admin.called(write) != 0 {
			t.Fatalf("republish wrote to Caddy: %v", admin.calls)
		}
	}
	if err := m.withdrawNativeCaddyRoute(ctx, "shop", []string{"shop.example.com"}); err != nil {
		t.Fatal(err)
	}
	if routes, _ := admin.at("/config/apps/http/servers/https/errors/routes").([]any); len(routes) != 1 {
		t.Fatalf("error routes after withdrawing shop: %v", routes)
	}
}

// foreignErrorRoute is an error route that serves pages from root.
func foreignErrorRoute(root string, hosts ...string) map[string]any {
	list := []any{}
	for _, h := range hosts {
		list = append(list, h)
	}
	return map[string]any{"match": []any{map[string]any{"host": list}}, "handle": []any{map[string]any{"handler": "subroute", "routes": []any{
		map[string]any{"handle": []any{map[string]any{"handler": "vars", "root": root}}},
		map[string]any{"match": []any{map[string]any{"file": map[string]any{"try_files": []any{"/{http.error.status_code}.html", "/error.html"}}}}, "handle": []any{map[string]any{"handler": "rewrite", "uri": "{http.matchers.file.relative}"}}},
		map[string]any{"handle": []any{map[string]any{"handler": "file_server"}}},
	}}}}
}

func TestUnmanagedErrorPageRootsAreReportedAndAdopted(t *testing.T) {
	dir := withErrorPagesDir(t)
	config := operatorCaddyConfig()
	https := config["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["https"].(map[string]any)
	gone := filepath.Join(t.TempDir(), "home", "deploy", "pages")
	https["errors"] = map[string]any{"routes": []any{
		foreignErrorRoute(gone, "a.example.com", "b.example.com"),
		foreignErrorRoute(gone, "c.example.com"),
		foreignErrorRoute(dir, "managed.example.com"),
	}}
	admin := newFakeCaddyAdmin(t, config)
	m := caddyManager(admin, "https")
	ctx := context.Background()

	status := m.hostIngressReport(ctx, false)
	pages := status.ErrorPages
	if pages == nil || !pages.Ready || pages.Dir != dir || pages.Managed != 1 || pages.UnmanagedHosts != 3 || len(pages.Unmanaged) != 1 || pages.Unmanaged[0].Exists || pages.CaddyUser != "caddy" {
		t.Fatalf("report: %+v", pages)
	}
	if pages.Detail != "error pages for 3 hostnames are served from an unmanaged directory" {
		t.Fatalf("detail: %q", pages.Detail)
	}

	mux := http.NewServeMux()
	m.hostIngressRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/host/ingress/error-pages", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"unmanaged_hosts":0`) {
		t.Fatalf("adopt: %d %s", w.Code, w.Body)
	}
	if admin.called("POST /load") != 1 {
		t.Fatalf("calls: %v", admin.calls)
	}
	for i := range 3 {
		root := admin.at("/config/apps/http/servers/https/errors/routes/" + string(rune('0'+i)) + "/handle/0/routes/0/handle/0/root")
		if root != dir {
			t.Fatalf("route %d root: %v", i, root)
		}
	}
	// The rest of the configuration is untouched.
	if route, _ := admin.at("/config/apps/http/servers/https/routes/0").(map[string]any); route["@id"] != "ops" {
		t.Fatalf("routes changed: %v", admin.at("/config/apps/http/servers/https/routes"))
	}
	// Nothing left to adopt: no second load.
	admin.reset()
	if err := m.adoptErrorPages(ctx); err != nil || admin.called("POST /load") != 0 {
		t.Fatalf("second adopt: %v %v", err, admin.calls)
	}
}

func TestCreatedApplicationOutageServesTheErrorPage(t *testing.T) {
	w := httptest.NewRecorder()
	nativeUnavailablePage(w, http.StatusBadGateway)
	if w.Code != 502 || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(w.Body.String(), "This site is temporarily unavailable") {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body)
	}
}
