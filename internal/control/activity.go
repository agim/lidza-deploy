package control

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/jobs"
)

// The activity feed (GET /api/control/activity) is what the console's
// header drawer lists: everything the panel knows is running or waiting,
// and what finished recently. It is assembled from what the panel already
// holds (moves, the update rollout, the job queue) plus the fleet's
// deployment lists, which are read at most once per activityCacheFor.
// Host maintenance, backups and restores are not listed: the panel does not
// track them and listing them would cost an agent call per server and poll.

const (
	activityCacheFor     = 15 * time.Second
	activityFinishedFor  = 2 * time.Hour  // succeeded items stay this long
	activityFailedFor    = 24 * time.Hour // failed items stay this long
	activityMaxFinished  = 30
	activityMaxLogLines  = 3
	activityMaxDetailLen = 300
)

// ActivityItem is one operation in the drawer.
type ActivityItem struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"` // move, deployment, update, job
	Title      string            `json:"title"`
	ServerID   string            `json:"server_id,omitempty"`
	ServerName string            `json:"server_name,omitempty"`
	AppID      string            `json:"app_id,omitempty"`
	State      string            `json:"state"` // running, waiting, succeeded, failed
	Message    string            `json:"message,omitempty"`
	WaitingOn  string            `json:"waiting_on,omitempty"`
	Started    *time.Time        `json:"started,omitempty"`
	Updated    time.Time         `json:"updated"`
	Progress   *ActivityProgress `json:"progress"`
	Link       string            `json:"link,omitempty"`
	Details    []string          `json:"details"`
}

// ActivityProgress is how far an item got; Unit is bytes, steps, servers
// or objects.
type ActivityProgress struct {
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	Unit  string `json:"unit"`
}

type activityDeployment struct {
	agent.Deployment
	ServerID string `json:"server_id"`
}

// activityFleet is the cached deployment read of the fleet.
type activityFleet struct {
	at          time.Time
	deployments []activityDeployment
	unavailable []string
}

func (c *Control) activity(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	fleet := c.activityFleetCached(r, now)
	var queued []jobs.Job
	if q, ok := lidza.Optional[*jobs.Queue](r.Context()); ok && q != nil {
		queued, _ = q.Recent(r.Context(), 200)
	}
	items := c.moveActivity(now)
	items = append(items, c.deploymentActivity(fleet.deployments, now)...)
	items = append(items, c.updateActivity(c.updateSettingsSnapshot(), now)...)
	items = append(items, c.jobActivity(queued, now)...)
	agent.JSON(w, 200, map[string]any{"items": sortActivity(items), "generated": now, "unavailable_servers": fleet.unavailable})
}

func (c *Control) activityFleetCached(r *http.Request, now time.Time) activityFleet {
	c.activityMu.Lock()
	defer c.activityMu.Unlock()
	if c.activityCache != nil && now.Sub(c.activityCache.at) < activityCacheFor {
		return *c.activityCache
	}
	fleet := activityFleet{at: now, deployments: []activityDeployment{}, unavailable: []string{}}
	for _, result := range c.readFleet(r, "/v1/deployments?logs=0") {
		var rows []agent.Deployment
		if result.Err != nil || json.Unmarshal(result.Body, &rows) != nil {
			fleet.unavailable = append(fleet.unavailable, result.Server.ID)
			continue
		}
		for _, d := range rows {
			fleet.deployments = append(fleet.deployments, activityDeployment{Deployment: d, ServerID: result.Server.ID})
		}
	}
	c.activityCache = &fleet
	return fleet
}

// sortActivity orders running, then waiting, then finished items, newest
// first within each group, and bounds the finished ones.
func sortActivity(items []ActivityItem) []ActivityItem {
	rank := map[string]int{"running": 0, "waiting": 1, "failed": 2, "succeeded": 2}
	slices.SortStableFunc(items, func(a, b ActivityItem) int {
		if d := cmp.Compare(rank[a.State], rank[b.State]); d != 0 {
			return d
		}
		return b.Updated.Compare(a.Updated)
	})
	out, finished := []ActivityItem{}, 0
	for _, item := range items {
		if item.Details == nil {
			item.Details = []string{}
		}
		if rank[item.State] == 2 {
			if finished++; finished > activityMaxFinished {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

// activityRecent reports whether a finished item is still shown.
func activityRecent(state string, at, now time.Time) bool {
	if state == "failed" {
		return now.Sub(at) < activityFailedFor
	}
	return now.Sub(at) < activityFinishedFor
}

func activityTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func activityClip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > activityMaxDetailLen {
		return s[:activityMaxDetailLen] + "…"
	}
	return s
}

func activityClock(t time.Time) string { return t.UTC().Format("15:04 UTC") }

// ---- moves -------------------------------------------------------------------

func (c *Control) moveActivity(now time.Time) []ActivityItem {
	c.mu.Lock()
	moves := make([]MoveJob, 0, len(c.data.Moves))
	for _, job := range c.data.Moves {
		if !terminalMoveState(job.State) || activityRecent("succeeded", job.Updated, now) {
			moves = append(moves, cloneMoveJob(job))
		}
	}
	c.mu.Unlock()
	out := []ActivityItem{}
	for _, job := range moves {
		out = append(out, c.moveActivityItem(job, now))
	}
	return out
}

func (c *Control) moveActivityItem(job MoveJob, now time.Time) ActivityItem {
	target, source := c.serverName(job.TargetServer), c.serverName(job.SourceServer)
	// The running step's live progress, as the move panel shows it.
	if live := c.liveMoveStep(job.ID); live != nil {
		if s := job.step(live.step); s != nil && s.Status == "running" {
			if p := live.tracker.Snapshot(); p.Phase != "" {
				s.Progress = &p
			}
		}
	}
	item := ActivityItem{ID: "move:" + job.ID, Kind: "move", Title: fmt.Sprintf("Moving %s to %s", job.AppID, target), ServerID: job.TargetServer, ServerName: target, AppID: job.AppID,
		Started: activityTime(job.Created), Updated: job.Updated, Link: "/console/applications?move=" + url.QueryEscape(job.AppID)}
	step := moveCurrentStep(job)
	done, total := 0, 0
	for _, s := range job.Steps {
		if s.Status == "skipped" {
			continue
		}
		total++
		if s.Status == "done" {
			done++
		}
	}
	if total > 0 {
		item.Progress = &ActivityProgress{Done: int64(done), Total: int64(total), Unit: "steps"}
	}
	item.Details = append(item.Details, fmt.Sprintf("From %s to %s", source, target))
	if step != nil {
		title := moveStepTitle(step.Key, step.Title)
		item.Message = title
		item.Details = append(item.Details, fmt.Sprintf("Step %d of %d: %s", min(done+1, total), total, title))
		if p := step.Progress; p != nil && step.Status == "running" {
			switch {
			case p.TotalBytes > 0:
				item.Progress = &ActivityProgress{Done: p.DoneBytes, Total: p.TotalBytes, Unit: "bytes"}
				item.Details = append(item.Details, fmt.Sprintf("%s: %s of %s", p.Phase, moveSize(p.DoneBytes), moveSize(p.TotalBytes)))
			case p.TotalObjects > 0:
				item.Progress = &ActivityProgress{Done: p.DoneObjects, Total: p.TotalObjects, Unit: "objects"}
				item.Details = append(item.Details, fmt.Sprintf("%s: %d of %d", p.Phase, p.DoneObjects, p.TotalObjects))
			default:
				item.Details = append(item.Details, p.Phase)
			}
			item.Message = title + " · " + p.Phase
		}
	}
	switch job.State {
	case moveStateFinished:
		item.Title, item.State, item.Message = fmt.Sprintf("Moved %s to %s", job.AppID, target), "succeeded", "Move finished"
	case moveStateAborted:
		item.Title, item.State, item.Message = fmt.Sprintf("Move of %s to %s aborted", job.AppID, target), "succeeded", "Aborted; the application stays on "+source
	case moveStateFailed:
		item.State = "failed"
		item.Message = cmp.Or(job.Error, "The move stopped")
		item.WaitingOn = "Waiting for you to retry or abort the move"
	case moveStatePlanned:
		item.State, item.WaitingOn = "waiting", "Waiting for you to start the move"
	case moveStateCutover:
		item.State = "waiting"
		item.WaitingOn = fmt.Sprintf("Waiting for DNS of %s to point to %s", cmp.Or(strings.Join(job.Hostnames, ", "), job.AppID), target)
		if job.AutoCutover {
			item.WaitingOn += "; the final sync starts on its own once HTTPS works there"
		}
	case moveStateReady:
		item.State, item.WaitingOn = "waiting", "Ready to finish; waiting for you to run the final sync"
	default:
		item.State = "running"
	}
	if f := job.Failure; f != nil && !terminalMoveState(job.State) {
		if f.NextRetry != nil && job.State != moveStateFailed {
			item.State = "waiting"
			item.WaitingOn = fmt.Sprintf("Retrying %s at %s (attempt %d of %d)", moveStepTitle(f.Step, f.Step), activityClock(*f.NextRetry), f.Attempt+1, f.MaxAttempts)
		}
		item.Details = append(item.Details, activityClip(f.Message))
		if f.Remediation != "" {
			item.Details = append(item.Details, activityClip(f.Remediation))
		}
	}
	if s := job.Stall; s != nil && !terminalMoveState(job.State) {
		item.Details = append(item.Details, fmt.Sprintf("No progress since %s; the attempt is cancelled at %s", activityClock(s.Since), activityClock(s.CancelAt)))
	}
	if step != nil {
		lines := []string{}
		for i := len(step.Log) - 1; i >= 0 && len(lines) < activityMaxLogLines; i-- {
			lines = append(lines, activityClip(step.Log[i].Message))
		}
		slices.Reverse(lines)
		item.Details = append(item.Details, lines...)
	}
	return item
}

// ---- deployments -------------------------------------------------------------

func deploymentVerb(kind string) (running, done string) {
	switch kind {
	case "reload":
		return "Reloading", "Reloaded"
	case "rollback":
		return "Rolling back", "Rolled back"
	case "start":
		return "Starting", "Started"
	case "stop":
		return "Stopping", "Stopped"
	}
	return "Deploying", "Deployed"
}

func (c *Control) deploymentActivity(rows []activityDeployment, now time.Time) []ActivityItem {
	out := []ActivityItem{}
	for _, d := range rows {
		verb, past := deploymentVerb(d.Kind)
		server := c.serverName(d.ServerID)
		item := ActivityItem{ID: "deployment:" + d.ServerID + ":" + d.ID, Kind: "deployment", ServerID: d.ServerID, ServerName: server, AppID: d.AppID,
			Started: activityTime(d.Created), Link: "/console/deployments/" + url.PathEscape(d.ID)}
		if d.Started != nil {
			item.Started = activityTime(*d.Started)
		}
		item.Updated = d.Created
		for _, t := range []*time.Time{d.Started, d.Updated, d.Finished} {
			if t != nil && t.After(item.Updated) {
				item.Updated = *t
			}
		}
		phase := ""
		if d.Progress != nil && d.Progress.Phase != "" {
			phase = strings.ToUpper(d.Progress.Phase[:1]) + d.Progress.Phase[1:]
		}
		switch d.Status {
		case "queued":
			item.Title, item.State = fmt.Sprintf("%s %s", verb, d.AppID), "waiting"
			item.Message = "Queued"
			item.WaitingOn = cmp.Or(phase, "Queued on "+server+"; starts when the server is free")
		case "building":
			item.Title, item.State = fmt.Sprintf("%s %s", verb, d.AppID), "running"
			item.Message = cmp.Or(phase, "Building")
		case "failed":
			at := item.Updated
			if !activityRecent("failed", at, now) {
				continue
			}
			item.Title, item.State = fmt.Sprintf("%s %s failed", verb, d.AppID), "failed"
			item.Message = cmp.Or(activityClip(d.Error), "The deployment failed")
		default:
			if d.Finished == nil || !activityRecent("succeeded", *d.Finished, now) {
				continue
			}
			item.Title, item.State, item.Message = fmt.Sprintf("%s %s", past, d.AppID), "succeeded", "Finished"
			if d.Duration > 0 {
				item.Message = fmt.Sprintf("Finished in %s", (time.Duration(d.Duration) * time.Second).Round(time.Second))
			}
		}
		if phase != "" && d.Progress.TotalBytes > 0 {
			item.Progress = &ActivityProgress{Done: d.Progress.DoneBytes, Total: d.Progress.TotalBytes, Unit: "bytes"}
		}
		if d.Commit != "" {
			commit := d.Commit
			if len(commit) > 7 {
				commit = commit[:7]
			}
			item.Details = append(item.Details, strings.TrimSpace("Commit "+commit+" "+activityClip(firstLine(d.CommitMessage))))
		}
		if d.Branch != "" {
			item.Details = append(item.Details, "Branch "+d.Branch)
		}
		if d.Error != "" && d.Status != "failed" {
			item.Details = append(item.Details, activityClip(d.Error))
		}
		out = append(out, item)
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// ---- updates -----------------------------------------------------------------

func (c *Control) updateActivity(v UpdateSettings, now time.Time) []ActivityItem {
	out := []ActivityItem{}
	if v.Version == "" {
		return out
	}
	for i, t := range v.Targets {
		at := cmp.Or(t.Updated, t.Started)
		item := ActivityItem{ID: "update:" + v.Version + ":" + t.ServerID, Kind: "update", Title: fmt.Sprintf("Updating %s to %s", cmp.Or(t.Name, t.ServerID), v.Version),
			ServerID: t.ServerID, ServerName: cmp.Or(t.Name, t.ServerID), Started: activityTime(t.Started), Updated: at, Message: t.Message, Link: "/console/updates",
			Details: []string{fmt.Sprintf("Server %d of %d in the rollout of %s", i+1, len(v.Targets), v.Version)}}
		if t.Local {
			item.Details = append(item.Details, "This is the control panel's host; it updates last and the panel restarts")
		}
		switch t.State {
		case "updating":
			item.State = "running"
			item.Message = cmp.Or(t.Message, "Installing the release")
		case "waiting":
			item.State, item.WaitingOn = "waiting", cmp.Or(t.Message, "Waiting for its turn in the rollout")
		case "failed":
			if at.IsZero() || !activityRecent("failed", at, now) {
				continue
			}
			item.State, item.Title = "failed", fmt.Sprintf("Updating %s to %s failed", item.ServerName, v.Version)
		case "succeeded":
			if at.IsZero() || !activityRecent("succeeded", at, now) {
				continue
			}
			item.State, item.Title = "succeeded", fmt.Sprintf("Updated %s to %s", item.ServerName, v.Version)
		default:
			continue
		}
		done := int64(0)
		for _, o := range v.Targets {
			if o.State == "succeeded" {
				done++
			}
		}
		item.Progress = &ActivityProgress{Done: done, Total: int64(len(v.Targets)), Unit: "servers"}
		out = append(out, item)
	}
	return out
}

// ---- background jobs ---------------------------------------------------------

// activityJobKinds names the queue's job kinds for the drawer. The minute
// ticks run all the time; only their retries and failures are listed.
var activityJobKinds = map[string]string{
	"deploy.push":       "Auto-deploy",
	"deploy.preview":    "Preview deployment",
	"deploy.task":       "Scheduled task",
	"errors.sync":       "Error collector sync",
	"ops.tick":          "Operations check",
	"move.tick":         "Move check",
	"updates.tick":      "Update check",
	"backups.retention": "Backup retention",
	"native.inventory":  "Native inventory sync",
}

func (c *Control) jobActivity(rows []jobs.Job, now time.Time) []ActivityItem {
	out := []ActivityItem{}
	for _, j := range rows {
		user := j.Kind == "deploy.push" || j.Kind == "deploy.preview" || j.Kind == "deploy.task"
		item := ActivityItem{ID: "job:" + j.ID, Kind: "job", Title: cmp.Or(activityJobKinds[j.Kind], j.Kind), Updated: j.RunAt, Link: "/console/deployments"}
		var payload struct {
			AppID    string `json:"app_id"`
			ServerID string `json:"server_id"`
			TaskID   string `json:"task_id"`
			Number   int    `json:"number"`
		}
		_ = json.Unmarshal(j.Payload, &payload)
		if payload.AppID != "" {
			item.AppID = payload.AppID
			item.Title += " of " + payload.AppID
			item.Link = "/console/applications/" + url.PathEscape(payload.AppID) + "/settings"
			c.mu.Lock()
			item.ServerID = c.data.Apps[payload.AppID].ServerID
			c.mu.Unlock()
		}
		if payload.ServerID != "" {
			item.ServerID = payload.ServerID
		}
		if item.ServerID != "" {
			item.ServerName = c.serverName(item.ServerID)
		}
		switch {
		case j.Kind == "deploy.preview" && payload.Number > 0:
			item.Title += fmt.Sprintf(" (pull request #%d)", payload.Number)
		case j.Kind == "deploy.task" && payload.TaskID != "":
			item.Title += " (" + payload.TaskID + ")"
		case j.Kind == "errors.sync":
			item.Link = "/console/errors"
		case j.Kind == "updates.tick":
			item.Link = "/console/updates"
		case j.Kind == "backups.retention":
			item.Link = "/console/remote-backups"
		case j.Kind == "native.inventory", j.Kind == "ops.tick", j.Kind == "move.tick":
			item.Link = "/console/servers"
		}
		lastError := ""
		if j.LastError != nil {
			lastError = activityClip(*j.LastError)
		}
		item.Details = append(item.Details, fmt.Sprintf("Attempt %d of %d", max(j.Attempts, 1), j.MaxAttempts))
		switch {
		case j.State == "running" && user:
			item.State, item.Message = "running", "Running"
		case j.State == "pending" && j.Attempts > 0:
			item.State = "waiting"
			item.Message = cmp.Or(lastError, "Failed; will retry")
			item.WaitingOn = fmt.Sprintf("Retries at %s (attempt %d of %d)", activityClock(j.RunAt), j.Attempts+1, j.MaxAttempts)
		case j.State == "pending" && user && !j.RunAt.After(now):
			item.State, item.Message, item.WaitingOn = "waiting", "Queued", "Waiting for a free worker"
		case j.State == "failed" && activityRecent("failed", j.RunAt, now):
			item.State = "failed"
			item.Message = cmp.Or(lastError, "Failed after its last attempt")
		default:
			continue
		}
		out = append(out, item)
	}
	return out
}
