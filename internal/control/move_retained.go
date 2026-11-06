package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Retained data and moves.
//
// A move's destination holds a copy of the source's data until the move
// switches. When such a destination is removed (an abort, or the operator
// removing the application) the copy is not worth keeping: the abort
// deletes it with the application, and a retained resource that still
// records a never-switched origin is removed by the next attempt's Prepare
// step without a backup. Retained data of an application that really ran on
// the destination (a switched move, or an unknown origin) stops Prepare with
// an operator failure; the move panel then offers the same guarded removal
// as the retained-resources page (confirmation and an optional final
// backup), after which the move continues.

// MoveRetainedData describes the retained resource that blocks a move's
// Prepare step, for the panel's removal action.
type MoveRetainedData struct {
	ID        string `json:"id"`
	Server    string `json:"server"`
	AppID     string `json:"app_id"`
	Databases int    `json:"databases"`
	// LastBackup is the newest backup of any of its databases; Offsite
	// says whether that backup is off-site.
	LastBackup *time.Time `json:"last_backup,omitempty"`
	Offsite    bool       `json:"offsite,omitempty"`
	// FinalBackupRequired is set when a database has no verified backup
	// taken after the application stopped writing.
	FinalBackupRequired bool `json:"final_backup_required"`
	// Label is the action's wording in the panel.
	Label string `json:"label"`
}

// moveRetainedError is the operator failure of a Prepare step that met
// retained data; it carries what the panel's action needs.
type moveRetainedError struct {
	retained MoveRetainedData
	err      error
}

func (e *moveRetainedError) Error() string { return e.err.Error() }
func (e *moveRetainedError) Unwrap() error { return e.err }

// moveRetainedOrigin is what a move records on the resources its removals
// leave behind.
func moveRetainedOrigin(job MoveJob, switched bool) *agent.RetainedOrigin {
	return &agent.RetainedOrigin{MoveID: job.ID, SourceServer: job.SourceServer, Switched: switched}
}

// moveRetainedRows lists the destination's retained resources of the
// application's id. An agent without the route has none.
func (c *Control) moveRetainedRows(r *http.Request, job MoveJob) ([]agent.RetainedResourceView, error) {
	var rows []agent.RetainedResourceView
	if err := c.agentCall(r, job.TargetServer, http.MethodGet, "/v1/retained-resources", nil, &rows); err != nil {
		var remote *agentHTTPError
		if errors.As(err, &remote) && remote.status == http.StatusNotFound && remote.message == "" {
			return nil, nil
		}
		return nil, err
	}
	out := rows[:0]
	for _, row := range rows {
		if row.AppID == job.AppID {
			out = append(out, row)
		}
	}
	return out, nil
}

// moveClearRetained makes room on the destination before the application
// is created there: copies left by earlier never-switched attempts are
// removed, removals in progress are awaited, and any other retained data
// of the id stops the step for the operator.
func (c *Control) moveClearRetained(ctx context.Context, job MoveJob) error {
	r := moveRequestFor(ctx)
	requested := map[string]string{} // retained id → the move that copied it
	waiting := map[string]bool{}
	return c.movePoll(ctx, 30*time.Minute, func() (bool, error) {
		rows, err := c.moveRetainedRows(r, job)
		if err != nil {
			return false, moveOn(job.TargetServer, err)
		}
		present := map[string]bool{}
		for _, row := range rows {
			present[row.ID] = true
			switch {
			case row.Removing:
				if !waiting[row.ID] {
					waiting[row.ID] = true
					c.moveLog(job.ID, moveStepPrepare, "Waiting for the removal of the retained data of %s on %s (%s).", job.AppID, c.serverName(job.TargetServer), row.ID)
				}
				return false, nil
			case requested[row.ID] != "":
				// Our own removal ended without removing it.
				return false, moveOn(job.TargetServer, fmt.Errorf("removing the data copied by the earlier attempt %s failed: %s", requested[row.ID], row.Error))
			case row.Copy:
				if err := c.agentCall(r, job.TargetServer, http.MethodDelete, "/v1/retained-resources/"+url.PathEscape(row.ID), agent.RetainedRemoveRequest{Confirm: row.ID}, nil); err != nil {
					return false, moveOn(job.TargetServer, fmt.Errorf("the destination could not remove the data copied by the earlier attempt %s: %s", row.Origin.MoveID, moveAgentError(err)))
				}
				requested[row.ID] = row.Origin.MoveID
				return false, nil
			default:
				return false, c.moveRetainedStop(job, row)
			}
		}
		for id, moveID := range requested {
			if !present[id] {
				c.moveLog(job.ID, moveStepPrepare, "Removed the data copied by the earlier attempt %s on %s.", moveID, c.serverName(job.TargetServer))
				delete(requested, id)
			}
		}
		return true, nil
	})
}

// moveRetainedStop is the operator failure for retained data that is not a
// copy: the application's own data on the destination.
func (c *Control) moveRetainedStop(job MoveJob, row agent.RetainedResourceView) error {
	data := MoveRetainedData{ID: row.ID, Server: job.TargetServer, AppID: row.AppID, Databases: len(row.Databases), FinalBackupRequired: row.FinalBackupRequired}
	for _, d := range row.Databases {
		for _, b := range []*agent.BackupRecord{d.LastLocal, d.LastOffsite} {
			if b != nil && (data.LastBackup == nil || b.Created.After(*data.LastBackup)) {
				created := b.Created
				data.LastBackup, data.Offsite = &created, b.Offsite
			}
		}
	}
	last := "no backup"
	if data.LastBackup != nil {
		last = "last backup " + data.LastBackup.UTC().Format("2006-01-02 15:04 UTC")
		if data.Offsite {
			last += ", off-site"
		}
	}
	name := c.serverName(job.TargetServer)
	data.Label = fmt.Sprintf("Remove retained data of %s on %s (%s)", row.AppID, name, last)
	why := "an application with this id ran there before"
	if row.Origin != nil && row.Origin.Switched {
		why = "move " + row.Origin.MoveID + " switched to it"
	}
	message := fmt.Sprintf("%s keeps retained data of %s (%s): %s.", name, row.AppID, row.ID, why)
	remediation := fmt.Sprintf("Remove the retained data of %s on %s from this move (a final backup is taken when you ask for it%s), or remove it under the server's retained resources; the move then continues.", row.AppID, name, map[bool]string{true: ", and required here", false: ""}[row.FinalBackupRequired])
	return &moveRetainedError{retained: data, err: moveOn(job.TargetServer, moveOperatorError("retained_data", message, remediation))}
}

// moveRemoveRetainedAction removes the retained data that stopped the
// move's Prepare step, on the operator's confirmation, and lets the move
// continue; Prepare waits for the removal before creating the application.
func (c *Control) moveRemoveRetainedAction(r *http.Request, job MoveJob, confirm string, finalBackup bool) (MoveJob, error) {
	if job.State != moveStateFailed || job.Failure == nil || job.Failure.Retained == nil {
		return job, errors.New("this move is not stopped by retained data")
	}
	held := *job.Failure.Retained
	if confirm != held.ID {
		return job, errors.New("type the retained resource id to confirm its removal")
	}
	if held.FinalBackupRequired && !finalBackup {
		return job, errors.New("a final backup is required: no verified backup was taken after the application stopped writing")
	}
	err := c.agentCall(r, held.Server, http.MethodDelete, "/v1/retained-resources/"+url.PathEscape(held.ID), agent.RetainedRemoveRequest{Confirm: held.ID, FinalBackup: finalBackup}, nil)
	if err != nil {
		var refused *agentHTTPError
		if errors.As(err, &refused) && refused.status == http.StatusNotFound {
			// Removed meanwhile: Prepare finds nothing and continues.
			err = nil
		} else {
			return job, fmt.Errorf("%s could not start the removal: %s", c.serverName(held.Server), moveAgentError(err))
		}
	}
	return c.updateMove(job.ID, func(job *MoveJob) error {
		backup := "without a final backup"
		if finalBackup {
			backup = "after a final backup"
		}
		job.logf(moveStepPrepare, "Removing the retained data of %s on %s (%s) %s, on the operator's confirmation.", held.AppID, c.serverName(held.Server), held.ID, backup)
		job.Error, job.Failure = "", nil
		for i := range job.Steps {
			if job.Steps[i].Status == "failed" && job.Steps[i].Key != moveStepFinalSync {
				job.Steps[i].Status = "pending"
			}
		}
		job.State = moveStateRunning
		return nil
	})
}
