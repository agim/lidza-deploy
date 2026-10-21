package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestStackMixedLiteralInterpolation(t *testing.T) {
	a := testApp("mixed")
	a.Env["LIVEKIT_API_SECRET"] = "private-value"
	env, err := stackEnvironment(a, StackService{Role: "app", Environment: map[string]string{"THURA_PUBLIC_HOSTS": "thura,${APP_DOMAIN}", "LIVEKIT_PUBLIC_URL": "wss://${APP_DOMAIN}/rtc"}})
	if err != nil || env["THURA_PUBLIC_HOSTS"] != "thura,mixed.example.com" || env["LIVEKIT_PUBLIC_URL"] != "wss://mixed.example.com/rtc" {
		t.Fatalf("mixed interpolation failed: %v", err)
	}
}

func TestStackPortReservationsAndMissingMasterKey(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("stack-owner")
	a.RuntimeProfile = "minimum"
	a.Env["LIDZA_MASTER_KEY"] = strings.Repeat("2", 64)
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	manifest := fixtureStackManifest()
	manifest.Services[0].PublicPorts = []StackPort{{Port: 25, Protocol: "tcp"}}
	other := testApp("other-stack")
	other.RuntimeProfile = "minimum"
	other.Current = &Release{ID: "old", Stack: &StackRelease{Manifest: manifest}}
	m.mu.Lock()
	m.data.Apps[other.ID] = other
	m.mu.Unlock()
	if _, err := m.prepareStack(context.Background(), a, &manifest); err == nil || !strings.Contains(err.Error(), "belongs to application") {
		t.Fatal("another app's fixed port was not reserved")
	}
	m.mu.Lock()
	delete(m.data.Apps, other.ID)
	saved := m.data.Apps[a.ID]
	delete(saved.Env, "LIDZA_MASTER_KEY")
	m.data.Apps[a.ID] = saved
	m.mu.Unlock()
	if _, err := m.prepareStack(context.Background(), saved, &manifest); err == nil || !strings.Contains(err.Error(), "LIDZA_MASTER_KEY") {
		t.Fatal("missing original master key accepted")
	}
}

func TestStackAttachExistingDatabaseDoesNotResize(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("attach-stack")
	a.RuntimeProfile = "minimum"
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	existing := Database{AppID: "shared", Mode: "local", Ready: true, Network: "lidza-db-shared", URL: "postgres://app:private@lidza-db-shared:5432/app?sslmode=disable", MemoryMiB: 1024, CPUs: "2", ResourceProfile: "legacy-dedicated", Backup: BackupPolicy{Keep: 1}}
	m.mu.Lock()
	m.data.Databases[existing.AppID] = existing
	m.mu.Unlock()
	if err := m.configureDatabase(a.ID, DatabaseRequest{Mode: "existing", ID: existing.AppID}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	got := m.data.Databases[existing.AppID]
	attached := m.data.Apps[a.ID]
	m.mu.Unlock()
	if got.MemoryMiB != 1024 || got.CPUs != "2" || got.ResourceProfile != "legacy-dedicated" || got.URL != existing.URL {
		t.Fatal("existing database was retuned or replaced")
	}
	if attached.Bindings["DATABASE_URL"] != existing.AppID {
		t.Fatal("managed database was not reused")
	}
}

func TestStackOfficePreservesVendorUser(t *testing.T) {
	args := stackPolicyArgs(StackService{Role: "office", Policy: "office"})
	for _, arg := range args {
		if arg == "--user" {
			t.Fatal("office policy overrides the pinned vendor image USER")
		}
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "no-new-privileges") || len(args) != 8 {
		t.Fatal("office file-capability policy has an unexpected privilege configuration")
	}
	for _, required := range []string{"--cap-drop ALL", "--cap-add CHOWN", "--cap-add FOWNER", "--cap-add SYS_CHROOT"} {
		if !strings.Contains(joined, required) {
			t.Fatal("office policy lost a security boundary", required)
		}
	}
}

func TestStackInitSupervisorCanSignalDroppedUID(t *testing.T) {
	args := strings.Join(stackPolicyArgs(StackService{Role: "app", Policy: "init"}), " ")
	if !strings.Contains(args, "--cap-add KILL") || !strings.Contains(args, "--cap-drop ALL") || !strings.Contains(args, "--security-opt no-new-privileges") {
		t.Fatal("init policy cannot supervise a dropped-UID worker within its PID namespace")
	}
}

func TestOfficeImageRequiresNonRootUser(t *testing.T) {
	for _, user := range []string{"", "root", "root:cool", "0", "0:0", "0000"} {
		if officeImageUserAllowed(user) {
			t.Fatal("root office image accepted", user)
		}
	}
	for _, user := range []string{"1001", "1001:1001", "cool"} {
		if !officeImageUserAllowed(user) {
			t.Fatal("non-root office image rejected", user)
		}
	}
}

func TestStackBootstrapReadinessIsExplicitAndFirstInstallOnly(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "docker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	for _, tc := range []struct {
		name         string
		eligible     bool
		status       int
		body         string
		health       int
		serviceFails bool
		want         bool
		bootstrap    bool
	}{
		{"guarded-first-install", true, 503, "Server setup is required. Open /setup", 200, false, true, true},
		{"established-release", false, 503, "Server setup is required. Open /setup", 200, false, false, false},
		{"unrelated-outage", true, 503, "database unavailable", 200, false, false, true},
		{"wrong-status", true, 500, "Server setup is required. Open /setup", 200, false, false, true},
		{"bootstrap-health-fails", true, 503, "Server setup is required. Open /setup", 503, false, false, true},
		{"service-unhealthy", true, 503, "Server setup is required. Open /setup", 200, true, false, true},
		{"genuine-ready", true, 200, "", 200, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code := "#!/bin/sh\nif [ \"$1\" = inspect ]; then echo '{\"status\":\"running\",\"exit\":0,\"oom\":false}'; exit 0; fi\nexit 0\n"
			if tc.serviceFails {
				code = "#!/bin/sh\nif [ \"$1\" = inspect ]; then echo '{\"status\":\"running\",\"exit\":0,\"oom\":false}'; exit 0; fi\nexit 1\n"
			}
			if err := os.WriteFile(script, []byte(code), 0700); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/readyz" {
					w.WriteHeader(tc.status)
					w.Write([]byte(tc.body))
					return
				}
				if r.URL.Path == "/api/v1/health" {
					w.WriteHeader(tc.health)
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			r := &Release{Port: port, Stack: &StackRelease{Bootstrap: tc.eligible, Manifest: StackManifest{BootstrapReadiness: &StackBootstrapReadiness{Path: "/api/v1/health", ReadyzBodyContains: "Server setup is required", SetupPath: "/setup"}}, Containers: []StackContainer{{Container: "fixture", Service: StackService{Name: "app", HealthCommand: []string{"probe"}}}}}}
			d := &Docker{readinessTimeout: 200 * time.Millisecond}
			err := d.waitStackReady(context.Background(), r)
			if (err == nil) != tc.want {
				t.Fatalf("bootstrap result = %v, wanted success %v", err, tc.want)
			}
			if r.Stack.Bootstrap != tc.bootstrap {
				t.Fatal("bootstrap readiness marker incorrect")
			}
		})
	}
	for _, path := range []string{"https://example.com/health", "//evil", "/../health", "/health?reset=true", "/readyz"} {
		m := fixtureStackManifest()
		m.BootstrapReadiness = &StackBootstrapReadiness{Path: path, ReadyzBodyContains: "Server setup is required"}
		if m.validate() == nil {
			t.Fatal("invalid bootstrap probe path accepted", path)
		}
	}
}
