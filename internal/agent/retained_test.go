package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemovedApplicationIDStartsFreshAndRetainsData(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("portal")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	oldSecret := m.data.Apps[a.ID].Env["AUTH_SECRET"]
	oldCache := CacheResource{Mode: "local", URL: "redis://:private-cache@lidza-cache-portal:6379", LocalPassword: "private-cache", Network: "lidza-cache-portal", Ready: true}
	m.data.StorageVolumes[a.ID] = "lidza-storage-portal"
	m.data.Caches[a.ID] = oldCache
	m.data.Databases[a.ID] = Database{AppID: a.ID, URL: "postgres://private-db", Backups: []BackupRecord{{ID: "kept"}}, Ready: true}
	if err := m.Retire(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if m.data.StorageVolumes[a.ID] != "" || m.data.Caches[a.ID].Mode != "" || len(m.data.RetainedResources) != 1 {
		t.Fatal("resources remain in active app namespace")
	}
	a.ResourceID = a.ID // A caller cannot select the old resource identity.
	if err := m.Upsert(a); err != nil {
		t.Fatal("removed ID unavailable", err)
	}
	fresh := m.data.Apps[a.ID]
	if fresh.resourceID() == a.ID || fresh.Env["AUTH_SECRET"] == oldSecret || fresh.Env["CACHE_URL"] != "" || fresh.Env["DATABASE_URL"] != "" || len(fresh.Bindings) != 0 {
		t.Fatal("recreated application inherited old resources or credentials")
	}
	docker := &Docker{Root: t.TempDir()}
	if docker.ownerClaimDirectory(a) == docker.ownerClaimDirectory(fresh) {
		t.Fatal("recreated app reused its old private setup directory")
	}
	if m.data.Databases[a.ID].URL != "postgres://private-db" || len(m.data.Databases[a.ID].Backups) != 1 || !m.databaseViews()[0].Retained {
		t.Fatal("database or backups changed")
	}
	view, _ := json.Marshal(m.retainedViews())
	if !strings.Contains(string(view), "lidza-storage-portal") || !strings.Contains(string(view), "lidza-cache-portal-data") || strings.Contains(string(view), "private-cache") || strings.Contains(string(view), "private-db") {
		t.Fatal("retained inventory missing resources or exposes credentials")
	}
	if err := m.Upsert(testApp(a.ID)); err != nil || m.data.Apps[a.ID].ResourceID != fresh.ResourceID {
		t.Fatal("update rotated resource identity", err)
	}
	other, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.data.Apps[a.ID].ResourceID != fresh.ResourceID || len(other.retainedViews()) != 1 || other.data.Apps[a.ID].Env["AUTH_SECRET"] != fresh.Env["AUTH_SECRET"] {
		t.Fatal("restart lost resource separation")
	}
}

func TestLegacyRemovalMigratesAndCreationRollbackKeepsResources(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	id := "legacy"
	m.data.StorageVolumes[id] = "lidza-storage-legacy"
	m.data.Caches[id] = CacheResource{Mode: "external", URL: "rediss://:private-cache@cache.example"}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m.Close() // Stop background readers before injecting a persistence failure.
	originalDir := m.cfg.DataDir
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	m.cfg.DataDir = blocked
	if err := m.Upsert(testApp(id)); err == nil {
		t.Fatal("persistence failure hidden")
	}
	if _, exists := m.data.Apps[id]; exists || m.data.StorageVolumes[id] == "" || m.data.Caches[id].URL == "" || len(m.data.RetainedResources) != 0 {
		t.Fatal("failed creation lost legacy retained data")
	}
	m.cfg.DataDir = originalDir
	other, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if len(other.retainedViews()) != 1 || other.data.StorageVolumes[id] == "" || other.data.Caches[id].Mode == "" {
		t.Fatal("legacy inventory lost or startup changed rollback-compatible resource maps")
	}
	if err := other.Upsert(testApp(id)); err != nil || other.data.Apps[id].resourceID() == id {
		t.Fatal("legacy ID cannot be safely reused", err)
	}
	if other.data.StorageVolumes[id] != "" || other.data.Caches[id].Mode != "" || len(other.data.RetainedResources) != 1 {
		t.Fatal("recreation did not archive older resources")
	}
}

func TestRecreatedAppWithoutManagedDataGetsFreshSetupIdentity(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("claim-only")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if err := m.Retire(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	fresh := m.data.Apps[a.ID]
	if fresh.resourceID() == a.ID {
		t.Fatal("private setup identity reused without managed data")
	}
	identity := fresh.ResourceID
	if err := m.Retire(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[a.ID].ResourceID == identity {
		t.Fatal("subsequent recreation reused setup identity")
	}
}

func TestDockerRemovedIDUsesFreshStorageCacheAndDatabase(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1")
	}
	ctx := context.Background()
	m := testManager(t, &fakeRuntime{})
	id := "recreate-" + newID()[:10]
	a := testApp(id)
	var volumes, networks, containers []string
	t.Cleanup(func() {
		for _, name := range containers {
			_ = exec.Command("docker", "rm", "-f", name).Run()
		}
		for _, name := range networks {
			_ = exec.Command("docker", "network", "rm", name).Run()
		}
		for _, name := range volumes {
			_ = exec.Command("docker", "volume", "rm", name).Run()
		}
	})
	manifest := cacheManifest(t, `["lidza/cache","lidza/storage"]`)
	for incarnation := 0; incarnation < 2; incarnation++ {
		if err := m.Upsert(a); err != nil {
			t.Fatal(err)
		}
		current := m.data.Apps[id]
		resourceID := current.resourceID()
		volumes = append(volumes, "lidza-storage-"+resourceID, "lidza-cache-"+resourceID+"-data", "lidza-db-"+resourceID+"-data")
		networks = append(networks, "lidza-cache-"+resourceID, "lidza-db-"+resourceID)
		containers = append(containers, "lidza-cache-"+resourceID, "lidza-db-"+resourceID)
		prepared, err := m.preflightRuntime(ctx, current, manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.configureDatabase(id, DatabaseRequest{Mode: "local", Backup: BackupPolicy{Keep: 7}}); err != nil {
			t.Fatal(err)
		}
		if db := waitDatabase(t, m, resourceID); !db.Ready || db.Error != "" {
			t.Fatal("database not ready", db.Error)
		}
		cache := m.data.Caches[id]
		cacheEnv := []string{"REDISCLI_AUTH=" + cache.LocalPassword}
		if incarnation == 0 {
			if _, err = command(ctx, "", nil, "docker", "run", "--rm", "--network", "none", "--mount", "type=volume,src="+prepared.StorageVolume+",dst=/data", cacheImage, "sh", "-c", "echo retained > /data/old-marker"); err != nil {
				t.Fatal(err)
			}
			if _, err = command(ctx, "", cacheEnv, "docker", "exec", "--env", "REDISCLI_AUTH", cache.Network, "valkey-cli", "SET", "old-marker", "retained"); err != nil {
				t.Fatal(err)
			}
			if _, err = command(ctx, "", nil, "docker", "exec", "lidza-db-"+resourceID, "psql", "-U", "postgres", "-d", "app", "-c", "CREATE TABLE old_marker(id int); ALTER TABLE old_marker OWNER TO app"); err != nil {
				t.Fatal(err)
			}
			if err = m.databaseAction(resourceID, "backup"); err != nil {
				t.Fatal(err)
			}
			if db := waitDatabase(t, m, resourceID); len(db.Backups) != 1 {
				t.Fatal("backup missing", db.Error)
			}
			if err = m.Retire(ctx, id); err != nil {
				t.Fatal(err)
			}
		} else {
			if resourceID == id {
				t.Fatal("physical resource identity reused")
			}
			if _, err = command(ctx, "", nil, "docker", "run", "--rm", "--network", "none", "--mount", "type=volume,src="+prepared.StorageVolume+",dst=/data", cacheImage, "sh", "-c", "test ! -e /data/old-marker"); err != nil {
				t.Fatal("old uploaded file attached to new app", err)
			}
			if value, err := command(ctx, "", cacheEnv, "docker", "exec", "--env", "REDISCLI_AUTH", cache.Network, "valkey-cli", "EXISTS", "old-marker"); err != nil || value != "0" {
				t.Fatal("old cache attached", err)
			}
			if value, err := command(ctx, "", nil, "docker", "exec", "lidza-db-"+resourceID, "psql", "-U", "postgres", "-d", "app", "-tAc", "SELECT count(*) FROM pg_tables WHERE tablename='old_marker'"); err != nil || value != "0" {
				t.Fatal("old database attached", err)
			}
		}
	}
	if _, err := command(ctx, "", nil, "docker", "run", "--rm", "--network", "none", "--mount", "type=volume,src=lidza-storage-"+id+",dst=/data", cacheImage, "sh", "-c", "test -f /data/old-marker"); err != nil {
		t.Fatal("retained file lost", err)
	}
	if db := m.data.Databases[id]; len(db.Backups) != 1 {
		t.Fatal("old backups lost")
	} else if _, err := os.Stat(filepath.Join(m.backupDir(id), db.Backups[0].ID+".dump")); err != nil {
		t.Fatal("old backup file lost", err)
	}
}
