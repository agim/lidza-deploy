package agent

// Forwarding a retired control panel's hostname. When a panel moves, DNS
// resolvers keep answering with this host's address for up to the record's
// TTL. Retiring the panel here used to leave its route proxying to a
// stopped port, so those visitors got 502. The retire replaces every route
// of the hostname with a reverse proxy to the destination's public address
// on 443 (TLS with the hostname as server name, Host kept), records it with
// an expiry, and the control route loop keeps it in place (a Caddy reload
// from its own configuration can bring the old route back) and removes it
// once it expires.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	controlForwardRouteID = "lidza-control-forward"
	// controlForwardFor is how long the forward stays: far beyond any
	// resolver's cache, short enough not to leave a stray route behind.
	controlForwardFor = 7 * 24 * time.Hour
)

// Replaced in tests.
var (
	controlForwardNow = time.Now
	// controlOwnAddress reports an address of this host: an interface
	// address or the detected public address (behind NAT).
	controlOwnAddress = func(ip netip.Addr) bool {
		addresses := moveDetectAddresses()
		for _, own := range []string{addresses.IPv4, addresses.IPv6} {
			if parsed, err := netip.ParseAddr(own); err == nil && parsed.Unmap() == ip {
				return true
			}
		}
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			if prefix, err := netip.ParsePrefix(a.String()); err == nil && prefix.Addr().Unmap() == ip {
				return true
			}
		}
		return false
	}
)

// ControlForward is a retired panel hostname forwarded to its new server.
type ControlForward struct {
	Hostname string `json:"hostname"`
	// Address is the destination's public address; the forward dials it on
	// 443.
	Address string `json:"address"`
	// Route is how it is published: caddy (admin API route), caddyfile (a
	// marked site block) or agent (this agent's proxy).
	Route   string    `json:"route"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

func (m *Manager) controlForwardPath() string {
	return filepath.Join(m.cfg.DataDir, "control-forward.json")
}

func (m *Manager) controlForward() *ControlForward {
	var f ControlForward
	raw, err := os.ReadFile(m.controlForwardPath())
	if err != nil || json.Unmarshal(raw, &f) != nil || f.Hostname == "" || f.Address == "" {
		return nil
	}
	return &f
}

// saveControlForward records the forward; nil removes the record.
func (m *Manager) saveControlForward(f *ControlForward) error {
	if f == nil {
		err := os.Remove(m.controlForwardPath())
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(m.cfg.DataDir, 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(f, "", "  ")
	return writeFileAtomic(m.controlForwardPath(), raw, 0o600)
}

// controlForwardAddress validates the destination address: a unicast IP
// that is not this host (a forward to itself would loop).
func controlForwardAddress(raw string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(raw), "[]"))
	if err != nil {
		return netip.Addr{}, errors.New("the destination's address is missing or invalid")
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() && !ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
		return netip.Addr{}, fmt.Errorf("%s is not an address the forward can reach", ip)
	}
	if controlOwnAddress(ip) {
		return netip.Addr{}, fmt.Errorf("%s is this server's own address; forwarding to it would loop", ip)
	}
	return ip, nil
}

func controlForwardDial(address string) string { return net.JoinHostPort(address, "443") }

// renderControlForwardRoute is the admin-API route that forwards host to
// the destination: HTTPS to address:443 with host as TLS server name, so the
// destination's certificate is verified, and the visitor's Host header.
func renderControlForwardRoute(host, address string) []byte {
	return mustJSON(map[string]any{
		"@id":   controlForwardRouteID,
		"match": []any{map[string]any{"host": []string{host}}},
		"handle": []any{map[string]any{
			"handler":   "reverse_proxy",
			"upstreams": []any{map[string]any{"dial": controlForwardDial(address)}},
			"transport": map[string]any{"protocol": "http", "tls": map[string]any{"server_name": host}},
			"headers":   map[string]any{"request": map[string]any{"set": map[string]any{"Host": []string{"{http.request.host}"}}}},
		}},
		"terminal": true,
	})
}

// controlForwardBody is the same forward as a Caddyfile site body.
func controlForwardBody(host, address string) string {
	return "\treverse_proxy https://" + controlForwardDial(address) + " {\n" +
		"\t\theader_up Host {host}\n" +
		"\t\ttransport http {\n\t\t\ttls_server_name " + host + "\n\t\t}\n" +
		"\t}\n"
}

type caddyRouteOp struct {
	server string
	index  int
	method string
	body   []byte
}

// placeControlForward makes the forward route the only route of host:
// the first route matching only host is replaced in place (it keeps its
// precedence), other such routes are deleted, and host is dropped from
// routes that match further hostnames. Without any, the route goes first
// into the HTTPS server. Nothing is written when it is already in place.
func (c nativeCaddy) placeControlForward(ctx context.Context, host string, route []byte) error {
	var servers map[string]struct {
		Routes []json.RawMessage `json:"routes"`
	}
	if _, err := c.get(ctx, "/config/apps/http/servers", &servers); err != nil {
		return err
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	ops := []caddyRouteOp{}
	placed := false
	for _, name := range names {
		for i, raw := range servers[name].Routes {
			var current caddyRoute
			if json.Unmarshal(raw, &current) != nil {
				continue
			}
			if current.ID == controlForwardRouteID {
				switch {
				case placed:
					ops = append(ops, caddyRouteOp{server: name, index: i, method: http.MethodDelete})
				case !jsonEqual(raw, route):
					ops = append(ops, caddyRouteOp{server: name, index: i, method: http.MethodPatch, body: route})
				}
				placed = true
				continue
			}
			hosts := caddyRouteHosts(current)
			if !slices.ContainsFunc(hosts, func(h string) bool { return strings.EqualFold(h, host) }) {
				continue
			}
			if !slices.ContainsFunc(hosts, func(h string) bool { return !strings.EqualFold(h, host) }) {
				if placed {
					ops = append(ops, caddyRouteOp{server: name, index: i, method: http.MethodDelete})
				} else {
					ops = append(ops, caddyRouteOp{server: name, index: i, method: http.MethodPatch, body: route})
					placed = true
				}
				continue
			}
			if rest, ok := caddyRouteWithoutHost(raw, host); ok {
				ops = append(ops, caddyRouteOp{server: name, index: i, method: http.MethodPatch, body: rest})
			}
		}
	}
	// Later indexes first, so a deletion does not shift the next one.
	sort.SliceStable(ops, func(a, b int) bool {
		if ops[a].server != ops[b].server {
			return ops[a].server < ops[b].server
		}
		return ops[a].index > ops[b].index
	})
	for _, op := range ops {
		if _, err := c.call(ctx, op.method, fmt.Sprintf("/config/apps/http/servers/%s/routes/%d", op.server, op.index), op.body); err != nil {
			return err
		}
	}
	if !placed {
		_, err := c.call(ctx, http.MethodPut, "/config/apps/http/servers/"+c.cfg.Server+"/routes/0", route)
		return err
	}
	return nil
}

// caddyRouteWithoutHost drops host from a route's host matchers; a matcher
// set left without hostnames is dropped with it, so it cannot match every
// hostname.
func caddyRouteWithoutHost(raw json.RawMessage, host string) ([]byte, bool) {
	var route map[string]any
	if json.Unmarshal(raw, &route) != nil {
		return nil, false
	}
	sets, _ := route["match"].([]any)
	kept := []any{}
	for _, set := range sets {
		matcher, ok := set.(map[string]any)
		if !ok {
			kept = append(kept, set)
			continue
		}
		list, has := matcher["host"].([]any)
		if !has {
			kept = append(kept, set)
			continue
		}
		hosts := []any{}
		for _, h := range list {
			if s, _ := h.(string); !strings.EqualFold(s, host) {
				hosts = append(hosts, h)
			}
		}
		if len(hosts) == 0 {
			continue
		}
		matcher["host"] = hosts
		kept = append(kept, matcher)
	}
	if len(kept) == 0 {
		return nil, false
	}
	route["match"] = kept
	return mustJSON(route), true
}

// installControlForward forwards host to address for controlForwardFor
// through this host's ingress and records it.
func (m *Manager) installControlForward(ctx context.Context, host, address string) (*ControlForward, error) {
	ip, err := controlForwardAddress(address)
	if err != nil {
		return nil, err
	}
	mode, _ := m.controlIngress(ctx)
	now := controlForwardNow().UTC()
	f := &ControlForward{Hostname: host, Address: ip.String(), Route: mode, Created: now, Expires: now.Add(controlForwardFor)}
	if err := m.applyControlForward(ctx, f); err != nil {
		return nil, err
	}
	if err := m.saveControlForward(f); err != nil {
		return nil, fmt.Errorf("the forward is in place but could not be recorded: %w", err)
	}
	return f, nil
}

// applyControlForward puts the forward in place; idempotent.
func (m *Manager) applyControlForward(ctx context.Context, f *ControlForward) error {
	switch f.Route {
	case "caddy":
		caddy, ok := m.nativeCaddy(ctx)
		if !ok {
			return errors.New("the Caddy admin API is unavailable")
		}
		return caddy.placeControlForward(ctx, f.Hostname, renderControlForwardRoute(f.Hostname, f.Address))
	case "caddyfile":
		return m.editControlCaddyfile(ctx, f.Hostname, controlForwardBody(f.Hostname, f.Address))
	}
	return nil // the agent's proxy forwards (serveControlForward)
}

// removeControlForward withdraws an expired forward.
func (m *Manager) removeControlForward(ctx context.Context, f *ControlForward) error {
	switch f.Route {
	case "caddy":
		caddy, ok := m.nativeCaddy(ctx)
		if !ok {
			return errors.New("the Caddy admin API is unavailable")
		}
		_, err := caddy.call(ctx, http.MethodDelete, "/id/"+controlForwardRouteID, nil)
		return err
	case "caddyfile":
		return m.editControlCaddyfile(ctx, f.Hostname, "")
	}
	return nil
}

// editControlCaddyfile sets (or, with an empty body, removes) the marked
// site block of host through the root helper.
func (m *Manager) editControlCaddyfile(ctx context.Context, host, body string) error {
	raw, err := os.ReadFile(controlCaddyfileAt)
	if err != nil {
		return err
	}
	next := controlCaddyfileSite(string(raw), host, body)
	if next == string(raw) {
		return nil
	}
	if err := os.MkdirAll(m.controlDir(), 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(m.controlDir(), "Caddyfile.next"), []byte(next), 0o600); err != nil {
		return err
	}
	return m.controlHelper(ctx, controlActionCaddy)
}

// controlForwardTick runs in the control route loop: an expired forward is
// removed, a live admin-API one is put back if a Caddy reload dropped it.
func (m *Manager) controlForwardTick(ctx context.Context) {
	f := m.controlForward()
	if f == nil {
		return
	}
	if !controlForwardNow().Before(f.Expires) {
		if err := m.removeControlForward(ctx, f); err != nil {
			log.Printf("control forward: removing the expired forward of %s failed: %v", f.Hostname, err)
			return
		}
		if err := m.saveControlForward(nil); err != nil {
			log.Printf("control forward: %v", err)
			return
		}
		log.Printf("control forward: %s is no longer forwarded to %s (expired %s)", f.Hostname, f.Address, f.Expires.Format(time.RFC3339))
		return
	}
	if f.Route == "caddy" {
		if err := m.applyControlForward(ctx, f); err != nil {
			log.Printf("control forward: keeping the forward of %s: %v", f.Hostname, err)
		}
	}
}

// activeControlForward is the live forward of host, if any.
func (m *Manager) activeControlForward(host string) *ControlForward {
	f := m.controlForward()
	if f == nil || !strings.EqualFold(f.Hostname, host) || !controlForwardNow().Before(f.Expires) {
		return nil
	}
	return f
}

// serveControlForward forwards a retired panel's hostname through this
// agent's proxy, for hosts whose web server forwards every hostname here.
func (m *Manager) serveControlForward(w http.ResponseWriter, r *http.Request, host string) bool {
	f := m.activeControlForward(host)
	if f == nil || f.Route != "agent" {
		return false
	}
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "https", Host: controlForwardDial(f.Address)})
	proxy.Transport = &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{ServerName: f.Hostname, MinVersion: tls.VersionTLS12},
		DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, ResponseHeaderTimeout: time.Minute}
	director := proxy.Director
	proxy.Director = func(req *http.Request) { director(req); req.Host = r.Host }
	proxy.ServeHTTP(w, r)
	return true
}

// controlForwardNote is the retirement note for a forward.
func controlForwardNote(f *ControlForward) string {
	return fmt.Sprintf("Visitors whose DNS still returns this server are forwarded to %s (HTTPS for %s) until %s; this server's agent then removes the forward.", f.Address, f.Hostname, f.Expires.Format("2006-01-02 15:04 MST"))
}
