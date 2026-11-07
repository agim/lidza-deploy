package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza/packs/storage"
)

// fakeArchiveStore is the off-site storage of the archive tests.
type fakeArchiveStore struct {
	mu      sync.Mutex
	objects map[string]int64
	deleted []string
}

func (s *fakeArchiveStore) List(_ context.Context, prefix string, limit int) ([]storage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storage.Object
	for key, size := range s.objects {
		if strings.HasPrefix(key, prefix) && len(out) < limit {
			out = append(out, storage.Object{Key: key, Size: size})
		}
	}
	return out, nil
}

func (s *fakeArchiveStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	s.deleted = append(s.deleted, key)
	return nil
}

func (s *fakeArchiveStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

type archiveFixture struct {
	m      *Manager
	docker *retainedDocker
	store  *fakeArchiveStore
	caddy  string
}

func newArchiveFixture(t *testing.T) *archiveFixture {
	t.Helper()
	f := &archiveFixture{docker: newRetainedDocker(t), store: &fakeArchiveStore{objects: map[string]int64{}}, caddy: t.TempDir()}
	f.m = testManager(t, &fakeRuntime{})
	oldStore, oldCaddy := newArchiveObjectStore, archiveCaddyStorage
	newArchiveObjectStore = func(storage.Config) (archiveObjectStore, error) { return f.store, nil }
	archiveCaddyStorage = func(context.Context, *Manager) string { return f.caddy }
	t.Cleanup(func() { newArchiveObjectStore, archiveCaddyStorage = oldStore, oldCaddy })
	f.m.cacheProvision = func(context.Context, CacheResource) error { return nil }
	return f
}

func (f *archiveFixture) certificate(t *testing.T, host string) string {
	t.Helper()
	dir := filepath.Join(f.caddy, "certificates", "acme-v02.api.letsencrypt.org-directory", host)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, host+".crt"), []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// shop is a container application with a local database (one local and one
// off-site backup), a local cache, a files volume and a redirect domain.
func (f *archiveFixture) shop(t *testing.T) {
	t.Helper()
	m := f.m
	a := testApp("shop")
	a.AdditionalDomains = []string{"www.shop.example.com"}
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps["shop"]
	a.Bindings = map[string]string{"DATABASE_URL": "shop"}
	a.Env["DATABASE_URL"] = "postgres://app:pw@lidza-db-shop:5432/app"
	a.Current = &Release{ID: "r1", Commit: "abc123", Container: "shop-r1"}
	m.data.Apps["shop"] = a
	m.data.Databases["shop"] = Database{AppID: "shop", Mode: "local", Network: "lidza-db-shop", Ready: true, Backup: BackupPolicy{Hours: 24, Keep: 2},
		Backups: []BackupRecord{{ID: "b1", Size: 4, SHA256: "x", Created: time.Now()}, {ID: "b2", Size: 7, SHA256: "y", Offsite: true, ObjectKey: "databases/shop/b2.dump", Created: time.Now()}}}
	m.data.Caches["shop"] = CacheResource{Mode: "local", Network: "lidza-cache-shop", LocalPassword: "pw", URL: "redis://:pw@lidza-cache-shop:6379/0", Ready: true}
	m.data.StorageVolumes["shop"] = "lidza-storage-shop"
	m.data.Redirects = map[string][]RedirectDomain{"shop": {{Host: "old-shop.example.com", KeepPath: true}}}
	m.data.BackupStorage = &storage.Config{Provider: "s3", Bucket: "backups", Endpoint: "https://s3.example.com", Prefix: "fleet"}
	m.mu.Unlock()
	if err := os.MkdirAll(m.backupDir("shop"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.backupDir("shop"), "b1.dump"), []byte("PGDM"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.store.objects["databases/shop/b2.dump"] = 7
	f.store.objects["databases/shop/predeployment.dump"] = 9
	f.store.objects["databases/other/x.dump"] = 3
	for _, o := range databaseObjects(Database{AppID: "shop", Network: "lidza-db-shop"}) {
		f.docker.add(t, o.kind, o.name, o.owner)
	}
	for _, o := range cacheObjects(m.data.Caches["shop"]) {
		f.docker.add(t, o.kind, o.name, o.owner)
	}
	f.docker.add(t, "volume", "lidza-storage-shop", "shop")
}

func (f *archiveFixture) calls(t *testing.T) string {
	t.Helper()
	raw, _ := os.ReadFile(f.docker.log)
	return string(raw)
}

func (f *archiveFixture) exists(kind, name string) bool {
	_, err := os.Stat(filepath.Join(f.docker.state, kind, name))
	return err == nil
}

func itemNamed(items []ArchiveItem, kind, name string) (ArchiveItem, bool) {
	for _, item := range items {
		if item.Kind == kind && item.Name == name {
			return item, true
		}
	}
	return ArchiveItem{}, false
}

func TestDeleteAppArchivesKeepsDataAndDeletesTheRest(t *testing.T) {
	f := newArchiveFixture(t)
	f.shop(t)
	m := f.m
	own := f.certificate(t, "shop.example.com")
	redirect := f.certificate(t, "old-shop.example.com")
	shared := f.certificate(t, "www.shop.example.com")
	// Another application serves www.shop.example.com too (a hostname the
	// certificate cleanup must leave alone).
	m.mu.Lock()
	m.data.Apps["mirror"] = App{ID: "mirror", Repository: "acme/mirror", Branch: "main", Domain: "mirror.example.com", AdditionalDomains: []string{"www.shop.example.com"}}
	m.mu.Unlock()

	view, err := m.DeleteApp(context.Background(), "shop", AppArchiveRequest{DeletedBy: "operator@example.com", Reason: "replaced", ExclusiveBackupPrefixes: []string{"shop"}})
	if err != nil {
		t.Fatal(err)
	}
	if view.Phase != archivePhaseArchived || view.AppID != "shop" || view.Commit != "abc123" || view.DeletedBy != "operator@example.com" || view.Reason != "replaced" {
		t.Fatalf("view = %+v", view)
	}
	if got := view.ExpiresAt.Sub(view.DeletedAt); got != AppArchiveRetention {
		t.Fatalf("expires after %s", got)
	}
	if !slices.Contains(view.Hostnames, "old-shop.example.com") || !slices.Contains(view.Hostnames, "www.shop.example.com") {
		t.Fatalf("hostnames = %v", view.Hostnames)
	}
	for _, kept := range [][2]string{{"settings", "shop"}, {"volume", "lidza-db-shop-data"}, {"volume", "lidza-storage-shop"}} {
		if _, ok := itemNamed(view.Kept, kept[0], kept[1]); !ok {
			t.Fatalf("kept lacks %v: %+v", kept, view.Kept)
		}
	}
	backups, ok := itemNamed(view.Deleted, "backups", "shop")
	if !ok || backups.Count != 3 || backups.SizeBytes != 4+7+9 {
		t.Fatalf("backups item = %+v (%+v)", backups, view.Deleted)
	}
	if _, ok := itemNamed(view.Deleted, "cache", "lidza-cache-shop"); !ok {
		t.Fatalf("cache not reported deleted: %+v", view.Deleted)
	}
	// Kept: the database container (stopped, no restart) and the volumes.
	if !f.exists("container", "lidza-db-shop") || !f.exists("volume", "lidza-db-shop-data") || !f.exists("volume", "lidza-storage-shop") {
		t.Fatal("kept data was removed")
	}
	calls := f.calls(t)
	if !strings.Contains(calls, "update --restart no lidza-db-shop") || !strings.Contains(calls, "stop --time 30 lidza-db-shop") {
		t.Fatalf("the database container was not stopped:\n%s", calls)
	}
	// Deleted: the cache, every backup (local and off-site), certificates.
	if f.exists("container", "lidza-cache-shop") || f.exists("volume", "lidza-cache-shop-data") {
		t.Fatal("cache survived")
	}
	if _, err := os.Stat(m.backupDir("shop")); !os.IsNotExist(err) {
		t.Fatal("local backups survived")
	}
	if f.store.has("databases/shop/b2.dump") || f.store.has("databases/shop/predeployment.dump") || !f.store.has("databases/other/x.dump") {
		t.Fatalf("off-site objects: %v", f.store.objects)
	}
	for _, dir := range []string{own, redirect} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("certificate %s survived", dir)
		}
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatal("a certificate another application uses was deleted")
	}
	if item, _ := itemNamed(view.Deleted, "certificate", "www.shop.example.com"); !strings.Contains(item.Detail, "mirror") {
		t.Fatalf("shared certificate item = %+v", item)
	}

	m.mu.Lock()
	_, live := m.data.Apps["shop"]
	d := m.data.Databases["shop"]
	_, redirects := m.data.Redirects["shop"]
	tasks := len(m.data.Tasks)
	m.mu.Unlock()
	if live || len(d.Backups) != 0 || redirects || tasks != 0 {
		t.Fatalf("state after deletion: live=%v backups=%d redirects=%v", live, len(d.Backups), redirects)
	}
	// The archive's data is not a retained resource the operator removes.
	for _, r := range m.retainedViews() {
		if r.AppID == "shop" {
			t.Fatalf("archived data listed as retained: %+v", r)
		}
	}
	// The hostnames are free at once.
	taker := testApp("taker")
	taker.Domain = "shop.example.com"
	if err := m.Upsert(taker); err != nil {
		t.Fatalf("hostname not released: %v", err)
	}
	if _, err := m.DeleteApp(context.Background(), "shop", AppArchiveRequest{}); err == nil {
		t.Fatal("deleting a deleted application again must report it missing")
	}
}

func TestDeleteAppKeepsNamedOffsiteObjectsOfSharedFolders(t *testing.T) {
	f := newArchiveFixture(t)
	f.shop(t)
	// Not confirmed exclusive: only the object the record names goes.
	if _, err := f.m.DeleteApp(context.Background(), "shop", AppArchiveRequest{}); err != nil {
		t.Fatal(err)
	}
	if f.store.has("databases/shop/b2.dump") || !f.store.has("databases/shop/predeployment.dump") {
		t.Fatalf("objects: %v", f.store.objects)
	}
}

func TestDeleteAppResumesAfterAFailedStep(t *testing.T) {
	f := newArchiveFixture(t)
	f.shop(t)
	failing := true
	newArchiveObjectStore = func(storage.Config) (archiveObjectStore, error) {
		if failing {
			return nil, os.ErrPermission
		}
		return f.store, nil
	}
	if _, err := f.m.DeleteApp(context.Background(), "shop", AppArchiveRequest{}); err == nil {
		t.Fatal("deletion must fail while the backups cannot be deleted")
	}
	views := f.m.archiveViews().Archives
	if len(views) != 1 || views[0].Phase != archivePhaseDeleting || views[0].Error == "" {
		t.Fatalf("tombstone = %+v", views)
	}
	if _, err := f.m.RestoreArchive(context.Background(), views[0].ID, ArchiveRestoreRequest{}); err == nil {
		t.Fatal("an unfinished deletion cannot be restored")
	}
	failing = false
	view, err := f.m.DeleteApp(context.Background(), "shop", AppArchiveRequest{})
	if err != nil || view.ID != views[0].ID || view.Phase != archivePhaseArchived {
		t.Fatalf("resume: %+v %v", view, err)
	}
}

func TestRestoreArchiveSkipsTakenHostnamesAndReattachesData(t *testing.T) {
	f := newArchiveFixture(t)
	f.shop(t)
	m := f.m
	view, err := m.DeleteApp(context.Background(), "shop", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	taker := testApp("taker")
	taker.Domain = "www.shop.example.com"
	if err := m.Upsert(taker); err != nil {
		t.Fatal(err)
	}
	result, err := m.RestoreArchive(context.Background(), view.ID, ArchiveRestoreRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.AppID != "shop" || result.Skipped["www.shop.example.com"] != "taker" || !slices.Equal(result.Hostnames, []string{"shop.example.com"}) || result.Commit != "abc123" {
		t.Fatalf("result = %+v", result)
	}
	m.mu.Lock()
	a, ok := m.data.Apps["shop"]
	volume := m.data.StorageVolumes["shop"]
	cache := m.data.Caches["shop"]
	redirects := m.data.Redirects["shop"]
	_, archived := m.data.AppArchives[view.ID]
	held := 0
	for _, r := range m.data.RetainedResources {
		if r.AppID == "shop" {
			held++
		}
	}
	m.mu.Unlock()
	if !ok || a.Domain != "shop.example.com" || len(a.AdditionalDomains) != 0 || a.Current != nil || a.Bindings["DATABASE_URL"] != "shop" || a.Env["APP_SECRET"] != "runtime-secret" {
		t.Fatalf("restored app = %+v", a)
	}
	if volume != "lidza-storage-shop" || !cache.Ready || cache.Network != "lidza-cache-shop" || len(redirects) != 1 || archived || held != 0 {
		t.Fatalf("volume=%q cache=%+v redirects=%v archived=%v held=%d", volume, cache, redirects, archived, held)
	}
	if calls := f.calls(t); !strings.Contains(calls, "update --restart unless-stopped lidza-db-shop") || !strings.Contains(calls, "start lidza-db-shop") {
		t.Fatalf("the database was not started:\n%s", calls)
	}
}

func TestRestoreArchiveUnderAnotherIDWhenTheIDIsTaken(t *testing.T) {
	f := newArchiveFixture(t)
	f.shop(t)
	m := f.m
	view, err := m.DeleteApp(context.Background(), "shop", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	again := testApp("shop")
	again.Domain = "new-shop.example.com"
	if err := m.Upsert(again); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RestoreArchive(context.Background(), view.ID, ArchiveRestoreRequest{}); err == nil || !strings.Contains(err.Error(), "another id") {
		t.Fatalf("restore over a live id: %v", err)
	}
	result, err := m.RestoreArchive(context.Background(), view.ID, ArchiveRestoreRequest{NewID: "shop-restored"})
	if err != nil || result.AppID != "shop-restored" {
		t.Fatalf("restore: %+v %v", result, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.StorageVolumes["shop-restored"] != "lidza-storage-shop" || m.data.Apps["shop-restored"].Bindings["DATABASE_URL"] != "shop" {
		t.Fatal("data not attached to the new id")
	}
}

func TestPurgeArchiveRefusedDuringRestoreThenDeletesEverything(t *testing.T) {
	f := newArchiveFixture(t)
	f.shop(t)
	m := f.m
	view, err := m.DeleteApp(context.Background(), "shop", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.updateArchive(view.ID, func(a *AppArchive) { a.Phase = archivePhaseRestoring }); err != nil {
		t.Fatal(err)
	}
	if purged := m.purgeExpiredArchives(context.Background(), time.Now().Add(AppArchiveRetention+time.Hour)); len(purged) != 0 {
		t.Fatal("an archive being restored was purged")
	}
	if _, err := m.PurgeArchive(context.Background(), view.ID, ArchivePurgeRequest{Confirm: "shop"}, "manual"); err == nil || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("purge during restore: %v", err)
	}
	_ = m.updateArchive(view.ID, func(a *AppArchive) { a.Phase = archivePhaseArchived })
	if _, err := m.PurgeArchive(context.Background(), view.ID, ArchivePurgeRequest{Confirm: "sho"}, "manual"); err == nil {
		t.Fatal("purge without the application id")
	}
	if purged := m.purgeExpiredArchives(context.Background(), time.Now()); len(purged) != 0 {
		t.Fatal("an archive was purged before its expiry")
	}
	purged := m.purgeExpiredArchives(context.Background(), time.Now().Add(AppArchiveRetention+time.Hour))
	if !slices.Equal(purged, []string{view.ID}) {
		t.Fatalf("purged = %v", purged)
	}
	for _, o := range [][2]string{{"container", "lidza-db-shop"}, {"volume", "lidza-db-shop-data"}, {"volume", "lidza-storage-shop"}} {
		if f.exists(o[0], o[1]) {
			t.Fatalf("%s %s survived the purge", o[0], o[1])
		}
	}
	all := m.archiveViews()
	if len(all.Archives) != 0 || len(all.Purged) != 1 || all.Purged[0].Trigger != "expiry" || all.Purged[0].AppID != "shop" {
		t.Fatalf("after purge: %+v", all)
	}
	m.mu.Lock()
	_, record := m.data.Databases["shop"]
	retained := len(m.data.RetainedResources)
	m.mu.Unlock()
	if record || retained != 0 {
		t.Fatalf("database record=%v retained=%d", record, retained)
	}
}

func TestPurgeArchiveDeletesPrefixedAppStorageOnly(t *testing.T) {
	f := newArchiveFixture(t)
	m := f.m
	for _, tc := range []struct {
		prefix string
		want   bool
	}{{"shop-files", true}, {"", false}} {
		f.store.objects = map[string]int64{"a.png": 1, "b.png": 2}
		a := AppArchive{AppID: "shop", App: &App{Env: map[string]string{"STORAGE_PROVIDER": "s3", "STORAGE_BUCKET": "media", "STORAGE_PREFIX": tc.prefix}}}
		item, err := m.purgeAppStorage(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		if deleted := len(f.store.objects) == 0; deleted != tc.want {
			t.Fatalf("prefix %q: deleted=%v item=%+v", tc.prefix, deleted, item)
		}
	}
}

func TestArchiveNativeAppDisablesLoginsAndRestoresThem(t *testing.T) {
	f := newNativeCreateFixture(t)
	m := f.m
	var mu sync.Mutex
	var scripts []string
	previous := nativeHostDataHost
	nativeHostDataHost = func(*Manager) nativeHostData {
		return nativeHostData{root: t.TempDir(), run: func(_ context.Context, stdin, name string, args ...string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			scripts = append(scripts, name+" "+strings.Join(args, " ")+"\n"+stdin)
			return "", nil
		}}
	}
	t.Cleanup(func() { nativeHostDataHost = previous })
	oldStore, oldCaddy := newArchiveObjectStore, archiveCaddyStorage
	newArchiveObjectStore = func(storage.Config) (archiveObjectStore, error) {
		return &fakeArchiveStore{objects: map[string]int64{}}, nil
	}
	archiveCaddyStorage = func(context.Context, *Manager) string { return "" }
	t.Cleanup(func() { newArchiveObjectStore, archiveCaddyStorage = oldStore, oldCaddy })

	body := `{"id":"shop","repository":"acme/shop","branch":"main","runtime":"go","domain":"shop.example.com","data":{},"memory_mib":512}`
	if w := f.do(t, "POST", "/v1/native/apps", body); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	m.mu.Lock()
	m.data.NativeHostData = map[string]NativeHostDataRecord{"shop": {Postgres: &NativeHostPostgres{Cluster: "16/main", Major: 16, Port: 5432, Name: "app_shop", Resource: "native-shop", Ready: true}}}
	m.data.Databases["native-shop"] = Database{AppID: "native-shop", Mode: "external", Ready: true}
	m.mu.Unlock()
	root := filepath.Join(f.appsRoot, "shop")
	if err := os.WriteFile(filepath.Join(root, "shared", "upload.txt"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}

	view, err := m.DeleteApp(context.Background(), "shop", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(scripts, "\n")
	if !strings.Contains(joined, `ALTER ROLE "app_shop" NOLOGIN;`) || !strings.Contains(joined, "pg_terminate_backend") || strings.Contains(joined, "DROP DATABASE") {
		t.Fatalf("logins not disabled, or data dropped:\n%s", joined)
	}
	if !f.ran("userdel app-shop") {
		t.Fatal("the account was not removed")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("the root stayed where a new application could use it")
	}
	files := filepath.Join(f.appsRoot, ".archived", view.ID, "root")
	if data, err := os.ReadFile(filepath.Join(files, "shared", "upload.txt")); err != nil || string(data) != "kept" {
		t.Fatalf("files not kept: %v", err)
	}
	if info, err := os.Stat(filepath.Dir(files)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("archive directory mode: %v %v", info, err)
	}
	m.mu.Lock()
	_, registered := m.data.NativeApps["shop"]
	ready := m.data.NativeHostData["shop"].Postgres.Ready
	m.mu.Unlock()
	if registered || ready {
		t.Fatalf("registered=%v ready=%v", registered, ready)
	}
	if _, ok := itemNamed(view.Kept, "database", "native-shop"); !ok {
		t.Fatalf("kept = %+v", view.Kept)
	}
	// The id cannot be reused while the archive can be restored.
	if w := f.do(t, "POST", "/v1/native/apps", body); w.Code == 201 {
		t.Fatal("an archived native id was reused")
	}

	scripts = nil
	result, err := m.RestoreArchive(context.Background(), view.ID, ArchiveRestoreRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Native || result.AppID != "shop" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(strings.Join(scripts, "\n"), `ALTER ROLE "app_shop" LOGIN;`) {
		t.Fatalf("login not enabled:\n%s", strings.Join(scripts, "\n"))
	}
	if data, err := os.ReadFile(filepath.Join(root, "shared", "upload.txt")); err != nil || string(data) != "kept" {
		t.Fatalf("files not restored: %v", err)
	}
	m.mu.Lock()
	_, registered = m.data.NativeApps["shop"]
	_, created := m.data.NativeCreated["shop"]
	ready = m.data.NativeHostData["shop"].Postgres.Ready
	m.mu.Unlock()
	if !registered || !created || !ready {
		t.Fatalf("registered=%v created=%v ready=%v", registered, created, ready)
	}
	if _, err := os.Stat(filepath.Join(f.units, "lidza-app-shop-web.service")); err != nil {
		t.Fatal("units not recreated")
	}
}

func TestRetainedMoveLeftoverIsRemovedWithoutCoveringBackup(t *testing.T) {
	f := newArchiveFixture(t)
	m := f.m
	m.mu.Lock()
	m.data.Databases["olddb"] = Database{AppID: "olddb", Mode: "local", Network: "lidza-db-olddb", Ready: true}
	m.data.RetainedResources = map[string]RetainedResource{
		"moved":   {ID: "moved", AppID: "shop", Databases: []string{"olddb"}, Origin: &RetainedOrigin{MoveID: "m1", Switched: true}, WritesStopped: time.Now()},
		"removed": {ID: "removed", AppID: "blog", StorageVolume: "lidza-storage-blog"},
	}
	m.mu.Unlock()
	for _, o := range databaseObjects(m.data.Databases["olddb"]) {
		f.docker.add(t, o.kind, o.name, o.owner)
	}
	if err := m.removeRetained("moved", RetainedRemoveRequest{Confirm: "moved"}); err == nil {
		t.Fatal("a switched move's data needs a covering backup without move_leftover")
	}
	if err := m.removeRetained("removed", RetainedRemoveRequest{Confirm: "removed", MoveLeftover: true}); err == nil {
		t.Fatal("data no move left cannot skip the backup check")
	}
	if err := m.removeRetained("moved", RetainedRemoveRequest{Confirm: "moved", MoveLeftover: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.exists("volume", "lidza-db-olddb-data") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.exists("volume", "lidza-db-olddb-data") {
		t.Fatal("the move's leftover was not removed")
	}
}
