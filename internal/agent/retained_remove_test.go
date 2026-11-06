package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// retainedDocker is a fake docker CLI over a state directory: an object
// exists while <state>/<kind>/<name> exists, and the file holds its
// ownership label. Volumes have a mount point under <state>/mount/<name>.
type retainedDocker struct {
	state string
	log   string
}

func newRetainedDocker(t *testing.T) *retainedDocker {
	t.Helper()
	bin, state := t.TempDir(), t.TempDir()
	d := &retainedDocker{state: state, log: filepath.Join(state, "calls.log")}
	for _, kind := range []string{"container", "network", "volume", "mount"} {
		if err := os.MkdirAll(filepath.Join(state, kind), 0700); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
state='` + state + `'
printf '%s\n' "$*" >> "$state/calls.log"
name() { printf '%s' "$1" | sed -e 's/^name=^\/*//' -e 's/\$$//'; }
case "$1" in
  ps) n=$(name "$4"); [ -f "$state/container/$n" ] && printf 'id-%s\n' "$n"; exit 0 ;;
  inspect) [ -f "$state/container/$4" ] || exit 1; cat "$state/container/$4"; exit 0 ;;
  stop) [ -f "$state/stopfail" ] && exit 1; exit 0 ;;
  rm) rm -f "$state/container/$2"; exit 0 ;;
  run) printf 'PGDMP final archive'; exit 0 ;;
  network|volume)
    kind=$1
    case "$2" in
      ls) n=$(name "$4"); [ -f "$state/$kind/$n" ] && printf '%s\n' "$n"; exit 0 ;;
      inspect)
        [ -f "$state/$kind/$5" ] || exit 1
        if [ "$4" = '{{.Mountpoint}}' ]; then printf '%s\n' "$state/mount/$5"; else cat "$state/$kind/$5"; fi
        exit 0 ;;
      rm) [ -f "$state/inuse-$3" ] && exit 1; rm -f "$state/$kind/$3"; exit 0 ;;
    esac ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return d
}

func (d *retainedDocker) add(t *testing.T, kind, name, owner string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.state, kind, name), []byte(owner+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if kind == "volume" {
		dir := filepath.Join(d.state, "mount", name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data"), []byte(strings.Repeat("x", 1000)), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func (d *retainedDocker) exists(kind, name string) bool {
	_, err := os.Stat(filepath.Join(d.state, kind, name))
	return err == nil
}

func (d *retainedDocker) calls(t *testing.T) []string {
	t.Helper()
	raw, _ := os.ReadFile(d.log)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// retainedFixture is a removed container application that left a local
// database, a local cache and a file volume.
func retainedFixture(t *testing.T, backupAfterStop bool) (*Manager, *retainedDocker, string) {
	t.Helper()
	m, docker := retainedApp(t, backupAfterStop)
	if err := m.Retire(context.Background(), "portal"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.data.RetainedResources) != 1 {
		t.Fatalf("retained: %+v", m.data.RetainedResources)
	}
	since := m.data.Databases["portal"].Backups[0].Created
	if backupAfterStop {
		since = since.Add(-time.Minute)
	} else {
		since = since.Add(time.Hour)
	}
	var id string
	for key, r := range m.data.RetainedResources {
		id = key
		if !slices.Equal(r.Databases, []string{"portal"}) || !r.WritesStopped.Equal(since) {
			t.Fatalf("removal record: %+v (maintenance since %s)", r, since)
		}
	}
	return m, docker, id
}

// retainedApp is a container application in maintenance with a local
// database (backed up before or after writes stopped), a local cache and a
// file volume, ready to be retired.
func retainedApp(t *testing.T, backupAfterStop bool) (*Manager, *retainedDocker) {
	t.Helper()
	docker := newRetainedDocker(t)
	m := testManager(t, &fakeRuntime{})
	a := testApp("portal")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if err := m.setMaintenance(a.ID, Maintenance{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	since := m.data.Apps[a.ID].Maintenance.Since
	if since.IsZero() {
		m.mu.Unlock()
		t.Fatal("maintenance start not recorded")
	}
	db := Database{AppID: "portal", Mode: "local", Network: "lidza-db-portal", URL: "postgres://app:secret@lidza-db-portal:5432/app", Ready: true, Backup: BackupPolicy{Hours: 24, Keep: 1}}
	created := since.Add(-time.Hour)
	if backupAfterStop {
		created = since.Add(time.Minute)
	}
	dump := []byte("PGDMP retained archive")
	sum := sha256.Sum256(dump)
	db.Backups = []BackupRecord{{ID: "b1", Created: created, Size: int64(len(dump)), SHA256: hex.EncodeToString(sum[:])}}
	m.data.Databases["portal"] = db
	live := m.data.Apps[a.ID]
	live.Bindings = map[string]string{"DATABASE_URL": "portal"}
	m.data.Apps[a.ID] = live
	m.data.Caches[a.ID] = CacheResource{Mode: "local", Network: "lidza-cache-portal", LocalPassword: "cache-secret", Ready: true}
	m.data.StorageVolumes[a.ID] = "lidza-storage-portal"
	m.mu.Unlock()
	if err := os.MkdirAll(m.backupDir("portal"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.backupDir("portal"), "b1.dump"), dump, 0600); err != nil {
		t.Fatal(err)
	}
	for _, o := range []struct{ kind, name, owner string }{
		{"container", "lidza-db-portal", "portal"}, {"network", "lidza-db-portal", "portal"}, {"volume", "lidza-db-portal-data", "portal"},
		{"container", "lidza-cache-portal", "lidza-cache-portal"}, {"network", "lidza-cache-portal", "lidza-cache-portal"}, {"volume", "lidza-cache-portal-data", "lidza-cache-portal"},
		{"volume", "lidza-storage-portal", "portal"},
	} {
		docker.add(t, o.kind, o.name, o.owner)
	}
	return m, docker
}

func retainedRequest(t *testing.T, m *Manager, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	m.retainedRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func waitRetainedRemoval(t *testing.T, m *Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rt := m.retainedState()
		rt.mu.Lock()
		busy := rt.removing[id]
		rt.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("removal did not finish")
}

func retainedJob(m *Manager) HostJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.hostUpdatesStateLocked().Jobs {
		if job.Kind == "retained-removal" {
			return job
		}
	}
	return HostJob{}
}

func TestRetainedResourceListShowsSizesBackupsAndRemovesInOrder(t *testing.T) {
	m, docker, id := retainedFixture(t, true)
	m.measureRetainedSizes(context.Background(), true)
	views := m.retainedViews()
	if len(views) != 1 {
		t.Fatalf("views: %+v", views)
	}
	view := views[0]
	if view.Kind != retainedKindApplication || view.AppID != "portal" || view.SizePending || view.SizeBytes != 3000 || len(view.Components) != 3 || view.FinalBackupRequired {
		t.Fatalf("view: %+v", view)
	}
	if len(view.Databases) != 1 || !view.Databases[0].Covered || view.Databases[0].LastLocal == nil || view.Databases[0].LastLocal.ID != "b1" || view.Databases[0].LastOffsite != nil {
		t.Fatalf("database view: %+v", view.Databases)
	}
	raw, _ := json.Marshal(views)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("the inventory exposes credentials")
	}

	if w := retainedRequest(t, m, "DELETE", "/v1/retained-resources/"+id, `{"confirm":"other"}`); w.Code != 409 {
		t.Fatalf("unconfirmed removal: %d %s", w.Code, w.Body)
	}
	if w := retainedRequest(t, m, "DELETE", "/v1/retained-resources/nope", `{"confirm":"nope"}`); w.Code != 404 {
		t.Fatalf("unknown id: %d %s", w.Code, w.Body)
	}
	if w := retainedRequest(t, m, "DELETE", "/v1/retained-resources/"+id, `{"confirm":"`+id+`"}`); w.Code != 202 {
		t.Fatalf("removal: %d %s", w.Code, w.Body)
	}
	waitRetainedRemoval(t, m, id)

	for _, o := range [][2]string{{"container", "lidza-db-portal"}, {"volume", "lidza-db-portal-data"}, {"container", "lidza-cache-portal"}, {"volume", "lidza-cache-portal-data"}, {"volume", "lidza-storage-portal"}, {"network", "lidza-db-portal"}} {
		if docker.exists(o[0], o[1]) {
			t.Fatalf("%s %s was not removed", o[0], o[1])
		}
	}
	calls := docker.calls(t)
	lastContainer, firstVolume := -1, len(calls)
	for i, call := range calls {
		if strings.HasPrefix(call, "stop ") || strings.HasPrefix(call, "rm ") {
			lastContainer = i
		}
		if strings.HasPrefix(call, "volume rm ") && i < firstVolume {
			firstVolume = i
		}
	}
	if lastContainer < 0 || lastContainer > firstVolume || !slices.Contains(calls, "stop --time 30 lidza-db-portal") {
		t.Fatalf("containers must be stopped and removed before volumes: %v", calls)
	}
	m.mu.Lock()
	d := m.data.Databases["portal"]
	retained := len(m.data.RetainedResources)
	m.mu.Unlock()
	if retained != 0 || d.Removed.IsZero() || d.URL != "" || d.Network != "" || d.Ready || len(d.Backups) != 1 || d.Operation != "" {
		t.Fatalf("after removal: retained=%d database=%+v", retained, d)
	}
	if _, err := os.Stat(filepath.Join(m.backupDir("portal"), "b1.dump")); err != nil {
		t.Fatal("the local backup must be kept")
	}
	if job := retainedJob(m); job.Status != "succeeded" || !strings.Contains(job.Output, "Removed volume lidza-db-portal-data") {
		t.Fatalf("host job: %+v", job)
	}
	if len(m.retainedViews()) != 0 {
		t.Fatal("a removed resource is still listed")
	}
	if err := m.databaseAction("portal", "provision"); err == nil {
		t.Fatal("a removed database must not be provisioned again")
	}
}

func TestRetainedRemovalRefusals(t *testing.T) {
	t.Run("backup older than the stop of writes", func(t *testing.T) {
		m, docker, id := retainedFixture(t, false)
		if !m.retainedViews()[0].FinalBackupRequired {
			t.Fatal("an old backup must not cover the database")
		}
		w := retainedRequest(t, m, "DELETE", "/v1/retained-resources/"+id, `{"confirm":"`+id+`"}`)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "final backup") {
			t.Fatalf("removal: %d %s", w.Code, w.Body)
		}
		if !docker.exists("volume", "lidza-db-portal-data") || len(docker.calls(t)) > 1 {
			t.Fatalf("a refusal must not touch Docker: %v", docker.calls(t))
		}
	})
	t.Run("attached to a live application", func(t *testing.T) {
		m, docker, id := retainedFixture(t, true)
		other := testApp("reporting")
		if err := m.Upsert(other); err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		a := m.data.Apps[other.ID]
		a.Bindings = map[string]string{"REPORTING_DATABASE_URL": "portal"}
		m.data.Apps[other.ID] = a
		m.mu.Unlock()
		if attached := m.retainedViews()[0].AttachedTo; !slices.Equal(attached, []string{"reporting"}) {
			t.Fatalf("attached: %v", attached)
		}
		w := retainedRequest(t, m, "DELETE", "/v1/retained-resources/"+id, `{"confirm":"`+id+`","final_backup":true}`)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "attached to reporting") {
			t.Fatalf("removal: %d %s", w.Code, w.Body)
		}
		if !docker.exists("container", "lidza-db-portal") {
			t.Fatal("an attached database was touched")
		}
	})
	t.Run("an object owned by something else", func(t *testing.T) {
		m, docker, id := retainedFixture(t, true)
		docker.add(t, "volume", "lidza-storage-portal", "<no value>")
		if w := retainedRequest(t, m, "DELETE", "/v1/retained-resources/"+id, `{"confirm":"`+id+`"}`); w.Code != 202 {
			t.Fatalf("removal: %d %s", w.Code, w.Body)
		}
		waitRetainedRemoval(t, m, id)
		if !docker.exists("volume", "lidza-storage-portal") {
			t.Fatal("an unowned volume was removed")
		}
		views := m.retainedViews()
		if len(views) != 1 || !strings.Contains(views[0].Error, "not owned") {
			t.Fatalf("the failure must stay listed: %+v", views)
		}
		if job := retainedJob(m); job.Status != "failed" {
			t.Fatalf("host job: %+v", job)
		}
		m.mu.Lock()
		d := m.data.Databases["portal"]
		m.mu.Unlock()
		if d.Operation != "" || !d.Removed.IsZero() {
			t.Fatalf("a failed removal must release the database: %+v", d)
		}
	})
}

func TestRetainedRemovalTakesAFinalBackupFirst(t *testing.T) {
	m, docker, id := retainedFixture(t, false)
	if w := retainedRequest(t, m, "DELETE", "/v1/retained-resources/"+id, `{"confirm":"`+id+`","final_backup":true}`); w.Code != 202 {
		t.Fatalf("removal: %d %s", w.Code, w.Body)
	}
	waitRetainedRemoval(t, m, id)
	m.mu.Lock()
	d := m.data.Databases["portal"]
	m.mu.Unlock()
	if d.Removed.IsZero() || len(d.Backups) != 2 || d.Backups[1].Kind != finalBackupKind || d.Backups[1].Offsite {
		t.Fatalf("database after removal: %+v", d)
	}
	calls := docker.calls(t)
	dump := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "run ") && strings.Contains(c, "pg_dump") })
	stop := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "stop ") })
	if dump < 0 || stop < dump {
		t.Fatalf("the final backup must run before anything is stopped: %v", calls)
	}
	if job := retainedJob(m); job.Status != "succeeded" || !strings.Contains(job.Output, "Final backup of portal") {
		t.Fatalf("host job: %+v", job)
	}
}

func TestRetainedStandaloneDatabaseNeedsAFinalBackup(t *testing.T) {
	docker := newRetainedDocker(t)
	m := testManager(t, &fakeRuntime{})
	m.mu.Lock()
	m.data.Databases["restored"] = Database{AppID: "restored", Mode: "local", Network: "lidza-db-restored", Ready: true, Backups: []BackupRecord{{ID: "old", Created: time.Now().Add(-time.Hour), Size: 1, SHA256: "x", Offsite: true, ObjectKey: "databases/restored/old.dump"}}}
	m.mu.Unlock()
	views := m.retainedViews()
	if len(views) != 1 || views[0].ID != "database-restored" || views[0].Kind != retainedKindDatabase || !views[0].FinalBackupRequired || views[0].Databases[0].LastOffsite == nil {
		t.Fatalf("views: %+v", views)
	}
	if w := retainedRequest(t, m, "DELETE", "/v1/retained-resources/database-restored", `{"confirm":"database-restored"}`); w.Code != 409 {
		t.Fatalf("removal without a final backup: %d %s", w.Code, w.Body)
	}
	docker.add(t, "container", "lidza-db-restored", "restored")
	if w := retainedRequest(t, m, "DELETE", "/v1/retained-resources/database-restored", `{"confirm":"database-restored","final_backup":true}`); w.Code != 202 {
		t.Fatalf("removal: %d %s", w.Code, w.Body)
	}
	waitRetainedRemoval(t, m, "database-restored")
	if docker.exists("container", "lidza-db-restored") || len(m.retainedViews()) != 0 {
		t.Fatal("the database container was not removed")
	}
}
