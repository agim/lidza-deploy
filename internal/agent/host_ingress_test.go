package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// stubHostIngress replaces the ingress probes: the listening web ports and
// the configuration each admin URL answers with (a missing URL fails as an
// unreachable admin API). An admin served by a fakeCaddyAdmin is read live.
func stubHostIngress(t *testing.T, ports []int, admins map[string]map[string]any) {
	t.Helper()
	oldListeners, oldGet := hostIngressListeners, hostIngressAdminGet
	t.Cleanup(func() { hostIngressListeners, hostIngressAdminGet = oldListeners, oldGet })
	hostIngressListeners = func() []int { return slices.Clone(ports) }
	hostIngressAdminGet = func(ctx context.Context, admin string) ([]byte, error) {
		if cfg, ok := admins[admin]; ok {
			return json.Marshal(cfg)
		}
		if strings.HasPrefix(admin, "http://127.0.0.1:") && !strings.HasPrefix(admin, hostIngressDefaultAdmin) {
			return caddyAdminConfig(ctx, admin) // a test server
		}
		return nil, errors.New("connection refused")
	}
}

func TestDetectHostIngressTellsTheAgentLayoutFromAnOperatorCaddy(t *testing.T) {
	ctx := context.Background()
	stubHostIngress(t, nil, nil)
	if got := detectHostIngress(ctx, hostIngressDefaultAdmin, "127.0.0.1:8081"); got.Mode != HostIngressAgent || got.AdminAPI || !strings.Contains(got.Detail, "no process listens") {
		t.Fatalf("bare host: %+v", got)
	}
	stubHostIngress(t, []int{80, 443}, nil)
	if got := detectHostIngress(ctx, hostIngressDefaultAdmin, "127.0.0.1:8081"); got.Mode != HostIngressAgent || got.AdminAPI || !strings.Contains(got.Detail, "no Caddy admin API") || !slices.Equal(got.Ports, []int{80, 443}) {
		t.Fatalf("web server without admin API: %+v", got)
	}
	// The installer's Caddy: every hostname to the agent proxy, on-demand TLS.
	stubHostIngress(t, []int{80, 443}, map[string]map[string]any{hostIngressDefaultAdmin: installerCaddyConfig("127.0.0.1:8081")})
	if got := detectHostIngress(ctx, hostIngressDefaultAdmin, "127.0.0.1:8081"); got.Mode != HostIngressAgent || !got.AdminAPI || got.Server != "srv0" || !strings.Contains(got.Detail, "on demand") {
		t.Fatalf("installer layout: %+v", got)
	}
	// The same layout forwarding to something else is not the agent's.
	if got := detectHostIngress(ctx, hostIngressDefaultAdmin, "127.0.0.1:18081"); got.Mode != HostIngressCaddy {
		t.Fatalf("foreign upstream: %+v", got)
	}
	// An operator-managed Caddy with host-matched routes; the HTTPS server is the one on :443.
	stubHostIngress(t, []int{80, 443}, map[string]map[string]any{hostIngressDefaultAdmin: operatorCaddyConfig()})
	if got := detectHostIngress(ctx, hostIngressDefaultAdmin, "127.0.0.1:8081"); got.Mode != HostIngressCaddy || !got.AdminAPI || got.Server != "https" {
		t.Fatalf("operator layout: %+v", got)
	}
	// A catch-all to the agent without on-demand certificates still needs subjects.
	plain := installerCaddyConfig("127.0.0.1:8081")
	delete(plain["apps"].(map[string]any), "tls")
	stubHostIngress(t, []int{443}, map[string]map[string]any{hostIngressDefaultAdmin: plain})
	if got := detectHostIngress(ctx, hostIngressDefaultAdmin, "127.0.0.1:8081"); got.Mode != HostIngressCaddy {
		t.Fatalf("catch-all without on-demand TLS: %+v", got)
	}
	// An admin API without HTTP servers is no ingress.
	stubHostIngress(t, nil, map[string]map[string]any{hostIngressDefaultAdmin: {"apps": map[string]any{}}})
	if got := detectHostIngress(ctx, hostIngressDefaultAdmin, "127.0.0.1:8081"); got.Mode != HostIngressAgent || got.AdminAPI {
		t.Fatalf("empty admin API: %+v", got)
	}
}

func TestHostIngressSettingsRoundTrip(t *testing.T) {
	operator := newFakeCaddyAdmin(t, operatorCaddyConfig())
	stubHostIngress(t, []int{80, 443}, nil) // no Caddy on the default admin address
	m := testManager(t, &fakeRuntime{})
	mux := http.NewServeMux()
	m.hostIngressRoutes(mux)
	call := func(method, body string) (int, HostIngressStatus) {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/host/ingress", strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		var status HostIngressStatus
		_ = json.Unmarshal(w.Body.Bytes(), &status)
		return w.Code, status
	}
	if code, status := call("GET", ""); code != 200 || status.Mode != HostIngressAgent || status.Source != "detected" || status.Override != nil || status.Configured || !slices.Equal(status.WebPorts, []int{80, 443}) {
		t.Fatalf("detected: %d %+v", code, status)
	}
	if _, ok := m.hostIngressCaddy(context.Background()); ok {
		t.Fatal("agent mode must not publish to Caddy")
	}
	// Overrides are validated before anything is stored.
	for _, bad := range []string{`{"caddy":{"admin":"http://10.0.0.5:2019"}}`, `{"caddy":{"admin":"` + operator.URL() + `","server":"nope"}}`, `{"caddy":{"admin":"http://127.0.0.1:1"}}`, `{"caddy":{"admin":"` + operator.URL() + `","upstream":"127.0.0.1:1"}}`} {
		if code, _ := call("PUT", bad); code != 409 {
			t.Fatalf("%s accepted: %d", bad, code)
		}
		if m.data.HostIngress != nil {
			t.Fatalf("%s stored", bad)
		}
	}
	code, status := call("PUT", `{"caddy":{"admin":"`+operator.URL()+`"}}`)
	if code != 200 || status.Mode != HostIngressCaddy || status.Source != "setting" || status.CaddyAdmin != operator.URL() || status.CaddyServer != "https" || status.Override == nil || status.Override.Server != "https" || status.Detected != HostIngressAgent {
		t.Fatalf("override: %d %+v", code, status)
	}
	if cfg, ok := m.hostIngressCaddy(context.Background()); !ok || cfg.Admin != operator.URL() || cfg.Server != "https" {
		t.Fatalf("created applications must publish to the override: %+v %v", cfg, ok)
	}
	// The setting is durable.
	var saved diskState
	if err := loadStateFile(m, &saved); err != nil || saved.HostIngress == nil || saved.HostIngress.Caddy.Admin != operator.URL() {
		t.Fatalf("not persisted: %+v %v", saved.HostIngress, err)
	}
	if code, status := call("PUT", `{"caddy":null}`); code != 200 || status.Mode != HostIngressAgent || status.Source != "detected" || status.Override != nil {
		t.Fatalf("clear: %d %+v", code, status)
	}
	// The configuration file wins and cannot be changed from the panel.
	m.cfg.Native = &NativeHostConfig{Root: "/srv/apps", User: "app", Caddy: &NativeCaddyConfig{Admin: "http://127.0.0.1:2019", Server: "srv1"}}
	if code, status := call("GET", ""); code != 200 || !status.Configured || status.Source != "config" || status.Mode != HostIngressCaddy || status.CaddyServer != "srv1" {
		t.Fatalf("configured: %d %+v", code, status)
	}
	if code, _ := call("PUT", `{"caddy":null}`); code != 409 {
		t.Fatalf("configured override changed: %d", code)
	}
	if health := m.serverHealth(context.Background()); health.Ingress == nil || health.Ingress.Mode != HostIngressCaddy || !health.Ingress.Configured {
		t.Fatalf("health lacks ingress: %+v", health.Ingress)
	}
}

// loadStateFile decodes the manager's saved state through the same path
// the agent uses at startup.
func loadStateFile(m *Manager, into *diskState) error {
	other := &Manager{cfg: m.cfg, key: m.key}
	if err := other.load(); err != nil {
		return err
	}
	*into = other.data
	return nil
}

func TestCloseStopsTheIngressLoopPromptly(t *testing.T) {
	oldGet := hostIngressAdminGet
	t.Cleanup(func() { hostIngressAdminGet = oldGet })
	probing := make(chan struct{}, 1)
	// A probe that only ends when its context does, like an admin API that
	// accepts the connection and never answers.
	hostIngressAdminGet = func(ctx context.Context, admin string) ([]byte, error) {
		select {
		case probing <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m, err := NewManager(context.Background(), Config{DataDir: t.TempDir(), APIKey: strings.Repeat("a", 32)}, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-probing:
	case <-time.After(5 * time.Second):
		t.Fatal("the ingress loop did not start probing")
	}
	// Detection never waits on the state lock.
	m.mu.Lock()
	detected := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		m.refreshIngressDetection(ctx)
		close(detected)
	}()
	select {
	case <-detected:
	case <-time.After(time.Second):
		m.mu.Unlock()
		t.Fatal("ingress detection waited on the state lock")
	}
	m.mu.Unlock()
	closed := make(chan struct{})
	start := time.Now()
	go func() { m.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not return within a second while the ingress probe was running")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %s", elapsed)
	}
}
