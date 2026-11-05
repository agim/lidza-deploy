package agent

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeOnlyDoesNotRunContainerOrHostMutationPaths(t *testing.T) {
	cfg := Config{DataDir: t.TempDir(), APIKey: strings.Repeat("n", 32), NativeOnly: true, Native: &NativeHostConfig{Root: t.TempDir(), User: "agim"}}
	m, err := NewManager(context.Background(), cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.StartMaintenance()
	if m.data.Maintenance != nil || m.serviceTLS != nil || len(m.data.Deployments) != 0 {
		t.Fatal("native observer initialized host/container managers")
	}
	h := Handler(m)
	for _, request := range []struct{ method, path, body string }{{"PUT", "/v1/apps/container", `{"repository":"acme/app","branch":"main","domain":"app.example.com"}`}, {"POST", "/v1/console", `{"command":"reboot"}`}, {"POST", "/v1/upgrade", `{"version":"v1.0.0"}`}, {"PATCH", "/v1/maintenance", `{}`}} {
		r := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
		r.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 409 {
			t.Fatalf("observer accepted mutation %s: %d", request.path, w.Code)
		}
	}
	if len(m.data.Apps) != 0 {
		t.Fatal("observer acquired container state")
	}
}

func TestNativeOnlyRefusesContainerStateDirectory(t *testing.T) {
	base := testManager(t, &fakeRuntime{})
	if err := base.Upsert(testApp("running-go")); err != nil {
		t.Fatal(err)
	}
	cfg := base.cfg
	base.Close()
	cfg.NativeOnly = true
	cfg.Native = &NativeHostConfig{Root: t.TempDir(), User: "agim"}
	if m, err := NewManager(context.Background(), cfg, &fakeRuntime{}); err == nil {
		m.Close()
		t.Fatal("native observer accepted a container agent data directory")
	}
}

func TestNativeOnlyLoadConfigKeepsManagementOnLoopbackWithoutIngress(t *testing.T) {
	t.Setenv("LIDZA_AGENT_TOKEN", strings.Repeat("x", 64))
	file := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(file, []byte(`{"native_only":true,"listen":"127.0.0.1:9092","data_dir":"/var/lib/lidza-native-agent","native":{"root":"/home/deploy","user":"deploy","service_control":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSListen != "" || cfg.ProxyListen != "" || cfg.Native.ServiceControl {
		t.Fatal("observer acquired ingress or service control")
	}
	if err := os.WriteFile(file, []byte(`{"native_only":true,"listen":"0.0.0.0:9092","native":{"root":"/home/deploy","user":"deploy"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(file); err == nil {
		t.Fatal("plaintext public native management accepted")
	}
}

func TestNativeAndIngressRolesCannotBeCombined(t *testing.T) {
	t.Setenv("LIDZA_AGENT_TOKEN", strings.Repeat("x", 64))
	file := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(file, []byte(`{"native_only":true,"ingress_only":true,"native":{"root":"/home/deploy","user":"deploy"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(file); err == nil {
		t.Fatal("combined native/ingress roles accepted")
	}
	cfg := Config{IngressOnly: true, Native: &NativeHostConfig{Root: t.TempDir(), User: "agim"}}
	if reader, err := NewIngressReader(context.Background(), cfg); err == nil {
		reader.Close()
		t.Fatal("native host initialized as an ingress reader")
	}
}
