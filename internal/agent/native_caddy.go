package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Hosts whose web ports belong to a Caddy server configured through its
// admin API (rather than the agent's own TLS listener) publish created
// applications with one route each. The route sends the application's
// hostnames to the agent's proxy, which serves process applications from
// their loopback port and static sites from their release, so Caddy needs no
// access to application files. Routes carry an @id so they are replaced and
// removed exactly, and nothing else in the Caddy configuration is touched.
//
// Each published application also gets an error route serving the
// agent's neutral error and maintenance pages (error_pages.go).
//
// Such a Caddy issues certificates only for the subjects of its TLS
// automation policies, so publishing also adds the hostnames to the policy
// that holds the ACME issuer (and withdrawing removes them again, unless
// another published application still serves them). A Caddy that issues
// certificates on demand needs no subjects. The admin API is the detected
// or configured one (host_ingress.go).

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

const nativeCaddyRoutePrefix = "lidza-app-"

func nativeCaddyRouteID(id string) string { return nativeCaddyRoutePrefix + id }

// renderNativeCaddyRoute is the route for one created application.
func renderNativeCaddyRoute(id string, hosts []string, upstream string) ([]byte, error) {
	if upstream == "" {
		return nil, errors.New("caddy route needs an application, hostnames and an upstream")
	}
	return renderCaddyRoute(id, hosts, map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": upstream}}})
}

// renderCaddyRoute is an application's route with the given reverse_proxy
// handler: the agent's proxy, or the protected gateway (gateway_auto.go).
func renderCaddyRoute(id string, hosts []string, proxy map[string]any) ([]byte, error) {
	if !idPattern.MatchString(id) || len(hosts) == 0 || proxy == nil {
		return nil, errors.New("caddy route needs an application, hostnames and an upstream")
	}
	route := map[string]any{
		"@id":   nativeCaddyRouteID(id),
		"match": []any{map[string]any{"host": hosts}},
		"handle": []any{map[string]any{
			"handler": "subroute",
			"routes": []any{map[string]any{"handle": []any{
				map[string]any{"handler": "encode", "encodings": map[string]any{"zstd": map[string]any{}, "gzip": map[string]any{}}, "prefer": []string{"zstd", "gzip"}},
				proxy,
			}}},
		}},
		"terminal": true,
	}
	return json.Marshal(route)
}

type nativeCaddy struct {
	cfg    NativeCaddyConfig
	client *http.Client
	// proxy replaces the reverse_proxy handler to the agent's proxy, for
	// protected applications behind the host gateway.
	proxy map[string]any
}

func (c nativeCaddy) route(id string, hosts []string) ([]byte, error) {
	if c.proxy != nil {
		return renderCaddyRoute(id, hosts, c.proxy)
	}
	return renderNativeCaddyRoute(id, hosts, c.cfg.Upstream)
}

// nativeCaddy resolves the Caddy this host publishes to: the configuration
// file's native.caddy, else the panel's override, else the detected
// admin-API Caddy. ok is false when the agent proxy is the ingress.
func (m *Manager) nativeCaddy(ctx context.Context) (nativeCaddy, bool) {
	cfg, ok := m.hostIngressCaddy(ctx)
	if !ok {
		return nativeCaddy{}, false
	}
	if cfg.Server == "" {
		cfg.Server = "srv0"
	}
	if cfg.Upstream == "" {
		cfg.Upstream = m.cfg.ProxyListen
	}
	return nativeCaddy{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}}, true
}

// call sends a write to the admin API. A 404 is reported through the status,
// not as an error, so callers can create what is missing.
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

// get reads a configuration path. found is false for a missing path or a
// JSON null.
func (c nativeCaddy) get(ctx context.Context, path string, out any) (found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.cfg.Admin, "/")+path, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("caddy admin unavailable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 300:
		return false, fmt.Errorf("caddy admin GET %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false, nil
	}
	if out != nil {
		if err := json.Unmarshal(trimmed, out); err != nil {
			return false, fmt.Errorf("caddy admin GET %s: unreadable configuration", path)
		}
	}
	return true, nil
}

// publishRoute adds or replaces the application's route. An identical route
// is left alone (no reload). New routes go first so a broader wildcard route
// elsewhere in the server cannot shadow them.
func (c nativeCaddy) publishRoute(ctx context.Context, id string, hosts []string) error {
	route, err := c.route(id, hosts)
	if err != nil {
		return err
	}
	var current json.RawMessage
	if found, err := c.get(ctx, "/id/"+nativeCaddyRouteID(id), &current); err != nil {
		return err
	} else if found && jsonEqual(current, route) {
		return nil
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

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	normalizedA, _ := json.Marshal(x)
	normalizedB, _ := json.Marshal(y)
	return bytes.Equal(normalizedA, normalizedB)
}

// routeHosts returns the hostnames of the application's published route;
// nil when none is published.
func (c nativeCaddy) routeHosts(ctx context.Context, id string) ([]string, error) {
	var route caddyRoute
	found, err := c.get(ctx, "/id/"+nativeCaddyRouteID(id), &route)
	if err != nil || !found {
		return nil, err
	}
	return caddyRouteHosts(route), nil
}

// servedHosts lists the hostnames of every application route this agent
// published into the server, except the given application's.
func (c nativeCaddy) servedHosts(ctx context.Context, except string) (map[string]bool, error) {
	var routes []json.RawMessage
	if _, err := c.get(ctx, "/config/apps/http/servers/"+c.cfg.Server+"/routes", &routes); err != nil {
		return nil, err
	}
	served := map[string]bool{}
	for _, raw := range routes {
		var route caddyRoute
		if json.Unmarshal(raw, &route) != nil || !strings.HasPrefix(route.ID, nativeCaddyRoutePrefix) || route.ID == nativeCaddyRouteID(except) {
			continue
		}
		for _, host := range caddyRouteHosts(route) {
			served[strings.ToLower(host)] = true
		}
	}
	return served, nil
}

// ---- TLS automation subjects -----------------------------------------------------

// caddyTLSPolicy is one automation policy. Subjects is nil when the policy
// applies to every hostname.
type caddyTLSPolicy struct {
	Subjects *[]string         `json:"subjects,omitempty"`
	Issuers  []json.RawMessage `json:"issuers,omitempty"`
	OnDemand bool              `json:"on_demand,omitempty"`
}

func (p caddyTLSPolicy) hasACMEIssuer() bool {
	for _, raw := range p.Issuers {
		var issuer struct {
			Module string `json:"module"`
		}
		if json.Unmarshal(raw, &issuer) == nil && issuer.Module == "acme" {
			return true
		}
	}
	return false
}

// matches reports whether the policy applies to host: every host when it
// has no subjects, else an exact or single-label wildcard subject.
func (p caddyTLSPolicy) matches(host string) bool {
	if p.Subjects == nil {
		return true
	}
	return caddySubjectsCover(*p.Subjects, host)
}

func caddySubjectsCover(subjects []string, host string) bool {
	host = strings.ToLower(host)
	for _, subject := range subjects {
		subject = strings.ToLower(subject)
		if subject == host {
			return true
		}
		if rest, wildcard := strings.CutPrefix(subject, "*."); wildcard {
			if label, parent, ok := strings.Cut(host, "."); ok && label != "" && parent == rest {
				return true
			}
		}
	}
	return false
}

// nativeCaddyTLS is the outcome of publishing hostnames to Caddy's TLS
// automation.
type nativeCaddyTLS struct {
	Added   []string // subjects added now
	Present []string // hostnames a policy already covered
	Skipped string   // why no subjects were needed (on-demand TLS)
	Policy  int      // index of the policy that holds the subjects; -1 when none
}

// Line is the log line for the create and move logs.
func (t nativeCaddyTLS) Line() string {
	switch {
	case t.Skipped != "":
		return "TLS: " + t.Skipped
	case len(t.Added) > 0:
		return "TLS: hostnames added to Caddy automation; certificate issuance follows DNS."
	default:
		return "TLS: hostnames are in Caddy automation; certificate issuance follows DNS."
	}
}

const caddyTLSPath = "/config/apps/tls"

type caddyTLSApp struct {
	Automation *struct {
		OnDemand json.RawMessage   `json:"on_demand,omitempty"`
		Policies *[]caddyTLSPolicy `json:"policies,omitempty"`
	} `json:"automation,omitempty"`
}

// targetPolicy chooses the policy that receives subjects: the first with an
// ACME issuer, else the first without a subjects restriction; -1 when none.
func caddyTargetPolicy(policies []caddyTLSPolicy) int {
	for i, policy := range policies {
		if policy.hasACMEIssuer() {
			return i
		}
	}
	for i, policy := range policies {
		if policy.Subjects == nil {
			return i
		}
	}
	return -1
}

// ensureTLSSubjects makes Caddy's automation cover hosts. Hostnames a policy
// already matches are left alone; the rest are added to the ACME policy's
// subjects (or a new policy when there is no automation). Other subjects are
// never changed.
func (c nativeCaddy) ensureTLSSubjects(ctx context.Context, hosts []string) (nativeCaddyTLS, error) {
	out := nativeCaddyTLS{Policy: -1}
	var tlsApp caddyTLSApp
	tlsFound, err := c.get(ctx, caddyTLSPath, &tlsApp)
	if err != nil {
		return out, err
	}
	var policies []caddyTLSPolicy
	automationFound, policiesFound := false, false
	if tlsFound && tlsApp.Automation != nil {
		automationFound = true
		if tlsApp.Automation.Policies != nil {
			policiesFound = true
			policies = *tlsApp.Automation.Policies
		}
	}
	var pending []string
	onDemand := 0
	for _, host := range hosts {
		matched := -1
		for i, policy := range policies {
			if policy.matches(host) {
				matched = i
				break
			}
		}
		switch {
		case matched >= 0 && policies[matched].OnDemand:
			onDemand++
		case matched >= 0:
			out.Present = append(out.Present, host)
		default:
			pending = append(pending, host)
		}
	}
	if onDemand == len(hosts) {
		out.Skipped = "Caddy issues certificates for these hostnames on demand; no automation subjects are needed."
		return out, nil
	}
	if len(pending) == 0 {
		return out, nil
	}
	target := caddyTargetPolicy(policies)
	switch {
	case target < 0:
		policy := map[string]any{"subjects": pending, "issuers": []any{map[string]any{"module": "acme"}}}
		var body []byte
		var path string
		switch {
		case !tlsFound:
			path, body = caddyTLSPath, mustJSON(map[string]any{"automation": map[string]any{"policies": []any{policy}}})
		case !automationFound:
			path, body = caddyTLSPath+"/automation", mustJSON(map[string]any{"policies": []any{policy}})
		case !policiesFound:
			path, body = caddyTLSPath+"/automation/policies", mustJSON([]any{policy})
		default:
			path, body = caddyTLSPath+"/automation/policies", mustJSON(policy)
		}
		if _, err := c.call(ctx, http.MethodPost, path, body); err != nil {
			return out, err
		}
		out.Policy, out.Added = len(policies), pending
	case policies[target].Subjects == nil:
		// An unrestricted policy applies to every hostname already; adding
		// subjects would restrict it. The hosts fall under it.
		out.Policy, out.Present = target, append(out.Present, pending...)
		return out, nil
	default:
		subjects := slices.Clone(*policies[target].Subjects)
		for _, host := range pending {
			if !caddySubjectsCover(subjects, host) {
				subjects = append(subjects, host)
			}
		}
		if _, err := c.call(ctx, http.MethodPatch, caddyTLSPath+"/automation/policies/"+strconv.Itoa(target)+"/subjects", mustJSON(subjects)); err != nil {
			return out, err
		}
		out.Policy, out.Added = target, pending
	}
	// Verify what Caddy holds now.
	var current []string
	if _, err := c.get(ctx, caddyTLSPath+"/automation/policies/"+strconv.Itoa(out.Policy)+"/subjects", &current); err != nil {
		return out, err
	}
	for _, host := range pending {
		if !caddySubjectsCover(current, host) {
			return out, fmt.Errorf("caddy automation policy %d does not list %s after the update", out.Policy, host)
		}
	}
	return out, nil
}

// removeTLSSubjects drops hosts from the ACME policy's subjects. Only exact
// subjects are removed, never wildcards or other names.
func (c nativeCaddy) removeTLSSubjects(ctx context.Context, hosts []string) error {
	if len(hosts) == 0 {
		return nil
	}
	var tlsApp caddyTLSApp
	if found, err := c.get(ctx, caddyTLSPath, &tlsApp); err != nil || !found || tlsApp.Automation == nil || tlsApp.Automation.Policies == nil {
		return err
	}
	policies := *tlsApp.Automation.Policies
	target := caddyTargetPolicy(policies)
	if target < 0 || policies[target].Subjects == nil {
		return nil
	}
	remaining := make([]string, 0, len(*policies[target].Subjects))
	removed := false
	for _, subject := range *policies[target].Subjects {
		if slices.ContainsFunc(hosts, func(host string) bool { return strings.EqualFold(host, subject) }) {
			removed = true
			continue
		}
		remaining = append(remaining, subject)
	}
	if !removed {
		return nil
	}
	_, err := c.call(ctx, http.MethodPatch, caddyTLSPath+"/automation/policies/"+strconv.Itoa(target)+"/subjects", mustJSON(remaining))
	return err
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// publish publishes the application's route and TLS subjects.
func (c nativeCaddy) publish(ctx context.Context, id string, hosts []string) (nativeCaddyTLS, error) {
	if err := c.publishRoute(ctx, id, hosts); err != nil {
		return nativeCaddyTLS{Policy: -1}, err
	}
	tls, err := c.ensureTLSSubjects(ctx, hosts)
	if err != nil {
		return tls, fmt.Errorf("tls automation: %w", err)
	}
	c.publishErrorPages(ctx, id, hosts)
	return tls, nil
}

// withdraw removes the application's route and the TLS subjects no other
// published application serves; a missing route is not an error. hosts are
// the application's hostnames as registered, for when the route is gone.
func (c nativeCaddy) withdraw(ctx context.Context, id string, hosts []string) error {
	routed, err := c.routeHosts(ctx, id)
	if err != nil {
		return err
	}
	if _, err := c.call(ctx, http.MethodDelete, "/id/"+nativeCaddyRouteID(id), nil); err != nil {
		return err
	}
	if err := c.withdrawErrorRoute(ctx, id); err != nil {
		return err
	}
	served, err := c.servedHosts(ctx, id)
	if err != nil {
		return err
	}
	var release []string
	for _, host := range append(routed, hosts...) {
		host = strings.ToLower(host)
		if host != "" && !served[host] && !slices.Contains(release, host) {
			release = append(release, host)
		}
	}
	return c.removeTLSSubjects(ctx, release)
}

// publishNativeCaddyRoute publishes a created application's hostnames when
// this host is fronted by an admin-API Caddy; otherwise it does nothing and
// returns an empty note. The note is the line for the create and move logs.
func (m *Manager) publishNativeCaddyRoute(ctx context.Context, id, domain string, additional []string) (string, error) {
	caddy, ok := m.nativeCaddy(ctx)
	if !ok {
		return "", nil
	}
	tls, err := caddy.publish(ctx, id, append([]string{domain}, additional...))
	if err != nil {
		return "", err
	}
	return tls.Line(), nil
}

func (m *Manager) withdrawNativeCaddyRoute(ctx context.Context, id string, hosts []string) error {
	caddy, ok := m.nativeCaddy(ctx)
	if !ok {
		return nil
	}
	return caddy.withdraw(ctx, id, hosts)
}

// nativeReleaseIngressNote keeps a created application's Caddy route and
// TLS subjects in place on every release (a reload of Caddy from an older
// configuration would otherwise lose them silently) and writes the outcome
// to the release log. Failures are logged, never fatal for the release.
func (m *Manager) nativeReleaseIngressNote(ctx context.Context, id string, logw io.Writer) {
	m.mu.Lock()
	app, found := m.data.NativeApps[id]
	_, created := m.data.NativeCreated[id]
	m.mu.Unlock()
	if !found || !created || app.Domain == "" {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	note, err := m.publishNativeCaddyRoute(bounded, id, app.Domain, app.AdditionalDomains)
	switch {
	case err != nil:
		note = "TLS: the Caddy route or automation could not be updated: " + boundedErrorText(err.Error(), 300)
		log.Printf("native %s: %s", id, note)
	case note == "":
		return
	}
	if logw != nil {
		_, _ = io.WriteString(logw, note+"\n")
	}
}
