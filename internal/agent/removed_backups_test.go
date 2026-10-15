package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRemovedApplicationStopsBackupsAcrossRestartAndIDReuse(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("removed")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	a.Bindings = map[string]string{"DATABASE_URL": a.ID}
	m.data.Apps[a.ID] = a
	m.data.Databases[a.ID] = Database{AppID: a.ID, Ready: true, Backup: BackupPolicy{Hours: 24, Keep: 1}, NextBackup: time.Now().Add(-time.Hour), Backups: []BackupRecord{{ID: "existing"}}}
	m.mu.Unlock()
	if view := m.databaseViews()[0]; view.Retained || view.Backup.Hours != 24 {
		t.Fatal("active database lost its schedule", view)
	}
	if err := m.Retire(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	assertStopped := func(m *Manager) {
		t.Helper()
		view := m.databaseViews()[0]
		if !view.Retained || view.Backup.Hours != 0 || !view.NextBackup.IsZero() || len(view.Backups) != 1 {
			t.Fatal("removed app still scheduled or existing backups lost", view)
		}
		mux := http.NewServeMux()
		m.databaseRoutes(mux)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/databases/removed/backup", nil))
		if w.Code != http.StatusConflict {
			t.Fatal("stale scheduler request accepted", w.Code, w.Body.String())
		}
	}
	assertStopped(m)
	m.Close()
	reopened, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertStopped(reopened)
	if err := reopened.Upsert(testApp(a.ID)); err != nil {
		t.Fatal(err)
	}
	assertStopped(reopened)
}

func TestBackupEligibilityDuringRemovalAndSharedUse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		apps   map[string]App
		active bool
	}{
		{"removal pending cleanup", map[string]App{"owner": {ID: "owner", Retiring: true, Bindings: map[string]string{"DATABASE_URL": "db"}, Current: &Release{DatabaseIDs: []string{"db"}}}}, false},
		{"shared by another active app", map[string]App{"owner": {ID: "owner", Retiring: true}, "consumer": {ID: "consumer", Bindings: map[string]string{"DATABASE_URL": "db"}}}, true},
		{"running release still uses database", map[string]App{"consumer": {ID: "consumer", Current: &Release{DatabaseIDs: []string{"db"}}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(t, &fakeRuntime{})
			m.mu.Lock()
			m.data.Apps = tc.apps
			m.data.Databases["db"] = Database{AppID: "db", Ready: true, Backup: BackupPolicy{Hours: 24, Keep: 1}, NextBackup: time.Now()}
			m.mu.Unlock()
			view := m.databaseViews()[0]
			if view.Retained == tc.active || (view.Backup.Hours > 0) != tc.active || view.NextBackup.IsZero() == tc.active {
				t.Fatal("incorrect backup eligibility", view)
			}
			if !tc.active {
				if err := m.databaseAction("db", "backup"); err == nil {
					t.Fatal("retiring app backup accepted")
				}
			}
		})
	}
}
