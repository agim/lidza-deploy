package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// autoGatewayHost points the gateway's host access at temporary
// directories and fakes: a caddy group, recorded chowns, and 8444 taken.
type autoGatewayHost struct {
	mu     sync.Mutex
	chowns []string
	root   string
}

func newAutoGatewayHost(t *testing.T) *autoGatewayHost {
	t.Helper()
	h := &autoGatewayHost{root: t.TempDir()}
	// Caddy's account must traverse the directories up to its copies.
	for _, dir := range []string{filepath.Dir(h.root), h.root} {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	edgeRoot, command, group, owner, chown, listen := gatewayEdgeRoot, gatewayCommand, gatewayLookupGroup, gatewayLookupUser, gatewayChown, gatewayListen
	t.Cleanup(func() {
		gatewayEdgeRoot, gatewayCommand, gatewayLookupGroup, gatewayLookupUser, gatewayChown, gatewayListen = edgeRoot, command, group, owner, chown, listen
	})
	gatewayEdgeRoot = filepath.Join(h.root, "etc-lidza")
	gatewayLookupGroup = func(name string) (int, error) {
		if name != "caddy" {
			return 0, errors.New("unknown group")
		}
		return 997, nil
	}
	gatewayCommand = func(context.Context, string, ...string) (string, error) {
		return "", errors.New("unexpected command")
	}
	gatewayChown = func(path string, uid, gid int) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		rel, _ := filepath.Rel(h.root, path)
		h.chowns = append(h.chowns, rel+":"+strconv.Itoa(uid)+":"+strconv.Itoa(gid))
		return nil
	}
	gatewayListen = func(address string) (net.Listener, error) {
		if address == gatewayDefaultListen {
			return nil, errors.New("address already in use")
		}
		return net.Listen("tcp", address)
	}
	return h
}

// autoGatewayManager is an agent without a configured gateway whose host
// Caddy is the fake admin API.
func autoGatewayManager(t *testing.T, h *autoGatewayHost, admin *fakeCaddyAdmin) (*Manager, App, *StackManifest) {
	t.Helper()
	m := testManager(t, &fakeRuntime{})
	m.cfg.nativeConfigPath = filepath.Join(h.root, "etc-lidza-agent", "config.json")
	m.cfg.ProxyListen = "127.0.0.1:18081"
	if admin != nil {
		m.data.HostIngress = &HostIngressState{Caddy: &NativeCaddyConfig{Admin: admin.URL(), Server: "https"}}
	}
	a := testApp("protected-app")
	a.RuntimeProfile = "minimum"
	a.AdditionalDomains = []string{"www.protected-app.example.com"}
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	m.mu.Unlock()
	manifest := fixtureStackManifest()
	manifest.Version = 2
	manifest.GatewayTransport = &StackGateway{Version: 1, Paths: []string{"/protected"}}
	return m, a, &manifest
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestGatewayAutoSetupStartsListenerAndPublishesProtectedRoute(t *testing.T) {
	h := newAutoGatewayHost(t)
	admin := newFakeCaddyAdmin(t, operatorCaddyConfig())
	m, a, manifest := autoGatewayManager(t, h, admin)
	ctx := context.Background()
	if m.gatewayReady() || m.gatewayCapability(ctx) != "auto" {
		t.Fatalf("before setup: ready %v capability %q", m.gatewayReady(), m.gatewayCapability(ctx))
	}
	next, err := m.prepareGateway(ctx, a, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if next.Env["DEPLOY_GATEWAY_DELEGATE_PINS"] == "" {
		t.Fatal("the app was not given the edge pins")
	}
	m.mu.Lock()
	state := *m.data.HostGateway
	jobs := m.hostUpdatesStateLocked().Jobs
	m.mu.Unlock()

	// Credentials: a new root-only directory next to the configuration.
	if filepath.Dir(state.Directory) != filepath.Join(h.root, "etc-lidza-agent") || !strings.HasPrefix(filepath.Base(state.Directory), "gateway-") {
		t.Fatalf("credential directory %s", state.Directory)
	}
	if mode(t, state.Directory) != 0700 || mode(t, filepath.Join(state.Directory, "agent-key.pem")) != 0600 || mode(t, filepath.Join(state.Directory, "ca-key.pem")) != 0600 {
		t.Fatal("credentials are not root-only")
	}
	// Caddy's copies: root:caddy 0750 / 0640.
	if state.EdgeDirectory != filepath.Join(gatewayEdgeRoot, filepath.Base(state.Directory)) || mode(t, state.EdgeDirectory) != 0750 || state.EdgeOwner != "root:caddy" {
		t.Fatalf("edge directory %s %o %s", state.EdgeDirectory, mode(t, state.EdgeDirectory), state.EdgeOwner)
	}
	for _, file := range []string{state.EdgeCA, state.EdgeCertificate, state.EdgeKey} {
		if filepath.Dir(file) != state.EdgeDirectory || mode(t, file) != 0640 {
			t.Fatalf("edge file %s %o", file, mode(t, file))
		}
	}
	if entries, _ := os.ReadDir(state.EdgeDirectory); len(entries) != 3 {
		t.Fatalf("the web server's directory holds more than its three files: %v", entries)
	}
	h.mu.Lock()
	chowns := strings.Join(h.chowns, " ")
	h.mu.Unlock()
	if strings.Count(chowns, ":0:997") != 4 {
		t.Fatalf("chowns %s", chowns)
	}
	// 8444 was taken: a free loopback port.
	if state.Config.Listen == gatewayDefaultListen || !strings.HasPrefix(state.Config.Listen, "127.0.0.1:") || m.gatewayListenAddress() != state.Config.Listen {
		t.Fatalf("listen %s", state.Config.Listen)
	}
	if len(jobs) == 0 || jobs[0].Kind != gatewaySetupKind || jobs[0].Status != "succeeded" || jobs[0].Trigger != "deploy" || !strings.Contains(jobs[0].Output, state.EdgeDirectory) {
		t.Fatalf("host job %+v", jobs)
	}

	// The listener runs in-process and accepts Caddy's client certificate.
	client := edgeClient(t, state)
	res, err := client.Get("https://" + state.Config.Listen + "/")
	if err != nil {
		t.Fatalf("gateway listener: %v", err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	// Without the edge certificate the handshake fails.
	plain := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if res, err := plain.Get("https://" + state.Config.Listen + "/"); err == nil {
		res.Body.Close()
		t.Fatal("gateway accepted a client without the edge certificate")
	}

	// The Caddy route sends the hostnames to the gateway over mTLS.
	route, _ := admin.at("/config/apps/http/servers/https/routes/0").(map[string]any)
	if route["@id"] != "lidza-app-protected-app" {
		t.Fatalf("route %v", admin.at("/config/apps/http/servers/https/routes"))
	}
	raw, _ := json.Marshal(route)
	for _, want := range []string{`"dial":"` + state.Config.Listen + `"`, `"server_name":"lidza-gateway.internal"`, `"root_ca_pem_files":["` + state.EdgeCA + `"]`,
		`"client_certificate_file":"` + state.EdgeCertificate + `"`, `"client_certificate_key_file":"` + state.EdgeKey + `"`, `"Lidza-Gateway-Transport":["edge-headers-v1"]`,
		`"Lidza-Gateway-Nonce":["{http.request.uuid}"]`, `"protocol":"http"`, `"host":["protected-app.example.com","www.protected-app.example.com"]`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("route lacks %s: %s", want, raw)
		}
	}
	if got := admin.strings(subjectsPath); !reflect.DeepEqual(got, []string{"ops.example.com", "protected-app.example.com", "www.protected-app.example.com"}) {
		t.Fatalf("TLS subjects %v", got)
	}
	if m.gatewayCapability(ctx) != "configured" {
		t.Fatal("a recorded gateway is reported as configured")
	}

	// Idempotent: the next deployment reuses the identity and the route.
	admin.reset()
	if _, err := m.prepareGateway(ctx, next, manifest); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	again := *m.data.HostGateway
	jobCount := len(m.hostUpdatesStateLocked().Jobs)
	m.mu.Unlock()
	if !reflect.DeepEqual(again, state) || jobCount != len(jobs) {
		t.Fatalf("gateway regenerated or setup repeated: %+v", again)
	}
	if admin.called("PATCH /id/") != 0 || admin.called("PUT ") != 0 {
		t.Fatal("an unchanged route was written again")
	}
	if entries, _ := os.ReadDir(filepath.Dir(state.Directory)); len(entries) != 1 {
		t.Fatalf("another credential directory was created: %v", entries)
	}

	// A restarted agent serves the recorded gateway on the same port.
	cfg := m.cfg
	m.Close()
	restarted, err := NewManager(context.Background(), cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if !restarted.gatewayReady() || restarted.gatewayListenAddress() != state.Config.Listen || restarted.GatewayServer() != nil {
		t.Fatal("the recorded gateway did not start with the agent")
	}
	if res, err := edgeClient(t, state).Get("https://" + state.Config.Listen + "/"); err != nil {
		t.Fatalf("restarted gateway listener: %v", err)
	} else {
		res.Body.Close()
	}

	// Removing the application withdraws its route.
	restarted.mu.Lock()
	app := restarted.data.Apps[a.ID]
	app.Current = &Release{Stack: &StackRelease{Gateway: &GatewayRelease{Identity: app.resourceID()}, Manifest: *manifest}}
	restarted.mu.Unlock()
	if err := restarted.withdrawGatewayRoute(ctx, app); err != nil {
		t.Fatal(err)
	}
	if route, _ := admin.at("/config/apps/http/servers/https/routes/0").(map[string]any); route["@id"] == "lidza-app-protected-app" {
		t.Fatal("the route stayed after removal")
	}
}

func edgeClient(t *testing.T, state HostGatewayState) *http.Client {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(state.EdgeCertificate, state.EdgeKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(state.EdgeCA)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: gatewayServerName, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}}}
}

func TestGatewayAutoNeedsAManagedIngress(t *testing.T) {
	h := newAutoGatewayHost(t)
	m, a, manifest := autoGatewayManager(t, h, nil)
	_, err := m.prepareGateway(context.Background(), a, manifest)
	if !errors.Is(err, errGatewayIngressMissing) {
		t.Fatalf("error %v", err)
	}
	for _, want := range []string{"no gateway section", "Caddy admin API", "127.0.0.1:2019", "-gateway-transport prepare"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not say %q: %v", want, err)
		}
	}
	m.mu.Lock()
	provisioned, jobs := m.data.HostGateway != nil, len(m.hostUpdatesStateLocked().Jobs)
	m.mu.Unlock()
	if provisioned || jobs != 0 {
		t.Fatal("a gateway was set up without an ingress to publish it")
	}
	if entries, _ := os.ReadDir(filepath.Join(h.root, "etc-lidza-agent")); len(entries) != 0 {
		t.Fatal("credentials were written")
	}
	if m.gatewayCapability(context.Background()) != "unavailable" {
		t.Fatal("capability")
	}
}

func TestGatewayAutoEdgeOwnerFollowsTheCaddyService(t *testing.T) {
	newAutoGatewayHost(t)
	gatewayLookupGroup = func(string) (int, error) { return 0, errors.New("no caddy group") }
	var asked []string
	gatewayCommand = func(_ context.Context, name string, args ...string) (string, error) {
		asked = append(asked, name+" "+strings.Join(args, " "))
		return "www-data\n", nil
	}
	gatewayLookupUser = func(name string) (int, int, error) {
		if name != "www-data" {
			return 0, 0, errors.New("unknown user")
		}
		return 33, 33, nil
	}
	access, err := gatewayEdgeOwner(context.Background())
	if err != nil || access.uid != 33 || access.gid != 33 || access.dirMode != 0700 || access.fileMode != 0600 || access.owner != "www-data" {
		t.Fatalf("%+v %v", access, err)
	}
	if len(asked) != 1 || asked[0] != "systemctl show caddy -p User --value" {
		t.Fatalf("commands %v", asked)
	}
	// A service without User= runs as root.
	gatewayCommand = func(context.Context, string, ...string) (string, error) { return "\n", nil }
	user := caddyProcessUser
	caddyProcessUser = func() string { return "" }
	defer func() { caddyProcessUser = user }()
	if access, err := gatewayEdgeOwner(context.Background()); err != nil || access.uid != 0 || access.gid != 0 || access.owner != "root" {
		t.Fatalf("%+v %v", access, err)
	}
}

func TestGatewayConfiguredHostKeepsItsReviewedCaddyConfiguration(t *testing.T) {
	admin := newFakeCaddyAdmin(t, operatorCaddyConfig())
	m, a, manifest := gatewayTestManager(t)
	m.data.HostIngress = &HostIngressState{Caddy: &NativeCaddyConfig{Admin: admin.URL(), Server: "https"}}
	admin.reset()
	if _, err := m.prepareGateway(context.Background(), a, manifest); err != nil {
		t.Fatal(err)
	}
	if admin.called("PUT ") != 0 || admin.called("PATCH ") != 0 || admin.called("POST ") != 0 {
		t.Fatal("a configured gateway's web server was changed")
	}
	if m.data.HostGateway != nil || m.gatewayCapability(context.Background()) != "configured" {
		t.Fatal("a configured host got an automatic gateway")
	}
}

func TestIngressProtectedRouteRendersTheGatewayTransport(t *testing.T) {
	state := HostGatewayState{Config: GatewayConfig{Listen: "127.0.0.1:8444"}, EdgeCA: "/etc/lidza/gateway-20261106/ca.pem", EdgeCertificate: "/etc/lidza/gateway-20261106/edge.pem", EdgeKey: "/etc/lidza/gateway-20261106/edge-key.pem"}
	raw, err := renderCaddyRoute("shop", []string{"shop.example.com"}, gatewayCaddyProxy(state))
	if err != nil {
		t.Fatal(err)
	}
	var route struct {
		Handle []struct {
			Routes []struct {
				Handle []map[string]any `json:"handle"`
			} `json:"routes"`
		} `json:"handle"`
	}
	if err := json.Unmarshal(raw, &route); err != nil {
		t.Fatal(err)
	}
	proxy := route.Handle[0].Routes[0].Handle[1]
	want := map[string]any{
		"handler":   "reverse_proxy",
		"upstreams": []any{map[string]any{"dial": "127.0.0.1:8444"}},
		"headers": map[string]any{"request": map[string]any{"set": map[string]any{
			"Host":                      []any{"{http.request.hostport}"},
			"Lidza-Gateway-Transport":   []any{"edge-headers-v1"},
			"Lidza-Gateway-Tls-Version": []any{"{http.request.tls.version}"},
			"Lidza-Gateway-Authority":   []any{"{http.request.hostport}"},
			"Lidza-Gateway-Server-Name": []any{"{http.request.tls.server_name}"},
			"Lidza-Gateway-Method":      []any{"{http.request.method}"},
			"Lidza-Gateway-Target":      []any{"{http.request.uri}"},
			"Lidza-Gateway-Issued":      []any{"{time.now.unix}"},
			"Lidza-Gateway-Nonce":       []any{"{http.request.uuid}"},
		}}},
		"transport": map[string]any{"protocol": "http", "tls": map[string]any{
			"server_name":                 "lidza-gateway.internal",
			"root_ca_pem_files":           []any{state.EdgeCA},
			"client_certificate_file":     state.EdgeCertificate,
			"client_certificate_key_file": state.EdgeKey,
		}},
	}
	if !reflect.DeepEqual(proxy, want) {
		got, _ := json.MarshalIndent(proxy, "", " ")
		t.Fatalf("protected proxy handler:\n%s", got)
	}
}
