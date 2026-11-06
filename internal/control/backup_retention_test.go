package control

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/retention"
	"github.com/agim/lidza-deploy/internal/retention/fakes3"
	"github.com/agim/lidza/packs/storage"
)

type retentionAgent struct {
	*httptest.Server
	mu    sync.Mutex
	views []agent.DatabaseView
	calls []string
}

func newRetentionAgent(t *testing.T, views ...agent.DatabaseView) *retentionAgent {
	a := &retentionAgent{views: views}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.calls = append(a.calls, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
		a.mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/databases":
			agent.JSON(w, 200, a.views)
		case "PUT /v1/backup-storage", "PUT /v1/backup-retention", "PATCH /v1/databases/app/backups/retention":
			agent.JSON(w, 200, map[string]string{"status": "saved"})
		case "GET /v1/backups/retention/plan":
			agent.JSON(w, 200, map[string]any{"fingerprint": "agent-plan", "databases": []any{}})
		case "POST /v1/backups/retention/apply":
			if !strings.Contains(string(body), "agent-plan") {
				agent.Fail(w, 409, retention.ErrPlanChanged)
				return
			}
			agent.JSON(w, 200, map[string]int{"deleted": 2})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *retentionAgent) received(prefix string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, c := range a.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func retentionControl(t *testing.T, bucket *fakes3.Server, agents ...*retentionAgent) *Control {
	t.Helper()
	var servers []Server
	for i, a := range agents {
		id := []string{"one", "two", "three"}[i]
		servers = append(servers, Server{ID: id, Name: "Server " + id, URL: a.URL, Token: strings.Repeat("a", 32)})
	}
	c, err := New(Config{User: "operator@example.com", Password: "fixture-password-16-chars", PublicURL: "http://localhost:3000", DataDir: t.TempDir(), Key: []byte(strings.Repeat("r", 32)), Servers: servers})
	if err != nil {
		t.Fatal(err)
	}
	c.data.BackupStorage = &storage.Config{Provider: "s3", Endpoint: bucket.URL, Bucket: "private", Region: "nyc3", Prefix: "saved", AccessKey: "access", SecretKey: "secret", Timeout: 5 * time.Second}
	return c
}

func ago(days int) time.Time {
	return time.Now().UTC().Add(-time.Duration(days)*24*time.Hour - time.Hour)
}

func history(bucket *fakes3.Server, prefix, id string, days int) {
	for d := 1; d <= days; d++ {
		bucket.Put(prefix+"databases/"+id+"/b"+string(rune('a'+d))+".dump", 1000, ago(d))
	}
}

func getPlan(t *testing.T, c *Control) RetentionPlan {
	t.Helper()
	w := httptest.NewRecorder()
	c.workspaceRetentionPlan(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var plan RetentionPlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func planFor(t *testing.T, plan RetentionPlan, database string) retention.DatabasePlan {
	t.Helper()
	for _, d := range plan.Databases {
		if d.Database == database {
			return d
		}
	}
	t.Fatalf("%s not planned: %+v", database, plan.Databases)
	return retention.DatabasePlan{}
}

func applyPlan(c *Control, fingerprint string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]string{"fingerprint": fingerprint})
	c.applyWorkspaceRetention(w, httptest.NewRequest("POST", "/", strings.NewReader(string(body))))
	return w
}

func TestWorkspaceRetentionPlanCoversEveryFolder(t *testing.T) {
	bucket := fakes3.New("private")
	defer bucket.Close()
	history(bucket, "saved/", "app", 12)
	history(bucket, "saved/", "shared", 12)
	history(bucket, "saved/", "busy", 12)
	history(bucket, "saved/", "moving", 12)
	history(bucket, "saved/", "native-archive-old", 3)
	history(bucket, "legacy/", "app", 3)
	bucket.Put("saved/databases/app/predeployment.dump", 50, ago(100))
	bucket.Put("saved/connection-check/abc", 1, ago(100))
	bucket.Put("saved/databases/app/readme.txt", 1, ago(100))
	one := newRetentionAgent(t,
		agent.DatabaseView{AppID: "app", Backup: agent.BackupPolicy{Retention: &retention.Policy{Daily: 2}}},
		agent.DatabaseView{AppID: "shared", Backup: agent.BackupPolicy{Retention: &retention.Policy{Daily: 1, Weekly: 1}}},
		agent.DatabaseView{AppID: "busy", Operation: "backup"},
		agent.DatabaseView{AppID: "moving"})
	two := newRetentionAgent(t, agent.DatabaseView{AppID: "shared", Backup: agent.BackupPolicy{Retention: &retention.Policy{Daily: 3}}})
	c := retentionControl(t, bucket, one, two)
	c.data.BackupStorageLegacyPrefix = "legacy"
	c.data.Moves = map[string]MoveJob{"m1": {ID: "m1", State: "running", Databases: []MoveDatabase{{SourceID: "moving", TargetID: "moving-new"}}}, "m0": {ID: "m0", State: moveStateFinished, Databases: []MoveDatabase{{SourceID: "app"}}}}

	plan := getPlan(t, c)
	if plan.Reviewed || plan.Settings.Policy != retention.Default() || plan.Ignored != 1 || plan.LegacyPrefix != "legacy" || len(plan.Unavailable) != 0 {
		t.Fatalf("plan header %+v", plan)
	}
	app := planFor(t, plan, "app")
	// Two dailies of twelve current-location backups plus three legacy ones
	// of the same days: the newest of each day across both is kept.
	if !app.Override || len(app.Keep) != 3 || len(app.Delete) != 13 {
		t.Fatalf("app %+v", app)
	}
	shared := planFor(t, plan, "shared")
	if shared.Policy != (retention.Policy{Daily: 3, Weekly: 1}) || len(shared.Keep) != 3 || !strings.Contains(shared.Note, "2 servers") {
		t.Fatalf("shared %+v", shared)
	}
	for _, id := range []string{"busy", "moving"} {
		if d := planFor(t, plan, id); d.Status != retention.StatusProtected || len(d.Delete) != 0 || len(d.Keep) != 12 {
			t.Fatalf("%s %+v", id, d)
		}
	}
	orphan := planFor(t, plan, "native-archive-old")
	if orphan.Status != retention.StatusOrphaned || len(orphan.Delete) != 0 || len(orphan.Keep) != 3 || orphan.DeleteAfter == nil {
		t.Fatalf("orphan %+v", orphan)
	}
	noticed := c.data.BackupOrphans["native-archive-old"]
	if noticed.IsZero() || len(c.data.BackupOrphans) != 1 {
		t.Fatal("orphan not recorded", c.data.BackupOrphans)
	}
	// A later plan keeps the first-noticed time.
	if again := getPlan(t, c); !planFor(t, again, "native-archive-old").Removed.Equal(noticed) || again.Fingerprint != plan.Fingerprint {
		t.Fatal("orphan time or plan changed")
	}

	// After the grace period the orphaned folder is deleted entirely.
	c.data.BackupOrphans["native-archive-old"] = time.Now().AddDate(0, 0, -31)
	plan = getPlan(t, c)
	if orphan := planFor(t, plan, "native-archive-old"); len(orphan.Delete) != 3 || len(orphan.Keep) != 0 {
		t.Fatalf("orphan after grace %+v", orphan)
	}
	before := bucket.Keys()
	if w := applyPlan(c, "stale"); w.Code != 409 || len(bucket.Deleted()) != 0 {
		t.Fatal("stale fingerprint", w.Code, w.Body.String())
	}
	w := applyPlan(c, plan.Fingerprint)
	var result RetentionResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Deleted != plan.DeleteObjects || result.DeletedBytes != plan.DeleteBytes || len(result.Failed) != 0 {
		t.Fatal(w.Code, w.Body.String(), plan.DeleteObjects)
	}
	var planned []string
	for _, d := range plan.Databases {
		for _, e := range d.Delete {
			prefix := "saved/"
			if e.Location == legacyLocation {
				prefix = "legacy/"
			}
			planned = append(planned, prefix+e.Key)
		}
	}
	deleted := bucket.Deleted()
	slices.Sort(planned)
	slices.Sort(deleted)
	if !slices.Equal(planned, deleted) {
		t.Fatalf("deleted %v planned %v", deleted, planned)
	}
	for _, key := range []string{"saved/databases/app/predeployment.dump", "saved/connection-check/abc", "saved/databases/app/readme.txt", "saved/databases/app/bb.dump"} {
		if !slices.Contains(bucket.Keys(), key) {
			t.Fatalf("%s deleted (before %d objects)", key, len(before))
		}
	}
	// The emptied orphan folder is forgotten.
	getPlan(t, c)
	if _, ok := c.data.BackupOrphans["native-archive-old"]; ok {
		t.Fatal("orphan record kept for an empty folder")
	}
}

func TestWorkspaceRetentionKeepsUnknownFoldersWhenAServerIsDown(t *testing.T) {
	bucket := fakes3.New("private")
	defer bucket.Close()
	history(bucket, "saved/", "app", 10)
	history(bucket, "saved/", "elsewhere", 10)
	one := newRetentionAgent(t, agent.DatabaseView{AppID: "app"})
	down := newRetentionAgent(t)
	c := retentionControl(t, bucket, one, down)
	down.Close()
	plan := getPlan(t, c)
	if len(plan.Unavailable) != 1 || plan.Unavailable[0] != "Server two" {
		t.Fatal(plan.Unavailable)
	}
	if d := planFor(t, plan, "elsewhere"); d.Status != retention.StatusUnknown || len(d.Delete) != 0 {
		t.Fatalf("%+v", d)
	}
	if len(c.data.BackupOrphans) != 0 {
		t.Fatal("orphan noticed while a server was down")
	}
	// Automatic cleanup waits for every server.
	c.data.BackupRetention = &retention.Settings{Policy: retention.Policy{Daily: 1}, OrphanGraceDays: 1, Automatic: true}
	result, err := c.automaticRetention(httptest.NewRequest("GET", "/", nil))
	if err != nil || result.Deleted != 0 || len(bucket.Deleted()) != 0 {
		t.Fatal(result, err)
	}
}

func TestAutomaticWorkspaceRetention(t *testing.T) {
	bucket := fakes3.New("private")
	defer bucket.Close()
	history(bucket, "saved/", "app", 10)
	c := retentionControl(t, bucket, newRetentionAgent(t, agent.DatabaseView{AppID: "app"}))
	// Not reviewed: the tick does nothing.
	if err := c.retentionTick(t.Context()); err != nil || len(bucket.Deleted()) != 0 {
		t.Fatal(err, bucket.Deleted())
	}
	c.data.BackupRetention = &retention.Settings{Policy: retention.Policy{Daily: 4}, OrphanGraceDays: 30, Automatic: false}
	if err := c.retentionTick(t.Context()); err != nil || len(bucket.Deleted()) != 0 {
		t.Fatal(err, bucket.Deleted())
	}
	c.data.BackupRetention.Automatic = true
	result, err := c.automaticRetention(httptest.NewRequest("GET", "/", nil))
	if err != nil || result.Deleted != 6 || len(bucket.Keys()) != 4 {
		t.Fatal(result, err, bucket.Keys())
	}
}

func TestRetentionSettingsSavedAndSentToAgents(t *testing.T) {
	bucket := fakes3.New("private")
	defer bucket.Close()
	one := newRetentionAgent(t)
	c := retentionControl(t, bucket, one)
	// Before review, storage sync sends no retention.
	if err := c.syncBackupStorage(httptest.NewRequest("GET", "/", nil), "one"); err != nil || len(one.received("PUT /v1/backup-retention")) != 0 {
		t.Fatal(err, one.calls)
	}
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c.putRetentionSettings(w, httptest.NewRequest("PUT", "/", strings.NewReader(body)))
		return w
	}
	for _, bad := range []string{`{"daily":366,"weekly":4,"monthly":0,"orphan_grace_days":30}`, `{"daily":7,"weekly":4,"monthly":0,"orphan_grace_days":0}`, `{"daily":7,"unknown":1}`} {
		if w := put(bad); w.Code != 400 {
			t.Fatal(bad, w.Code)
		}
	}
	if c.data.BackupRetention != nil {
		t.Fatal("invalid settings saved")
	}
	if w := put(`{"daily":10,"weekly":6,"monthly":12,"orphan_grace_days":60,"automatic":true}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"unsynced_servers":[]`) {
		t.Fatal(w.Code, w.Body.String())
	}
	sent := one.received("PUT /v1/backup-retention")
	if len(sent) != 1 || !strings.Contains(sent[0], `"daily":10`) || !strings.Contains(sent[0], `"orphan_grace_days":60`) {
		t.Fatal(sent)
	}
	if err := c.syncBackupStorage(httptest.NewRequest("GET", "/", nil), "one"); err != nil || len(one.received("PUT /v1/backup-retention")) != 2 {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c.getRetentionSettings(w, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(w.Body.String(), `"reviewed":true`) || !strings.Contains(w.Body.String(), `"monthly":12`) {
		t.Fatal(w.Body.String())
	}
	// Settings survive a restart.
	reloaded, err := New(c.cfg)
	if err != nil || reloaded.data.BackupRetention == nil || reloaded.data.BackupRetention.Monthly != 12 {
		t.Fatal(err)
	}
}

func TestServerRetentionProxies(t *testing.T) {
	bucket := fakes3.New("private")
	defer bucket.Close()
	one := newRetentionAgent(t)
	c := retentionControl(t, bucket, one)
	mux := http.NewServeMux()
	c.backupRetentionRoutes(func(pattern string, fn http.HandlerFunc) { mux.HandleFunc(pattern, fn) })
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	if w := call("GET", "/api/control/servers/one/backups/retention/plan", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "agent-plan") || len(one.received("PUT /v1/backup-storage")) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("POST", "/api/control/servers/one/backups/retention/apply", `{"fingerprint":"old"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "review it again") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("POST", "/api/control/servers/one/backups/retention/apply", `{"fingerprint":"agent-plan"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"deleted":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("PATCH", "/api/control/servers/one/databases/app/backups/retention", `{"daily":500,"weekly":0,"monthly":0}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := call("PATCH", "/api/control/servers/one/databases/app/backups/retention", `{"daily":3,"weekly":2,"monthly":1}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("PATCH", "/api/control/servers/one/databases/app/backups/retention", `null`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	patches := one.received("PATCH /v1/databases/app/backups/retention")
	if len(patches) != 2 || !strings.Contains(patches[0], `"daily":3`) || !strings.HasSuffix(patches[1], "null") {
		t.Fatal(patches)
	}
	if routePermission("GET /api/control/storage/retention/plan") != "infrastructure.manage" || routePermission("GET /api/control/servers/{server}/backups/retention/plan") != "infrastructure.manage" || routePermission("POST /api/control/storage/retention/apply") != "infrastructure.manage" {
		t.Fatal("retention plans must require infrastructure management")
	}
}
