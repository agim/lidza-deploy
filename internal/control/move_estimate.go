package control

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Move estimates. The plan shows each engine's measured size and objects
// with an estimated bulk-copy and final-sync duration, and the move panel
// shows the expected lock duration before the final sync starts. Rates come
// from, in order of preference:
//
//  1. transfers of earlier moves (bytes and seconds of finished engine
//     checkpoints, end to end per engine kind);
//  2. the source's recent PostgreSQL backups (their size and duration) for
//     the export, and a short disk probe on both agents (POST
//     /v1/move/probe) for writing on the destination;
//  3. conservative defaults.
//
// Every figure is labelled as an estimate with its basis. Once the bulk copy
// ran, the lock estimate uses this move's own measured copy times.

const (
	moveDefaultExportRate   = 40 << 20 // bytes per second
	moveDefaultTransferRate = 25 << 20
	moveDefaultRestoreRate  = 20 << 20
	moveEngineOverhead      = 30.0 // seconds per engine: requests, verification
	moveDeployDefault       = 5 * 60.0
	moveFreezeOverhead      = 60.0
	moveLockConfirmAbove    = 15 * time.Minute
	moveProbeKeep           = 15 * time.Minute
)

// MoveEstimate is a plan's or a move's duration estimate.
type MoveEstimate struct {
	Engines      []MoveEngineEstimate `json:"engines"`
	BulkSeconds  float64              `json:"bulk_seconds"`
	FinalSeconds float64              `json:"final_seconds"`
	// Basis says where the rates come from: "previous moves", "backups
	// and disk probe", "disk probe" or "defaults".
	Basis string   `json:"basis"`
	Notes []string `json:"notes,omitempty"`
}

// MoveEngineEstimate is one engine's size and durations.
type MoveEngineEstimate struct {
	Kind         string  `json:"kind"`
	Object       string  `json:"object"`
	Label        string  `json:"label"`
	Bytes        int64   `json:"bytes"`
	Objects      int64   `json:"objects"`
	ObjectLabel  string  `json:"object_label"`
	Measured     bool    `json:"measured"`
	BulkSeconds  float64 `json:"bulk_seconds"`
	FinalSeconds float64 `json:"final_seconds"`
	Basis        string  `json:"basis"`
	// FinalBasis explains a final-sync figure smaller than the bulk copy:
	// tables copied incrementally, with their insert rates.
	FinalBasis string `json:"final_basis,omitempty"`
}

// MoveLockEstimate is the expected duration of the final-sync lock.
type MoveLockEstimate struct {
	Seconds float64 `json:"seconds"`
	Basis   string  `json:"basis"`
	// Confirm is set when the lock is expected to last longer than 15
	// minutes: the final sync then needs the operator's confirmation.
	Confirm bool `json:"confirm"`
}

type moveProbeResult struct {
	probe agent.MoveProbe
	at    time.Time
}

// moveRates are the throughputs an estimate uses, in bytes per second.
type moveRates struct {
	export, transfer, restore float64
	history                   map[string]float64 // end to end, by engine kind
	basis                     string
}

// moveProbe returns a server's disk probe, measured at most every 15
// minutes; ok is false when the agent cannot probe.
func (c *Control) moveDiskProbe(ctx context.Context, server string) (agent.MoveProbe, bool) {
	c.mu.Lock()
	cached, found := c.moveProbes[server]
	c.mu.Unlock()
	if found && time.Since(cached.at) < moveProbeKeep {
		return cached.probe, cached.probe.WriteBytes > 0
	}
	var probe agent.MoveProbe
	err := c.moveCall(ctx, 90*time.Second, server, http.MethodPost, "/v1/move/probe", nil, &probe)
	if err != nil {
		probe = agent.MoveProbe{}
	}
	c.mu.Lock()
	if c.moveProbes == nil {
		c.moveProbes = map[string]moveProbeResult{}
	}
	c.moveProbes[server] = moveProbeResult{probe: probe, at: time.Now()}
	c.mu.Unlock()
	return probe, err == nil && probe.WriteBytes > 0
}

// moveHistoryRates are end-to-end rates of earlier moves by engine kind.
func (c *Control) moveHistoryRates() map[string]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	bytes, seconds := map[string]float64{}, map[string]float64{}
	for _, job := range c.data.Moves {
		for _, cp := range job.Checkpoints {
			if cp.Done != nil && cp.Seconds > 1 && cp.Size >= 1<<20 {
				bytes[cp.Engine] += float64(cp.Size)
				seconds[cp.Engine] += cp.Seconds
			}
		}
	}
	out := map[string]float64{}
	for kind, n := range bytes {
		out[kind] = n / seconds[kind]
	}
	return out
}

// moveEstimateRates gathers the rates for a move between two servers.
func (c *Control) moveEstimateRates(ctx context.Context, source, target string, sourceBackups []agent.DatabaseView, probe bool) moveRates {
	rates := moveRates{export: moveDefaultExportRate, transfer: moveDefaultTransferRate, restore: moveDefaultRestoreRate, history: c.moveHistoryRates(), basis: "defaults"}
	var backupBytes, backupSeconds float64
	for _, view := range sourceBackups {
		for _, b := range view.Backups {
			if b.Seconds > 1 && b.Size >= 1<<20 {
				backupBytes += float64(b.Size)
				backupSeconds += b.Seconds
			}
		}
	}
	if backupSeconds > 0 {
		rates.export = backupBytes / backupSeconds
		rates.basis = "backups"
	}
	if !probe {
		if len(rates.history) > 0 {
			rates.basis = "previous moves"
		}
		return rates
	}
	if probe, ok := c.moveDiskProbe(ctx, target); ok {
		// Restoring writes, indexes and checks: a third of raw disk writes.
		rates.restore = max(probe.WriteBytes/3, 1<<20)
		if rates.basis == "backups" {
			rates.basis = "backups and disk probe"
		} else {
			rates.basis = "disk probe"
		}
	}
	if probe, ok := c.moveDiskProbe(ctx, source); ok && backupSeconds == 0 {
		rates.export = max(probe.ReadBytes/4, 1<<20)
	}
	if len(rates.history) > 0 {
		rates.basis = "previous moves"
	}
	return rates
}

// moveEstimateEngines estimates every engine of job.
func (c *Control) moveEstimateEngines(ctx context.Context, job MoveJob, rates moveRates, deploySeconds float64) MoveEstimate {
	run := moveRun{c: c, id: job.ID, phase: movePhaseBulk}
	out := MoveEstimate{Engines: []MoveEngineEstimate{}, Basis: rates.basis}
	for _, engine := range c.moveEnginesOf(run, job) {
		info := engine.Info()
		m, _ := engine.Measure(ctx)
		entry := MoveEngineEstimate{Kind: info.Kind, Object: info.Object, Label: info.Label, Bytes: m.Bytes, Objects: m.Objects, ObjectLabel: m.ObjectLabel, Measured: m.Measured, Basis: rates.basis}
		bytes := float64(max(m.Bytes, 0))
		if info.Kind == moveEngineRedis {
			out.Engines = append(out.Engines, entry)
			continue
		}
		if rate := rates.history[info.Kind]; rate > 0 {
			entry.BulkSeconds, entry.Basis = bytes/rate+moveEngineOverhead, "previous moves"
		} else {
			entry.BulkSeconds = bytes/rates.export + bytes/rates.transfer + bytes/rates.restore + moveEngineOverhead
		}
		// The engines copy everything again at the final sync, except
		// PostgreSQL tables with an incremental key: those only send the
		// rows added since the bulk copy, expected from the source's insert
		// rate over the bulk copy's duration and an hour more; and
		// ClickHouse between agents with incremental copies, which sends
		// the parts added since the base backup, measured on the source.
		entry.FinalSeconds = entry.BulkSeconds
		if info.Kind == moveEngineClickHouse && bytes > 0 {
			window := time.Duration(entry.BulkSeconds)*time.Second + time.Hour
			if delta, basis := c.moveClickHouseFinalEstimate(ctx, job, window); basis != "" {
				final := min(float64(max(delta, 0)), bytes)
				entry.FinalSeconds = moveEngineOverhead + (entry.BulkSeconds-moveEngineOverhead)*final/bytes
				entry.FinalBasis = basis
				out.Notes = append(out.Notes, fmt.Sprintf("Final sync of ClickHouse %s: %s.", info.Object, basis))
			}
		}
		if info.Kind == moveEnginePostgres && bytes > 0 {
			for _, d := range job.Databases {
				if d.SourceID != info.Object {
					continue
				}
				if d.Replication == moveReplicationContinuous {
					// The destination follows the source continuously; the
					// locked sync only waits for the last transactions.
					entry.FinalSeconds, entry.FinalBasis = moveEngineOverhead, moveReplicationFinalBasis
					out.Notes = append(out.Notes, fmt.Sprintf("Final sync of %s: seconds (replication lag); the destination follows the source continuously.", info.Object))
					break
				}
				window := time.Duration(entry.BulkSeconds)*time.Second + time.Hour
				if full, delta, basis := moveIncrementalEstimate(d, window); basis != "" {
					final := max(bytes-float64(full)+delta, 0)
					entry.FinalSeconds = moveEngineOverhead + (entry.BulkSeconds-moveEngineOverhead)*final/bytes
					entry.FinalBasis = basis
					out.Notes = append(out.Notes, fmt.Sprintf("Final sync of %s: %s.", info.Object, basis))
				}
			}
		}
		out.BulkSeconds += entry.BulkSeconds
		out.FinalSeconds += entry.FinalSeconds
		out.Engines = append(out.Engines, entry)
	}
	out.BulkSeconds += deploySeconds
	out.FinalSeconds += deploySeconds + moveFreezeOverhead
	out.Notes = append(out.Notes, "Estimates: the final sync copies every file set and every dumped database again with the application locked, waits for replicated databases to catch up, then redeploys the destination.")
	return out
}

// moveEnginesOf builds the engines of a job that need not be stored (a
// plan's): they read the job given, not the state.
func (c *Control) moveEnginesOf(run moveRun, job MoveJob) []moveEngine {
	run.static = &job
	return c.moveEngines(run)
}

// moveLockEstimate is the expected duration of a move's final-sync lock:
// this move's measured bulk copy and deploy times when it has them, else
// the plan's estimate.
func moveLockEstimate(job MoveJob) *MoveLockEstimate {
	seconds, measured := 0.0, false
	for _, cp := range job.Checkpoints {
		if cp.Phase == movePhaseBulk && cp.Done != nil && cp.Seconds > 0 {
			seconds += cp.Seconds
			measured = true
		}
	}
	deploy := moveDeployDefault
	if s := job.step(moveStepDeploy); s != nil && s.Started != nil && s.Finished != nil && s.Status == "done" {
		deploy = s.Finished.Sub(*s.Started).Seconds()
	}
	estimate := &MoveLockEstimate{Basis: "this move's bulk copy"}
	switch {
	case measured:
		if saved := moveIncrementalSavings(job) + moveClickHouseSavings(job); saved > 0 {
			// An engine whose plan expects an incremental final sync
			// (PostgreSQL tables with a watermark, ClickHouse with a kept
			// base) takes the plan's share of its measured bulk time.
			seconds = max(seconds-saved, 0)
			estimate.Basis = "this move's bulk copy, less the data synced incrementally"
		}
		if saved := moveReplicatedSavings(job); saved > 0 {
			// A replicated database's initial sync is not repeated: the
			// locked sync waits for the lag only.
			seconds = max(seconds-saved, 0)
			estimate.Basis = "this move's bulk copy, less the databases that replicate continuously"
		}
		estimate.Seconds = seconds + deploy + moveFreezeOverhead
	case job.Plan.Estimate != nil:
		estimate.Seconds, estimate.Basis = job.Plan.Estimate.FinalSeconds, "the plan's estimate ("+job.Plan.Estimate.Basis+")"
	default:
		estimate.Seconds, estimate.Basis = deploy+moveFreezeOverhead, "defaults"
	}
	estimate.Seconds = math.Round(estimate.Seconds)
	estimate.Confirm = estimate.Seconds > moveLockConfirmAbove.Seconds()
	return estimate
}

func moveSeconds(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return d.Round(time.Minute).String()
}

// movePlanEstimate estimates a planned move from its inventory.
func (c *Control) movePlanEstimate(ctx context.Context, plan MovePlan, source agent.MoveSource, host agent.MoveHost) MoveEstimate {
	job := MoveJob{Plan: plan, Redis: source.Redis, ClickHouse: moveClickHouseFrom(source)}
	for _, d := range source.Databases {
		if !d.Copyable {
			continue
		}
		entry := MoveDatabase{SourceID: d.ID, Copyable: true}
		for _, p := range plan.Databases {
			if p.ID == d.ID && p.Engine != moveEngineClickHouse {
				entry.Excluded, entry.Replication = p.ExcludedTables, p.Replication
			}
		}
		job.Databases = append(job.Databases, entry)
	}
	for _, f := range plan.Files {
		if f.Action == "copy" {
			job.Files = append(job.Files, MoveFileCopy{ID: f.ID, Path: f.Path, Source: f.Source, Files: f.Files, Bytes: f.Bytes, Transfer: true})
		}
	}
	var views []agent.DatabaseView
	_ = c.moveCall(ctx, 30*time.Second, plan.SourceServer, http.MethodGet, "/v1/databases", nil, &views)
	probe := host.Probe
	rates := moveRates{export: moveDefaultExportRate, transfer: moveDefaultTransferRate, restore: moveDefaultRestoreRate, history: c.moveHistoryRates(), basis: "defaults"}
	if probe || len(views) > 0 {
		rates = c.moveEstimateRates(ctx, plan.SourceServer, plan.TargetServer, views, probe)
	}
	return c.moveEstimateEngines(ctx, job, rates, moveDeployDefault)
}

// moveIncrementalSavings is the time the plan expects the incremental
// copies of excluded tables to save at the final sync, scaled to the
// measured bulk copy of each database.
func moveIncrementalSavings(job MoveJob) float64 {
	if job.Plan.Estimate == nil {
		return 0
	}
	saved := 0.0
	for _, entry := range job.Plan.Estimate.Engines {
		if entry.Kind != moveEnginePostgres || entry.FinalBasis == "" || entry.BulkSeconds <= 0 || len(moveIncrementalTables(moveDatabaseOf(job, entry.Object))) == 0 {
			continue
		}
		if cp, ok := job.Checkpoints[moveCheckpointKey(movePhaseBulk, moveEnginePostgres, entry.Object)]; ok && cp.Done != nil && cp.Seconds > 0 {
			saved += cp.Seconds * (1 - entry.FinalSeconds/entry.BulkSeconds)
		}
	}
	return saved
}

func moveDatabaseOf(job MoveJob, source string) MoveDatabase {
	for _, d := range job.Databases {
		if d.SourceID == source {
			return d
		}
	}
	return MoveDatabase{}
}
