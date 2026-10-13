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
