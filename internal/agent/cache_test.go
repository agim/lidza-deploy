package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza/pkg/config"
	"github.com/agim/lidza/pkg/credentials"
)

func cacheManifest(t *testing.T, packs string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lidza.json"), []byte(`{"name":"fixture","frontend":{"template":"htmx"},"packs":`+packs+`}`), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCacheAutomaticFirstFailedAndExistingApplication(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("cache-test")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.cacheProvision = func(context.Context, CacheResource) error { return nil }
	manifest := cacheManifest(t, `["lidza/cache"]`)
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	m.mu.Unlock()
	prepared, err := m.preflightRuntime(context.Background(), a, manifest)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	resource := m.data.Caches[a.ID]
	m.mu.Unlock()
	if len(resource.LocalPassword) != 64 || !strings.Contains(prepared.Env["CACHE_URL"], resource.LocalPassword) || !slices.Contains(prepared.Networks, resource.Network) {
		t.Fatal("first deployment did not attach an authenticated cache and network")
	}
	settings, _ := m.Settings(a.ID)
	view, _ := json.Marshal(settings)
	if strings.Contains(string(view), resource.LocalPassword) || !settings.Cache.Managed {
		t.Fatal("cache secret exposed through settings")
	}
	state, _ := os.ReadFile(m.path())
	if strings.Contains(string(state), resource.LocalPassword) {
		t.Fatal("cache secret persisted in plaintext")
	}
	// Failed apps have no Current release, but retain the saved connection.
	m.mu.Lock()
	saved := m.data.Apps[a.ID]
	m.mu.Unlock()
	if _, err = m.preflightRuntime(context.Background(), saved, manifest); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	stable := m.data.Caches[a.ID]
	m.mu.Unlock()
	if stable.URL != resource.URL {
		t.Fatal("retry rotated cache credentials")
	}
	other, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.mu.Lock()
	persisted := other.data.Caches[a.ID]
	other.mu.Unlock()
	if persisted.URL != resource.URL {
		t.Fatal("restart lost cache credentials")
	}
	// An interrupted switch recovers the previously working network and URL.
	m.mu.Lock()
	_, _, err = m.reserveCacheLocked(a.ID, CacheRequest{Mode: "external", URL: "rediss://:pending-secret@pending.example.com:6379"}, false)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	recovered.mu.Lock()
	restored := recovered.data.Caches[a.ID]
	recovered.mu.Unlock()
	recovered.Close()
	if !restored.Ready || restored.Operation || restored.URL != resource.URL || restored.Network != resource.Network {
		t.Fatal("restart lost working cache during a switch")
	}
	m.mu.Lock()
	m.data.Caches[a.ID] = resource
	err = m.save()
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, EnvChanges: map[string]*string{"CACHE_URL": nil}}); err == nil {
		t.Fatal("managed connection deleted through env editor")
	}
	if err = m.Upsert(testApp(a.ID)); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	updated := m.data.Apps[a.ID]
	m.mu.Unlock()
	if updated.Env["CACHE_URL"] != resource.URL || updated.Env["CACHE_PREFIX"] != prepared.Env["CACHE_PREFIX"] {
		t.Fatal("full app update lost cache connection or namespace")
	}
	// Existing successful apps reload after a cache connection is changed.
	d, err := m.Enqueue(a.ID, DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waitDeployment(t, m, d.ID)
	previous := m.Current(a.ID).ID
	if err = m.configureCache(a.ID, CacheRequest{Mode: "external", URL: "rediss://:external-secret@cache.example.com:6379/0"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && m.Current(a.ID).ID == previous {
		time.Sleep(10 * time.Millisecond)
	}
	if m.Current(a.ID).ID == previous {
		t.Fatal("cache attachment did not reload current app")
	}
	// An invalid attachment must leave the healthy connection in place, and not echo credentials.
	m.cacheProvision = func(context.Context, CacheResource) error { return errors.New("private diagnostic secret") }
	m.mu.Lock()
	next, old, err := m.reserveCacheLocked(a.ID, CacheRequest{Mode: "external", URL: "rediss://:bad-secret@other.example.com:6379"}, false)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	err = m.finishCache(a.ID, next, old, errors.New("bad-secret"), true)
	m.mu.Lock()
	failed := m.data.Caches[a.ID]
	actual := m.data.Apps[a.ID].Env["CACHE_URL"]
	m.mu.Unlock()
	if err == nil || strings.Contains(err.Error(), "bad-secret") || failed.Operation || actual != old.URL || !failed.Ready {
		t.Fatal("failed cache switch displaced the working connection or leaked credentials")
	}
}

func TestCacheManualConfigurationAndProductionPreflight(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("custom-cache")
	a.Env["CACHE_URL"] = "rediss://:custom-secret@custom.example.com:6379"
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.cacheProvision = func(context.Context, CacheResource) error {
		t.Fatal("manual URL replaced with automatic cache")
		return nil
	}
	prepared, err := m.preflightRuntime(context.Background(), a, cacheManifest(t, `["lidza/cache"]`))
	if err != nil || prepared.Env["CACHE_URL"] != a.Env["CACHE_URL"] {
		t.Fatal("manual connection lost", err)
	}
	cfg := &config.Config{Packs: []string{"lidza/db", "lidza/auth", "lidza/cache", "lidza/mail"}}
	err = productionRequirements(cfg, map[string]string{"CACHE_URL": "memory"})
	if err == nil {
		t.Fatal("missing production configuration accepted")
	}
	for _, key := range []string{"DATABASE_URL", "AUTH_SECRET", "CACHE_URL", "MAIL_PROVIDER"} {
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("preflight did not report %s together with other requirements", key)
		}
	}
	for _, raw := range []string{"redis://cache.example.com", "redis://:password@localhost", "redis://:password@127.0.0.1", "https://:password@cache.example.com", "rediss://:password@cache.example.com#fragment"} {
		if err := (CacheRequest{Mode: "external", URL: raw}).Validate(); err == nil {
			t.Fatalf("invalid connection accepted: %s", raw)
		}
	}
}

func TestCacheInvalidEnvironmentDoesNotLeaveOperationHung(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("full-env")
	// Leave room for the generated AUTH_SECRET and default DB_MIGRATE.
	for i := 0; i < 97; i++ {
		a.Env["VAR"+strings.Repeat("X", i+1)] = "value"
	}
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	next, old, err := m.reserveCacheLocked(a.ID, CacheRequest{Mode: "local"}, false)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = m.finishCache(a.ID, next, old, nil, false); err == nil {
		t.Fatal("overfull env accepted")
	}
	m.mu.Lock()
	resource := m.data.Caches[a.ID]
	original := m.data.Apps[a.ID]
	m.mu.Unlock()
	if resource.Operation || resource.Ready || original.Env["CACHE_URL"] != "" || resource.LocalPassword == "" {
		t.Fatal("failed attachment left operation stuck or lost durable resource identity")
	}
}

func TestCachePreflightReadsSealedProductionCredentialsWithManualKey(t *testing.T) {
	dir := cacheManifest(t, `["lidza/cache"]`)
	if err := os.Mkdir(filepath.Join(dir, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	sealed, err := credentials.Encrypt(key, []byte("CACHE_URL: memory\nproduction:\n  CACHE_URL: rediss://:sealed-secret@cache.example.com:6379\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, credentials.File), []byte(sealed), 0600); err != nil {
		t.Fatal(err)
	}
	m := testManager(t, &fakeRuntime{})
	a := testApp("sealed-cache")
	if _, err = m.preflightRuntime(context.Background(), a, dir); err == nil || !strings.Contains(err.Error(), "LIDZA_MASTER_KEY") {
		t.Fatal("missing manual master key not reported", err)
	}
	a.Env["LIDZA_MASTER_KEY"] = hex.EncodeToString(key)
	m.cacheProvision = func(context.Context, CacheResource) error { t.Fatal("sealed existing service overwritten"); return nil }
	if _, err = m.preflightRuntime(context.Background(), a, dir); err != nil {
		t.Fatal("production credential resolution failed", err)
	}
	if len(m.data.Caches) != 0 {
		t.Fatal("cache auto-created despite sealed connection")
	}
}
