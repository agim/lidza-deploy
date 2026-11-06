package control

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Incremental final sync of append-only tables. The bulk copy of a table
// the backups leave out records the table's incremental key and watermark
// when the source agent found one (an integer primary key or an indexed
// creation timestamp, with no updates or deletes while the dump ran). At
// the final sync, with the source frozen, such a table is not dumped
// again: the destination keeps it across the base restore (?keep=), the
// source exports the rows at or after the watermark as a binary COPY, and
// the destination replaces its rows from the watermark on with them in one
// transaction. Both ends then report count(*) and max(key), which must
// match exactly. A table that was updated or deleted from since the bulk
// copy, or whose key the source no longer recognises, falls back to the
// full copy of that table alone, and the log says why.

// moveDeltasCheckpoint records the incremental copies verified in a phase
// against one base dump, by table: the destination's row count.
type moveDeltasCheckpoint struct {
	Dump string           `json:"dump"`
	Rows map[string]int64 `json:"rows"`
}

// moveIncrementalTables lists the tables of d the final sync can copy
// incrementally: chosen for copying, loaded and verified by the bulk copy,
// with an incremental key recorded at that dump.
func moveIncrementalTables(d MoveDatabase) []string {
	out := []string{}
	for _, table := range d.Excluded {
		if table.Choice == moveTableCopy && table.Copied != nil && table.Exported != nil && table.Exported.Key != nil {
			out = append(out, table.Name)
		}
	}
	return out
}

// moveCopyTableDelta copies the rows added to one table since its
// watermark and verifies the result. It returns the source's reason when
// the table must be copied in full instead.
func (c *Control) moveCopyTableDelta(ctx context.Context, run moveRun, i int, table string, key agent.MoveTableKey, cp moveCheckpointRef, saved *movePostgresCheckpoint, dump string) (fallback string, err error) {
	job := run.job()
	d := job.Databases[i]
	source := "/v1/move/databases/" + url.PathEscape(d.SourceID)
	since := "from the start"
	if key.Watermark != "" {
		since = key.Column + " after " + key.Watermark
	}
	run.log("Copying the rows added to %s since the bulk copy (%s) instead of the whole table.", table, since)
	run.stage(fmt.Sprintf("copying the rows added to %s on %s", table, job.SourceServer), 0, nil)
	stop := c.moveWatchDatabase(ctx, job.SourceServer, d.SourceID, table, run.id)
	var export agent.MoveTableDeltaExport
	err = c.moveCall(ctx, 6*time.Hour, job.SourceServer, http.MethodPost, source+"/table-delta", agent.MoveTableDeltaRequest{Table: table, Key: key}, &export)
	stop()
	if err != nil {
		return "", moveOn(job.SourceServer, fmt.Errorf("the rows added to %s could not be exported: %w", table, err))
	}
	if export.Fallback != "" {
		return export.Fallback, nil
	}
	if export.Size <= 0 || export.Size > moveMaxDumpBytes || !moveDigestPattern.MatchString(export.SHA256) || !moveDigestPattern.MatchString(export.ID) {
		return "", fmt.Errorf("the incremental export of %s has no usable size or digest", table)
	}
	run.log("%d rows were added to %s since the bulk copy (%s); streaming them to %s.", export.Rows, table, moveSize(export.Size), job.TargetServer)
	var result agent.MoveTableDeltaResult
	err = c.moveRelay(ctx, job, moveRelayStream{
		download: source + "/table-delta/" + url.PathEscape(export.ID),
		upload:   agent.MoveTableDeltaRestorePath(d.TargetID, table, key),
		size:     export.Size, digest: export.SHA256, mismatch: errMoveChecksum, what: "rows added to " + table, resume: moveUploadID(job, cp.key+"/delta/"+table, export.SHA256),
		phase: fmt.Sprintf("streaming the rows added to %s to %s", table, job.TargetServer),
		after: fmt.Sprintf("appending the rows added to %s on %s", table, job.TargetServer),
	}, &result)
	if err != nil {
		return "", fmt.Errorf("appending the rows added to %s failed: %w", table, err)
	}
	if result.Total != export.Total || result.Max != export.Max {
		return "", fmt.Errorf("verification of %s failed after the incremental copy: %d rows with max %s %q on the source, %d rows with %q on the destination",
			table, export.Total, key.Column, export.Max, result.Total, result.Max)
	}
	_, err = c.updateMove(run.id, func(job *MoveJob) error {
		for j := range job.Databases[i].Excluded {
			t := &job.Databases[i].Excluded[j]
			if t.Name != table {
				continue
			}
			total := export.Total
			next := key
			next.Watermark = export.Max
			t.Copied = &total
			t.Exported = &moveRowBracket{Low: total, High: total, Export: export.ID, SHA256: export.SHA256, Dump: dump, Key: &next}
		}
		job.logf(run.step, "Appended %d rows to %s on %s; %d rows and max %s match the source exactly.", export.Rows, table, job.TargetServer, export.Total, key.Column)
		if saved.Deltas == nil || saved.Deltas.Dump != dump {
			saved.Deltas = &moveDeltasCheckpoint{Dump: dump, Rows: map[string]int64{}}
		}
		saved.Deltas.Rows[table] = export.Total
		return job.setCheckpointMeta(cp.key, *saved)
	})
	return "", err
}

// moveSplitIncremental copies the tables with a watermark incrementally
// and returns the tables that still need the full copy, with the
// checkpoint updated for every delta that verified.
func (c *Control) moveSplitIncremental(ctx context.Context, run moveRun, i int, tables []string, cp moveCheckpointRef, saved *movePostgresCheckpoint, dump string) ([]string, error) {
	d := run.job().Databases[i]
	full := []string{}
	for _, name := range tables {
		var key *agent.MoveTableKey
		for _, table := range d.Excluded {
			if table.Name == name && table.Copied != nil && table.Exported != nil {
				key = table.Exported.Key
			}
		}
		if key == nil {
			full = append(full, name)
			continue
		}
		if saved.Deltas != nil && saved.Deltas.Dump == dump {
			if rows, ok := saved.Deltas.Rows[name]; ok {
				run.log("The rows added to %s were appended on %s before this attempt (%d rows verified); not copying them again.", name, run.job().TargetServer, rows)
				continue
			}
		}
		why, err := c.moveCopyTableDelta(ctx, run, i, name, *key, cp, saved, dump)
		if err != nil {
			return nil, err
		}
		if why != "" {
			run.log("Copying %s in full again: %s.", name, why)
			full = append(full, name)
		}
	}
	return full, nil
}

// moveIncrementalEstimate sizes a database's final sync when some of its
// excluded tables copy incrementally: the bytes those tables would take in
// full, the bytes their rows added over window are expected to take (from
// the source's insert rate and average row size), and the label of that
// basis. Tables without a key, or without an insert rate, count in full.
func moveIncrementalEstimate(d MoveDatabase, window time.Duration) (full int64, delta float64, basis string) {
	parts := []string{}
	for _, table := range d.Excluded {
		if table.Choice != moveTableCopy || table.Key == nil || table.Key.RowsPerHour <= 0 || table.Rows <= 0 || table.Bytes <= 0 {
			continue
		}
		full += table.Bytes
		rows := table.Key.RowsPerHour * window.Hours()
		delta += rows * float64(table.Bytes) / float64(table.Rows)
		parts = append(parts, fmt.Sprintf("%s (%s/hour from %s)", table.Name, moveRowEstimate(int64(table.Key.RowsPerHour)), table.Key.RateBasis))
	}
	if len(parts) == 0 {
		return 0, 0, ""
	}
	return full, delta, "only the rows added since the bulk copy for " + strings.Join(parts, ", ") + "; the other tables in full"
}

// moveDeriveIncrementalKeys gives the tables of database i that the bulk
// copy loaded without recording an incremental key (an older agent made
// the copy, or a checkpoint lost it) the key they would have had: the
// source names the key, the destination reports its highest value and row
// count, and the source confirms it holds exactly that many rows up to it.
// The final sync then keeps those tables and copies only the rows after
// the watermark. A table whose key cannot be derived is copied in full, and
// the log says why. It reports whether any key was recorded.
func (c *Control) moveDeriveIncrementalKeys(ctx context.Context, run moveRun, i int) (bool, error) {
	job := run.job()
	d := job.Databases[i]
	derived := false
	for _, table := range d.Excluded {
		if table.Choice != moveTableCopy || table.Copied == nil || (table.Exported != nil && table.Exported.Key != nil) {
			continue
		}
		key, why, err := c.moveDeriveTableKey(ctx, job, d, table.Name)
		if err != nil {
			return derived, err
		}
		if key == nil {
			run.log("%s is copied in full at the final sync: %s.", table.Name, why)
			continue
		}
		count := *table.Copied
		if _, err := c.updateMove(run.id, func(job *MoveJob) error {
			for j := range job.Databases[i].Excluded {
				t := &job.Databases[i].Excluded[j]
				if t.Name != table.Name {
					continue
				}
				dump := ""
				if t.Exported != nil {
					dump = t.Exported.Dump
				}
				t.Exported = &moveRowBracket{Low: count, High: count, Dump: dump, Key: key}
			}
			job.logf(run.step, "The bulk copy of %s recorded no incremental key; derived %s up to %s from the %d rows on %s, which match the source exactly. Only the rows after it follow.", table.Name, key.Column, key.Watermark, count, job.TargetServer)
			return nil
		}); err != nil {
			return derived, err
		}
		derived = true
	}
	return derived, nil
}

// moveDeriveTableKey finds the key and watermark of one loaded table, or
// says why it has none. Agent errors (an older agent without the route)
// are reasons, not failures: the table is then copied in full.
func (c *Control) moveDeriveTableKey(ctx context.Context, job MoveJob, d MoveDatabase, table string) (*agent.MoveTableKey, string, error) {
	source := "/v1/move/databases/" + url.PathEscape(d.SourceID) + "/table-key"
	target := "/v1/move/databases/" + url.PathEscape(d.TargetID) + "/table-key"
	var detected agent.MoveTableKeyView
	if err := c.moveCall(ctx, 10*time.Minute, job.SourceServer, http.MethodPost, source, agent.MoveTableKeyRequest{Table: table}, &detected); err != nil {
		return nil, "the source could not name an incremental key (" + moveAgentError(err) + ")", ctx.Err()
	}
	if detected.Key == nil {
		return nil, "the source found no incremental key (an integer primary key or an indexed creation timestamp)", nil
	}
	key := *detected.Key
	key.Watermark = ""
	var loaded agent.MoveTableKeyView
	if err := c.moveCall(ctx, 30*time.Minute, job.TargetServer, http.MethodPost, target, agent.MoveTableKeyRequest{Table: table, Key: &key}, &loaded); err != nil {
		return nil, "the destination could not measure the loaded rows (" + moveAgentError(err) + ")", ctx.Err()
	}
	if loaded.Total == 0 || loaded.Max == "" {
		return nil, "the destination holds no rows of it", nil
	}
	key.Watermark = loaded.Max
	var upTo agent.MoveTableKeyView
	if err := c.moveCall(ctx, 30*time.Minute, job.SourceServer, http.MethodPost, source, agent.MoveTableKeyRequest{Table: table, Key: &key}, &upTo); err != nil {
		return nil, "the source could not count the rows up to the destination's watermark (" + moveAgentError(err) + ")", ctx.Err()
	}
	if upTo.UpTo != loaded.Total {
		return nil, fmt.Sprintf("the source holds %d rows up to %s %s but the destination loaded %d; rows changed in other ways than appends", upTo.UpTo, key.Column, key.Watermark, loaded.Total), nil
	}
	return &key, "", nil
}
