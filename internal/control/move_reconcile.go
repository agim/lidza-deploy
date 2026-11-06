package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Changes on the live source during a move. The application stays usable
// while the move copies in the background, so the source may be deployed,
// reconfigured or have its data changed after the destination was prepared.
// Each change through the panel or a webhook is recorded with the move; the
// final sync (and a finish without one) reconciles the destination with the
// source's current state before the switch:
//
//   - the environment is exported again and the differences are logged by
//     key (values never leave memory);
//   - hostnames and the branch are carried over when they changed;
//   - a source deploy marks the destination's release stale, and the
//     destination redeploys the source's current commit;
//   - databases and files are copied again by the final sync itself.
//
// A database added to or removed from the application while it moved stops
// the move with instructions: the move's database mapping was planned at
// its start.

const (
	moveChangeRelease  = "release"
	moveChangeSettings = "settings"
	moveChangeData     = "data"

	moveMaxSourceChanges = 50
)

// MoveSourceChange is one change made to the live source during a move.
type MoveSourceChange struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`   // release, settings or data
	Detail string    `json:"detail"` // the route or delivery, never values
}

// moveChangeKind classifies a mutating application route.
func moveChangeKind(pattern string) string {
	_, path, _ := strings.Cut(pattern, " ")
	switch {
	case strings.HasSuffix(path, "/deploy"), strings.HasSuffix(path, "/rollback"), strings.HasSuffix(path, "/reload"):
		return moveChangeRelease
	case strings.Contains(path, "/database"), strings.Contains(path, "/databases/"), strings.Contains(path, "/restore"), strings.Contains(path, "/backups"), strings.Contains(path, "/cache"):
		return moveChangeData
	}
	return moveChangeSettings
}

// noteMoveSourceChange records a change of the live source; a release
// change marks the destination stale once it has deployed.
func (c *Control) noteMoveSourceChange(id, kind, detail string) {
	_, _ = c.updateMove(id, func(job *MoveJob) error {
		if terminalMoveState(job.State) || moveLocks(*job) {
			return errMoveUnchanged
		}
		job.SourceChanges = append(job.SourceChanges, MoveSourceChange{Time: time.Now().UTC(), Kind: kind, Detail: detail})
		if len(job.SourceChanges) > moveMaxSourceChanges {
			job.SourceChanges = job.SourceChanges[len(job.SourceChanges)-moveMaxSourceChanges:]
		}
		step := nextMoveStep(*job)
		if step == "" {
			step = moveStepCutover
		}
		switch kind {
		case moveChangeRelease:
			if s := job.step(moveStepDeploy); s != nil && (s.Status == "done" || s.Status == "running") {
				job.Stale = true
			}
			job.logf(step, "The source was deployed while the move copies in the background (%s); the destination redeploys the source's current commit at the final sync.", detail)
		case moveChangeData:
			job.logf(step, "The source's data changed while the move copies in the background (%s); the final sync copies the data again.", detail)
		default:
			job.logf(step, "The source's settings changed while the move copies in the background (%s); the final sync carries them over.", detail)
		}
		return nil
	})
}

// moveEnvDigest fingerprints one variable with a key only this control panel
// holds, so a job records whether a value changed without recording it.
func (c *Control) moveEnvDigest(job MoveJob, key, value string) string {
	derive := hmac.New(sha256.New, c.cfg.Key)
	derive.Write([]byte("move-environment:" + job.ID))
	mac := hmac.New(sha256.New, derive.Sum(nil))
	mac.Write([]byte(key))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))[:24]
}

// moveEnvironmentDiff compares the recorded fingerprints with the current
// copied values: keys added, changed and removed since the last copy.
func moveEnvironmentDiff(previous, current map[string]string) (added, changed, removed []string) {
	for key, digest := range current {
		switch old, ok := previous[key]; {
		case !ok:
			added = append(added, key)
		case old != digest:
			changed = append(changed, key)
		}
	}
	for key := range previous {
		if _, ok := current[key]; !ok {
			removed = append(removed, key)
		}
	}
	slices.Sort(added)
	slices.Sort(changed)
	slices.Sort(removed)
	return added, changed, removed
}

// moveReconcile brings the destination's configuration up to the source's
// current state before a switch: settings, environment and release.
func (c *Control) moveReconcile(ctx context.Context, id, step string) error {
	job, _ := c.moveJob(id)
	var source agent.MoveSource
	if err := c.moveCall(ctx, 2*time.Minute, job.SourceServer, http.MethodGet, "/v1/move/apps/"+url.PathEscape(job.AppID), nil, &source); err != nil {
		return moveOn(job.SourceServer, fmt.Errorf("the source could not describe the application: %w", err))
	}
	if err := moveDatabasesUnchanged(job, source); err != nil {
		return err
	}
	if err := c.moveReconcileSettings(ctx, id, step, source); err != nil {
		return err
	}
	if err := c.moveCopyEnvironmentTo(ctx, id, step); err != nil {
		return err
	}
	_, err := c.updateMove(id, func(job *MoveJob) error {
		if source.Commit != "" && source.Commit != job.Commit {
			job.logf(step, "The source now runs commit %s (the move started with %s); the destination deploys it.", moveShortCommit(source.Commit), moveShortCommit(job.Commit))
			job.Commit = source.Commit
			job.Stale = true
		}
		return nil
	})
	return err
}

// moveDatabasesUnchanged refuses a switch when the application's copyable
// databases changed after the move was planned.
func moveDatabasesUnchanged(job MoveJob, source agent.MoveSource) error {
	planned, current := []string{}, []string{}
	for _, d := range job.Databases {
		if d.Copyable {
			planned = append(planned, d.SourceID)
		}
	}
	// A database that is busy right now (a scheduled backup) is not
	// copyable this instant but still the same database.
	for _, d := range source.Databases {
		if d.Mode != "external" {
			current = append(current, d.ID)
		}
	}
	slices.Sort(planned)
	slices.Sort(current)
	if slices.Equal(planned, current) {
		return nil
	}
	return moveOperatorError("databases_changed", fmt.Sprintf("the application's databases changed during the move (planned %s, now %s)", moveKeyList(planned), moveKeyList(current)),
		"Abort this move and start a new one, so the new database layout is planned and copied.")
}

// moveReconcileSettings carries over hostname and branch changes.
func (c *Control) moveReconcileSettings(ctx context.Context, id, step string, source agent.MoveSource) error {
	job, _ := c.moveJob(id)
	hostnames := append([]string{source.Domain}, source.AdditionalDomains...)
	if source.Domain == "" || slices.Equal(hostnames, job.Hostnames) && source.Branch == job.Target.Branch {
		return nil
	}
	additional := slices.Clone(source.AdditionalDomains)
	if additional == nil {
		additional = []string{}
	}
	patch := agent.SettingsPatch{Branch: source.Branch, Domain: source.Domain, AdditionalDomains: &additional}
	path := "/v1/apps/" + url.PathEscape(job.AppID) + "/settings"
	if job.TargetRuntime == moveRuntimeNative {
		path = "/v1/native/apps/" + url.PathEscape(job.AppID) + "/settings"
	}
	if err := c.moveCall(ctx, time.Minute, job.TargetServer, http.MethodPatch, path, patch, nil); err != nil {
		return moveOn(job.TargetServer, fmt.Errorf("the destination did not accept the source's new hostnames or branch: %w", err))
	}
	_, err := c.updateMove(id, func(job *MoveJob) error {
		job.logf(step, "Carried over the source's settings changed during the move: hostnames %s, branch %s.", strings.Join(hostnames, ", "), source.Branch)
		job.Hostnames = hostnames
		job.Target.Domain, job.Target.AdditionalDomains, job.Target.Branch = source.Domain, slices.Clone(source.AdditionalDomains), source.Branch
		job.TLSReady = false
		return nil
	})
	return err
}
