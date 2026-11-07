package agent

import (
	"testing"
	"time"
)

// An older record has no time its writes stopped: a verified final backup
// covers it, so the panel never asks for another one.
func TestRetainedFinalBackupCoversRecordsWithoutWritesStopped(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.mu.Lock()
	m.data.Databases["old"] = Database{AppID: "old", Mode: "local", Network: "lidza-db-old", Backups: []BackupRecord{{ID: "f1", Kind: finalBackupKind, SHA256: "x", Offsite: true, ObjectKey: "databases/old/f1.dump", Created: time.Now()}}}
	m.data.Databases["bare"] = Database{AppID: "bare", Mode: "local", Network: "lidza-db-bare", Backups: []BackupRecord{{ID: "b1", SHA256: "x", Offsite: true, ObjectKey: "databases/bare/b1.dump", Created: time.Now()}}}
	m.data.RetainedResources = map[string]RetainedResource{"r": {ID: "r", AppID: "old", Databases: []string{"old"}}, "s": {ID: "s", AppID: "bare", Databases: []string{"bare"}}}
	m.mu.Unlock()
	views := map[string]RetainedResourceView{}
	for _, v := range m.retainedViews() {
		views[v.ID] = v
	}
	if v := views["r"]; v.FinalBackupRequired || !v.Databases[0].Covered || v.Databases[0].Engine != "postgresql" {
		t.Fatalf("final backup not counted: %+v", v)
	}
	if v := views["s"]; !v.FinalBackupRequired {
		t.Fatalf("an ordinary backup of unknown age must not cover: %+v", v)
	}
}
