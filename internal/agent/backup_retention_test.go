package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRollingBackupIndependentRetention(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	id := "rolling"
	if err := os.MkdirAll(m.backupDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	records := []BackupRecord{{ID: "manual-old"}, {ID: "pre-old", Kind: predeploymentBackup}, {ID: "manual-new"}, {ID: "pre-new", Kind: predeploymentBackup}}
	for _, b := range records {
		if err := os.WriteFile(filepath.Join(m.backupDir(id), b.ID+".dump"), []byte("backup"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m.data.Databases[id] = Database{AppID: id, Backup: BackupPolicy{Keep: 1}, Backups: records}
	if err := m.pruneBackupsLocked(id); err != nil {
		t.Fatal(err)
	}
	got := m.data.Databases[id].Backups
	if len(got) != 2 || got[0].ID != "manual-new" || got[1].ID != "pre-new" {
		t.Fatalf("retention: %+v", got)
	}
	files, _ := filepath.Glob(filepath.Join(m.backupDir(id), "*.dump"))
	if len(files) != 2 {
		t.Fatalf("files: %v", files)
	}
	for _, b := range []BackupRecord{records[1], records[3]} {
		if key := backupObjectKey(id, b); key != "databases/rolling/predeployment.dump" {
			t.Fatal(key)
		}
	}
	if key := backupObjectKey(id, records[2]); key != "databases/rolling/manual-new.dump" {
		t.Fatal(key)
	}
}

func TestRollingBackupPruneFailureReported(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	id := "rolling"
	oldPath := filepath.Join(m.backupDir(id), "old.dump")
	if err := os.MkdirAll(oldPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldPath, "block"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	m.data.Databases[id] = Database{AppID: id, Backup: BackupPolicy{Keep: 1}, Backups: []BackupRecord{{ID: "old", Kind: predeploymentBackup}, {ID: "new", Kind: predeploymentBackup}}}
	if err := m.pruneBackupsLocked(id); err == nil {
		t.Fatal("failed pruning was not reported")
	}
	if len(m.data.Databases[id].Backups) != 2 || m.data.Databases[id].Error == "" {
		t.Fatal("failed file disappeared from inventory")
	}
}
