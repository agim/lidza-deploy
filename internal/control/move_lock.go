package control

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/audit"
)

// A move holds its application only for its final sync. While the move
// copies in the background (plan, prepare, environment, data and files,
// destination deploy, waiting for DNS) the application stays fully usable:
// deploys, settings, environment, domains and data actions work and
// webhooks deploy as usual. Each such change is recorded with the move
// (move_reconcile.go), and the final sync picks it up: it re-exports the
// environment, copies the data and files again and redeploys the source's
// current commit on the destination.
//
// From the start of the final sync until the move finishes or is aborted
// (and while a failed final sync waits to be resumed or aborted) every
// mutating panel route of the application answers 409 and pushes are
// skipped, because the source is frozen and its last state is being copied.
// The databases the move created on the destination stay locked for the
// whole move: they belong to the move until it finishes.
//
// The move runner is not affected: it talks to the hosting agents directly
// (moveCall/agentCall), never through these routes, and the destination's
// checkout token comes from /api/agent/checkout-token, which is outside the
// panel API.

// moveLocks reports whether a move holds its application now.
func moveLocks(job MoveJob) bool {
	if terminalMoveState(job.State) {
		return false
	}
	switch {
	case job.State == moveStateSyncing, job.State == moveStateFinishing:
		return true
	case job.SourceFrozen, job.Switched:
		return true
	}
	if job.State == moveStateAborting {
		return false
	}
	for _, key := range []string{moveStepFinalSync, moveStepFinish} {
		if s := job.step(key); s != nil && (s.Status == "running" || s.Status == "failed" || key == moveStepFinalSync && s.Status == "done") {
			return true
		}
	}
	return false
}

// movePhase names what a move does for the panel: "live" while it copies in
// the background, "locked" while it holds the application, "done" after.
func movePhase(job MoveJob) string {
	switch {
	case terminalMoveState(job.State):
		return "done"
	case moveLocks(job):
		return "locked"
	}
	return "live"
}

// moveLockScope says how a route names what a move can hold.
type moveLockScope int

const (
	moveLockNone     moveLockScope = iota
	moveLockApp                    // {id} is the application
	moveLockDatabase               // {server} and {id} name a database the move copies
)

// moveLockExempt lists the mutating routes of an application that stay open
// during a move, and why. Everything else under the application is locked.
var moveLockExempt = map[string]string{
	"POST /api/control/apps/{id}/move":                                          "a move that is already active answers with its own blocker",
	"POST /api/control/apps/{id}/environment/{key}/reveal":                      "shows a saved credential and changes nothing",
	"POST /api/control/servers/{server}/databases/{id}/backups/{backup}/upload": "uploads an existing backup off-site and changes nothing the move copies",
}

// moveLockRoute classifies a registered route pattern.
func moveLockRoute(pattern string) moveLockScope {
	method, path, _ := strings.Cut(pattern, " ")
	if method == http.MethodGet || method == http.MethodHead {
		return moveLockNone
	}
	if _, exempt := moveLockExempt[pattern]; exempt {
		return moveLockNone
	}
	switch {
	case path == "/api/control/apps/{id}" || strings.HasPrefix(path, "/api/control/apps/{id}/"),
		strings.HasPrefix(path, "/api/control/native-apps/{id}/"):
		return moveLockApp
	case strings.HasPrefix(path, "/api/control/servers/{server}/databases/{id}/"):
		return moveLockDatabase
	}
	return moveLockNone
}

// moveLock refuses a locked route while a move holds what it changes, and
// records a successful change made while the move copies in the
// background.
func (c *Control) moveLock(pattern string, next http.HandlerFunc) http.HandlerFunc {
	scope := moveLockRoute(pattern)
	if scope == moveLockNone {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Taking a backup on demand changes nothing the move copies.
		if scope == moveLockDatabase && r.PathValue("action") == "backup" {
			next(w, r)
			return
		}
		var job MoveJob
		var active, held bool
		if scope == moveLockApp {
			job, active = c.activeMove(r.PathValue("id"))
			held = active && moveLocks(job)
		} else {
			var target bool
			job, active, target = c.activeMoveOfDatabase(r.PathValue("server"), r.PathValue("id"))
			held = active && (target || moveLocks(job))
		}
		if held {
			agent.Fail(w, http.StatusConflict, c.moveLockError(job))
			return
		}
		if !active {
			next(w, r)
			return
		}
		recorder := &moveStatusWriter{ResponseWriter: w, status: http.StatusOK}
		next(recorder, r)
		if recorder.status < 300 {
			c.noteMoveSourceChange(job.ID, moveChangeKind(pattern), pattern)
		}
	}
}

// moveStatusWriter remembers the status a handler answered with.
type moveStatusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *moveStatusWriter) WriteHeader(status int) {
	if !w.wrote {
		w.status, w.wrote = status, true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *moveStatusWriter) Write(p []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(p)
}

func (w *moveStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// activeMove returns the application's move that is not finished or
// aborted, if any.
func (c *Control) activeMove(appID string) (MoveJob, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	job, ok := c.activeMoveLocked(appID)
	return cloneMoveJob(job), ok
}

// lockingMove returns the move that holds appID now, if any.
func (c *Control) lockingMove(appID string) (MoveJob, bool) {
	job, ok := c.activeMove(appID)
	return job, ok && moveLocks(job)
}

// activeMoveOfDatabase returns the active move that copies database id on
// server, on either side of the move; target reports the destination side.
func (c *Control) activeMoveOfDatabase(server, id string) (job MoveJob, found, target bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, job := range c.data.Moves {
		if terminalMoveState(job.State) {
			continue
		}
		side := func(source, destination string) (bool, bool) {
			if job.TargetServer == server && destination != "" && destination == id {
				return true, true
			}
			return job.SourceServer == server && source == id, false
		}
		for _, d := range job.Databases {
			if hit, dest := side(d.SourceID, d.TargetID); hit {
				return cloneMoveJob(job), true, dest
			}
		}
		if job.ClickHouse != nil {
			if hit, dest := side(job.ClickHouse.SourceDatabase, job.ClickHouse.TargetDatabase); hit {
				return cloneMoveJob(job), true, dest
			}
		}
	}
	return MoveJob{}, false, false
}

func (c *Control) moveLockError(job MoveJob) error {
	if !moveLocks(job) {
		return fmt.Errorf("%s is moving to %s and this database belongs to the move until it finishes; finish or abort the move first", job.AppID, c.serverName(job.TargetServer))
	}
	return fmt.Errorf("%s is in the final sync of its move to %s; finish or abort the move first", job.AppID, c.serverName(job.TargetServer))
}

// serverName is the server's display name, or its id when it has none.
func (c *Control) serverName(id string) string {
	for _, s := range c.servers() {
		if s.ID == id && s.Name != "" {
			return s.Name
		}
	}
	return id
}

// moveSummary is what the application list shows of an application's
// active move; the move panel loads the full job.
type moveSummary struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	SourceServer string `json:"source_server"`
	TargetServer string `json:"target_server"`
	// Locked reports the final-sync lock; Phase is live or locked.
	Locked bool   `json:"locked"`
	Phase  string `json:"phase"`
	// Step is the current step's title, Error the last failure's message
	// and Retry when its automatic retry runs.
	Step  string     `json:"step,omitempty"`
	Error string     `json:"error,omitempty"`
	Retry *time.Time `json:"retry,omitempty"`
}

// activeMoveSummaries maps each application with an active move to it.
func (c *Control) activeMoveSummaries() map[string]moveSummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]moveSummary{}
	for _, job := range c.data.Moves {
		if terminalMoveState(job.State) {
			continue
		}
		summary := moveSummary{ID: job.ID, State: job.State, SourceServer: job.SourceServer, TargetServer: job.TargetServer, Locked: moveLocks(job), Phase: movePhase(job), Error: job.Error}
		if step := moveCurrentStep(job); step != nil {
			summary.Step = moveStepTitle(step.Key, step.Title)
		}
		if job.Failure != nil && job.Failure.NextRetry != nil {
			retry := *job.Failure.NextRetry
			summary.Retry = &retry
		}
		out[job.AppID] = summary
	}
	return out
}

// moveCurrentStep is the step a move is on: running, failed or waiting,
// else the first pending one.
func moveCurrentStep(job MoveJob) *MoveStep {
	for i := range job.Steps {
		switch job.Steps[i].Status {
		case "running", "failed", "waiting":
			return &job.Steps[i]
		}
	}
	for i := range job.Steps {
		if job.Steps[i].Status == "pending" {
			return &job.Steps[i]
		}
	}
	return nil
}

// skipPushDuringMove reports whether a push delivery for a must be dropped
// because the move's final sync holds it, and records why. A push during the
// background copy deploys normally; the move records it and the final sync
// redeploys the source's new commit on the destination.
func (c *Control) skipPushDuringMove(ctx context.Context, a Application, delivery string) bool {
	job, held := c.lockingMove(a.ID)
	if !held {
		return false
	}
	log.Printf("auto-deploy of %s skipped: final sync of its move to %s (move %s, %s); delivery %s not deployed", a.ID, job.TargetServer, job.ID, job.State, delivery)
	if record, ok := lidza.Optional[*audit.Audit](ctx); ok && record != nil {
		meta := map[string]string{"move": job.ID, "target": job.TargetServer, "reason": "move final sync"}
		if len(delivery) <= 64 && safeAuditID(delivery) {
			meta["delivery"] = delivery
		}
		_ = record.Record(audit.System(ctx, "github-webhook"), audit.Event{Action: "deploy.push.skipped", Scope: fleetScope, Resource: "app/" + a.ID, Meta: meta})
	}
	return true
}
