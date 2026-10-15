package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func backupFixture(t *testing.T, m *Manager, id string, records []BackupRecord) {
	t.Helper()
	if err := os.MkdirAll(m.backupDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	for _, b := range records {
		if err := os.WriteFile(filepath.Join(m.backupDir(id), b.ID+".dump"), []byte("dump"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m.data.Databases[id] = Database{AppID: id, Backup: BackupPolicy{Keep: 7}, Backups: records}
}
func waitMaintenance(t *testing.T, m *Manager) MaintenanceState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s := m.maintenanceStatus()
		if !s.Running {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("maintenance did not finish")
	return MaintenanceState{}
}
func TestLocalCleanupKeepsNewestUsableFileAndLeavesUnmanagedData(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	now := time.Now().UTC()
	backupFixture(t, m, "retained", []BackupRecord{{ID: "newest", Created: now}, {ID: "old", Created: now.Add(-time.Hour)}})
	d := m.data.Databases["retained"]
	d.Backups = append(d.Backups, BackupRecord{ID: "missing", Created: now.Add(time.Hour)})
	m.data.Databases["retained"] = d
	unknown := filepath.Join(m.backupDir("retained"), "unmanaged.txt")
	os.WriteFile(unknown, []byte("preserve"), 0600)
	if err := m.pruneBackupsLocked("retained"); err != nil {
		t.Fatal(err)
	}
	if got := m.data.Databases["retained"].Backups; len(got) != 1 || got[0].ID != "newest" {
		t.Fatalf("lost usable newest backup: %+v", got)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatal("unmanaged file removed")
	}
	if _, err := os.Stat(filepath.Join(m.backupDir("retained"), "old.dump")); !os.IsNotExist(err) {
		t.Fatal("old dump not removed")
	}
}
func TestLocalCleanupPreservesFilesWhileDatabaseBusy(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	backupFixture(t, m, "busy", []BackupRecord{{ID: "old"}, {ID: "new"}})
	d := m.data.Databases["busy"]
	d.Operation = "restore"
	m.data.Databases["busy"] = d
	if err := m.pruneBackupsLocked("busy"); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(m.backupDir("busy"), "*.dump"))
	if len(files) != 2 {
		t.Fatal("cleanup interfered with restore")
	}
	if err := m.startMaintenance(CleanupRequest{Backups: true}, time.Now()); err == nil {
		t.Fatal("started during restore")
	}
}
func TestLocalCleanupRefusesTraversalAndNoUsableReplacement(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.data.Databases["missing"] = Database{Backups: []BackupRecord{{ID: "missing"}}}
	if err := m.pruneBackupsLocked("missing"); err != nil || len(m.data.Databases["missing"].Backups) != 1 {
		t.Fatal("discarded inventory with no usable backup", err)
	}
	m.data.Databases["unsafe"] = Database{Backups: []BackupRecord{{ID: "../outside"}}}
	if m.pruneBackupsLocked("unsafe") == nil {
		t.Fatal("accepted traversal")
	}
}
func TestMaintenanceUsesOnlyFixedBoundedCommandsAndPersistsSchedule(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	now := time.Now().UTC()
	backupFixture(t, m, "retained", []BackupRecord{{ID: "old"}, {ID: "new"}})
	calls := [][]string{}
	m.maintenanceRun = func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, append([]string{name}, args...))
		return "reclaimed space", nil
	}
	m.maintenanceTick(now)
	result := waitMaintenance(t, m)
	want := [][]string{{"docker", "builder", "prune", "--all", "--force", "--filter", "until=168h"}, {"journalctl", "--rotate"}, {"journalctl", "--vacuum-time=7d"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unsafe commands: %v", calls)
	}
	if result.Error != "" || !result.NextBackupCleanup.After(now) || result.NextBackupCleanup.After(now.Add(time.Hour)) || !result.NextRun.After(now) || result.NextRun.After(now.Add(24*time.Hour)) {
		t.Fatalf("schedule/result: %+v", result)
	}
	m.maintenanceTick(now.Add(result.NextBackupCleanup.Sub(now) / 2))
	if m.maintenanceStatus().Running {
		t.Fatal("cleanup ran before due")
	}
	m.Close()
	reopened, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.maintenanceStatus().NextBackupCleanup.Equal(result.NextBackupCleanup) {
		t.Fatal("lost hourly timer on restart")
	}
	if len(reopened.data.Databases["retained"].Backups) != 1 {
		t.Fatal("local retention was not persisted")
	}
}
func TestHourlyBackupsContinueWhenCacheLogTimerDisabled(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	backupFixture(t, m, "retained", []BackupRecord{{ID: "old"}, {ID: "new"}})
	p := defaultMaintenancePolicy()
	p.Enabled = false
	if err := m.setMaintenancePolicy(p); err != nil {
		t.Fatal(err)
	}
	m.maintenanceRun = func(context.Context, string, ...string) (string, error) {
		t.Error("disabled cache/log cleanup ran")
		return "", nil
	}
	m.maintenanceTick(time.Now().UTC())
	result := waitMaintenance(t, m)
	if result.Error != "" || len(m.data.Databases["retained"].Backups) != 1 {
		t.Fatal("hourly retention disabled", result)
	}
}
func TestCleanupFailureReportedAndPolicyValidation(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.maintenanceRun = func(context.Context, string, ...string) (string, error) {
		return "fixture failure", errors.New("not permitted")
	}
	now := time.Now().UTC()
	if err := m.startMaintenance(CleanupRequest{Logs: true}, now); err != nil {
		t.Fatal(err)
	}
	result := waitMaintenance(t, m)
	if result.Error == "" || result.NextRun.After(now) {
		t.Fatal("failure hidden or postponed", result)
	}
	for _, p := range []MaintenancePolicy{{IntervalHours: 0, OlderThanDays: 7}, {IntervalHours: 24, OlderThanDays: 0}, {IntervalHours: 24, OlderThanDays: 366}} {
		if m.setMaintenancePolicy(p) == nil {
			t.Fatal("invalid cleanup policy accepted", p)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/maintenance/run", nil)
	w := httptest.NewRecorder()
	Handler(m).ServeHTTP(w, request)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated cleanup allowed", w.Code)
	}
}

func TestAgentStartupRunsMaintenanceOnce(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	var calls atomic.Int32
	m.maintenanceRun = func(context.Context, string, ...string) (string, error) { calls.Add(1); return "clean", nil }
	m.StartMaintenance()
	m.StartMaintenance()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status := m.maintenanceStatus()
		if !status.LastRun.IsZero() && !status.Running {
			if calls.Load() != 3 {
				t.Fatalf("startup job duplicated or omitted: %d commands", calls.Load())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agent startup did not run cleanup")
}
