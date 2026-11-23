package control

// Moving a plugin's service (docs/plugins-proposal.md). The service is one
// more engine of a move: the source backs it up through its plugin's backup
// hook, the backup is relayed like a database dump, and the destination's
// plugin restores it. Plugins that export and import copy only what changed
// in the final sync; with others both phases copy the whole service.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

const moveEnginePlugin = "plugin"

// MovePluginService is one plugin service a move copies.
type MovePluginService struct {
	Service  string `json:"service"`
	Plugin   string `json:"plugin"`
	SourceID string `json:"source_id"`
	TargetID string `json:"target_id,omitempty"`
	Copied   string `json:"copied_sha256,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	// Exports says both plugins can export and import for a move, so the
	// final sync copies only what changed; otherwise both phases copy a
	// whole backup.
	Exports bool `json:"exports,omitempty"`
}

func movePluginsFrom(source agent.MoveSource, plan MovePlan) []MovePluginService {
	var out []MovePluginService
	for _, p := range source.Plugins {
		if p.Copyable {
			out = append(out, MovePluginService{Service: p.Service, Plugin: p.Plugin, SourceID: p.Resource, Exports: p.Exports && slices.Contains(plan.PluginImports, p.Service)})
		}
	}
	return out
}

// planMovePlugins adds the application's plugin services to the plan and
// refuses a destination that cannot restore them: every service needs a
// plugin there that provides it and implements backup and restore.
func (c *Control) planMovePlugins(r *http.Request, plan *MovePlan, source agent.MoveSource, target string) uint64 {
	if len(source.Plugins) == 0 {
		return 0
	}
	var needed uint64
	for _, p := range source.Plugins {
		entry := MovePlanDatabase{ID: p.Resource, Engine: p.Service, Mode: "plugin", Tables: -1, SizeBytes: p.SizeBytes, Action: "copy", Note: "copied through the " + p.Plugin + " plugin's backup and restore"}
		if !p.Copyable {
			entry.ID, entry.Action, entry.Note = p.Service, "manual", p.Reason
		}
		plan.Databases = append(plan.Databases, entry)
		// The backup arrives as one archive beside the restored data.
		needed += 2 * uint64(max(p.SizeBytes, 0))
	}
	if plan.TargetRuntime != moveRuntimeNative {
		plan.Blockers = append(plan.Blockers, "Plugin services are provided to native applications; move with the native runtime.")
		return needed
	}
	var installed agent.PluginsView
	if err := c.agentCall(r, target, http.MethodGet, "/v1/plugins", nil, &installed); err != nil {
		plan.Blockers = append(plan.Blockers, "The destination agent cannot run plugins, which this application's services need; update it first.")
		return needed
	}
	for _, p := range source.Plugins {
		provided := slices.IndexFunc(installed.Plugins, func(candidate agent.PluginRecord) bool { return candidate.Provides == p.Service })
		switch {
		case provided < 0:
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("The destination has no plugin that provides the %s service; install the %s plugin there first (the server's Plugins tab).", p.Service, p.Plugin))
		case !slices.Contains(installed.Plugins[provided].Hooks, "backup") || !slices.Contains(installed.Plugins[provided].Hooks, "restore"):
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("The %s plugin on the destination (version %s) cannot restore a backup; install a version that can.", installed.Plugins[provided].Name, installed.Plugins[provided].Version))
		case installed.Plugins[provided].Version != p.Version:
			plan.Notes = append(plan.Notes, fmt.Sprintf("The %s plugin is version %s here and %s on the destination; the destination's version restores the backup.", p.Plugin, p.Version, installed.Plugins[provided].Version))
		}
		if provided >= 0 && slices.Contains(installed.Plugins[provided].Hooks, "import") {
			plan.PluginImports = append(plan.PluginImports, p.Service)
		}
		if p.Copyable && !(p.Exports && slices.Contains(plan.PluginImports, p.Service)) {
			plan.Notes = append(plan.Notes, fmt.Sprintf("The %s service is copied whole twice, in the bulk copy and again in the final sync: its plugin cannot copy only what changed on both servers.", p.Service))
		}
	}
	return needed
}

type movePluginEngine struct {
	run   moveRun
	index int
}

func (e *movePluginEngine) service() MovePluginService { return e.run.job().Plugins[e.index] }

func (e *movePluginEngine) Info() moveEngineInfo {
	p := e.service()
	return moveEngineInfo{Kind: moveEnginePlugin, Object: p.SourceID, Label: p.Service + " service " + p.SourceID}
}

func (e *movePluginEngine) Measure(context.Context) (moveMeasurement, error) {
	p := e.service()
	for _, planned := range e.run.job().Plan.Databases {
		if planned.ID == p.SourceID {
			return moveMeasurement{Bytes: planned.SizeBytes, ObjectLabel: "services", Objects: 1, Measured: planned.SizeBytes > 0}, nil
		}
	}
	return moveMeasurement{ObjectLabel: "services", Objects: 1}, nil
}

func (e *movePluginEngine) BulkCopy(ctx context.Context, cp moveCheckpointRef) error {
	return e.run.c.moveCopyPlugin(ctx, e.run, e.index, cp)
}

// FinalSync copies the whole service again, with the source frozen.
func (e *movePluginEngine) FinalSync(ctx context.Context, cp moveCheckpointRef) error {
	return e.run.c.moveCopyPlugin(ctx, e.run, e.index, cp)
}

// Verify: the relay compares the archive's SHA-256 on both ends, and the
// plugin's restore or import hook answers for what it restored.
func (e *movePluginEngine) Verify(context.Context) error { return nil }

// Cleanup tells both servers that the move ended, once it finishes or is
// given up: the exports and whatever the plugins kept for the final sync
// (a base backup) are removed. Until then they stay, so a repeated final
// sync is incremental too.
func (e *movePluginEngine) Cleanup(ctx context.Context) error {
	job := e.run.job()
	p := job.Plugins[e.index]
	ending := job.State == moveStateFinishing || job.State == moveStateFinished || job.State == moveStateAborting || job.State == moveStateAborted
	if !p.Exports || !ending {
		return nil
	}
	for server, resource := range map[string]string{job.SourceServer: p.SourceID, job.TargetServer: p.TargetID} {
		if resource == "" {
			continue
		}
		if err := e.run.c.moveCall(ctx, 10*time.Minute, server, http.MethodDelete, "/v1/move/plugin-services/"+url.PathEscape(resource)+"/moves/"+url.PathEscape(job.ID), nil, nil); err != nil {
			e.run.log("The %s service's move files on %s could not be removed: %s", p.Service, server, moveAgentError(err))
		}
	}
	return nil
}

// moveCopyPlugin copies the service in one phase. With plugins that export
// and import, the bulk phase copies everything and the final phase what
// changed since; otherwise each phase takes a backup through the normal
// pipeline. The archive is relayed, and the destination's plugin restores it.
func (c *Control) moveCopyPlugin(ctx context.Context, run moveRun, i int, cp moveCheckpointRef) error {
	job := run.job()
	p := job.Plugins[i]
	id, step := run.id, run.step
	if p.TargetID == "" {
		return moveOperatorError("destination_unknown", fmt.Sprintf("the destination's %s service is unknown", p.Service), "Retry the move from Prepare destination, or abort it.")
	}
	saved := cp.Get()
	var done movePostgresCheckpoint
	cp.Meta(&done)
	if done.Restored != "" && done.Restored == saved.SHA256 {
		c.moveLog(id, step, "The copy of the %s service (%s) was restored on %s before this attempt; not restoring it again.", p.Service, saved.Artifact, job.TargetServer)
		return nil
	}
	restored := func(digest string, size int64) error {
		_, err := c.updateMove(id, func(job *MoveJob) error {
			job.Plugins[i].Copied, job.Plugins[i].Bytes = digest, size
			job.logf(step, "Restored the %s service on %s; the SHA-256 matched on both ends.", p.Service, job.TargetServer)
			return job.setCheckpointMeta(cp.key, movePostgresCheckpoint{Restored: digest})
		})
		return err
	}
	if p.Exports {
		return c.moveCopyPluginExport(ctx, run, p, cp, restored)
	}
	record, reused := c.moveReusableBackup(ctx, job.SourceServer, p.SourceID, saved)
	if reused {
		c.moveLog(id, step, "Reusing backup %s of the %s service from the previous attempt (%s).", record.ID, p.Service, moveSize(record.Size))
	} else {
		c.moveLog(id, step, "Backing up the %s service on %s through the %s plugin.", p.Service, job.SourceServer, p.Plugin)
		c.moveStage(id, fmt.Sprintf("backing up the %s service on %s", p.Service, job.SourceServer), 0, nil)
		var err error
		record, err = c.moveFreshBackup(ctx, job.SourceServer, p.SourceID, func(progress *agent.Progress) { c.moveMirror(id, p.SourceID, job.SourceServer, progress) })
		if err != nil {
			return moveOn(job.SourceServer, fmt.Errorf("the backup of the %s service failed: %w", p.Service, err))
		}
		if err := cp.SaveMeta(record.ID, record.SHA256, record.Size, nil); err != nil {
			return err
		}
		c.moveLog(id, step, "Backup %s is ready (%s, SHA-256 %s…); streaming it to %s.", record.ID, moveSize(record.Size), record.SHA256[:12], job.TargetServer)
	}
	if err := c.moveWaitTargetIdle(ctx, job, p.TargetID); err != nil {
		return err
	}
	if record.Size <= 0 || record.Size > moveMaxDumpBytes || !moveDigestPattern.MatchString(record.SHA256) {
		return fmt.Errorf("the backup of the %s service has no usable size or digest", p.Service)
	}
	if err := c.moveRelay(ctx, job, moveRelayStream{
		download: "/v1/move/databases/" + url.PathEscape(p.SourceID) + "/backups/" + url.PathEscape(record.ID),
		upload:   "/v1/move/plugin-services/" + url.PathEscape(p.TargetID) + "/restore",
		size:     record.Size, digest: record.SHA256, mismatch: errMoveChecksum, what: "dump", resume: moveUploadID(job, cp.key, record.SHA256),
		phase: fmt.Sprintf("streaming the backup of the %s service to %s", p.Service, job.TargetServer),
		after: fmt.Sprintf("restoring the %s service on %s through its plugin", p.Service, job.TargetServer),
	}, nil); err != nil {
		return fmt.Errorf("copying the %s service failed: %w", p.Service, err)
	}
	return restored(record.SHA256, record.Size)
}

// movePluginExport has the source's plugin export the service for this
// phase (or reuses the export an earlier attempt made) and waits for it.
func (c *Control) movePluginExport(ctx context.Context, run moveRun, p MovePluginService, cp moveCheckpointRef, full bool) (agent.PluginExport, error) {
	job := run.job()
	path := "/v1/move/plugin-services/" + url.PathEscape(p.SourceID) + "/exports/" + url.PathEscape(job.ID) + "/" + run.phase
	var state agent.PluginExport
	saved := cp.Get()
	if err := c.moveCall(ctx, time.Minute, job.SourceServer, http.MethodGet, path, nil, &state); err == nil && state.State == "ready" && state.SHA256 == saved.SHA256 && !full {
		c.moveLog(run.id, run.step, "Reusing the export of the %s service from the previous attempt (%s).", p.Service, moveSize(state.Size))
		return state, nil
	}
	if state.State != "running" {
		start := path
		if full {
			start += "?full=1"
		}
		if err := c.moveCall(ctx, time.Minute, job.SourceServer, http.MethodPost, start, nil, nil); err != nil {
			return state, moveOn(job.SourceServer, fmt.Errorf("the export of the %s service could not start: %s", p.Service, moveAgentError(err)))
		}
	}
	c.moveStage(run.id, fmt.Sprintf("exporting the %s service on %s", p.Service, job.SourceServer), 0, nil)
	err := c.movePoll(ctx, 6*time.Hour, func() (bool, error) {
		// A fresh value each time: fields the agent omits must not keep
		// what an earlier answer said.
		var current agent.PluginExport
		if err := c.moveCall(ctx, time.Minute, job.SourceServer, http.MethodGet, path, nil, &current); err != nil {
			return false, nil
		}
		state = current
		switch state.State {
		case "ready":
			return true, nil
		case "failed":
			return false, moveOn(job.SourceServer, fmt.Errorf("the export of the %s service failed: %s", p.Service, state.Error))
		case "missing":
			return false, moveOn(job.SourceServer, fmt.Errorf("the export of the %s service was lost; was the agent restarted?", p.Service))
		}
		return false, nil
	})
	if err != nil {
		return state, err
	}
	if state.Size <= 0 || state.Size > moveMaxDumpBytes || !moveDigestPattern.MatchString(state.SHA256) {
		return state, fmt.Errorf("the export of the %s service has no usable size or digest", p.Service)
	}
	return state, cp.SaveMeta("export-"+run.phase, state.SHA256, state.Size, nil)
}

// moveCopyPluginExport copies one phase through the plugins' export and
// import. An incremental final copy that fails is repeated in full: the
// increment is the optimisation, not the move.
func (c *Control) moveCopyPluginExport(ctx context.Context, run moveRun, p MovePluginService, cp moveCheckpointRef, restored func(string, int64) error) error {
	job := run.job()
	send := func(state agent.PluginExport, attempt string) error {
		if err := c.moveWaitTargetIdle(ctx, job, p.TargetID); err != nil {
			return err
		}
		upload := "/v1/move/plugin-services/" + url.PathEscape(p.TargetID) + "/restore?move=" + url.QueryEscape(job.ID) + "&phase=" + run.phase
		what := "export"
		if state.Incremental {
			upload += "&incremental=1"
			what = "changes since the bulk copy"
		}
		c.moveLog(run.id, run.step, "The %s of the %s service is ready (%s, SHA-256 %s…); streaming it to %s.", what, p.Service, moveSize(state.Size), state.SHA256[:12], job.TargetServer)
		return c.moveRelay(ctx, job, moveRelayStream{
			download: "/v1/move/plugin-services/" + url.PathEscape(p.SourceID) + "/exports/" + url.PathEscape(job.ID) + "/" + run.phase + "/archive",
			upload:   upload,
			size:     state.Size, digest: state.SHA256, mismatch: errMoveChecksum, what: "dump", resume: moveUploadID(job, cp.key+attempt, state.SHA256),
			phase: fmt.Sprintf("streaming the %s of the %s service to %s", what, p.Service, job.TargetServer),
			after: fmt.Sprintf("importing the %s service on %s through its plugin", p.Service, job.TargetServer),
		}, nil)
	}
	state, err := c.movePluginExport(ctx, run, p, cp, false)
	if err != nil {
		return err
	}
	err = send(state, "")
	if err != nil && state.Incremental && ctx.Err() == nil {
		c.moveLog(run.id, run.step, "Copying only the changes of the %s service failed (%v); copying the whole service again.", p.Service, err)
		if state, err = c.movePluginExport(ctx, run, p, cp, true); err != nil {
			return err
		}
		err = send(state, "/full")
	}
	if err != nil {
		return fmt.Errorf("copying the %s service failed: %w", p.Service, err)
	}
	return restored(state.SHA256, state.Size)
}

// movePluginsReady reports whether the destination provisioned every plugin
// service the application asked for.
func movePluginsReady(asked []agent.NativePluginService, provisioned []agent.NativeHostPluginView) bool {
	for _, service := range asked {
		if !slices.ContainsFunc(provisioned, func(v agent.NativeHostPluginView) bool { return v.Service == service.Service && v.Ready }) {
			return false
		}
	}
	return true
}
