package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/platform/state"
	"github.com/agim/lidza/pkg/credentials"
)

func ingressConfig(m *Manager) Config {
	cfg := m.cfg
	cfg.IngressOnly = true
	cfg.Listen = "127.0.0.1:19091"
	cfg.ProxyListen = "127.0.0.1:18081"
	return cfg
}

func saveIngressFixture(t *testing.T, m *Manager, next diskState) {
	t.Helper()
	plain, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := credentials.Encrypt(m.key, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Save(m.path(), sealed); err != nil {
		t.Fatal(err)
	}
}

func awaitIngress(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("ingress snapshot did not converge")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIngressReaderSurvivesManagerShutdownAndReloads(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/socket" {
			io.WriteString(w, "first backend")
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test-echo\r\n\r\n")
		rw.Flush()
		io.Copy(conn, rw)
	}))
	defer backend.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	writer := testManager(t, &fakeRuntime{})
	a := testApp("ingress")
	a.Current = &Release{ID: "first", Port: port}
	first := diskState{Apps: map[string]App{a.ID: a}}
	saveIngressFixture(t, writer, first)
	before, _ := os.ReadFile(writer.path())
	keyBefore, _ := os.ReadFile(filepath.Join(writer.cfg.DataDir, credentials.MasterKeyFile))
	reader, err := NewIngressReader(context.Background(), ingressConfig(writer))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	proxy := httptest.NewServer(Proxy(reader))
	defer proxy.Close()
	request := func() (int, string) {
		req, _ := http.NewRequest("GET", proxy.URL+"/", nil)
		req.Host = a.Domain
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}
	if code, body := request(); code != 200 || body != "first backend" {
		t.Fatal(code, body)
	}
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxy.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "GET /socket HTTP/1.1\r\nHost: "+a.Domain+"\r\nConnection: Upgrade\r\nUpgrade: test-echo\r\n\r\n")
	rw := bufio.NewReader(conn)
	response, err := http.ReadResponse(rw, nil)
	if err != nil || response.StatusCode != 101 {
		t.Fatal("upgrade failed", response, err)
	}
	writer.Close()
	io.WriteString(conn, "alive")
	var echo [5]byte
	if _, err := io.ReadFull(rw, echo[:]); err != nil || string(echo[:]) != "alive" {
		t.Fatal("stream interrupted", err)
	}
	if code, body := request(); code != 200 || body != "first backend" {
		t.Fatal("management shutdown interrupted routing", code, body)
	}
	if err := reader.save(); err == nil {
		t.Fatal("reader wrote deployment inventory")
	}
	after, _ := os.ReadFile(writer.path())
	keyAfter, _ := os.ReadFile(filepath.Join(writer.cfg.DataDir, credentials.MasterKeyFile))
	if string(before) != string(after) || string(keyBefore) != string(keyAfter) {
		t.Fatal("reader mutated retained state or encryption key")
	}

	secondBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "second backend") }))
	defer secondBackend.Close()
	_, port, _ = net.SplitHostPort(strings.TrimPrefix(secondBackend.URL, "http://"))
	a.Current = &Release{ID: "second", Port: port}
	second := diskState{Apps: map[string]App{a.ID: a}}
	saveIngressFixture(t, writer, second)
	awaitIngress(t, func() bool { code, body := request(); return code == 200 && body == "second backend" })
	// An established upgraded connection remains bound to its original backend.
	io.WriteString(conn, "still")
	if _, err := io.ReadFull(rw, echo[:]); err != nil || string(echo[:]) != "still" {
		t.Fatal("snapshot reload interrupted stream", err)
	}
	if err := state.Save(writer.path(), "corrupt ciphertext"); err != nil {
		t.Fatal(err)
	}
	awaitIngress(t, func() bool { return !reader.ingressAvailable() })
	if code, _ := request(); code != 503 {
		t.Fatal("invalid snapshot failed open", code)
	}
	for _, path := range []string{"/health", "/tls/allow?domain=" + a.Domain} {
		r := httptest.NewRecorder()
		Handler(reader).ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 503 {
			t.Fatal("unavailable reader reported healthy or authorized certificate", path, r.Code)
		}
	}
	saveIngressFixture(t, writer, second)
	awaitIngress(t, func() bool { return reader.ingressAvailable() })
	if code, body := request(); code != 200 || body != "second backend" {
		t.Fatal("recovery failed", code, body)
	}
}

func TestIngressReaderRejectsMissingAndInvalidRetainedState(t *testing.T) {
	t.Setenv(credentials.EnvMasterKey, "")
	t.Setenv(credentials.EnvKeyOff, "")
	empty := Config{IngressOnly: true, DataDir: filepath.Join(t.TempDir(), "absent"), Listen: "127.0.0.1:19091", ProxyListen: "127.0.0.1:18081"}
	if _, err := NewIngressReader(context.Background(), empty); err == nil {
		t.Fatal("missing state accepted")
	}
	if _, err := os.Stat(empty.DataDir); !os.IsNotExist(err) {
		t.Fatal("reader created state directory")
	}
	writer := testManager(t, &fakeRuntime{})
	for _, invalid := range []any{diskState{}, diskState{Apps: map[string]App{"wrong": testApp("actual")}}, diskState{Apps: map[string]App{"invalid": {ID: "invalid", Domain: "bad"}}}} {
		plain, _ := json.Marshal(invalid)
		sealed, _ := credentials.Encrypt(writer.key, plain)
		if err := state.Save(writer.path(), sealed); err != nil {
			t.Fatal(err)
		}
		if m, err := NewIngressReader(context.Background(), ingressConfig(writer)); err == nil {
			m.Close()
			t.Fatal("invalid encrypted snapshot accepted")
		}
	}
	if err := state.Save(writer.path(), diskState{Apps: map[string]App{}}); err != nil {
		t.Fatal(err)
	}
	if m, err := NewIngressReader(context.Background(), ingressConfig(writer)); err == nil {
		m.Close()
		t.Fatal("unauthenticated plaintext accepted")
	}
}

func TestIngressReaderRestrictsListenersAndAPI(t *testing.T) {
	for _, address := range []string{"localhost:9000", ":9000", "0.0.0.0:9000", "127.0.0.1.evil:9000", "127.0.0.1:0", "127.0.0.1:65536", "[::]:9000"} {
		if validateIngressListen(address) {
			t.Fatal("unsafe listener accepted", address)
		}
	}
	for _, address := range []string{"127.0.0.1:9091", "[::1]:9091"} {
		if !validateIngressListen(address) {
			t.Fatal(address)
		}
	}
	writer := testManager(t, &fakeRuntime{})
	a := testApp("ingress")
	saveIngressFixture(t, writer, diskState{Apps: map[string]App{a.ID: a}})
	cfg := ingressConfig(writer)
	if m, err := NewManager(context.Background(), cfg, &fakeRuntime{}); err == nil {
		m.Close()
		t.Fatal("reader used deployment-manager constructor")
	}
	reader, err := NewIngressReader(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, path := range []string{"/v1/apps", "/v1/apps/ingress/deploy", "/metrics"} {
		r := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		Handler(reader).ServeHTTP(r, req)
		if r.Code != 404 {
			t.Fatal("management surface exposed", path, r.Code)
		}
	}
	for _, remote := range []string{"127.0.0.1:30000", "192.0.2.1:30000"} {
		r := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/tls/allow?domain="+a.Domain, nil)
		req.RemoteAddr = remote
		Handler(reader).ServeHTTP(r, req)
		expected := 403
		if strings.HasPrefix(remote, "127.") {
			expected = 200
		}
		if r.Code != expected {
			t.Fatal("certificate permission changed", remote, r.Code)
		}
	}
}
