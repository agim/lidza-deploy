package control

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/jobs"
)

func activityControl(t *testing.T) *Control {
	t.Helper()
	token := strings.Repeat("a", 32)
	c, err := New(Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32)),
		Servers: []Server{{ID: "local", Name: "GUI", URL: "http://127.0.0.1:9090", Token: token}, {ID: "media", Name: "media-01", URL: "https://media.example", Token: token}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func activityByID(items []ActivityItem, id string) ActivityItem {
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	return ActivityItem{}
}

func TestActivityListsMovesWithStepProgressAndWaits(t *testing.T) {
	c := activityControl(t)
	now := time.Now().UTC()
	steps := func(running string) []MoveStep {
		out, seen := []MoveStep{}, false
		for _, s := range moveStepTitles {
			status := "done"
			if seen {
				status = "pending"
			}
			if s.key == running {
				status, seen = "running", true
			}
			out = append(out, MoveStep{Key: s.key, Title: s.title, Status: status, Log: []MoveLogLine{{Time: now, Message: "copied " + s.key}}})
		}
		return out
	}
	retry := now.Add(2 * time.Minute)
	c.mu.Lock()
	c.data.Moves = map[string]MoveJob{
		"m1": {ID: "m1", AppID: "analytics", SourceServer: "local", TargetServer: "media", State: moveStateRunning, Steps: steps(moveStepData), Created: now.Add(-time.Hour), Updated: now},
		"m2": {ID: "m2", AppID: "shop", SourceServer: "local", TargetServer: "media", State: moveStateCutover, Hostnames: []string{"shop.example"}, Steps: steps(""), Created: now.Add(-time.Hour), Updated: now},
		"m3": {ID: "m3", AppID: "blog", SourceServer: "local", TargetServer: "media", State: moveStateRunning, Steps: steps(moveStepFiles), Created: now, Updated: now,
			Failure: &MoveFailure{Step: moveStepFiles, Message: "agent unavailable", Attempt: 1, MaxAttempts: 5, NextRetry: &retry}},
		"old": {ID: "old", AppID: "gone", State: moveStateFinished, Updated: now.Add(-3 * time.Hour)},
		"new": {ID: "new", AppID: "done", TargetServer: "media", State: moveStateFinished, Updated: now.Add(-time.Minute)},
	}
	c.mu.Unlock()
	items := c.moveActivity(now)
	if len(items) != 4 {
		t.Fatalf("want 4 moves (old finished one dropped), got %d", len(items))
	}
	running := activityByID(items, "move:m1")
	if running.Title != "Moving analytics to media-01" || running.State != "running" || running.ServerName != "media-01" {
		t.Fatalf("running move: %+v", running)
	}
	if running.Link != "/console/applications?move=analytics" || running.Progress == nil || running.Progress.Unit != "steps" || running.Progress.Done != 3 || running.Progress.Total != int64(len(moveStepTitles)) {
		t.Fatalf("running move link/progress: %+v %+v", running.Link, running.Progress)
	}
	if running.Message != "Copy data" || !strings.Contains(strings.Join(running.Details, "\n"), "Step 4 of 9: Copy data") || !strings.Contains(strings.Join(running.Details, "\n"), "copied data") {
		t.Fatalf("running move details: %q %q", running.Message, running.Details)
	}
	cutover := activityByID(items, "move:m2")
	if cutover.State != "waiting" || cutover.WaitingOn != "Waiting for DNS of shop.example to point to media-01" {
		t.Fatalf("cutover move: %+v", cutover)
	}
	retrying := activityByID(items, "move:m3")
	if retrying.State != "waiting" || !strings.HasPrefix(retrying.WaitingOn, "Retrying Copy files at ") || !strings.Contains(retrying.WaitingOn, "attempt 2 of 5") {
		t.Fatalf("retrying move: %+v", retrying)
	}
	if done := activityByID(items, "move:new"); done.State != "succeeded" || done.Title != "Moved done to media-01" {
		t.Fatalf("finished move: %+v", done)
	}
}

func TestActivityListsDeploymentsQueuedRunningAndRecent(t *testing.T) {
	c := activityControl(t)
	now := time.Now().UTC()
	started, finished, old := now.Add(-time.Minute), now.Add(-10*time.Minute), now.Add(-5*time.Hour)
	rows := []activityDeployment{
		{ServerID: "media", Deployment: agent.Deployment{ID: "d1", AppID: "storefront", Kind: "deploy", Status: "building", Created: started, Started: &started, Commit: "abcdef123456", CommitMessage: "Fix menu\n\nbody"}},
		{ServerID: "media", Deployment: agent.Deployment{ID: "d2", AppID: "shop", Kind: "deploy", Status: "queued", Created: now, Progress: &agent.Progress{Phase: "waiting for another deployment on this server (deploying storefront)"}}},
		{ServerID: "media", Deployment: agent.Deployment{ID: "d3", AppID: "blog", Kind: "rollback", Status: "live", Created: finished, Finished: &finished, Duration: 42}},
		{ServerID: "media", Deployment: agent.Deployment{ID: "d4", AppID: "blog", Kind: "deploy", Status: "live", Created: old, Finished: &old}},
		{ServerID: "media", Deployment: agent.Deployment{ID: "d5", AppID: "api", Kind: "deploy", Status: "failed", Created: old, Finished: &old, Error: "bundle install failed"}},
	}
	items := c.deploymentActivity(rows, now)
	if len(items) != 4 {
		t.Fatalf("want 4 deployments (old success dropped), got %+v", items)
	}
	building := activityByID(items, "deployment:media:d1")
	if building.Title != "Deploying storefront" || building.State != "running" || building.Link != "/console/deployments/d1" || building.Details[0] != "Commit abcdef1 Fix menu" {
		t.Fatalf("building: %+v", building)
	}
	queued := activityByID(items, "deployment:media:d2")
	if queued.State != "waiting" || queued.WaitingOn != "Waiting for another deployment on this server (deploying storefront)" {
		t.Fatalf("queued: %+v", queued)
	}
	if done := activityByID(items, "deployment:media:d3"); done.State != "succeeded" || done.Title != "Rolled back blog" || done.Message != "Finished in 42s" {
		t.Fatalf("finished: %+v", done)
	}
	if failed := activityByID(items, "deployment:media:d5"); failed.State != "failed" || failed.Message != "bundle install failed" {
		t.Fatalf("failed deployment within a day must stay: %+v", failed)
	}
}

func TestActivityListsUpdateTargetsWithTheirWaitingReason(t *testing.T) {
	c := activityControl(t)
	now := time.Now().UTC()
	v := c.updateSettingsSnapshot()
	v.Latest = "v0.3.0-private.48"
	if err := c.prepareUpdatePlan(v); err != nil {
		t.Fatal(err)
	}
	v = c.updateSettingsSnapshot()
	v.Targets[0].State, v.Targets[0].Message, v.Targets[0].Started = "updating", "Restarting the agent", now
	if err := c.saveUpdateSettings(v); err != nil {
		t.Fatal(err)
	}
	items := c.updateActivity(c.updateSettingsSnapshot(), now)
	if len(items) != 2 {
		t.Fatalf("want both targets, got %+v", items)
	}
	media := activityByID(items, "update:v0.3.0-private.48:media")
	if media.Title != "Updating media-01 to v0.3.0-private.48" || media.State != "running" || media.Message != "Restarting the agent" || media.Link != "/console/updates" {
		t.Fatalf("updating target: %+v", media)
	}
	panel := activityByID(items, "update:v0.3.0-private.48:local")
	if panel.State != "waiting" || panel.WaitingOn != "Waits for media-01 to finish updating; the control panel's host updates last" {
		t.Fatalf("waiting target: %+v", panel)
	}
	if panel.Progress == nil || panel.Progress.Done != 0 || panel.Progress.Total != 2 || panel.Progress.Unit != "servers" {
		t.Fatalf("rollout progress: %+v", panel.Progress)
	}
}

func TestActivityListsRetryingAndFailedJobs(t *testing.T) {
	c := activityControl(t)
	c.mu.Lock()
	c.data.Apps = map[string]Application{"storefront": {ID: "storefront", ServerID: "media"}}
	c.mu.Unlock()
	now := time.Now().UTC()
	failure := "github: 502 bad gateway"
	rows := []jobs.Job{
		{ID: "j1", Kind: "deploy.push", Payload: json.RawMessage(`{"app_id":"storefront","delivery":"x"}`), State: "pending", RunAt: now.Add(time.Minute), Attempts: 2, MaxAttempts: 12, LastError: &failure},
		{ID: "j2", Kind: "errors.sync", State: "failed", RunAt: now.Add(-time.Hour), Attempts: 3, MaxAttempts: 3, LastError: &failure},
		{ID: "j3", Kind: "move.tick", State: "running", RunAt: now},
		{ID: "j4", Kind: "errors.sync", State: "failed", RunAt: now.Add(-48 * time.Hour), Attempts: 3, MaxAttempts: 3},
		{ID: "j5", Kind: "deploy.push", State: "done", RunAt: now},
	}
	items := c.jobActivity(rows, now)
	if len(items) != 2 {
		t.Fatalf("want the retrying push and the recent failure, got %+v", items)
	}
	push := activityByID(items, "job:j1")
	if push.Title != "Auto-deploy of storefront" || push.State != "waiting" || push.ServerName != "media-01" || !strings.Contains(push.WaitingOn, "attempt 3 of 12") || push.Message != failure {
		t.Fatalf("retrying push: %+v", push)
	}
	if sync := activityByID(items, "job:j2"); sync.State != "failed" || sync.Link != "/console/errors" {
		t.Fatalf("failed sync: %+v", sync)
	}
}

func TestActivityEndpointGroupsAndCachesTheFleetRead(t *testing.T) {
	c := activityControl(t)
	reads := 0
	c.client = &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		reads++
		body := `[]`
		if strings.Contains(r.URL.Host, "media") {
			body = `[{"id":"d1","app_id":"storefront","status":"building","kind":"deploy","created":"` + time.Now().UTC().Format(time.RFC3339) + `"}]`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	c.mu.Lock()
	c.data.Moves = map[string]MoveJob{"m1": {ID: "m1", AppID: "analytics", TargetServer: "media", State: moveStatePlanned, Updated: time.Now().UTC()}}
	c.mu.Unlock()
	get := func() []ActivityItem {
		w := httptest.NewRecorder()
		c.activity(w, httptest.NewRequest("GET", "/api/control/activity", nil))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		var out struct{ Items []ActivityItem }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Items
	}
	items := get()
	if len(items) != 2 || items[0].State != "running" || items[0].Title != "Deploying storefront" || items[1].State != "waiting" || items[1].Kind != "move" {
		t.Fatalf("items: %+v", items)
	}
	first := reads
	get()
	if reads != first {
		t.Fatal("the fleet was read again within the cache window")
	}
}
