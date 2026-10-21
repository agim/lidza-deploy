package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agim/lidza/pkg/config"
	"github.com/agim/lidza/pkg/credentials"
)

type masterRuntime struct {
	fakeRuntime
	keys []string
}

func (r *masterRuntime) Deploy(ctx context.Context, a App, id, token string) (*Release, error) {
	r.mu.Lock()
	r.keys = append(r.keys, a.Env["LIDZA_MASTER_KEY"])
	r.mu.Unlock()
	return r.fakeRuntime.Deploy(ctx, a, id, token)
}
func (r *masterRuntime) Reload(ctx context.Context, a App, id string) (*Release, error) {
	return r.Deploy(ctx, a, id, "")
}

func TestMasterKeyGeneratedOncePrivateAndStable(t *testing.T) {
	rt := &masterRuntime{}
	m := testManager(t, rt)
	a := testApp("master-one")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	key := m.data.Apps[a.ID].Env["LIDZA_MASTER_KEY"]
	decoded, err := hex.DecodeString(key)
	if err != nil || len(decoded) != 32 {
		t.Fatal("master key must be 256-bit hex")
	}
	if a.Env["LIDZA_MASTER_KEY"] != "" {
		t.Fatal("mutated caller's environment")
	}
	sealed, err := credentials.Encrypt(decoded, []byte("TOKEN: generated-key-works\n"))
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err = os.MkdirAll(filepath.Join(source, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, credentials.File), []byte(sealed), 0600); err != nil {
		t.Fatal(err)
	}
	values, err := productionValues(source, &config.Config{}, m.data.Apps[a.ID])
	if err != nil || values["TOKEN"] != "generated-key-works" {
		t.Fatal("generated key cannot open framework credentials", err)
	}
	other := testApp("master-two")
	if err = m.Upsert(other); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[other.ID].Env["LIDZA_MASTER_KEY"] == key {
		t.Fatal("apps share master key")
	}
	for i := 0; i < 2; i++ {
		d, e := m.Enqueue(a.ID, DeployRequest{})
		if e != nil {
			t.Fatal(e)
		}
		if waitDeployment(t, m, d.ID).Status != "live" {
			t.Fatal("deployment failed")
		}
	}
	d, err := m.Reload(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitDeployment(t, m, d.ID)
	for _, sent := range rt.keys {
		if sent != key {
			t.Fatal("runtime did not retain master key")
		}
	}
	if err = m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[a.ID].Env["LIDZA_MASTER_KEY"] != key {
		t.Fatal("omitted key rotated on full update")
	}
	raw, e := os.ReadFile(m.path())
	if e != nil || strings.Contains(string(raw), key) {
		t.Fatal("master key persisted in plaintext", e)
	}
	settings, e := m.Settings(a.ID)
	if e != nil {
		t.Fatal(e)
	}
	wire, _ := json.Marshal(settings)
	listed, _ := json.Marshal(m.Apps())
	if strings.Contains(string(wire), key) || strings.Contains(string(listed), key) {
		t.Fatal("master key exposed by API")
	}
	for _, entry := range settings.Environment {
		if entry.Key == "LIDZA_MASTER_KEY" && (entry.Kind != EnvironmentCredential || entry.Value != "") {
			t.Fatal("master key not masked")
		}
	}
	ctx := m.deploymentDiagnostics(context.Background(), job{app: m.data.Apps[a.ID], deployment: d.ID})
	commandDiagnostic(ctx, "runtime", key)
	if strings.Contains(m.Deployments()[0].Log, key) {
		t.Fatal("key leaked in diagnostics")
	}
	m.Close()
	restarted, e := NewManager(context.Background(), m.cfg, rt)
	if e != nil {
		t.Fatal(e)
	}
	defer restarted.Close()
	if restarted.data.Apps[a.ID].Env["LIDZA_MASTER_KEY"] != key {
		t.Fatal("restart changed key")
	}
}

func TestMasterKeyImportValidationAndLegacySafety(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	for _, value := range []string{"short", strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		a := testApp("bad-key")
		a.Env["LIDZA_MASTER_KEY"] = value
		if err := m.Upsert(a); err == nil {
			t.Fatal("accepted invalid key")
		}
	}
	a := testApp("import-key")
	key := strings.Repeat("Ab", 32)
	a.Env["LIDZA_MASTER_KEY"] = key
	a.EnvKinds = map[string]string{"LIDZA_MASTER_KEY": EnvironmentSetting}
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[a.ID].Env["LIDZA_MASTER_KEY"] != key {
		t.Fatal("replaced imported key")
	}
	view, _ := m.Settings(a.ID)
	wire, _ := json.Marshal(view)
	if strings.Contains(string(wire), key) {
		t.Fatal("explicit setting kind exposed master key")
	}
	for _, v := range []*string{nil, new(string)} {
		if err := m.PatchSettings(a.ID, SettingsPatch{EnvChanges: map[string]*string{"LIDZA_MASTER_KEY": v}}); err == nil {
			t.Fatal("allowed clearing encryption key")
		}
	}
	legacy := testApp("legacy-master")
	m.mu.Lock()
	m.data.Apps[legacy.ID] = legacy
	m.mu.Unlock()
	if err := m.Upsert(legacy); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[legacy.ID].Env["LIDZA_MASTER_KEY"] != "" {
		t.Fatal("silently generated replacement for legacy installation")
	}
	// Framework sealed-credential verification must still reject a mismatched key.
	sealed, e := credentials.Encrypt(make([]byte, 32), []byte("TOKEN: fixture\n"))
	if e != nil {
		t.Fatal(e)
	}
	source := t.TempDir()
	if e = os.MkdirAll(filepath.Join(source, "config"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(source, credentials.File), []byte(sealed), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = productionValues(source, &config.Config{}, m.data.Apps[a.ID]); e == nil {
		t.Fatal("mismatched master key bypassed sealed credential check")
	}
	supplied := m.data.Apps[a.ID]
	supplied.Env["LIDZA_MASTER_KEY"] = strings.Repeat("0", 64)
	if _, e = productionValues(source, &config.Config{}, supplied); e != nil {
		t.Fatal("matching original key rejected", e)
	}
}

func TestMasterKeyGenerationDoesNotRegisterUnsavedApp(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.Close()
	if err := os.Rename(m.path(), m.path()+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(m.path(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := m.Upsert(testApp("unsaved-master")); err == nil {
		t.Fatal("hidden persistence failure")
	}
	if _, ok := m.data.Apps["unsaved-master"]; ok {
		t.Fatal("registered app without durable key")
	}
}
