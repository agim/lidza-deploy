package control

// Moving a plugin's service (docs/plugins-proposal.md). The service is one
// more engine of a move: the source backs it up through its plugin's backup
// hook, the backup is relayed like a database dump, and the destination's
// plugin restores it. Both phases copy the whole service; a plugin cannot
// copy incrementally yet.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"

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
}

func movePluginsFrom(source agent.MoveSource) []MovePluginService {
	var out []MovePluginService
	for _, p := range source.Plugins {
		if p.Copyable {
			out = append(out, MovePluginService{Service: p.Service, Plugin: p.Plugin, SourceID: p.Resource})
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
// plugin's restore hook answers for what it restored.
func (e *movePluginEngine) Verify(context.Context) error  { return nil }
func (e *movePluginEngine) Cleanup(context.Context) error { return nil }

// moveCopyPlugin takes a backup of the service on the source (or reuses the
// one an earlier attempt of this phase took), relays it and has the
// destination's plugin restore it.
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
		c.moveLog(id, step, "Backup %s of the %s service was restored on %s before this attempt; not restoring it again.", saved.Artifact, p.Service, job.TargetServer)
		return nil
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
	_, err := c.updateMove(id, func(job *MoveJob) error {
		job.Plugins[i].Copied, job.Plugins[i].Bytes = record.SHA256, record.Size
		job.logf(step, "Restored the %s service on %s; the SHA-256 matched on both ends.", p.Service, job.TargetServer)
		return job.setCheckpointMeta(cp.key, movePostgresCheckpoint{Restored: record.SHA256})
	})
	return err
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
