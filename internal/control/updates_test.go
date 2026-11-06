package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

type updateTransport func(*http.Request) (*http.Response, error)

func (f updateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestFleetUpdatesPersistRetryAndUpgradePanelLast(t *testing.T) {
	token := strings.Repeat("a", 32)
	cfg := Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32)), Servers: []Server{{ID: "local", Name: "GUI", URL: "http://127.0.0.1:9090", Token: token}, {ID: "remote", Name: "Remote", URL: "https://agent.example", Token: token}}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]agent.UpgradeStatus{"local": {Current: "v0.2.14", State: "idle", Supported: true}, "remote": {Current: "v0.2.14", State: "idle", Supported: true}}
	queued := []string{}
	busy := true
	client := &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("agent token not forwarded")
		}
		id := "remote"
		if strings.HasPrefix(r.URL.Host, "127.") {
			id = "local"
		}
		code := 200
		var out any = states[id]
		if r.Method == "POST" {
			var in struct{ Version string }
			json.NewDecoder(r.Body).Decode(&in)
			if in.Version != "v0.2.15" {
				t.Error("wrong release")
			}
			if busy {
				code = 409
				busy = false
				out = map[string]string{"error": "deployment running"}
			} else {
				queued = append(queued, id)
				status := states[id]
				status.State = "queued"
				states[id] = status
				code = 202
				out = map[string]string{"status": "queued"}
			}
		}
		body, _ := json.Marshal(out)
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	attach := func() {
		c.client = client
		c.releaseCheck = func(*http.Request) (string, error) { return "v0.2.15", nil }
	}
	attach()
	r := httptest.NewRequest("POST", "/api/control/updates/install", strings.NewReader(`{}`))
	r.SetPathValue("action", "install")
	w := httptest.NewRecorder()
	c.updates(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	if len(queued) != 0 {
		t.Fatal("request restarted GUI before durable dispatch")
	}
	tick := func() {
		t.Helper()
		if err := c.updateTick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if c.updateSettingsSnapshot().Targets[0].State != "waiting" {
		t.Fatal("busy server was not deferred")
	}
	tick()
	if len(queued) != 1 || queued[0] != "remote" {
		t.Fatal("remote must update first", queued)
	}
	// A panel restart must preserve progress and not queue the remote twice.
	c, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	attach()
	tick()
	if len(queued) != 1 {
		t.Fatal("queued update duplicated", queued)
	}
	status := states["remote"]
	status.Current = "v0.2.15"
	status.State = "upgrading"
	states["remote"] = status
	tick()
	if len(queued) != 1 {
		t.Fatal("next host queued before CLI health checks completed", queued)
	}
	status.State = "succeeded"
	states["remote"] = status
	tick()
	if len(queued) != 2 || queued[1] != "local" {
		t.Fatal("panel host was not last", queued)
	}
	status = states["local"]
	status.Current = "v0.2.15"
	status.State = "succeeded"
	states["local"] = status
	tick()
	if updateInProgress(c.updateSettingsSnapshot()) {
		t.Fatal("completed plan remained active")
	}
	// Failed remote upgrades stop local dispatch and permit a manual retry.
	v := c.updateSettingsSnapshot()
	v.Targets[0].State = "updating"
	v.Targets[1].State = "waiting"
	c.saveUpdateSettings(v)
	status = states["remote"]
	status.Current = "v0.2.14"
	status.State = "failed"
	status.Message = "previous binaries restored"
	states["remote"] = status
	tick()
	if updateInProgress(c.updateSettingsSnapshot()) || len(queued) != 2 {
		t.Fatal("failed batch did not stop")
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/control/updates/install", strings.NewReader(`{}`))
	r.SetPathValue("action", "install")
	c.updates(w, r)
	if w.Code != 202 {
		t.Fatal("retry blocked", w.Code, w.Body)
	}
}
func TestRetiredSelfUpdateHookAndPermissions(t *testing.T) {
	if routePermission("POST /api/control/updates/install") != "infrastructure.manage" || routePermission("PATCH /api/control/updates") != "infrastructure.manage" {
		t.Fatal("fleet update must require administrator")
	}
	c, err := New(Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c.Handler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest("POST", "/hooks/self-update", nil))
	if w.Code != 404 {
		t.Fatal("retired hook still available", w.Code)
	}
}

func TestHourlyChecksArePublicAndAutomaticPolicyPersists(t *testing.T) {
	cfg := Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32))}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	c.releaseCheck = func(*http.Request) (string, error) { checks++; return "v0.2.15", nil }
	if err = c.updateTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if checks != 1 || c.updateSettingsSnapshot().Latest != "v0.2.15" || len(c.updateSettingsSnapshot().Targets) != 0 {
		t.Fatal("notify-only check queued installation")
	}
	if err = c.updateTick(context.Background()); err != nil || checks != 1 {
		t.Fatal("release API polled more often than hourly")
	}
	w := httptest.NewRecorder()
	c.updates(w, httptest.NewRequest("PATCH", "/api/control/updates", strings.NewReader(`{"automatic":true}`)))
	c, err = New(cfg)
	if err != nil || !c.updateSettingsSnapshot().Automatic {
		t.Fatal("automatic preference lost after restart")
	}
}

func TestAutomaticModeQueuesWithoutAnOpenBrowser(t *testing.T) {
	token := strings.Repeat("a", 32)
	c, err := New(Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32)), Servers: []Server{{ID: "local", Name: "GUI", URL: "http://127.0.0.1:9090", Token: token}}})
	if err != nil {
		t.Fatal(err)
	}
	c.releaseCheck = func(*http.Request) (string, error) { return "v0.2.15", nil }
	posts := 0
	c.client = &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"current":"v0.2.14","state":"idle","supported":true}`
		if r.Method == "POST" {
			posts++
			body = `{"status":"queued"}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if err = c.saveUpdateSettings(UpdateSettings{Automatic: true}); err != nil {
		t.Fatal(err)
	}
	if err = c.updateTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := c.updateSettingsSnapshot()
	if posts != 1 || v.Version != "v0.2.15" || len(v.Targets) != 1 || v.Targets[0].State != "updating" {
		t.Fatal("automatic durable tick did not queue CLI upgrade", posts, v)
	}
}

func TestUpdateTickWaitsForMovesInACopy(t *testing.T) {
	token := strings.Repeat("a", 32)
	cfg := Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32)),
		Servers: []Server{{ID: "local", Name: "GUI", URL: "http://127.0.0.1:9090", Token: token}, {ID: "alpha", Name: "Alpha", URL: "https://alpha.example", Token: token}, {ID: "beta", Name: "Beta", URL: "https://beta.example", Token: token}}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.releaseCheck = func(*http.Request) (string, error) { return "v0.2.15", nil }
	posts := []string{}
	c.client = &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"current":"v0.2.14","state":"idle","supported":true}`
		if r.Method == "POST" {
			posts = append(posts, r.URL.Host)
			body = `{"status":"queued"}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	setMove := func(id string, change func(*MoveJob)) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.data.Moves == nil {
			c.data.Moves = map[string]MoveJob{}
		}
		job := c.data.Moves[id]
		change(&job)
		c.data.Moves[id] = job
	}
	steps := func(running string) []MoveStep {
		out := []MoveStep{}
		for _, s := range moveStepTitles {
			status := "done"
			if s.key == running {
				status = "running"
			}
			out = append(out, MoveStep{Key: s.key, Title: s.title, Status: status})
		}
		return out
	}
	// The shop moves from alpha to beta and is copying its data.
	setMove("m1", func(job *MoveJob) {
		*job = MoveJob{ID: "m1", AppID: "shop", SourceServer: "alpha", TargetServer: "beta", State: moveStateRunning, Steps: steps(moveStepData)}
	})
	tick := func() {
		t.Helper()
		if err := c.updateTick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/control/updates/install", strings.NewReader(`{}`))
	r.SetPathValue("action", "install")
	c.updates(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	target := func(id string) UpdateTarget {
		for _, t := range c.updateSettingsSnapshot().Targets {
			if t.ServerID == id {
				return t
			}
		}
		return UpdateTarget{}
	}
	tick()
	if got := target("alpha"); got.State != "waiting" || got.Message != "Waiting for the move of shop to Beta to reach a safe point" || len(posts) != 0 {
		t.Fatalf("the move's source agent was not held: %+v %v", got, posts)
	}
	// Waiting at the cutover is a safe point: the agents update.
	setMove("m1", func(job *MoveJob) { job.State = moveStateCutover; job.Steps = steps("") })
	tick()
	tick()
	if len(posts) != 1 || posts[0] != "alpha.example" {
		t.Fatalf("alpha not dispatched once the move waited: %v", posts)
	}
	// A move's final sync between two other servers still holds the panel
	// host, which relays every move's streams, but not an uninvolved agent.
	setMove("m2", func(job *MoveJob) {
		*job = MoveJob{ID: "m2", AppID: "blog", SourceServer: "alpha", TargetServer: "gamma", State: moveStateSyncing, Steps: steps(moveStepFinalSync)}
	})
	v := c.updateSettingsSnapshot()
	for i := range v.Targets {
		if v.Targets[i].ServerID == "alpha" {
			v.Targets[i].State = "succeeded"
		}
	}
	if err := c.saveUpdateSettings(v); err != nil {
		t.Fatal(err)
	}
	tick()
	if len(posts) != 2 || posts[1] != "beta.example" {
		t.Fatalf("beta is not part of the final sync: %v", posts)
	}
	v = c.updateSettingsSnapshot()
	for i := range v.Targets {
		if v.Targets[i].ServerID == "beta" {
			v.Targets[i].State = "succeeded"
		}
	}
	if err := c.saveUpdateSettings(v); err != nil {
		t.Fatal(err)
	}
	tick()
	if got := target("local"); got.State != "waiting" || got.Message != "Waiting for the move of blog to gamma to reach a safe point" || len(posts) != 2 {
		t.Fatalf("the panel host was not held during a final sync: %+v %v", got, posts)
	}
	// The hold survives a restart of the panel and ends with the move.
	c2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c2.client, c2.releaseCheck = c.client, c.releaseCheck
	if err := c2.updateTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 {
		t.Fatalf("restarted panel dispatched itself during a move: %v", posts)
	}
	setMove("m2", func(job *MoveJob) { job.State = moveStateFinished; job.Steps = steps("") })
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	tick()
	if got := target("local"); got.State != "updating" || len(posts) != 3 || posts[2] != "127.0.0.1:9090" {
		t.Fatalf("the panel host was not dispatched once the move finished: %+v %v", got, posts)
	}
}

func TestUpdateQueueSaysWhatAWaitingTargetWaitsFor(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	targets := []UpdateTarget{
		{ServerID: "a", Name: "Alpha", State: "updating", Message: "Installing"},
		{ServerID: "b", Name: "Beta", State: "waiting"},
		{ServerID: "gui", Name: "GUI", State: "waiting", Local: true},
	}
	annotateUpdateQueue(targets, nil, now)
	if targets[0].Message != "Installing" {
		t.Fatalf("the running target's message was replaced: %q", targets[0].Message)
	}
	if targets[1].Message != "Waits for Alpha to finish updating" {
		t.Fatalf("waiting target: %q", targets[1].Message)
	}
	if targets[2].Message != "Waits for Alpha to finish updating; the control panel's host updates last" {
		t.Fatalf("panel host: %q", targets[2].Message)
	}
	if !targets[1].Updated.Equal(now) {
		t.Fatal("a changed target was not stamped")
	}

	// Alpha finishes: Beta becomes the next, the panel host now waits for Beta.
	old := slices.Clone(targets)
	targets[0].State, targets[0].Message = "succeeded", "Release installed"
	later := now.Add(time.Minute)
	annotateUpdateQueue(targets, old, later)
	if targets[1].Message != updateQueueNext || !strings.HasPrefix(targets[2].Message, "Waits for Beta to finish updating") {
		t.Fatalf("queue did not advance: %+v", targets)
	}

	// A hold set by the tick is the target's own reason and stays.
	targets[1].Message = "Waiting for the move of shop to Beta to reach a safe point"
	annotateUpdateQueue(targets, slices.Clone(targets), later)
	if targets[1].Message != "Waiting for the move of shop to Beta to reach a safe point" {
		t.Fatalf("move hold replaced: %q", targets[1].Message)
	}

	// A failed target stops the rollout; the next ones say so.
	targets[1].State, targets[1].Message = "failed", "health checks failed"
	annotateUpdateQueue(targets, slices.Clone(targets), later)
	if !strings.HasPrefix(targets[2].Message, "Waits for Beta, whose update failed") {
		t.Fatalf("failed head: %q", targets[2].Message)
	}
	unchanged := slices.Clone(targets)
	annotateUpdateQueue(targets, unchanged, later.Add(time.Hour))
	if !targets[2].Updated.Equal(later) {
		t.Fatal("an unchanged target was restamped")
	}
}

func TestPreparedUpdatePlanExplainsTheQueue(t *testing.T) {
	token := strings.Repeat("a", 32)
	cfg := Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32)),
		Servers: []Server{{ID: "local", Name: "GUI", URL: "http://127.0.0.1:9090", Token: token}, {ID: "alpha", Name: "Alpha", URL: "https://alpha.example", Token: token}}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	v := c.updateSettingsSnapshot()
	v.Latest = "v0.2.15"
	if err := c.prepareUpdatePlan(v); err != nil {
		t.Fatal(err)
	}
	got := c.updateSettingsSnapshot().Targets
	if got[0].ServerID != "alpha" || got[0].Message != updateQueueNext {
		t.Fatalf("first target: %+v", got[0])
	}
	if got[1].Message != "Waits for Alpha to finish updating; the control panel's host updates last" || !got[1].Local {
		t.Fatalf("panel host: %+v", got[1])
	}
}
