package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/agim/lidza/pkg/config"
)

func TestLocalStorageRequiresManagedPersistence(t *testing.T) {
	cfg := &config.Config{Packs: []string{"lidza/storage"}}
	values := map[string]string{"STORAGE_PROVIDER": "local", "STORAGE_DIR": appStorageMount}
	if err := productionRequirements(cfg, values); err == nil {
		t.Fatal("unattached local storage accepted")
	}
	if err := productionRequirements(cfg, values, true); err != nil {
		t.Fatal("lasting disk rejected", err)
	}
	values["STORAGE_DIR"] = "/tmp/uploads"
	if err := productionRequirements(cfg, values, true); err == nil {
		t.Fatal("ephemeral storage accepted")
	}
	values["STORAGE_PROVIDER"] = "s3"
	if err := productionRequirements(cfg, values); err != nil {
		t.Fatal("external provider rejected", err)
	}
	m := testManager(t, &fakeRuntime{})
	a := testApp("external-storage")
	a.Env["STORAGE_PROVIDER"] = "s3"
	a.Env["STORAGE_DIR"] = "custom"
	prepared, err := m.preflightRuntime(context.Background(), a, cacheManifest(t, `["lidza/storage"]`))
	if err != nil || prepared.Env["STORAGE_PROVIDER"] != "s3" || prepared.Env["STORAGE_DIR"] != "custom" || len(m.data.StorageVolumes) != 0 {
		t.Fatal("external storage changed", err)
	}
}

func TestStorageVolumeOwnershipAndAutomaticDefaults(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1")
	}
	ctx := context.Background()
	id := "storage-" + newID()[:10]
	volume := "lidza-storage-" + id
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", volume).Run() })
	if _, err := command(ctx, "", nil, "docker", "volume", "create", volume); err != nil {
		t.Fatal(err)
	}
	if err := prepareStorageVolume(ctx, id, volume); err == nil {
		t.Fatal("unowned volume adopted")
	}
	if _, err := command(ctx, "", nil, "docker", "volume", "rm", volume); err != nil {
		t.Fatal(err)
	}
	m := testManager(t, &fakeRuntime{})
	a := testApp(id)
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps[id]
	m.mu.Unlock()
	prepared, err := m.preflightRuntime(ctx, a, cacheManifest(t, `["lidza/storage"]`))
	if err != nil || prepared.Env["STORAGE_PROVIDER"] != "local" || prepared.Env["STORAGE_DIR"] != appStorageMount || prepared.StorageVolume != volume {
		t.Fatal("automatic storage defaults not attached", err)
	}
	if err = m.PatchSettings(id, SettingsPatch{Branch: a.Branch, Domain: a.Domain, EnvChanges: map[string]*string{"STORAGE_DIR": nil}}); err == nil {
		t.Fatal("managed mount removed through env editor")
	}
	if err = m.Upsert(testApp(id)); err != nil {
		t.Fatal(err)
	}
	settings, _ := m.Settings(id)
	if !settings.PersistentStorage {
		t.Fatal("attachment missing from settings")
	}
	restarted, err := NewManager(ctx, m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.data.StorageVolumes[id] != volume || restarted.data.Apps[id].Env["STORAGE_DIR"] != appStorageMount {
		t.Fatal("restart lost storage attachment")
	}
	if args := strings.Join(storageMountArgs(prepared), " "); !strings.Contains(args, "volume-nocopy") || !strings.Contains(args, volume) {
		t.Fatal("missing volume mount")
	}
}
