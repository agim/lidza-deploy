package agent

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

const customerHostCacheLimit = 10000
const customerHostApproveTTL = 30 * time.Second
const customerHostRefuseTTL = 15 * time.Second

var customerHostClient = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, ResponseHeaderTimeout: 5 * time.Second, MaxIdleConns: 32, IdleConnTimeout: 30 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type hostVerdict struct {
	generation string
	appID      string
	expires    time.Time
}

func operatorPath(p string) bool {
	switch path.Clean(p) {
	case "/healthz", "/readyz", "/metrics", "/tls-allow":
		return true
	}
	return strings.HasPrefix(path.Clean(p), "/tls-allow/")
}
func customerHostname(host string) (string, bool) {
	host = strings.ToLower(host)
	if !domainPattern.MatchString(host) || !strings.Contains(host, ".") || net.ParseIP(host) != nil || strings.Contains(host, "..") {
		return "", false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", false
		}
	}
	return host, true
}

// The generation includes all registered names, release ports and opt-ins. An
// assignment, removal, restart, reload, stop or opt-out invalidates old decisions.
func (m *Manager) hostSnapshot() ([]App, string) {
	m.mu.Lock()
	apps := make([]App, 0, len(m.data.Apps))
	for _, a := range m.data.Apps {
		if a.Current != nil {
			r := *a.Current
			a.Current = &r
		}
		apps = append(apps, a)
	}
	m.mu.Unlock()
	slices.SortFunc(apps, func(a, b App) int { return cmp.Compare(a.ID, b.ID) })
	var b strings.Builder
	for i := range apps {
		a := &apps[i]
		if a.Current != nil {
			r := *a.Current
			a.Current = &r
		}
		fmt.Fprintf(&b, "%s|%s|%t|%t|%t|%t|", a.ID, a.Domain, a.CustomerHosts, a.Stopped, a.Retiring, a.Preview)
		if a.Current != nil {
			fmt.Fprintf(&b, "%s|%s", a.Current.ID, a.Current.Port)
		}
		b.WriteByte('\n')
	}
	return apps, fmt.Sprintf("%x", sha256.Sum256([]byte(b.String())))
}
func registeredHost(apps []App, host string) (App, bool) {
	for _, a := range apps {
		if a.Domain == host {
			return a, true
		}
	}
	return App{}, false
}
func findHostOwner(apps []App, id string) (App, bool) {
	for _, a := range apps {
		if a.ID == id && a.CustomerHosts && !a.Stopped && !a.Retiring && !a.Preview && a.Current != nil {
			return a, true
		}
	}
	return App{}, false
}
func (m *Manager) hostApplication(ctx context.Context, host string) (App, bool) {
	host, valid := customerHostname(host)
	if !valid {
		return App{}, false
	}
	apps, generation := m.hostSnapshot()
	// Registered FQDNs remain reserved even when stopped, retired or not deployed.
	if a, ok := registeredHost(apps, host); ok {
		return a, !a.Retiring
	}
	m.hostMu.Lock()
	v, hit := m.hostCache[host]
	m.hostMu.Unlock()
	if hit && v.generation == generation && time.Now().Before(v.expires) {
		return findHostOwner(apps, v.appID)
	}
	result := m.hostFlights.DoChan(generation+":"+host, func() (any, error) {
		// Do not let a disconnected first reader cancel a concurrent Caddy check.
		probeCtx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		select {
		case m.hostSlots <- struct{}{}:
			defer func() { <-m.hostSlots }()
		default:
			return "", nil
		}
		owner := ""
		for _, a := range apps {
			if !a.CustomerHosts || a.Stopped || a.Retiring || a.Preview || a.Current == nil {
				continue
			}
			if probeCtx.Err() != nil {
				return "", nil
			}
			if asksCustomerHost(probeCtx, a, host) {
				if owner != "" {
					owner = ""
					break
				}
				owner = a.ID
			}
		}
		// A timed-out unqueried app might also own the host. Fail closed.
		if probeCtx.Err() != nil {
			owner = ""
		}
		ttl := customerHostRefuseTTL
		if owner != "" {
			ttl = customerHostApproveTTL
		}
		m.hostMu.Lock()
		if len(m.hostCache) >= customerHostCacheLimit {
			now := time.Now()
			for key, entry := range m.hostCache {
				if !now.Before(entry.expires) {
					delete(m.hostCache, key)
				}
			}
			if len(m.hostCache) >= customerHostCacheLimit {
				for key := range m.hostCache {
					delete(m.hostCache, key)
					break
				}
			}
		}
		m.hostCache[host] = hostVerdict{generation: generation, appID: owner, expires: time.Now().Add(ttl)}
		m.hostMu.Unlock()
		return owner, nil
	})
	select {
	case <-ctx.Done():
		return App{}, false
	case r := <-result:
		current, newGeneration := m.hostSnapshot()
		if newGeneration != generation {
			return App{}, false
		}
		id, _ := r.Val.(string)
		return findHostOwner(current, id)
	}
}
func asksCustomerHost(ctx context.Context, a App, host string) bool {
	port, err := strconv.Atoi(a.Current.Port)
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/tls-allow?domain="+url.QueryEscape(host), nil)
	if err != nil {
		return false
	}
	req.Host = a.Domain
	res, err := customerHostClient.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	// The framework returns an empty 200. A SPA fallback returning an HTML 200
	// must never authorize arbitrary certificates when the hook is missing.
	body, err := io.ReadAll(io.LimitReader(res.Body, 1))
	return err == nil && res.StatusCode == http.StatusOK && len(body) == 0
}
