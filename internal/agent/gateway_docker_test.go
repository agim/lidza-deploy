package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
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

func TestDockerProtectedStackLifecycle(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for real protected stack lifecycle")
	}
	if os.Getenv("LIDZA_TEST_CADDY") == "" {
		t.Fatal("protected stack acceptance requires LIDZA_TEST_CADDY")
	}
	t.Setenv("TEST_STACK_GATEWAY", "1")
	TestDockerStackV2Lifecycle(t)
}
func assertDockerGateway(t *testing.T, m *Manager, id string) {
	t.Helper()
	m.mu.Lock()
	a := m.data.Apps[id]
	m.mu.Unlock()
	if a.Current == nil || a.Current.Stack.Gateway == nil {
		t.Fatal("missing private gateway release metadata")
	}
	if err := (&Docker{gatewayClient: m.gatewayClient}).probeGateway(context.Background(), a.Current); err != nil {
		t.Fatal("container mTLS readiness", err)
	}
	raw, err := command(context.Background(), "", nil, "docker", "port", a.Current.Container, "3001/tcp")
	if err != nil || raw != "127.0.0.1:"+a.Current.Stack.Gateway.Port {
		t.Fatal("private TLS publication was not loopback-only", raw, err)
	}
	ingress := m.GatewayServer()
	relay := httptest.NewUnstartedServer(ingress.Handler)
	relay.TLS = ingress.TLSConfig
	relay.StartTLS()
	defer relay.Close()
	directory := t.TempDir()
	publicPKI, err := newGatewayPKI(a.Domain)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cert, _ := decodeGatewayPEM(publicPKI.ServerCert)
	key, _ := decodeGatewayPEM(publicPKI.ServerKey)
	certPath := write("public.pem", cert)
	keyPath := write("public-key.pem", key)
	snippet, err := os.ReadFile(filepath.Join(filepath.Dir(m.cfg.Gateway.Certificate), "Caddyfile.gateway"))
	if err != nil {
		t.Fatal(err)
	}
	snippet = []byte(strings.Replace(string(snippet), "https://127.0.0.1:8444", relay.URL, 1))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, port, _ := net.SplitHostPort(addr)
	config := fmt.Sprintf("{\n admin off\n auto_https off\n}\nhttps://%s:%s {\n bind 127.0.0.1\n tls %s %s\n%s\n}\n", a.Domain, port, certPath, keyPath, snippet)
	file := write("Caddyfile", []byte(config))
	log, err := os.Create(filepath.Join(directory, "caddy.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, os.Getenv("LIDZA_TEST_CADDY"), "run", "--config", file, "--adapter", "caddyfile")
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(directory, "config"), "XDG_DATA_HOME="+filepath.Join(directory, "data"))
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
			b, _ := os.ReadFile(filepath.Join(directory, "caddy.log"))
			t.Fatalf("real TLS edge did not start: %s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	roots, err := gatewayRoots(publicPKI.CA)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: a.Domain, MinVersion: tls.VersionTLS13}}
	defer transport.CloseIdleConnections()
	req, _ := http.NewRequest("POST", "https://"+addr+"/gateway-protected", strings.NewReader("durable stack gateway"))
	req.Host = a.Domain
	res, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "gateway-accepted:durable stack gateway" {
		t.Fatalf("actual edge -> agent -> Docker app protected request: %d %s", res.StatusCode, body)
	}
	t.Log("real TLS edge -> authenticated agent -> Docker private app endpoint passed with preserved per-app identity")
}
