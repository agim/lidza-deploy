package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
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

func TestExplicitMasterKeyGeneratorIsAuthenticatedMaskedAndNoRotation(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("older-install")
	a.Current = &Release{ID: "running", Container: "existing-container"}
	m.mu.Lock()
	m.data.Apps[a.ID] = a
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	handler := Handler(m)
	req := httptest.NewRequest("POST", "/v1/apps/"+a.ID+"/master-key", nil)
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, req)
	if denied.Code != 401 {
		t.Fatal("unauthenticated key generation accepted", denied.Code)
	}
	req = httptest.NewRequest("POST", "/v1/apps/"+a.ID+"/master-key", nil)
	req.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	reply := httptest.NewRecorder()
	handler.ServeHTTP(reply, req)
	if reply.Code != 200 {
		t.Fatal(reply.Code, reply.Body)
	}
	key := m.data.Apps[a.ID].Env["LIDZA_MASTER_KEY"]
	var entry EnvironmentEntry
	if err := json.Unmarshal(reply.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if len(key) != 64 || entry.Key != "LIDZA_MASTER_KEY" || entry.Value != "" || entry.Length != 64 || entry.Kind != EnvironmentCredential || strings.Contains(reply.Body.String(), key) {
		t.Fatal("invalid or exposed key metadata")
	}
	if m.Current(a.ID).ID != "running" || len(m.queue) != 0 || len(m.data.Deployments) != 0 {
		t.Fatal("generation changed running release")
	}
	for i := 0; i < 2; i++ {
		if err := m.GenerateMasterKey(a.ID); err == nil {
			t.Fatal("rotated existing key")
		}
	}
	if m.data.Apps[a.ID].Env["LIDZA_MASTER_KEY"] != key {
		t.Fatal("existing key changed")
	}
	// A second operator must not win a racing generation request.
	b := testApp("racing-install")
	m.mu.Lock()
	m.data.Apps[b.ID] = b
	m.mu.Unlock()
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- m.GenerateMasterKey(b.ID) }()
	}
	successes := 0
	for i := 0; i < 2; i++ {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatal("generation was not atomic", successes)
	}
	settings, _ := m.Settings(a.ID)
	if !settings.MasterKeyGenerationSupported {
		t.Fatal("generation capability not exposed")
	}
}

func TestExplicitMasterKeyGeneratorBlocksBusyAppsAndRollsBackSaveFailure(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("generator-busy")
	m.mu.Lock()
	m.data.Apps[a.ID] = a
	m.data.Deployments = append(m.data.Deployments, Deployment{ID: "active", AppID: a.ID, Status: "building"})
	m.mu.Unlock()
	if err := m.GenerateMasterKey(a.ID); err == nil {
		t.Fatal("changed key during deployment")
	}
	m.mu.Lock()
	m.data.Deployments = nil
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	m.Close()
	if err := os.Rename(m.path(), m.path()+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(m.path(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := m.GenerateMasterKey(a.ID); err == nil {
		t.Fatal("hidden key persistence failure")
	}
	if m.data.Apps[a.ID].Env["LIDZA_MASTER_KEY"] != "" {
		t.Fatal("used non-durable key")
	}
}
