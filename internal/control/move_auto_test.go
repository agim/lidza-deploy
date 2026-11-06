package control

import (
	"context"
	"slices"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestMoveAutoCutoverFinalSyncsAndFinishesWhenDNSResolves(t *testing.T) {
	fx := newMoveFixture(t)
	rep := fx.replicationSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatal(status)
	}
	// Every database replicates continuously: the option is on by default.
	if !view.Plan.AutoCutover || !view.AutoCutover {
		t.Fatalf("automatic cutover not on by default: plan %v, job %v", view.Plan.AutoCutover, view.AutoCutover)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if !slices.Contains(moveActions(job), "auto-cutover") {
		t.Fatalf("actions %v", moveActions(job))
	}
	if fx.a.called("PUT /v1/apps/shop/maintenance") {
		t.Fatal("the source must keep serving until DNS points to the destination")
	}
	rep.with(func() { rep.sourceLSN = "0/5000900" })
	fx.resolver.set("shop.example.com", moveTestTargetIP)
	fx.resolver.set("www.shop.example.com", moveTestTargetIP)
	fx.probe.set(true)
	fx.c.moveTick(context.Background())
	job = waitMove(t, fx.c, view.ID, moveStateFinished)
	if !job.FinalSynced || !job.Switched || job.step(moveStepFinalSync).Status != "done" || job.step(moveStepFinish).Status != "done" {
		t.Fatalf("the move did not sync and finish by itself: %+v", job.Steps)
	}
	for _, want := range []string{
		"Starting the final sync automatically: DNS points to the destination.",
		"Finishing the move automatically: the final sync is done and DNS points to the destination.",
	} {
		if !moveLogged(job, moveStepCutover, want) {
			t.Fatalf("%q not logged: %+v", want, job.step(moveStepCutover).Log)
		}
	}
	if live := fx.c.data.Apps["shop"]; live.ServerID != "beta" {
		t.Fatalf("live record not switched: %+v", live)
	}
	if !fx.a.called("DELETE /v1/apps/shop") {
		t.Fatal("the source was not retired")
	}
}

func TestMoveAutoCutoverStopsAfterAFailedFinalSyncAndCanBeSwitchedOn(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	// A dumped database: off by default, requested for this move.
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "max_attempts": 1})
	if status != 202 || view.AutoCutover {
		t.Fatalf("%d: a dumped database keeps the cutover manual by default: %v", status, view.AutoCutover)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	if code := moveAction(t, fx.c, view.ID, "auto-cutover", nil); code != 409 {
		t.Fatalf("toggle without a value: %d", code)
	}
	if code := moveAction(t, fx.c, view.ID, "auto-cutover", map[string]any{"enabled": true}); code != 202 {
		t.Fatalf("toggle: %d", code)
	}
	job, _ := fx.c.moveJob(view.ID)
	if !job.AutoCutover || !moveLogged(job, moveStepCutover, "Automatic cutover on") {
		t.Fatalf("toggle not recorded: %v %+v", job.AutoCutover, job.step(moveStepCutover).Log)
	}
	// The source serves corrupt bytes: the automatic final sync fails.
	fx.a.with(func() { fx.a.corrupt = true })
	fx.resolver.set("shop.example.com", moveTestTargetIP, moveTestTargetIP6)
	fx.resolver.set("www.shop.example.com", moveTestTargetIP)
	fx.probe.set(true)
	fx.c.moveTick(context.Background())
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	if job.Failure == nil || job.FinalSynced || job.AutoCutover {
		t.Fatalf("a failed automatic final sync must stop the automation: failure %+v, synced %v, auto %v", job.Failure, job.FinalSynced, job.AutoCutover)
	}
	if !moveLogged(job, moveStepCutover, "Starting the final sync automatically") || !moveLogged(job, moveStepFinalSync, "The automatic cutover is off after this failure") {
		t.Fatalf("logs: %+v %+v", job.step(moveStepCutover).Log, job.step(moveStepFinalSync).Log)
	}
	// DNS checks no longer start anything; the operator retries by hand.
	fx.c.moveTick(context.Background())
	job = waitMove(t, fx.c, view.ID, moveStateReady)
	if job.FinalSynced || fx.c.data.Apps["shop"].ServerID != "alpha" {
		t.Fatalf("the automation continued: %+v", job)
	}
	fx.a.with(func() { fx.a.corrupt = false })
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatalf("manual final sync: %d", code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateCutover, moveStateReady)
	if !job.FinalSynced {
		t.Fatalf("final sync: %+v", job.step(moveStepFinalSync).Log)
	}
	// Switched on again while DNS already points here, the finish runs at once.
	if code := moveAction(t, fx.c, view.ID, "auto-cutover", map[string]any{"enabled": true}); code != 202 {
		t.Fatalf("toggle: %d", code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateFinished)
	if !moveLogged(job, moveStepCutover, "Finishing the move automatically: the final sync is done and DNS points to the destination.") {
		t.Fatalf("finish not logged: %+v", job.step(moveStepCutover).Log)
	}
	if code := moveAction(t, fx.c, view.ID, "auto-cutover", map[string]any{"enabled": false}); code != 409 {
		t.Fatalf("toggle after the finish: %d", code)
	}
}

func TestMovePlanDefaultsTheAutoCutoverToReplicatedDatabases(t *testing.T) {
	continuous := MovePlan{Databases: []MovePlanDatabase{{ID: "shop", Action: "copy", Replication: moveReplicationContinuous}, {ID: "empty", Action: "skip_empty"}}}
	planMoveAutoCutover(&continuous, nil)
	if !continuous.AutoCutover || len(continuous.Notes) != 0 {
		t.Fatalf("%+v", continuous)
	}
	mixed := MovePlan{Databases: []MovePlanDatabase{{ID: "shop", Action: "copy", Replication: moveReplicationContinuous}, {ID: "analytics", Action: "copy", Replication: moveReplicationDump}}}
	planMoveAutoCutover(&mixed, nil)
	if mixed.AutoCutover {
		t.Fatal("a dumped database keeps the cutover manual")
	}
	none := MovePlan{}
	planMoveAutoCutover(&none, nil)
	if none.AutoCutover {
		t.Fatal("no copied database keeps the cutover manual")
	}
	// The request's choice wins; published ports add the note.
	on := true
	ports := MovePlan{PublicPorts: []agent.StackPort{{Port: 25, Protocol: "tcp"}}}
	planMoveAutoCutover(&ports, &on)
	if !ports.AutoCutover || !slices.Contains(ports.Notes, moveAutoCutoverNote) {
		t.Fatalf("%+v", ports)
	}
}
