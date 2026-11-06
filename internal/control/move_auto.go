package control

import (
	"context"
	"errors"
)

// Automatic cutover. Between DNS resolving to the destination and the final
// sync, the destination serves the bulk copy, and what it receives
// meanwhile (writes, mail delivered to published ports) is replaced by the
// final sync. The length of that window depends on the operator pressing
// Final sync. With the option on, the panel's DNS check starts the final
// sync as soon as every hostname points to the destination and HTTPS works
// there, and finishes the move once the final sync succeeded, so the window
// closes within a minute of the DNS change.

// moveAutoCutoverNote is the plan's note for applications that publish
// ports.
const moveAutoCutoverNote = "Connections to the published ports reach the destination as soon as DNS changes; with the automatic cutover the final sync follows within a minute."

// planMoveAutoCutover records the option: the request's choice, else the
// default. The default is on when every database the move copies
// replicates continuously, because the automatic lock then lasts seconds;
// with a dumped database the lock lasts as long as its restore, which the
// operator schedules by hand.
func planMoveAutoCutover(plan *MovePlan, requested *bool) {
	if requested != nil {
		plan.AutoCutover = *requested
	} else {
		plan.AutoCutover = moveAutoCutoverDefault(*plan)
	}
	if len(plan.PublicPorts) > 0 {
		plan.Notes = append(plan.Notes, moveAutoCutoverNote)
	}
}

// moveAutoCutoverDefault reports whether every database the plan copies
// replicates continuously; a plan without a copied database stays manual.
func moveAutoCutoverDefault(plan MovePlan) bool {
	copied := 0
	for _, d := range plan.Databases {
		if d.Action != "copy" {
			continue
		}
		if d.Replication != moveReplicationContinuous {
			return false
		}
		copied++
	}
	return copied > 0
}

// moveSetAutoCutover switches the option while the move waits at the
// cutover. Switched on while DNS already points to the destination, the
// final sync starts at once.
func (c *Control) moveSetAutoCutover(ctx context.Context, id string, enabled bool) (MoveJob, error) {
	job, err := c.updateMove(id, func(job *MoveJob) error {
		if job.State != moveStateCutover && job.State != moveStateReady {
			return errors.New("the automatic cutover is set while the move waits for DNS")
		}
		if job.AutoCutover == enabled {
			return nil
		}
		job.AutoCutover = enabled
		if enabled {
			job.logf(moveStepCutover, "Automatic cutover on: the final sync and the finish start by themselves once DNS points to the destination and HTTPS works there.")
		} else {
			job.logf(moveStepCutover, "Automatic cutover off: the final sync and the finish wait for the operator.")
		}
		return nil
	})
	if err != nil {
		return job, err
	}
	if enabled && job.State == moveStateReady {
		c.moveAutoCutover(ctx, id)
		job, _ = c.moveJob(id)
	}
	return job, nil
}

// moveAutoCutover runs the next automatic step of a move whose DNS check
// found it ready: the final sync when it has not run yet, else the finish.
// The checks are the manual actions' (move.go); the lock estimate counts as
// confirmed by the option. A move that cannot continue by itself (excluded
// tables without a choice) switches the option off and says so, instead of
// repeating the message at every DNS check.
func (c *Control) moveAutoCutover(ctx context.Context, id string) {
	var action string
	job, err := c.updateMove(id, func(job *MoveJob) error {
		if job.State != moveStateReady || !job.AutoCutover {
			return errMoveUnchanged
		}
		if !job.FinalSynced && moveCanFinalSync(*job) {
			for _, d := range job.Databases {
				for _, table := range d.Excluded {
					if table.Choice == "" {
						job.AutoCutover = false
						job.logf(moveStepCutover, "The final sync cannot start automatically: choose whether to copy or leave behind the data of %s in %s first, then run it by hand or switch the automatic cutover on again.", table.Name, d.SourceID)
						return nil
					}
				}
			}
			action = "final_sync"
			estimate := moveLockEstimate(*job)
			job.logf(moveStepCutover, "Starting the final sync automatically: DNS points to the destination. The application is locked for about %s (%s).", moveSeconds(estimate.Seconds), estimate.Basis)
			job.State = moveStateSyncing
			job.Error, job.Failure = "", nil
			job.clearMoveCheckpoints(movePhaseFinal)
			job.setStep(moveStepFinalSync, "pending")
			return nil
		}
		action = "finish"
		if job.FinalSynced {
			job.logf(moveStepCutover, "Finishing the move automatically: the final sync is done and DNS points to the destination.")
		} else {
			job.logf(moveStepCutover, "Finishing the move automatically: DNS points to the destination and there is nothing to synchronize.")
		}
		job.State = moveStateFinishing
		job.Error = ""
		return nil
	})
	if err != nil || action == "" {
		return
	}
	moveAuditMeta(ctx, job, action, false, map[string]string{"automatic": "true"})
	c.startMoveRunner(ctx, id)
}
