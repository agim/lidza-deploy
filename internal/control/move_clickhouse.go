package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Moving a managed ClickHouse database. The source agent exports a verified
// archive of the application's database (a ClickHouse BACKUP when the
// destination runs the same or a newer release series, otherwise one Native
// file per table). The BACKUP is written to a directory and streamed as one
// tar, Zstandard-compressed when both agents have the zstd tool; agents
// that predate directory backups exchange a .zip BACKUP instead, which
// restores far slower for many parts. The control panel streams it to the
// destination with its
// length and SHA-256 checked on the way, like PostgreSQL dumps; the
// destination verifies the digest again, restores the archive under its own
// database name and checks the row counts, which the control panel checks a
// second time against the source's export. The final sync repeats the copy
// with the source in maintenance. The application's CLICKHOUSE_* variables
// always come from the destination's own service.

var errMoveClickHouseSum = errors.New("the ClickHouse archive does not match its recorded SHA-256; nothing was restored")

// moveClickHouseFrom is the job record for a source's copyable database.
func moveClickHouseFrom(source agent.MoveSource) *MoveClickHouse {
	ch := source.ClickHouse
	if ch == nil || !ch.Copyable {
		return nil
	}
	return &MoveClickHouse{SourceMode: ch.Mode, SourceDatabase: ch.Database, SourceVersion: ch.Version}
}

// moveClickHouseSeries is a version's release series (26.8.15.10 → 26.8).
func moveClickHouseSeries(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// planMoveClickHouse lists the ClickHouse database in the plan and returns
// the destination disk space its copy needs: the received archive, the
// staged backup and the restored data.
func planMoveClickHouse(plan *MovePlan, source agent.MoveSource, host agent.MoveHost) uint64 {
	ch := source.ClickHouse
	if ch == nil {
		return 0
	}
	entry := MovePlanDatabase{ID: ch.Database, Engine: "clickhouse", Mode: ch.Mode, Tables: ch.Tables, SizeBytes: ch.SizeBytes, Note: ch.Reason}
	switch {
	case !ch.Copyable:
		entry.Action = "manual"
	case ch.Tables == 0:
		entry.Action, entry.Note = "skip_empty", "no tables; nothing to copy"
	default:
		entry.Action = "copy"
		entry.Note = fmt.Sprintf("ClickHouse %s, %d rows", ch.Version, ch.Rows)
	}
	plan.Databases = append(plan.Databases, entry)
	if !host.ClickHouse {
		plan.Blockers = append(plan.Blockers, "The destination agent cannot receive ClickHouse data; upgrade its agent.")
	} else if plan.TargetRuntime == moveRuntimeNative && !host.HostData {
		plan.Blockers = append(plan.Blockers, "The destination agent cannot provision ClickHouse.")
	}
	if ch.Copyable {
		plan.Notes = append(plan.Notes, fmt.Sprintf("ClickHouse %s is copied as a ClickHouse BACKUP when the destination runs release %s or newer, otherwise table by table; the destination restores into place only after the copy verified, and row counts are verified on both ends.", ch.Database, cmpOrUnknown(moveClickHouseSeries(ch.Version))))
	}
	return 3 * uint64(max(ch.SizeBytes, 0))
}

func cmpOrUnknown(value string) string {
	if value == "" {
		return "of the source"
	}
	return value
}

// movePrepareClickHouse has the destination provision (container) or report
// (native) the application's ClickHouse database.
func (c *Control) movePrepareClickHouse(ctx context.Context, job MoveJob) error {
	var target agent.MoveClickHouseTarget
	path := "/v1/move/apps/" + url.PathEscape(job.AppID) + "/clickhouse/prepare"
	if err := c.moveCall(ctx, 6*time.Minute, job.TargetServer, http.MethodPost, path, nil, &target); err != nil {
		return fmt.Errorf("the destination could not prepare ClickHouse: %s", moveAgentError(err))
	}
	if !target.Ready || target.Database == "" || target.Version == "" {
		return errors.New("the destination's ClickHouse is not ready")
	}
	_, err := c.updateMove(job.ID, func(job *MoveJob) error {
		job.ClickHouse.TargetDatabase, job.ClickHouse.TargetVersion = target.Database, target.Version
		job.logf(moveStepPrepare, "ClickHouse %s database %s is ready on the destination.", target.Version, target.Database)
		return nil
	})
	return err
}

// moveClickHouseCheckpoint is the ClickHouse engine's reuse data: the export
// a retry streams again, and, once the destination restored and verified
// it, its digest as Restored, so an attempt resumed after a restart of the
// control panel does not restore it a second time. The export is embedded,
// so checkpoints written before Restored existed still decode.
type moveClickHouseCheckpoint struct {
	agent.MoveClickHouseExport
	Restored string `json:"restored,omitempty"`
}

// moveCopyClickHouse copies the database once; the final sync calls it again
// with the source frozen. An export a previous attempt of the phase made is
// streamed again (resuming its upload) while the source still has it; one
// the destination already restored and verified is not restored again
// while the destination's row counts still match it.
func (c *Control) moveCopyClickHouse(ctx context.Context, run moveRun, cp moveCheckpointRef) error {
	id, step := run.id, run.step
	job := run.job()
	ch := job.ClickHouse
	if ch.TargetDatabase == "" || ch.TargetVersion == "" {
		return moveOperatorError("destination_unknown", "the destination ClickHouse database is unknown", "Retry the move from Prepare destination, or abort it.")
	}
	app := url.PathEscape(job.AppID)
	format, compression := c.moveClickHouseChoice(ctx, job)
	var export agent.MoveClickHouseExport
	reused := false
	var saved moveClickHouseCheckpoint
	if point := cp.Get(); point.Artifact != "" && json.Unmarshal(point.Meta, &saved) == nil && saved.ID == point.Artifact {
		export = saved.MoveClickHouseExport
		reused = true
		if saved.Restored != "" && saved.Restored == export.SHA256 {
			holds, err := c.moveClickHouseRestoreHolds(ctx, run, export)
			if err != nil {
				return err
			}
			if holds {
				c.moveCleanupClickHouse(ctx, job)
				run.objects(run.c.moveLiveEngine(id), int64(len(export.Tables)), int64(len(export.Tables)))
				return nil
			}
		}
		c.moveLog(id, step, "Reusing ClickHouse export %s… from the previous attempt (%s).", export.SHA256[:12], moveSize(export.Size))
	}
	for {
		if !reused {
			c.moveLog(id, step, "Exporting ClickHouse %s on %s (%s%s).", ch.SourceDatabase, job.SourceServer, moveClickHouseFormatLabel(format), moveCompressionLabel(compression))
			c.moveStage(id, fmt.Sprintf("exporting ClickHouse %s on %s", ch.SourceDatabase, job.SourceServer), 0, nil)
			stop := c.moveWatchFiles(ctx, job, job.SourceServer, "clickhouse", "ClickHouse "+ch.SourceDatabase)
			query := url.Values{"format": {format}, "move": {job.ID}}
			if compression != "" {
				query.Set("compress", compression)
			}
			err := c.moveCall(ctx, 6*time.Hour, job.SourceServer, http.MethodPost, "/v1/move/apps/"+app+"/clickhouse/exports?"+query.Encode(), nil, &export)
			stop()
			if err != nil {
				return moveOn(job.SourceServer, fmt.Errorf("the ClickHouse export of %s failed: %w", ch.SourceDatabase, err))
			}
			if export.Size <= 0 || export.Size > moveMaxDumpBytes || !moveDigestPattern.MatchString(export.SHA256) || !moveDigestPattern.MatchString(export.ID) {
				return errors.New("the ClickHouse export has no usable size or digest")
			}
			if export.Fallback != "" {
				c.moveLog(id, step, "The source could not create a ClickHouse BACKUP (%s); it exported the tables one by one instead.", export.Fallback)
			}
			if len(export.Tables) == 0 {
				c.moveCleanupClickHouse(ctx, job)
				_, err := c.updateMove(id, func(job *MoveJob) error {
					job.ClickHouse.Skipped = "no tables"
					job.logf(step, "ClickHouse %s has no tables; nothing to copy.", ch.SourceDatabase)
					return nil
				})
				return err
			}
			if err := cp.SaveMeta(export.ID, export.SHA256, export.Size, export); err != nil {
				return err
			}
			files := ""
			if export.Files > 0 {
				files = fmt.Sprintf(", %d files", export.Files)
			}
			c.moveLog(id, step, "ClickHouse export %s… is ready (%s%s, %d tables%s, %s, %s); streaming it to %s.", export.SHA256[:12], moveClickHouseFormatLabel(export.Format), moveCompressionLabel(export.Compression), len(export.Tables), files, moveSize(export.Size), moveSeconds(export.Seconds), job.TargetServer)
		}
		var result agent.ClickHouseRestoreResult
		err := c.moveRelay(ctx, job, moveRelayStream{
			download: "/v1/move/apps/" + app + "/clickhouse/exports/" + url.PathEscape(export.ID),
			upload:   "/v1/move/apps/" + app + "/clickhouse/restore?replace=1",
			size:     export.Size, digest: export.SHA256, mismatch: errMoveClickHouseSum, what: "ClickHouse archive", resume: moveUploadID(job, cp.key, export.SHA256),
			phase: fmt.Sprintf("streaming ClickHouse %s to %s", ch.SourceDatabase, job.TargetServer),
			after: fmt.Sprintf("restoring ClickHouse %s into %s on %s", ch.SourceDatabase, ch.TargetDatabase, job.TargetServer),
		}, &result)
		if reused && err != nil && strings.Contains(err.Error(), "refused the download") {
			// The source no longer has the export: make a new one.
			reused = false
			continue
		}
		if err == nil {
			err = moveVerifyClickHouse(export, result)
		}
		if err != nil {
			// The BACKUP format is chosen only when the destination runs the
			// same or a newer release series, so a failed restore is a real
			// error to fix and retry, not a reason for the per-table copy,
			// which is several times larger on the source's disk.
			return fmt.Errorf("copying ClickHouse %s failed: %w", ch.SourceDatabase, err)
		}
		c.moveCleanupClickHouse(ctx, job)
		run.objects(run.c.moveLiveEngine(id), int64(result.Tables), int64(len(export.Tables)))
		_, err = c.updateMove(id, func(job *MoveJob) error {
			job.ClickHouse.Copied, job.ClickHouse.Bytes, job.ClickHouse.Format, job.ClickHouse.Skipped = export.SHA256, export.Size, export.Format, ""
			job.ClickHouse.Tables, job.ClickHouse.Rows, job.ClickHouse.SourceVersion = result.Tables, result.Rows, export.Version
			job.logf(step, "Restored ClickHouse %s into %s on %s; the SHA-256 matched on both ends and the row counts of %d tables (%d rows) match the source.", ch.SourceDatabase, result.Database, job.TargetServer, result.Tables, result.Rows)
			// The verified restore is checkpointed in the same write.
			return job.setCheckpointMeta(cp.key, moveClickHouseCheckpoint{MoveClickHouseExport: export, Restored: export.SHA256})
		})
		return err
	}
}

// moveClickHouseRestoreHolds asks the destination whether the database a
// previous attempt restored from export still verifies: it is ready under
// the expected name and its row counts (with the export's cutoff) match the
// export's tables. A destination agent that reports no counts answered
// before they existed; the verification recorded with the restore stands.
// A database that no longer verifies is restored again; an unreachable
// destination is a transient failure, retried like any other.
func (c *Control) moveClickHouseRestoreHolds(ctx context.Context, run moveRun, export agent.MoveClickHouseExport) (bool, error) {
	job := run.job()
	ch := job.ClickHouse
	query := url.Values{"counts": {"1"}}
	if !export.CountCutoff.IsZero() {
		query.Set("cutoff", export.CountCutoff.UTC().Format(time.RFC3339))
	}
	var target agent.MoveClickHouseTarget
	if err := c.moveCall(ctx, 10*time.Minute, job.TargetServer, http.MethodGet, "/v1/move/apps/"+url.PathEscape(job.AppID)+"/clickhouse?"+query.Encode(), nil, &target); err != nil {
		return false, moveOn(job.TargetServer, fmt.Errorf("the destination's ClickHouse %s could not be checked after the earlier restore: %w", ch.TargetDatabase, err))
	}
	switch {
	case !target.Ready || target.Database != ch.TargetDatabase:
		run.log("ClickHouse %s on %s no longer holds the earlier restore of export %s…; restoring it again.", ch.TargetDatabase, job.TargetServer, export.SHA256[:12])
		return false, nil
	case target.Counts == nil:
		run.log("ClickHouse export %s… was restored into %s on %s and verified before this attempt (the destination agent reports no row counts to check again); not restoring it again.", export.SHA256[:12], ch.TargetDatabase, job.TargetServer)
		return true, nil
	}
	if err := agent.VerifyClickHouseRestore(export.Tables, target.Counts); err != nil {
		run.log("ClickHouse %s on %s no longer matches the earlier restore of export %s… (%v); restoring it again.", ch.TargetDatabase, job.TargetServer, export.SHA256[:12], err)
		return false, nil
	}
	run.log("ClickHouse export %s… was restored into %s on %s before this attempt and the row counts of %d tables still match the source's export; not restoring it again.", export.SHA256[:12], ch.TargetDatabase, job.TargetServer, len(target.Counts))
	return true, nil
}

// moveVerifyClickHouse checks the destination's restored row counts against
// the source's export.
func moveVerifyClickHouse(export agent.MoveClickHouseExport, result agent.ClickHouseRestoreResult) error {
	if result.Status != "restored" || result.SHA256 != export.SHA256 {
		return errors.New("the destination did not confirm the ClickHouse restore")
	}
	if err := agent.VerifyClickHouseRestore(export.Tables, result.Counts); err != nil {
		return fmt.Errorf("row-count verification failed: %w", err)
	}
	return nil
}

func moveClickHouseFormatLabel(format string) string {
	switch format {
	case "native":
		return "table by table in Native format"
	case "directory":
		return "ClickHouse BACKUP to a directory"
	}
	return "ClickHouse BACKUP archive"
}

func moveCompressionLabel(compression string) string {
	if compression == "" {
		return ""
	}
	return ", " + compression
}

// moveClickHouseChoice picks the archive format and compression of a copy:
// a BACKUP for a destination of the same or a newer release series, as a
// directory when both agents support it (else the .zip older agents
// restore), Zstandard when both agents have the tool; per-table Native
// otherwise. An agent whose capabilities cannot be read gets the formats
// every agent supports.
func (c *Control) moveClickHouseChoice(ctx context.Context, job MoveJob) (format, compression string) {
	var source, target agent.MoveHost
	sourceErr := c.moveCall(ctx, 15*time.Second, job.SourceServer, http.MethodGet, "/v1/move/host", nil, &source)
	targetErr := c.moveCall(ctx, 15*time.Second, job.TargetServer, http.MethodGet, "/v1/move/host", nil, &target)
	if sourceErr != nil {
		source = agent.MoveHost{}
	}
	if targetErr != nil {
		target = agent.MoveHost{}
	}
	return moveClickHouseFormats(job.ClickHouse.SourceVersion, job.ClickHouse.TargetVersion, source, target)
}

func moveClickHouseFormats(sourceVersion, targetVersion string, source, target agent.MoveHost) (format, compression string) {
	format = agent.ClickHouseMoveFormat(sourceVersion, targetVersion)
	if format == "backup" && slices.Contains(source.ClickHouseFormats, "directory") && slices.Contains(target.ClickHouseFormats, "directory") {
		format = "directory"
	}
	// Only agents that know directory backups detect compressed archives.
	if source.Zstd && target.Zstd && slices.Contains(target.ClickHouseFormats, "directory") {
		compression = "zstd"
	}
	return format, compression
}

// moveCleanupClickHouse removes the source's exports; failures are ignored
// (an older agent, or an export already removed).
func (c *Control) moveCleanupClickHouse(ctx context.Context, job MoveJob) {
	if job.ClickHouse == nil {
		return
	}
	_ = c.moveCall(ctx, time.Minute, job.SourceServer, http.MethodDelete, "/v1/move/apps/"+url.PathEscape(job.AppID)+"/clickhouse/exports", nil, nil)
}
