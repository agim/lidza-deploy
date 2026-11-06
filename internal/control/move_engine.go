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

// Move engines. Each kind of data a move carries (a PostgreSQL database, a
// ClickHouse database, a set of persistent files, Redis) is an engine with
// the same contract, and the move framework drives every engine the same
// way: retries, checkpoints, progress, stall detection, error reporting and
// estimates live here and in move_run.go, never in an engine.
//
//   - Measure reports the engine's size and object count from the job,
//     without changing anything (estimates, move_estimate.go).
//   - BulkCopy copies while the source keeps serving. It must be
//     resumable: it reads and updates its checkpoint, so a retried attempt
//     continues (reuses an export, resumes an upload) instead of starting
//     over, and a finished checkpoint is skipped.
//   - FinalSync copies what changed, with the source frozen. Engines that
//     cannot copy incrementally fall back to a full copy.
//   - Verify checks the destination after a copy (digests, row counts).
//   - Cleanup removes the engine's temporary artifacts; it is idempotent
//     and never fails the move.
//
// Engines report progress through the step's single tracker (moveRun.stage
// and moveRun.objects, mirrored agent progress), and return errors the
// framework classifies (move_retry.go); an engine returns
// moveOperatorError for causes it knows only the operator can fix.

const (
	movePhaseBulk  = "bulk"
	movePhaseFinal = "final"

	moveEnginePostgres   = "postgresql"
	moveEngineClickHouse = "clickhouse"
	moveEngineFiles      = "files"
	moveEngineRedis      = "redis"
)

// moveEngine is one kind of data a move copies.
type moveEngine interface {
	Info() moveEngineInfo
	Measure(ctx context.Context) (moveMeasurement, error)
	BulkCopy(ctx context.Context, cp moveCheckpointRef) error
	FinalSync(ctx context.Context, cp moveCheckpointRef) error
	Verify(ctx context.Context) error
	Cleanup(ctx context.Context) error
}

// moveEngineInfo names an engine: Kind is the engine (postgresql…), Object
// what it copies (a database or file set id), Label both for people.
type moveEngineInfo struct {
	Kind, Object, Label string
}

// moveMeasurement is an engine's size: bytes and objects (tables, files,
// sets) with what the objects are. Measured is false for sizes that are
// unknown or estimated by the engine itself.
type moveMeasurement struct {
	Bytes       int64
	Objects     int64
	ObjectLabel string
	Measured    bool
}

// MoveCheckpoint is what one engine finished in one phase for one object.
// Artifact, SHA256 and Size name the exported artifact (a backup, export or
// archive) a retry reuses; Meta holds what else the engine needs to reuse
// it; Done marks a finished copy and Seconds how long it took.
type MoveCheckpoint struct {
	Phase    string          `json:"phase"`
	Engine   string          `json:"engine"`
	Object   string          `json:"object"`
	Artifact string          `json:"artifact,omitempty"`
	SHA256   string          `json:"sha256,omitempty"`
	Size     int64           `json:"size,omitempty"`
	Meta     json.RawMessage `json:"meta,omitempty"`
	Started  *time.Time      `json:"started,omitempty"`
	Done     *time.Time      `json:"done,omitempty"`
	Seconds  float64         `json:"seconds,omitempty"`
}

func moveCheckpointKey(phase, engine, object string) string {
	return phase + "/" + engine + "/" + object
}

// moveRun is one engine phase of a step.
type moveRun struct {
	c                *Control
	id, step, phase  string
	engineIndex, all int
	// static is a job that is not stored, for measuring a plan.
	static *MoveJob
}

func (r moveRun) job() MoveJob {
	if r.static != nil {
		return *r.static
	}
	job, _ := r.c.moveJob(r.id)
	return job
}

func (r moveRun) log(format string, args ...any) { r.c.moveLog(r.id, r.step, format, args...) }

// stage reports the engine's current phase with an optional byte counter.
func (r moveRun) stage(phase string, total int64, read func() int64) {
	r.c.moveStage(r.id, phase, total, read)
}

// objects reports how many of the engine's objects are done.
func (r moveRun) objects(label string, done, total int64) {
	r.c.moveTracker(r.id).Objects(label, done, total)
}

// moveCheckpointRef reads and updates one checkpoint of a job.
type moveCheckpointRef struct {
	run moveRun
	key string
}

func (cp moveCheckpointRef) Get() MoveCheckpoint {
	return cp.run.job().Checkpoints[cp.key]
}

func (cp moveCheckpointRef) Save(change func(*MoveCheckpoint)) error {
	_, err := cp.run.c.updateMove(cp.run.id, func(job *MoveJob) error {
		if job.Checkpoints == nil {
			job.Checkpoints = map[string]MoveCheckpoint{}
		}
		current := job.Checkpoints[cp.key]
		change(&current)
		job.Checkpoints[cp.key] = current
		return nil
	})
	return err
}

// SaveMeta stores the engine's reuse data with the artifact.
func (cp moveCheckpointRef) SaveMeta(artifact, digest string, size int64, meta any) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return cp.Save(func(p *MoveCheckpoint) { p.Artifact, p.SHA256, p.Size, p.Meta = artifact, digest, size, raw })
}

// Meta decodes the checkpoint's reuse data into out; a checkpoint without
// any leaves out untouched.
func (cp moveCheckpointRef) Meta(out any) {
	if raw := cp.Get().Meta; len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
}

// setCheckpointMeta replaces one checkpoint's reuse data inside an
// updateMove change, so it is written together with the job's other
// fields of the same transition.
func (job *MoveJob) setCheckpointMeta(key string, meta any) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if job.Checkpoints == nil {
		job.Checkpoints = map[string]MoveCheckpoint{}
	}
	current := job.Checkpoints[key]
	current.Meta = raw
	job.Checkpoints[key] = current
	return nil
}

// moveEngineError attributes an error to the engine that returned it.
type moveEngineError struct {
	engine string
	err    error
}

func (e *moveEngineError) Error() string { return e.err.Error() }
func (e *moveEngineError) Unwrap() error { return e.err }

func moveEngineOf(err error) string {
	var engine *moveEngineError
	if errors.As(err, &engine) {
		return engine.engine
	}
	return ""
}

// moveEngines builds the engines of a job of the given kinds, in copy
// order.
func (c *Control) moveEngines(run moveRun, kinds ...string) []moveEngine {
	job := run.job()
	want := func(kind string) bool { return len(kinds) == 0 || slices.Contains(kinds, kind) }
	out := []moveEngine{}
	if want(moveEnginePostgres) {
		for i, d := range job.Databases {
			if d.Copyable {
				out = append(out, &movePostgresEngine{run: run, index: i})
			}
		}
	}
	if want(moveEngineClickHouse) && job.ClickHouse != nil {
		out = append(out, &moveClickHouseEngine{run: run})
	}
	if want(moveEngineFiles) {
		shared := &moveFilesShared{}
		for i, f := range job.Files {
			if f.Transfer {
				out = append(out, &moveFilesEngine{run: run, index: i, shared: shared})
			}
		}
	}
	if want(moveEngineRedis) && job.Redis {
		out = append(out, &moveRedisEngine{run: run})
	}
	return out
}

// runMoveEngines runs one phase of every engine of kinds; finished
// checkpoints are skipped, so a retried step continues.
func (c *Control) runMoveEngines(ctx context.Context, id, step, phase string, kinds ...string) error {
	run := moveRun{c: c, id: id, step: step, phase: phase}
	engines := c.moveEngines(run, kinds...)
	for i, engine := range engines {
		info := engine.Info()
		run.engineIndex, run.all = i, len(engines)
		cp := moveCheckpointRef{run: run, key: moveCheckpointKey(phase, info.Kind, info.Object)}
		if done := cp.Get().Done; done != nil {
			run.log("%s was copied in this phase already (%s); skipping it.", info.Label, done.Format(time.RFC3339))
			continue
		}
		c.setMoveLiveEngine(id, info.Label)
		run.objects(info.Label, int64(i), int64(len(engines)))
		started := time.Now().UTC()
		if err := cp.Save(func(p *MoveCheckpoint) {
			p.Phase, p.Engine, p.Object = phase, info.Kind, info.Object
			if p.Started == nil {
				p.Started = &started
			}
		}); err != nil {
			return err
		}
		copy := engine.BulkCopy
		if phase == movePhaseFinal {
			copy = engine.FinalSync
		}
		err := copy(ctx, cp)
		if err == nil {
			err = engine.Verify(ctx)
		}
		if err != nil {
			return &moveEngineError{engine: info.Label, err: err}
		}
		finished := time.Now().UTC()
		if err := cp.Save(func(p *MoveCheckpoint) { p.Done, p.Seconds = &finished, finished.Sub(started).Seconds() }); err != nil {
			return err
		}
		run.objects(info.Label, int64(i+1), int64(len(engines)))
	}
	c.setMoveLiveEngine(id, "")
	moveCleanupOnce(ctx, engines)
	return nil
}

// moveCleanupOnce cleans up every engine; file sets share the move's spool,
// which is removed once.
func moveCleanupOnce(ctx context.Context, engines []moveEngine) {
	files := false
	for _, engine := range engines {
		if engine.Info().Kind == moveEngineFiles {
			if files {
				continue
			}
			files = true
		}
		_ = engine.Cleanup(ctx)
	}
}

// clearMoveCheckpoints forgets a phase's checkpoints, so it copies anew.
func (job *MoveJob) clearMoveCheckpoints(phase string) {
	for key := range job.Checkpoints {
		if strings.HasPrefix(key, phase+"/") {
			delete(job.Checkpoints, key)
		}
	}
}

// ---- PostgreSQL ----------------------------------------------------------------

// movePostgresEngine copies one PostgreSQL database through a fresh backup
// of the source's backup pipeline, streamed into the destination's
// database; tables whose data the backups leave out are copied as a
// separate data-only dump, or left behind, as the operator chose.
type movePostgresEngine struct {
	run   moveRun
	index int
}

// movePostgresCheckpoint is the PostgreSQL engine's reuse data: what of the
// phase's copy the destination already holds, so an attempt resumed after a
// restart of the control panel (or a retry) never restores or loads again
// what finished and verified. The checkpoint's Artifact and SHA256 name the
// dump; both are reset when a fresh backup is taken, which forgets this.
type movePostgresCheckpoint struct {
	// Restored is the digest of the dump restored into the destination
	// database, with its SHA-256 matched on both ends.
	Restored string `json:"restored,omitempty"`
	// Tables is the verified load of the excluded tables' data that
	// followed the restore of Restored.
	Tables *moveTablesCheckpoint `json:"tables,omitempty"`
}

// moveTablesCheckpoint records one finished load of the data of tables the
// backups leave out (move_tables.go).
type moveTablesCheckpoint struct {
	// Dump is the digest of the base dump the load followed: a restore of
	// another dump replaces the database, so the load is needed again.
	Dump string `json:"dump"`
	// Export and SHA256 name the data-only dump that was loaded.
	Export string `json:"export"`
	SHA256 string `json:"sha256"`
	// Tables is the sorted set of tables chosen for copying at the time.
	Tables []string `json:"tables"`
	// Rows are the row counts verified after the load, by table.
	Rows map[string]int64 `json:"rows"`
}

func (e *movePostgresEngine) database() MoveDatabase { return e.run.job().Databases[e.index] }

func (e *movePostgresEngine) Info() moveEngineInfo {
	d := e.database()
	return moveEngineInfo{Kind: moveEnginePostgres, Object: d.SourceID, Label: "PostgreSQL " + d.SourceID}
}

func (e *movePostgresEngine) Measure(context.Context) (moveMeasurement, error) {
	job := e.run.job()
	d := job.Databases[e.index]
	for _, p := range job.Plan.Databases {
		if p.ID == d.SourceID && p.Engine != moveEngineClickHouse {
			bytes := p.SizeBytes
			for _, table := range d.Excluded {
				if table.Choice == moveTableLeave && table.Bytes > 0 {
					bytes = max(bytes-table.Bytes, 0)
				}
			}
			return moveMeasurement{Bytes: bytes, Objects: int64(max(p.Tables, 0)), ObjectLabel: "tables", Measured: p.SizeBytes > 0}, nil
		}
	}
	return moveMeasurement{ObjectLabel: "tables"}, nil
}

func (e *movePostgresEngine) BulkCopy(ctx context.Context, cp moveCheckpointRef) error {
	return e.run.c.moveCopyDatabase(ctx, e.run, e.index, cp)
}

// FinalSync copies the database again: there is no incremental copy yet.
func (e *movePostgresEngine) FinalSync(ctx context.Context, cp moveCheckpointRef) error {
	return e.run.c.moveCopyDatabase(ctx, e.run, e.index, cp)
}

func (e *movePostgresEngine) Verify(ctx context.Context) error {
	return e.run.c.moveVerifyExcludedTables(ctx, e.run, e.index)
}

func (e *movePostgresEngine) Cleanup(ctx context.Context) error {
	job := e.run.job()
	d := job.Databases[e.index]
	if len(d.Excluded) == 0 {
		return nil
	}
	return e.run.c.moveCall(ctx, time.Minute, job.SourceServer, http.MethodDelete, "/v1/move/databases/"+url.PathEscape(d.SourceID)+"/table-data", nil, nil)
}

// moveCopyDatabase copies database index of the job.
func (c *Control) moveCopyDatabase(ctx context.Context, run moveRun, i int, cp moveCheckpointRef) error {
	job := run.job()
	d := job.Databases[i]
	id, step := run.id, run.step
	var count struct {
		Tables int `json:"tables"`
	}
	if err := c.moveCall(ctx, time.Minute, job.SourceServer, http.MethodGet, "/v1/move/databases/"+url.PathEscape(d.SourceID)+"/tables", nil, &count); err != nil {
		return moveOn(job.SourceServer, fmt.Errorf("database %s could not be measured: %w", d.SourceID, err))
	}
	if count.Tables == 0 {
		_, err := c.updateMove(id, func(job *MoveJob) error {
			job.Databases[i].Skipped = "no tables"
			job.logf(step, "Database %s has no tables; nothing to copy.", d.SourceID)
			return nil
		})
		return err
	}
	if d.TargetID == "" {
		return moveOperatorError("destination_unknown", fmt.Sprintf("the destination database for %s is unknown", d.SourceID), "Retry the move from Prepare destination, or abort it.")
	}
	// A dump this phase restored already (the attempt stopped afterwards,
	// for example when the control panel restarted during the excluded
	// tables' load) is not streamed or restored again, whether or not the
	// source still has the backup.
	saved := cp.Get()
	var done movePostgresCheckpoint
	cp.Meta(&done)
	if done.Restored != "" && done.Restored == saved.SHA256 {
		c.moveLog(id, step, "Backup %s of %s was restored into %s on %s before this attempt (SHA-256 %s… matched on both ends); not restoring it again.", saved.Artifact, d.SourceID, d.TargetID, job.TargetServer, saved.SHA256[:12])
		return c.moveCopyExcludedTables(ctx, run, i, cp, saved.SHA256)
	}
	record, reused := c.moveReusableBackup(ctx, job.SourceServer, d.SourceID, saved)
	if reused {
		c.moveLog(id, step, "Reusing backup %s of %s from the previous attempt (%s).", record.ID, d.SourceID, moveSize(record.Size))
	} else {
		c.moveLog(id, step, "Taking a fresh backup of %s (%d tables) on %s.", d.SourceID, count.Tables, job.SourceServer)
		c.moveStage(id, fmt.Sprintf("dumping %s on %s", d.SourceID, job.SourceServer), 0, nil)
		var err error
		record, err = c.moveFreshBackup(ctx, job.SourceServer, d.SourceID, func(p *agent.Progress) { c.moveMirror(id, d.SourceID, job.SourceServer, p) })
		if err != nil {
			return moveOn(job.SourceServer, fmt.Errorf("the backup of %s failed: %w", d.SourceID, err))
		}
		if err := cp.SaveMeta(record.ID, record.SHA256, record.Size, nil); err != nil {
			return err
		}
		c.moveLog(id, step, "Backup %s is ready (%s, SHA-256 %s…); streaming it to %s.", record.ID, moveSize(record.Size), record.SHA256[:12], job.TargetServer)
	}
	if err := moveExcludedDecided(d, record); err != nil {
		// The choice shows estimated rows and sizes from the source's
		// table listing.
		var listing agent.DatabaseTables
		if moveExcludedUnmeasured(moveMergeExcluded(d.Excluded, record.ExcludedTableData)) {
			_ = c.moveCall(ctx, time.Minute, job.SourceServer, http.MethodGet, "/v1/databases/"+url.PathEscape(d.SourceID)+"/tables", nil, &listing)
		}
		if _, saveErr := c.updateMove(id, func(job *MoveJob) error {
			merged := moveMergeExcluded(job.Databases[i].Excluded, record.ExcludedTableData)
			moveFillExcludedEstimates(merged, listing)
			job.Databases[i].Excluded = merged
			return nil
		}); saveErr != nil {
			return saveErr
		}
		return err
	}
	if err := c.moveTransferDump(ctx, job, d, record, moveUploadID(job, cp.key, record.SHA256)); err != nil {
		return fmt.Errorf("copying %s failed: %w", d.SourceID, err)
	}
	if _, err := c.updateMove(id, func(job *MoveJob) error {
		job.Databases[i].Copied, job.Databases[i].Bytes, job.Databases[i].Skipped = record.SHA256, record.Size, ""
		job.logf(step, "Restored %s into %s on %s; the SHA-256 matched on both ends.", d.SourceID, d.TargetID, job.TargetServer)
		// The restore is checkpointed with the job's record of it, in one
		// write: a later attempt loads the excluded tables and nothing else.
		return job.setCheckpointMeta(cp.key, movePostgresCheckpoint{Restored: record.SHA256})
	}); err != nil {
		return err
	}
	return c.moveCopyExcludedTables(ctx, run, i, cp, record.SHA256)
}

// moveReusableBackup returns the backup a previous attempt of this phase
// took, when the source still has it.
func (c *Control) moveReusableBackup(ctx context.Context, server, database string, cp MoveCheckpoint) (agent.BackupRecord, bool) {
	if cp.Artifact == "" || cp.SHA256 == "" {
		return agent.BackupRecord{}, false
	}
	var views []agent.DatabaseView
	if err := c.moveCall(ctx, time.Minute, server, http.MethodGet, "/v1/databases", nil, &views); err != nil {
		return agent.BackupRecord{}, false
	}
	for _, v := range views {
		if v.AppID != database {
			continue
		}
		for _, b := range v.Backups {
			if b.ID == cp.Artifact && b.SHA256 == cp.SHA256 && b.Size > 0 {
				return b, true
			}
		}
	}
	return agent.BackupRecord{}, false
}

// ---- ClickHouse ----------------------------------------------------------------

type moveClickHouseEngine struct{ run moveRun }

func (e *moveClickHouseEngine) Info() moveEngineInfo {
	ch := e.run.job().ClickHouse
	return moveEngineInfo{Kind: moveEngineClickHouse, Object: ch.SourceDatabase, Label: "ClickHouse " + ch.SourceDatabase}
}

func (e *moveClickHouseEngine) Measure(context.Context) (moveMeasurement, error) {
	job := e.run.job()
	for _, p := range job.Plan.Databases {
		if p.Engine == moveEngineClickHouse {
			return moveMeasurement{Bytes: p.SizeBytes, Objects: int64(max(p.Tables, 0)), ObjectLabel: "tables", Measured: p.SizeBytes > 0}, nil
		}
	}
	return moveMeasurement{ObjectLabel: "tables"}, nil
}

func (e *moveClickHouseEngine) BulkCopy(ctx context.Context, cp moveCheckpointRef) error {
	return e.run.c.moveCopyClickHouse(ctx, e.run, cp)
}

// FinalSync copies the database again: there is no incremental copy yet.
func (e *moveClickHouseEngine) FinalSync(ctx context.Context, cp moveCheckpointRef) error {
	return e.run.c.moveCopyClickHouse(ctx, e.run, cp)
}

// Verify: the copy checks the row counts of every table on both ends.
func (e *moveClickHouseEngine) Verify(context.Context) error { return nil }

func (e *moveClickHouseEngine) Cleanup(ctx context.Context) error {
	e.run.c.moveCleanupClickHouse(ctx, e.run.job())
	return nil
}

// ---- files -----------------------------------------------------------------------

type moveFilesShared struct {
	checked, storage bool
	// compression is the archives' compression both agents support.
	negotiated  bool
	compression string
}

type moveFilesEngine struct {
	run    moveRun
	index  int
	shared *moveFilesShared
}

func (e *moveFilesEngine) set() MoveFileCopy { return e.run.job().Files[e.index] }

func (e *moveFilesEngine) Info() moveEngineInfo {
	set := e.set()
	return moveEngineInfo{Kind: moveEngineFiles, Object: set.ID, Label: "files in " + set.Source}
}

func (e *moveFilesEngine) Measure(context.Context) (moveMeasurement, error) {
	set := e.set()
	return moveMeasurement{Bytes: max(set.Bytes, 0), Objects: max(set.Files, 0), ObjectLabel: "files", Measured: set.Bytes >= 0}, nil
}

func (e *moveFilesEngine) BulkCopy(ctx context.Context, cp moveCheckpointRef) error {
	return e.run.c.moveCopyFileSet(ctx, e.run, e.index, e.shared, cp)
}

// FinalSync copies the set again: there is no incremental copy yet.
func (e *moveFilesEngine) FinalSync(ctx context.Context, cp moveCheckpointRef) error {
	return e.run.c.moveCopyFileSet(ctx, e.run, e.index, e.shared, cp)
}

// Verify: the destination verifies the archive's digest before it
// extracts it.
func (e *moveFilesEngine) Verify(context.Context) error { return nil }

func (e *moveFilesEngine) Cleanup(ctx context.Context) error {
	e.run.c.moveCleanupFiles(ctx, e.run.job(), e.run.step)
	return nil
}

// ---- Redis -----------------------------------------------------------------------

// moveRedisEngine stands for the application's Redis: the destination
// provisions its own, and its contents (cache and queues) are not copied.
type moveRedisEngine struct{ run moveRun }

func (e *moveRedisEngine) Info() moveEngineInfo {
	return moveEngineInfo{Kind: moveEngineRedis, Object: "redis", Label: "Redis"}
}
func (e *moveRedisEngine) Measure(context.Context) (moveMeasurement, error) {
	return moveMeasurement{ObjectLabel: "keys", Measured: true}, nil
}
func (e *moveRedisEngine) BulkCopy(context.Context, moveCheckpointRef) error {
	e.run.log("Redis contents are not copied; the destination starts with an empty cache and queues.")
	return nil
}
func (e *moveRedisEngine) FinalSync(ctx context.Context, cp moveCheckpointRef) error {
	return e.BulkCopy(ctx, cp)
}
func (e *moveRedisEngine) Verify(context.Context) error  { return nil }
func (e *moveRedisEngine) Cleanup(context.Context) error { return nil }
