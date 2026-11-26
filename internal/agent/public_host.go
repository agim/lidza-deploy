package agent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A server behind a router has only a private address and often cannot
// connect to its own public one (the router does not loop the connection
// back), so the probe that proves a hostname leads here never succeeds from
// the server itself, although it does from everywhere else.
//
// The agent therefore remembers the name the control panel reaches it by:
// the Host of an authenticated API request. Such a request proves the name
// leads to this agent, through the same public address and ports as the
// applications' hostnames. An application hostname that resolves to an
// address of that name leads here too, whatever a connection from inside
// shows. The certificate authority still validates every certificate from
// outside; this only decides whether one is requested.

const publicHostFile = "public-host"

// publicHostLookup resolves the agent's public name; tests replace it.
var publicHostLookup = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// publicHostName returns host as a public DNS name, or "" for an address,
// a loopback or a single-label name.
func publicHostName(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 || net.ParseIP(host) != nil || !strings.Contains(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.ContainsAny(host, "/\\ \t\n[]") {
		return ""
	}
	return host
}

// notePublicHost records the name an authenticated API request arrived by.
func (m *Manager) notePublicHost(host string) {
	host = publicHostName(host)
	if host == "" {
		return
	}
	if current := m.publicHost.Load(); current != nil && *current == host {
		return
	}
	m.publicHost.Store(&host)
	if m.cfg.DataDir != "" {
		_ = os.WriteFile(filepath.Join(m.cfg.DataDir, publicHostFile), []byte(host+"\n"), 0o600)
	}
}

// publicHostKnown is the recorded name, read back after a restart.
func (m *Manager) publicHostKnown() string {
	if current := m.publicHost.Load(); current != nil {
		return *current
	}
	if m.cfg.DataDir == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(m.cfg.DataDir, publicHostFile))
	if err != nil {
		return ""
	}
	host := publicHostName(string(raw))
	if host != "" {
		m.publicHost.CompareAndSwap(nil, &host)
	}
	return host
}

// ownPublicAddress reports whether ip is an address of the name the control
// panel reaches this agent by.
func (m *Manager) ownPublicAddress(ctx context.Context, ip net.IP) bool {
	host := m.publicHostKnown()
	if host == "" || ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	addresses, err := publicHostLookup(ctx, host)
	if err != nil {
		return false
	}
	for _, own := range addresses {
		if own.Equal(ip) {
			return true
		}
	}
	return false
}
