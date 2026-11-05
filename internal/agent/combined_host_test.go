package agent

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCombinedHostKeepsNativeOwnershipWhileDeployingGo(t *testing.T) {
	runtime := &fakeRuntime{}
	fixture, appDir := nativeFixture(t)
	cfg := fixture.cfg
	fixture.Close()
	cfg.HostMode = "coexist"
	m, err := NewManager(context.Background(), cfg, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	native, err := m.nativeRegistration(NativeApplication{ID: "my-rails", Name: "MyRails", Domain: "rails.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Readlink(filepath.Join(appDir, "current"))
	if err := m.Upsert(testApp("go-app")); err != nil {
		t.Fatal(err)
	}
	conflict := testApp("conflict")
	conflict.AdditionalDomains = []string{native.Domain}
	if err := m.Upsert(conflict); err == nil {
		t.Fatal("Go app acquired a native hostname")
	}
	if err := m.PatchSettings("go-app", SettingsPatch{Branch: "main", Domain: native.Domain}); err == nil {
		t.Fatal("Go settings acquired a native hostname")
	}
	if _, err := m.nativeRegistration(NativeApplication{ID: "other-native", Name: "MyRails", Domain: "go-app.example.com"}); err == nil {
		t.Fatal("native registration acquired a Go hostname")
	}
	request := httptest.NewRequest("POST", "/v1/apps/go-app/deploy", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	w := httptest.NewRecorder()
	Handler(m).ServeHTTP(w, request)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var started Deployment
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if result := waitDeployment(t, m, started.ID); result.Status != "live" {
		t.Fatal(result.Status)
	}
	request = httptest.NewRequest("GET", "/v1/apps", nil)
	request.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	w = httptest.NewRecorder()
	Handler(m).ServeHTTP(w, request)
	var all []App
	if err := json.Unmarshal(w.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatal("combined inventory missing runtime", w.Body.String())
	}
	if len(m.Apps()) != 1 || len(m.nativeViews()) != 1 || len(m.data.NativeBindings[native.ID]) == 0 {
		t.Fatal("native inventory entered container ownership")
	}
	if after, _ := os.Readlink(filepath.Join(appDir, "current")); after != before {
		t.Fatal("Go deploy changed native release")
	}
	if m.cfg.Native.ServiceControl {
		t.Fatal("Go deployment enabled native service control")
	}
	m.StartMaintenance()
	if m.data.Maintenance.Running {
		t.Fatal("combined host ran host-wide cleanup")
	}
}
