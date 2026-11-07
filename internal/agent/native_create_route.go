package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agim/lidza/pkg/gateway"
)

// Web routes of created applications. The host's web server terminates TLS
// (on-demand certificates approved through /tls/allow) and forwards every
// request to the agent's proxy; the proxy serves a created application's
// hosts itself: a static site from <root>/current/<output>, any other
// application by forwarding to its web process on its loopback port. No
// web server configuration is written and the web server needs no access
// to application files.

var nativeCreatedTransport = &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	ResponseHeaderTimeout: 60 * time.Second, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 100}

// nativeCreatedHost returns the created application that owns host.
func (m *Manager) nativeCreatedHost(host string) (NativeApplication, NativeReleasePolicy, bool) {
	host = strings.ToLower(host)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, policy := range m.data.NativeCreated {
		a, found := m.data.NativeApps[id]
		if found && policy.Enabled && (App{Domain: a.Domain, AdditionalDomains: a.AdditionalDomains}).HasDomain(host) {
			return a, policy, true
		}
	}
	return NativeApplication{}, NativeReleasePolicy{}, false
}

// nativeCreatedDomain reports whether host belongs to a created application;
// the domain proof and certificate approval accept such hosts.
func (m *Manager) nativeCreatedDomain(host string) bool {
	_, _, found := m.nativeCreatedHost(host)
	return found
}

// nativeCreatedDomainAllowed approves a certificate for a created
// application's host once its DNS reaches this agent, as for container
// applications. matched is false when the host is not a created one.
func (m *Manager) nativeCreatedDomainAllowed(ctx context.Context, domain string) (allowed, matched bool) {
	domain = strings.ToLower(domain)
	if m.hostReserved(domain) || !m.nativeCreatedDomain(domain) {
		return false, false
	}
	if m.cfg.TLSListen == "" {
		return true, true
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return checkDomainRoute(ctx, domain, m.domainProof(domain), net.DefaultResolver.LookupIP, "80") == nil, true
}

// serveNativeCreated serves a request for a created application's host and
// reports whether host was one.
func (m *Manager) serveNativeCreated(w http.ResponseWriter, r *http.Request, host string) bool {
	a, policy, found := m.nativeCreatedHost(host)
	if !found {
		return false
	}
	if m.maintenancePage(w, r, a.Domain) {
		return true
	}
	if policy.Runtime == "static" {
		m.serveNativeCreatedStatic(w, r, policy)
		return true
	}
	if policy.Port == "" {
		nativeUnavailablePage(w, http.StatusServiceUnavailable)
		return true
	}
	r = r.Clone(r.Context())
	r.Header.Del(gateway.Header)
	for _, name := range gateway.EdgeHeaderNames() {
		r.Header.Del(name)
	}
	// Only the local TLS terminator supplies trusted forwarding headers.
	remote, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(remote); ip == nil || !ip.IsLoopback() {
		r.Header.Del("X-Forwarded-For")
		r.Header.Del("X-Forwarded-Host")
		r.Header.Del("X-Forwarded-Proto")
	}
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "127.0.0.1:" + policy.Port})
	proxy.Transport = nativeCreatedTransport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		nativeUnavailablePage(w, http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
	return true
}

// serveNativeCreatedStatic serves files of the current release's output
// directory. The directory is opened as a root, so no link inside the
// release can reach files outside it.
func (m *Manager) serveNativeCreatedStatic(w http.ResponseWriter, r *http.Request, policy NativeReleasePolicy) {
	current, err := filepath.EvalSymlinks(filepath.Join(policy.Root, "current"))
	releases := filepath.Join(policy.Root, "releases") + string(filepath.Separator)
	if err != nil || !strings.HasPrefix(current, releases) || policy.Output == "" {
		http.Error(w, "application unavailable", http.StatusServiceUnavailable)
		return
	}
	root, err := os.OpenRoot(filepath.Join(current, filepath.FromSlash(policy.Output)))
	if err != nil {
		http.Error(w, "application unavailable", http.StatusServiceUnavailable)
		return
	}
	defer root.Close()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.FileServerFS(root.FS()).ServeHTTP(w, r)
}
