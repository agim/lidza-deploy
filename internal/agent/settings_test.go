package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSettingsPreserveSecretsAndRejectBusyChanges(t *testing.T) {
	m := testManager(t, &fakeRuntime{gate: make(chan struct{})})
	for _, id := range []string{"one", "two"} {
		if err := m.Upsert(testApp(id)); err != nil {
			t.Fatal(err)
		}
	}
	value := "new-secret"
	patch := SettingsPatch{Branch: "release", Domain: "updated.example.com", EnvChanges: map[string]*string{"NEW": &value}}
	if err := m.PatchSettings("one", patch); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a := m.data.Apps["one"]
	m.mu.Unlock()
	if a.Env["APP_SECRET"] != "runtime-secret" || a.Env["NEW"] != value {
		t.Fatal("lost variables")
	}
	settings, err := m.Settings("one")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(settings)
	if strings.Contains(string(data), value) || strings.Contains(string(data), "runtime-secret") {
		t.Fatal("leaked secret")
	}
	patch.EnvChanges = map[string]*string{"APP_SECRET": nil}
	if err := m.PatchSettings("one", patch); err != nil {
		t.Fatal(err)
	}
	// Rejected patches must not mutate the previous environment map.
	patch.EnvChanges = map[string]*string{"NEW": nil}
	patch.Domain = "two.example.com"
	if err := m.PatchSettings("one", patch); err == nil {
		t.Fatal("duplicate domain accepted")
	}
	patch.Domain = "updated.example.com"
	if _, err := m.Enqueue("one", DeployRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := m.PatchSettings("one", patch); err == nil {
		t.Fatal("busy app edited")
	}
	if err := m.Retire(context.Background(), "one"); err == nil {
		t.Fatal("active deployment removed")
	}
	// Stop the queued worker and verify the saved settings survive restart.
	m.cancel()
	m.wg.Wait()
	reloaded, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	reloaded.mu.Lock()
	a = reloaded.data.Apps["one"]
	reloaded.mu.Unlock()
	if _, exists := a.Env["APP_SECRET"]; exists || a.Env["NEW"] != value {
		t.Fatal("persisted patch incorrect")
	}
}
