package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Removing a native application keeps its data services like a container
// application's volumes: the database stays bound and backed up, Redis is
// stopped with its data kept, and both are listed as one retained resource.
// Removing that resource, after a covering backup, drops them.
func TestNativeDataIsRetainedAndRemovedAsRetainedResource(t *testing.T) {
	m, f := newHostDataFixture(t)
	m.cfg.Native = &NativeHostConfig{Root: "/srv/legacy", User: "deploy", AppsRoot: "/srv/apps"}
	account := NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	grant, err := provisionNativeData(ctx, m, "shop", account, NativeDataRequest{Postgres: true})
	if err != nil {
		t.Fatal(err)
	}
	resource := grant.Databases[0]
	if err := m.updateHostDataRecord("shop", func(r *NativeHostDataRecord) {
		r.Redis = &NativeHostRedis{Unit: nativeRedisUnit("shop"), Socket: nativeRedisSocket(account.Root), User: account.User, Ready: true}
	}); err != nil {
		t.Fatal(err)
	}
	redisData := filepath.Join(f.root, nativeRedisDir(account.Root))
	if err := os.MkdirAll(redisData, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(redisData, "dump.rdb"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	key, err := retainNativeData(ctx, m, "shop", nil)
	if err != nil || key == "" {
		t.Fatal(err)
	}
	if again, err := retainNativeData(ctx, m, "shop", nil); err != nil || again != key {
		t.Fatal("retaining twice must be a no-op", err)
	}
	if hostDataScriptStartsWith(f.scripts, `SELECT 'ALTER ROLE "app_shop" NOLOGIN'`) {
		t.Fatal("removing the application dropped its database")
	}
	if !slices.Contains(f.commands, "/usr/bin/systemctl disable --now lidza-redis@shop.service") {
		t.Fatalf("Redis runs as the removed account and must be stopped: %v", f.commands)
	}
	if _, err := os.Stat(filepath.Join(redisData, "dump.rdb")); err != nil {
		t.Fatal("Redis data must be kept")
	}
	m.mu.Lock()
	inUse := m.nativeDatabaseInUseLocked(resource)
	records := len(m.data.RetainedResources)
	m.mu.Unlock()
	if !inUse || records != 1 {
		t.Fatalf("the retained database keeps its backups: in use %v, records %d", inUse, records)
	}
	views := m.retainedViews()
	if len(views) != 1 || views[0].AppID != "shop" || len(views[0].Databases) != 1 || views[0].Databases[0].ID != resource || !views[0].FinalBackupRequired || views[0].SizePending {
		t.Fatalf("view = %+v", views)
	}
	kinds := []string{}
	for _, c := range views[0].Components {
		if !c.Host {
			t.Fatalf("native data is not a Docker volume: %+v", c)
		}
		kinds = append(kinds, c.Kind+"/"+c.Name)
	}
	if !slices.Equal(kinds, []string{"database/app_shop", "redis/lidza-redis@shop.service"}) {
		t.Fatalf("components = %v", kinds)
	}

	// Without a covering backup the removal is refused before anything
	// changes.
	id := views[0].ID
	if err := m.removeRetained(id, RetainedRemoveRequest{Confirm: id}); err == nil || !strings.Contains(err.Error(), "no verified backup") {
		t.Fatalf("uncovered removal = %v", err)
	}
	m.mu.Lock()
	d := m.data.Databases[resource]
	d.Backups = append(d.Backups, BackupRecord{ID: "final", Created: time.Now().UTC(), SHA256: strings.Repeat("ab", 32), Offsite: true, ObjectKey: "backups/final.dump", Size: 10})
	m.data.Databases[resource] = d
	m.mu.Unlock()
	if err := m.removeRetained(id, RetainedRemoveRequest{Confirm: id}); err != nil {
		t.Fatal(err)
	}
	m.wg.Wait()
	m.mu.Lock()
	_, recorded := m.data.NativeHostData["shop"]
	_, bound := m.data.NativeDatabases[resource]
	d = m.data.Databases[resource]
	left := len(m.data.RetainedResources)
	m.mu.Unlock()
	if recorded || bound || d.Removed.IsZero() || left != 0 || len(d.Backups) != 1 {
		t.Fatalf("after removal: recorded %v bound %v removed %v retained %d backups %d (%+v)", recorded, bound, d.Removed, left, len(d.Backups), m.retainedViews())
	}
	if !hostDataScriptStartsWith(f.scripts, `SELECT 'ALTER ROLE "app_shop" NOLOGIN'`) {
		t.Fatal("removing the retained resource must drop the database and role")
	}
	if _, err := os.Stat(redisData); !os.IsNotExist(err) {
		t.Fatal("removing the retained resource must delete the Redis data")
	}
}

// An application id whose data services are retained cannot be created
// again until the retained resource is removed: the names derive from it.
func TestNativeCreateRefusesIDWithRetainedData(t *testing.T) {
	f := newNativeCreateFixture(t)
	f.m.mu.Lock()
	f.m.data.NativeHostData = map[string]NativeHostDataRecord{"shop": {Postgres: &NativeHostPostgres{Name: "app_shop", Resource: "shop-db1"}}}
	f.m.mu.Unlock()
	w := f.do(t, "POST", "/v1/native/apps", `{"id":"shop","repository":"acme/shop","runtime":"go","domain":"shop.example.com","data":{}}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "retained resources") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
