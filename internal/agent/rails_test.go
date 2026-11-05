package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/updates"
)

func railsApp() App {
	a := testApp("rails-app")
	a.Runtime = "rails"
	a.Env = map[string]string{"RAILS_MASTER_KEY": "original-rails-key"}
	return a
}

func TestRailsIdentityAndSecretsSurviveRestart(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := railsApp()
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"LIDZA_MASTER_KEY", "AUTH_SECRET", "DB_MIGRATE"} {
		if m.data.Apps[a.ID].Env[key] != "" {
			t.Fatalf("unexpected Go setting %s", key)
		}
	}
	if err := m.GenerateMasterKey(a.ID); err == nil {
		t.Fatal("Rails key generation allowed")
	}
	s, err := m.Settings(a.ID)
	if err != nil || s.Runtime != "rails" || s.MasterKeyGenerationSupported || s.StackProfilesSupported {
		t.Fatalf("settings: %+v %v", s, err)
	}
	encoded, _ := json.Marshal(s)
	if strings.Contains(string(encoded), "original-rails-key") {
		t.Fatal("Rails key leaked")
	}
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[a.ID].Env["RAILS_MASTER_KEY"] != "original-rails-key" {
		t.Fatal("Rails key changed on reload")
	}
	update := railsApp()
	update.Env = map[string]string{"LOG_LEVEL": "info"}
	if err := m.Upsert(update); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[a.ID].Env["RAILS_MASTER_KEY"] != "original-rails-key" {
		t.Fatal("omitted Rails key was cleared by a full update")
	}
	a.Runtime = "go"
	a.Rails = nil
	if err := m.Upsert(a); err == nil {
		t.Fatal("language change allowed")
	}
}

func TestRailsValidation(t *testing.T) {
	a := railsApp()
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"https://example.com/up", "//evil/up", "/up?secret=1", "/../up", "/up\nheader"} {
		a.Rails = &RailsConfig{HealthPath: path}
		if a.Validate() == nil {
			t.Fatalf("accepted health path %q", path)
		}
	}
	a.Rails = nil
	a.RuntimeProfile = "minimum"
	if a.Validate() == nil {
		t.Fatal("Go stack on Rails accepted")
	}
	a.RuntimeProfile = ""
	a.CustomerHosts = true
	if a.Validate() == nil {
		t.Fatal("framework host approval on Rails accepted")
	}
}

func TestRailsBuildContextExcludesKeysAfterNegations(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Gemfile", "Gemfile.lock", "Dockerfile", "bin/rails", "Dockerfile.dockerignore"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte("!config/master.key\n!**/.env\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareRailsBuild(source); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".dockerignore", "Dockerfile.dockerignore"} {
		data, _ := os.ReadFile(filepath.Join(source, name))
		if !strings.HasSuffix(string(data), "**/*.key\n") {
			t.Fatalf("unsafe %s", name)
		}
	}
	if err := os.Remove(filepath.Join(source, "Dockerfile.dockerignore")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, "Gemfile"), filepath.Join(source, "Dockerfile.dockerignore")); err != nil {
		t.Fatal(err)
	}
	if prepareRailsBuild(source) == nil {
		t.Fatal("symlink Docker ignore accepted")
	}
}

func TestRailsReadinessUsesRecordedPathAndOldGoReleasesStillUseReadyz(t *testing.T) {
	path := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { path = r.URL.Path; w.WriteHeader(200) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	d := &Docker{}
	r := &Release{Port: u.Port(), HealthPath: "/up"}
	if err := d.Ready(context.Background(), r); err != nil || path != "/up" {
		t.Fatalf("Rails readiness %s: %v", path, err)
	}
	r.HealthPath = ""
	if err := d.Ready(context.Background(), r); err != nil || path != "/readyz" {
		t.Fatalf("Go readiness %s: %v", path, err)
	}
}

func TestRailsRuntimeAndFilesUseRailsIdentity(t *testing.T) {
	a := railsApp()
	a.StorageVolume = "lidza-storage-rails-app"
	args := strings.Join(runtimeArgs(a), " ") + strings.Join(storageMountArgs(a), " ")
	for _, required := range []string{"/rails/storage", "/rails/tmp", "/rails/log", "RAILS_ENV=production", "PORT=3000"} {
		if !strings.Contains(args, required) {
			t.Fatalf("missing %s", required)
		}
	}
	if a.runtimeUser() != "1000:1000" || strings.Contains(args, "LIDZA_MODE") {
		t.Fatal("incorrect Rails container identity")
	}
	a.Env["CACHE_URL"] = "redis://:private-password@cache:6379"
	if a.runtimeEnvironment()["REDIS_URL"] != a.Env["CACHE_URL"] {
		t.Fatal("managed cache not available to Rails workers")
	}
	if strings.Contains(strings.Join(runtimeArgs(a), " "), "private-password") {
		t.Fatal("cache credential placed in container command arguments")
	}
}

func TestSignedDistributionCannotQueuePublicUpgrades(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	saved := upgradeTrust
	upgradeTrust = func() updates.Trust { return updates.Trust{Keys: []ed25519.PublicKey{pub}} }
	defer func() { upgradeTrust = saved }()
	if status := m.upgradeStatus(); status.Supported || status.Channel != "private" {
		t.Fatal("public upgrades enabled in a signed distribution", status)
	}
	if _, err := LatestRelease(httptest.NewRequest("GET", "/", nil)); err == nil {
		t.Fatal("signed distribution queried public releases")
	}
}
