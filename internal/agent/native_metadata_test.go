package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nativeAstroFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	m, _ := nativeFixture(t)
	app := filepath.Join(m.cfg.Native.Root, "marketing")
	release := filepath.Join(app, "releases", "one")
	for _, dir := range []string{filepath.Join(release, "public"), filepath.Join(app, "repo"), filepath.Join(m.cfg.Native.Root, ".config/caddy")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{filepath.Join(release, "package.json"): `{"dependencies":{"astro":"7.4.4"}}`, filepath.Join(release, "public/index.html"): "<h1>Live</h1>", filepath.Join(app, "repo/config"): "[remote \"origin\"]\n url = https://do-not-expose@github.com/org/marketing.git\n", filepath.Join(app, ".asdf-vars"): "export SECRET_KEY='preserve-original-key'\nPORT=9180\n"}
	for path, data := range files {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(release, filepath.Join(app, "current")); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"routes": []any{map[string]any{"match": []any{map[string]any{"host": []string{"marketing.example.com", "www.marketing.example.com"}}}, "handle": []any{map[string]any{"handler": "vars", "root": filepath.Join(app, "current/public")}, map[string]any{"handler": "file_server", "headers": map[string]string{"Authorization": "do-not-expose"}}}}}}
	data, _ := json.Marshal(config)
	os.WriteFile(filepath.Join(m.cfg.Native.Root, ".config/caddy/autosave.json"), data, 0600)
	return m, app
}

func TestNativeAstroDiscoveryRegistrationAndStatusNeverExecuteOrChangeFiles(t *testing.T) {
	m, app := nativeAstroFixture(t)
	commands := 0
	m.nativeCommand = func(context.Context, []string) (string, error) { commands++; return "", nil }
	before, _ := os.Readlink(filepath.Join(app, "current"))
	environment, _ := os.ReadFile(filepath.Join(app, ".asdf-vars"))
	units, err := m.nativeBinding("marketing")
	if err != nil || len(units) != 0 {
		t.Fatal("static Astro discovery", err, units)
	}
	discovered := m.nativeDiscoveredApplication("marketing", units, m.nativeObservedRoutes(context.Background()))
	if discovered.ID != "astro-marketing" || discovered.Runtime != "astro" || discovered.Repository != "org/marketing" || discovered.Domain != "marketing.example.com" || len(discovered.AdditionalDomains) != 1 || discovered.DocumentRoot != "public" {
		t.Fatal("metadata inference", discovered)
	}
	registered, err := m.nativeRegistration(discovered)
	if err != nil {
		t.Fatal(err)
	}
	status, err := m.nativeStatus(context.Background(), registered.ID)
	if err != nil || len(status.Services) != 0 || status.DeploymentSupported {
		t.Fatal("static status acquired a process", err, status)
	}
	settings, err := m.nativeSettings(registered.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(settings)
	if strings.Contains(string(data), "preserve-original-key") || strings.Contains(string(data), "do-not-expose") || settings.Capabilities.Configure || settings.Capabilities.Environment {
		t.Fatal("settings exposed credentials or acquired configuration ownership")
	}
	after, _ := os.Readlink(filepath.Join(app, "current"))
	afterEnvironment, _ := os.ReadFile(filepath.Join(app, ".asdf-vars"))
	if commands != 0 || before != after || string(environment) != string(afterEnvironment) || len(m.Apps()) != 0 {
		t.Fatal("native Astro observation caused a host action")
	}
	for _, view := range m.nativeViews() {
		if view.ID == registered.ID && view.Runtime != "astro" {
			t.Fatal("Astro was presented as Rails")
		}
	}
}

func TestNativeSettingsShowOrdinarySettingsMaskCredentialsAndNeverExecuteShellValues(t *testing.T) {
	m, app := nativeFixture(t)
	marker := filepath.Join(t.TempDir(), "must-not-run")
	os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("RAILS_MAX_THREADS=5\nRAILS_ENV=production\nFEATURE_ENABLED=true\nPUBLIC_URL=https://example.com\nRAILS_MASTER_KEY=original-master-key\nDATABASE_URL=postgres://user:original-password@db/app\nDYNAMIC=$(touch "+marker+")\n"), 0600)
	a, err := m.nativeRegistration(NativeApplication{ID: "rails", Name: "MyRails", Domain: "rails.example.com", Repository: "org/rails"})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := m.nativeSettings(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(settings)
	if strings.Contains(string(raw), "original-master-key") || strings.Contains(string(raw), "original-password") || strings.Contains(string(raw), "touch ") {
		t.Fatal("native settings contained an existing value")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("environment data was executed")
	}
	wantSettings := map[string]string{"RAILS_MAX_THREADS": "5", "RAILS_ENV": "production", "FEATURE_ENABLED": "true", "PUBLIC_URL": "https://example.com"}
	for _, entry := range settings.Environment {
		if value, ordinary := wantSettings[entry.Key]; ordinary {
			if entry.Kind != EnvironmentSetting || entry.Value != value {
				t.Fatal("ordinary native setting was masked", entry.Key)
			}
		} else if entry.Kind != EnvironmentCredential || entry.Value != "" || entry.Preview != "" || entry.Length == 0 {
			t.Fatal("native credential was included in settings", entry.Key)
		}
	}
	if settings.Capabilities.Configure || settings.Capabilities.Environment {
		t.Fatal("classification enabled native configuration writes")
	}
	if len(settings.Bindings) != 1 || len(settings.Environment) != len(wantSettings)+3 {
		t.Fatal("named resources or masked variables missing", settings)
	}
}

func TestNativeMetadataRejectsAmbiguousCanonicalDomainsAndCredentialHosts(t *testing.T) {
	m, _ := nativeAstroFixture(t)
	a := m.nativeDiscoveredApplication("marketing", nil, []nativeObservedRoute{{Root: filepath.Join(m.cfg.Native.Root, "marketing/current/public"), Hosts: []string{"one.example.com", "two.example.com"}}})
	if a.Domain != "" {
		t.Fatal("ambiguous canonical hostname guessed")
	}
	for _, raw := range []string{"[remote \"origin\"]\nurl=https://token@evil.example/org/repo.git", "[remote \"origin\"]\nurl=$(touch /tmp/no)"} {
		if nativeGitHubRepository([]byte(raw)) != "" {
			t.Fatal("unsafe Git metadata accepted")
		}
	}
}

func TestNativeMetadataReadsInternalAbsoluteCurrentLinksButRejectsEscapes(t *testing.T) {
	m, app := nativeAstroFixture(t)
	os.WriteFile(filepath.Join(app, "current/REVISION"), []byte(strings.Repeat("a", 40)), 0600)
	if string(m.nativeMetadataFile("marketing/current/REVISION")) != strings.Repeat("a", 40) {
		t.Fatal("existing absolute current symlink prevented metadata reads")
	}
	external := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(external, []byte("must not be read"), 0600)
	os.Symlink(external, filepath.Join(app, "current/private"))
	if m.nativeMetadataFile("marketing/current/private") != nil {
		t.Fatal("native metadata escaped the configured root")
	}
}

func TestNativeBuildEnvironmentReadsReleaseScopedValues(t *testing.T) {
	m, app := nativeAstroFixture(t)
	os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("PORT=9180\n"), 0600)
	os.WriteFile(filepath.Join(app, "current/.asdf-vars"), []byte("PUBLIC_SITE=release\n"), 0600)
	values, err := m.nativeBuildEnvironment("marketing")
	if err != nil || values["PUBLIC_SITE"] != "release" || values["PORT"] != "9180" {
		t.Fatal("literal build environment omitted release values", err)
	}
}
