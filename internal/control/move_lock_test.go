package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// lockedMoveRoutes is every mutating route a move locks. A new route under
// an application or one of its databases fails this test until it is added
// here or exempted in moveLockExempt with a reason.
var lockedMoveRoutes = []string{
	"POST /api/control/native-apps/{id}/services/{unit}/{action}",
	"POST /api/control/apps/{id}/apply-defaults",
	"PUT /api/control/apps/{id}/previews",
	"PUT /api/control/apps/{id}/redirects",
	"PUT /api/control/apps/{id}/tasks/{task}",
	"POST /api/control/apps/{id}/tasks/{task}/{action}",
	"PUT /api/control/apps/{id}/{feature}",
	"POST /api/control/apps/{id}/restore",
	"POST /api/control/apps/{id}/cache",
	"POST /api/control/apps/{id}/database",
	"POST /api/control/apps/{id}/database/{action}",
	"PATCH /api/control/apps/{id}/backups",
	"PATCH /api/control/servers/{server}/databases/{id}/backups/retention",
	"POST /api/control/servers/{server}/databases/{id}/backups/{backup}/restore",
	"POST /api/control/apps/{id}/master-key",
	"PATCH /api/control/apps/{id}/settings",
	"DELETE /api/control/apps/{id}/webhook",
	"DELETE /api/control/apps/{id}",
	"POST /api/control/apps/{id}/reload",
	"POST /api/control/apps/{id}/stop",
	"POST /api/control/apps/{id}/start",
	"POST /api/control/apps/{id}/restart",
	"POST /api/control/servers/{server}/databases/{id}/{action}",
	"PATCH /api/control/servers/{server}/databases/{id}/backups",
	"PATCH /api/control/servers/{server}/databases/{id}/backups/before-deploy",
	"PATCH /api/control/servers/{server}/databases/{id}/backups/excluded-tables",
	"POST /api/control/apps/{id}/deploy",
	"POST /api/control/apps/{id}/rollback",
	"POST /api/control/apps/{id}/webhook",
	"POST /api/control/apps/{id}/github-app",
	"POST /api/control/apps/{id}/owner-claim",
}

func newMoveLockControl(t *testing.T, agentURL string) *Control {
	t.Helper()
	c, err := New(Config{PublicURL: "https://deploy.example.com", User: "operator@example.com", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(),
		Servers: []Server{{ID: "alpha", Name: "Alpha", URL: agentURL, Token: strings.Repeat("a", 32)}, {ID: "beta", Name: "Beta", URL: "http://127.0.0.1:9", Token: strings.Repeat("b", 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	c.data.Apps["shop"] = Application{ID: "shop", ServerID: "alpha", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", AutoDeploy: true, Secret: "webhook-secret", Generation: "g1"}
	c.data.Moves = map[string]MoveJob{"m1": {ID: "m1", AppID: "shop", SourceServer: "alpha", TargetServer: "beta", State: moveStateRunning, Created: time.Now().UTC(),
		Databases:  []MoveDatabase{{SourceID: "shop-db", TargetID: "shop-db-new"}},
		ClickHouse: &MoveClickHouse{SourceDatabase: "shop-events", TargetDatabase: "shop-events"}}}
	return c
}

func (c *Control) setMoveState(id, state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	job := c.data.Moves[id]
	job.State = state
	c.data.Moves[id] = job
}

// moveLockRequest fills a route's path values for the held application and
// the database the move copies on its source.
func moveLockRequest(pattern string) *http.Request {
	method, path, _ := strings.Cut(pattern, " ")
	r := httptest.NewRequest(method, "/", nil)
	values := map[string]string{"id": "shop", "server": "alpha", "unit": "web.service", "action": "restart", "task": "nightly", "feature": "maintenance", "backup": "b1", "key": "SECRET"}
	if strings.HasPrefix(path, "/api/control/servers/{server}/databases/{id}/") {
		values["id"] = "shop-db"
		values["action"] = "provision"
	}
	for name, value := range values {
		if strings.Contains(path, "{"+name+"}") {
			r.SetPathValue(name, value)
		}
	}
	return r
}

func TestMoveLockCoversEveryMutatingApplicationRoute(t *testing.T) {
	c := &Control{}
	var locked, all []string
	c.routes(func(pattern string, _ http.HandlerFunc) {
		all = append(all, pattern)
		if moveLockRoute(pattern) != moveLockNone {
			locked = append(locked, pattern)
		}
	})
	slices.Sort(locked)
	want := slices.Clone(lockedMoveRoutes)
	slices.Sort(want)
	if !slices.Equal(locked, want) {
		t.Fatalf("locked routes changed:\n got %q\nwant %q", locked, want)
	}
	for pattern := range moveLockExempt {
		if !slices.Contains(all, pattern) {
			t.Errorf("exempt route %q is not registered", pattern)
		}
	}
	// The move's own routes are never locked: they start, follow, resume,
	// finish and abort the move that holds the application.
	for _, pattern := range []string{"POST /api/control/apps/{id}/move", "POST /api/control/moves/{id}/{action}", "GET /api/control/apps/{id}/move"} {
		if moveLockRoute(pattern) != moveLockNone {
			t.Errorf("%s is locked", pattern)
		}
	}
	// Read-only routes stay open.
	for _, pattern := range all {
		if method, _, _ := strings.Cut(pattern, " "); method == http.MethodGet && moveLockRoute(pattern) != moveLockNone {
			t.Errorf("read-only %s is locked", pattern)
		}
	}
}

// setMovePhase puts the fixture's move into a background or locked phase.
func (c *Control) setMovePhase(id, state string, change func(*MoveJob)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	job := c.data.Moves[id]
	job.State = state
	job.Steps = []MoveStep{{Key: moveStepData, Status: "done"}, {Key: moveStepDeploy, Status: "done"}, {Key: moveStepFinalSync, Status: "skipped"}, {Key: moveStepFinish, Status: "pending"}}
	job.SourceFrozen, job.Switched = false, false
	if change != nil {
		change(&job)
	}
	c.data.Moves[id] = job
}

func TestMoveLockAppliesOnlyFromTheFinalSync(t *testing.T) {
	background := map[string]func(*MoveJob){
		"running":        nil,
		"failed in data": func(job *MoveJob) { job.Steps[0].Status = "failed" },
		"cutover":        nil,
		"ready":          nil,
	}
	backgroundStates := map[string]string{"running": moveStateRunning, "failed in data": moveStateFailed, "cutover": moveStateCutover, "ready": moveStateReady}
	locked := map[string]struct {
		state  string
		change func(*MoveJob)
	}{
		"final sync":          {moveStateSyncing, func(job *MoveJob) { job.Steps[2].Status = "running"; job.SourceFrozen = true }},
		"synced, waiting DNS": {moveStateCutover, func(job *MoveJob) { job.Steps[2].Status = "done"; job.SourceFrozen = true }},
		"failed final sync":   {moveStateCutover, func(job *MoveJob) { job.Steps[2].Status = "failed" }},
		"finishing":           {moveStateFinishing, func(job *MoveJob) { job.Steps[3].Status = "running" }},
		"failed finish":       {moveStateFailed, func(job *MoveJob) { job.Steps[3].Status = "failed"; job.Switched = true }},
	}
	for _, pattern := range lockedMoveRoutes {
		t.Run(pattern, func(t *testing.T) {
			c := newMoveLockControl(t, "http://127.0.0.1:1")
			var reached atomic.Int32
			h := c.moveLock(pattern, func(w http.ResponseWriter, r *http.Request) { reached.Add(1); w.WriteHeader(http.StatusOK) })
			for name, change := range background {
				c.setMovePhase("m1", backgroundStates[name], change)
				w := httptest.NewRecorder()
				h(w, moveLockRequest(pattern))
				if w.Code != http.StatusOK {
					t.Fatalf("%s during the background phase (%s): %d %s", pattern, name, w.Code, w.Body.String())
				}
			}
			if int(reached.Load()) != len(background) {
				t.Fatalf("background changes reached the handler %d times", reached.Load())
			}
			// Each change is recorded with the move for the final sync.
			if job, _ := c.moveJob("m1"); len(job.SourceChanges) == 0 {
				t.Fatal("a background change is recorded with the move")
			}
			for name, phase := range locked {
				c.setMovePhase("m1", phase.state, phase.change)
				w := httptest.NewRecorder()
				h(w, moveLockRequest(pattern))
				var body struct {
					Error string `json:"error"`
				}
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if w.Code != http.StatusConflict || body.Error != "shop is in the final sync of its move to Beta; finish or abort the move first" {
					t.Fatalf("%s during %s: %d %s", pattern, name, w.Code, w.Body.String())
				}
			}
			if int(reached.Load()) != len(background) {
				t.Fatal("a locked route reached its handler")
			}
			for _, state := range []string{moveStateFinished, moveStateAborted} {
				c.setMoveState("m1", state)
				w := httptest.NewRecorder()
				h(w, moveLockRequest(pattern))
				if w.Code != http.StatusOK {
					t.Fatalf("%s after %s: %d %s", pattern, state, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestMoveLockScopesToTheMovedApplicationAndItsDatabases(t *testing.T) {
	c := newMoveLockControl(t, "http://127.0.0.1:1")
	c.setMovePhase("m1", moveStateSyncing, func(job *MoveJob) { job.Steps[2].Status = "running" })
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	call := func(pattern string, values map[string]string) int {
		method, _, _ := strings.Cut(pattern, " ")
		r := httptest.NewRequest(method, "/", nil)
		for k, v := range values {
			r.SetPathValue(k, v)
		}
		w := httptest.NewRecorder()
		c.moveLock(pattern, ok)(w, r)
		return w.Code
	}
	policy := "PATCH /api/control/servers/{server}/databases/{id}/backups"
	action := "POST /api/control/servers/{server}/databases/{id}/{action}"
	for _, tc := range []struct {
		name    string
		pattern string
		values  map[string]string
		want    int
	}{
		{"another application", "PATCH /api/control/apps/{id}/settings", map[string]string{"id": "blog"}, 200},
		{"source database", policy, map[string]string{"server": "alpha", "id": "shop-db"}, 409},
		{"destination database", policy, map[string]string{"server": "beta", "id": "shop-db-new"}, 409},
		{"ClickHouse database", policy, map[string]string{"server": "beta", "id": "shop-events"}, 409},
		{"same name on an unrelated server", policy, map[string]string{"server": "gamma", "id": "shop-db"}, 200},
		{"another database", policy, map[string]string{"server": "alpha", "id": "blog"}, 200},
		{"backup on demand", action, map[string]string{"server": "alpha", "id": "shop-db", "action": "backup"}, 200},
		{"provision", action, map[string]string{"server": "alpha", "id": "shop-db", "action": "provision"}, 409},
		{"credential reveal", "POST /api/control/apps/{id}/environment/{key}/reveal", map[string]string{"id": "shop", "key": "SECRET"}, 200},
		{"settings read", "GET /api/control/apps/{id}/settings", map[string]string{"id": "shop"}, 200},
	} {
		if got := call(tc.pattern, tc.values); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	// In the background phase the source's databases are open; the
	// destination's belong to the move until it ends.
	c.setMovePhase("m1", moveStateRunning, nil)
	if got := call(policy, map[string]string{"server": "alpha", "id": "shop-db"}); got != 200 {
		t.Errorf("source database during the background copy: %d", got)
	}
	if got := call(policy, map[string]string{"server": "beta", "id": "shop-db-new"}); got != 409 {
		t.Errorf("destination database during the background copy: %d", got)
	}
}

func TestPushDuringMoveIsSkippedNotQueued(t *testing.T) {
	var agentCalls atomic.Int32
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { agentCalls.Add(1); w.WriteHeader(500) }))
	defer agentSrv.Close()
	c := newMoveLockControl(t, agentSrv.URL)
	c.setMovePhase("m1", moveStateSyncing, func(job *MoveJob) { job.Steps[2].Status = "running"; job.SourceFrozen = true })
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)

	body := `{"ref":"refs/heads/main","repository":{"full_name":"acme/shop"}}`
	mac := hmac.New(sha256.New, []byte("webhook-secret"))
	mac.Write([]byte(body))
	r := httptest.NewRequest("POST", "/hooks/github/shop", strings.NewReader(body))
	r.SetPathValue("id", "shop")
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	r.Header.Set("X-GitHub-Event", "push")
	r.Header.Set("X-GitHub-Delivery", "delivery-1")
	w := httptest.NewRecorder()
	// Queueing would need the jobs service, which this control plane does
	// not have: reaching it fails the test.
	c.webhook(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"skipped"`) {
		t.Fatalf("push during move: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(logged.String(), "auto-deploy of shop skipped: final sync of its move to beta (move m1, syncing); delivery delivery-1 not deployed") {
		t.Fatalf("no skip log line: %q", logged.String())
	}

	// A delivery queued before the move started is dropped when it runs.
	payload, _ := json.Marshal(pushJob{AppID: "shop", Delivery: "delivery-0", Generation: "g1"})
	if err := c.dispatchPush(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if agentCalls.Load() != 0 {
		t.Fatalf("queued push reached the agent %d times", agentCalls.Load())
	}

	// Once the move is aborted, the queued delivery deploys again.
	c.setMoveState("m1", moveStateAborted)
	if err := c.dispatchPush(context.Background(), payload); err == nil || agentCalls.Load() == 0 {
		t.Fatalf("push after the move did not reach the agent: %v", err)
	}
}
