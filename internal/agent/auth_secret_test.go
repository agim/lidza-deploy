package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type authRuntime struct {
	fakeRuntime
	secrets []string
}

func (f *authRuntime) Deploy(ctx context.Context, a App, id, token string) (*Release, error) {
	f.mu.Lock()
	f.secrets = append(f.secrets, a.Env["AUTH_SECRET"])
	f.mu.Unlock()
	return f.fakeRuntime.Deploy(ctx, a, id, token)
}
func (f *authRuntime) Reload(ctx context.Context, a App, id string) (*Release, error) {
	return f.Deploy(ctx, a, id, "")
}

func TestAuthSecretProvisioningStableAndPrivate(t *testing.T) {
	rt := &authRuntime{}
	m := testManager(t, rt)
	a := testApp("auth-one")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	secret := m.data.Apps[a.ID].Env["AUTH_SECRET"]
	bytes, err := hex.DecodeString(secret)
	if err != nil || len(bytes) != 32 {
		t.Fatal("expected 256-bit random auth secret")
	}
	if m.data.Apps[a.ID].Env["LIDZA_MASTER_KEY"] != "" {
		t.Fatal("master key must remain manual")
	}
	second := testApp("auth-two")
	if err := m.Upsert(second); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[second.ID].Env["AUTH_SECRET"] == secret {
		t.Fatal("apps share authentication secret")
	}
	raw, err := os.ReadFile(m.path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("secret persisted in plaintext")
	}
	settings, err := m.Settings(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(settings)
	if strings.Contains(string(wire), secret) {
		t.Fatal("settings expose secret")
	}
	listed, _ := json.Marshal(m.Apps())
	if strings.Contains(string(listed), secret) {
		t.Fatal("app list exposes secret")
	}
	for i := 0; i < 2; i++ {
		d, err := m.Enqueue(a.ID, DeployRequest{})
		if err != nil {
			t.Fatal(err)
		}
		waitDeployment(t, m, d.ID)
	}
	d, err := m.Reload(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitDeployment(t, m, d.ID)
	for _, sent := range rt.secrets {
		if sent != secret {
			t.Fatal("deployment/reload rotated or omitted secret")
		}
	}
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[a.ID].Env["AUTH_SECRET"] != secret {
		t.Fatal("full app update rotated secret")
	}
	restarted, err := NewManager(context.Background(), m.cfg, rt)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.data.Apps[a.ID].Env["AUTH_SECRET"] != secret {
		t.Fatal("restart rotated secret")
	}
	ctx := m.deploymentDiagnostics(context.Background(), job{app: m.data.Apps[a.ID], deployment: d.ID})
	commandDiagnostic(ctx, "runtime", secret)
	for _, log := range m.Deployments() {
		if strings.Contains(log.Log, secret) {
			t.Fatal("diagnostics expose generated secret")
		}
	}
}

func TestSuppliedAuthAndMasterKeysPreservedAndLegacyAppBackfilled(t *testing.T) {
	m := testManager(t, &authRuntime{})
	a := testApp("auth-import")
	a.Env["AUTH_SECRET"] = strings.Repeat("existing-signing-key-", 3)
	a.Env["LIDZA_MASTER_KEY"] = strings.Repeat("a", 64)
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[a.ID].Env["AUTH_SECRET"] != a.Env["AUTH_SECRET"] || m.data.Apps[a.ID].Env["LIDZA_MASTER_KEY"] != a.Env["LIDZA_MASTER_KEY"] {
		t.Fatal("supplied keys replaced")
	}
	p := SettingsPatch{Branch: a.Branch, Domain: a.Domain, EnvChanges: map[string]*string{"AUTH_SECRET": nil}}
	if m.PatchSettings(a.ID, p) == nil {
		t.Fatal("clearing auth secret silently rotates identity")
	}
	replacement := strings.Repeat("replacement-signing-key-", 2)
	p.EnvChanges["AUTH_SECRET"] = &replacement
	if err := m.PatchSettings(a.ID, p); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[a.ID].Env["AUTH_SECRET"] != replacement {
		t.Fatal("explicit import/rotation not applied")
	}
	// Represent an existing pre-feature app with no auth key.
	m.mu.Lock()
	stored := m.data.Apps[a.ID]
	delete(stored.Env, "AUTH_SECRET")
	m.data.Apps[a.ID] = stored
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	d, err := m.Enqueue(a.ID, DeployRequest{Key: "same-event"})
	if err != nil {
		t.Fatal(err)
	}
	waitDeployment(t, m, d.ID)
	secret := m.data.Apps[a.ID].Env["AUTH_SECRET"]
	if secret == "" {
		t.Fatal("legacy app not backfilled")
	}
	if _, err = m.Enqueue(a.ID, DeployRequest{Key: "same-event"}); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[a.ID].Env["AUTH_SECRET"] != secret {
		t.Fatal("idempotent retry rotated secret")
	}
	if m.data.Apps[a.ID].Env["LIDZA_MASTER_KEY"] != a.Env["LIDZA_MASTER_KEY"] {
		t.Fatal("manual master key changed")
	}
}

func TestAuthSecretNotQueuedWithoutPersistence(t *testing.T) {
	m := testManager(t, &authRuntime{})
	a := testApp("auth-save-failure")
	m.mu.Lock()
	m.data.Apps[a.ID] = a
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	if err := os.Rename(m.path(), m.path()+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(m.path(), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Enqueue(a.ID, DeployRequest{}); err == nil {
		t.Fatal("queued deployment without persisting generated secret")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.Apps[a.ID].Env["AUTH_SECRET"] != "" || len(m.data.Deployments) != 0 || len(m.queue) != 0 {
		t.Fatal("failed persistence retained unsaved key or queued work")
	}
}
