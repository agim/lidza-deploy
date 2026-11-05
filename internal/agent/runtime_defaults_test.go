package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateRuntimeDefaults(t *testing.T) {
	seeds := map[string]map[string]string{
		"rails": {"RAILS_ENV": "production", "RACK_ENV": "production", "RAILS_LOG_TO_STDOUT": "1", "RAILS_SERVE_STATIC_FILES": "1"},
		"astro": {"NODE_ENV": "production", "ASTRO_TELEMETRY_DISABLED": "1"},
		"node":  {"NODE_ENV": "production", "TZ": "Europe/Riga", "GREETING": "hello world"},
	}
	for runtime, values := range seeds {
		if err := ValidateRuntimeDefaults(runtime, values); err != nil {
			t.Fatal(runtime, err)
		}
	}
	// The Līdza set keeps its strict allowlist.
	if ValidateRuntimeDefaults("go", map[string]string{"RAILS_ENV": "production"}) == nil || ValidateRuntimeDefaults("", map[string]string{"NODE_ENV": "production"}) == nil {
		t.Fatal("Go defaults accepted a non-allowlisted key")
	}
	if ValidateRuntimeDefaults("go", map[string]string{"LOG_LEVEL": "info"}) != nil {
		t.Fatal("Go defaults refused an allowlisted key")
	}
	if ValidateRuntimeDefaults("php", map[string]string{}) == nil {
		t.Fatal("unknown runtime accepted")
	}
	refused := []string{
		// credential-like and per-application services
		"SECRET_KEY_BASE", "RAILS_MASTER_KEY", "API_TOKEN", "SMTP_PASSWORD", "AWS_CREDENTIALS", "PRIVATE_NOTE", "DATABASE_URL", "REDIS_URL", "SENTRY_DSN", "api_key",
		// host-owned
		"PATH", "HOME", "USER", "SHELL", "PORT", "PWD", "LD_PRELOAD", "LD_ANYTHING", "BUNDLE_GEMFILE", "NODE_OPTIONS", "BASH_ENV", "ENV", "XDG_CONFIG_HOME", "LIDZA_ANYTHING", "NODE_VERSION",
		// invalid names
		"1BAD", "A-B", "WITH SPACE", "",
	}
	for _, runtime := range []string{"rails", "astro", "node"} {
		for _, key := range refused {
			if ValidateRuntimeDefaults(runtime, map[string]string{key: "x"}) == nil {
				t.Fatal(runtime, "accepted", key)
			}
		}
		for name, value := range map[string]string{"quote": "it's", "newline": "a\nb", "return": "a\rb", "nul": "a\x00b", "long": strings.Repeat("a", 2049)} {
			if ValidateRuntimeDefaults(runtime, map[string]string{"SETTING": value}) == nil {
				t.Fatal(runtime, "accepted value", name)
			}
		}
		if ValidateRuntimeDefaults(runtime, map[string]string{"SETTING": strings.Repeat("a", 2048)}) != nil {
			t.Fatal(runtime, "refused a 2048 character value")
		}
		many := map[string]string{}
		for i := range MaxRuntimeDefaults + 1 {
			many[fmt.Sprintf("SETTING_%d", i)] = "1"
		}
		if ValidateRuntimeDefaults(runtime, many) == nil {
			t.Fatal(runtime, "accepted more than the maximum number of defaults")
		}
		delete(many, "SETTING_0")
		if err := ValidateRuntimeDefaults(runtime, many); err != nil {
			t.Fatal(runtime, err)
		}
	}
}

func TestInitialDefaultsForContainerRailsAndAstro(t *testing.T) {
	m := testManager(t, &fakeRuntime{fail: true})
	rails := testApp("rails-app")
	rails.Runtime, rails.Rails = "rails", &RailsConfig{HealthPath: "/up"}
	rails.Env["RAILS_ENV"] = "staging"
	astro := testApp("astro-site")
	astro.Runtime, astro.Astro = "astro", &AstroConfig{Mode: "static", OutputDir: "dist", NodeMajor: "24"}
	legacy := testApp("legacy-rails")
	legacy.Runtime, legacy.Rails = "rails", &RailsConfig{HealthPath: "/up"}
	golang := testApp("go-app")
	for _, a := range []App{rails, astro, legacy, golang} {
		if err := m.Upsert(a); err != nil {
			t.Fatal(a.ID, err)
		}
	}
	railsDefaults := map[string]string{"RAILS_ENV": "production", "RACK_ENV": "production", "RAILS_LOG_TO_STDOUT": "1"}
	requests := map[string]DeployRequest{
		rails.ID: {Defaults: railsDefaults, DefaultsRuntime: "rails"},
		astro.ID: {Defaults: map[string]string{"NODE_ENV": "production", "ASTRO_TELEMETRY_DISABLED": "1"}, DefaultsRuntime: "astro"},
		// An older control panel sends the Go set without a runtime: it is
		// never copied into a Rails application.
		legacy.ID: {Defaults: map[string]string{"LOG_LEVEL": "info"}},
		// A set for another runtime is ignored.
		golang.ID: {Defaults: railsDefaults, DefaultsRuntime: "rails"},
	}
	for id, request := range requests {
		d, err := m.Enqueue(id, request)
		if err != nil {
			t.Fatal(id, err)
		}
		waitDeployment(t, m, d.ID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	saved := m.data.Apps[rails.ID]
	if !saved.DefaultsApplied || saved.Env["RAILS_ENV"] != "staging" || saved.EnvSources["RAILS_ENV"] != "" || saved.Env["RACK_ENV"] != "production" || saved.EnvSources["RACK_ENV"] != "workspace" || saved.Env["RAILS_LOG_TO_STDOUT"] != "1" {
		t.Fatalf("rails defaults %v %v", saved.Env, saved.EnvSources)
	}
	saved = m.data.Apps[astro.ID]
	if !saved.DefaultsApplied || saved.Env["NODE_ENV"] != "production" || saved.EnvSources["ASTRO_TELEMETRY_DISABLED"] != "workspace" {
		t.Fatalf("astro defaults %v %v", saved.Env, saved.EnvSources)
	}
	for _, id := range []string{legacy.ID, golang.ID} {
		saved = m.data.Apps[id]
		if !saved.DefaultsApplied || saved.Env["LOG_LEVEL"] != "" || saved.Env["RAILS_ENV"] != "" {
			t.Fatalf("%s received defaults of another runtime: %v", id, saved.Env)
		}
	}
	// A Rails set is validated with the Rails rules, never the Go allowlist.
	if _, err := m.queueLocked(m.data.Apps[rails.ID], DeployRequest{Defaults: map[string]string{"SECRET_KEY_BASE": "shared"}, DefaultsRuntime: "rails"}, false); err == nil {
		t.Fatal("credential accepted as a Rails default")
	}
}

func TestContainerApplyDefaultsUsesRuntimeRules(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("rails-apply")
	a.Runtime, a.Rails = "rails", &RailsConfig{HealthPath: "/up"}
	a.Env["RAILS_ENV"] = "staging"
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	patch := SettingsPatch{Branch: a.Branch, Domain: a.Domain, FromDefaults: true, OnlyMissing: true, EnvChanges: map[string]*string{"RAILS_ENV": ptrString("production"), "RACK_ENV": ptrString("production")}}
	if err := m.PatchSettings(a.ID, patch); err != nil {
		t.Fatal(err)
	}
	settings, _ := m.Settings(a.ID)
	if settings.EnvSources["RAILS_ENV"] != "" || settings.EnvSources["RACK_ENV"] != "workspace" {
		t.Fatal("missing-only Rails defaults", settings.EnvSources)
	}
	patch.EnvChanges = map[string]*string{"SECRET_KEY_BASE": ptrString("shared")}
	if err := m.PatchSettings(a.ID, patch); err == nil {
		t.Fatal("credential applied as a Rails default")
	}
}

func TestRuntimeDefaultsCredentialNamesBySegment(t *testing.T) {
	for _, name := range []string{"MASTER_KEY", "API_TOKEN", "SECRET_KEY_BASE", "DB_PASSWORD", "AWS_SECRETS", "SIGNING_PRIVATE_PEM"} {
		if err := ValidateRuntimeDefaults("rails", map[string]string{name: "x"}); err == nil {
			t.Fatal("credential-like default accepted:", name)
		}
	}
	for _, name := range []string{"MONKEY_MODE", "KEYBOARD_LAYOUT", "TOKENIZER_MODE", "RAILS_MAX_THREADS"} {
		if err := ValidateRuntimeDefaults("rails", map[string]string{name: "x"}); err != nil {
			t.Fatal("ordinary default refused:", name, err)
		}
	}
}
