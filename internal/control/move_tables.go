package control

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Tables a source's backups leave out. A PostgreSQL database moves through
// the source's backup pipeline, and a database's "backups skip data of
// these tables" setting keeps the rows of disposable tables out of backups
// (their schema stays). Without more, those tables would arrive empty. The
// plan lists them with their estimated rows and sizes; before the move
// starts the operator chooses per table to copy its data (the default) or
// leave it behind (schema only), and the start request records the choice. Chosen tables move as a separate data-only dump of
// just those tables, at the bulk copy and again at the final sync, so the
// backup policy itself stays as it is. Verification compares the exact row
// counts with the source's (strictly at the final sync, when the source is
// frozen) and requires left-behind tables to be empty. The log and the
// finish summary name every table left behind.
//
// A backup that leaves out a table the job has no choice for (a job created
// before plans offered the choice, or a setting changed during the move)
// stops the move with the tables listed, measured from the source's table
// listing; the operator chooses in the move panel and continues.

const (
	moveTableCopy  = "copy"
	moveTableLeave = "leave"
)

// MoveExcludedTable is one table whose data the source's backups leave out.
// Choice is copy or leave ("" until the operator chose); Rows and Bytes
// are the source's estimate at planning; Copied is the exact row count the
// last copy verified.
type MoveExcludedTable struct {
	Name   string `json:"name"`
	Rows   int64  `json:"rows"`
	Bytes  int64  `json:"bytes"`
	Choice string `json:"choice"`
	Copied *int64 `json:"copied_rows,omitempty"`
}

// moveExcludedChoices builds a database's table list from the source's
// inventory and the operator's choices (copy unless leave is chosen).
func moveExcludedChoices(tables []agent.MoveExcludedTable, choices map[string]string) []MoveExcludedTable {
	out := []MoveExcludedTable{}
	for _, table := range tables {
		choice := moveTableCopy
		if choices[table.Name] == moveTableLeave {
			choice = moveTableLeave
		}
		out = append(out, MoveExcludedTable{Name: table.Name, Rows: table.Rows, Bytes: table.Bytes, Choice: choice})
	}
	return out
}

// moveMergeExcluded adds tables a backup left out that the job does not
// list yet, undecided.
func moveMergeExcluded(known []MoveExcludedTable, excluded []string) []MoveExcludedTable {
	out := slices.Clone(known)
	for _, name := range excluded {
		if !slices.ContainsFunc(out, func(t MoveExcludedTable) bool { return t.Name == name }) {
			out = append(out, MoveExcludedTable{Name: name, Rows: -1, Bytes: -1})
		}
	}
	return out
}

// moveFillExcludedEstimates fills unknown estimated rows and sizes from a
// database's table listing.
func moveFillExcludedEstimates(tables []MoveExcludedTable, listing agent.DatabaseTables) {
	for i := range tables {
		for _, listed := range listing.Tables {
			if listed.Name != tables[i].Name {
				continue
			}
			if tables[i].Rows < 0 {
				tables[i].Rows = listed.Rows
			}
			if tables[i].Bytes < 0 {
				tables[i].Bytes = listed.Bytes
			}
		}
	}
}

// moveExcludedUnmeasured reports whether any table lacks an estimate.
func moveExcludedUnmeasured(tables []MoveExcludedTable) bool {
	return slices.ContainsFunc(tables, func(t MoveExcludedTable) bool { return t.Rows < 0 || t.Bytes < 0 })
}

// moveExcludedDecided refuses a backup that left out tables without a
// choice.
func moveExcludedDecided(d MoveDatabase, record agent.BackupRecord) error {
	undecided := []string{}
	for _, name := range record.ExcludedTableData {
		i := slices.IndexFunc(d.Excluded, func(t MoveExcludedTable) bool { return t.Name == name })
		if i < 0 || d.Excluded[i].Choice == "" {
			undecided = append(undecided, name)
		}
	}
	if len(undecided) == 0 {
		return nil
	}
	return moveOperatorError("excluded_tables", fmt.Sprintf("the source's backups of %s leave out the data of %s", d.SourceID, strings.Join(undecided, ", ")),
		"Choose in the move panel whether to copy each table's data or leave it behind (schema only), then continue the move.")
}

func moveExcludedNames(d MoveDatabase, choice string) []string {
	out := []string{}
	for _, table := range d.Excluded {
		if table.Choice == choice {
			out = append(out, table.Name)
		}
	}
	return out
}

// moveCopyExcludedTables copies the data of the tables chosen for copying
// as a data-only dump of just those tables, after the restore of the base
// dump with digest dump. A load that finished and verified in this phase,
// for the same dump and the same set of tables, is recorded in the
// engine's checkpoint and never repeated: an attempt resumed after a
// restart of the control panel skips straight to the verification.
func (c *Control) moveCopyExcludedTables(ctx context.Context, run moveRun, i int, cp moveCheckpointRef, dump string) error {
	job := run.job()
	d := job.Databases[i]
	tables := moveExcludedNames(d, moveTableCopy)
	if left := moveExcludedNames(d, moveTableLeave); len(left) > 0 {
		run.log("Left behind as chosen (schema only, no rows): %s.", moveKeyList(left))
	}
	if len(tables) == 0 {
		return nil
	}
	chosen := slices.Sorted(slices.Values(tables))
	var saved movePostgresCheckpoint
	cp.Meta(&saved)
	if loaded := saved.Tables; loaded != nil && loaded.Dump == dump && loaded.SHA256 != "" && slices.Equal(loaded.Tables, chosen) {
		run.log("The data of %s was loaded into %s on %s before this attempt (dump %s…, %s verified); not loading it again.", moveKeyList(tables), d.TargetID, job.TargetServer, loaded.SHA256[:12], moveTableRowsList(loaded.Rows))
		return nil
	}
	source := "/v1/move/databases/" + url.PathEscape(d.SourceID)
	run.log("Dumping the data of %s, which the backups of %s leave out.", moveKeyList(tables), d.SourceID)
	run.stage(fmt.Sprintf("dumping the excluded tables of %s on %s", d.SourceID, job.SourceServer), 0, nil)
	stop := c.moveWatchDatabase(ctx, job.SourceServer, d.SourceID, d.SourceID+" (excluded tables)", run.id)
	var export agent.MoveTableDataExport
	err := c.moveCall(ctx, 6*time.Hour, job.SourceServer, http.MethodPost, source+"/table-data", agent.MoveTableDataRequest{Tables: tables}, &export)
	stop()
	if err != nil {
		return moveOn(job.SourceServer, fmt.Errorf("the excluded tables of %s could not be dumped: %w", d.SourceID, err))
	}
	if export.Size <= 0 || export.Size > moveMaxDumpBytes || !moveDigestPattern.MatchString(export.SHA256) || !moveDigestPattern.MatchString(export.ID) {
		return fmt.Errorf("the excluded tables' dump of %s has no usable size or digest", d.SourceID)
	}
	run.log("The excluded tables' dump is ready (%s, %s); streaming it to %s.", moveSize(export.Size), moveTableRowsList(export.Rows), job.TargetServer)
	var restored agent.MoveTableRows
	err = c.moveRelay(ctx, job, moveRelayStream{
		download: source + "/table-data/" + url.PathEscape(export.ID),
		upload:   "/v1/move/databases/" + url.PathEscape(d.TargetID) + "/table-data/restore?tables=" + url.QueryEscape(strings.Join(tables, ",")),
		size:     export.Size, digest: export.SHA256, mismatch: errMoveChecksum, what: "excluded tables' dump", resume: moveUploadID(job, cp.key+"/tables", export.SHA256),
		phase: fmt.Sprintf("streaming the excluded tables of %s to %s", d.SourceID, job.TargetServer),
		after: fmt.Sprintf("loading the excluded tables of %s on %s", d.SourceID, job.TargetServer),
	}, &restored)
	if err != nil {
		return fmt.Errorf("copying the excluded tables of %s failed: %w", d.SourceID, err)
	}
	mismatched := []string{}
	for _, table := range tables {
		if restored.Rows[table] != export.Rows[table] {
			mismatched = append(mismatched, fmt.Sprintf("%s (%d on the source, %d on the destination)", table, export.Rows[table], restored.Rows[table]))
		}
	}
	if len(mismatched) > 0 {
		return fmt.Errorf("row counts of the excluded tables of %s differ after the copy: %s", d.SourceID, strings.Join(mismatched, ", "))
	}
	_, err = c.updateMove(run.id, func(job *MoveJob) error {
		for j := range job.Databases[i].Excluded {
			table := &job.Databases[i].Excluded[j]
			if n, ok := restored.Rows[table.Name]; ok && table.Choice == moveTableCopy {
				count := n
				table.Copied = &count
			}
		}
		job.logf(run.step, "Copied the data of %s into %s on %s; the row counts match the dump.", moveTableRowsList(restored.Rows), d.TargetID, job.TargetServer)
		// Checkpoint the verified load with the counts, in the same write.
		saved.Tables = &moveTablesCheckpoint{Dump: dump, Export: export.ID, SHA256: export.SHA256, Tables: chosen, Rows: restored.Rows}
		return job.setCheckpointMeta(cp.key, saved)
	})
	return err
}

// moveVerifyExcludedTables compares the destination's row counts with the
// source's: copied tables match (at the final sync, with the source
// frozen, exactly; during the bulk copy a live source may have moved on)
// and tables left behind are empty.
func (c *Control) moveVerifyExcludedTables(ctx context.Context, run moveRun, i int) error {
	job := run.job()
	d := job.Databases[i]
	if len(d.Excluded) == 0 || d.TargetID == "" || d.Skipped != "" {
		return nil
	}
	names := []string{}
	for _, table := range d.Excluded {
		names = append(names, table.Name)
	}
	var target, source agent.MoveTableRows
	if err := c.moveCall(ctx, 10*time.Minute, job.TargetServer, http.MethodPost, "/v1/move/databases/"+url.PathEscape(d.TargetID)+"/row-counts", agent.MoveTableDataRequest{Tables: names}, &target); err != nil {
		return moveOn(job.TargetServer, fmt.Errorf("the destination's row counts of %s could not be read: %w", d.TargetID, err))
	}
	copied := moveExcludedNames(d, moveTableCopy)
	if len(copied) > 0 {
		if err := c.moveCall(ctx, 10*time.Minute, job.SourceServer, http.MethodPost, "/v1/move/databases/"+url.PathEscape(d.SourceID)+"/row-counts", agent.MoveTableDataRequest{Tables: copied}, &source); err != nil {
			return moveOn(job.SourceServer, fmt.Errorf("the source's row counts of %s could not be read: %w", d.SourceID, err))
		}
	}
	problems, drift := []string{}, []string{}
	for _, table := range d.Excluded {
		got := target.Rows[table.Name]
		switch {
		case table.Choice == moveTableLeave && got > 0:
			problems = append(problems, fmt.Sprintf("%s was left behind but has %d rows on the destination", table.Name, got))
		case table.Choice == moveTableCopy && got != source.Rows[table.Name]:
			drift = append(drift, fmt.Sprintf("%s (%d on the source, %d on the destination)", table.Name, source.Rows[table.Name], got))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("verification of %s failed: %s", d.SourceID, strings.Join(problems, "; "))
	}
	if len(drift) > 0 {
		if run.phase == movePhaseFinal {
			return fmt.Errorf("verification of %s failed: row counts differ for %s", d.SourceID, strings.Join(drift, ", "))
		}
		run.log("The live source changed since the dump of %s: %s; the final sync copies them again.", d.SourceID, strings.Join(drift, ", "))
		return nil
	}
	run.log("Verified %s: %d copied excluded tables match the source's row counts; %d left-behind tables are empty as expected.", d.SourceID, len(copied), len(d.Excluded)-len(copied))
	return nil
}

func moveTableRowsList(rows map[string]int64) string {
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	slices.Sort(names)
	parts := []string{}
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s: %d rows", name, rows[name]))
	}
	return moveKeyList(parts)
}

// moveLeftBehind lists every table the move left behind, for the finish
// summary.
func moveLeftBehind(job MoveJob) []string {
	out := []string{}
	for _, d := range job.Databases {
		for _, table := range d.Excluded {
			if table.Choice == moveTableLeave {
				out = append(out, d.SourceID+": "+table.Name)
			}
		}
	}
	return out
}

// moveSetExcludedChoices records the operator's choices for a database's
// excluded tables.
func moveSetExcludedChoices(job *MoveJob, database string, choices map[string]string) error {
	for i := range job.Databases {
		d := &job.Databases[i]
		if d.SourceID != database {
			continue
		}
		for name, choice := range choices {
			if choice != moveTableCopy && choice != moveTableLeave {
				return fmt.Errorf("choose copy or leave for %s", name)
			}
			j := slices.IndexFunc(d.Excluded, func(t MoveExcludedTable) bool { return t.Name == name })
			if j < 0 {
				return fmt.Errorf("%s is not a table the backups of %s leave out", name, database)
			}
			d.Excluded[j].Choice = choice
		}
		// The data step copies these tables again under the new choices.
		delete(job.Checkpoints, moveCheckpointKey(movePhaseBulk, moveEnginePostgres, database))
		if s := job.step(moveStepData); s != nil && s.Status == "done" && !moveLocks(*job) {
			job.logf(moveStepData, "Choices for the excluded tables of %s: copy %s; leave behind %s. The final sync copies accordingly.", database, moveKeyList(moveExcludedNames(*d, moveTableCopy)), moveKeyList(moveExcludedNames(*d, moveTableLeave)))
		}
		return nil
	}
	return fmt.Errorf("the move does not copy database %s", database)
}

// moveWatchDatabase mirrors an agent's progress of a database operation
// while a synchronous call to it runs.
func (c *Control) moveWatchDatabase(ctx context.Context, server, database, subject, id string) (stop func()) {
	interval := c.movePollInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
			var views []agent.DatabaseView
			if c.moveCall(ctx, 15*time.Second, server, http.MethodGet, "/v1/databases", nil, &views) == nil && ctx.Err() == nil {
				for _, v := range views {
					if v.AppID == database && v.Progress != nil {
						c.moveMirror(id, subject, server, v.Progress)
					}
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

// planMoveExcludedTables explains a database's excluded tables in the plan
// and blocks a copy the destination cannot load.
func planMoveExcludedTables(plan *MovePlan, entry MovePlanDatabase, host agent.MoveHost) {
	if len(entry.ExcludedTables) == 0 {
		return
	}
	parts := []string{}
	copies := false
	for _, table := range entry.ExcludedTables {
		parts = append(parts, fmt.Sprintf("%s (%s: %s)", table.Name, moveTableEstimate(table), map[string]string{moveTableCopy: "data copied", moveTableLeave: "left behind"}[table.Choice]))
		copies = copies || table.Choice == moveTableCopy
	}
	plan.Notes = append(plan.Notes, fmt.Sprintf("The backups of %s leave out the data of %d tables: %s. Their data is copied separately unless you leave a table behind (schema only).", entry.ID, len(parts), strings.Join(parts, "; ")))
	if copies && !host.TableData {
		plan.Blockers = append(plan.Blockers, "The destination agent cannot load the data of tables the backups leave out; upgrade it, or choose to leave those tables behind.")
	}
}

// moveTableEstimate describes a table's estimated rows and size; an unknown
// row estimate is left out rather than shown on its own.
func moveTableEstimate(table MoveExcludedTable) string {
	switch {
	case table.Rows >= 0 && table.Bytes >= 0:
		return moveRowEstimate(table.Rows) + ", " + moveSize(table.Bytes)
	case table.Bytes >= 0:
		return moveSize(table.Bytes)
	case table.Rows >= 0:
		return moveRowEstimate(table.Rows)
	}
	return "not measured"
}

func moveRowEstimate(rows int64) string {
	switch {
	case rows < 0:
		return "not measured"
	case rows >= 1_000_000:
		return fmt.Sprintf("about %.1fM rows", float64(rows)/1e6)
	case rows >= 10_000:
		return fmt.Sprintf("about %dk rows", rows/1000)
	}
	return fmt.Sprintf("about %d rows", rows)
}
