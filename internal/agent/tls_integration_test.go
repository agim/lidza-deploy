package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaddyDomainCertificates(t *testing.T) {
	if os.Getenv("TEST_CADDY") != "1" {
		t.Skip("set TEST_CADDY=1 for real local certificate issuance")
	}
	m := testManager(t, &fakeRuntime{})
	for _, id := range []string{"cert-one", "cert-two"} {
		if err := m.Upsert(testApp(id)); err != nil {
			t.Fatal(err)
		}
	}
	api := httptest.NewServer(Handler(m))
	defer api.Close()
	free := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		return p
	}
	port, httpPort := free(), free()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	name := "lidza-caddy-test-" + newID()
	run := func(args ...string) []byte {
		t.Helper()
		c := exec.CommandContext(ctx, "docker", args...)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v %s", args, err, out)
		}
		return out
	}
	// A local CA tests issuance and hostname verification without consuming public ACME limits.
	proofProxy := httptest.NewServer(Proxy(m))
	defer proofProxy.Close()
	config := fmt.Sprintf("{\n admin off\n https_port %d\n http_port %d\n on_demand_tls {\n ask %s/tls/allow\n }\n}\nhttps:// {\n tls internal {\n on_demand\n }\n respond \"certificate works\" 200\n}\n", port, httpPort, api.URL)
	config += fmt.Sprintf("http:// {\n handle /.well-known/lidza-deploy-host {\n reverse_proxy %s\n }\n handle {\n redir https://{host}{uri} permanent\n }\n}\n", strings.TrimPrefix(proofProxy.URL, "http://"))
	dir := t.TempDir()
	file := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(file, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	run("create", "--name", name, "--network", "host", "caddy:2.10.2-alpine")
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(c, "docker", "rm", "-f", name).Run()
	})
	run("cp", file, name+":/etc/caddy/Caddyfile")
	run("start", name)
	root := filepath.Join(dir, "root.crt")
	ready := false
	for i := 0; i < 50; i++ {
		c := exec.CommandContext(ctx, "docker", "cp", name+":/data/caddy/pki/authorities/local/root.crt", root)
		if c.Run() == nil {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("Caddy CA not ready")
	}
	cert, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cert) {
		t.Fatal("invalid CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", port))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	for _, host := range []string{"cert-one.example.com", "cert-two.example.com"} {
		lookup := func(context.Context, string, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		if err := checkDomainRoute(ctx, host, m.domainProof(host), lookup, fmt.Sprint(httpPort)); err != nil {
			t.Fatal("Caddy HTTP verification route", err)
		}
		if err := verifyDomainCertificate(ctx, host, fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			t.Fatal("proactive certificate request", err)
		}
		res, err := client.Get(fmt.Sprintf("https://%s:%d/", host, port))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 || res.TLS == nil || len(res.TLS.VerifiedChains) == 0 {
			t.Fatal("certificate did not verify")
		}
		if err = res.TLS.PeerCertificates[0].VerifyHostname(host); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := client.Get(fmt.Sprintf("https://unregistered.example.com:%d/", port)); err == nil {
		res.Body.Close()
		t.Fatal("unregistered hostname received a certificate")
	}
}
