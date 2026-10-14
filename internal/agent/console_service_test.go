package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQueueConsoleServiceMigrationRequiresReleasedHelper(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("old updater\n"), 0700); err != nil {
		t.Fatal(err)
	}
	queued, err := queueConsoleServiceMigration(dir, helper)
	if err != nil || queued {
		t.Fatal("migration queued before helper support", queued, err)
	}
	if err := os.WriteFile(helper, []byte("# lidza-console-root-v1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	queued, err = queueConsoleServiceMigration(dir, helper)
	if err != nil || !queued {
		t.Fatal("migration was not queued", queued, err)
	}
	request := filepath.Join(dir, "upgrade", "request")
	if data, err := os.ReadFile(request); err != nil || string(data) != consoleRootAction+"\n" {
		t.Fatal("wrong fixed migration request", string(data), err)
	}
	queued, err = queueConsoleServiceMigration(dir, helper)
	if err != nil || queued {
		t.Fatal("duplicate migration request queued", queued, err)
	}
}

func TestQueueControlBootMigrationIsOneShot(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	unit := filepath.Join(dir, "lidza-control.service")
	override := filepath.Join(dir, "20-boot.conf")
	if err := os.WriteFile(unit, []byte("[Service]\nRestart=on-failure\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, []byte("old updater\n"), 0700); err != nil {
		t.Fatal(err)
	}
	done, err := queueControlBootMigration(dir, helper, unit, override)
	if err != nil || done {
		t.Fatal("migration completed before helper support", done, err)
	}
	if err := os.WriteFile(helper, []byte("# "+controlBootMarker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	done, err = queueControlBootMigration(dir, helper, unit, override)
	if err != nil || !done {
		t.Fatal("migration was not queued", done, err)
	}
	request := filepath.Join(dir, "upgrade", "request")
	if data, err := os.ReadFile(request); err != nil || string(data) != controlBootAction+"\n" {
		t.Fatal("wrong fixed boot migration request", string(data), err)
	}
	if err := os.Remove(request); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte("# "+controlBootMarker+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	done, err = queueControlBootMigration(dir, helper, unit, override)
	if err != nil || !done {
		t.Fatal("completed migration was queued again", done, err)
	}
	if _, err := os.Stat(request); !os.IsNotExist(err) {
		t.Fatal("completed migration recreated request", err)
	}
}
