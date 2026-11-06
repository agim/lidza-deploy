package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza/pkg/gateway"
)

// Automatic host gateway. A protected release (gateway_transport) reaches
// the agent through a loopback mTLS listener, the host gateway, instead of
// the plain proxy. Hosts whose agent configuration names no gateway get one
// at the first protected deployment, when the agent publishes routes to the
// host's Caddy through its admin API:
//
//   - credentials: a new root-only directory next to the agent
//     configuration (<config dir>/gateway-<UTC date>, 0700, files 0600) with
//     the same files the reviewed manual setup prepares;
//   - the web server's copies of ca.pem, edge.pem and edge-key.pem in
//     /etc/lidza/gateway-<UTC date>: root:caddy 0750/0640 when a caddy group
//     exists, else owned by the account Caddy runs as (0700/0600);
//   - the gateway configuration in the agent's encrypted state; the agent
//     configuration file is never rewritten;
//   - the listener on 127.0.0.1:8444, or a free loopback port when that is
//     taken, served by the agent itself without a restart;
//   - each protected application's Caddy route proxies to the listener over
//     mTLS with the native-edge headers (gatewayCaddyProxy).
//
// An existing identity is never regenerated: later deployments and agent
// starts reuse the recorded gateway. A gateway in the agent configuration
// (the reviewed manual setup) takes precedence and its web server keeps
// its hand-installed configuration.

const (
	gatewaySetupKind     = "gateway-setup"
	gatewayDefaultListen = "127.0.0.1:8444"
	gatewayCaddyUnit     = "caddy"
	gatewayCaddyGroup    = "caddy"
)

// HostGatewayState is an automatic host gateway.
type HostGatewayState struct {
	Config GatewayConfig `json:"config"`
	// Directory holds the private credentials; EdgeDirectory the web
	// server's copies of the CA and its client certificate and key.
	Directory       string    `json:"directory"`
	EdgeDirectory   string    `json:"edge_directory"`
	EdgeCA          string    `json:"edge_ca"`
	EdgeCertificate string    `json:"edge_certificate"`
	EdgeKey         string    `json:"edge_key"`
	EdgeOwner       string    `json:"edge_owner,omitempty"`
	Created         time.Time `json:"created"`
}

// Host access replaced in tests.
var (
	gatewayEdgeRoot = "/etc/lidza"
	gatewayCommand  = func(ctx context.Context, name string, args ...string) (string, error) {
		return hostCommand(ctx, name, args...)
	}
	gatewayLookupGroup = func(name string) (int, error) {
		group, err := user.LookupGroup(name)
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(group.Gid)
	}
	gatewayLookupUser = func(name string) (int, int, error) {
		account, err := user.Lookup(name)
		if err != nil {
			return 0, 0, err
		}
		uid, err := strconv.Atoi(account.Uid)
		if err != nil {
			return 0, 0, err
		}
		gid, err := strconv.Atoi(account.Gid)
		return uid, gid, err
	}
	gatewayChown  = os.Chown
	gatewayListen = func(address string) (net.Listener, error) { return net.Listen("tcp", address) }
)

// errGatewayIngressMissing names what a protected release needs on a host
// that has neither.
var errGatewayIngressMissing = errors.New("gateway_transport needs a protected host gateway, and this host has none: " +
	"the agent configuration has no gateway section and no Caddy admin API answers on 127.0.0.1:2019 for the agent to publish routes to. " +
	"Run Caddy with its admin API enabled (the agent then sets the gateway up itself at the next deployment), " +
	"or prepare one with `lidza-agent -gateway-transport prepare -gateway-output <new directory>`, merge its gateway-config.json into the agent configuration and install its Caddyfile.gateway snippet")

// gatewayCaddy is the Caddy the host gateway's routes are published to:
// the publishing Caddy (host_ingress.go) or, where Caddy forwards every
// hostname to the agent's proxy, the same Caddy's admin API. mode is the
// ingress mode.
func (m *Manager) gatewayCaddy(ctx context.Context) (nativeCaddy, string, bool) {
	if caddy, ok := m.nativeCaddy(ctx); ok {
		return caddy, HostIngressCaddy, true
	}
	status := m.hostIngress(ctx, false)
	if status.Mode != HostIngressAgent || !status.AdminAPI || status.adminServer == "" {
		return nativeCaddy{}, "", false
	}
	return nativeCaddy{cfg: NativeCaddyConfig{Admin: hostIngressDefaultAdmin, Server: status.adminServer, Upstream: m.cfg.ProxyListen}, client: &http.Client{Timeout: 15 * time.Second}}, HostIngressAgent, true
}

// ensureGateway makes the host gateway listener available: the configured
// one, the recorded automatic one, or a new automatic one.
func (m *Manager) ensureGateway(ctx context.Context, trigger string) error {
	if m.gatewayReady() {
		return nil
	}
	m.gatewaySetupMu.Lock()
	defer m.gatewaySetupMu.Unlock()
	if m.gatewayReady() {
		return nil
	}
	if m.cfg.Gateway != nil {
		return errors.New("the host gateway in the agent configuration is not running; check its gateway section (listen address and certificate files) and restart the agent")
	}
	if m.cfg.IngressOnly {
		return errors.New("protected releases are deployed by the full agent, not by an ingress-only reader")
	}
	if _, _, ok := m.gatewayCaddy(ctx); !ok {
		return errGatewayIngressMissing
	}
	m.mu.Lock()
	existing := m.data.HostGateway
	m.mu.Unlock()
	now := time.Now().UTC()
	job := newHostJob(gatewaySetupKind, trigger, now)
	m.mu.Lock()
	m.recordHostJobLocked(job)
	_ = m.save()
	m.mu.Unlock()
	var out strings.Builder
	var err error
	if existing == nil {
		var state HostGatewayState
		var listener net.Listener
		if state, listener, err = m.provisionHostGateway(ctx, now, &out); err == nil {
			err = m.startAutoGateway(state, listener)
			job.Summary = "Protected gateway set up on " + state.Config.Listen
		}
	} else {
		fmt.Fprintf(&out, "Starting the recorded protected gateway from %s on %s.\n", existing.Directory, existing.Config.Listen)
		err = m.startAutoGateway(*existing, nil)
		job.Summary = "Protected gateway restarted on " + existing.Config.Listen
	}
	finished := time.Now().UTC()
	job.Finished = &finished
	job.Status = "succeeded"
	if err != nil {
		job.Status, job.Summary = "failed", boundedErrorText(err.Error(), 300)
		fmt.Fprintf(&out, "Failed: %v\n", err)
	} else {
		fmt.Fprintf(&out, "The gateway listens on %s.\n", m.gatewayListenAddress())
	}
	job.Output = strings.TrimSpace(out.String())
	m.mu.Lock()
	m.recordHostJobLocked(job)
	_ = m.save()
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf("the protected host gateway could not be set up: %w", err)
	}
	return nil
}

func (m *Manager) gatewayListenAddress() string {
	m.gatewayMu.Lock()
	defer m.gatewayMu.Unlock()
	if m.gatewayConfig == nil {
		return ""
	}
	return m.gatewayConfig.Listen
}

// gatewayConfigDir is where new gateway credentials go: the agent
// configuration's directory, or the data directory without one.
func (m *Manager) gatewayConfigDir() string {
	if m.cfg.nativeConfigPath != "" {
		return filepath.Dir(m.cfg.nativeConfigPath)
	}
	return m.cfg.DataDir
}

// provisionHostGateway generates and records a new automatic gateway. The
// returned listener holds the chosen port until the gateway serves on it.
func (m *Manager) provisionHostGateway(ctx context.Context, now time.Time, out *strings.Builder) (state HostGatewayState, listener net.Listener, err error) {
	reserved, err := gatewayListen(gatewayDefaultListen)
	if err != nil {
		fmt.Fprintf(out, "%s is in use; choosing a free loopback port.\n", gatewayDefaultListen)
		if reserved, err = gatewayListen("127.0.0.1:0"); err != nil {
			return state, nil, fmt.Errorf("no loopback port is free for the gateway: %w", err)
		}
	}
	listen := reserved.Addr().String()
	var created []string
	defer func() {
		if err != nil {
			reserved.Close()
			for _, dir := range created {
				_ = os.RemoveAll(dir)
			}
		}
	}()
	name, private, err := gatewayNewDirectory(m.gatewayConfigDir(), "gateway-"+now.Format("20060102"))
	if err != nil {
		return state, nil, err
	}
	created = append(created, private)
	cfg, err := writeGatewayCredentials(private, listen)
	if err != nil {
		return state, nil, err
	}
	fragment, _ := json.MarshalIndent(map[string]any{"gateway": cfg}, "", "  ")
	if err := os.WriteFile(filepath.Join(private, "gateway-config.json"), fragment, 0600); err != nil {
		return state, nil, err
	}
	readme := "The agent created this protected gateway itself and records it in its encrypted state; do not merge gateway-config.json into the agent configuration.\n" +
		"The web server reads its copies of ca.pem, edge.pem and edge-key.pem from the edge directory recorded with the gateway. Keep these files: an existing identity is never regenerated.\n"
	if err := os.WriteFile(filepath.Join(private, "README.txt"), []byte(readme), 0600); err != nil {
		return state, nil, err
	}
	fmt.Fprintf(out, "Wrote the gateway credentials to %s.\n", private)
	state = HostGatewayState{Config: cfg, Directory: private, Created: now}
	edge, err := m.writeGatewayEdgeFiles(ctx, private, name, &state, out)
	if edge != "" {
		created = append(created, edge)
	}
	if err != nil {
		return state, nil, err
	}
	m.mu.Lock()
	if m.data.HostGateway != nil {
		m.mu.Unlock()
		return state, nil, errors.New("a protected gateway was recorded concurrently")
	}
	stored := state
	m.data.HostGateway = &stored
	if err := m.save(); err != nil {
		m.data.HostGateway = nil
		m.mu.Unlock()
		return state, nil, fmt.Errorf("could not record the gateway: %w", err)
	}
	m.mu.Unlock()
	return state, reserved, nil
}

// gatewayNewDirectory creates a new 0700 directory named base (or base-2,
// base-3 ... when taken) under parent.
func gatewayNewDirectory(parent, base string) (string, string, error) {
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", "", err
	}
	for n := 1; n < 100; n++ {
		name := base
		if n > 1 {
			name = base + "-" + strconv.Itoa(n)
		}
		path := filepath.Join(parent, name)
		if err := os.Mkdir(path, 0700); err == nil {
			return name, path, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", "", err
		}
	}
	return "", "", errors.New("no free gateway directory name under " + parent)
}

// gatewayEdgeAccess is how the web server's copies are owned.
type gatewayEdgeAccess struct {
	uid, gid          int
	dirMode, fileMode os.FileMode
	owner             string
}

// gatewayEdgeOwner chooses the owner of the web server's copies: the caddy
// group when it exists, else the account the Caddy service runs as.
func gatewayEdgeOwner(ctx context.Context) (gatewayEdgeAccess, error) {
	if gid, err := gatewayLookupGroup(gatewayCaddyGroup); err == nil {
		return gatewayEdgeAccess{uid: 0, gid: gid, dirMode: 0750, fileMode: 0640, owner: "root:" + gatewayCaddyGroup}, nil
	}
	account := ""
	if text, err := gatewayCommand(ctx, "systemctl", "show", gatewayCaddyUnit, "-p", "User", "--value"); err == nil {
		account = strings.TrimSpace(text)
	}
	if account == "" {
		account = caddyProcessUser()
	}
	if account == "" || account == "root" || strings.HasPrefix(account, "uid ") && strings.TrimPrefix(account, "uid ") == "0" {
		return gatewayEdgeAccess{dirMode: 0700, fileMode: 0600, owner: "root"}, nil
	}
	uid, gid, err := gatewayLookupUser(account)
	if err != nil {
		return gatewayEdgeAccess{}, fmt.Errorf("the account Caddy runs as (%s) is unknown: %w", account, err)
	}
	return gatewayEdgeAccess{uid: uid, gid: gid, dirMode: 0700, fileMode: 0600, owner: account}, nil
}

// writeGatewayEdgeFiles copies the CA and the edge certificate and key for
// the web server. It returns the directory it created, if any.
func (m *Manager) writeGatewayEdgeFiles(ctx context.Context, private, name string, state *HostGatewayState, out *strings.Builder) (string, error) {
	access, err := gatewayEdgeOwner(ctx)
	if err != nil {
		return "", err
	}
	root := filepath.Clean(gatewayEdgeRoot)
	if err := os.MkdirAll(root, 0755); err != nil {
		return "", err
	}
	if access.uid != 0 || access.gid != 0 {
		// The web server's account must reach the directory.
		for current := root; ; current = filepath.Dir(current) {
			info, err := os.Stat(current)
			if err != nil {
				return "", err
			}
			if info.Mode().Perm()&0o001 == 0 {
				return "", fmt.Errorf("%s is not traversable by Caddy's account", current)
			}
			if filepath.Dir(current) == current {
				break
			}
		}
	}
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", fmt.Errorf("the edge directory %s could not be created: %w", dir, err)
	}
	if err := gatewayChown(dir, access.uid, access.gid); err != nil {
		return dir, err
	}
	paths := map[string]*string{"ca.pem": &state.EdgeCA, "edge.pem": &state.EdgeCertificate, "edge-key.pem": &state.EdgeKey}
	for file, target := range paths {
		raw, err := os.ReadFile(filepath.Join(private, file))
		if err != nil {
			return dir, err
		}
		path := filepath.Join(dir, file)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			return dir, err
		}
		if err := gatewayChown(path, access.uid, access.gid); err != nil {
			return dir, err
		}
		if err := os.Chmod(path, access.fileMode); err != nil {
			return dir, err
		}
		*target = path
	}
	if err := os.Chmod(dir, access.dirMode); err != nil {
		return dir, err
	}
	state.EdgeDirectory, state.EdgeOwner = dir, access.owner
	fmt.Fprintf(out, "Wrote Caddy's copies of ca.pem, edge.pem and edge-key.pem to %s (owner %s, directory %04o, files %04o).\n", dir, access.owner, access.dirMode, access.fileMode)
	return dir, nil
}

// startAutoGateway loads an automatic gateway and serves it until the
// manager closes. listener is the port reserved at setup, or nil to bind
// the recorded address.
func (m *Manager) startAutoGateway(state HostGatewayState, listener net.Listener) error {
	serverTLS, verify, err := m.loadGatewayTLS(state.Config)
	if err != nil {
		if listener != nil {
			listener.Close()
		}
		return err
	}
	if listener == nil {
		if listener, err = gatewayListen(state.Config.Listen); err != nil {
			return fmt.Errorf("the gateway port %s is unavailable: %w", state.Config.Listen, err)
		}
	}
	cfg := state.Config
	m.gatewayMu.Lock()
	m.gatewayTLS, m.gatewayVerify, m.gatewayConfig, m.gatewayAuto = serverTLS, verify, &cfg, true
	m.gatewayMu.Unlock()
	server := m.gatewayHTTPServer(cfg.Listen)
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	stopped := make(chan struct{})
	m.wg.Add(2)
	go func() {
		defer m.wg.Done()
		defer close(stopped)
		if err := server.ServeTLS(listener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("host gateway %s: %v", cfg.Listen, err)
			// The next protected deployment starts it again.
			m.gatewayMu.Lock()
			m.gatewayTLS, m.gatewayVerify = nil, nil
			m.gatewayMu.Unlock()
		}
	}()
	go func() {
		defer m.wg.Done()
		select {
		case <-ctx.Done():
		case <-stopped:
			return
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("host gateway listens on %s", cfg.Listen)
	return nil
}

// gatewayCaddyProxy is the reverse_proxy handler that sends a protected
// application's requests to the gateway listener over mTLS, with the
// native-edge headers set from the actual connection (the same as the
// reviewed Caddyfile.gateway snippet).
func gatewayCaddyProxy(state HostGatewayState) map[string]any {
	set := map[string]any{
		"Host":                      []string{"{http.request.hostport}"},
		gateway.Header:              []string{gateway.NativeEdgeHeaders},
		"Lidza-Gateway-Tls-Version": []string{"{http.request.tls.version}"},
		"Lidza-Gateway-Authority":   []string{"{http.request.hostport}"},
		"Lidza-Gateway-Server-Name": []string{"{http.request.tls.server_name}"},
		"Lidza-Gateway-Method":      []string{"{http.request.method}"},
		"Lidza-Gateway-Target":      []string{"{http.request.uri}"},
		"Lidza-Gateway-Issued":      []string{"{time.now.unix}"},
		"Lidza-Gateway-Nonce":       []string{"{http.request.uuid}"},
	}
	return map[string]any{
		"handler":   "reverse_proxy",
		"upstreams": []any{map[string]any{"dial": state.Config.Listen}},
		"headers":   map[string]any{"request": map[string]any{"set": set}},
		"transport": map[string]any{
			"protocol": "http",
			"tls": map[string]any{
				"server_name":                 gatewayServerName,
				"root_ca_pem_files":           []string{state.EdgeCA},
				"client_certificate_file":     state.EdgeCertificate,
				"client_certificate_key_file": state.EdgeKey,
			},
		},
	}
}

// autoGateway returns the automatic gateway when it serves this host.
func (m *Manager) autoGateway() (HostGatewayState, bool) {
	m.gatewayMu.Lock()
	auto := m.gatewayAuto
	m.gatewayMu.Unlock()
	if !auto || m.cfg.Gateway != nil {
		return HostGatewayState{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.HostGateway == nil {
		return HostGatewayState{}, false
	}
	return *m.data.HostGateway, true
}

// publishGatewayRoute points a protected application's hostnames at the
// automatic gateway in the host's Caddy. A configured gateway's web server
// keeps its reviewed configuration, so nothing is published for it.
func (m *Manager) publishGatewayRoute(ctx context.Context, a App) error {
	state, ok := m.autoGateway()
	if !ok {
		return nil
	}
	caddy, mode, ok := m.gatewayCaddy(ctx)
	if !ok {
		return errors.New("the protected route cannot be published: the host's Caddy admin API is unavailable on 127.0.0.1:2019")
	}
	caddy.proxy = gatewayCaddyProxy(state)
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hosts := a.Hostnames()
	var err error
	if mode == HostIngressAgent {
		// Caddy issues certificates on demand there and already serves
		// the agent's error pages through the proxy.
		err = caddy.publishRoute(bounded, a.ID, hosts)
	} else {
		_, err = caddy.publish(bounded, a.ID, hosts)
	}
	if err != nil {
		return fmt.Errorf("the protected route could not be published to Caddy: %w", err)
	}
	return nil
}

// withdrawGatewayRoute removes a protected application's route from the
// host's Caddy when the automatic gateway published it.
func (m *Manager) withdrawGatewayRoute(ctx context.Context, a App) error {
	if a.Current == nil || a.Current.Stack == nil || a.Current.Stack.Gateway == nil {
		return nil
	}
	if _, ok := m.autoGateway(); !ok {
		return nil
	}
	caddy, mode, ok := m.gatewayCaddy(ctx)
	if !ok {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if mode == HostIngressAgent {
		_, err := caddy.call(bounded, http.MethodDelete, "/id/"+nativeCaddyRouteID(a.ID), nil)
		return err
	}
	return caddy.withdraw(bounded, a.ID, a.Hostnames())
}

// gatewayCapability is what a move destination reports about protected
// releases: configured (a gateway exists: the agent configuration's or one
// the agent set up), auto (the agent sets one up at the first protected
// deployment) or unavailable (neither a gateway nor a Caddy admin API).
func (m *Manager) gatewayCapability(ctx context.Context) string {
	if m.cfg.Gateway != nil {
		return "configured"
	}
	if _, ok := m.autoGateway(); ok {
		return "configured"
	}
	if m.cfg.IngressOnly {
		return "unavailable"
	}
	if _, _, ok := m.gatewayCaddy(ctx); ok {
		return "auto"
	}
	return "unavailable"
}
