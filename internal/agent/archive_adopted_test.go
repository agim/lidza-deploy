package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agim/lidza/packs/storage"
)

// adoptedArchiveFixture is the shared deployment user's home with two
// adopted Rails applications: MyRails (being deleted) and Other. They share
// a Redis database number and a PostgreSQL role (a superuser).
type adoptedArchiveFixture struct {
	m        *Manager
	home     string
	appsRoot string
	mu       sync.Mutex
	calls    [][]string
	scripts  []string
}

func newAdoptedArchiveFixture(t *testing.T) *adoptedArchiveFixture {
	t.Helper()
	m, app := nativeFixture(t)
	f := &adoptedArchiveFixture{m: m, home: m.cfg.Native.Root, appsRoot: filepath.Join(t.TempDir(), "apps")}
	m.cfg.Native.AppsRoot = f.appsRoot
	m.cfg.Native.ServiceControl = true
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(app, ".asdf-vars"), "PORT=9180\nREDIS_URL=redis://localhost:6379/3\nCACHE_REDIS_URL=redis://localhost:6379/1\n"+
		"DATABASE_URL=postgres://myrails:pw@127.0.0.1:5432/myrails_production\nQUEUE_DATABASE_URL=postgres://deploy:pw@127.0.0.1:5432/myrails_queue\n")
	write(filepath.Join(app, "shared", "storage", "upload.txt"), "kept")
	other := filepath.Join(f.home, "Other")
	write(filepath.Join(other, "releases", "one", "Gemfile"), "source 'https://rubygems.org'\n")
	if err := os.Symlink(filepath.Join(other, "releases", "one"), filepath.Join(other, "current")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(other, ".asdf-vars"), "REDIS_URL=redis://127.0.0.1:6379/1\nDATABASE_URL=postgres://deploy:pw@localhost/other_production\n")
	write(filepath.Join(f.home, ".config", "systemd", "user", "other-server.service"), "[Service]\nWorkingDirectory="+filepath.Join(other, "current")+"\n")
	for _, a := range []NativeApplication{{ID: "my-rails", Name: "MyRails", Domain: "rails.example.com", Repository: "acme/rails"}, {ID: "other", Name: "Other", Domain: "other.example.com", Repository: "acme/other"}} {
		if _, err := m.nativeRegistration(a); err != nil {
			t.Fatal(err)
		}
	}
	m.nativeCommand = func(_ context.Context, args []string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, slices.Clone(args))
		if len(args) > 2 && args[1] == "--user" && args[2] == "show" {
			return "Id=myrails-server.service\nActiveState=active\nSubState=running\nUnitFileState=enabled\n\nId=myrails-worker.service\nActiveState=inactive\nSubState=dead\nUnitFileState=disabled", nil
		}
		return "", nil
	}
	previous := nativeHostDataHost
	nativeHostDataHost = func(*Manager) nativeHostData {
		return nativeHostData{root: t.TempDir(), run: func(_ context.Context, stdin, name string, args ...string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.scripts = append(f.scripts, name+" "+strings.Join(args, " ")+"\n"+stdin)
			switch {
			case strings.Contains(stdin, "rolsuper"):
				if strings.Contains(stdin, "'deploy'") {
					return "t", nil
				}
				return "f", nil
			case strings.Contains(stdin, "has_database_privilege"):
				return "t", nil
			case strings.Contains(stdin, "FLUSHDB"):
				return "OK\nOK\n", nil
			}
			return "", nil
		}}
	}
	oldStore, oldCaddy := newArchiveObjectStore, archiveCaddyStorage
	newArchiveObjectStore = func(storage.Config) (archiveObjectStore, error) {
		return &fakeArchiveStore{objects: map[string]int64{}}, nil
	}
	archiveCaddyStorage = func(context.Context, *Manager) string { return "" }
	t.Cleanup(func() { nativeHostDataHost, newArchiveObjectStore, archiveCaddyStorage = previous, oldStore, oldCaddy })
	// The adopted database the agent backs up.
	m.mu.Lock()
	m.data.NativeDatabases = map[string]NativeDatabaseBinding{"myrails-db": {Policy: NativeDatabasePolicy{Enabled: true, AppID: "my-rails", EnvironmentKey: "DATABASE_URL", Roles: []string{"myrails"}, Host: "127.0.0.1", Port: "5432"}}}
	m.data.Databases["myrails-db"] = Database{AppID: "myrails-db", Mode: "external", URL: "postgres://myrails:pw@127.0.0.1:5432/myrails_production", Ready: true,
		Backup: BackupPolicy{Hours: 24, Keep: 1}, Backups: []BackupRecord{{ID: "b1", Size: 4}}}
	m.mu.Unlock()
	write(filepath.Join(m.backupDir("myrails-db"), "b1.dump"), "PGDM")
	return f
}

func (f *adoptedArchiveFixture) ran(want ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.calls, func(args []string) bool { return slices.Equal(args, want) })
}

func (f *adoptedArchiveFixture) script() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.scripts, "\n")
}

func TestArchiveAdoptedAppLeavesTheSharedAccountAndSharedDataAlone(t *testing.T) {
	f := newAdoptedArchiveFixture(t)
	m := f.m
	units := filepath.Join(f.home, ".config", "systemd", "user")
	server, _ := os.ReadFile(filepath.Join(units, "myrails-server.service"))

	view, err := m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{DeletedBy: "op"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Kind != archiveKindAdopted || view.Phase != archivePhaseArchived {
		t.Fatalf("view %+v", view)
	}
	// Units: stopped, disabled and their files removed; nothing else.
	for _, unit := range []string{"myrails-server.service", "myrails-worker.service"} {
		if !f.ran("systemctl", "--user", "disable", "--now", "--quiet", unit) {
			t.Fatalf("%s not disabled", unit)
		}
		if _, err := os.Stat(filepath.Join(units, unit)); !os.IsNotExist(err) {
			t.Fatalf("%s file kept", unit)
		}
	}
	for _, kept := range []string{filepath.Join(units, "unrelated.service"), filepath.Join(units, "other-server.service"), filepath.Join(f.home, "Other", ".asdf-vars")} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s was touched: %v", kept, err)
		}
	}
	// The shared account's home stays; the application's directory moved.
	if info, err := os.Stat(f.home); err != nil || !info.IsDir() {
		t.Fatal("the deployment user's home is gone")
	}
	if _, err := os.Stat(filepath.Join(f.home, "MyRails")); !os.IsNotExist(err) {
		t.Fatal("the application's directory stayed")
	}
	files := filepath.Join(f.appsRoot, ".archived", view.ID, "root")
	if data, err := os.ReadFile(filepath.Join(files, "shared", "storage", "upload.txt")); err != nil || string(data) != "kept" {
		t.Fatalf("files not kept: %v", err)
	}
	if strings.Contains(strings.Join(slices.Concat(f.callsOf("userdel"), f.callsOf("usermod")), " "), "agim") {
		t.Fatal("the shared account was changed")
	}
	script := f.script()
	// Redis: the exclusive database number is flushed, the shared one is not.
	if !strings.Contains(script, "SELECT 3\nFLUSHDB") || strings.Contains(script, "SELECT 1\nFLUSHDB") || strings.Contains(script, "FLUSHALL") {
		t.Fatalf("redis:\n%s", script)
	}
	// PostgreSQL: the exclusive role loses its login; the shared superuser
	// role keeps it and only loses CONNECT on this application's database.
	if !strings.Contains(script, `ALTER ROLE "myrails" NOLOGIN;`) || strings.Contains(script, `ALTER ROLE "deploy"`) ||
		!strings.Contains(script, `REVOKE CONNECT ON DATABASE "myrails_queue" FROM "deploy";`) || !strings.Contains(script, `REVOKE CONNECT ON DATABASE "myrails_queue" FROM PUBLIC;`) ||
		strings.Contains(script, "DROP ") || strings.Contains(script, "other_production") {
		t.Fatalf("postgres:\n%s", script)
	}
	if item, ok := itemNamed(view.Kept, "database", "myrails_queue"); !ok || !strings.Contains(item.Detail, "superuser") {
		t.Fatalf("kept %+v", view.Kept)
	}
	if item, ok := itemNamed(view.Deleted, "cache", "Redis 127.0.0.1:6379 database 1"); !ok || !strings.Contains(item.Detail, "kept: other") {
		t.Fatalf("deleted %+v", view.Deleted)
	}
	if _, ok := itemNamed(view.Deleted, "backups", "myrails-db"); !ok {
		t.Fatalf("backups not deleted: %+v", view.Deleted)
	}
	if _, err := os.Stat(m.backupDir("myrails-db")); !os.IsNotExist(err) {
		t.Fatal("local backups kept")
	}
	m.mu.Lock()
	_, registered := m.data.NativeApps["my-rails"]
	_, otherRegistered := m.data.NativeApps["other"]
	_, database := m.data.Databases["myrails-db"]
	_, binding := m.data.NativeDatabases["myrails-db"]
	m.mu.Unlock()
	if registered || !otherRegistered || database || binding {
		t.Fatalf("registered=%v other=%v database=%v binding=%v", registered, otherRegistered, database, binding)
	}
	if _, err := m.nativeRegistration(NativeApplication{ID: "my-rails", Name: "Other", Domain: "x.example.com"}); err == nil {
		t.Fatal("an archived id was registered again")
	}

	// Restore reverses every step.
	f.mu.Lock()
	f.calls, f.scripts = nil, nil
	f.mu.Unlock()
	result, err := m.RestoreArchive(context.Background(), view.ID, ArchiveRestoreRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Adopted || !result.Native || result.AppID != "my-rails" {
		t.Fatalf("result %+v", result)
	}
	if restored, err := os.ReadFile(filepath.Join(units, "myrails-server.service")); err != nil || string(restored) != string(server) {
		t.Fatalf("unit file not restored: %v", err)
	}
	if !f.ran("systemctl", "--user", "daemon-reload") || !f.ran("systemctl", "--user", "enable", "--now", "--no-block", "--quiet", "myrails-server.service") {
		t.Fatalf("server not started: %v", f.calls)
	}
	f.mu.Lock()
	workerStarted := slices.ContainsFunc(f.calls, func(args []string) bool {
		return slices.Contains(args, "myrails-worker.service") && (slices.Contains(args, "start") || slices.Contains(args, "enable"))
	})
	f.mu.Unlock()
	if workerStarted {
		t.Fatal("the disabled worker was started")
	}
	script = f.script()
	if !strings.Contains(script, `ALTER ROLE "myrails" LOGIN;`) || !strings.Contains(script, `GRANT CONNECT ON DATABASE "myrails_queue" TO "deploy";`) || !strings.Contains(script, `GRANT CONNECT ON DATABASE "myrails_queue" TO PUBLIC;`) {
		t.Fatalf("logins not restored:\n%s", script)
	}
	if data, err := os.ReadFile(filepath.Join(f.home, "MyRails", "shared", "storage", "upload.txt")); err != nil || string(data) != "kept" {
		t.Fatalf("files not restored: %v", err)
	}
	if _, err := m.nativeApp("my-rails"); err != nil {
		t.Fatalf("the restored application's bindings do not verify: %v", err)
	}
	m.mu.Lock()
	restored, database := m.data.Databases["myrails-db"]
	_, binding = m.data.NativeDatabases["myrails-db"]
	m.mu.Unlock()
	if !database || !binding || len(restored.Backups) != 0 || restored.NextBackup.IsZero() {
		t.Fatalf("database record %+v binding=%v", restored, binding)
	}
}

func (f *adoptedArchiveFixture) callsOf(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, args := range f.calls {
		if len(args) > 0 && args[0] == name {
			out = append(out, strings.Join(args, " "))
		}
	}
	return out
}

func TestArchiveAdoptedAppFlushesNothingWhenEveryCacheIsShared(t *testing.T) {
	f := newAdoptedArchiveFixture(t)
	app := filepath.Join(f.home, "MyRails")
	if err := os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("REDIS_URL=redis://localhost:6379/1\nDATABASE_URL=postgres://deploy:pw@127.0.0.1:5432/other_production\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.m.mu.Lock()
	delete(f.m.data.NativeDatabases, "myrails-db")
	delete(f.m.data.Databases, "myrails-db")
	f.m.mu.Unlock()
	view, err := f.m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	script := f.script()
	if strings.Contains(script, "FLUSHDB") || strings.Contains(script, "REVOKE") || strings.Contains(script, "ALTER ROLE") {
		t.Fatalf("shared data changed:\n%s", script)
	}
	if item, ok := itemNamed(view.Kept, "database", "other_production"); !ok || !strings.Contains(item.Detail, "unchanged") {
		t.Fatalf("kept %+v", view.Kept)
	}
}

func TestAdoptedRouteServesOnlyTheApplicationsOwnRoutes(t *testing.T) {
	hosts := []string{"rails.example.com", "www.rails.example.com"}
	route := func(id string, hosts []string, dial string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"@id": id, "match": []any{map[string]any{"host": hosts}}, "handle": []any{map[string]any{"handler": "subroute", "routes": []any{map[string]any{"handle": []any{map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": dial}}}}}}}}})
		return raw
	}
	for _, tc := range []struct {
		name           string
		raw            json.RawMessage
		serves, shared bool
	}{
		{"own route", route("", []string{"rails.example.com"}, "127.0.0.1:9180"), true, false},
		{"own route by socket", route("", hosts, "unix//home/deploy/MyRails/shared/tmp/puma.sock"), true, false},
		{"shared with another site", route("", []string{"rails.example.com", "other.example.com"}, "127.0.0.1:9180"), false, true},
		{"another application", route("", []string{"other.example.com"}, "127.0.0.1:9180"), false, false},
		{"another upstream", route("", hosts, "127.0.0.1:9999"), false, false},
		{"the agent's own route", route("lidza-app-my-rails", hosts, "127.0.0.1:9180"), false, false},
	} {
		serves, shared := adoptedRouteServes(tc.raw, hosts, "9180", "/home/deploy/MyRails")
		if serves != tc.serves || shared != tc.shared {
			t.Fatalf("%s: serves=%v shared=%v", tc.name, serves, shared)
		}
	}
}

func TestRedisTargetsParseLocalAndExternal(t *testing.T) {
	targets := redisTargets(map[string]string{"REDIS_URL": "redis://:secret@localhost:6380/4", "SIDEKIQ_REDIS": "unix:///run/redis/redis.sock?db=2", "CACHE_URL": "rediss://cache.example.com:6379/0", "OTHER": "postgres://x"})
	if len(targets) != 3 {
		t.Fatalf("targets %+v", targets)
	}
	byKey := map[string]redisTarget{}
	for _, target := range targets {
		byKey[target.key()] = target
	}
	if tcp := byKey["tcp:6380/4"]; tcp.Password != "secret" {
		t.Fatalf("tcp %+v", byKey)
	}
	if _, ok := byKey["unix:/run/redis/redis.sock/2"]; !ok {
		t.Fatalf("unix %+v", byKey)
	}
	if external := byKey["external:cache.example.com/0"]; external.External != "cache.example.com" {
		t.Fatalf("external %+v", byKey)
	}
}
