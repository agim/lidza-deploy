package control

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// nativeMoveSource is a native Rails application on alpha without data.
func (fx *moveFixture) nativeMoveSource() {
	native := agent.NativeApplication{ID: "shop", Name: "shop", Runtime: "rails", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", Services: []string{"app-shop-web.service"}}
	fx.c.data.Apps["shop"] = Application{ID: "shop", ServerID: "alpha", Native: &native, Runtime: "rails", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", Generation: "g-source"}
	spec := agent.NativeCreateRequest{ID: "shop", Runtime: "rails", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", HealthPath: "/up",
		Processes: []agent.NativeProcessSpec{{Name: "web", Command: `bin/rails server -p "$PORT"`, Web: true}}}
	fx.a.source = &agent.MoveSource{AppID: "shop", Runtime: "rails", Native: true, Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", Commit: moveTestCommit,
		EnvironmentKeys: []string{"PORT", "SECRET_KEY_BASE"}, Volumes: []string{}, Tasks: []string{}, Blockers: []string{}, Notes: []string{}, CreateSpec: &spec, SpecRecorded: true}
	fx.a.env = agent.MoveEnvironment{AppID: "shop", Native: true, Values: map[string]string{"PORT": "3001", "SECRET_KEY_BASE": "base-secret"}, Kinds: map[string]string{"SECRET_KEY_BASE": "credential"}}
	fx.a.nativeSource = &agent.NativeStatus{Application: native}
}

// An abort removes the application the move created even when the
// destination agent restarted while it was created and again while the
// abort ran: an unreachable agent never counts as "already removed".
func TestMoveAbortRemovesTheCreatedNativeTargetAcrossAgentRestarts(t *testing.T) {
	fx := newMoveFixture(t)
	fx.nativeMoveSource()
	// The destination restarts during Prepare: its status check fails
	// before and after the creation, so the step is retried and adopts
	// the application it created.
	fx.b.faults = map[string][]moveFault{"GET /v1/native/apps/shop": {{503, "agent restarting"}, {503, "agent restarting"}}}
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover, moveStateFailed)
	if !job.TargetCreated {
		t.Fatalf("the created destination is not recorded: %+v", job.Steps)
	}
	fx.b.with(func() {
		if fx.b.native == nil {
			t.Fatal("the destination application was not created")
		}
	})
	// The abort meets the restarting agent: nothing is reported removed.
	fx.b.with(func() {
		fx.b.faults = map[string][]moveFault{"DELETE /v1/native/apps/shop": {{502, "agent unavailable"}}, "GET /v1/native/apps/shop": {{503, "agent restarting"}}}
	})
	if code := moveAction(t, fx.c, view.ID, "abort", nil); code != 202 {
		t.Fatalf("abort: %d", code)
	}
	job = waitMoveUntil(t, fx.c, view.ID, func(job MoveJob) bool { return job.Error != "" || job.State == moveStateAborted })
	if !job.TargetCreated || !strings.Contains(job.Error, "abort incomplete") {
		t.Fatalf("an unreachable destination counted as removed: created=%t error=%q", job.TargetCreated, job.Error)
	}
	fx.b.with(func() {
		if fx.b.native == nil || len(fx.b.deleted) != 0 {
			t.Fatalf("deleted %v", fx.b.deleted)
		}
	})
	// The retried abort removes it with its data.
	if code := moveAction(t, fx.c, view.ID, "continue", nil); code != 202 {
		t.Fatalf("continue: %d", code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateAborted)
	fx.b.with(func() {
		if !slices.Contains(fx.b.deleted, "/v1/native/apps/shop?purge=1") || fx.b.native != nil {
			t.Fatalf("destination DELETE %v", fx.b.deleted)
		}
	})
	if job.TargetCreated || !moveLogContains(job, "Removed the application from beta") {
		t.Fatalf("abort log %+v", job.Steps[0].Log)
	}
}

// A move that created nothing says so and leaves the destination alone.
func TestMoveAbortWithoutTargetLogsWhyNothingIsRemoved(t *testing.T) {
	fx := newMoveFixture(t)
	fx.nativeMoveSource()
	fx.b.native = &agent.NativeStatus{Application: agent.NativeApplication{ID: "shop", Name: "shop"}}
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	waitMove(t, fx.c, view.ID, moveStateFailed, moveStateRunning)
	if code := moveAction(t, fx.c, view.ID, "abort", nil); code != 202 {
		t.Fatalf("abort: %d", code)
	}
	job := waitMove(t, fx.c, view.ID, moveStateAborted)
	fx.b.with(func() {
		if len(fx.b.deleted) != 0 || fx.b.native == nil {
			t.Fatalf("an application the move did not create was removed: %v", fx.b.deleted)
		}
	})
	if !moveLogContains(job, "created nothing on beta") {
		t.Fatalf("abort log %+v", job.Steps[0].Log)
	}
}

func moveLogContains(job MoveJob, text string) bool {
	for _, step := range job.Steps {
		for _, line := range step.Log {
			if strings.Contains(line.Message, text) {
				return true
			}
		}
	}
	return false
}

// waitMoveUntil waits for a job that satisfies done while no runner holds it.
func waitMoveUntil(t *testing.T, c *Control, id string, done func(MoveJob) bool) MoveJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, _ := c.moveJob(id)
		c.mu.Lock()
		running := c.moveRunning[id]
		c.mu.Unlock()
		if done(job) && !running {
			return job
		}
		time.Sleep(2 * time.Millisecond)
	}
	job, _ := c.moveJob(id)
	t.Fatalf("move %s (%s) never reached the expected state", job.State, job.Error)
	return job
}
