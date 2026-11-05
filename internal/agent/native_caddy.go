package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Hosts whose web ports belong to a Caddy server configured through its
// admin API (rather than the agent's own TLS listener) publish created
// applications with one route each. The route sends the application's
// hostnames to the agent's proxy, which serves process applications from
// their loopback port and static sites from their release, so Caddy needs no
// access to application files. Caddy obtains certificates for the routed
// hostnames itself. Routes carry an @id so they are replaced and removed
// exactly, and nothing else in the Caddy configuration is touched.

// NativeCaddyConfig names the Caddy admin API and server that front this host.
type NativeCaddyConfig struct {
	Admin  string `json:"admin"`            // e.g. http://127.0.0.1:2019 (loopback only)
	Server string `json:"server,omitempty"` // HTTPS server name; default srv0
	// Upstream is where routed requests go; default the agent's proxy_listen.
	Upstream string `json:"upstream,omitempty"`
}

var nativeCaddyServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (c NativeCaddyConfig) validate() error {
	u, err := url.Parse(c.Admin)
	if err != nil || u.Scheme != "http" || u.Path != "" && u.Path != "/" || u.User != nil {
		return errors.New("native caddy admin must be a loopback http URL")
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("native caddy admin must be a loopback http URL")
	}
	if c.Server != "" && !nativeCaddyServerName.MatchString(c.Server) {
		return errors.New("native caddy server name is invalid")
	}
	if c.Upstream != "" {
		if host, _, err := net.SplitHostPort(c.Upstream); err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			return errors.New("native caddy upstream must be a loopback host:port")
		}
	}
	return nil
}

func nativeCaddyRouteID(id string) string { return "lidza-app-" + id }

// renderNativeCaddyRoute is the route for one created application.
func renderNativeCaddyRoute(id string, hosts []string, upstream string) ([]byte, error) {
	if !idPattern.MatchString(id) || len(hosts) == 0 || upstream == "" {
		return nil, errors.New("caddy route needs an application, hostnames and an upstream")
	}
	route := map[string]any{
		"@id":   nativeCaddyRouteID(id),
		"match": []any{map[string]any{"host": hosts}},
		"handle": []any{map[string]any{
			"handler": "subroute",
			"routes": []any{map[string]any{"handle": []any{
				map[string]any{"handler": "encode", "encodings": map[string]any{"zstd": map[string]any{}, "gzip": map[string]any{}}, "prefer": []string{"zstd", "gzip"}},
				map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": upstream}}},
			}}},
		}},
		"terminal": true,
	}
	return json.Marshal(route)
}

type nativeCaddy struct {
	cfg    NativeCaddyConfig
	client *http.Client
}

func (m *Manager) nativeCaddy() (nativeCaddy, bool) {
	if m.cfg.Native == nil || m.cfg.Native.Caddy == nil || m.cfg.Native.Caddy.Admin == "" {
		return nativeCaddy{}, false
	}
	cfg := *m.cfg.Native.Caddy
	if cfg.Server == "" {
		cfg.Server = "srv0"
	}
	if cfg.Upstream == "" {
		cfg.Upstream = m.cfg.ProxyListen
	}
	return nativeCaddy{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}}, true
}

func (c nativeCaddy) call(ctx context.Context, method, path string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.cfg.Admin, "/")+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("caddy admin unavailable: %w", err)
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return resp.StatusCode, fmt.Errorf("caddy admin %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return resp.StatusCode, nil
}

// publish adds or replaces the application's route. New routes go first so a
// broader wildcard route elsewhere in the server cannot shadow them.
func (c nativeCaddy) publish(ctx context.Context, id string, hosts []string) error {
	route, err := renderNativeCaddyRoute(id, hosts, c.cfg.Upstream)
	if err != nil {
		return err
	}
	status, err := c.call(ctx, http.MethodPatch, "/id/"+nativeCaddyRouteID(id), route)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound {
		return nil
	}
	_, err = c.call(ctx, http.MethodPut, "/config/apps/http/servers/"+c.cfg.Server+"/routes/0", route)
	return err
}

// withdraw removes the application's route; a missing route is not an error.
func (c nativeCaddy) withdraw(ctx context.Context, id string) error {
	_, err := c.call(ctx, http.MethodDelete, "/id/"+nativeCaddyRouteID(id), nil)
	return err
}

// publishNativeCaddyRoute publishes a created application's hostnames when
// this host is fronted by an admin-API Caddy; otherwise it does nothing.
func (m *Manager) publishNativeCaddyRoute(ctx context.Context, id, domain string, additional []string) error {
	caddy, ok := m.nativeCaddy()
	if !ok {
		return nil
	}
	return caddy.publish(ctx, id, append([]string{domain}, additional...))
}

func (m *Manager) withdrawNativeCaddyRoute(ctx context.Context, id string) error {
	caddy, ok := m.nativeCaddy()
	if !ok {
		return nil
	}
	return caddy.withdraw(ctx, id)
}
