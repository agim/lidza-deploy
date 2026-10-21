package agent

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/platform/state"
	"github.com/agim/lidza/pkg/credentials"
	"github.com/agim/lidza/pkg/gateway"
)

func gatewayTestConfig(t *testing.T) GatewayConfig {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "prepared")
	if err := PrepareGateway(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "gateway-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Gateway GatewayConfig `json:"gateway"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out.Gateway
}
func gatewayTestManager(t *testing.T) (*Manager, App, *StackManifest) {
	t.Helper()
	m := testManager(t, &fakeRuntime{})
	cfg := gatewayTestConfig(t)
	m.cfg.Gateway = &cfg
	if err := m.initGateway(); err != nil {
		t.Fatal(err)
	}
	a := testApp("gateway-app")
	a.RuntimeProfile = "minimum"
	a.Env["MAIL_PROVIDER"] = "saved-mail"
	a.Env["LLM_PROVIDER"] = "saved-ai"
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	m.mu.Unlock()
	manifest := fixtureStackManifest()
	manifest.Version = 2
	manifest.GatewayTransport = &StackGateway{Version: 1, Paths: []string{"/protected"}}
	next, err := m.prepareGateway(a, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	return m, next, &manifest
}
func TestGatewayPersistencePrivacyAndSettings(t *testing.T) {
	m, a, manifest := gatewayTestManager(t)
	first := m.data.GatewayPKI[a.resourceID()]
	next, err := m.prepareGateway(a, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if m.data.GatewayPKI[a.resourceID()] != first {
		t.Fatal("stable gateway identity regenerated")
	}
	if next.Env["MAIL_PROVIDER"] != "saved-mail" || next.Env["LLM_PROVIDER"] != "saved-ai" {
		t.Fatal("saved providers replaced")
	}
	settings, err := m.Settings(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.GatewayHostConfigured || !settings.GatewayTransportSupported {
		t.Fatal("missing gateway capability metadata")
	}
	for _, entry := range settings.Environment {
		if strings.HasPrefix(entry.Key, gatewayEnvPrefix) {
			t.Fatal("managed gateway field exposed as editable application configuration")
		}
	}
	null := (*string)(nil)
	if err := m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, EnvChanges: map[string]*string{"DEPLOY_GATEWAY_KEY": null}}); err == nil {
		t.Fatal("managed gateway key deletion accepted")
	}
	disk, err := os.ReadFile(m.path())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{first.CAKey, first.ClientKey, first.ServerKey} {
		if strings.Contains(string(disk), secret) {
			t.Fatal("unencrypted gateway key persisted")
		}
	}
	keyPEM, _ := decodeGatewayPEM(first.ServerKey)
	block, _ := pem.Decode(keyPEM)
	line := strings.Split(string(keyPEM), "\n")[1]
	raw := first.ServerKey + " " + string(keyPEM) + " " + strings.ReplaceAll(string(keyPEM), "\n", `\n`) + " " + base64.StdEncoding.EncodeToString(block.Bytes) + " " + line
	scrubbed := scrubOutput(raw, outputSecrets(next.Env))
	if strings.Contains(scrubbed, line) || strings.Contains(scrubbed, first.ServerKey) {
		t.Fatal("private key variant leaked in diagnostics")
	}
	plain, err := credentials.Decrypt(m.key, func() string { var s string; _ = json.Unmarshal(disk, &s); return s }())
	if err != nil {
		t.Fatal(err)
	}
	var saved diskState
	if json.Unmarshal(plain, &saved) != nil || saved.GatewayPKI[a.resourceID()] != first {
		t.Fatal("gateway identity missing from authenticated state")
	}
	a.Current = &Release{Stack: &StackRelease{Gateway: &GatewayRelease{Identity: a.resourceID()}, Manifest: *manifest}}
	removed := *manifest
	removed.GatewayTransport = nil
	if _, err := m.prepareGateway(a, &removed); err == nil {
		t.Fatal("protected release downgraded to ordinary HTTP")
	}
	missing := *manifest
	missing.GatewayTransport = &StackGateway{Version: 1, Paths: []string{"/different"}}
	if _, err := m.prepareGateway(a, &missing); err == nil {
		t.Fatal("protected path removed in place")
	}
	delete(m.data.GatewayPKI, a.resourceID())
	if _, err := m.prepareGateway(a, manifest); err == nil {
		t.Fatal("missing live identity silently regenerated")
	}
}
func TestGatewayReplayJournal(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	twin := &Manager{cfg: m.cfg, key: append([]byte(nil), m.key...)}
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			manager := m
			if i%2 != 0 {
				manager = twin
			}
			fresh, err := manager.claimGatewayNonce(context.Background(), "gateway:same", 30*time.Second)
			if err != nil {
				t.Error(err)
			}
			if fresh {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("shared atomic claims accepted=%d", accepted.Load())
	}
	restarted := &Manager{cfg: m.cfg, key: append([]byte(nil), m.key...)}
	if fresh, err := restarted.claimGatewayNonce(context.Background(), "gateway:same", 30*time.Second); err != nil || fresh {
		t.Fatal("replay accepted after restart", err)
	}
	data, err := os.ReadFile(filepath.Join(m.cfg.DataDir, "gateway-nonces.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "gateway:same") {
		t.Fatal("nonce journal was not sealed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if fresh, err := m.claimGatewayNonce(ctx, "gateway:cancelled", time.Second); fresh || err == nil {
		t.Fatal("cancelled journal operation accepted")
	}
	if err := os.WriteFile(filepath.Join(m.cfg.DataDir, "gateway-nonces.json"), []byte(`"corrupt"`), 0600); err != nil {
		t.Fatal(err)
	}
	if fresh, err := m.claimGatewayNonce(context.Background(), "gateway:new", time.Second); fresh || err == nil {
		t.Fatal("corrupt journal failed open")
	}
}
func TestGatewayJournalCapacityAndExpiry(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	claims := map[string]int64{}
	for i := 0; i < 8192; i++ {
		claims[fmt.Sprintf("key-%d", i)] = time.Now().Add(time.Minute).UnixMilli()
	}
	save := func() {
		b, _ := json.Marshal(claims)
		sealed, err := credentials.Encrypt(m.key, b)
		if err != nil {
			t.Fatal(err)
		}
		if err := state.Save(filepath.Join(m.cfg.DataDir, "gateway-nonces.json"), sealed); err != nil {
			t.Fatal(err)
		}
	}
	save()
	if fresh, err := m.claimGatewayNonce(context.Background(), "gateway:overflow", time.Second); fresh || err == nil {
		t.Fatal("journal capacity failed open")
	}
	for k := range claims {
		claims[k] = time.Now().Add(-time.Second).UnixMilli()
	}
	save()
	if fresh, err := m.claimGatewayNonce(context.Background(), "gateway:after-expiry", time.Second); err != nil || !fresh {
		t.Fatal("expired journal not reclaimed", err)
	}
}
func TestGatewayManifestAndConfigurationValidation(t *testing.T) {
	for _, g := range []*StackGateway{{Version: 2, Paths: []string{"/protocol"}}, {Version: 1}, {Version: 1, Paths: []string{"/"}}, {Version: 1, Paths: []string{"/readyz"}}, {Version: 1, Paths: []string{"/a/../b"}}, {Version: 1, Paths: []string{"/protocol", "/protocol"}}} {
		if g.validate() == nil {
			t.Fatal("invalid gateway declaration accepted", g)
		}
	}
	manifest := fixtureStackManifest()
	manifest.GatewayTransport = &StackGateway{Version: 1, Paths: []string{"/rtc"}}
	if manifest.validate() == nil {
		t.Fatal("v1 gateway accepted")
	}
	manifest.Version = 2
	if manifest.validate() == nil {
		t.Fatal("ordinary route overlap accepted")
	}
	cfg := gatewayTestConfig(t)
	for _, listen := range []string{"0.0.0.0:8444", ":8444", "localhost:8444", "127.0.0.1:0"} {
		m := &Manager{cfg: Config{Gateway: &cfg}}
		m.cfg.Gateway.Listen = listen
		if m.initGateway() == nil {
			t.Fatal("non-private gateway listener accepted", listen)
		}
	}
}
func TestRealProtectedAgentGateway(t *testing.T) {
	binary := os.Getenv("LIDZA_TEST_CADDY")
	if binary == "" {
		t.Skip("set LIDZA_TEST_CADDY for real edge/agent/application mTLS acceptance")
	}
	m, a, manifest := gatewayTestManager(t)
	pki := m.data.GatewayPKI[a.resourceID()]
	serverCert, err := gatewayCertificate(pki.ServerCert, pki.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := gatewayRoots(pki.CA)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := gateway.ServerTLS(serverCert, roots)
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := gatewayCertificate(pki.ClientCert, pki.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	verify, err := gateway.Verify(gateway.Policy{Peers: []string{gateway.Pin(clientCert.Leaf)}, Delegates: m.cfg.Gateway.PeerPins, Authorize: func(_ context.Context, host string) error {
		if host != a.Domain {
			return fmt.Errorf("unapproved authority")
		}
		return nil
	}, Claim: m.claimGatewayNonce})
	if err != nil {
		t.Fatal(err)
	}
	var protocolCalls atomic.Int64
	privateMux := http.NewServeMux()
	privateMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	privateMux.Handle("/protected", verify(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !gateway.RequireTLS13(r) {
			http.Error(w, "TLS1.3 required", 403)
			return
		}
		e, _ := gateway.From(r.Context())
		if len(e.Gateways) != 2 || r.TLS.ServerName != gatewayAppName {
			t.Error("original/backend TLS evidence confused")
		}
		protocolCalls.Add(1)
		_, _ = io.Copy(w, r.Body)
	})))
	app := httptest.NewUnstartedServer(privateMux)
	app.TLS = serverTLS
	app.StartTLS()
	defer app.Close()
	_, appPort, _ := net.SplitHostPort(strings.TrimPrefix(app.URL, "https://"))
	legacyApp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(gateway.Header) != "" {
			t.Error("origin evidence leaked to plaintext backend")
		}
		_, _ = w.Write([]byte("ordinary internal HTTP"))
	}))
	defer legacyApp.Close()
	_, legacyPort, _ := net.SplitHostPort(strings.TrimPrefix(legacyApp.URL, "http://"))
	release := &Release{ID: "protected", Port: legacyPort, Stack: &StackRelease{Manifest: *manifest, Gateway: &GatewayRelease{Identity: a.resourceID(), Authority: a.Domain, Port: appPort}}}
	m.mu.Lock()
	saved := m.data.Apps[a.ID]
	saved.Current = release
	m.data.Apps[a.ID] = saved
	m.mu.Unlock()
	ingress := m.GatewayServer()
	relay := httptest.NewUnstartedServer(ingress.Handler)
	relay.TLS = ingress.TLSConfig
	relay.StartTLS()
	defer relay.Close()
	plain := httptest.NewServer(Proxy(m))
	defer plain.Close()
	req, _ := http.NewRequest("POST", plain.URL+"/protected", strings.NewReader("must not execute"))
	req.Host = a.Domain
	req.Header.Set(gateway.Header, "forged")
	req.Header.Set("X-Forwarded-Proto", "https")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("legacy ingress accepted protected request", res.StatusCode)
	}
	dir := filepath.Dir(m.cfg.Gateway.Certificate)
	snippet, err := os.ReadFile(filepath.Join(dir, "Caddyfile.gateway"))
	if err != nil {
		t.Fatal(err)
	}
	snippet = []byte(strings.Replace(string(snippet), "https://127.0.0.1:8444", relay.URL, 1))
	publicPKI, err := newGatewayPKI(a.Domain)
	if err != nil {
		t.Fatal(err)
	}
	pubCert, _ := decodeGatewayPEM(publicPKI.ServerCert)
	pubKey, _ := decodeGatewayPEM(publicPKI.ServerKey)
	certPath := filepath.Join(dir, "public.pem")
	keyPath := filepath.Join(dir, "public-key.pem")
	_ = os.WriteFile(certPath, pubCert, 0600)
	_ = os.WriteFile(keyPath, pubKey, 0600)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, port, _ := net.SplitHostPort(addr)
	config := fmt.Sprintf("{\n admin off\n auto_https off\n}\nhttps://%s:%s {\n bind 127.0.0.1\n tls %s %s\n%s\n}\n", a.Domain, port, certPath, keyPath, snippet)
	configPath := filepath.Join(dir, "Caddyfile.test")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "caddy.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary, "run", "--config", configPath, "--adapter", "caddyfile")
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_DATA_HOME="+filepath.Join(dir, "data"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	for deadline := time.Now().Add(5 * time.Second); ; {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real Caddy did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	publicRoots, err := gatewayRoots(publicPKI.CA)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []uint16{tls.VersionTLS13, tls.VersionTLS12} {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: publicRoots, ServerName: a.Domain, MinVersion: version, MaxVersion: version}, TLSHandshakeTimeout: time.Second}
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		req, _ := http.NewRequest("POST", "https://"+addr+"/protected?one=1", strings.NewReader("protected body"))
		req.Host = a.Domain
		req.Header.Set(gateway.Header, "client forged proof")
		for _, key := range gateway.EdgeHeaderNames() {
			req.Header.Set(key, "forged")
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		transport.CloseIdleConnections()
		expected := 200
		if version == tls.VersionTLS12 {
			expected = 403
		}
		if res.StatusCode != expected {
			logData, _ := os.ReadFile(filepath.Join(dir, "caddy.log"))
			t.Fatalf("TLS%x status=%d body=%s logs=%s", version, res.StatusCode, body, logData)
		}
		if version == tls.VersionTLS13 && string(body) != "protected body" {
			t.Fatal("request body changed")
		}
	}
	if protocolCalls.Load() != 1 {
		t.Fatal("invalid request reached protected application")
	}
	if err := (&Docker{gatewayClient: m.gatewayClient}).probeGateway(context.Background(), release); err != nil {
		t.Fatal("private TLS health probe", err)
	}
	t.Log("actual stock Caddy -> protected agent Proxy -> app mTLS passed; original TLS1.2 and forged legacy evidence denied; app received distinct original-client evidence and unchanged body")
}

func TestGatewayLeafRenewalPreservesKeysAndTrust(t *testing.T) {
	original, err := newGatewayPKI(gatewayAppName)
	if err != nil {
		t.Fatal(err)
	}
	shorten := func(encoded string) string {
		certPEM, _ := decodeGatewayPEM(encoded)
		certBlock, _ := pem.Decode(certPEM)
		cert, err := x509.ParseCertificate(certBlock.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		cert.NotAfter = time.Now().AddDate(0, 0, 14)
		caPEM, _ := decodeGatewayPEM(original.CA)
		caBlock, _ := pem.Decode(caPEM)
		ca, _ := x509.ParseCertificate(caBlock.Bytes)
		keyPEM, _ := decodeGatewayPEM(original.CAKey)
		keyBlock, _ := pem.Decode(keyPEM)
		signer, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, cert.PublicKey, signer)
		if err != nil {
			t.Fatal(err)
		}
		return encodeGatewayPEM("CERTIFICATE", der)
	}
	near := original
	near.ServerCert = shorten(original.ServerCert)
	near.ClientCert = shorten(original.ClientCert)
	renewed, changed, err := renewGatewayPKI(near)
	if err != nil || !changed {
		t.Fatal("certificate renewal", changed, err)
	}
	if renewed.CA != original.CA || renewed.CAKey != original.CAKey || renewed.ServerKey != original.ServerKey || renewed.ClientKey != original.ClientKey {
		t.Fatal("renewal replaced retained trust or keys")
	}
	if err := renewed.validate(); err != nil {
		t.Fatal(err)
	}
	before, _ := gatewayCertificate(original.ClientCert, original.ClientKey)
	after, _ := gatewayCertificate(renewed.ClientCert, renewed.ClientKey)
	if gateway.Pin(before.Leaf) != gateway.Pin(after.Leaf) {
		t.Fatal("renewal changed receiver pin")
	}
	corrupt := renewed
	other, err := newGatewayPKI(gatewayAppName)
	if err != nil {
		t.Fatal(err)
	}
	corrupt.CA = other.CA
	if corrupt.validate() == nil {
		t.Fatal("mismatched retained CA accepted")
	}
}
func TestGatewayRollbackCannotDowngradeTransport(t *testing.T) {
	spec := StackManifest{GatewayTransport: &StackGateway{Version: 1, Paths: []string{"/protocol"}}}
	a := App{Current: &Release{Stack: &StackRelease{Gateway: &GatewayRelease{}, Manifest: spec}}, Previous: &Release{}}
	if validateGatewayRollback(a) == nil {
		t.Fatal("protected to single-container rollback accepted")
	}
	a.Previous = &Release{Stack: &StackRelease{Manifest: StackManifest{}}}
	if validateGatewayRollback(a) == nil {
		t.Fatal("protected to plaintext stack rollback accepted")
	}
	a.Previous = &Release{Stack: &StackRelease{Gateway: &GatewayRelease{}, Manifest: spec}}
	if err := validateGatewayRollback(a); err != nil {
		t.Fatal("compatible protected rollback refused", err)
	}
}
