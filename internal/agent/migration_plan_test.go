package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMigrationPlanDoesNotExposeSecretsOrMutateState(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("source")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	a.Current = &Release{Commit: "abc123"}
	a.Bindings = map[string]string{"DATABASE_URL": "shared"}
	m.data.Apps[a.ID] = a
	m.data.Apps["other"] = App{Bindings: map[string]string{"DATABASE_URL": "shared"}}
	m.data.Databases["shared"] = Database{Mode: "local", Ready: true, URL: "postgres://secret-password", AdminPassword: "admin-secret", Backups: []BackupRecord{{Created: time.Now(), Size: 12, SHA256: "digest", ObjectKey: "private-object"}}}
	m.data.Tasks["source:worker"] = Task{ID: "worker", AppID: "source", Mode: "worker", Command: []string{"secret-command"}}
	before, _ := json.Marshal(m.data)
	m.mu.Unlock()
	p, err := m.MigrationPlan(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(p)
	for _, secret := range []string{"runtime-secret", "secret-password", "admin-secret", "private-object", "secret-command"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	if len(p.Databases) != 1 || len(p.Databases[0].SharedWith) != 1 || p.Databases[0].LatestBackup == nil || len(p.Blockers) != 1 || p.ExecutionSupported {
		t.Fatalf("unexpected plan: %s", encoded)
	}
	m.mu.Lock()
	after, _ := json.Marshal(m.data)
	m.mu.Unlock()
	if string(before) != string(after) {
		t.Fatal("planning changed state")
	}
}

func TestMigrationPlanIdentifiesUnmanagedDependencies(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("source")
	a.Env["DATABASE_URL"] = "postgres://user:password@remote/db"
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	p, err := m.MigrationPlan(a.ID)
	if err != nil || len(p.Blockers) != 2 {
		t.Fatalf("%+v: %v", p, err)
	}
	if _, err = m.MigrationPlan("missing"); err == nil {
		t.Fatal("missing app accepted")
	}
}

func TestMigrationPlanIncludesLegacyLiveDatabaseDependencies(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("legacy")
	a.Current = &Release{Commit: "current", DatabaseIDs: []string{"old", "old", "missing", ""}}
	a.Bindings = map[string]string{"EVENT_DATABASE_URL": "next"}
	a.Env["DATABASE_URL"] = "postgres://user:secret@old/db"
	m.mu.Lock()
	m.data.Apps[a.ID] = a
	m.data.Apps["legacy-peer"] = App{Current: &Release{DatabaseIDs: []string{"old"}}}
	m.data.Databases["old"] = Database{Mode: "local", Ready: true}
	m.data.Databases["next"] = Database{Mode: "local", Ready: true}
	before, _ := json.Marshal(m.data)
	m.mu.Unlock()
	p, err := m.MigrationPlan(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(p)
	if len(p.Databases) != 3 {
		t.Fatalf("lost release or next-deploy dependency: %s", encoded)
	}
	old := p.Databases[2]
	if old.ID != "old" || len(old.EnvironmentKeys) != 0 || len(old.SharedWith) != 1 || old.SharedWith[0] != "legacy-peer" {
		t.Fatalf("lost legacy sharing or invented binding: %s", encoded)
	}
	if p.Databases[1].ID != "next" || len(p.Databases[1].EnvironmentKeys) != 1 || p.Databases[1].EnvironmentKeys[0] != "EVENT_DATABASE_URL" {
		t.Fatalf("lost explicit binding: %s", encoded)
	}
	if !strings.Contains(strings.Join(p.Blockers, " "), "Database missing is unavailable") || !strings.Contains(strings.Join(p.Blockers, " "), "manually configured database connection in DATABASE_URL") {
		t.Fatalf("missing unresolved/manual dependency review: %s", encoded)
	}
	if strings.Contains(string(encoded), "postgres://") || strings.Contains(string(encoded), "user:secret") {
		t.Fatal("connection leaked")
	}
	m.mu.Lock()
	after, _ := json.Marshal(m.data)
	m.mu.Unlock()
	if string(before) != string(after) {
		t.Fatal("planning changed legacy bindings")
	}
}
