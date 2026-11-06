package control

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// TestMoveFinalSyncAppendsOnlyTheRowsAddedSinceTheBulkCopy records the
// watermark of an append-only table at the bulk copy, keeps the table
// across the final sync's base restore and appends only the rows added
// since, verified by exact totals.
func TestMoveFinalSyncAppendsOnlyTheRowsAddedSinceTheBulkCopy(t *testing.T) {
	fx := newMoveFixture(t)
	tables := fx.excludedSource()
	fx.a.with(func() {
		tables.keys = map[string]agent.MoveTableKey{"public.events": {Column: "occurred_at", Kind: "timestamptz", Watermark: "2026-11-06 10:00:00+00", Updates: 0, Deletes: 0}}
	})
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "excluded_tables": map[string]map[string]string{"shop": {"public.audit_log": "leave"}}})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	events := job.Databases[0].Excluded[0]
	if events.Name != "public.events" || events.Exported == nil || events.Exported.Key == nil || events.Exported.Key.Watermark != "2026-11-06 10:00:00+00" || events.Copied == nil || *events.Copied != 39_900 {
		t.Fatalf("the bulk copy records the watermark: %+v", events)
	}
	// The source keeps appending until the final sync freezes it.
	fx.a.with(func() {
		tables.source["public.events"] = 40_150
		tables.deltaRows, tables.maximum = 250, "2026-11-06 12:30:00+00"
	})
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if !slices.Equal(tables.restored, []string{"public.events"}) || !slices.Equal(tables.appended, []string{"public.events after 2026-11-06 10:00:00+00"}) {
			t.Fatalf("the final sync appends instead of loading again: restored %v, appended %v", tables.restored, tables.appended)
		}
		if !slices.Equal(fx.b.keeps, []string{"", "public.events"}) {
			t.Fatalf("the final sync's base restore keeps the table: %q", fx.b.keeps)
		}
	})
	fx.a.with(func() {
		if len(tables.deltas) != 1 || tables.deltas[0].Table != "public.events" || tables.deltas[0].Key.Column != "occurred_at" || tables.deltas[0].Key.Watermark != "2026-11-06 10:00:00+00" {
			t.Fatalf("delta requests %+v", tables.deltas)
		}
	})
	for _, want := range []string{
		"beta keeps public.events across the restore; only the rows added since the bulk copy follow.",
		"Copying the rows added to public.events since the bulk copy (occurred_at after 2026-11-06 10:00:00+00) instead of the whole table.",
		"Appended 250 rows to public.events on beta; 40150 rows and max occurred_at match the source exactly.",
		"1 copied excluded tables match the source's row counts",
	} {
		if !moveLogged(job, moveStepFinalSync, want) {
			t.Fatalf("%q not logged: %+v", want, job.step(moveStepFinalSync).Log)
		}
	}
	events = job.Databases[0].Excluded[0]
	if events.Copied == nil || *events.Copied != 40_150 || events.Exported == nil || events.Exported.Key == nil || events.Exported.Key.Watermark != "2026-11-06 12:30:00+00" {
		t.Fatalf("the watermark moves on: %+v", events)
	}
}

// TestMoveFinalSyncCopiesAChangedTableInFullAgain falls back to the full
// copy of a table that was updated since the bulk copy, and says why.
func TestMoveFinalSyncCopiesAChangedTableInFullAgain(t *testing.T) {
	fx := newMoveFixture(t)
	tables := fx.excludedSource()
	fx.a.with(func() {
		tables.keys = map[string]agent.MoveTableKey{"public.events": {Column: "id", Kind: "integer", Watermark: "39900"}}
	})
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "excluded_tables": map[string]map[string]string{"shop": {"public.audit_log": "leave"}}})
	if status != 202 {
		t.Fatal(status)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.a.with(func() { tables.fallback = "3 rows were updated since the bulk copy" })
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if !slices.Equal(tables.restored, []string{"public.events", "public.events"}) || len(tables.appended) != 0 {
			t.Fatalf("restored %v, appended %v", tables.restored, tables.appended)
		}
	})
	if !moveLogged(job, moveStepFinalSync, "Copying public.events in full again: 3 rows were updated since the bulk copy.") {
		t.Fatalf("fallback not logged: %+v", job.step(moveStepFinalSync).Log)
	}
	if !moveLogged(job, moveStepFinalSync, "Copied the data of public.events: 39900 rows into") {
		t.Fatalf("full copy not logged: %+v", job.step(moveStepFinalSync).Log)
	}
}

// TestMoveFinalSyncFallsBackWhenTheKeptRestoreFails restores the database
// in full and copies the excluded tables again when the destination cannot
// restore around the kept tables, instead of failing the move.
func TestMoveFinalSyncFallsBackWhenTheKeptRestoreFails(t *testing.T) {
	fx := newMoveFixture(t)
	tables := fx.excludedSource()
	fx.a.with(func() {
		tables.keys = map[string]agent.MoveTableKey{"public.events": {Column: "occurred_at", Kind: "timestamptz", Watermark: "2026-11-06T10:00:00.000000"}}
	})
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "excluded_tables": map[string]map[string]string{"shop": {"public.audit_log": "leave"}}})
	if status != 202 {
		t.Fatal(status)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() { fx.b.failKeep = true })
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if !slices.Equal(fx.b.keeps, []string{"", "public.events", ""}) || len(fx.b.restores["shop"]) != 2 {
			t.Fatalf("keeps %q, restores %d", fx.b.keeps, len(fx.b.restores["shop"]))
		}
		if !slices.Equal(tables.restored, []string{"public.events", "public.events"}) || len(tables.appended) != 0 {
			t.Fatalf("restored %v, appended %v", tables.restored, tables.appended)
		}
	})
	fx.a.with(func() {
		if len(tables.deltas) != 0 {
			t.Fatalf("no delta after a full restore: %+v", tables.deltas)
		}
	})
	if !moveLogged(job, moveStepFinalSync, "Restoring shop while keeping public.events failed (") || !moveLogged(job, moveStepFinalSync, "restoring it in full and copying the excluded tables again.") {
		t.Fatalf("fallback not logged: %+v", job.step(moveStepFinalSync).Log)
	}
	if !moveLogged(job, moveStepFinalSync, "Copied the data of public.events: 39900 rows into") {
		t.Fatalf("full copy not logged: %+v", job.step(moveStepFinalSync).Log)
	}
}

// TestMovePlanEstimatesAnIncrementalFinalSync labels the final-sync figure
// of a database whose excluded table has an incremental key with the
// source's insert rate.
func TestMovePlanEstimatesAnIncrementalFinalSync(t *testing.T) {
	fx := newMoveFixture(t)
	fx.excludedSource()
	fx.a.with(func() {
		fx.a.source.Databases[0].ExcludedTables[0].Key = &agent.MoveTableKey{Column: "occurred_at", Kind: "timestamptz", RowsPerHour: 1200, RateBasis: "the source's insert statistics over 9 days"}
	})
	plan, _, err := fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta"})
	if err != nil || plan.Estimate == nil {
		t.Fatalf("plan: %v %+v", err, plan.Estimate)
	}
	want := "Final sync of shop: only the rows added since the bulk copy for public.events (about 1200 rows/hour from the source's insert statistics over 9 days); the other tables in full."
	if !slices.Contains(plan.Estimate.Notes, want) {
		t.Fatalf("notes %q", plan.Estimate.Notes)
	}
	i := slices.IndexFunc(plan.Estimate.Engines, func(e MoveEngineEstimate) bool { return e.Kind == moveEnginePostgres })
	if entry := plan.Estimate.Engines[i]; i < 0 || entry.FinalBasis == "" || !(entry.FinalSeconds < entry.BulkSeconds) || plan.Databases[0].ExcludedTables[0].Key == nil {
		t.Fatalf("engine estimate %+v", plan.Estimate.Engines)
	}
	// The sizing itself: a 27 MiB table of 39,900 rows growing by 1,200
	// rows an hour sends about 2,400 rows' worth over a two-hour window.
	d := MoveDatabase{Excluded: plan.Databases[0].ExcludedTables}
	full, delta, basis := moveIncrementalEstimate(d, 2*time.Hour)
	if full != 27<<20 || int64(delta) != 1_702_950 || !strings.HasPrefix(basis, "only the rows added since the bulk copy for public.events") {
		t.Fatalf("full %d delta %.0f basis %q", full, delta, basis)
	}
}

// TestMoveFinalSyncDerivesAMissingKeyFromTheLoadedRows keeps a table whose
// bulk copy recorded no incremental key (an older agent made it): the
// source names the key, the destination's highest value becomes the
// watermark once the source holds exactly the loaded rows up to it.
func TestMoveFinalSyncDerivesAMissingKeyFromTheLoadedRows(t *testing.T) {
	fx := newMoveFixture(t)
	tables := fx.excludedSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "excluded_tables": map[string]map[string]string{"shop": {"public.audit_log": "leave"}}})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if events := job.Databases[0].Excluded[0]; events.Exported == nil || events.Exported.Key != nil || events.Copied == nil || *events.Copied != 39_900 {
		t.Fatalf("the bulk copy without keys: %+v", events)
	}
	fx.a.with(func() {
		tables.detected = &agent.MoveTableKey{Column: "id", Kind: "integer", Watermark: "40150"}
		tables.source["public.events"] = 40_150
		tables.upTo = 39_900 // rows up to the destination's highest id
		tables.deltaRows, tables.maximum = 250, "40150"
	})
	fx.b.with(func() { tables.loadedMax = "39900" })
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if !slices.Equal(tables.appended, []string{"public.events after 39900"}) || !slices.Equal(fx.b.keeps, []string{"", "public.events"}) {
			t.Fatalf("the final sync keeps the table and appends: appended %v keeps %q", tables.appended, fx.b.keeps)
		}
	})
	for _, want := range []string{
		"The bulk copy of public.events recorded no incremental key; derived id up to 39900 from the 39900 rows on beta, which match the source exactly. Only the rows after it follow.",
		"Appended 250 rows to public.events on beta; 40150 rows and max id match the source exactly.",
	} {
		if !moveLogged(job, moveStepFinalSync, want) {
			t.Fatalf("%q not logged: %+v", want, job.step(moveStepFinalSync).Log)
		}
	}
}

// TestMoveFinalSyncCopiesInFullWhenTheLoadedRowsDoNotMatch copies a table
// in full when the source holds a different number of rows up to the
// destination's watermark.
func TestMoveFinalSyncCopiesInFullWhenTheLoadedRowsDoNotMatch(t *testing.T) {
	fx := newMoveFixture(t)
	tables := fx.excludedSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "excluded_tables": map[string]map[string]string{"shop": {"public.audit_log": "leave"}}})
	if status != 202 {
		t.Fatal(status)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.a.with(func() {
		tables.detected = &agent.MoveTableKey{Column: "id", Kind: "integer", Watermark: "40150"}
		tables.source["public.events"] = 40_150
		tables.upTo = 39_890
	})
	fx.b.with(func() { tables.loadedMax = "39900" })
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if len(tables.appended) != 0 || !slices.Equal(fx.b.keeps, []string{"", ""}) {
			t.Fatalf("nothing kept or appended: appended %v keeps %q", tables.appended, fx.b.keeps)
		}
	})
	if want := "public.events is copied in full at the final sync: the source holds 39890 rows up to id 39900 but the destination loaded 39900; rows changed in other ways than appends."; !moveLogged(job, moveStepFinalSync, want) {
		t.Fatalf("%q not logged: %+v", want, job.step(moveStepFinalSync).Log)
	}
}
