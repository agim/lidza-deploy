package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/retention"
	"github.com/agim/lidza-deploy/internal/retention/fakes3"
	"github.com/agim/lidza/packs/storage"
)

func retentionFixture(t *testing.T) (*Manager, *fakes3.Server, *http.ServeMux) {
	t.Helper()
	bucket := fakes3.New("private")
	t.Cleanup(bucket.Close)
	m := testManager(t, &fakeRuntime{})
	m.data.BackupStorage = &storage.Config{Provider: "s3", Endpoint: bucket.URL, Bucket: "private", Region: "nyc3", Prefix: "saved", AccessKey: "access", SecretKey: "secret"}
	mux := http.NewServeMux()
	m.databaseRoutes(mux)
	return m, bucket, mux
}

func daysAgo(days int) time.Time {
	return time.Now().UTC().Add(-time.Duration(days)*24*time.Hour - time.Hour)
}

// seedHistory stores one backup a day for days days, plus a pre-deployment
// copy and another database's object.
func seedHistory(bucket *fakes3.Server, id string, days int) {
	for d := 1; d <= days; d++ {
		bucket.Put("saved/databases/"+id+"/day"+string(rune('a'+d))+".dump", 1<<20, daysAgo(d))
	}
	bucket.Put("saved/databases/"+id+"/predeployment.dump", 7, daysAgo(90))
	bucket.Put("saved/databases/other/keep.dump", 3, daysAgo(90))
	bucket.Put("saved/connection-check/x", 1, daysAgo(90))
}

func serve(t *testing.T, mux *http.ServeMux, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&payload).Encode(body)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, &payload))
	return rec
}

func TestOffsiteRetentionAfterUpload(t *testing.T) {
	m, bucket, _ := retentionFixture(t)
	bucket.PageSize = 2 // exercise ListObjectsV2 continuation
	seedHistory(bucket, "app", 10)
	m.data.BackupRetention = &retention.Settings{Policy: retention.Policy{Daily: 3}, OrphanGraceDays: 30, Automatic: true}
	old := BackupRecord{ID: "dayj", Offsite: true, ObjectKey: "databases/app/dayj.dump", Created: daysAgo(9)}
	m.data.Databases["app"] = Database{AppID: "app", Backup: BackupPolicy{Keep: 1, Offsite: true}, Backups: []BackupRecord{old, {ID: "fixture", Size: 4}}}
	if err := os.MkdirAll(m.backupDir("app"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.backupDir("app"), "fixture.dump"), []byte("dump"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.retryBackupUpload("app", "fixture"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(bucket.Deleted()) < 8 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Kept: the fresh upload (today), the two newest earlier days, the
	// pre-deployment copy, and everything outside the database's folder.
	want := []string{"saved/connection-check/x", "saved/databases/app/dayb.dump", "saved/databases/app/dayc.dump", "saved/databases/app/fixture.dump", "saved/databases/app/predeployment.dump", "saved/databases/other/keep.dump"}
	if got := bucket.Keys(); !slices.Equal(got, want) {
		t.Fatalf("kept %v", got)
	}
	m.mu.Lock()
	d := m.data.Databases["app"]
	m.mu.Unlock()
	if d.Backups[0].Offsite || d.Backups[0].ObjectKey != "" || !d.Backups[1].Offsite {
		t.Fatalf("inventory not updated: %+v", d.Backups)
	}
}

func TestOffsiteRetentionWaitsForWorkspaceSettings(t *testing.T) {
	for _, settings := range []*retention.Settings{nil, {Policy: retention.Policy{}, OrphanGraceDays: 30, Automatic: false}} {
		m, bucket, _ := retentionFixture(t)
		seedHistory(bucket, "app", 5)
		m.data.Databases["app"] = Database{AppID: "app"}
		m.data.BackupRetention = settings
		m.mu.Lock()
		m.offsiteRetentionAfterUploadLocked("app")
		m.mu.Unlock()
		m.Close()
		if len(bucket.Deleted()) != 0 {
			t.Fatalf("%+v: deleted %v", settings, bucket.Deleted())
		}
	}
}

func TestOffsiteRetentionPlanAndApplyAsReviewed(t *testing.T) {
	m, bucket, mux := retentionFixture(t)
	seedHistory(bucket, "app", 6)
	seedHistory(bucket, "busy", 6)
	m.data.Databases["app"] = Database{AppID: "app", Backup: BackupPolicy{Keep: 1, Retention: &retention.Policy{Daily: 2}}}
	m.data.Databases["busy"] = Database{AppID: "busy", Operation: "restore"}
	m.data.Databases["empty"] = Database{AppID: "empty"}
	rec := serve(t, mux, "GET", "/v1/backups/retention/plan", nil)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var plan retention.Plan
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Databases) != 2 || plan.Databases[0].Database != "app" || !plan.Databases[0].Override || len(plan.Databases[0].Delete) != 4 || plan.DeleteObjects != 4 || plan.DeleteBytes != 4<<20 {
		t.Fatalf("plan %+v", plan)
	}
	if busy := plan.Databases[1]; busy.Status != retention.StatusProtected || len(busy.Delete) != 0 || len(busy.Keep) != 7 {
		t.Fatalf("busy %+v", busy)
	}
	// A backup arriving after the review changes what would be deleted.
	bucket.Put("saved/databases/app/fresh.dump", 9, time.Now())
	if rec := serve(t, mux, "POST", "/v1/backups/retention/apply", map[string]string{"fingerprint": plan.Fingerprint}); rec.Code != 409 || !strings.Contains(rec.Body.String(), "review it again") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if len(bucket.Deleted()) != 0 {
		t.Fatal("refused plan deleted objects")
	}
	rec = serve(t, mux, "GET", "/v1/backups/retention/plan", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &plan)
	bucket.FailDelete("saved/databases/app/dayg.dump")
	rec = serve(t, mux, "POST", "/v1/backups/retention/apply", map[string]string{"fingerprint": plan.Fingerprint})
	var result OffsiteRetentionResult
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if result.Deleted != plan.DeleteObjects-1 || len(result.Failed) != 1 || result.Failed[0].Key != "databases/app/dayg.dump" {
		t.Fatalf("result %+v plan %d", result, plan.DeleteObjects)
	}
	for _, key := range bucket.Deleted() {
		if !strings.HasPrefix(key, "saved/databases/app/day") {
			t.Fatalf("deleted %s", key)
		}
	}
}

func TestOffsiteRetentionSettingsRoutes(t *testing.T) {
	m, _, mux := retentionFixture(t)
	m.data.Databases["app"] = Database{AppID: "app", Backup: BackupPolicy{Keep: 1, Hours: 24}}
	if rec := serve(t, mux, "PUT", "/v1/backup-retention", map[string]any{"daily": 400, "weekly": 4, "monthly": 0, "orphan_grace_days": 30, "automatic": true}); rec.Code != 400 {
		t.Fatal(rec.Code)
	}
	if rec := serve(t, mux, "PUT", "/v1/backup-retention", map[string]any{"daily": 14, "weekly": 8, "monthly": 6, "orphan_grace_days": 45, "automatic": true}); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if s := m.data.BackupRetention; s == nil || s.Daily != 14 || s.Monthly != 6 || s.OrphanGraceDays != 45 || !s.Automatic {
		t.Fatalf("%+v", s)
	}
	if p, override := m.offsitePolicyLocked(m.data.Databases["app"]); override || p != (retention.Policy{Daily: 14, Weekly: 8, Monthly: 6}) {
		t.Fatal(p)
	}
	if rec := serve(t, mux, "PATCH", "/v1/databases/app/backups/retention", map[string]int{"daily": 3, "weekly": 0, "monthly": 12}); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	// A schedule change keeps the override.
	if err := m.setBackupPolicy("app", BackupPolicy{Hours: 168, Keep: 1}); err != nil {
		t.Fatal(err)
	}
	if p, override := m.offsitePolicyLocked(m.data.Databases["app"]); !override || p != (retention.Policy{Daily: 3, Monthly: 12}) {
		t.Fatal(p)
	}
	if rec := serve(t, mux, "PATCH", "/v1/databases/app/backups/retention", nil); rec.Code != 400 {
		t.Fatal("empty body accepted", rec.Code)
	}
	req := httptest.NewRequest("PATCH", "/v1/databases/app/backups/retention", strings.NewReader("null"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || m.data.Databases["app"].Backup.Retention != nil {
		t.Fatal("override not cleared", rec.Code)
	}
	if rec := serve(t, mux, "PATCH", "/v1/databases/missing/backups/retention", map[string]int{"daily": 1}); rec.Code != 409 {
		t.Fatal(rec.Code)
	}
	if err := (BackupPolicy{Keep: 1, Retention: &retention.Policy{Weekly: -1}}).Validate(); err == nil {
		t.Fatal("invalid override accepted")
	}
}

func TestOffsiteRetentionRequiresStorage(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if _, _, err := m.offsiteRetentionPlan(context.Background(), nil); err == nil {
		t.Fatal("plan without storage")
	}
}
