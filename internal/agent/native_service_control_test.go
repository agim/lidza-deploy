package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serviceControlCall(t *testing.T, m *Manager, method, body string) (int, ServiceControlStatus) {
	t.Helper()
	mux := http.NewServeMux()
	m.serviceControlRoutes(mux)
	req := httptest.NewRequest(method, "/v1/host/service-control", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var status ServiceControlStatus
	_ = json.Unmarshal(w.Body.Bytes(), &status)
	return w.Code, status
}

func TestServiceControlSettingIsPersistedAndAppliedWithoutRestart(t *testing.T) {
	m, calls := adoptedLifecycleFixture(t)
	ctx := context.Background()
	a, _ := m.nativeApp("my-rails")
	if code, status := serviceControlCall(t, m, "GET", ""); code != 200 || !status.Supported || status.Enabled || status.Source != "config" || status.ConfigDefault {
		t.Fatalf("initial: %d %+v", code, status)
	}
	if capability := m.nativeManagementCapabilities(a); capability.Lifecycle || capability.LifecycleReason != serviceControlOffReason || capability.LifecycleSetting != "service_control" {
		t.Fatalf("off: %+v", capability)
	}
	if err := m.nativeServiceAction(ctx, "my-rails", "myrails-server.service", "restart", "MyRails"); err == nil || !strings.Contains(err.Error(), "Agent settings") {
		t.Fatalf("service action while off: %v", err)
	}
	if code, _ := serviceControlCall(t, m, "PUT", `{}`); code != 400 {
		t.Fatalf("missing enabled accepted: %d", code)
	}

	// Turning it on applies at once: capabilities, status and the lifecycle.
	if code, status := serviceControlCall(t, m, "PUT", `{"enabled":true}`); code != 200 || !status.Enabled || status.Source != "setting" {
		t.Fatalf("enable: %d %+v", code, status)
	}
	if capability := m.nativeManagementCapabilities(a); !capability.Lifecycle || capability.LifecycleReason != "" || capability.LifecycleSetting != "" {
		t.Fatalf("on: %+v", capability)
	}
	if status, err := m.nativeStatus(ctx, "my-rails"); err != nil || !status.ServiceControl {
		t.Fatalf("status: %+v %v", status, err)
	}
	if _, err := m.AppLifecycle(ctx, "my-rails", "restart", AppLifecycleRequest{}); err != nil || len(*calls) == 0 {
		t.Fatalf("restart with the setting on: %v %v", err, *calls)
	}

	// The stored setting survives a restart and wins over the configuration.
	cfg := m.cfg
	cfg.Native.ServiceControl = false
	m.Close()
	reopened, err := NewManager(ctx, cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.nativeAdoptedServiceControl() {
		t.Fatal("stored setting lost on restart")
	}
	reopened.cfg.Native.ServiceControl = true
	if code, status := serviceControlCall(t, reopened, "PUT", `{"enabled":false}`); code != 200 || status.Enabled || !status.ConfigDefault || status.Source != "setting" {
		t.Fatalf("disable over a configured default: %d %+v", code, status)
	}
	if reopened.nativeLifecycleAllowed("my-rails") != serviceControlOffReason {
		t.Fatal("setting did not win over the configuration")
	}
}

func TestServiceControlSettingLeavesCreatedApplicationsControllable(t *testing.T) {
	m, _ := adoptedLifecycleFixture(t)
	if code, _ := serviceControlCall(t, m, "PUT", `{"enabled":false}`); code != 200 {
		t.Fatal("disable failed")
	}
	m.mu.Lock()
	if m.data.NativeCreated == nil {
		m.data.NativeCreated = map[string]NativeReleasePolicy{}
	}
	m.data.NativeCreated["shop"] = NativeReleasePolicy{Enabled: true, Runtime: "go", User: "lidza-shop", UnitScope: "system"}
	m.mu.Unlock()
	if !m.nativeServiceControlFor("shop") || m.nativeLifecycleAllowed("shop") != "" {
		t.Fatal("created application lost service control")
	}
	if m.nativeServiceControlFor("my-rails") {
		t.Fatal("adopted application kept service control")
	}
}

func TestServiceControlSettingNeedsNativeManagement(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if code, status := serviceControlCall(t, m, "GET", ""); code != 200 || status.Supported {
		t.Fatalf("status without native management: %d %+v", code, status)
	}
	if code, _ := serviceControlCall(t, m, "PUT", `{"enabled":true}`); code != 409 {
		t.Fatalf("set without native management: %d", code)
	}
}
