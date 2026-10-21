package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza/pkg/gateway"
)

const gatewayAppName = "lidza-app.internal"
const gatewayEnvPrefix = "DEPLOY_GATEWAY_"

type GatewayConfig struct {
	Listen      string   `json:"listen"`
	Certificate string   `json:"certificate"`
	PrivateKey  string   `json:"private_key"`
	ClientCA    string   `json:"client_ca"`
	PeerPins    []string `json:"peer_pins"`
}
type StackGateway struct {
	Version int      `json:"version"`
	Paths   []string `json:"paths"`
}
type GatewayRelease struct {
	Identity string `json:"identity"`
	Port     string `json:"port"`
}

func (g *StackGateway) validate() error {
	if g == nil {
		return nil
	}
	if g.Version != 1 || len(g.Paths) == 0 || len(g.Paths) > 8 {
		return errors.New("gateway_transport requires version1 and 1..8 explicit protected path prefixes")
	}
	seen := map[string]bool{}
	for _, p := range g.Paths {
		if !validStackProbePath(p) || p == "/" || p == "/readyz" || p == "/healthz" || p == "/metrics" || strings.HasPrefix(p, "/.well-known/") || seen[p] {
			return errors.New("invalid or duplicate protected gateway path prefix")
		}
		seen[p] = true
	}
	return nil
}
func protectedGatewayPath(r *Release, path string) bool {
	if r == nil || r.Stack == nil || r.Stack.Manifest.GatewayTransport == nil {
		return false
	}
	for attempt := 0; attempt < 3; attempt++ {
		clean := pathpkg.Clean("/" + strings.TrimPrefix(path, "/"))
		for _, prefix := range r.Stack.Manifest.GatewayTransport.Paths {
			if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
				return true
			}
		}
		next, err := url.PathUnescape(path)
		if err != nil || next == path {
			break
		}
		path = next
	}
	return false
}
func readGatewayFile(path string, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 32768 || private && info.Mode().Perm()&0027 != 0 {
		return nil, errors.New("gateway TLS file unavailable, oversized or insecure")
	}
	return os.ReadFile(path)
}
func (m *Manager) initGateway() error {
	c := m.cfg.Gateway
	if c == nil {
		return nil
	}
	host, port, err := net.SplitHostPort(c.Listen)
	n, e := strconv.Atoi(port)
	ip := net.ParseIP(host)
	if err != nil || e != nil || n < 1 || n > 65535 || ip == nil || !ip.IsLoopback() {
		return errors.New("gateway listen must be an explicit loopback IP and TCP port")
	}
	cert, err := readGatewayFile(c.Certificate, false)
	if err != nil {
		return err
	}
	key, err := readGatewayFile(c.PrivateKey, true)
	if err != nil {
		return err
	}
	ca, err := readGatewayFile(c.ClientCA, false)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return errors.New("gateway host certificate/key is invalid")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return errors.New("gateway client CA is invalid")
	}
	m.gatewayTLS, err = gateway.ServerTLS(pair, pool)
	if err != nil {
		return err
	}
	m.gatewayVerify, err = gateway.Verify(gateway.Policy{Peers: slices.Clone(c.PeerPins), NativeEdges: slices.Clone(c.PeerPins), Authorize: m.authorizeGatewayAuthority, Claim: m.claimGatewayNonce})
	return err
}
func (m *Manager) authorizeGatewayAuthority(ctx context.Context, authority string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	host := authority
	if h, p, err := net.SplitHostPort(authority); err == nil {
		if p != "443" {
			return errors.New("unapproved gateway port")
		}
		host = h
	}
	if _, found := m.hostApplication(ctx, host); found {
		return nil
	}
	return errors.New("unapproved gateway authority")
}
func (m *Manager) GatewayServer() *http.Server {
	if m.gatewayTLS == nil || m.gatewayVerify == nil {
		return nil
	}
	legacy := Proxy(m)
	verified := m.gatewayVerify(legacy)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		owner, found := m.hostApplication(r.Context(), host)
		if found && protectedGatewayPath(owner.Current, r.URL.Path) {
			verified.ServeHTTP(w, r)
			return
		}
		// Ordinary assets/APIs do not need origin attestation or replay-journal I/O.
		// Proxy rechecks the current release and rejects protected paths without it.
		legacy.ServeHTTP(w, r)
	})
	return &http.Server{Addr: m.cfg.Gateway.Listen, Handler: handler, TLSConfig: m.gatewayTLS.Clone(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32768}
}
func (m *Manager) gatewayClient(identity string) (*http.Client, error) {
	m.mu.Lock()
	pki, ok := m.data.GatewayPKI[identity]
	m.mu.Unlock()
	if !ok {
		return nil, errors.New("retained app gateway identity is unavailable")
	}
	m.gatewayMu.Lock()
	defer m.gatewayMu.Unlock()
	if m.gatewayClients == nil {
		m.gatewayClients = map[string]*gateway.Transport{}
	}
	if t := m.gatewayClients[identity]; t != nil {
		return &http.Client{Transport: t, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
	}
	transport, err := pki.client()
	if err != nil {
		return nil, err
	}
	m.gatewayClients[identity] = transport
	return &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func (m *Manager) prepareGateway(a App, manifest *StackManifest) (App, error) {
	if manifest.GatewayTransport == nil {
		if a.Current != nil && a.Current.Stack != nil && a.Current.Stack.Gateway != nil {
			return a, errors.New("a protected release cannot remove gateway_transport during an in-place update")
		}
		return a, nil
	}
	if a.Current != nil && a.Current.Stack != nil && a.Current.Stack.Manifest.GatewayTransport != nil {
		for _, old := range a.Current.Stack.Manifest.GatewayTransport.Paths {
			if !slices.Contains(manifest.GatewayTransport.Paths, old) {
				return a, errors.New("protected gateway path prefixes cannot be removed by an in-place update")
			}
		}
	}
	if m.gatewayTLS == nil {
		return a, errors.New("gateway_transport requires a configured protected host ingress; prepare and install the reviewed Caddy mTLS configuration first")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.GatewayPKI == nil {
		m.data.GatewayPKI = map[string]gatewayPKI{}
	}
	id := a.resourceID()
	pki, exists := m.data.GatewayPKI[id]
	if !exists {
		if a.Current != nil && a.Current.Stack != nil && a.Current.Stack.Gateway != nil {
			return a, errors.New("retained gateway identity is missing; restore it rather than regenerating a running identity")
		}
		var err error
		pki, err = newGatewayPKI(gatewayAppName)
		if err != nil {
			return a, err
		}
	}
	oldPKI := pki
	pki, renewed, err := renewGatewayPKI(pki)
	if err != nil {
		return a, err
	}
	cert, err := gatewayCertificate(pki.ClientCert, pki.ClientKey)
	if err != nil {
		return a, err
	}
	if _, err := gatewayCertificate(pki.ServerCert, pki.ServerKey); err != nil {
		return a, err
	}
	values := map[string]string{
		"DEPLOY_GATEWAY_ADDR": "0.0.0.0:3001",
		"DEPLOY_GATEWAY_CERT": pki.ServerCert, "DEPLOY_GATEWAY_KEY": pki.ServerKey, "DEPLOY_GATEWAY_CLIENT_CA": pki.CA,
		"DEPLOY_GATEWAY_PEER_PINS": gateway.Pin(cert.Leaf), "DEPLOY_GATEWAY_DELEGATE_PINS": strings.Join(m.cfg.Gateway.PeerPins, ","), "DEPLOY_GATEWAY_AUTHORITIES": strings.Join(a.Hostnames(), ","),
	}
	old, ok := m.data.Apps[a.ID]
	if !ok {
		return a, errors.New("application unavailable")
	}
	next := old
	next.Env = maps.Clone(old.Env)
	next.EnvKinds = maps.Clone(old.EnvKinds)
	next.EnvSources = maps.Clone(old.EnvSources)
	if next.Env == nil {
		next.Env = map[string]string{}
	}
	if next.EnvKinds == nil {
		next.EnvKinds = map[string]string{}
	}
	if next.EnvSources == nil {
		next.EnvSources = map[string]string{}
	}
	for k, v := range values {
		next.Env[k] = v
		next.EnvKinds[k] = EnvironmentCredential
		next.EnvSources[k] = "agent-gateway"
	}
	if len(next.Env) > 100 {
		return a, errors.New("gateway-managed environment exceeds application limit")
	}
	m.data.GatewayPKI[id] = pki
	m.data.Apps[a.ID] = next
	if err := m.save(); err != nil {
		m.data.Apps[a.ID] = old
		if !exists {
			delete(m.data.GatewayPKI, id)
		} else {
			m.data.GatewayPKI[id] = oldPKI
		}
		return a, err
	}
	if renewed {
		m.gatewayMu.Lock()
		if t := m.gatewayClients[id]; t != nil {
			t.CloseIdleConnections()
			delete(m.gatewayClients, id)
		}
		m.gatewayMu.Unlock()
	}
	a.Env = maps.Clone(next.Env)
	a.EnvKinds = maps.Clone(next.EnvKinds)
	a.EnvSources = maps.Clone(next.EnvSources)
	return a, nil
}
func (d *Docker) probeGateway(ctx context.Context, r *Release) error {
	if r == nil || r.Stack == nil || r.Stack.Gateway == nil {
		return nil
	}
	if d.gatewayClient == nil {
		return errors.New("protected app client is unavailable")
	}
	client, err := d.gatewayClient(r.Stack.Gateway.Identity)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://127.0.0.1:"+r.Stack.Gateway.Port+"/healthz", nil)
	if err != nil {
		return err
	}
	req.Host = gatewayAppName
	res, err := client.Do(req)
	if err != nil {
		return errors.New("private app mTLS health unavailable; verify the released framework private listener is enabled")
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("private app mTLS health returned HTTP %d", res.StatusCode)
	}
	return nil
}
