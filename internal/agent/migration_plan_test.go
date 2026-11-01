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
