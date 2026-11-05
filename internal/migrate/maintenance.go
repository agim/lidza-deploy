package migrate

import (
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
)

// Maintenance is a per-application maintenance switch that the web server
// evaluates on every request: while the marker file exists, the server
// answers 503 with the maintenance page instead of proxying to the
// application. Toggling it needs no server reload and no application
// restart, which makes it the write freeze used during cutover.
type Maintenance struct {
	Dir string // directory holding the marker and page, for example <root>/shared/maintenance
}

const (
	maintenanceMarker = "enabled"
	maintenancePage   = "index.html"
)

func (m Maintenance) Marker() string { return filepath.Join(m.Dir, maintenanceMarker) }
func (m Maintenance) Page() string   { return filepath.Join(m.Dir, maintenancePage) }

// Enable writes the page (when message is not empty) and creates the marker.
func (m Maintenance) Enable(message string) error {
	if !filepath.IsAbs(m.Dir) {
		return errors.New("migrate: maintenance directory must be absolute")
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	if message != "" || !exists(m.Page()) {
		if message == "" {
			message = "We are performing scheduled maintenance. Please try again shortly."
		}
		if err := os.WriteFile(m.Page(), []byte(maintenanceHTML(message)), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(m.Marker(), []byte("maintenance\n"), 0o644)
}

// Disable removes the marker; the page stays for the next time.
func (m Maintenance) Disable() error {
	err := os.Remove(m.Marker())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Enabled reports whether requests are currently answered with the page.
func (m Maintenance) Enabled() bool { return exists(m.Marker()) }

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func maintenanceHTML(message string) string {
	return `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Maintenance</title><style>body{background:#171214;color:#eee7e3;font:18px system-ui;margin:0;min-height:100vh;display:grid;place-items:center}main{max-width:38rem;padding:3rem}h1{font-size:2rem}</style><main><h1>We’ll be back soon</h1><p>` + html.EscapeString(message) + `</p></main></html>` + "\n"
}

// CaddySite renders a Caddyfile site block that serves the maintenance page
// while the marker exists and proxies to upstream otherwise. The marker is
// checked per request with the file matcher, so Enable and Disable take
// effect immediately. Health checks at healthPath bypass maintenance so
// monitoring keeps seeing the application.
func (m Maintenance) CaddySite(domain, upstream, healthPath string) (string, error) {
	switch {
	case !hostnamePattern.MatchString(domain):
		return "", fmt.Errorf("migrate: invalid domain %q", domain)
	case upstream == "" || strings.ContainsAny(upstream, " \n{}"):
		return "", fmt.Errorf("migrate: invalid upstream %q", upstream)
	case !filepath.IsAbs(m.Dir) || strings.ContainsAny(m.Dir, " \n{}\""):
		return "", errors.New("migrate: maintenance directory must be an absolute path without spaces")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s {\n", domain)
	fmt.Fprintf(&b, "\t@maintenance {\n\t\tfile {\n\t\t\troot %s\n\t\t\ttry_files %s\n\t\t}\n", m.Dir, maintenanceMarker)
	if healthPath != "" && strings.HasPrefix(healthPath, "/") {
		fmt.Fprintf(&b, "\t\tnot path %s\n", healthPath)
	}
	b.WriteString("\t}\n")
	fmt.Fprintf(&b, "\thandle @maintenance {\n\t\troot * %s\n\t\trewrite * /%s\n\t\theader Retry-After 60\n\t\theader Cache-Control no-store\n\t\tfile_server {\n\t\t\tstatus 503\n\t\t}\n\t}\n", m.Dir, maintenancePage)
	fmt.Fprintf(&b, "\thandle {\n\t\treverse_proxy %s\n\t}\n}\n", upstream)
	return b.String(), nil
}
