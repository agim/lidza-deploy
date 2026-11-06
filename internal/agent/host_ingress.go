package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/procfs"
)

// Web ingress of the host: who answers the public web ports, and how created
// applications are published.
//
//   - agent: the agent's proxy (proxy_listen) is the ingress. The web server
//     installed with the agent forwards every hostname to it and issues
//     on-demand certificates approved through /tls/allow; nothing per
//     application is written into the web server.
//   - caddy: an operator-managed Caddy owns the web ports and is configured
//     through its admin API. Created applications are published there as one
//     route each plus their TLS automation subjects (native_caddy.go).
//
// The mode is detected at startup and refreshed periodically: a Caddy admin
// API on 127.0.0.1:2019 whose HTTPS server forwards every hostname to the
// agent's proxy with on-demand TLS is the agent layout; any other admin-API
// Caddy is the caddy layout. The agent configuration's native.caddy remains
// an explicit override, and the panel can store one (PUT /v1/host/ingress)
// when the configuration file sets none.

const (
	HostIngressAgent = "agent"
	HostIngressCaddy = "caddy"

	hostIngressDefaultAdmin = "http://127.0.0.1:2019"
	hostIngressProbeTimeout = 4 * time.Second
)

// HostIngressStatus is the host's detected and effective web ingress.
type HostIngressStatus struct {
	Mode        string `json:"mode"`   // agent or caddy
	Source      string `json:"source"` // config, setting or detected
	CaddyAdmin  string `json:"caddy_admin,omitempty"`
	CaddyServer string `json:"caddy_server,omitempty"`
	// AdminAPI reports that a Caddy admin API answered on the loopback
	// address; WebPorts lists the public web ports another process listens
	// on (80, 443).
	AdminAPI bool      `json:"admin_api"`
	WebPorts []int     `json:"web_ports,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Checked  time.Time `json:"checked"`
	// Detected is what the probes found, before any override.
	Detected string `json:"detected,omitempty"`
	// Override is the panel's stored setting; nil when none is stored.
	Override *NativeCaddyConfig `json:"override,omitempty"`
	// Configured reports that the agent's configuration file sets
	// native.caddy; the panel cannot change that.
	Configured bool `json:"configured"`
	// ErrorPages reports the managed error pages and foreign error-page
	// directories of a fronting Caddy (error_pages.go); nil for the agent
	// proxy and in cached detections.
	ErrorPages *HostErrorPages `json:"error_pages,omitempty"`
	// adminServer is the HTTPS server of a detected admin-API Caddy in
	// either mode; the automatic gateway publishes protected routes there
	// (gateway_auto.go).
	adminServer string
}

// HostIngressSettings is the panel's override: a Caddy admin API to publish
// created applications to, or nil to detect the ingress automatically.
type HostIngressSettings struct {
	Caddy *NativeCaddyConfig `json:"caddy"`
}

// Validate checks an override before it is stored or forwarded.
func (s HostIngressSettings) Validate() error {
	if s.Caddy == nil {
		return nil
	}
	if s.Caddy.Admin == "" {
		return errors.New("the Caddy admin API URL is required")
	}
	if s.Caddy.Upstream != "" {
		return errors.New("the proxy upstream is the agent's own listener and cannot be set here")
	}
	return s.Caddy.validate()
}

// HostIngressState is the stored override.
type HostIngressState struct {
	Caddy *NativeCaddyConfig `json:"caddy,omitempty"`
}

// Probes replaced in tests.
var (
	hostIngressListeners = webPortListeners
	hostIngressAdminGet  = caddyAdminConfig
	hostIngressInterval  = 5 * time.Minute
)

// webPortListeners lists the public web ports (80, 443) some process listens
// on. The agent never binds them itself: its listeners are the API, the
// proxy and the optional gateway.
func webPortListeners() []int {
	fs, err := procfs.NewFS("/proc")
	if err != nil {
		return nil
	}
	found := map[int]bool{}
	for _, table := range []func() (procfs.NetTCP, error){fs.NetTCP, fs.NetTCP6} {
		lines, err := table()
		if err != nil {
			continue
		}
		for _, line := range lines {
			if line.St == 0x0A && (line.LocalPort == 80 || line.LocalPort == 443) { // LISTEN
				found[int(line.LocalPort)] = true
			}
		}
	}
	ports := make([]int, 0, len(found))
	for port := range found {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

// caddyAdminConfig reads the complete configuration of a Caddy admin API.
func caddyAdminConfig(ctx context.Context, admin string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(admin, "/")+"/config/", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: hostIngressProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("caddy admin returned " + resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// caddyConfig is the part of a Caddy configuration the detection reads.
type caddyConfig struct {
	Apps struct {
		HTTP struct {
			Servers map[string]caddyServer `json:"servers"`
		} `json:"http"`
		TLS *struct {
			Automation *struct {
				OnDemand json.RawMessage  `json:"on_demand"`
				Policies []caddyTLSPolicy `json:"policies"`
			} `json:"automation"`
		} `json:"tls"`
	} `json:"apps"`
}

type caddyServer struct {
	Listen []string          `json:"listen"`
	Routes []json.RawMessage `json:"routes"`
}

type caddyRoute struct {
	ID     string            `json:"@id"`
	Match  []json.RawMessage `json:"match"`
	Handle []json.RawMessage `json:"handle"`
}

type caddyHandler struct {
	Handler   string            `json:"handler"`
	Routes    []json.RawMessage `json:"routes"`
	Upstreams []struct {
		Dial string `json:"dial"`
	} `json:"upstreams"`
}

// caddyServerFor picks the server that terminates HTTPS: the one listening
// on port 443, else the first by name.
func caddyServerFor(servers map[string]caddyServer) (string, caddyServer) {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, listen := range servers[name].Listen {
			if strings.HasSuffix(listen, ":443") {
				return name, servers[name]
			}
		}
	}
	if len(names) == 0 {
		return "", caddyServer{}
	}
	return names[0], servers[names[0]]
}

// caddyRouteHosts returns the host matchers of a route; nil when the route
// is not restricted to hostnames.
func caddyRouteHosts(route caddyRoute) []string {
	var hosts []string
	for _, raw := range route.Match {
		var matcher struct {
			Host []string `json:"host"`
		}
		if json.Unmarshal(raw, &matcher) == nil {
			hosts = append(hosts, matcher.Host...)
		}
	}
	return hosts
}

// caddyProxiesTo reports whether a handler chain (through subroutes)
// reverse-proxies to upstream.
func caddyProxiesTo(handlers []json.RawMessage, upstream string, depth int) bool {
	if depth > 8 {
		return false
	}
	for _, raw := range handlers {
		var handler caddyHandler
		if json.Unmarshal(raw, &handler) != nil {
			continue
		}
		switch handler.Handler {
		case "reverse_proxy":
			for _, u := range handler.Upstreams {
				if u.Dial == upstream {
					return true
				}
			}
		case "subroute":
			for _, rawRoute := range handler.Routes {
				var route caddyRoute
				if json.Unmarshal(rawRoute, &route) == nil && caddyProxiesTo(route.Handle, upstream, depth+1) {
					return true
				}
			}
		}
	}
	return false
}

// caddyForwardsAllHosts reports whether the server has a route without host
// restriction that reaches the agent's proxy: the layout the agent
// installer writes.
func caddyForwardsAllHosts(server caddyServer, upstream string) bool {
	if upstream == "" {
		return false
	}
	for _, raw := range server.Routes {
		var route caddyRoute
		if json.Unmarshal(raw, &route) != nil {
			continue
		}
		if len(caddyRouteHosts(route)) == 0 && caddyProxiesTo(route.Handle, upstream, 0) {
			return true
		}
	}
	return false
}

// caddyOnDemandTLS reports whether Caddy issues certificates on demand: an
// on_demand automation block, or a policy that enables it.
func caddyOnDemandTLS(cfg caddyConfig) bool {
	if cfg.Apps.TLS == nil || cfg.Apps.TLS.Automation == nil {
		return false
	}
	automation := cfg.Apps.TLS.Automation
	if len(automation.OnDemand) > 0 && string(automation.OnDemand) != "null" {
		return true
	}
	for _, policy := range automation.Policies {
		if policy.OnDemand {
			return true
		}
	}
	return false
}

// hostIngressDetection is the result of the probes.
type hostIngressDetection struct {
	Mode     string
	Server   string
	Detail   string
	AdminAPI bool
	Ports    []int
}

// detectHostIngress probes the host's web ports and the loopback Caddy
// admin API. proxyUpstream is the agent's proxy listener.
func detectHostIngress(ctx context.Context, admin, proxyUpstream string) hostIngressDetection {
	out := hostIngressDetection{Mode: HostIngressAgent, Ports: hostIngressListeners()}
	ctx, cancel := context.WithTimeout(ctx, hostIngressProbeTimeout)
	defer cancel()
	raw, err := hostIngressAdminGet(ctx, admin)
	if err != nil {
		if len(out.Ports) > 0 {
			out.Detail = "the host's web server has no Caddy admin API on " + strings.TrimPrefix(admin, "http://") + "; the agent proxy is the ingress behind it"
		} else {
			out.Detail = "no process listens on ports 80 or 443 yet"
		}
		return out
	}
	var cfg caddyConfig
	if json.Unmarshal(raw, &cfg) != nil || len(cfg.Apps.HTTP.Servers) == 0 {
		out.Detail = "the Caddy admin API has no HTTP servers; the agent proxy is the ingress"
		return out
	}
	out.AdminAPI = true
	name, server := caddyServerFor(cfg.Apps.HTTP.Servers)
	out.Server = name
	if caddyForwardsAllHosts(server, proxyUpstream) && caddyOnDemandTLS(cfg) {
		out.Detail = "Caddy forwards every hostname to the agent proxy and issues certificates on demand"
		return out
	}
	out.Mode = HostIngressCaddy
	out.Detail = "Caddy serves the web ports and is configured through its admin API; created applications are published there"
	return out
}

// hostIngress returns the host's ingress status, refreshing the detection
// when it is stale or refresh is set. The configuration file and the stored
// override take precedence over what was detected.
func (m *Manager) hostIngress(ctx context.Context, refresh bool) HostIngressStatus {
	m.ingressMu.Lock()
	cached := m.ingressDetected
	m.ingressMu.Unlock()
	if cached == nil || refresh || time.Since(cached.Checked) > hostIngressInterval {
		cached = m.refreshIngressDetection(ctx)
	}
	status := *cached
	status.WebPorts = slices.Clone(status.WebPorts)
	m.mu.Lock()
	if m.data.HostIngress != nil && m.data.HostIngress.Caddy != nil {
		override := *m.data.HostIngress.Caddy
		status.Override = &override
	}
	m.mu.Unlock()
	var explicit *NativeCaddyConfig
	if m.cfg.Native != nil && m.cfg.Native.Caddy != nil && m.cfg.Native.Caddy.Admin != "" {
		status.Configured = true
		status.Source = "config"
		explicit = m.cfg.Native.Caddy
	} else if status.Override != nil && status.Override.Admin != "" {
		status.Source = "setting"
		explicit = status.Override
	}
	if explicit != nil {
		status.Mode = HostIngressCaddy
		status.CaddyAdmin, status.CaddyServer = explicit.Admin, explicit.Server
		if status.CaddyServer == "" {
			status.CaddyServer = "srv0"
		}
	}
	return status
}

// hostIngressReport is the ingress status with the error-page report of a
// fronting Caddy, for the panel.
func (m *Manager) hostIngressReport(ctx context.Context, refresh bool) HostIngressStatus {
	status := m.hostIngress(ctx, refresh)
	if status.Mode == HostIngressCaddy {
		bounded, cancel := context.WithTimeout(ctx, 2*hostIngressProbeTimeout)
		status.ErrorPages = m.hostErrorPages(bounded)
		cancel()
	}
	return status
}

// hostIngressCaddy resolves the Caddy admin API created applications are
// published to; ok is false when the agent proxy is the ingress.
func (m *Manager) hostIngressCaddy(ctx context.Context) (NativeCaddyConfig, bool) {
	// Explicit choices need no probes.
	if m.cfg.Native != nil && m.cfg.Native.Caddy != nil && m.cfg.Native.Caddy.Admin != "" {
		return *m.cfg.Native.Caddy, true
	}
	m.mu.Lock()
	override := m.data.HostIngress
	m.mu.Unlock()
	if override != nil && override.Caddy != nil && override.Caddy.Admin != "" {
		return *override.Caddy, true
	}
	status := m.hostIngress(ctx, false)
	if status.Mode != HostIngressCaddy || status.CaddyAdmin == "" {
		return NativeCaddyConfig{}, false
	}
	return NativeCaddyConfig{Admin: status.CaddyAdmin, Server: status.CaddyServer}, true
}

// setHostIngress stores the panel's override after checking that the named
// admin API answers and has the named server.
func (m *Manager) setHostIngress(ctx context.Context, settings HostIngressSettings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	if m.cfg.Native != nil && m.cfg.Native.Caddy != nil && m.cfg.Native.Caddy.Admin != "" {
		return errors.New("native.caddy is set in the agent configuration file; change it there")
	}
	if settings.Caddy != nil {
		probe, cancel := context.WithTimeout(ctx, hostIngressProbeTimeout)
		raw, err := hostIngressAdminGet(probe, settings.Caddy.Admin)
		cancel()
		if err != nil {
			return errors.New("the Caddy admin API did not answer at " + settings.Caddy.Admin)
		}
		var cfg caddyConfig
		if json.Unmarshal(raw, &cfg) != nil || len(cfg.Apps.HTTP.Servers) == 0 {
			return errors.New("the Caddy admin API has no HTTP servers")
		}
		server := settings.Caddy.Server
		if server == "" {
			server, _ = caddyServerFor(cfg.Apps.HTTP.Servers)
			settings.Caddy.Server = server
		}
		if _, found := cfg.Apps.HTTP.Servers[server]; !found {
			return errors.New("the Caddy configuration has no HTTP server named " + server)
		}
	}
	m.mu.Lock()
	previous := m.data.HostIngress
	if settings.Caddy == nil {
		m.data.HostIngress = nil
	} else {
		stored := *settings.Caddy
		m.data.HostIngress = &HostIngressState{Caddy: &stored}
	}
	if err := m.save(); err != nil {
		m.data.HostIngress = previous
		m.mu.Unlock()
		return errors.New("could not save the ingress setting")
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) hostIngressRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/host/ingress", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, 200, m.hostIngressReport(r.Context(), r.URL.Query().Get("refresh") == "1"))
	})
	// Points error routes that serve pages from a foreign directory at the
	// managed error pages.
	mux.HandleFunc("POST /v1/host/ingress/error-pages", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		if err := m.adoptErrorPages(ctx); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, m.hostIngressReport(r.Context(), false))
	})
	mux.HandleFunc("PUT /v1/host/ingress", func(w http.ResponseWriter, r *http.Request) {
		var settings HostIngressSettings
		if err := Decode(w, r, &settings); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setHostIngress(r.Context(), settings); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, m.hostIngressReport(r.Context(), true))
	})
}

// refreshIngressDetection runs the probes (bounded by
// hostIngressProbeTimeout and ctx) and caches the result. It takes no
// manager lock other than ingressMu, so it never waits on state writers.
func (m *Manager) refreshIngressDetection(ctx context.Context) *HostIngressStatus {
	detection := detectHostIngress(ctx, hostIngressDefaultAdmin, m.cfg.ProxyListen)
	status := HostIngressStatus{Mode: detection.Mode, Detected: detection.Mode, Source: "detected", AdminAPI: detection.AdminAPI,
		WebPorts: detection.Ports, Detail: detection.Detail, Checked: time.Now().UTC()}
	if detection.AdminAPI {
		status.adminServer = detection.Server
	}
	if detection.Mode == HostIngressCaddy {
		status.CaddyAdmin, status.CaddyServer = hostIngressDefaultAdmin, detection.Server
	}
	m.ingressMu.Lock()
	m.ingressDetected = &status
	m.ingressMu.Unlock()
	return &status
}

// hostIngressLoop detects the ingress at startup and keeps the detection
// fresh, so health reports and application creation never wait for probes.
// It only refreshes the detection cache: it never takes m.mu, so Close
// returns promptly even when its caller holds the state lock, and every
// probe is bounded and cancelled with the manager's context.
func (m *Manager) hostIngressLoop() {
	defer m.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-timer.C:
		}
		ensureErrorPagesForIngress(m.refreshIngressDetection(m.ctx))
		timer.Reset(hostIngressInterval)
	}
}
