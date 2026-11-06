package agent

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Removing the destination of a move that never switched deletes the data
// with the application: it is a partial copy of the source's, so no backup
// is taken or required, and nothing stays retained.
func TestRetireDeletesNeverSwitchedMoveCopyWithoutBackup(t *testing.T) {
	m, docker := retainedApp(t, false) // the only backup predates the stop
	origin := &RetainedOrigin{MoveID: "mv-1", SourceServer: "alpha"}
	if err := m.RetireWith(context.Background(), "portal", AppRemoveRequest{Origin: origin, DeleteData: true}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	retained, removed := len(m.data.RetainedResources), m.data.Databases["portal"].Removed
	m.mu.Unlock()
	if retained != 0 || removed.IsZero() {
		t.Fatalf("retained %d, database removed %v", retained, removed)
	}
	for _, o := range [][2]string{{"container", "lidza-db-portal"}, {"volume", "lidza-db-portal-data"}, {"volume", "lidza-cache-portal-data"}, {"volume", "lidza-storage-portal"}} {
		if docker.exists(o[0], o[1]) {
			t.Fatalf("%s %s was not removed", o[0], o[1])
		}
	}
	job := retainedJob(m)
	if job.Status != "succeeded" || job.Trigger != "move" || !strings.Contains(job.Output, "copy made by move mv-1") {
		t.Fatalf("job: %+v", job)
	}
}

// A removal that names its move records the origin on the retained
// resource: after a switch the data is the application's own, so the
// backup rule applies and the data cannot be deleted with the application.
func TestRetireRecordsMoveOriginAndKeepsSwitchedData(t *testing.T) {
	m, docker := retainedApp(t, false)
	origin := &RetainedOrigin{MoveID: "mv-2", SourceServer: "alpha", Switched: true}
	err := m.RetireWith(context.Background(), "portal", AppRemoveRequest{Origin: origin, DeleteData: true})
	if err == nil || !strings.Contains(err.Error(), "never switched") {
		t.Fatalf("deleting switched data = %v", err)
	}
	m.mu.Lock()
	_, live := m.data.Apps["portal"]
	m.mu.Unlock()
	if !live {
		t.Fatal("a refused request must not remove the application")
	}
	if err := m.RetireWith(context.Background(), "portal", AppRemoveRequest{Origin: origin}); err != nil {
		t.Fatal(err)
	}
	views := m.retainedViews()
	if len(views) != 1 || views[0].Origin == nil || *views[0].Origin != *origin || views[0].Copy || !views[0].FinalBackupRequired {
		t.Fatalf("views: %+v", views)
	}
	if err := m.removeRetained(views[0].ID, RetainedRemoveRequest{Confirm: views[0].ID}); err == nil || !strings.Contains(err.Error(), "no verified backup") {
		t.Fatalf("uncovered removal = %v", err)
	}
	if !docker.exists("volume", "lidza-db-portal-data") {
		t.Fatal("the data must stay")
	}
}

// A retained copy (never-switched origin) is listed as such and removed
// through the retained-resources route without a backup requirement.
func TestRetainedCopyIsRemovableWithoutBackup(t *testing.T) {
	m, docker := retainedApp(t, false)
	if err := m.RetireWith(context.Background(), "portal", AppRemoveRequest{Origin: &RetainedOrigin{MoveID: "mv-3", SourceServer: "alpha"}}); err != nil {
		t.Fatal(err)
	}
	views := m.retainedViews()
	if len(views) != 1 || !views[0].Copy || views[0].FinalBackupRequired || views[0].Databases[0].Covered {
		t.Fatalf("views: %+v", views)
	}
	id := views[0].ID
	if w := retainedRequest(t, m, "DELETE", "/v1/retained-resources/"+id, `{"confirm":"`+id+`"}`); w.Code != 202 {
		t.Fatalf("removal: %d %s", w.Code, w.Body)
	}
	waitRetainedRemoval(t, m, id)
	if len(m.retainedViews()) != 0 || docker.exists("volume", "lidza-db-portal-data") {
		t.Fatal("the copy was not removed")
	}
}

// The removal routes accept the optional body and refuse deleting data
// that is not a never-switched move's copy before touching the application.
func TestAppRemoveRequestIsValidatedByTheRoutes(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.cfg.APIKey = strings.Repeat("a", 32)
	if err := m.Upsert(testApp("portal")); err != nil {
		t.Fatal(err)
	}
	call := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("DELETE", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
		w := httptest.NewRecorder()
		Handler(m).ServeHTTP(w, r)
		return w
	}
	if w := call("/v1/apps/portal", `{"delete_data":true}`); w.Code != 400 || !strings.Contains(w.Body.String(), "never switched") {
		t.Fatalf("unknown origin: %d %s", w.Code, w.Body)
	}
	if w := call("/v1/apps/portal", `{"delete_data":true,"origin":{"move_id":"mv","switched":true}}`); w.Code != 400 {
		t.Fatalf("switched origin: %d %s", w.Code, w.Body)
	}
	m.mu.Lock()
	_, live := m.data.Apps["portal"]
	m.mu.Unlock()
	if !live {
		t.Fatal("refused requests must not remove the application")
	}
	if w := call("/v1/apps/portal", `{"delete_data":true,"origin":{"move_id":"mv","source_server":"alpha"}}`); w.Code != 200 {
		t.Fatalf("copy removal: %d %s", w.Code, w.Body)
	}
	if w := call("/v1/native/apps/portal", `{"delete_data":true}`); w.Code != 400 {
		t.Fatalf("native unknown origin: %d %s", w.Code, w.Body)
	}
}

// Native data services retained for a never-switched move's destination
// are a copy too: deleting them with the application drops the database
// and role without a backup.
func TestNativeMoveCopyIsDeletedWithTheApplication(t *testing.T) {
	m, f := newHostDataFixture(t)
	m.cfg.Native = &NativeHostConfig{Root: "/srv/legacy", User: "deploy", AppsRoot: "/srv/apps"}
	account := NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	grant, err := provisionNativeData(ctx, m, "shop", account, NativeDataRequest{Postgres: true})
	if err != nil {
		t.Fatal(err)
	}
	resource := grant.Databases[0]
	origin := &RetainedOrigin{MoveID: "mv-4", SourceServer: "alpha"}
	key, err := retainNativeData(ctx, m, "shop", origin)
	if err != nil || key == "" {
		t.Fatal(err)
	}
	views := m.retainedViews()
	if len(views) != 1 || !views[0].Copy || views[0].FinalBackupRequired || views[0].Origin == nil || views[0].Origin.MoveID != "mv-4" {
		t.Fatalf("views: %+v", views)
	}
	if err := m.removeRetainedCopy(ctx, key); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	_, recorded := m.data.NativeHostData["shop"]
	d := m.data.Databases[resource]
	left := len(m.data.RetainedResources)
	m.mu.Unlock()
	if recorded || d.Removed.IsZero() || left != 0 || len(d.Backups) != 0 {
		t.Fatalf("after removal: recorded %v removed %v retained %d backups %d", recorded, d.Removed, left, len(d.Backups))
	}
	if !hostDataScriptStartsWith(f.scripts, `SELECT 'ALTER ROLE "app_shop" NOLOGIN'`) {
		t.Fatal("deleting the copy must drop the database and role")
	}
}
