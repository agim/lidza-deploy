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

func wizardConfiguration() StackManifest {
	m := fixtureStackManifest()
	m.BootstrapReadiness = &StackBootstrapReadiness{Path: "/api/v1/health", ReadyzBodyContains: "Server setup is required", SetupPath: "/setup"}
	m.Configuration = []StackConfiguration{
		{Name: "MAIL_PROVIDER", Stage: "setup", Label: "Mail provider"},
		{Name: "MAIL_FROM", Stage: "setup", Label: "Sender"},
		{Name: "LLM_PROVIDER", Stage: "optional", Label: "AI provider"},
	}
	return m
}
func TestStackConfigurationProductionOwnership(t *testing.T) {
	cfg := &config.Config{Packs: []string{"lidza/mail", "lidza/llm", "lidza/db"}}
	values := map[string]string{"APP_URL": "https://wizard.example.com", "DATABASE_URL": "postgres://db/app"}
	m := wizardConfiguration()
	if err := productionRequirements(cfg, values); err == nil {
		t.Fatal("legacy strict defaults changed")
	}
	if err := stackProductionRequirements(cfg, values, false, &m); err != nil {
		t.Fatal("empty app-owned mail/AI settings blocked wizard", err)
	}
	// No dummy values or enabling optional AI is needed, even on established
	// configured releases whose app-owned values live in their database.
	for _, key := range []string{"MAIL_PROVIDER", "MAIL_FROM", "LLM_PROVIDER"} {
		if values[key] != "" {
			t.Fatal("injected configuration")
		}
	}
	for _, pair := range [][2]string{{"MAIL_PROVIDER", "log"}, {"MAIL_PROVIDER", "outbox"}, {"LLM_PROVIDER", "fake"}, {"EMBED_PROVIDER", "fake"}} {
		values[pair[0]] = pair[1]
		if err := stackProductionRequirements(cfg, values, false, &m); err == nil {
			t.Fatalf("dev value accepted: %v", pair)
		}
		delete(values, pair[0])
	}
	delete(values, "DATABASE_URL")
	if err := stackProductionRequirements(cfg, values, false, &m); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatal("infrastructure bypassed", err)
	}
	m.Configuration = append(m.Configuration, StackConfiguration{Name: "CUSTOM_PROVIDER", Stage: "deploy", Label: "Vendor endpoint", Help: "Add the production endpoint."})
	err := stackProductionRequirements(cfg, values, false, &m)
	if err == nil || !strings.Contains(err.Error(), "Vendor endpoint") || !strings.Contains(err.Error(), "Add the production endpoint.") {
		t.Fatal("custom deploy guidance absent", err)
	}
}
func TestStackConfigurationOptionalDependencies(t *testing.T) {
	m := wizardConfiguration()
	m.Configuration = append(m.Configuration, StackConfiguration{Name: "REPORT_ENDPOINT", Stage: "optional", RequiredWhen: &StackConfigurationCondition{Name: "REPORTS_ENABLED", Equals: "true"}})
	cfg := &config.Config{}
	for _, enabled := range []string{"", "false", "TRUE", "true"} {
		values := map[string]string{"REPORTS_ENABLED": enabled}
		err := stackProductionRequirements(cfg, values, false, &m)
		if (err != nil) != (enabled == "true") {
			t.Fatalf("conditional requirement %q: %v", enabled, err)
		}
		values["REPORT_ENDPOINT"] = "https://reports.example.com"
		if err := stackProductionRequirements(cfg, values, false, &m); err != nil {
			t.Fatal(err)
		}
	}
}
func TestStackConfigurationRejectsContradictions(t *testing.T) {
	for _, key := range []string{"LIDZA_MASTER_KEY", "DATABASE_URL", "CACHE_URL", "CLICKHOUSE_URL", "STORAGE_PROVIDER", "AUTH_SECRET", "APP_URL", "THURA_SETUP_TOKEN"} {
		m := wizardConfiguration()
		m.Configuration = append(m.Configuration, StackConfiguration{Name: key, Stage: "optional"})
		if m.validate() == nil {
			t.Fatalf("deferred infrastructure/generated secret %s", key)
		}
	}
	for name, mutate := range map[string]func(*StackManifest){
		"no-bootstrap": func(m *StackManifest) { m.BootstrapReadiness = nil },
		"duplicate":    func(m *StackManifest) { m.Configuration = append(m.Configuration, m.Configuration[0]) },
		"required-env": func(m *StackManifest) { m.RequiredEnvironment = append(m.RequiredEnvironment, "MAIL_FROM") },
		"stage":        func(m *StackManifest) { m.Configuration[0].Stage = "later" },
		"control-text": func(m *StackManifest) { m.Configuration[0].Help = "bad\nlog" },
		"secret-condition": func(m *StackManifest) {
			m.Configuration[2].RequiredWhen = &StackConfigurationCondition{Name: "API_TOKEN", Equals: "secret"}
		},
		"deferred-condition": func(m *StackManifest) {
			m.Configuration[2].RequiredWhen = &StackConfigurationCondition{Name: "MAIL_PROVIDER", Equals: "smtp"}
		},
		"self-condition": func(m *StackManifest) {
			m.Configuration[2].RequiredWhen = &StackConfigurationCondition{Name: "LLM_PROVIDER", Equals: "openai"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := wizardConfiguration()
			mutate(&m)
			if m.validate() == nil {
				t.Fatal("invalid contract accepted")
			}
		})
	}
}
func TestStackConfigurationRealPreflightCredentials(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("wizard-config")
	a.RuntimeProfile = "minimum"
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	m.mu.Unlock()
	dir := cacheManifest(t, `["lidza/mail","lidza/llm"]`)
	os.MkdirAll(filepath.Join(dir, "deploy"), 0700)
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch"), 0600)
	manifest := wizardConfiguration()
	raw, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(dir, "deploy", "lidza.stack.json"), raw, 0600)
	if _, err := m.preflightRuntime(context.Background(), a, dir); err != nil {
		t.Fatal("fresh missing settings preflight", err)
	}
	// Old declarations do not bypass validation; production secrets must decrypt
	// before any setup-owned missing setting can be delegated.
	os.MkdirAll(filepath.Join(dir, "config"), 0700)
	key := []byte(strings.Repeat("k", 32))
	sealed, err := credentials.Encrypt(key, []byte("production:\n  MAIL_PROVIDER: smtp\n"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, credentials.File), []byte(sealed), 0600)
	if _, err := m.preflightRuntime(context.Background(), a, dir); err == nil || !strings.Contains(err.Error(), "could not open") {
		t.Fatal("unusable existing credentials accepted", err)
	}
	a.Env["LIDZA_MASTER_KEY"] = hex.EncodeToString(key)
	// prepareStack uses durable encrypted app settings, not mutable caller values.
	m.mu.Lock()
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	if _, err := m.preflightRuntime(context.Background(), a, dir); err != nil {
		t.Fatal("original credential key rejected", err)
	}
	delete(a.Env, "LIDZA_MASTER_KEY")
	m.mu.Lock()
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	if _, err := m.preflightRuntime(context.Background(), a, dir); err == nil {
		t.Fatal("missing required master key accepted")
	}
}
func TestStackConfigurationSecretMetadata(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("metadata")
	a.RuntimeProfile = "minimum"
	a.Env["CUSTOM_VALUE"] = "must-stay-secret"
	a.EnvKinds = map[string]string{"CUSTOM_VALUE": EnvironmentSetting}
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	manifest := wizardConfiguration()
	manifest.Configuration = append(manifest.Configuration, StackConfiguration{Name: "CUSTOM_VALUE", Stage: "deploy", Secret: true})
	if _, err := m.prepareStack(context.Background(), a, &manifest); err != nil {
		t.Fatal(err)
	}
	settings, _ := m.Settings(a.ID)
	raw, _ := json.Marshal(settings)
	if strings.Contains(string(raw), "must-stay-secret") {
		t.Fatal("manifest secret exposed")
	}
	// Absent optional secrets must not create classifications for missing values.
	manifest.Configuration = append(manifest.Configuration, StackConfiguration{Name: "OPTIONAL_VALUE", Stage: "optional", Secret: true})
	if _, err := m.prepareStack(context.Background(), a, &manifest); err != nil {
		t.Fatal(err)
	}
}
