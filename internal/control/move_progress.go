package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Live progress of a running move step. Every running step has a tracker
// in memory: the step reports its current phase into it, with byte counts
// while a dump or archive streams through the control panel or while an
// agent creates or restores one (polled from the agent). GET
// /api/control/moves/{id} shows the live progress on the running step; the
// encrypted state file is written only on step transitions, which persist
// the last progress with the step, and by heartbeats.
//
// A heartbeat is a log line written when a step with a phase has logged
// nothing for moveHeartbeatEvery: "Still dumping shop on alpha: 1.2 GiB
// written (4m0s elapsed)." Consecutive heartbeats replace each other, so a
// long step keeps one current heartbeat instead of filling its log. Phases
// name databases, file sets and servers only, never values or credentials.

const moveHeartbeatEvery = 30 * time.Second

// Stall detection. A step whose phase has live counters (bytes or objects,
// agent.Progress.Counted) and whose counters stand still for
// moveStallWarnAfter gets a warning line and a warning in the panel; after
// moveStallCancelAfter the attempt is cancelled with the agents'
// diagnostics captured, and retried like any transient failure. Phases
// without counters (an agent that reports none, a deploy) only show their
// elapsed time and are bounded by their own timeouts.
const (
	moveStallWarnAfter   = 10 * time.Minute
	moveStallCancelAfter = 30 * time.Minute
)

// MoveStall describes a running step whose progress stands still.
type MoveStall struct {
	Since      time.Time `json:"since"`
	Phase      string    `json:"phase"`
	DoneBytes  int64     `json:"done_bytes"`
	TotalBytes int64     `json:"total_bytes,omitempty"`
	CancelAt   time.Time `json:"cancel_at"`
}

// moveLiveStep is the in-memory progress of a move's running step. engine
// is the engine working; cancel and state belong to the running attempt
// (moveAttempt), which the watchdog cancels when the move left state or
// the step stalled; diagnostics are what the agents reported then.
type moveLiveStep struct {
	step        string
	tracker     *agent.ProgressTracker
	engine      string
	cancel      context.CancelCauseFunc
	state       string
	cancelled   bool
	diagnostics []string
}

type moveStallLimits struct{ warn, cancel time.Duration }

func (c *Control) moveStallLimits() moveStallLimits {
	limits := moveStallLimits{warn: moveStallWarnAfter, cancel: moveStallCancelAfter}
	if c.moveStallWarn > 0 {
		limits.warn = c.moveStallWarn
	}
	if c.moveStallCancel > 0 {
		limits.cancel = c.moveStallCancel
	}
	return limits
}

func (c *Control) liveMoveStep(id string) *moveLiveStep {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.moveLive[id]
}

// watchMoveAttempt connects the running attempt to the watchdog.
func (c *Control) watchMoveAttempt(id, state string, cancel context.CancelCauseFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if live := c.moveLive[id]; live != nil {
		live.cancel, live.state = cancel, state
	}
}

// setMoveLiveEngine names the engine the running step works with.
func (c *Control) setMoveLiveEngine(id, engine string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if live := c.moveLive[id]; live != nil {
		live.engine = engine
	}
}

func (c *Control) moveLiveEngine(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if live := c.moveLive[id]; live != nil {
		return live.engine
	}
	return ""
}

// moveLiveStepKey is the running step of a move, for log lines of shared
// helpers; "" (which logs nothing) when none runs.
func (c *Control) moveLiveStepKey(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if live := c.moveLive[id]; live != nil {
		return live.step
	}
	return ""
}

// moveTakeDiagnostics returns the diagnostics captured for the running
// attempt.
func (c *Control) moveTakeDiagnostics(id string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if live := c.moveLive[id]; live != nil {
		out := live.diagnostics
		live.diagnostics = nil
		return out
	}
	return nil
}

// moveWatchdog cancels an attempt whose move left its state (an abort) and
// warns about, then cancels, an attempt whose progress stands still.
func (c *Control) moveWatchdog(id string, live *moveLiveStep) {
	c.mu.Lock()
	cancel, state, cancelled := live.cancel, live.state, live.cancelled
	c.mu.Unlock()
	job, ok := c.moveJob(id)
	if cancel != nil && (!ok || job.State != state) {
		cancel(errMoveInterrupted)
		return
	}
	p := live.tracker.Snapshot()
	limits := c.moveStallLimits()
	now := time.Now().UTC()
	idle := now.Sub(p.UpdatedAt)
	if !p.Counted || p.Phase == "" || idle < limits.warn {
		if job.Stall != nil {
			_, _ = c.updateMove(id, func(job *MoveJob) error {
				if job.Stall == nil {
					return errMoveUnchanged
				}
				job.Stall = nil
				job.logf(live.step, "Progress resumed: %s.", p.Phase)
				return nil
			})
		}
		return
	}
	if job.Stall == nil || job.Stall.Phase != p.Phase {
		_, _ = c.updateMove(id, func(job *MoveJob) error {
			job.Stall = &MoveStall{Since: p.UpdatedAt, Phase: p.Phase, DoneBytes: p.DoneBytes, TotalBytes: p.TotalBytes, CancelAt: p.UpdatedAt.Add(limits.cancel)}
			job.logf(live.step, "Warning: no progress for %s while %s (%s); the attempt is cancelled and retried when nothing moves for %s.", moveDuration(idle.Round(time.Second)), p.Phase, moveProgressBytes(p), moveDuration(limits.cancel))
			return nil
		})
	}
	if idle < limits.cancel || cancel == nil || cancelled {
		return
	}
	c.mu.Lock()
	live.cancelled = true
	c.mu.Unlock()
	diagnostics := c.moveCollectDiagnostics(job)
	c.mu.Lock()
	live.diagnostics = diagnostics
	c.mu.Unlock()
	c.moveLog(id, live.step, "No progress for %s while %s; cancelling the attempt to retry it.", moveDuration(idle.Round(time.Second)), p.Phase)
	cancel(errMoveStalled)
}

func moveProgressBytes(p agent.Progress) string {
	if p.TotalBytes > 0 {
		return moveSize(p.DoneBytes) + " of " + moveSize(p.TotalBytes)
	}
	return moveSize(p.DoneBytes) + " done"
}

// moveCollectDiagnostics asks both agents what they do for the move.
func (c *Control) moveCollectDiagnostics(job MoveJob) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lines := []string{}
	for _, server := range []string{job.SourceServer, job.TargetServer} {
		var out agent.MoveDiagnostics
		err := c.moveCall(ctx, 25*time.Second, server, http.MethodGet, "/v1/move/apps/"+url.PathEscape(job.AppID)+"/diagnostics?move="+url.QueryEscape(job.ID), nil, &out)
		name := c.serverName(server)
		if err != nil {
			lines = append(lines, name+": diagnostics unavailable ("+moveAgentError(err)+")")
			continue
		}
		for i, line := range out.Lines {
			if i >= 15 {
				break
			}
			lines = append(lines, name+": "+moveBounded(line, 300))
		}
	}
	return lines
}

// errMoveUnchanged leaves a job unsaved from inside updateMove.
var errMoveUnchanged = errors.New("unchanged")

// beginMoveLive starts the live progress of a running step and its
// heartbeat. end stops both and returns the last progress, or nil when the
// step never reported a phase; calling it again only returns the progress.
func (c *Control) beginMoveLive(ctx context.Context, id, step string) (end func() *agent.Progress) {
	tracker := agent.NewProgressTracker("")
	live := &moveLiveStep{step: step, tracker: tracker}
	c.mu.Lock()
	if c.moveLive == nil {
		c.moveLive = map[string]*moveLiveStep{}
	}
	c.moveLive[id] = live
	c.mu.Unlock()
	interval := c.moveHeartbeat
	if interval <= 0 {
		interval = moveHeartbeatEvery
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.moveBeat(id, step, tracker, interval)
				c.moveWatchdog(id, live)
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	var once sync.Once
	return func() *agent.Progress {
		once.Do(func() {
			close(stop)
			<-done
			c.mu.Lock()
			if c.moveLive[id] == live {
				delete(c.moveLive, id)
			}
			c.mu.Unlock()
		})
		p := tracker.Snapshot()
		if p.Phase == "" {
			return nil
		}
		return &p
	}
}

// moveTracker returns the running step's tracker of a move; nil (which
// ignores reports) when none runs, for example in a direct call.
func (c *Control) moveTracker(id string) *agent.ProgressTracker {
	c.mu.Lock()
	defer c.mu.Unlock()
	if live := c.moveLive[id]; live != nil {
		return live.tracker
	}
	return nil
}

// moveStage sets the running step's phase.
func (c *Control) moveStage(id, phase string, total int64, read func() int64) {
	c.moveTracker(id).Stage(phase, total, read)
}

// moveBeat writes a heartbeat when the step has a phase and logged nothing
// for interval.
func (c *Control) moveBeat(id, step string, tracker *agent.ProgressTracker, interval time.Duration) {
	p := tracker.Snapshot()
	if p.Phase == "" {
		return
	}
	now := time.Now().UTC()
	_, _ = c.updateMove(id, func(job *MoveJob) error {
		s := job.step(step)
		if s == nil || s.Status != "running" {
			return errMoveUnchanged
		}
		for i := len(s.Log) - 1; i >= 0; i-- {
			if !s.Log[i].Heartbeat {
				if now.Sub(s.Log[i].Time) < interval {
					return errMoveUnchanged
				}
				break
			}
		}
		job.heartbeat(step, moveHeartbeatMessage(p, now))
		return nil
	})
}

// heartbeat logs message, replacing the step's previous heartbeat when it
// is the last line.
func (job *MoveJob) heartbeat(key, message string) {
	s := job.step(key)
	if s == nil {
		return
	}
	line := MoveLogLine{Time: time.Now().UTC(), Message: message, Heartbeat: true}
	if n := len(s.Log); n > 0 && s.Log[n-1].Heartbeat {
		s.Log[n-1] = line
		return
	}
	s.Log = append(s.Log, line)
	if len(s.Log) > moveMaxLogLines {
		s.Log = s.Log[len(s.Log)-moveMaxLogLines:]
	}
}

// moveHeartbeatMessage describes progress as a log line.
func moveHeartbeatMessage(p agent.Progress, now time.Time) string {
	text := "Still " + p.Phase
	switch {
	case p.TotalBytes > 0:
		text += fmt.Sprintf(": %s of %s", moveSize(p.DoneBytes), moveSize(p.TotalBytes))
	case p.DoneBytes > 0:
		text += fmt.Sprintf(": %s written", moveSize(p.DoneBytes))
	}
	if !p.StartedAt.IsZero() && now.After(p.StartedAt) {
		text += fmt.Sprintf(" (%s elapsed)", now.Sub(p.StartedAt).Round(time.Second))
	}
	return text + "."
}

// moveLiveView is a job as the browser sees it, with the running step's
// live progress.
func (c *Control) moveLiveView(job MoveJob) moveView {
	c.mu.Lock()
	live := c.moveLive[job.ID]
	running := c.moveRunning[job.ID]
	c.mu.Unlock()
	if live != nil {
		if s := job.step(live.step); s != nil && s.Status == "running" {
			if p := live.tracker.Snapshot(); p.Phase != "" {
				s.Progress = &p
			}
		}
	}
	view := withoutActiveResume(moveJobView(job), running)
	view.TargetServerName = c.serverName(job.TargetServer)
	return view
}

// withoutActiveResume drops "continue" while a runner is working on the
// move: resuming is only for a step whose runner stopped (for example after
// a restart), never for one that is in progress.
func withoutActiveResume(view moveView, running bool) moveView {
	if !running || view.State == moveStateFailed {
		return view
	}
	view.Actions = slices.DeleteFunc(slices.Clone(view.Actions), func(action string) bool { return action == "continue" })
	return view
}

// moveRemotePhase names an agent's phase for a move step's log: subject is
// what the agent works on (a database or a file set), server where.
func moveRemotePhase(remote, subject, server string) string {
	word, _, _ := strings.Cut(remote, " ")
	switch word {
	case "Waiting":
		return fmt.Sprintf("waiting for a free database slot on %s for %s", server, subject)
	case "Dumping", "Creating":
		return fmt.Sprintf("dumping %s on %s", subject, server)
	case "Finalizing":
		return fmt.Sprintf("finalizing the backup of %s on %s", subject, server)
	case "Authorizing", "Uploading":
		return fmt.Sprintf("uploading %s to off-site storage from %s", subject, server)
	case "Verifying":
		return fmt.Sprintf("verifying %s on %s", subject, server)
	case "Measuring":
		return fmt.Sprintf("measuring %s on %s", subject, server)
	case "Archiving":
		return fmt.Sprintf("archiving %s on %s", subject, server)
	case "Downloading", "Receiving":
		return fmt.Sprintf("receiving %s on %s", subject, server)
	case "Extracting":
		return fmt.Sprintf("extracting %s on %s", subject, server)
	case "Copying":
		return fmt.Sprintf("copying %s into its volume on %s", subject, server)
	case "Restoring":
		return fmt.Sprintf("restoring %s on %s", subject, server)
	}
	return fmt.Sprintf("working on %s on %s", subject, server)
}

// moveMirror copies an agent's progress into a move's tracker.
func (c *Control) moveMirror(id, subject, server string, p *agent.Progress) {
	if p == nil || p.Phase == "" {
		return
	}
	c.moveTracker(id).Mirror(moveRemotePhase(p.Phase, subject, server), *p)
}

// moveWatchFiles polls an agent's progress of one file set while a
// synchronous call to it runs, until stop is called. Agents without the
// progress endpoint answer 404 and are not asked again.
func (c *Control) moveWatchFiles(ctx context.Context, job MoveJob, server, set, subject string) (stop func()) {
	interval := c.movePollInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		path := "/v1/move/files/" + url.PathEscape(job.ID) + "/progress"
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
			var out agent.MoveFileProgress
			err := c.moveCall(ctx, 15*time.Second, server, http.MethodGet, path, nil, &out)
			var remote *agentHTTPError
			if errors.As(err, &remote) && remote.status == http.StatusNotFound {
				return
			}
			if err == nil {
				if p, ok := out.Sets[set]; ok && ctx.Err() == nil {
					c.moveMirror(job.ID, subject, server, &p)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}
