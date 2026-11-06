package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Persistent files of a move. The source agent archives each file set
// (shared directories of native applications, storage and stack volumes of
// container applications) into its spool with the archive's SHA-256. When
// workspace off-site storage is configured on both agents, the source
// uploads the archive to a move-scoped object (moves/<move>/files/<set>)
// and the destination downloads and verifies it; otherwise, or when that
// fails, the archive streams through the control panel like a database
// dump, verified end to end. The destination extracts into a staging
// directory and swaps it in, so every copy is a complete replacement and a
// retry is safe. Spools and objects are removed after the copy, and again
// when the move finishes or is aborted.

const (
	moveFileMethodS3     = "s3"
	moveFileMethodDirect = "direct"
)

// planMoveFiles adds the source's file sets to the plan and returns the
// destination disk space they need.
func (c *Control) planMoveFiles(plan *MovePlan, source agent.MoveSource, host agent.MoveHost) uint64 {
	if source.Files == nil {
		// Agents that cannot transfer files report volumes only.
		for _, volume := range source.Volumes {
			plan.Warnings = append(plan.Warnings, "Files are not moved automatically: "+volume+". The source agent cannot transfer files; upgrade it, or copy them to the destination before finishing.")
		}
		return 0
	}
	c.mu.Lock()
	storage := c.data.BackupStorage != nil
	c.mu.Unlock()
	method := moveFileMethodDirect
	if storage && source.Files.Storage {
		method = moveFileMethodS3
	}
	var needed uint64
	var skipped int64
	for _, set := range source.Files.Sets {
		entry := MovePlanFileSet{ID: set.ID, Path: set.Path, Source: set.Source, Files: set.Files, Bytes: set.Bytes, Skipped: set.Skipped}
		skipped += set.Skipped
		switch {
		case len(set.Problems) > 0:
			entry.Action = "blocked"
			plan.Blockers = append(plan.Blockers, moveFileBlocker(set))
		case plan.TargetRuntime == moveRuntimeNative && strings.HasPrefix(set.Path, "stack/"):
			entry.Action, entry.Note = "manual", "the native runtime has no service volumes"
			if !set.Empty {
				plan.Warnings = append(plan.Warnings, "Files in "+set.Source+" are not moved: the native runtime has no service volumes. Copy them to the destination yourself if the application needs them.")
			}
		case !host.Files:
			entry.Action, entry.Note = "manual", "the destination agent cannot receive files"
			if !set.Empty {
				plan.Warnings = append(plan.Warnings, "Files are not moved automatically: "+set.Source+". The destination agent cannot receive files; upgrade it, or copy them before finishing.")
			}
		case set.Empty:
			entry.Action = "skip_empty"
		default:
			entry.Action, entry.Method = "copy", method
			if set.Bytes > 0 {
				// The archive and the extracted files.
				needed += 2 * uint64(set.Bytes)
			}
			if set.Files < 0 {
				entry.Note = "not measured in time; measured while copying"
			}
		}
		plan.Files = append(plan.Files, entry)
	}
	plan.Warnings = append(plan.Warnings, source.Files.Warnings...)
	if len(plan.Files) > 0 {
		plan.Notes = append(plan.Notes, "Files written on the source after they are copied are not on the destination. The final sync copies them again with the source in maintenance.")
	}
	if skipped > 0 {
		plan.Notes = append(plan.Notes, "Sockets and pipes in the file sets are runtime state that the services recreate when they start on the destination; they are not copied.")
	}
	return needed
}

// moveFileBlocker explains a set that cannot move. Current agents report
// only links that leave the set; older agents also listed sockets, pipes
// and devices, which an upgraded agent skips instead.
func moveFileBlocker(set agent.MoveFileSet) string {
	advice := "Links must stay inside the directory; remove or replace them on the source."
	for _, problem := range set.Problems {
		for _, kind := range []string{"(socket)", "(named pipe)", "(device)", "(special file)"} {
			if strings.HasSuffix(problem, kind) {
				advice = "Links must stay inside the directory. Sockets, pipes and devices are runtime files that an upgraded source agent skips; upgrade the agent on the source, or remove them there."
			}
		}
	}
	return fmt.Sprintf("Files in %s cannot be moved: %s. %s", set.Source, strings.Join(set.Problems, ", "), advice)
}

// moveFileSkipped names the runtime files an archive left behind, for the
// step log.
func moveFileSkipped(archive agent.MoveFileArchive) string {
	if archive.Skipped <= 0 {
		return ""
	}
	return fmt.Sprintf("; skipped: %d runtime sockets/pipes (recreated by the service)", archive.Skipped)
}

// moveFileStorage sends workspace storage to the destination so it can
// download archives; false means archives stream directly.
func (c *Control) moveFileStorage(ctx context.Context, job MoveJob) bool {
	c.mu.Lock()
	storage := c.data.BackupStorage
	c.mu.Unlock()
	if storage == nil {
		return false
	}
	return c.agentCall(moveRequestFor(ctx), job.TargetServer, http.MethodPut, "/v1/backup-storage", backupStorageConfig(*storage), nil) == nil
}

func moveFileSize(set MoveFileCopy) string {
	if set.Bytes < 0 {
		return "size unknown"
	}
	return fmt.Sprintf("%d files, %s", set.Files, moveSize(set.Bytes))
}

// moveCopyFiles copies every transferable file set. The source must be
// frozen for a final sync; the regular step copies while it serves.
func (c *Control) moveCopyFiles(ctx context.Context, id, step string) error {
	job, _ := c.moveJob(id)
	if len(job.Files) == 0 {
		c.moveLog(id, step, "The application has no persistent files to copy.")
		return nil
	}
	for _, set := range job.Files {
		if !set.Transfer {
			c.moveLog(id, step, "Files in %s are not moved (acknowledged in the plan).", set.Source)
		}
	}
	phase := movePhaseBulk
	if step == moveStepFinalSync {
		phase = movePhaseFinal
	}
	return c.runMoveEngines(ctx, id, step, phase, moveEngineFiles)
}

// moveCopyFileSet copies set index of the job. An archive a previous
// attempt of the phase made is sent again while the source still has it.
func (c *Control) moveCopyFileSet(ctx context.Context, run moveRun, i int, shared *moveFilesShared, cp moveCheckpointRef) error {
	id, step := run.id, run.step
	job := run.job()
	set := job.Files[i]
	appPath := "/v1/move/apps/" + url.PathEscape(job.AppID) + "/files/"
	if !shared.checked && set.Method == moveFileMethodS3 {
		shared.checked = true
		shared.storage = c.moveFileStorage(ctx, job)
		if !shared.storage {
			c.moveLog(id, step, "Workspace storage could not be sent to %s; archives stream through the control panel instead.", job.TargetServer)
		}
	}
	upload := shared.storage && set.Method == moveFileMethodS3
	if !shared.negotiated {
		shared.negotiated = true
		if c.moveBothZstd(ctx, job) {
			shared.compression = "zstd"
		}
	}
	var archive agent.MoveFileArchive
	reused := false
	if saved := cp.Get(); saved.Artifact != "" && json.Unmarshal(saved.Meta, &archive) == nil && archive.SHA256 == saved.Artifact {
		reused = true
		c.moveLog(id, step, "Reusing the archive of %s from the previous attempt (%s).", set.Source, moveSize(archive.Size))
	}
	for {
		if !reused {
			c.moveStage(id, fmt.Sprintf("archiving %s on %s", set.Source, job.SourceServer), 0, nil)
			stopWatch := c.moveWatchFiles(ctx, job, job.SourceServer, set.ID, set.Source)
			err := c.moveCall(ctx, 6*time.Hour, job.SourceServer, http.MethodPost, appPath+url.PathEscape(set.ID)+"/archive", agent.MoveFileArchiveRequest{Move: job.ID, Upload: upload, Compression: shared.compression}, &archive)
			stopWatch()
			if err != nil {
				return moveOn(job.SourceServer, fmt.Errorf("files in %s could not be archived: %w", set.Source, err))
			}
			if archive.Empty {
				_, err := c.updateMove(id, func(job *MoveJob) error {
					job.Files[i].Skipped, job.Files[i].Files, job.Files[i].Bytes = "no files", 0, 0
					job.logf(step, "%s: no files; nothing to copy%s.", set.Source, moveFileSkipped(archive))
					return nil
				})
				return err
			}
			if archive.Size <= 0 || !moveDigestPattern.MatchString(archive.SHA256) {
				return fmt.Errorf("the archive of %s has no usable size or digest", set.Source)
			}
			if err := cp.SaveMeta(archive.SHA256, archive.SHA256, archive.Size, archive); err != nil {
				return err
			}
			c.moveLog(id, step, "Archived %d files (%s) from %s on %s: %s%s, SHA-256 %s…%s.", archive.Files, moveSize(archive.Bytes), set.Source, job.SourceServer, moveSize(archive.Size), moveCompressionLabel(archive.Compression), archive.SHA256[:12], moveFileSkipped(archive))
		}
		used := moveFileMethodDirect
		var restored agent.MoveFileRestore
		switch {
		case archive.Key != "":
			c.moveStage(id, fmt.Sprintf("receiving %s on %s from workspace storage", set.Source, job.TargetServer), 0, nil)
			stopWatch := c.moveWatchFiles(ctx, job, job.TargetServer, set.ID, set.Source)
			err := c.moveCall(ctx, 6*time.Hour, job.TargetServer, http.MethodPost, appPath+url.PathEscape(set.ID)+"/fetch", agent.MoveFileFetchRequest{Move: job.ID, Key: archive.Key, Size: archive.Size, SHA256: archive.SHA256}, &restored)
			stopWatch()
			if err == nil {
				used = moveFileMethodS3
			} else {
				c.moveLog(id, step, "%s could not restore %s from workspace storage (%s); streaming the archive directly instead.", job.TargetServer, set.Source, moveAgentError(err))
			}
		case upload && !reused:
			reason := archive.UploadError
			if reason == "" {
				reason = "no object was reported"
			}
			c.moveLog(id, step, "The source could not upload %s to workspace storage (%s); streaming the archive directly instead.", set.Source, reason)
		}
		if used == moveFileMethodDirect {
			if err := c.moveTransferFiles(ctx, job, set, archive, &restored, moveUploadID(job, cp.key, archive.SHA256)); err != nil {
				if reused && strings.Contains(err.Error(), "refused the download") {
					// The source no longer has the archive: make a new one.
					reused = false
					continue
				}
				return fmt.Errorf("copying files in %s failed: %w", set.Source, err)
			}
		}
		run.objects(c.moveLiveEngine(id), archive.Files, archive.Files)
		_, err := c.updateMove(id, func(job *MoveJob) error {
			f := &job.Files[i]
			f.Copied, f.ArchiveBytes, f.Used, f.Files, f.Bytes, f.Skipped = archive.SHA256, archive.Size, used, archive.Files, archive.Bytes, ""
			how := "through the control panel"
			if used == moveFileMethodS3 {
				how = "through workspace storage"
			}
			where := restored.Path
			if where == "" {
				where = set.Path
			}
			job.logf(step, "Restored %s (%s) into %s on %s %s; the SHA-256 matched on both ends.", set.Source, moveFileSize(*f), where, job.TargetServer, how)
			return nil
		})
		return err
	}
}

// moveTransferFiles streams one archive from the source to the destination.
func (c *Control) moveTransferFiles(ctx context.Context, job MoveJob, set MoveFileCopy, archive agent.MoveFileArchive, restored *agent.MoveFileRestore, upload string) error {
	appPath := "/v1/move/apps/" + url.PathEscape(job.AppID) + "/files/" + url.PathEscape(set.ID)
	query := "?move=" + url.QueryEscape(job.ID)
	return c.moveRelay(ctx, job, moveRelayStream{download: appPath + "/archive" + query, upload: appPath + "/restore" + query,
		size: archive.Size, digest: archive.SHA256, mismatch: errMoveArchiveSum, what: "archive", resume: upload,
		phase: fmt.Sprintf("streaming the archive of %s to %s", set.Source, job.TargetServer),
		after: fmt.Sprintf("extracting %s on %s", set.Source, job.TargetServer),
		watch: func() func() { return c.moveWatchFiles(ctx, job, job.TargetServer, set.ID, set.Source) }}, restored)
}

// moveCleanupFiles removes the move's archives from both agents' spools and
// from workspace storage. Failures are logged; they never fail the move.
func (c *Control) moveCleanupFiles(ctx context.Context, job MoveJob, step string) {
	if !moveHasFiles(job) {
		return
	}
	for _, server := range []string{job.SourceServer, job.TargetServer} {
		if err := c.moveCall(ctx, 3*time.Minute, server, http.MethodDelete, "/v1/move/files/"+url.PathEscape(job.ID), nil, nil); err != nil {
			c.moveLog(job.ID, step, "The move's temporary file archives could not be removed on %s (%s); remove moves/%s/ from workspace storage if it remains.", server, moveAgentError(err), job.ID)
		}
	}
}

// moveBothZstd reports whether both agents write and read Zstandard, so
// archives use it instead of gzip.
func (c *Control) moveBothZstd(ctx context.Context, job MoveJob) bool {
	for _, server := range []string{job.SourceServer, job.TargetServer} {
		var host agent.MoveHost
		if err := c.moveCall(ctx, 15*time.Second, server, http.MethodGet, "/v1/move/host", nil, &host); err != nil || !host.Zstd {
			return false
		}
	}
	return true
}
