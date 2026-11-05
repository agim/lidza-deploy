package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAstroSettingsAreSealedDeferredAndPreserveCanonicalBinding(t *testing.T) {
	m, a, app := nativeStaticPublisherFixture(t)
	before, _ := os.ReadFile(filepath.Join(app, ".asdf-vars"))
	current, _ := os.Readlink(filepath.Join(app, "current"))
	value, secret := "next-title", "new-private-credential-value"
	if err := m.patchNativeSettings(a.ID, SettingsPatch{Branch: "preview", Domain: a.Domain, Astro: &AstroConfig{Mode: "static", OutputDir: "build"}, EnvChanges: map[string]*string{"TITLE": &value, "API_TOKEN": &secret}, EnvKindChanges: map[string]string{"TITLE": EnvironmentSetting}}); err != nil {
		t.Fatal(err)
	}
	settings, err := m.nativeSettings(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(settings)
	if settings.Branch != "preview" || settings.Astro.OutputDir != "build" || !settings.Capabilities.Configure || !settings.SettingsApplyOnDeploy || strings.Contains(string(raw), secret) {
		t.Fatal("incorrect or unmasked settings", string(raw))
	}
	after, _ := os.ReadFile(filepath.Join(app, ".asdf-vars"))
	link, _ := os.Readlink(filepath.Join(app, "current"))
	if string(before) != string(after) || current != link {
		t.Fatal("save modified the live application")
	}
	// Reload the sealed state using the existing manager key, without starting a
	// worker or making a service call.
	m.data.NativeSettings = nil
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(m.cfg.DataDir, "state.json"))
	if strings.Contains(string(data), secret) {
		t.Fatal("credential saved as plaintext")
	}
	values, err := m.nativeStaticEnvironment(a)
	if err != nil || values["API_TOKEN"] != secret || values["SECRET_KEY"] != "preserve-original-key" {
		t.Fatal("environment did not survive sealed reload", err)
	}
	canonical, err := m.nativeApp(a.ID)
	if err != nil || canonical.Branch != a.Branch {
		t.Fatal("settings changed the host identity", err)
	}
	m.Close() // Inspect the queue without a competing worker.
	d, err := m.enqueueNativeStatic(a.ID, DeployRequest{})
	if err != nil || d.Branch != "preview" {
		t.Fatal("deployment ignored the saved branch", err)
	}
	if err := m.patchNativeSettings(a.ID, SettingsPatch{Branch: "main"}); err == nil {
		t.Fatal("settings changed during an active build")
	}
	j := <-m.queue
	if j.app.Branch != "preview" || j.app.Env["API_TOKEN"] != secret {
		t.Fatal("queued build lost settings")
	}
}

func TestNativeSettingsRejectUnapprovedAndHostChanges(t *testing.T) {
	for _, scenario := range []string{"unapproved", "rails", "hostname", "runtime", "managed-env", "database", "output"} {
		t.Run(scenario, func(t *testing.T) {
			m, a, _ := nativeStaticPublisherFixture(t)
			p := SettingsPatch{}
			v := "new"
			switch scenario {
			case "unapproved":
				m.cfg.Native.StaticDeployments = nil
			case "rails":
				a.Runtime = "rails"
				m.data.NativeApps[a.ID] = a
			case "hostname":
				p.Domain = "other.example.com"
			case "runtime":
				p.Astro = &AstroConfig{Mode: "node"}
			case "managed-env":
				p.EnvChanges = map[string]*string{"NODE_VERSION": &v}
			case "database":
				v = "postgres://user:pass@db/app"
				p.EnvChanges = map[string]*string{"DATABASE_URL": &v}
			case "output":
				p.Astro = &AstroConfig{OutputDir: "../outside"}
			}
			if err := m.patchNativeSettings(a.ID, p); err == nil {
				t.Fatal("unapproved native change accepted")
			}
			if len(m.data.NativeSettings) != 0 {
				t.Fatal("rejected settings were persisted")
			}
		})
	}
}

func TestNativeStaticInheritsHostPinAndRejectsBuildTimeMutation(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(map[bool]string{false: "inherit", true: "build-mutation"}[mutate], func(t *testing.T) {
			m, a, app := nativeStaticPublisherFixture(t)
			if err := os.WriteFile(filepath.Join(app, "current/.node-version"), []byte("26.12.0\n"), 0644); err != nil {
				t.Fatal(err)
			}
			base := m.nativeBuildCommand
			m.nativeBuildCommand = func(ctx context.Context, a NativeApplication, path string, args []string) (string, error) {
				pin, err := os.ReadFile(filepath.Join(path, ".node-version"))
				if err != nil || string(pin) != "26.12.0\n" {
					t.Fatal("build did not inherit the host pin", err)
				}
				out, err := base(ctx, a, path, args)
				if mutate && strings.Join(args, " ") == "npm run build" {
					os.WriteFile(filepath.Join(path, ".node-version"), []byte("24.0.0"), 0644)
				}
				return out, err
			}
			_, err := m.prepareNativeStatic(context.Background(), a, newID(), "")
			if mutate && err == nil || !mutate && err != nil {
				t.Fatal("unexpected pin validation result", err)
			}
		})
	}
}

func TestNativeAstroStaticApplyWorkspaceDefaults(t *testing.T) {
	m, a, _ := nativeStaticPublisherFixture(t)
	value := func(s string) *string { return &s }
	if err := m.patchNativeSettings(a.ID, SettingsPatch{Domain: a.Domain, EnvChanges: map[string]*string{"NODE_ENV": value("development")}}); err != nil {
		t.Fatal(err)
	}
	defaults := SettingsPatch{FromDefaults: true, OnlyMissing: true, EnvChanges: map[string]*string{"NODE_ENV": value("production"), "ASTRO_TELEMETRY_DISABLED": value("1")}}
	if err := m.patchNativeSettings(a.ID, defaults); err != nil {
		t.Fatal(err)
	}
	values, err := m.nativeStaticEnvironment(a)
	if err != nil || values["NODE_ENV"] != "development" || values["ASTRO_TELEMETRY_DISABLED"] != "1" || values["SECRET_KEY"] != "preserve-original-key" {
		t.Fatal("missing-only defaults", values, err)
	}
	defaults.OnlyMissing = false
	if err := m.patchNativeSettings(a.ID, defaults); err != nil {
		t.Fatal(err)
	}
	if values, _ := m.nativeStaticEnvironment(a); values["NODE_ENV"] != "production" {
		t.Fatal("approved replacement not applied", values["NODE_ENV"])
	}
	for name, changes := range map[string]map[string]*string{
		"host key":   {"PORT": value("1")},
		"credential": {"API_TOKEN": value("shared")},
		"imported":   {"SECRET_KEY": value("shared")},
		"delete":     {"NODE_ENV": nil},
	} {
		if err := m.patchNativeSettings(a.ID, SettingsPatch{FromDefaults: true, EnvChanges: changes}); err == nil {
			t.Fatal("accepted default", name)
		}
	}
}
