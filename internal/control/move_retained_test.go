package control

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// A destination that keeps the data an earlier attempt copied (a retained
// resource whose move never switched) is cleared by Prepare on its own: the
// copy is removed without a backup and the application is created.
func TestMovePrepareRemovesTheCopyOfAnEarlierAttempt(t *testing.T) {
	fx := newMoveFixture(t)
	fx.nativeMoveSource()
	fx.b.with(func() {
		fx.b.retained = []agent.RetainedResourceView{{ID: "r-copy", AppID: "shop", Kind: "application", Copy: true, Origin: &agent.RetainedOrigin{MoveID: "m-earlier", SourceServer: "alpha"},
			Databases: []agent.RetainedDatabaseView{{ID: "shop-db0001"}}}}
	})
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if len(fx.b.retainedRemovals) != 1 || fx.b.retainedRemovals[0].Confirm != "r-copy" || fx.b.retainedRemovals[0].FinalBackup {
			t.Fatalf("retained removals %+v", fx.b.retainedRemovals)
		}
		if fx.b.native == nil || len(fx.b.retained) != 0 {
			t.Fatalf("destination not created: native %v retained %+v", fx.b.native, fx.b.retained)
		}
	})
	if !moveLogContains(job, "Removed the data copied by the earlier attempt m-earlier on Beta") || !moveLogContains(job, "Created the application on beta") {
		t.Fatalf("prepare log %+v", stepLog(job, moveStepPrepare))
	}
}

// Retained data of an application that really ran on the destination stops
// Prepare for the operator. The move panel's action removes it with the
// same confirmation and final-backup rules as the retained-resources page,
// and the move continues once the removal finished.
func TestMovePrepareStopsForRetainedDataUntilTheOperatorRemovesIt(t *testing.T) {
	fx := newMoveFixture(t)
	fx.nativeMoveSource()
	backup := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	fx.b.with(func() {
		fx.b.retained = []agent.RetainedResourceView{{ID: "r-old", AppID: "shop", Kind: "application", FinalBackupRequired: true,
			Databases: []agent.RetainedDatabaseView{{ID: "shop-db0001", LastLocal: &agent.BackupRecord{ID: "b1", Created: backup}}}}}
	})
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateFailed)
	f := job.Failure
	if f == nil || f.Step != moveStepPrepare || f.Class != moveClassOperator || f.Code != "retained_data" || f.Retained == nil || f.Server != "beta" {
		t.Fatalf("failure %+v", f)
	}
	if f.Retained.ID != "r-old" || f.Retained.Server != "beta" || !f.Retained.FinalBackupRequired || f.Retained.Label != "Remove retained data of shop on Beta (last backup 2026-11-05 10:00 UTC)" {
		t.Fatalf("retained %+v", f.Retained)
	}
	if actions := moveActions(job); !slices.Contains(actions, "remove-retained") || !slices.Contains(actions, "continue") {
		t.Fatalf("actions %v", actions)
	}
	fx.b.with(func() {
		if fx.b.native != nil || len(fx.b.retainedRemovals) != 0 {
			t.Fatal("nothing is created or removed before the operator decides")
		}
	})
	// The same guards as the retained-resources page.
	if code := moveAction(t, fx.c, view.ID, "remove-retained", map[string]any{"confirm": "r-ol", "final_backup": true}); code != 409 {
		t.Fatalf("unconfirmed: %d", code)
	}
	if code := moveAction(t, fx.c, view.ID, "remove-retained", map[string]any{"confirm": "r-old"}); code != 409 {
		t.Fatalf("without the required final backup: %d", code)
	}
	if code := moveAction(t, fx.c, view.ID, "remove-retained", map[string]any{"confirm": "r-old", "final_backup": true}); code != 202 {
		t.Fatalf("remove-retained: %d", code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if len(fx.b.retainedRemovals) != 1 || !fx.b.retainedRemovals[0].FinalBackup || fx.b.native == nil || len(fx.b.retained) != 0 {
			t.Fatalf("removals %+v native %v retained %+v", fx.b.retainedRemovals, fx.b.native, fx.b.retained)
		}
	})
	if !moveLogContains(job, "Removing the retained data of shop on Beta (r-old) after a final backup") || !moveLogContains(job, "Created the application on beta") {
		t.Fatalf("prepare log %+v", stepLog(job, moveStepPrepare))
	}
	if job.Failure != nil || slices.Contains(moveActions(job), "remove-retained") {
		t.Fatalf("the failure must be cleared: %+v", job.Failure)
	}
}

// Aborting before the switch removes the destination with the data copied
// there: the copy is not retained.
func TestMoveAbortBeforeSwitchDeletesTheCopiedData(t *testing.T) {
	fx := newMoveFixture(t)
	fx.nativeMoveSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	if code := moveAction(t, fx.c, view.ID, "abort", nil); code != 202 {
		t.Fatalf("abort: %d", code)
	}
	job := waitMove(t, fx.c, view.ID, moveStateAborted)
	fx.b.with(func() {
		remove, ok := fx.b.removals["/v1/native/apps/shop?purge=1"]
		if !ok || !remove.DeleteData || remove.Origin == nil || remove.Origin.MoveID != view.ID || remove.Origin.SourceServer != "alpha" || remove.Origin.Switched {
			t.Fatalf("removal request %+v (%v)", remove, ok)
		}
		if len(fx.b.retained) != 0 || fx.b.native != nil {
			t.Fatalf("the copy was retained: %+v", fx.b.retained)
		}
	})
	if !moveLogContains(job, "Removed the application from beta with the data copied there") {
		t.Fatalf("abort log %+v", stepLog(job, moveStepPlan))
	}
}

// Once a move switched, the destination's data is the application's own:
// removing the application there retains it, with the move recorded as its
// origin, and the source's retirement at the finish records the same.
func TestMoveRemovalsAfterTheSwitchRetainData(t *testing.T) {
	fx := newMoveFixture(t)
	fx.nativeMoveSource()
	fx.b.native = &agent.NativeStatus{Application: agent.NativeApplication{ID: "shop", Name: "shop"}}
	job := MoveJob{ID: "m-switched", AppID: "shop", SourceServer: "alpha", TargetServer: "beta", TargetRuntime: moveRuntimeNative, TargetCreated: true, Switched: true}
	ctx := context.Background()
	if gone, err := fx.c.moveRemoveTarget(ctx, moveRequestFor(ctx), job); err != nil || gone {
		t.Fatalf("remove target: gone %v err %v", gone, err)
	}
	job.TargetRuntime = moveRuntimeContainer
	if _, err := fx.c.moveRemoveTarget(ctx, moveRequestFor(ctx), job); err != nil {
		t.Fatal(err)
	}
	fx.b.with(func() {
		for _, path := range []string{"/v1/native/apps/shop?purge=1", "/v1/apps/shop"} {
			remove, ok := fx.b.removals[path]
			if !ok || remove.DeleteData || remove.Origin == nil || remove.Origin.MoveID != "m-switched" || !remove.Origin.Switched {
				t.Fatalf("%s removal request %+v (%v)", path, remove, ok)
			}
		}
		if len(fx.b.retained) != 2 || fx.b.retained[0].Copy || fx.b.retained[0].Origin == nil || !fx.b.retained[0].Origin.Switched {
			t.Fatalf("retained %+v", fx.b.retained)
		}
	})

	// A finished move retires the source with a switched origin.
	fx = newMoveFixture(t)
	fx.containerSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.resolver.set("shop.example.com", moveTestTargetIP, moveTestTargetIP6)
	fx.resolver.set("www.shop.example.com", moveTestTargetIP)
	fx.probe.set(true)
	fx.c.moveTick(context.Background())
	waitMove(t, fx.c, view.ID, moveStateReady)
	if code := moveAction(t, fx.c, view.ID, "finish", nil); code != 202 {
		t.Fatalf("finish: %d", code)
	}
	waitMove(t, fx.c, view.ID, moveStateFinished)
	fx.a.with(func() {
		remove, ok := fx.a.removals["/v1/apps/shop"]
		if !ok || remove.DeleteData || remove.Origin == nil || remove.Origin.MoveID != view.ID || !remove.Origin.Switched {
			t.Fatalf("source retirement %+v (%v)", remove, ok)
		}
	})
}

func stepLog(job MoveJob, key string) []string {
	var out []string
	for _, step := range job.Steps {
		if step.Key == key {
			for _, line := range step.Log {
				out = append(out, line.Message)
			}
		}
	}
	return out
}
