package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/agim/lidza-deploy/internal/release"
)

// Error and maintenance pages for hosts fronted by an admin-API Caddy.
//
// The agent owns a directory of neutral, self-contained pages (errorPagesDir)
// and writes them idempotently whenever Caddy fronts the host. Every created
// application published to Caddy also gets an error route for its hostnames
// (server "errors" routes, @id lidza-errors-<app>) that serves them: a 503 is
// answered with maintenance.html, any other error with <status>.html when
// present, else error.html. The pages are root-owned and world-readable
// (directory 0755, files 0644) so Caddy reads them under whatever account it
// runs as, while nobody but root can change them; the agent's 0077 umask is
// never relied on.
//
// Error routes that serve pages from another directory (one that is missing,
// or outside the managed directory) are reported in the ingress status, and
// POST /v1/host/ingress/error-pages points them at the managed directory by
// loading the modified configuration through the admin API.

// errorPagesDir holds the managed pages; tests point it elsewhere.
var errorPagesDir = "/etc/lidza/pages"

const nativeCaddyErrorPrefix = "lidza-errors-"

func nativeCaddyErrorRouteID(id string) string { return nativeCaddyErrorPrefix + id }

const errorPageStyle = `:root{color-scheme:light dark;--bg:#f6f7f8;--fg:#1d2329;--muted:#5b6670;--line:#d9dee3}
@media (prefers-color-scheme:dark){:root{--bg:#121518;--fg:#e8ecef;--muted:#a3adb6;--line:#2b3238}}
*{box-sizing:border-box}html,body{height:100%}
body{margin:0;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:16px/1.6 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;padding:24px}
main{max-width:34rem;border-top:3px solid var(--line);padding-top:24px}
h1{font-size:1.75rem;line-height:1.25;margin:0 0 12px}p{margin:0 0 8px;color:var(--muted)}`

func renderErrorPage(title, heading, body string) []byte {
	return []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex">
<meta name="color-scheme" content="light dark">
<title>` + title + `</title>
<style>` + errorPageStyle + `</style>
</head>
<body>
<main role="main">
<h1>` + heading + `</h1>
<p>` + body + `</p>
</main>
</body>
</html>
`)
}

// managedErrorPages are the files of errorPagesDir.
var managedErrorPages = map[string][]byte{
	"error.html":       renderErrorPage("Temporarily unavailable", "This site is temporarily unavailable", "This site is temporarily unavailable. Please try again in a few minutes."),
	"maintenance.html": renderErrorPage("Down for maintenance", "Down for maintenance", "Down for maintenance. We’ll be back shortly; please try again in a few minutes."),
}

var errorPagesMu sync.Mutex

// ensureErrorPages writes the managed pages when they differ from what is
// on disk, and fixes their modes. It returns whether anything changed.
func ensureErrorPages() (bool, error) {
	errorPagesMu.Lock()
	defer errorPagesMu.Unlock()
	dir := filepath.Clean(errorPagesDir)
	if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory", dir)
	}
	if err := release.MkdirAllReadable(filepath.Dir(dir)); err != nil {
		return false, err
	}
	changed := false
	if err := os.Mkdir(dir, 0o755); err == nil {
		changed = true
	} else if !errors.Is(err, fs.ErrExist) {
		return false, err
	}
	if info, err := os.Stat(dir); err != nil {
		return false, err
	} else if info.Mode().Perm() != 0o755 {
		if err := os.Chmod(dir, 0o755); err != nil {
			return false, err
		}
		changed = true
	}
	names := make([]string, 0, len(managedErrorPages))
	for name := range managedErrorPages {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		wrote, err := writeReadableFile(filepath.Join(dir, name), managedErrorPages[name])
		if err != nil {
			return changed, err
		}
		changed = changed || wrote
	}
	return changed, nil
}

// writeReadableFile writes a 0644 file atomically unless it already holds
// data with that mode. The mode is set explicitly, never from the umask.
func writeReadableFile(path string, data []byte) (bool, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o644 {
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
			return false, nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".page-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o644); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

// errorPagesReadable checks that an unprivileged account can reach the
// pages: every directory up to the pages must be traversable by others and
// the pages readable by others.
func errorPagesReadable() error {
	dir := filepath.Clean(errorPagesDir)
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o001 == 0 {
			return fmt.Errorf("%s is not traversable by the web server's account", current)
		}
		if parent := filepath.Dir(current); parent == current {
			break
		}
	}
	for name := range managedErrorPages {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o004 == 0 {
			return fmt.Errorf("%s is not readable by the web server's account", name)
		}
	}
	return nil
}

// caddyProcessUser names the account Caddy runs as, from the process table;
// "" when no Caddy process is found. Tests replace it.
var caddyProcessUser = func() string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "caddy" {
			continue
		}
		status, err := os.Open(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(status)
		uid := ""
		for scanner.Scan() {
			if rest, ok := strings.CutPrefix(scanner.Text(), "Uid:"); ok {
				if fields := strings.Fields(rest); len(fields) > 1 {
					uid = fields[1] // effective uid
				}
				break
			}
		}
		status.Close()
		if uid == "" {
			continue
		}
		if u, err := user.LookupId(uid); err == nil {
			return u.Username
		}
		return "uid " + uid
	}
	return ""
}

// renderNativeCaddyErrorRoute is the error route of one application: the
// managed directory as the file root, 503 as maintenance.html, other errors
// as <status>.html or error.html.
func renderNativeCaddyErrorRoute(id string, hosts []string, dir string) ([]byte, error) {
	if !idPattern.MatchString(id) || len(hosts) == 0 || dir == "" {
		return nil, errors.New("caddy error route needs an application, hostnames and a page directory")
	}
	route := map[string]any{
		"@id":   nativeCaddyErrorRouteID(id),
		"match": []any{map[string]any{"host": hosts}},
		"handle": []any{map[string]any{
			"handler": "subroute",
			"routes": []any{
				map[string]any{"handle": []any{map[string]any{"handler": "vars", "root": dir}}},
				map[string]any{
					"match":    []any{map[string]any{"vars": map[string]any{"{http.error.status_code}": []string{"503"}}}},
					"handle":   []any{map[string]any{"handler": "rewrite", "uri": "/maintenance.html"}, map[string]any{"handler": "file_server"}},
					"terminal": true,
				},
				map[string]any{
					"match":  []any{map[string]any{"file": map[string]any{"try_files": []string{"/{http.error.status_code}.html", "/error.html"}}}},
					"handle": []any{map[string]any{"handler": "rewrite", "uri": "{http.matchers.file.relative}"}},
				},
				map[string]any{"handle": []any{map[string]any{"handler": "file_server"}}},
			},
		}},
		"terminal": true,
	}
	return json.Marshal(route)
}

// ensureErrorRoute adds or replaces the application's error route; an
// identical route is left alone. New routes go first so they take
// precedence over broader error routes of the server.
func (c nativeCaddy) ensureErrorRoute(ctx context.Context, id string, hosts []string) error {
	route, err := renderNativeCaddyErrorRoute(id, hosts, filepath.Clean(errorPagesDir))
	if err != nil {
		return err
	}
	var current json.RawMessage
	if found, err := c.get(ctx, "/id/"+nativeCaddyErrorRouteID(id), &current); err != nil {
		return err
	} else if found && jsonEqual(current, route) {
		return nil
	}
	status, err := c.call(ctx, http.MethodPatch, "/id/"+nativeCaddyErrorRouteID(id), route)
	if err != nil || status != http.StatusNotFound {
		return err
	}
	base := "/config/apps/http/servers/" + c.cfg.Server + "/errors"
	var errorsBlock struct {
		Routes []json.RawMessage `json:"routes"`
	}
	found, err := c.get(ctx, base, &errorsBlock)
	switch {
	case err != nil:
		return err
	case !found:
		_, err = c.call(ctx, http.MethodPost, base, mustJSON(map[string]any{"routes": []json.RawMessage{route}}))
	case errorsBlock.Routes == nil:
		_, err = c.call(ctx, http.MethodPost, base+"/routes", mustJSON([]json.RawMessage{route}))
	default:
		_, err = c.call(ctx, http.MethodPut, base+"/routes/0", route)
	}
	return err
}

// withdrawErrorRoute removes the application's error route; a missing
// route is not an error.
func (c nativeCaddy) withdrawErrorRoute(ctx context.Context, id string) error {
	_, err := c.call(ctx, http.MethodDelete, "/id/"+nativeCaddyErrorRouteID(id), nil)
	return err
}

// publishErrorPages writes the pages and the application's error route.
// Failures are logged: error pages never block publishing an application.
func (c nativeCaddy) publishErrorPages(ctx context.Context, id string, hosts []string) {
	if _, err := ensureErrorPages(); err != nil {
		log.Printf("error pages: %v", err)
		return
	}
	if err := c.ensureErrorRoute(ctx, id, hosts); err != nil {
		log.Printf("native %s: error route: %v", id, err)
	}
}

// ---- status and adoption of foreign roots ---------------------------------------

// HostErrorPages reports the managed error pages and the error routes that
// serve pages from elsewhere.
type HostErrorPages struct {
	Dir       string `json:"dir"`
	Ready     bool   `json:"ready"`
	Problem   string `json:"problem,omitempty"`
	CaddyUser string `json:"caddy_user,omitempty"`
	// Managed counts error routes that serve the managed pages.
	Managed int `json:"managed"`
	// Unmanaged lists error-page roots outside the managed directory;
	// UnmanagedHosts counts the hostnames they serve.
	Unmanaged      []HostErrorPageRoot `json:"unmanaged,omitempty"`
	UnmanagedHosts int                 `json:"unmanaged_hosts"`
	Detail         string              `json:"detail,omitempty"`
}

// HostErrorPageRoot is one error-page directory of a Caddy error route.
type HostErrorPageRoot struct {
	Server string   `json:"server"`
	Root   string   `json:"root"`
	Exists bool     `json:"exists"`
	Hosts  []string `json:"hosts"`
}

// errorPageRootManaged reports whether root is the managed directory or
// inside it.
func errorPageRootManaged(root string) bool {
	dir := filepath.Clean(errorPagesDir)
	root = filepath.Clean(root)
	return root == dir || strings.HasPrefix(root, dir+string(filepath.Separator))
}

// walkErrorPageRoots calls visit for every handler with a literal file
// root inside the error routes of every server. visit may change the
// handler's root.
func walkErrorPageRoots(config map[string]any, visit func(server string, hosts []string, handler map[string]any)) {
	apps, _ := config["apps"].(map[string]any)
	httpApp, _ := apps["http"].(map[string]any)
	servers, _ := httpApp["servers"].(map[string]any)
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		server, _ := servers[name].(map[string]any)
		block, _ := server["errors"].(map[string]any)
		routes, _ := block["routes"].([]any)
		for _, raw := range routes {
			route, _ := raw.(map[string]any)
			hosts := caddyMatchHosts(route["match"])
			walkHandlers(route["handle"], 0, func(handler map[string]any) { visit(name, hosts, handler) })
		}
	}
}

func caddyMatchHosts(match any) []string {
	var hosts []string
	list, _ := match.([]any)
	for _, raw := range list {
		matcher, _ := raw.(map[string]any)
		values, _ := matcher["host"].([]any)
		for _, v := range values {
			if host, ok := v.(string); ok {
				hosts = append(hosts, host)
			}
		}
	}
	return hosts
}

func walkHandlers(handle any, depth int, visit func(map[string]any)) {
	if depth > 8 {
		return
	}
	list, _ := handle.([]any)
	for _, raw := range list {
		handler, _ := raw.(map[string]any)
		if handler == nil {
			continue
		}
		kind, _ := handler["handler"].(string)
		if root, ok := handler["root"].(string); ok && (kind == "vars" || kind == "file_server") && root != "" && !strings.Contains(root, "{") {
			visit(handler)
		}
		if kind == "subroute" {
			routes, _ := handler["routes"].([]any)
			for _, rawRoute := range routes {
				route, _ := rawRoute.(map[string]any)
				walkHandlers(route["handle"], depth+1, visit)
			}
		}
	}
}

// errorPageReport scans a configuration's error routes.
func errorPageReport(config map[string]any) HostErrorPages {
	out := HostErrorPages{Dir: filepath.Clean(errorPagesDir)}
	seen := map[string]int{}
	hostSet := map[string]bool{}
	walkErrorPageRoots(config, func(server string, hosts []string, handler map[string]any) {
		root := handler["root"].(string)
		if errorPageRootManaged(root) {
			out.Managed++
			return
		}
		key := server + "\x00" + filepath.Clean(root)
		i, ok := seen[key]
		if !ok {
			_, err := os.Stat(root)
			out.Unmanaged = append(out.Unmanaged, HostErrorPageRoot{Server: server, Root: filepath.Clean(root), Exists: err == nil, Hosts: []string{}})
			i = len(out.Unmanaged) - 1
			seen[key] = i
		}
		for _, host := range hosts {
			if !slices.Contains(out.Unmanaged[i].Hosts, host) {
				out.Unmanaged[i].Hosts = append(out.Unmanaged[i].Hosts, host)
			}
			hostSet[strings.ToLower(host)] = true
		}
	})
	out.UnmanagedHosts = len(hostSet)
	if len(out.Unmanaged) > 0 {
		subject := fmt.Sprintf("%d hostnames", out.UnmanagedHosts)
		switch out.UnmanagedHosts {
		case 0:
			subject = "every hostname"
		case 1:
			subject = "1 hostname"
		}
		out.Detail = "error pages for " + subject + " are served from an unmanaged directory"
	}
	return out
}

// readConfig reads the complete Caddy configuration as a JSON tree.
func (c nativeCaddy) readConfig(ctx context.Context) (map[string]any, error) {
	var config map[string]any
	found, err := c.get(ctx, "/config/", &config)
	if err != nil {
		return nil, err
	}
	if !found || config == nil {
		return nil, errors.New("caddy has no configuration")
	}
	return config, nil
}

// hostErrorPages reports the pages and the error routes of the Caddy that
// fronts the host; nil when the agent proxy is the ingress.
func (m *Manager) hostErrorPages(ctx context.Context) *HostErrorPages {
	caddy, ok := m.nativeCaddy(ctx)
	if !ok {
		return nil
	}
	out := HostErrorPages{Dir: filepath.Clean(errorPagesDir)}
	if config, err := caddy.readConfig(ctx); err != nil {
		out.Problem = "the Caddy configuration could not be read: " + boundedErrorText(err.Error(), 200)
	} else {
		out = errorPageReport(config)
	}
	out.CaddyUser = caddyProcessUser()
	if _, err := ensureErrorPages(); err != nil {
		out.Problem = "the error pages could not be written: " + boundedErrorText(err.Error(), 200)
	} else if err := errorPagesReadable(); err != nil {
		out.Problem = boundedErrorText(err.Error(), 200)
	} else {
		out.Ready = true
	}
	return &out
}

// adoptErrorPages points every unmanaged error-page root at the managed
// directory: it loads the modified configuration (POST /load) and checks
// that Caddy holds it afterwards.
func (m *Manager) adoptErrorPages(ctx context.Context) error {
	caddy, ok := m.nativeCaddy(ctx)
	if !ok {
		return errors.New("this host is not fronted by an admin-API Caddy")
	}
	if _, err := ensureErrorPages(); err != nil {
		return fmt.Errorf("the error pages could not be written: %w", err)
	}
	if err := errorPagesReadable(); err != nil {
		return err
	}
	config, err := caddy.readConfig(ctx)
	if err != nil {
		return err
	}
	dir := filepath.Clean(errorPagesDir)
	changed := 0
	walkErrorPageRoots(config, func(_ string, _ []string, handler map[string]any) {
		if !errorPageRootManaged(handler["root"].(string)) {
			handler["root"] = dir
			changed++
		}
	})
	if changed == 0 {
		return nil
	}
	status, err := caddy.call(ctx, http.MethodPost, "/load", mustJSON(config))
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return errors.New("the Caddy admin API has no /load endpoint")
	}
	after, err := caddy.readConfig(ctx)
	if err != nil {
		return fmt.Errorf("could not verify the new configuration: %w", err)
	}
	if report := errorPageReport(after); len(report.Unmanaged) > 0 {
		return fmt.Errorf("caddy still serves error pages from %s after the update", report.Unmanaged[0].Root)
	}
	return nil
}

// ensureErrorPagesForIngress writes the pages when the detection found a
// fronting Caddy; the ingress loop calls it after each detection. It reads
// neither the manager's state nor its configuration (the loop never takes a
// lock); an explicitly configured Caddy gets its pages when an application
// is published or the ingress status is read.
func ensureErrorPagesForIngress(detected *HostIngressStatus) {
	if detected == nil || detected.Mode != HostIngressCaddy {
		return
	}
	if _, err := ensureErrorPages(); err != nil {
		log.Printf("error pages: %v", err)
	}
}

// nativeUnavailablePage answers a request for a created application whose
// processes do not answer, with the managed error page.
func nativeUnavailablePage(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "60")
	w.WriteHeader(status)
	_, _ = w.Write(managedErrorPages["error.html"])
}
