package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/retention"
	"github.com/agim/lidza/packs/storage"
)

// adoptedPrivateAddressFixture is the adopted fixture with the database
// URL of an adopted host: the server's private address, a role of its own
// and no database record of the agent (only the environment names it).
func adoptedPrivateAddressFixture(t *testing.T) *leftoverFixture {
	t.Helper()
	f := newLeftoverFixture(t)
	app := filepath.Join(f.home, "MyRails")
	if err := os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("PORT=9180\n"+
		"DATABASE_URL=postgres://user_0b88c2cb4895:p%40ss@10.1.2.3:5432/shop_db\n"+
		"QUEUE_DATABASE_URL=postgres://deploy:pw@10.1.2.3:5432/myrails_queue\n"+
		"CACHE_DATABASE_URL=postgres://deploy:pw@10.1.2.3/other_production\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.m.mu.Lock()
	delete(f.m.data.NativeDatabases, "myrails-db")
	delete(f.m.data.Databases, "myrails-db")
	f.m.mu.Unlock()
	previousLocal := postgresHostLocal
	postgresHostLocal = func(host string) bool { return host == "10.1.2.3" || host == "127.0.0.1" || host == "localhost" }
	previousHost := nativeHostDataHost
	nativeHostDataHost = func(m *Manager) nativeHostData {
		h := previousHost(m)
		inner := h.run
		h.run = func(ctx context.Context, stdin, name string, args ...string) (string, error) {
			out, err := inner(ctx, stdin, name, args...)
			if strings.Contains(stdin, "pg_database_size") && strings.Contains(stdin, "'shop_db'") {
				return "123456", err
			}
			return out, err
		}
		return h
	}
	t.Cleanup(func() { postgresHostLocal, nativeHostDataHost = previousLocal, previousHost })
	return f
}

func (f *adoptedArchiveFixture) scriptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.scripts)
}

func TestArchiveAdoptedDatabaseOnPrivateAddressIsRecordedDisabledRestoredAndPurged(t *testing.T) {
	f := adoptedPrivateAddressFixture(t)
	m := f.m
	view, err := m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	item, ok := archiveItemNamed(view.Kept, "database", "shop_db")
	if !ok || item.Role != "user_0b88c2cb4895" || item.SizeBytes != 123456 || !strings.Contains(item.Detail, "NOLOGIN") {
		t.Fatalf("database not recorded: %+v", view.Kept)
	}
	if shared, ok := archiveItemNamed(view.Kept, "database", "other_production"); !ok || !strings.Contains(shared.Detail, "kept: shared with other") {
		t.Fatalf("shared database: %+v", view.Kept)
	}
	script := f.script()
	if !strings.Contains(script, `ALTER ROLE "user_0b88c2cb4895" NOLOGIN;`) || !strings.Contains(script, "usename = 'user_0b88c2cb4895'") ||
		strings.Contains(script, `ALTER ROLE "deploy"`) || strings.Contains(script, `ON DATABASE "other_production"`) {
		t.Fatalf("postgres:\n%s", script)
	}

	// Restore enables the login again.
	f.mu.Lock()
	f.scripts = nil
	f.mu.Unlock()
	if _, err := m.RestoreArchive(context.Background(), view.ID, ArchiveRestoreRequest{}); err != nil {
		t.Fatal(err)
	}
	if script := f.script(); !strings.Contains(script, `ALTER ROLE "user_0b88c2cb4895" LOGIN;`) {
		t.Fatalf("login not restored:\n%s", script)
	}

	// Deleted again and purged: the application's database and role are
	// dropped; the database of the shared role loses only the database;
	// the shared database stays.
	view, err = m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.scripts = nil
	f.mu.Unlock()
	purge, err := m.PurgeArchive(context.Background(), view.ID, ArchivePurgeRequest{Confirm: "my-rails"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	script = f.script()
	if !strings.Contains(script, `DROP DATABASE IF EXISTS "shop_db" WITH (FORCE);`) || !strings.Contains(script, `DROP ROLE IF EXISTS "user_0b88c2cb4895";`) ||
		!strings.Contains(script, `DROP DATABASE IF EXISTS "myrails_queue" WITH (FORCE);`) || strings.Contains(script, `DROP ROLE IF EXISTS "deploy"`) ||
		strings.Contains(script, `"other_production"`) || strings.Contains(script, "pg_dump") {
		t.Fatalf("purge:\n%s", script)
	}
	dropped, ok := archiveItemNamed(purge.Deleted, "database", "shop_db")
	if !ok || !strings.Contains(dropped.Detail, "role user_0b88c2cb4895 dropped") || dropped.SizeBytes != 123456 {
		t.Fatalf("purge inventory %+v", purge.Deleted)
	}
	if left, ok := archiveItemNamed(purge.Deleted, "database", "other_production"); !ok || !strings.Contains(left.Detail, "left on the host PostgreSQL") {
		t.Fatalf("purge inventory %+v", purge.Deleted)
	}
}

func TestArchivePurgeKeepsAnAdoptedDatabaseALiveApplicationUsesByThen(t *testing.T) {
	f := adoptedPrivateAddressFixture(t)
	m := f.m
	view, err := m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Other now uses the archived database.
	if err := os.WriteFile(filepath.Join(f.home, "Other", ".asdf-vars"), []byte("DATABASE_URL=postgres://user_0b88c2cb4895:pw@127.0.0.1/shop_db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.scripts = nil
	f.mu.Unlock()
	purge, err := m.PurgeArchive(context.Background(), view.ID, ArchivePurgeRequest{Confirm: "my-rails"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if script := f.script(); strings.Contains(script, `"shop_db"`) || strings.Contains(script, `DROP ROLE IF EXISTS "user_0b88c2cb4895"`) {
		t.Fatalf("a used database was dropped:\n%s", script)
	}
	if item, ok := archiveItemNamed(purge.Deleted, "database", "shop_db"); !ok || !strings.Contains(item.Detail, "other uses database shop_db now") {
		t.Fatalf("purge inventory %+v", purge.Deleted)
	}
}

func TestArchiveSweepRecordsDatabasesLoggerNamesLocksAndFailedUnitsOfAnEarlierDeletionOnce(t *testing.T) {
	f := adoptedPrivateAddressFixture(t)
	m := f.m
	f.caddy.mu.Lock()
	srv := f.caddy.config["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["srv0"].(map[string]any)
	srv["logs"] = map[string]any{"logger_names": map[string]any{"rails.example.com": []any{""}, "other.example.com": []any{""}}}
	f.caddy.mu.Unlock()
	// An earlier agent: no private-address recognition, no web server API.
	postgresHostLocal = func(host string) bool { return host == "127.0.0.1" || host == "localhost" }
	saved := m.cfg.Native.Caddy
	m.cfg.Native.Caddy = nil
	view, err := m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := archiveItemNamed(view.Kept, "database", "shop_db"); ok {
		t.Fatal("fixture: the earlier deletion recorded the database")
	}
	_ = m.updateArchive(view.ID, func(a *AppArchive) {
		a.Native.Leftovers.ResetFailed = false
		a.Native.Adopted.Records = map[string]ArchivedDatabaseRecord{"shop-db": {}}
	})
	lock := m.nativeBackupLockPath("shop-db")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	postgresHostLocal = func(host string) bool { return host == "10.1.2.3" || host == "127.0.0.1" || host == "localhost" }
	m.cfg.Native.Caddy = saved
	f.mu.Lock()
	f.calls, f.scripts = nil, nil
	f.mu.Unlock()

	m.sweepArchiveLeftovers(context.Background())
	archive := m.archiveSnapshot(view.ID)
	item, ok := archiveItemNamed(archive.Kept, "database", "shop_db")
	if !ok || item.Role != "user_0b88c2cb4895" || item.SizeBytes != 123456 || !strings.Contains(f.script(), `ALTER ROLE "user_0b88c2cb4895" NOLOGIN;`) {
		t.Fatalf("sweep did not record the database: %+v\n%s", archive.Kept, f.script())
	}
	if names := f.caddy.at("/config/apps/http/servers/srv0/logs/logger_names").(map[string]any); names["rails.example.com"] != nil || names["other.example.com"] == nil {
		t.Fatalf("logger names %v", names)
	}
	if _, err := os.Lstat(lock); !os.IsNotExist(err) {
		t.Fatal("backup lock kept")
	}
	if !f.ran("systemctl", "--user", "reset-failed", "myrails-server.service") {
		t.Fatalf("failed state not reset: %v", f.calls)
	}
	if _, ok := archiveItemNamed(archive.Deleted, "log name", "srv0 rails.example.com"); !ok {
		t.Fatalf("deleted inventory %+v", archive.Deleted)
	}

	// Idempotent: a second sweep changes nothing.
	before := f.scriptCount()
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	f.caddy.reset()
	m.sweepArchiveLeftovers(context.Background())
	if f.scriptCount() != before || len(f.callsOf("systemctl")) != 0 || f.caddy.called("DELETE") != 0 || f.caddy.called("POST") != 0 {
		t.Fatalf("second sweep changed something: %v", f.calls)
	}
	if after := m.archiveSnapshot(view.ID); len(after.Kept) != len(archive.Kept) || len(after.Deleted) != len(archive.Deleted) {
		t.Fatal("second sweep changed the inventory")
	}

	// Restore puts the logger name back.
	if _, err := m.RestoreArchive(context.Background(), view.ID, ArchiveRestoreRequest{}); err != nil {
		t.Fatal(err)
	}
	if names := f.caddy.at("/config/apps/http/servers/srv0/logs/logger_names").(map[string]any); names["rails.example.com"] == nil {
		t.Fatalf("logger name not restored: %v", names)
	}
}

func TestArchiveLoggerNamesSkipAHostnameARedirectUses(t *testing.T) {
	f := newLeftoverFixture(t)
	m := f.m
	f.caddy.mu.Lock()
	srv := f.caddy.config["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["srv0"].(map[string]any)
	srv["logs"] = map[string]any{"logger_names": map[string]any{"rails.example.com": []any{""}, "www.rails.example.com": "default"}}
	f.caddy.mu.Unlock()
	view, err := m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if names := f.caddy.at("/config/apps/http/servers/srv0/logs/logger_names").(map[string]any); names["rails.example.com"] != nil {
		t.Fatalf("delete left logger names %v", names)
	}
	// A hostname another application's redirect serves stays.
	m.mu.Lock()
	m.data.Redirects = map[string][]RedirectDomain{"other": {{Host: "www.rails.example.com"}}}
	m.mu.Unlock()
	target := leftoverTarget{appID: "my-rails", hosts: []string{"www.rails.example.com"}}
	if err := m.withdrawLoggerNames(context.Background(), view.ID, target); err != nil {
		t.Fatal(err)
	}
	if names := f.caddy.at("/config/apps/http/servers/srv0/logs/logger_names").(map[string]any); names["www.rails.example.com"] != "default" {
		t.Fatalf("a shared hostname was removed: %v", names)
	}
}

func TestArchiveDeleteResetsTheFailedStateOfRemovedUnits(t *testing.T) {
	f := newAdoptedArchiveFixture(t)
	if _, err := f.m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{}); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{"myrails-server.service", "myrails-worker.service"} {
		if !f.ran("systemctl", "--user", "reset-failed", unit) {
			t.Fatalf("%s: failed state not reset: %v", unit, f.calls)
		}
	}
}

func TestArchiveBackupLockIsRemovedOnlyWhenNoBackupHoldsIt(t *testing.T) {
	f := newAdoptedArchiveFixture(t)
	m := f.m
	lock := m.nativeBackupLockPath("myrails-db")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if removed, err := m.removeNativeBackupLock("myrails-db"); err != nil || removed {
		t.Fatalf("a held lock was removed: %v %v", removed, err)
	}
	if _, err := os.Lstat(lock); err != nil {
		t.Fatal("held lock file gone")
	}
	syscall.Flock(int(held.Fd()), syscall.LOCK_UN)
	held.Close()
	view, err := m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(lock); !os.IsNotExist(err) {
		t.Fatal("the deletion kept the backup lock")
	}
	if _, ok := archiveItemNamed(view.Deleted, "backup lock", "myrails-db"); !ok {
		t.Fatalf("deleted inventory %+v", view.Deleted)
	}
}

// flakyArchiveStore fails its first listings with err.
type flakyArchiveStore struct {
	fakeArchiveStore
	mu       sync.Mutex
	failures int
	err      error
	lists    int
}

func (s *flakyArchiveStore) List(ctx context.Context, prefix string, limit int) ([]storage.Object, error) {
	s.mu.Lock()
	s.lists++
	fail := s.failures > 0
	if fail {
		s.failures--
	}
	s.mu.Unlock()
	if fail {
		return nil, s.err
	}
	return s.fakeArchiveStore.List(ctx, prefix, limit)
}

func TestArchiveOffsiteBackupListingRetriesTransientErrorsAndBlamesPermissionsOnlyOnAccessDenied(t *testing.T) {
	previous := archiveStorageRetryDelays
	archiveStorageRetryDelays = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	t.Cleanup(func() { archiveStorageRetryDelays = previous })
	transient := errors.New("storage: GET /backups: 503 SlowDown: please reduce your request rate")
	timeout := errors.New("storage: s3.example.com: context deadline exceeded")
	denied := errors.New("storage: GET /backups: 403 AccessDenied: Access Denied")
	for _, tc := range []struct {
		name     string
		failures int
		err      error
		ok       bool
		lists    int
		wording  string
	}{
		{"transient then success", 2, transient, true, 4, ""}, // 3 attempts, then the empty listing ends the loop
		{"transient throughout", 5, timeout, false, 3, "temporary off-site storage error after 3 attempts"},
		{"access denied", 5, denied, false, 1, "check its ListBucket permission"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdoptedArchiveFixture(t)
			m := f.m
			store := &flakyArchiveStore{fakeArchiveStore: fakeArchiveStore{objects: map[string]int64{retention.Prefix("myrails-db") + "x.dump": 4}}, failures: tc.failures, err: tc.err}
			newArchiveObjectStore = func(storage.Config) (archiveObjectStore, error) { return store, nil }
			m.mu.Lock()
			m.data.BackupStorage = &storage.Config{Provider: "s3", Bucket: "backups", Endpoint: "https://s3.example.com"}
			m.mu.Unlock()
			_, err := m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{ExclusiveBackupPrefixes: []string{"myrails-db"}})
			if tc.ok != (err == nil) || store.lists != tc.lists {
				t.Fatalf("err=%v lists=%d", err, store.lists)
			}
			if tc.ok {
				if store.has(retention.Prefix("myrails-db") + "x.dump") {
					t.Fatal("off-site backup kept")
				}
				return
			}
			if !strings.Contains(err.Error(), tc.wording) || tc.err != denied && strings.Contains(err.Error(), "permission") {
				t.Fatalf("wording: %v", err)
			}
		})
	}
}

func TestOffsiteAccessDeniedClassification(t *testing.T) {
	for raw, want := range map[string]bool{
		"storage: GET /b: 403 AccessDenied: Access Denied": true,
		"storage: GET /b: 403 ":                            true,
		"storage: GET /b: 401 Unauthorized":                true,
		"storage: GET /b: 400 InvalidAccessKeyId: x":       true,
		"storage: GET /b: 500 InternalError: x":            false,
		"storage: GET /b: 503 SlowDown: x":                 false,
		"storage: s3.example.com: i/o timeout":             false,
		"storage: GET /b4031: 500 x":                       false,
	} {
		if got := offsiteAccessDenied(errors.New(raw)); got != want {
			t.Fatalf("%q: %v", raw, got)
		}
	}
}

func TestLocalPostgresTargetAcceptsThisServersAddressesAndUnescapedPasswords(t *testing.T) {
	previous := postgresHostLocal
	postgresHostLocal = func(host string) bool {
		return host == "10.0.0.5" || host == "localhost" || host == "" || strings.HasPrefix(host, "/")
	}
	t.Cleanup(func() { postgresHostLocal = previous })
	for raw, want := range map[string]string{
		"postgres://user_c8a142f7955e:pw@10.0.0.5:5432/appshop":      "5432/appshop user_c8a142f7955e",
		"postgresql://u:p#w%d@10.0.0.5/db":                           "5432/db u",
		"postgres:///db?host=/var/run/postgresql&user=app&port=5433": "5433/db app",
		"postgres://u:pw@10.9.9.9/db":                                "",
		"mysql://u:pw@localhost/db":                                  "",
	} {
		got := ""
		if target, ok := localPostgresTarget(raw); ok {
			got = target.databaseKey() + " " + target.Role
		}
		if got != want {
			t.Fatalf("%s: %q", raw, got)
		}
	}
	if !slices.Contains([]bool{previous("127.0.0.1")}, true) || previous("192.0.2.77") {
		t.Fatal("the default does not recognise loopback, or claims a foreign address")
	}
}
