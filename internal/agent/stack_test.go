package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureStackManifest() StackManifest {
	return StackManifest{Version: 1, RequiredEnvironment: []string{"LIDZA_MASTER_KEY"}, GeneratedSecrets: []string{"THURA_SETUP_TOKEN", "LIVEKIT_API_SECRET"}, Services: []StackService{
		{Name: "app", Role: "app", Policy: "init", Build: &StackBuild{Dockerfile: "Dockerfile"}, Aliases: []string{"thura"}, Environment: map[string]string{"FIXTURE_ROLE": "app"}, HTTPRoutes: []StackRoute{{Prefix: "/rtc", Port: 7880, StripPrefix: true}}, HealthCommand: []string{"/fixture", "--health"}},
		{Name: "mail", Role: "mail", Policy: "mail", Build: &StackBuild{Dockerfile: "Dockerfile"}, ShareAppNetwork: true, Environment: map[string]string{"FIXTURE_ROLE": "mail"}, Volumes: []StackVolume{{Name: "mail-state", Target: "/var/lib/fixture"}}, HealthCommand: []string{"/fixture", "--health"}},
		{Name: "office", Role: "office", Policy: "office", Build: &StackBuild{Dockerfile: "Dockerfile"}, Environment: map[string]string{"FIXTURE_ROLE": "office"}, HealthCommand: []string{"/fixture", "--health"}},
	}}
}
func TestStackManifestValidation(t *testing.T) {
	cases := map[string]func(*StackManifest){
		"host-policy":             func(m *StackManifest) { m.Services[0].Policy = "privileged" },
		"unconfined-role":         func(m *StackManifest) { m.Services[1].Role = "anything" },
		"literal-secret":          func(m *StackManifest) { m.Services[1].Environment["SECRET"] = "plaintext" },
		"master-replacement":      func(m *StackManifest) { m.GeneratedSecrets = append(m.GeneratedSecrets, "LIDZA_MASTER_KEY") },
		"path-traversal":          func(m *StackManifest) { m.Services[0].Build.Dockerfile = "../Dockerfile" },
		"unbounded-image":         func(m *StackManifest) { m.Services[0].Build = nil; m.Services[0].Image = "collabora/code:latest" },
		"unsafe-mount":            func(m *StackManifest) { m.Services[1].Volumes[0].Target = "/var/../run/docker.sock" },
		"shared-writers":          func(m *StackManifest) { m.Services[2].Volumes = m.Services[1].Volumes },
		"public-office":           func(m *StackManifest) { m.Services[2].PublicPorts = []StackPort{{9980, "tcp"}} },
		"bad-app-port":            func(m *StackManifest) { m.Services[0].PublicPorts = []StackPort{{22, "tcp"}} },
		"private-route-injection": func(m *StackManifest) { m.Services[0].HTTPRoutes[0].Published = "5432" },
		"alias-collision":         func(m *StackManifest) { m.Services[1].Aliases = []string{"thura"} },
		"missing-health":          func(m *StackManifest) { m.Services[0].HealthCommand = nil },
		"duplicate-role":          func(m *StackManifest) { m.Services[2].Role = "mail" },
	}
	if err := fixtureStackManifest().validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := fixtureStackManifest()
			mutate(&m)
			if m.validate() == nil {
				t.Fatal("unsafe manifest accepted")
			}
		})
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch"), 0600)
	_ = os.MkdirAll(filepath.Join(dir, "deploy"), 0700)
	raw, _ := json.Marshal(fixtureStackManifest())
	_ = os.WriteFile(filepath.Join(dir, "deploy", "lidza.stack.json"), raw, 0600)
	if _, err := loadStackManifest(dir); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, "Dockerfile"))
	_ = os.Symlink("/etc/passwd", filepath.Join(dir, "Dockerfile"))
	if _, err := loadStackManifest(dir); err == nil {
		t.Fatal("symlink Dockerfile accepted")
	}
	_ = os.WriteFile(filepath.Join(dir, "deploy", "lidza.stack.json"), []byte(`{"version":1,"privileged":true}`), 0600)
	if _, err := loadStackManifest(dir); err == nil {
		t.Fatal("unknown manifest option accepted")
	}
}
func TestStackProfilesAndEnvironment(t *testing.T) {
	for _, p := range stackProfiles {
		sum := p.App.MemoryMiB + p.Mail.MemoryMiB + p.Office.MemoryMiB + p.Database.MemoryMiB + p.ReserveMiB
		if sum != p.HostMiB {
			t.Fatalf("%s budget %d != %d", p.ID, sum, p.HostMiB)
		}
	}
	a := testApp("stack-env")
	a.Env["TOKEN"] = "private"
	env, err := stackEnvironment(a, StackService{Role: "mail", Environment: map[string]string{"TOKEN": "${TOKEN}", "PUBLIC": "wss://${APP_DOMAIN}/rtc"}})
	if err != nil || env["TOKEN"] != "private" || env["APP_SECRET"] != "" || env["PUBLIC"] != "wss://stack-env.example.com/rtc" {
		t.Fatalf("environment isolation failed: %v", err)
	}
	if _, err = stackEnvironment(a, StackService{Environment: map[string]string{"TOKEN": "${MISSING}"}}); err == nil {
		t.Fatal("missing reference accepted")
	}
}
func TestStackSecretsPersistAndRemainMasked(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("stack-secrets")
	a.RuntimeProfile = "minimum"
	a.Env["LIDZA_MASTER_KEY"] = strings.Repeat("1", 64)
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	manifest := fixtureStackManifest()
	prepared, err := m.prepareStack(context.Background(), a, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	token := prepared.Env["THURA_SETUP_TOKEN"]
	if token == "" {
		t.Fatal("missing generated token")
	}
	prepared2, err := m.prepareStack(context.Background(), a, &manifest)
	if err != nil || prepared2.Env["THURA_SETUP_TOKEN"] != token {
		t.Fatal("generated secret rotated")
	}
	if prepared2.Env["LIDZA_MASTER_KEY"] != a.Env["LIDZA_MASTER_KEY"] {
		t.Fatal("master key replaced")
	}
	settings, _ := m.Settings(a.ID)
	for _, v := range settings.Environment {
		if v.Key == "THURA_SETUP_TOKEN" && (v.Kind != EnvironmentCredential || v.Value != "") {
			t.Fatal("secret exposed")
		}
	}
	raw, _ := os.ReadFile(m.path())
	if strings.Contains(string(raw), token) {
		t.Fatal("secret persisted without encryption")
	}
	a.RuntimeProfile = "invalid"
	if a.Validate() == nil {
		t.Fatal("unknown profile accepted")
	}
	a.RuntimeProfile = "minimum"
	a.Preview = true
	if a.Validate() == nil {
		t.Fatal("preview stack accepted")
	}
}
