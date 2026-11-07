package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// Redirect domains are hostnames an application answers with a permanent
// (or temporary) redirect instead of its content: retired sites, old brand
// names, a bare domain to www. They belong to an application of any runtime
// (container, native, static, stack) and are stored next to it, keyed by its
// id, in diskState.Redirects. They never reach the application's process,
// so they keep working while it is stopped, failing or in maintenance.
//
// Every ingress layout serves them:
//
//   - agent: the web server forwards every hostname to the agent's proxy,
//     which answers redirect hosts itself (serveRedirect) and approves
//     their on-demand certificates through /tls/allow once their DNS
//     reaches this host, exactly like application hostnames.
//   - caddy: an admin-API Caddy owns the web ports. The agent publishes one
//     route per application, @id lidza-redirect-<id>, matching all of its
//     redirect hosts; a subroute per distinct rule answers with
//     static_response (status plus Location). The hosts are added to the
//     TLS automation subjects so Caddy obtains their certificates, and are
//     removed again with the route.
//   - caddyfile: a Caddy without admin API whose Caddyfile does not forward
//     to the agent. The agent never writes application hostnames into a
//     Caddyfile, so redirects (like application hostnames) are not served
//     there; the view reports it so the panel can say so.

const (
	redirectRoutePrefix  = "lidza-redirect-"
	redirectStatusPrefix = "redirect/"
	maxRedirectDomains   = 50
)

// RedirectDomain is one hostname that redirects. Target defaults to the
// application's primary hostname; Status to 301; KeepPath (default true)
// carries the request's path and query to the target.
type RedirectDomain struct {
	Host     string `json:"host"`
	Target   string `json:"target,omitempty"`
	Status   int    `json:"status,omitempty"`
	KeepPath bool   `json:"keep_path"`
}

// UnmarshalJSON keeps the path by default when keep_path is omitted, and
// rejects unknown fields like the API's other request bodies.
func (d *RedirectDomain) UnmarshalJSON(raw []byte) error {
	type plain RedirectDomain
	value := plain{KeepPath: true}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*d = RedirectDomain(value)
	return nil
}

// RedirectStatuses are the redirect codes a redirect domain may answer with.
var RedirectStatuses = []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect}

// NormalizeRedirectDomains validates redirect domains of an application
// whose own hostnames are own (primary first) and returns them normalized:
// lowercase hostnames, the default status filled in. It checks only the
// list itself; conflicts with other applications are the caller's.
func NormalizeRedirectDomains(own []string, list []RedirectDomain) ([]RedirectDomain, error) {
	if len(list) > maxRedirectDomains {
		return nil, fmt.Errorf("at most %d redirect domains per application", maxRedirectDomains)
	}
	out := make([]RedirectDomain, 0, len(list))
	seen := map[string]bool{}
	for _, d := range list {
		d.Host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d.Host), "."))
		d.Target = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d.Target), "."))
		if err := validateHostname(d.Host); err != nil {
			return nil, fmt.Errorf("redirect domain %q: %w", d.Host, err)
		}
		if seen[d.Host] {
			return nil, errors.New("duplicate redirect domain: " + d.Host)
		}
		seen[d.Host] = true
		if slices.Contains(own, d.Host) {
			return nil, fmt.Errorf("%s is a hostname of this application; remove it there before redirecting it", d.Host)
		}
		if d.Target != "" {
			if err := validateHostname(d.Target); err != nil {
				return nil, fmt.Errorf("redirect target %q: %w", d.Target, err)
			}
		}
		if d.Target == d.Host {
			return nil, fmt.Errorf("%s cannot redirect to itself", d.Host)
		}
		if d.Status == 0 {
			d.Status = http.StatusMovedPermanently
		}
		if !slices.Contains(RedirectStatuses, d.Status) {
			return nil, fmt.Errorf("redirect status %d is not one of 301, 302, 307 or 308", d.Status)
		}
		out = append(out, d)
	}
	for _, d := range out {
		// A target that is another redirect host of the list would chain.
		if d.Target != "" && seen[d.Target] {
			return nil, fmt.Errorf("%s redirects to %s, which redirects too; point it at the final hostname", d.Host, d.Target)
		}
	}
	return out, nil
}

// RedirectHosts lists the hostnames of redirect domains.
func RedirectHosts(list []RedirectDomain) []string {
	out := make([]string, 0, len(list))
	for _, d := range list {
		out = append(out, d.Host)
	}
	return out
}

// RedirectTarget is the hostname a redirect points to.
func (d RedirectDomain) RedirectTarget(primary string) string {
	if d.Target != "" {
		return d.Target
	}
	return primary
}

// location is the Location of a redirect: uri is the request URI (path and
// query), or Caddy's placeholder for it.
func (d RedirectDomain) location(primary, uri string) string {
	if !d.KeepPath || uri == "" {
		uri = "/"
	}
	return "https://" + d.RedirectTarget(primary) + uri
}

// ---- state ------------------------------------------------------------------

// redirectOwnersLocked returns each redirect host's application id.
func (m *Manager) redirectOwnersLocked() map[string]string {
	owners := map[string]string{}
	for id, list := range m.data.Redirects {
		for _, d := range list {
			owners[d.Host] = id
		}
	}
	return owners
}

// appHostnamesLocked returns the primary hostname and every hostname of the
// application id, container or native; ok is false when there is none.
func (m *Manager) appHostnamesLocked(id string) (hosts []string, ok bool) {
	if a, found := m.data.Apps[id]; found && !a.Retiring {
		return a.Hostnames(), true
	}
	if a, found := m.data.NativeApps[id]; found && a.Domain != "" {
		return append([]string{a.Domain}, a.AdditionalDomains...), true
	}
	return nil, false
}

// checkRedirectHostsLocked rejects hostnames another application (or this
// one, except) redirects; except names the application whose redirects
// are being replaced.
func (m *Manager) checkRedirectHostsLocked(hosts []string, except string) error {
	owners := m.redirectOwnersLocked()
	for _, host := range hosts {
		if owner, found := owners[host]; found && owner != except {
			return fmt.Errorf("%s is a redirect domain of %s", host, owner)
		}
	}
	return nil
}

// redirectFor finds the redirect of host and the primary hostname of its
// application.
func (m *Manager) redirectFor(host string) (RedirectDomain, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, list := range m.data.Redirects {
		for _, d := range list {
			if d.Host != host {
				continue
			}
			hosts, ok := m.appHostnamesLocked(id)
			if !ok {
				return RedirectDomain{}, "", false
			}
			return d, hosts[0], true
		}
	}
	return RedirectDomain{}, "", false
}

func (m *Manager) redirectHost(host string) bool {
	_, _, found := m.redirectFor(strings.ToLower(host))
	return found
}

// Redirects returns the application's redirect domains.
func (m *Manager) Redirects(id string) []RedirectDomain {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.data.Redirects[id])
}

// SetRedirects replaces the application's redirect domains and publishes
// them to the host's ingress. The returned note says how they are served.
func (m *Manager) SetRedirects(ctx context.Context, id string, list []RedirectDomain) (string, error) {
	control := m.controlState()
	m.mu.Lock()
	own, ok := m.appHostnamesLocked(id)
	if !ok {
		m.mu.Unlock()
		return "", errors.New("unknown application")
	}
	normalized, err := NormalizeRedirectDomains(own, list)
	if err == nil {
		err = m.redirectConflictsLocked(id, normalized, control)
	}
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	previous, had := m.data.Redirects[id]
	if m.data.Redirects == nil {
		m.data.Redirects = map[string][]RedirectDomain{}
	}
	if len(normalized) == 0 {
		delete(m.data.Redirects, id)
	} else {
		m.data.Redirects[id] = normalized
	}
	if err := m.save(); err != nil {
		if had {
			m.data.Redirects[id] = previous
		} else {
			delete(m.data.Redirects, id)
		}
		m.mu.Unlock()
		return "", errors.New("could not save the redirect domains")
	}
	m.mu.Unlock()
	m.wakeDomains()
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return m.syncRedirectIngress(bounded, id)
}

// redirectConflictsLocked rejects redirect hosts that another application
// serves or redirects, reserved hostnames and the control panel's.
func (m *Manager) redirectConflictsLocked(id string, list []RedirectDomain, control *ControlHostState) error {
	hosts := RedirectHosts(list)
	for _, host := range hosts {
		if m.hostReserved(host) {
			return errors.New(host + " is reserved for an independently managed application")
		}
		if control != nil && strings.EqualFold(control.Hostname, host) {
			return errors.New(host + " is the control panel's hostname")
		}
		for otherID, other := range m.data.Apps {
			if otherID != id && other.HasDomain(host) {
				return fmt.Errorf("%s is a hostname of %s", host, otherID)
			}
		}
		for otherID, other := range m.data.NativeApps {
			if otherID != id && (App{Domain: other.Domain, AdditionalDomains: other.AdditionalDomains}).HasDomain(host) {
				return fmt.Errorf("%s is a hostname of %s", host, otherID)
			}
		}
	}
	return m.checkRedirectHostsLocked(hosts, id)
}

// dropRedirectsLocked forgets a removed application's redirect domains;
// the caller saves and then withdraws them from the ingress.
func (m *Manager) dropRedirectsLocked(id string) bool {
	if _, found := m.data.Redirects[id]; !found {
		return false
	}
	delete(m.data.Redirects, id)
	return true
}

// withdrawRedirects removes a removed application's redirects from the
// ingress; failures are logged and repaired by the reconcile loop.
func (m *Manager) withdrawRedirects(ctx context.Context, id string) {
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := m.syncRedirectIngress(bounded, id); err != nil {
		log.Printf("redirects %s: %v", id, err)
	}
}

// ---- proxy ------------------------------------------------------------------

// serveRedirect answers a request for a redirect host and reports whether
// host was one. It never depends on the application's process.
func (m *Manager) serveRedirect(w http.ResponseWriter, r *http.Request, host string) bool {
	d, primary, found := m.redirectFor(host)
	if !found {
		return false
	}
	w.Header().Set("Location", d.location(primary, r.URL.RequestURI()))
	if d.Status == http.StatusFound || d.Status == http.StatusTemporaryRedirect {
		w.Header().Set("Cache-Control", "no-store") // a temporary redirect may change
	}
	w.WriteHeader(d.Status)
	return true
}

// ---- admin-API Caddy ----------------------------------------------------------

func redirectRouteID(id string) string { return redirectRoutePrefix + id }

// renderRedirectRoute is the Caddy route of an application's redirect
// domains: one route matching every redirect host, with a subroute per
// distinct (target, status, keep path) rule answering with static_response.
func renderRedirectRoute(id, primary string, list []RedirectDomain) ([]byte, error) {
	if !idPattern.MatchString(id) || len(list) == 0 {
		return nil, errors.New("redirect route needs an application and redirect domains")
	}
	type rule struct {
		location string
		status   int
	}
	order := []rule{}
	hosts := map[rule][]string{}
	for _, d := range list {
		key := rule{location: d.location(primary, "{http.request.uri}"), status: d.Status}
		if _, found := hosts[key]; !found {
			order = append(order, key)
		}
		hosts[key] = append(hosts[key], d.Host)
	}
	routes := make([]any, 0, len(order))
	for _, key := range order {
		routes = append(routes, map[string]any{
			"match": []any{map[string]any{"host": hosts[key]}},
			"handle": []any{map[string]any{
				"handler":     "static_response",
				"status_code": key.status,
				"headers":     map[string][]string{"Location": {key.location}},
			}},
		})
	}
	route := map[string]any{
		"@id":      redirectRouteID(id),
		"match":    []any{map[string]any{"host": RedirectHosts(list)}},
		"handle":   []any{map[string]any{"handler": "subroute", "routes": routes}},
		"terminal": true,
	}
	return json.Marshal(route)
}

// putRoute adds or replaces the route with the given @id; an identical
// route is left alone. New routes go first, before broader routes.
func (c nativeCaddy) putRoute(ctx context.Context, routeID string, route []byte) error {
	var current json.RawMessage
	if found, err := c.get(ctx, "/id/"+routeID, &current); err != nil {
		return err
	} else if found && jsonEqual(current, route) {
		return nil
	}
	status, err := c.call(ctx, http.MethodPatch, "/id/"+routeID, route)
	if err != nil || status != http.StatusNotFound {
		return err
	}
	_, err = c.call(ctx, http.MethodPut, "/config/apps/http/servers/"+c.cfg.Server+"/routes/0", route)
	return err
}

// routeHostsByID returns the host matchers of the route with the @id.
func (c nativeCaddy) routeHostsByID(ctx context.Context, routeID string) ([]string, bool, error) {
	var route caddyRoute
	found, err := c.get(ctx, "/id/"+routeID, &route)
	if err != nil || !found {
		return nil, false, err
	}
	return caddyRouteHosts(route), true, nil
}

// publishRedirects publishes (or, for an empty list, withdraws) the
// application's redirect route and keeps the TLS subjects in step.
func (c nativeCaddy) publishRedirects(ctx context.Context, id, primary string, list []RedirectDomain) (nativeCaddyTLS, error) {
	out := nativeCaddyTLS{Policy: -1}
	previous, published, err := c.routeHostsByID(ctx, redirectRouteID(id))
	if err != nil {
		return out, err
	}
	hosts := RedirectHosts(list)
	if len(list) > 0 {
		route, err := renderRedirectRoute(id, primary, list)
		if err != nil {
			return out, err
		}
		if err := c.putRoute(ctx, redirectRouteID(id), route); err != nil {
			return out, err
		}
		if out, err = c.ensureTLSSubjects(ctx, hosts); err != nil {
			return out, fmt.Errorf("tls automation: %w", err)
		}
	} else if published {
		if _, err := c.call(ctx, http.MethodDelete, "/id/"+redirectRouteID(id), nil); err != nil {
			return out, err
		}
	}
	var release []string
	for _, host := range previous {
		host = strings.ToLower(host)
		if !slices.Contains(hosts, host) && !slices.Contains(release, host) {
			release = append(release, host)
		}
	}
	if len(release) == 0 {
		return out, nil
	}
	served, err := c.servedHosts(ctx, "")
	if err != nil {
		return out, err
	}
	release = slices.DeleteFunc(release, func(host string) bool { return served[host] })
	return out, c.removeTLSSubjects(ctx, release)
}

// redirectRouteIDs lists the applications with a redirect route published
// in the server.
func (c nativeCaddy) redirectRouteIDs(ctx context.Context) ([]string, error) {
	var routes []json.RawMessage
	if _, err := c.get(ctx, "/config/apps/http/servers/"+c.cfg.Server+"/routes", &routes); err != nil {
		return nil, err
	}
	ids := []string{}
	for _, raw := range routes {
		var route caddyRoute
		if json.Unmarshal(raw, &route) == nil && strings.HasPrefix(route.ID, redirectRoutePrefix) {
			ids = append(ids, strings.TrimPrefix(route.ID, redirectRoutePrefix))
		}
	}
	return ids, nil
}

// syncRedirectIngress publishes the application's current redirect domains
// to an admin-API Caddy; elsewhere the agent's proxy serves them and the
// note says so.
func (m *Manager) syncRedirectIngress(ctx context.Context, id string) (string, error) {
	m.mu.Lock()
	list := slices.Clone(m.data.Redirects[id])
	hosts, ok := m.appHostnamesLocked(id)
	m.mu.Unlock()
	if !ok {
		list = nil
	}
	caddy, admin := m.nativeCaddy(ctx)
	if !admin {
		mode, _ := m.redirectIngress(ctx)
		if mode == "caddyfile" && len(list) > 0 {
			return "Redirects: this host's Caddy is configured by a Caddyfile that does not forward to the agent; enable Caddy's admin API so redirect domains are published.", nil
		}
		if len(list) == 0 {
			return "", nil
		}
		return "Redirects: the agent's proxy answers the redirect domains; their certificates are issued on demand once DNS points here.", nil
	}
	primary := ""
	if len(hosts) > 0 {
		primary = hosts[0]
	}
	tls, err := caddy.publishRedirects(ctx, id, primary, list)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "Redirects: the redirect route was removed from Caddy.", nil
	}
	return "Redirects: published to Caddy. " + tls.Line(), nil
}

// redirectIngress names how this host serves redirect domains: caddy (the
// admin API), agent (the agent's proxy) or caddyfile (not served).
func (m *Manager) redirectIngress(ctx context.Context) (mode, detail string) {
	status := m.hostIngress(ctx, false)
	switch {
	case status.Mode == HostIngressCaddy:
		return "caddy", "Caddy answers the redirect domains through a route published with its admin API and obtains their certificates."
	case !status.AdminAPI && len(status.WebPorts) > 0 && m.caddyfileWithoutAgent():
		return "caddyfile", "This host's Caddy is configured by a Caddyfile that does not forward to the agent, and the agent does not edit Caddyfiles for applications. Enable Caddy's admin API (127.0.0.1:2019) so redirect domains are published."
	}
	return "agent", "The agent's proxy answers the redirect domains; certificates are issued on demand once their DNS points to this server."
}

// reconcileRedirects republishes every application's redirects to an
// admin-API Caddy (a Caddy reload from an older configuration loses them)
// and withdraws routes of applications that have none any more.
func (m *Manager) reconcileRedirects(ctx context.Context) {
	caddy, ok := m.nativeCaddy(ctx)
	if !ok {
		return
	}
	m.mu.Lock()
	ids := []string{}
	for id := range m.data.Redirects {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	if published, err := caddy.redirectRouteIDs(ctx); err == nil {
		for _, id := range published {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		if _, err := m.syncRedirectIngress(ctx, id); err != nil {
			log.Printf("redirects %s: %v", id, err)
		}
	}
}

func (m *Manager) redirectLoop() {
	defer m.wg.Done()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-timer.C:
		}
		ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
		m.reconcileRedirects(ctx)
		cancel()
		timer.Reset(time.Minute)
	}
}

// ---- certificate status -------------------------------------------------------

func redirectStatusKey(id, host string) string { return redirectStatusPrefix + id + "/" + host }

// redirectChecks are the redirect hosts the domain loop verifies, as
// pseudo applications with their own status keys.
func (m *Manager) redirectChecks() []App {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []App{}
	for id, list := range m.data.Redirects {
		if _, ok := m.appHostnamesLocked(id); !ok {
			continue
		}
		for _, d := range list {
			out = append(out, App{ID: id, Domain: d.Host, domainCheckKey: redirectStatusKey(id, d.Host), Current: &Release{}})
		}
	}
	return out
}

// redirectStatusesLocked returns the DNS and certificate status of each
// redirect host when this agent terminates TLS itself.
func (m *Manager) redirectStatusesLocked(id string) []DomainStatus {
	if m.cfg.TLSListen == "" {
		return nil
	}
	out := []DomainStatus{}
	for _, d := range m.data.Redirects[id] {
		status, ok := m.domains[redirectStatusKey(id, d.Host)]
		if !ok || status.Domain != d.Host {
			status = DomainStatus{Domain: d.Host, State: "waiting_dns", RecheckSeconds: int(domainPendingInterval / time.Second)}
		}
		out = append(out, status)
	}
	return out
}

// ---- API ----------------------------------------------------------------------

// RedirectView is an application's redirect domains with how this host
// serves them and the addresses their DNS records point to.
type RedirectView struct {
	Redirects []RedirectDomain `json:"redirects"`
	// Hostnames are the application's own hostnames (primary first), the
	// targets offered by default.
	Hostnames []string       `json:"hostnames"`
	Statuses  []DomainStatus `json:"statuses,omitempty"`
	Ingress   string         `json:"ingress"`
	Detail    string         `json:"detail"`
	IPv4      string         `json:"ipv4,omitempty"`
	IPv6      string         `json:"ipv6,omitempty"`
	Note      string         `json:"note,omitempty"`
}

// RedirectRequest replaces an application's redirect domains.
type RedirectRequest struct {
	Redirects []RedirectDomain `json:"redirects"`
}

func (m *Manager) redirectView(ctx context.Context, id string) (RedirectView, error) {
	m.mu.Lock()
	hosts, ok := m.appHostnamesLocked(id)
	view := RedirectView{Redirects: slices.Clone(m.data.Redirects[id]), Hostnames: hosts, Statuses: m.redirectStatusesLocked(id)}
	m.mu.Unlock()
	if !ok {
		return view, errors.New("unknown application")
	}
	if view.Redirects == nil {
		view.Redirects = []RedirectDomain{}
	}
	view.Ingress, view.Detail = m.redirectIngress(ctx)
	addresses := moveDetectAddresses()
	view.IPv4, view.IPv6 = addresses.IPv4, addresses.IPv6
	return view, nil
}

func (m *Manager) redirectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/apps/{id}/redirects", func(w http.ResponseWriter, r *http.Request) {
		view, err := m.redirectView(r.Context(), r.PathValue("id"))
		if err != nil {
			Fail(w, 404, err)
			return
		}
		JSON(w, 200, view)
	})
	mux.HandleFunc("PUT /v1/apps/{id}/redirects", func(w http.ResponseWriter, r *http.Request) {
		var in RedirectRequest
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		note, err := m.SetRedirects(r.Context(), r.PathValue("id"), in.Redirects)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		view, err := m.redirectView(r.Context(), r.PathValue("id"))
		if err != nil {
			Fail(w, 404, err)
			return
		}
		view.Note = note
		JSON(w, 200, view)
	})
}

// caddyfileWithoutAgent reports a Caddyfile that does not forward to the
// agent's proxy (control_host.go reads the same file).
func (m *Manager) caddyfileWithoutAgent() bool {
	raw, err := os.ReadFile(controlCaddyfileAt)
	return err == nil && !bytes.Contains(raw, []byte(m.cfg.ProxyListen))
}
