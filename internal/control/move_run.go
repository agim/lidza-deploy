package control

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/audit"
)

// The move runner executes a job's pending steps in order until it reaches
// the cutover, fails, or finishes. One runner per job runs at a time; the
// minute tick resumes jobs whose runner stopped, for example after a
// restart of the control panel. Every step runs through moveAttempt, which
// retries transient failures with backoff (move_retry.go), cancels a
// stalled attempt (move_progress.go) and stops an attempt the operator
// aborted.

var (
	errMoveTimeout = errors.New("timed out")
	// errMoveStopped ends an attempt without a failure: the control panel
	// shuts down (the tick resumes the step) or the move's state changed.
	errMoveStopped = errors.New("the attempt stopped")
)

func (c *Control) startMoveRunner(ctx context.Context, id string) {
	go c.runMove(ctx, id)
}

func (c *Control) claimMove(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.moveRunning[id] {
		return false
	}
	if c.moveRunning == nil {
		c.moveRunning = map[string]bool{}
	}
	c.moveRunning[id] = true
	return true
}

func (c *Control) releaseMove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.moveRunning, id)
}

// runMove drives one job until it waits for the operator.
func (c *Control) runMove(ctx context.Context, id string) {
	if !c.claimMove(id) {
		return
	}
	defer c.releaseMove(id)
	for {
		job, ok := c.moveJob(id)
		if !ok || ctx.Err() != nil {
			return
		}
		switch job.State {
		case moveStateRunning:
			key := nextMoveStep(job)
			if key == "" || key == moveStepCutover {
				c.enterMoveCutover(ctx, id)
				return
			}
			// A destination prepared earlier may have been removed since (an
			// operator deleted the application there, or an abort of another
			// attempt cleaned it up): prepare it again instead of failing later.
			if key != moveStepPrepare && job.TargetCreated && c.moveDestinationGone(ctx, job) {
				if _, err := c.updateMove(id, func(job *MoveJob) error {
					job.setStep(moveStepPrepare, "pending")
					job.setStep(moveStepEnvironment, "pending")
					job.logf(moveStepPrepare, "%s no longer has %s; preparing the destination and copying the environment again before %s.", job.TargetServer, job.AppID, key)
					return nil
				}); err != nil {
					return
				}
				continue
			}
			if _, err := c.updateMove(id, func(job *MoveJob) error { job.setStep(key, "running"); return nil }); err != nil {
				return
			}
			progress, failure, err := c.moveAttempt(ctx, id, key, moveStateRunning, func(ctx context.Context) error {
				switch key {
				case moveStepPrepare:
					return c.movePrepare(ctx, id)
				case moveStepEnvironment:
					return c.moveCopyEnvironment(ctx, id)
				case moveStepData:
					return c.moveCopyData(ctx, id, moveStepData)
				case moveStepFiles:
					return c.moveCopyFiles(ctx, id, moveStepFiles)
				case moveStepDeploy:
					return c.moveDeploy(ctx, id, moveStepDeploy, false)
				}
				return fmt.Errorf("unknown step %s", key)
			})
			switch {
			case err != nil:
				// Shutting down (the step stays running and the tick resumes
				// it after the restart) or the state changed (an abort).
				continue
			case failure != nil:
				c.failMove(ctx, id, key, *failure, progress)
				// An abort requested while the step was failing runs now
				// instead of being lost under the failure.
				if next, ok := c.moveJob(id); ok && next.State == moveStateAborting {
					continue
				}
				return
			}
			if _, err := c.updateMove(id, func(job *MoveJob) error { job.setStep(key, "done"); job.setProgress(key, progress); return nil }); err != nil {
				return
			}
		case moveStateSyncing, moveStateFinishing, moveStateAborting:
			switch job.State {
			case moveStateSyncing:
				c.runFinalSync(ctx, id)
			case moveStateFinishing:
				c.runMoveFinish(ctx, id)
			default:
				c.runMoveAbort(ctx, id)
			}
			// Continue while the state moved on (an abort during a final
			// sync); stop when the step left it unchanged.
			if next, ok := c.moveJob(id); !ok || next.State == job.State {
				return
			}
		default:
			return
		}
	}
}

func nextMoveStep(job MoveJob) string {
	for _, step := range job.Steps {
		switch step.Key {
		case moveStepPlan, moveStepFinalSync, moveStepFinish:
			continue
		}
		if step.Status != "done" && step.Status != "skipped" {
			return step.Key
		}
	}
	return ""
}

// moveAttempt runs one step until it succeeds, fails for good, or stops.
// Transient failures are retried after their backoff while the move stays
// in state. It returns the step's last progress; failure is set when the
// step failed for good, err (errMoveStopped) when the attempt stopped
// without a failure.
func (c *Control) moveAttempt(ctx context.Context, id, step, state string, run func(context.Context) error) (*agent.Progress, *MoveFailure, error) {
	for {
		if !c.moveWaitRetry(ctx, id, step, state) {
			return nil, nil, errMoveStopped
		}
		_, _ = c.updateMove(id, func(job *MoveJob) error {
			if job.Failure == nil || job.Failure.NextRetry == nil {
				return errMoveUnchanged
			}
			job.Failure.NextRetry = nil
			job.logf(step, "Attempt %d/%d started.", job.Failure.Attempt+1, job.Failure.MaxAttempts)
			return nil
		})
		attemptCtx, cancel := context.WithCancelCause(ctx)
		end := c.beginMoveLive(attemptCtx, id, step)
		c.watchMoveAttempt(id, state, cancel)
		err := run(attemptCtx)
		diagnostics := c.moveTakeDiagnostics(id)
		progress := end()
		cause := context.Cause(attemptCtx)
		cancel(nil)
		if err == nil {
			_, _ = c.updateMove(id, func(job *MoveJob) error {
				if job.Failure == nil && job.Stall == nil {
					return errMoveUnchanged
				}
				if job.Failure != nil {
					job.logf(step, "Succeeded after %d failed attempts.", job.Failure.Attempt)
				}
				job.Failure, job.Stall = nil, nil
				return nil
			})
			return progress, nil, nil
		}
		if ctx.Err() != nil || errors.Is(cause, errMoveInterrupted) {
			return progress, nil, errMoveStopped
		}
		if errors.Is(cause, errMoveStalled) {
			err = moveTransientError("stalled", fmt.Errorf("%w for %s; the attempt was cancelled (%v)", errMoveStalled, moveDuration(c.moveStallLimits().cancel), err))
		}
		job, _ := c.moveJob(id)
		failure := c.moveFailureFor(job, step, moveEngineOf(err), err)
		failure.Diagnostics = diagnostics
		if failure.Class == moveClassTransient && failure.Attempt < failure.MaxAttempts {
			delay := c.moveRetryDelay(failure.Attempt + 1)
			next := time.Now().UTC().Add(delay)
			failure.NextRetry = &next
			job, _ = c.updateMove(id, func(job *MoveJob) error {
				job.Failure = &failure
				job.Stall = nil
				job.logf(step, "Transient failure (%s); attempt %d/%d in %s: %s", failure.Code, failure.Attempt+1, failure.MaxAttempts, moveDuration(delay), failure.Message)
				for _, line := range diagnostics {
					job.logf(step, "Diagnostics: %s", line)
				}
				return nil
			})
			c.reportMoveFailure(ctx, job, failure, false)
			continue
		}
		return progress, &failure, nil
	}
}

// moveWaitRetry waits until the step's scheduled retry is due. It returns
// false when the move left state or the control panel shuts down.
func (c *Control) moveWaitRetry(ctx context.Context, id, step, state string) bool {
	for {
		job, ok := c.moveJob(id)
		if !ok || job.State != state {
			return false
		}
		f := job.Failure
		if f == nil || f.NextRetry == nil || f.Step != step {
			return true
		}
		wait := time.Until(*f.NextRetry)
		if wait <= 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(min(wait, time.Second)):
		}
	}
}

// failMove fails a step for good; progress is the step's last live
// progress, if any.
func (c *Control) failMove(ctx context.Context, id, key string, failure MoveFailure, progress *agent.Progress) {
	job, _ := c.updateMove(id, func(job *MoveJob) error {
		job.setStep(key, "failed")
		job.setProgress(key, progress)
		job.logf(key, "Failed (%s, attempt %d/%d): %s", failure.Class, failure.Attempt, failure.MaxAttempts, failure.Message)
		if failure.Remediation != "" {
			job.logf(key, "What to do: %s", failure.Remediation)
		}
		for _, line := range failure.Diagnostics {
			job.logf(key, "Diagnostics: %s", line)
		}
		// An abort requested meanwhile keeps its state: the runner aborts
		// next instead of leaving the move failed with the abort lost.
		if job.State != moveStateAborting {
			job.State = moveStateFailed
		}
		job.Error = failure.Message
		job.Failure = &failure
		job.Stall = nil
		return nil
	})
	c.reportMoveFailure(ctx, job, failure, true)
}

// movePoll calls check until it reports done, fails or the timeout passes.
func (c *Control) movePoll(ctx context.Context, timeout time.Duration, check func() (bool, error)) error {
	interval := c.movePollInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		done, err := check()
		if err != nil || done {
			return err
		}
		if time.Now().After(deadline) {
			return errMoveTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// moveCall is agentCall bounded by a longer timeout for slow operations.
func (c *Control) moveCall(ctx context.Context, timeout time.Duration, serverID, method, path string, body, out any) error {
	return c.agentCall(moveRequestFor(withAgentTimeout(ctx, timeout)), serverID, method, path, body, out)
}

// ---- prepare ------------------------------------------------------------------

func (c *Control) movePrepare(ctx context.Context, id string) error {
	job, _ := c.moveJob(id)
	if err := c.movePrepareReplication(ctx, job); err != nil {
		return err
	}
	job, _ = c.moveJob(id)
	if job.TargetRuntime == moveRuntimeNative {
		return c.movePrepareNative(ctx, job)
	}
	return c.movePrepareContainer(ctx, job)
}

// moveBackupStorage sends off-site storage to the destination when a
// database requests off-site copies; without it backups stay local.
func (c *Control) moveBackupStorage(ctx context.Context, job MoveJob, step string) bool {
	c.mu.Lock()
	storage := c.data.BackupStorage
	c.mu.Unlock()
	if storage == nil {
		c.moveLog(job.ID, step, "Off-site storage is not configured; destination backups stay local.")
		return false
	}
	if err := c.agentCall(moveRequestFor(ctx), job.TargetServer, http.MethodPut, "/v1/backup-storage", backupStorageConfig(*storage), nil); err != nil {
		c.moveLog(job.ID, step, "Off-site storage could not be sent to the destination; its backups stay local.")
		return false
	}
	return true
}

func (c *Control) movePrepareNative(ctx context.Context, job MoveJob) error {
	r := moveRequestFor(ctx)
	path := "/v1/native/apps/" + url.PathEscape(job.AppID)
	var status agent.NativeStatus
	exists := c.agentCall(r, job.TargetServer, http.MethodGet, path, nil, &status) == nil && status.Application.ID == job.AppID
	if exists && !job.TargetCreated {
		return errors.New("the destination already has a native application with this id that this move did not create")
	}
	if !exists {
		if err := c.moveClearRetained(ctx, job); err != nil {
			return err
		}
		// Record the attempt first: an interrupted creation is then adopted
		// on retry, or removed by an abort.
		var err error
		if job, err = c.updateMove(job.ID, func(job *MoveJob) error {
			job.TargetCreated = true
			job.logf(moveStepPrepare, "Creating %s on %s with the %s runtime.", job.AppID, job.TargetServer, job.NativeSpec.Runtime)
			return nil
		}); err != nil {
			return err
		}
		spec := *job.NativeSpec
		if spec.Data.Offsite && !c.moveBackupStorage(ctx, job, moveStepPrepare) {
			spec.Data.Offsite = false
		}
		if spec.Data.Postgres || spec.Data.Redis {
			c.moveLog(job.ID, moveStepPrepare, "Requesting PostgreSQL=%t and Redis=%t on the destination.", spec.Data.Postgres, spec.Data.Redis)
		}
		if spec.Data.ClickHouse {
			c.moveLog(job.ID, moveStepPrepare, "Requesting a ClickHouse database on the destination (release series %s when ClickHouse is installed there now).", cmp.Or(spec.Data.ClickHouseVersion, "latest LTS"))
		}
		var created struct {
			IngressNote string `json:"ingress_note"`
		}
		createErr := c.moveCall(ctx, 6*time.Minute, job.TargetServer, http.MethodPost, "/v1/native/apps", spec, &created)
		if c.agentCall(r, job.TargetServer, http.MethodGet, path, nil, &status) != nil || status.Application.ID != job.AppID {
			if createErr != nil {
				return fmt.Errorf("the destination could not create the application: %s", moveAgentError(createErr))
			}
			return errors.New("the destination did not report the created application")
		}
		c.moveLog(job.ID, moveStepPrepare, "Created the application on %s.", job.TargetServer)
		if created.IngressNote != "" {
			c.moveLog(job.ID, moveStepPrepare, "%s", created.IngressNote)
		}
	} else {
		c.moveLog(job.ID, moveStepPrepare, "The application already exists on %s from an earlier attempt.", job.TargetServer)
	}
	native := status.Application
	if data := job.NativeSpec.Data; data.Postgres || data.Redis || data.ClickHouse {
		var view agent.NativeHostDataView
		if err := c.movePoll(ctx, 15*time.Minute, func() (bool, error) {
			if err := c.agentCall(r, job.TargetServer, http.MethodGet, path+"/data", nil, &view); err != nil {
				return false, nil
			}
			return (!data.Postgres || view.Postgres != nil && view.Postgres.Ready) && (!data.Redis || view.Redis != nil && view.Redis.Ready) &&
				(!data.ClickHouse || view.ClickHouse != nil && view.ClickHouse.Ready), nil
		}); err != nil {
			return fmt.Errorf("the destination's data services are not ready: %w", err)
		}
		if view.Postgres != nil {
			c.moveLog(job.ID, moveStepPrepare, "PostgreSQL %d database %s is ready on the destination.", view.Postgres.Version, view.Postgres.Resource)
		}
		if view.Redis != nil {
			c.moveLog(job.ID, moveStepPrepare, "Redis is ready on the destination.")
		}
		if job.ClickHouse != nil {
			if err := c.movePrepareClickHouse(ctx, job); err != nil {
				return err
			}
		}
		_, err := c.updateMove(job.ID, func(job *MoveJob) error {
			for i := range job.Databases {
				if job.Databases[i].Copyable && view.Postgres != nil {
					job.Databases[i].TargetID = view.Postgres.Resource
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, d := range job.Databases {
			if d.Copyable && view.Postgres != nil {
				c.moveCarryBeforeDeploy(ctx, job, d, view.Postgres.Resource)
			}
		}
	}
	_, err := c.updateMove(job.ID, func(job *MoveJob) error {
		job.Target.Native = &native
		if native.Runtime != "" {
			job.Target.Runtime = native.Runtime
		}
		return nil
	})
	return err
}

func (c *Control) movePrepareContainer(ctx context.Context, job MoveJob) error {
	if err := c.movePrepareHostMail(ctx, job); err != nil {
		return err
	}
	r := moveRequestFor(ctx)
	path := "/v1/apps/" + url.PathEscape(job.AppID)
	var settings agent.Settings
	exists := c.agentCall(r, job.TargetServer, http.MethodGet, path+"/settings", nil, &settings) == nil && settings.ID == job.AppID
	if exists && !job.TargetCreated {
		return errors.New("the destination already has an application with this id that this move did not create")
	}
	if !exists {
		spec := *job.ContainerSpec
		if err := c.verifyApplicationRuntime(r, job.TargetServer, spec); err != nil {
			return err
		}
		if err := spec.Validate(); err != nil {
			return err
		}
		if err := c.moveClearRetained(ctx, job); err != nil {
			return err
		}
		var err error
		if job, err = c.updateMove(job.ID, func(job *MoveJob) error {
			job.TargetCreated = true
			job.logf(moveStepPrepare, "Creating %s on %s as a container application.", job.AppID, job.TargetServer)
			return nil
		}); err != nil {
			return err
		}
		if err := c.agentCall(r, job.TargetServer, http.MethodPut, path, spec, nil); err != nil {
			return fmt.Errorf("the destination could not create the application: %s", moveAgentError(err))
		}
		if err := c.agentCall(r, job.TargetServer, http.MethodGet, path+"/settings", nil, &settings); err != nil {
			return err
		}
		c.moveLog(job.ID, moveStepPrepare, "Created the application on %s.", job.TargetServer)
	} else {
		c.moveLog(job.ID, moveStepPrepare, "The application already exists on %s from an earlier attempt.", job.TargetServer)
	}
	for i, d := range job.Databases {
		if !d.Copyable || len(d.EnvKeys) == 0 {
			continue
		}
		keys := slices.Clone(d.EnvKeys)
		slices.SortStableFunc(keys, func(a, b string) int {
			if a == "DATABASE_URL" {
				return -1
			}
			if b == "DATABASE_URL" {
				return 1
			}
			return 0
		})
		bound := settings.Bindings[keys[0]]
		if bound == "" {
			request := agent.DatabaseRequest{Mode: "local", EnvKey: keys[0], Backup: d.Backup}
			if keys[0] != "DATABASE_URL" {
				request.ID = d.SourceID
			}
			if request.Backup.Keep < 1 {
				request.Backup.Keep = 1
			}
			if request.Backup.Validate() != nil {
				request.Backup = agent.BackupPolicy{Hours: 24, Keep: 1}
			}
			if request.Backup.Offsite && !c.moveBackupStorage(ctx, job, moveStepPrepare) {
				request.Backup.Offsite = false
			}
			if err := c.agentCall(r, job.TargetServer, http.MethodPost, path+"/database", request, nil); err != nil {
				return fmt.Errorf("the destination could not create a database for %s: %s", keys[0], moveAgentError(err))
			}
			if err := c.agentCall(r, job.TargetServer, http.MethodGet, path+"/settings", nil, &settings); err != nil {
				return err
			}
			if bound = settings.Bindings[keys[0]]; bound == "" {
				return fmt.Errorf("the destination did not bind a database to %s", keys[0])
			}
			c.moveLog(job.ID, moveStepPrepare, "Requested PostgreSQL database %s for %s on the destination.", bound, keys[0])
		}
		for _, key := range keys[1:] {
			if settings.Bindings[key] == "" {
				if err := c.agentCall(r, job.TargetServer, http.MethodPost, path+"/database", agent.DatabaseRequest{Mode: "existing", ID: bound, EnvKey: key}, nil); err != nil {
					return fmt.Errorf("the destination could not attach %s: %s", key, moveAgentError(err))
				}
			}
		}
		// A database whose provisioning failed keeps its error until it is
		// provisioned again; a retried Prepare finds it already bound and
		// would only poll. Ask once per attempt for provisioning again, and
		// fail only when that fails too.
		reprovisioned := false
		if err := c.movePoll(ctx, 15*time.Minute, func() (bool, error) {
			var views []agent.DatabaseView
			if err := c.agentCall(r, job.TargetServer, http.MethodGet, "/v1/databases", nil, &views); err != nil {
				return false, nil
			}
			for _, v := range views {
				if v.AppID == bound {
					if v.Ready {
						return true, nil
					}
					if v.Operation == "" && v.Error != "" {
						if reprovisioned {
							return false, errors.New(v.Error)
						}
						reprovisioned = true
						c.moveLog(job.ID, moveStepPrepare, "Database %s on the destination failed earlier (%s); provisioning it again.", bound, moveBounded(v.Error, 160))
						if err := c.agentCall(r, job.TargetServer, http.MethodPost, "/v1/databases/"+url.PathEscape(bound)+"/provision", nil, nil); err != nil {
							return false, fmt.Errorf("%s; provisioning it again was refused: %s", v.Error, moveAgentError(err))
						}
						return false, nil
					}
				}
			}
			return false, nil
		}); err != nil {
			return fmt.Errorf("database %s on the destination is not ready: %w", bound, err)
		}
		c.moveLog(job.ID, moveStepPrepare, "Database %s is ready on the destination.", bound)
		c.moveCarryBeforeDeploy(ctx, job, d, bound)
		if _, err := c.updateMove(job.ID, func(job *MoveJob) error { job.Databases[i].TargetID = bound; return nil }); err != nil {
			return err
		}
	}
	if job.ClickHouse != nil {
		if err := c.movePrepareClickHouse(ctx, job); err != nil {
			return err
		}
	}
	if job.CacheLocal {
		var cache agent.CacheView
		if err := c.agentCall(r, job.TargetServer, http.MethodGet, path+"/cache", nil, &cache); err != nil || cache.Mode != "local" {
			if err := c.agentCall(r, job.TargetServer, http.MethodPost, path+"/cache", agent.CacheRequest{Mode: "local"}, nil); err != nil {
				return fmt.Errorf("the destination could not configure the cache: %s", moveAgentError(err))
			}
			c.moveLog(job.ID, moveStepPrepare, "Configured the managed cache on the destination; it starts empty.")
		}
	}
	return nil
}

// movePrepareHostMail binds the destination's host Postfix to loopback when
// the plan found it holding a published port while serving only local
// system mail (move_ports.go). The agent call is idempotent, so a retried
// Prepare repeats it harmlessly.
func (c *Control) movePrepareHostMail(ctx context.Context, job MoveJob) error {
	mail := job.Plan.HostMail
	if mail == nil || !mail.Loopback {
		return nil
	}
	c.moveLog(job.ID, moveStepPrepare, "Binding the destination's host mail server (Postfix) to loopback to free port %s.", movePortLabels(mail.Ports))
	var result agent.HostMailLoopbackResult
	request := agent.HostMailLoopbackRequest{Ports: mail.Ports, Trigger: "move", Reference: "move " + job.ID + " of " + job.AppID}
	if err := c.moveCall(ctx, 2*time.Minute, job.TargetServer, http.MethodPost, "/v1/host/mail/loopback", request, &result); err != nil {
		return fmt.Errorf("the destination's host mail server could not be bound to loopback: %s", moveAgentError(err))
	}
	c.moveLog(job.ID, moveStepPrepare, "%s", cmp.Or(result.Summary, "The destination's host mail server listens on loopback only."))
	return nil
}

// ---- environment ---------------------------------------------------------------

func (c *Control) moveCopyEnvironment(ctx context.Context, id string) error {
	return c.moveCopyEnvironmentTo(ctx, id, moveStepEnvironment)
}

// moveEnsureEnvironment repeats the environment copy when a native
// destination lacks variables the move copied earlier. Container
// destinations keep their settings in the agent and cannot be read back.
func (c *Control) moveEnsureEnvironment(ctx context.Context, id, step string) error {
	job, _ := c.moveJob(id)
	if job.TargetRuntime != moveRuntimeNative || len(job.Plan.CopiedKeys) == 0 {
		return nil
	}
	var values agent.MoveEnvironment
	path := "/v1/move/apps/" + url.PathEscape(job.AppID) + "/environment"
	if err := c.moveCall(ctx, time.Minute, job.TargetServer, http.MethodGet, path, nil, &values); err != nil {
		return moveOn(job.TargetServer, fmt.Errorf("the destination environment could not be read: %w", err))
	}
	var missing []string
	for _, key := range job.Plan.CopiedKeys {
		if _, ok := values.Values[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	c.moveLog(id, step, "The destination lacks %d copied variable(s) (%s); copying the environment again before deploying.", len(missing), moveKeyList(missing))
	return c.moveCopyEnvironmentTo(ctx, id, step)
}

// moveCopyEnvironmentTo copies the source's environment to the destination
// and logs into step. A copy after the first one (the final sync) shows
// which keys changed on the source since, and removes keys the source no
// longer has.
func (c *Control) moveCopyEnvironmentTo(ctx context.Context, id, step string) error {
	job, _ := c.moveJob(id)
	path := "/v1/move/apps/" + url.PathEscape(job.AppID) + "/environment"
	var source agent.MoveEnvironment
	if err := c.moveCall(ctx, time.Minute, job.SourceServer, http.MethodGet, path, nil, &source); err != nil {
		return moveOn(job.SourceServer, fmt.Errorf("the source environment could not be read: %w", err))
	}
	var target *agent.MoveEnvironment
	if job.TargetRuntime == moveRuntimeNative {
		var values agent.MoveEnvironment
		if err := c.moveCall(ctx, time.Minute, job.TargetServer, http.MethodGet, path, nil, &values); err != nil {
			return moveOn(job.TargetServer, fmt.Errorf("the destination environment could not be read: %w", err))
		}
		target = &values
	}
	changes, kinds, report := moveEnvironmentChanges(job, source, target)
	digests := map[string]string{}
	for _, key := range report.copied {
		digests[key] = c.moveEnvDigest(job, key, source.Values[key])
	}
	var added, changed, removed []string
	again := job.EnvDigests != nil
	if again {
		added, changed, removed = moveEnvironmentDiff(job.EnvDigests, digests)
		for _, key := range removed {
			changes[key] = nil
		}
	}
	// Values exist only in this function's memory; logs name keys only.
	clear(source.Values)
	if target != nil {
		clear(target.Values)
	}
	if len(changes) > 0 {
		patch := agent.SettingsPatch{EnvChanges: changes, EnvKindChanges: kinds}
		settingsPath := "/v1/native/apps/" + url.PathEscape(job.AppID) + "/settings"
		if job.TargetRuntime == moveRuntimeContainer {
			patch.Branch, patch.Domain = job.Target.Branch, job.Target.Domain
			settingsPath = "/v1/apps/" + url.PathEscape(job.AppID) + "/settings"
		}
		err := c.moveCall(ctx, time.Minute, job.TargetServer, http.MethodPatch, settingsPath, patch, nil)
		for key := range changes {
			changes[key] = nil
		}
		if err != nil {
			return moveOn(job.TargetServer, fmt.Errorf("the destination rejected the environment: %w", err))
		}
	}
	_, err := c.updateMove(id, func(job *MoveJob) error {
		job.EnvDigests = digests
		if again {
			if len(added)+len(changed)+len(removed) == 0 {
				job.logf(step, "Environment exported again: unchanged since the move copied it (%d variables).", len(report.copied))
			} else {
				job.logf(step, "Environment exported again: %d changed (%s), %d added (%s), %d removed (%s) on the source since the move copied it.", len(changed), moveKeyList(changed), len(added), moveKeyList(added), len(removed), moveKeyList(removed))
			}
			return nil
		}
		job.logf(step, "Copied %d variables%s: %s.", len(report.copied), moveSourceSummary(report.sources), moveKeyList(report.copied))
		if len(report.rebuilt) > 0 {
			job.logf(step, "The destination's own build provides %s (deploy defaults and image).", moveKeyList(report.rebuilt))
		}
		if len(report.mapped) > 0 {
			job.logf(step, "Pointed %s at the destination's own data services.", moveKeyList(report.mapped))
		}
		if len(report.owned) > 0 {
			job.logf(step, "Kept the destination's values for %s.", moveKeyList(report.owned))
		}
		return nil
	})
	return err
}

func moveKeyList(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ", ")
}

type moveEnvironmentReport struct {
	copied, mapped, owned []string
	// rebuilt are deploy-default and image variables a container
	// destination's own build sets again; sources counts the copied
	// variables by origin (settings, deploy defaults, image).
	rebuilt []string
	sources map[string]int
}

// moveSourceOrder is the order and wording of the copied-variable summary.
var moveSourceOrder = []struct{ source, label string }{
	{agent.EnvironmentSourcePanel, "from settings"},
	{agent.EnvironmentSourceDefaults, "from deploy defaults"},
	{agent.EnvironmentSourceImage, "from image"},
}

// moveSourceSummary is " (14 from settings, 6 from deploy defaults, 2 from
// image)", or "" when every variable is a setting.
func moveSourceSummary(counts map[string]int) string {
	if counts[agent.EnvironmentSourceDefaults] == 0 && counts[agent.EnvironmentSourceImage] == 0 {
		return ""
	}
	parts := []string{}
	for _, entry := range moveSourceOrder {
		if n := counts[entry.source]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, entry.label))
		}
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// moveApplicationLidzaKeys are LIDZA_ variables that belong to the
// application rather than the agent: its sealed-credentials key and its
// framework mode.
var moveApplicationLidzaKeys = map[string]bool{"LIDZA_MASTER_KEY": true, "LIDZA_MODE": true}

var moveNativeReservedKeys = []string{"PATH", "HOME", "USER", "SHELL", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "BASH_ENV", "ENV", "BUNDLE_GEMFILE", "PWD", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "NODE_VERSION"}

// moveDestinationOwned reports keys the destination sets itself: ports,
// storage paths, host-managed and agent-owned variables, and the connections
// of data services it provisions.
func moveDestinationOwned(key string, job MoveJob) bool {
	switch {
	case key == "PORT" || key == "LIDZA_ADDR" || key == "STORAGE_DIR":
		return true
	case key == "LIDZA_MODE" && job.TargetRuntime == moveRuntimeNative:
		return true // a native host sets the framework mode itself and refuses it as a setting
	case strings.HasPrefix(key, "LIDZA_") && !moveApplicationLidzaKeys[key]:
		return true
	case strings.HasPrefix(key, "DEPLOY_GATEWAY_"):
		return true
	case job.ClickHouse != nil && slices.Contains(agent.ClickHouseEnvironmentKeys, key):
		return true // the destination's own ClickHouse database
	}
	if job.TargetRuntime == moveRuntimeNative {
		if slices.Contains(moveNativeReservedKeys, key) || strings.HasPrefix(key, "ASDF_") || strings.HasPrefix(key, "NVM_") {
			return true
		}
		if job.Redis && (key == "REDIS_URL" || key == "REDIS_SOCKET" || key == "REDIS_PASSWORD") {
			return true
		}
		if key == "DATABASE_URL" && moveHasCopyableData(job) {
			return true
		}
	}
	return false
}

// moveEnvironmentChanges maps the source environment onto the destination:
// keys the destination owns keep its values; database and cache connections
// of copied services point at the destination's services; everything else
// is copied with its classification. target is the destination's current
// environment (native destinations), used for mapping.
func moveEnvironmentChanges(job MoveJob, source agent.MoveEnvironment, target *agent.MoveEnvironment) (map[string]*string, map[string]string, moveEnvironmentReport) {
	changes, kinds := map[string]*string{}, map[string]string{}
	report := moveEnvironmentReport{sources: map[string]int{}}
	databaseKeys := map[string]bool{}
	for _, d := range job.Databases {
		if d.Copyable {
			for _, key := range d.EnvKeys {
				databaseKeys[key] = true
			}
		}
	}
	mapTo := func(key, targetKey string) {
		if target == nil { // planning: no values yet
			report.mapped = append(report.mapped, key)
			return
		}
		if target.Values[targetKey] == "" {
			report.owned = append(report.owned, key)
			return
		}
		value := target.Values[targetKey]
		changes[key] = &value
		kinds[key] = agent.EnvironmentCredential
		report.mapped = append(report.mapped, key)
	}
	for key, value := range source.Values {
		switch {
		case moveDestinationOwned(key, job):
			report.owned = append(report.owned, key)
		case databaseKeys[key] && job.TargetRuntime == moveRuntimeNative:
			mapTo(key, "DATABASE_URL")
		case databaseKeys[key]:
			report.owned = append(report.owned, key) // the destination binds its own database
		case key == "CACHE_URL" && job.CacheLocal && job.TargetRuntime == moveRuntimeNative:
			mapTo(key, "REDIS_URL")
		case key == "CACHE_URL" && job.CacheLocal:
			report.owned = append(report.owned, key)
		case job.TargetRuntime == moveRuntimeContainer && source.Sources[key] != "" && source.Sources[key] != agent.EnvironmentSourcePanel:
			// A container destination builds the same manifest and image;
			// storing these as settings would pin them past later changes.
			report.rebuilt = append(report.rebuilt, key)
		default:
			copy := value
			changes[key] = &copy
			if kind := source.Kinds[key]; kind == agent.EnvironmentSetting || kind == agent.EnvironmentCredential {
				kinds[key] = kind
			}
			report.copied = append(report.copied, key)
			report.sources[cmp.Or(source.Sources[key], agent.EnvironmentSourcePanel)]++
		}
	}
	slices.Sort(report.rebuilt)
	slices.Sort(report.copied)
	slices.Sort(report.mapped)
	slices.Sort(report.owned)
	return changes, kinds, report
}

// ---- data -----------------------------------------------------------------------

// moveCopyData copies the databases (data step and final sync) through
// their engines (move_engine.go).
func (c *Control) moveCopyData(ctx context.Context, id, step string) error {
	job, _ := c.moveJob(id)
	for _, d := range job.Databases {
		if !d.Copyable && d.Mode == "external" {
			c.moveLog(id, step, "Database %s is external; the destination uses the same connection.", d.SourceID)
		}
	}
	phase := movePhaseBulk
	if step == moveStepFinalSync {
		phase = movePhaseFinal
	}
	return c.runMoveEngines(ctx, id, step, phase, moveEnginePostgres, moveEngineClickHouse, moveEngineRedis)
}

func moveSize(n int64) string {
	switch {
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
}

// moveFreshBackup takes a backup through the source's normal backup
// pipeline and returns its record once it is finished. observe receives the
// source's live progress of the backup while it runs (nil from agents that
// do not report it).
func (c *Control) moveFreshBackup(ctx context.Context, serverID, database string, observe func(*agent.Progress)) (agent.BackupRecord, error) {
	r := moveRequestFor(ctx)
	find := func() (agent.DatabaseView, error) {
		var views []agent.DatabaseView
		if err := c.agentCall(r, serverID, http.MethodGet, "/v1/databases", nil, &views); err != nil {
			return agent.DatabaseView{}, err
		}
		for _, v := range views {
			if v.AppID == database {
				return v, nil
			}
		}
		return agent.DatabaseView{}, errors.New("the source database is gone")
	}
	before, err := find()
	if err != nil {
		return agent.BackupRecord{}, err
	}
	known := map[string]bool{}
	for _, b := range before.Backups {
		known[b.ID] = true
	}
	if before.Operation == "" {
		if err := c.agentCall(r, serverID, http.MethodPost, "/v1/databases/"+url.PathEscape(database)+"/backup", nil, nil); err != nil {
			var remote *agentHTTPError
			if !errors.As(err, &remote) || remote.status != http.StatusConflict || !strings.Contains(remote.message, "already running") {
				return agent.BackupRecord{}, errors.New(moveAgentError(err))
			}
		}
	}
	var record agent.BackupRecord
	err = c.movePoll(ctx, 3*time.Hour, func() (bool, error) {
		v, err := find()
		if err != nil {
			return false, nil
		}
		if v.Operation != "" {
			if observe != nil {
				observe(v.Progress)
			}
			return false, nil
		}
		for _, b := range v.Backups {
			if !known[b.ID] && b.SHA256 != "" && b.Size > 0 && (record.ID == "" || b.Created.After(record.Created)) {
				record = b
			}
		}
		if record.ID != "" {
			return true, nil
		}
		if v.Error != "" {
			return false, errors.New(v.Error)
		}
		// The backup finished without a new file; start another.
		if err := c.agentCall(r, serverID, http.MethodPost, "/v1/databases/"+url.PathEscape(database)+"/backup", nil, nil); err != nil {
			return false, errors.New(moveAgentError(err))
		}
		return false, nil
	})
	return record, err
}

// ---- deploy ---------------------------------------------------------------------

func moveDeploymentFinished(status string) (finished, success bool) {
	switch status {
	case "live", "setup_required":
		return true, true
	case "queued", "building", "":
		return false, false
	}
	return true, false
}

// moveDeploy releases the source's commit on the destination and checks it
// there. fresh starts a new deployment even when an earlier one succeeded.
func (c *Control) moveDeploy(ctx context.Context, id, step string, fresh bool) error {
	deployStarted := time.Now().UTC()
	if step == moveStepDeploy {
		// The destination may have been prepared again since the environment
		// was copied (a removed destination, a retried attempt): when copied
		// variables are missing there, the copy is repeated before anything
		// runs, and logs only what changed.
		if err := c.moveEnsureEnvironment(ctx, id, step); err != nil {
			return err
		}
	}
	job, _ := c.moveJob(id)
	r := moveRequestFor(ctx)
	if step == moveStepDeploy && job.Deployment == "" {
		// The source may have been deployed while the move copied.
		var source agent.MoveSource
		if c.agentCall(r, job.SourceServer, http.MethodGet, "/v1/move/apps/"+url.PathEscape(job.AppID), nil, &source) == nil && source.Commit != "" && source.Commit != job.Commit {
			job, _ = c.updateMove(id, func(job *MoveJob) error {
				job.logf(step, "The source now runs commit %s (the move started with %s); the destination deploys it.", moveShortCommit(source.Commit), moveShortCommit(job.Commit))
				job.Commit = source.Commit
				return nil
			})
		}
	}
	deployment := agent.Deployment{}
	reuse := false
	if job.Deployment != "" && !fresh {
		if err := c.agentCall(r, job.TargetServer, http.MethodGet, "/v1/deployments/"+url.PathEscape(job.Deployment), nil, &deployment); err == nil {
			if finished, success := moveDeploymentFinished(deployment.Status); !finished || success {
				reuse = true
			}
		}
	}
	if !reuse {
		var err error
		deployment, err = c.moveStartDeployment(ctx, job, step)
		if err != nil {
			return err
		}
	}
	c.moveStage(id, fmt.Sprintf("deploying %s on %s (deployment %s)", moveShortCommit(job.Commit), job.TargetServer, deployment.ID), 0, nil)
	if err := c.movePoll(ctx, 90*time.Minute, func() (bool, error) {
		var d agent.Deployment
		if err := c.agentCall(r, job.TargetServer, http.MethodGet, "/v1/deployments/"+url.PathEscape(deployment.ID), nil, &d); err != nil {
			return false, nil
		}
		deployment = d
		finished, success := moveDeploymentFinished(d.Status)
		if finished && !success {
			message := d.Error
			if message == "" {
				message = d.Status
			}
			return false, fmt.Errorf("the deployment on the destination failed: %s", message)
		}
		return finished, nil
	}); err != nil {
		return err
	}
	_, _ = c.updateMove(id, func(next *MoveJob) error {
		next.DeployedCommit = cmp.Or(deployment.Commit, job.Commit)
		// A source deploy during this deployment is not covered by it.
		next.Stale = next.Commit != job.Commit || moveReleasedSince(next.SourceChanges, deployStarted)
		next.logf(step, "Deployment %s is %s on %s (commit %s).", deployment.ID, deployment.Status, job.TargetServer, moveShortCommit(deployment.Commit))
		return nil
	})
	if job.Commit != "" && deployment.Commit != "" && deployment.Commit != job.Commit {
		c.moveLog(id, step, "Note: the destination runs commit %s; the source runs %s.", moveShortCommit(deployment.Commit), moveShortCommit(job.Commit))
	}
	c.moveStage(id, "checking the application's health on "+job.TargetServer, 0, nil)
	var health agent.MoveHealth
	if err := c.movePoll(ctx, 3*time.Minute, func() (bool, error) {
		if err := c.agentCall(r, job.TargetServer, http.MethodPost, "/v1/move/apps/"+url.PathEscape(job.AppID)+"/health", nil, &health); err != nil {
			return false, nil
		}
		return health.Healthy, nil
	}); err != nil {
		detail := health.Detail
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("the application is not healthy on the destination: %s", detail)
	}
	c.moveLog(id, step, "Health check on the destination passed: %s.", health.Detail)
	if address := moveProbeAddress(job.Addresses); address != "" && len(job.Hostnames) > 0 {
		if err := c.moveHTTPSProbe(ctx, address, job.Hostnames[0]); err != nil {
			c.moveLog(id, step, "HTTPS for %s is not served by the destination yet (%s); its certificate is issued once DNS points there.", job.Hostnames[0], err)
		} else {
			c.moveLog(id, step, "HTTPS for %s already works on the destination.", job.Hostnames[0])
		}
	}
	return nil
}

// moveReleasedSince reports a source deploy recorded after since.
func moveReleasedSince(changes []MoveSourceChange, since time.Time) bool {
	for _, change := range changes {
		if change.Kind == moveChangeRelease && change.Time.After(since) {
			return true
		}
	}
	return false
}

func moveShortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	if commit == "" {
		return "unknown"
	}
	return commit
}

func (c *Control) moveStartDeployment(ctx context.Context, job MoveJob, step string) (agent.Deployment, error) {
	r := moveRequestFor(ctx)
	var d agent.Deployment
	attempt := job.DeployAttempt + 1
	issue := c.deploymentCredentials
	if c.credentialsFixture != nil {
		issue = c.credentialsFixture
	}
	credentials, err := issue(ctx, job.Target, "move:"+job.ID+":"+strconv.Itoa(attempt))
	if err != nil {
		return d, fmt.Errorf("deployment credentials: %w", err)
	}
	if job.TargetRuntime == moveRuntimeNative {
		path := "/v1/native/apps/" + url.PathEscape(job.AppID)
		var status agent.NativeStatus
		if err := c.agentCall(r, job.TargetServer, http.MethodGet, path, nil, &status); err != nil {
			return d, err
		}
		if !status.DeploymentSupported && !status.ReleaseManaged {
			return d, errors.New("the destination has not enabled deployments for this application")
		}
		body := struct {
			agent.DeployRequest
			Ref string `json:"ref,omitempty"`
		}{DeployRequest: credentials}
		if moveCommitPattern.MatchString(job.Commit) {
			body.Ref = job.Commit
		}
		if err := c.moveCall(ctx, time.Minute, job.TargetServer, http.MethodPost, path+"/deploy", body, &d); err != nil {
			return d, fmt.Errorf("the destination did not start the deployment: %s", moveAgentError(err))
		}
	} else {
		if err := c.syncStorageIfConfigured(r, job.TargetServer); err != nil {
			return d, err
		}
		if err := c.moveCall(ctx, time.Minute, job.TargetServer, http.MethodPost, "/v1/apps/"+url.PathEscape(job.AppID)+"/deploy", credentials, &d); err != nil {
			return d, fmt.Errorf("the destination did not start the deployment: %s", moveAgentError(err))
		}
	}
	_, err = c.updateMove(job.ID, func(job *MoveJob) error {
		job.Deployment, job.DeployAttempt = d.ID, attempt
		job.logf(step, "Started deployment %s of commit %s on %s.", d.ID, moveShortCommit(job.Commit), job.TargetServer)
		return nil
	})
	return d, err
}

// ---- cutover --------------------------------------------------------------------

func (c *Control) enterMoveCutover(ctx context.Context, id string) {
	_, err := c.updateMove(id, func(job *MoveJob) error {
		if job.State != moveStateRunning {
			return errors.New("state changed")
		}
		job.State = moveStateCutover
		job.setStep(moveStepCutover, "waiting")
		job.logf(moveStepCutover, "The destination is ready and the source keeps serving. Set these DNS records at your DNS provider:")
		for _, record := range moveRecords(*job) {
			if record.Note != "" {
				job.logf(moveStepCutover, "%s %s %s (%s)", record.Hostname, record.Type, record.Value, strings.TrimSuffix(record.Note, "."))
				continue
			}
			job.logf(moveStepCutover, "%s %s %s", record.Hostname, record.Type, record.Value)
		}
		job.logf(moveStepCutover, "Remove every other A and AAAA record of these hostnames. The move continues when DNS resolves to the destination.")
		job.logf(moveStepCutover, "Until the final sync, the destination serves the bulk copy; anything written there after DNS switches is replaced by the source's data at the final sync, which holds both servers in maintenance while it copies.")
		if job.Plan.Mail != nil {
			job.logf(moveStepCutover, "The application sends mail: the reverse DNS of the destination is checked with DNS and logged here; it does not hold the move. %s", moveMailDKIMNote)
		}
		return nil
	})
	if err == nil {
		job, _ := c.moveJob(id)
		moveAudit(ctx, job, "cutover", false)
		c.checkMoveDNS(ctx, id, false)
	}
}

// ---- final sync -----------------------------------------------------------------

const moveMaintenanceMessage = "This application is moving to a new server and will be back in a few minutes."

// runFinalSync freezes the source and copies its last state: it
// reconciles settings, environment and release (move_reconcile.go), copies
// every engine again and redeploys the destination. The application is
// locked from here on (move_lock.go). Transient failures are retried with
// the source kept in maintenance; a final failure lets the source serve
// again and keeps the lock until the final sync is resumed or the move is
// aborted.
func (c *Control) runFinalSync(ctx context.Context, id string) {
	job, err := c.updateMove(id, func(job *MoveJob) error {
		if s := job.step(moveStepFinalSync); s == nil || s.Status != "running" {
			job.setStep(moveStepFinalSync, "running")
			job.logf(moveStepFinalSync, "Final sync: the application is locked; putting the source in maintenance to stop writes.")
		}
		job.SourceFrozen = true
		return nil
	})
	if err != nil {
		return
	}
	moveAudit(ctx, job, "final_sync.start", false)
	progress, failure, stop := c.moveAttempt(ctx, id, moveStepFinalSync, moveStateSyncing, func(ctx context.Context) error {
		if err := c.moveFreeze(ctx, job, true); err != nil {
			return moveOn(job.SourceServer, fmt.Errorf("the source could not enter maintenance: %w", err))
		}
		if err := c.moveFreezeTarget(ctx, job, true); err != nil {
			return moveOn(job.TargetServer, fmt.Errorf("the destination could not enter maintenance: %w", err))
		}
		_, _ = c.updateMove(id, func(job *MoveJob) error { job.TargetFrozen = true; return nil })
		c.moveLog(id, moveStepFinalSync, "The source is in maintenance, and so is the destination: what it received since the DNS switch is replaced by the source's data. Reconciling the destination and copying the databases and files again.")
		if err := c.moveReconcile(ctx, id, moveStepFinalSync); err != nil {
			return err
		}
		if err := c.moveCopyData(ctx, id, moveStepFinalSync); err != nil {
			return err
		}
		if err := c.moveCopyFiles(ctx, id, moveStepFinalSync); err != nil {
			return err
		}
		if err := c.moveDeploy(ctx, id, moveStepFinalSync, true); err != nil {
			return err
		}
		// The redeploy starts a native destination; a container one is
		// still flagged for maintenance.
		if err := c.moveFreezeTarget(ctx, job, false); err != nil {
			return moveOn(job.TargetServer, fmt.Errorf("the destination could not leave maintenance: %w", err))
		}
		_, _ = c.updateMove(id, func(job *MoveJob) error { job.TargetFrozen = false; return nil })
		return nil
	})
	if stop != nil {
		return
	}
	if failure != nil {
		thaw := c.moveFreeze(ctx, job, false)
		thawTarget := c.moveFreezeTarget(ctx, job, false)
		job, _ = c.updateMove(id, func(job *MoveJob) error {
			job.setStep(moveStepFinalSync, "failed")
			job.setProgress(moveStepFinalSync, progress)
			job.logf(moveStepFinalSync, "Failed (%s, attempt %d/%d): %s", failure.Class, failure.Attempt, failure.MaxAttempts, failure.Message)
			if failure.Remediation != "" {
				job.logf(moveStepFinalSync, "What to do: %s", failure.Remediation)
			}
			if thaw == nil {
				job.SourceFrozen = false
				job.logf(moveStepFinalSync, "The source left maintenance and serves again; the application stays locked until the final sync is resumed or the move is aborted.")
			} else {
				job.logf(moveStepFinalSync, "The source could not leave maintenance (%s); abort or retry the final sync.", thaw)
			}
			if thawTarget == nil {
				job.TargetFrozen = false
				job.logf(moveStepFinalSync, "The destination serves the bulk copy again.")
			} else {
				job.logf(moveStepFinalSync, "The destination could not leave maintenance (%s); retry the final sync.", thawTarget)
			}
			job.State = moveStateCutover
			job.Error = "final sync failed: " + failure.Message
			job.Failure = failure
			if job.AutoCutover {
				job.AutoCutover = false
				job.logf(moveStepFinalSync, "The automatic cutover is off after this failure: retry the final sync by hand, then finish the move or switch the automatic cutover on again.")
			}
			return nil
		})
		c.reportMoveFailure(ctx, job, *failure, true)
		return
	}
	job, _ = c.updateMove(id, func(job *MoveJob) error {
		job.setStep(moveStepFinalSync, "done")
		job.setProgress(moveStepFinalSync, progress)
		job.FinalSynced = true
		job.Error = ""
		job.logf(moveStepFinalSync, "The destination has the source's final data. The source stays in maintenance; switch DNS now.")
		job.State = moveStateCutover
		return nil
	})
	c.clearMoveIncident(ctx, job)
	moveAudit(ctx, job, "final_sync", false)
	c.checkMoveDNS(ctx, id, false)
}

// moveFreeze puts the source in maintenance (containers) or stops its
// processes (native applications), or undoes that.
func (c *Control) moveFreeze(ctx context.Context, job MoveJob, freeze bool) error {
	return c.moveFreezeOn(ctx, job, job.SourceServer, job.SourceRuntime, freeze)
}

// moveFreezeTarget does the same on the destination. The destination
// serves the bulk copy from the DNS switch on, and anything written there
// is replaced by the final sync; while the final sync runs it must not
// write at all, or rows it adds collide with the same rows arriving from
// the source. The redeploy at the end of the final sync starts it again.
func (c *Control) moveFreezeTarget(ctx context.Context, job MoveJob, freeze bool) error {
	return c.moveFreezeOn(ctx, job, job.TargetServer, job.TargetRuntime, freeze)
}

func (c *Control) moveFreezeOn(ctx context.Context, job MoveJob, server, runtime string, freeze bool) error {
	r := moveRequestFor(ctx)
	if runtime == moveRuntimeContainer {
		return c.agentCall(r, server, http.MethodPut, "/v1/apps/"+url.PathEscape(job.AppID)+"/maintenance", agent.Maintenance{Enabled: freeze, Message: moveMaintenanceMessage}, nil)
	}
	var status agent.NativeStatus
	path := "/v1/native/apps/" + url.PathEscape(job.AppID)
	if err := c.agentCall(r, server, http.MethodGet, path, nil, &status); err != nil {
		return err
	}
	action := "stop"
	if !freeze {
		action = "start"
	}
	units := slices.Clone(status.Application.Services)
	if !freeze {
		slices.Reverse(units)
	}
	for _, unit := range units {
		if err := c.moveCall(ctx, time.Minute, server, http.MethodPost, path+"/services/"+url.PathEscape(unit)+"/"+action, map[string]string{"confirm": status.Application.Name}, nil); err != nil {
			return fmt.Errorf("%s %s: %s", action, unit, moveAgentError(err))
		}
	}
	return nil
}

// ---- finish ---------------------------------------------------------------------

func (c *Control) runMoveFinish(ctx context.Context, id string) {
	if _, err := c.updateMove(id, func(job *MoveJob) error {
		if s := job.step(moveStepFinish); s == nil || s.Status != "running" {
			job.setStep(moveStepFinish, "running")
		}
		return nil
	}); err != nil {
		return
	}
	_, failure, stop := c.moveAttempt(ctx, id, moveStepFinish, moveStateFinishing, func(ctx context.Context) error {
		return c.moveFinishOnce(ctx, id)
	})
	if stop != nil {
		return
	}
	if failure != nil {
		c.failMove(ctx, id, moveStepFinish, *failure, nil)
		return
	}
	job, _ := c.updateMove(id, func(job *MoveJob) error {
		job.logf(moveStepFinish, "Retired the application on %s. Its databases and last backups stay there as retained resources.", job.SourceServer)
		if left := moveLeftBehind(*job); len(left) > 0 {
			job.logf(moveStepFinish, "Summary: tables left behind as chosen (schema only, no rows on %s): %s.", job.TargetServer, strings.Join(left, ", "))
		}
		job.setStep(moveStepFinish, "done")
		if s := job.step(moveStepFinalSync); s != nil && s.Status == "pending" {
			s.Status = "skipped"
		}
		job.State = moveStateFinished
		job.SourceFrozen = false
		job.Error, job.Failure = "", nil
		return nil
	})
	c.clearMoveIncident(ctx, job)
	moveAudit(ctx, job, "finish", false)
}

// moveFinishOnce switches the application to the destination and retires
// the source; each part is idempotent. A move finished without a final sync
// reconciles the destination first, so a source deploy or configuration
// change made meanwhile is not lost.
func (c *Control) moveFinishOnce(ctx context.Context, id string) error {
	job, _ := c.moveJob(id)
	r := moveRequestFor(ctx)
	if !job.Switched {
		if !job.FinalSynced {
			if err := c.moveReconcile(ctx, id, moveStepFinish); err != nil {
				return err
			}
			if job, _ = c.moveJob(id); job.Stale {
				if err := c.moveDeploy(ctx, id, moveStepFinish, true); err != nil {
					return err
				}
			}
			job, _ = c.moveJob(id)
		}
		if job.TargetRuntime == moveRuntimeNative {
			var status agent.NativeStatus
			if err := c.agentCall(r, job.TargetServer, http.MethodGet, "/v1/native/apps/"+url.PathEscape(job.AppID), nil, &status); err != nil || status.Application.ID != job.AppID {
				if err == nil {
					err = errors.New("the destination application is unavailable")
				}
				return moveOn(job.TargetServer, fmt.Errorf("the destination application is unavailable: %w", err))
			}
			job.Target.Native = &status.Application
		}
		if err := c.switchMovedApplication(job); err != nil {
			return moveOperatorError("record_changed", err.Error(), "Review the application in the panel; abort the move if its record was changed on purpose.")
		}
		job, _ = c.moveJob(id)
		moveAudit(ctx, job, "switch", false)
	}
	c.moveCleanupEngines(ctx, id, moveStepFinish)
	var retireErr error
	// The source's data is the application's own: its retained resource
	// records the move as switched, so it is never treated as a copy.
	retire := agent.AppRemoveRequest{Origin: moveRetainedOrigin(job, true)}
	if job.SourceRuntime == moveRuntimeNative {
		path := "/v1/native/apps/" + url.PathEscape(job.AppID)
		retireErr = c.moveCall(ctx, 6*time.Minute, job.SourceServer, http.MethodDelete, path, retire, nil)
		if retireErr != nil {
			var status agent.NativeStatus
			if c.agentCall(r, job.SourceServer, http.MethodGet, path, nil, &status) != nil {
				retireErr = nil // already removed
			}
		}
	} else {
		retireErr = c.moveCall(ctx, 6*time.Minute, job.SourceServer, http.MethodDelete, "/v1/apps/"+url.PathEscape(job.AppID), retire, nil)
	}
	if retireErr != nil {
		return moveOn(job.SourceServer, fmt.Errorf("the application now runs on %s, but retiring it on %s failed: %w", job.TargetServer, job.SourceServer, retireErr))
	}
	return nil
}

// moveCleanupEngines removes every engine's temporary artifacts.
func (c *Control) moveCleanupEngines(ctx context.Context, id, step string) {
	moveCleanupOnce(ctx, c.moveEngines(moveRun{c: c, id: id, step: step, phase: movePhaseBulk, teardown: true}))
}

// switchMovedApplication makes the destination the live record: same id,
// the destination server, and the source's deployment settings.
func (c *Control) switchMovedApplication(job MoveJob) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.data.Apps[job.AppID]
	if !ok || current.ServerID != job.SourceServer {
		return errors.New("the application record changed during the move")
	}
	previousJob := c.data.Moves[job.ID]
	live := job.Target
	live.AutoDeploy, live.HookID, live.Secret, live.Previews = current.AutoDeploy, current.HookID, current.Secret, current.Previews
	if current.GitHubInstallation != 0 {
		live.GitHubInstallation = current.GitHubInstallation
	}
	live.Retiring = false
	next := cloneMoveJob(previousJob)
	next.Switched = true
	next.Target = live
	next.Target.Secret = ""
	next.logf(moveStepFinish, "The application record now points to %s; auto-deploy, webhook and repository access carried over.", job.TargetServer)
	next.Updated = time.Now().UTC()
	removed := map[string]Incident{}
	for _, kind := range []string{"database:", "health:", "deploy:", "security-burst:", "security-review:"} {
		key := kind + job.SourceServer + ":" + job.AppID
		if incident, found := c.data.Incidents[key]; found {
			removed[key] = incident
			delete(c.data.Incidents, key)
		}
	}
	c.data.Apps[job.AppID] = live
	c.data.Moves[job.ID] = next
	if err := c.save(); err != nil {
		c.data.Apps[job.AppID] = current
		c.data.Moves[job.ID] = previousJob
		for key, incident := range removed {
			c.data.Incidents[key] = incident
		}
		return err
	}
	return nil
}

// ---- abort ----------------------------------------------------------------------

func (c *Control) runMoveAbort(ctx context.Context, id string) {
	job, _ := c.moveJob(id)
	r := moveRequestFor(ctx)
	fail := func(err error) {
		job, _ := c.updateMove(id, func(job *MoveJob) error {
			job.Error = "abort incomplete: " + err.Error() + "; continue to retry"
			job.logf(moveStepPlan, "Abort incomplete: %s", err)
			return nil
		})
		moveAudit(ctx, job, "abort", true)
	}
	if job.SourceFrozen {
		if err := c.moveFreeze(ctx, job, false); err != nil {
			fail(fmt.Errorf("the source could not leave maintenance: %s", moveAgentError(err)))
			return
		}
		job, _ = c.updateMove(id, func(job *MoveJob) error {
			job.SourceFrozen = false
			job.logf(moveStepPlan, "The source left maintenance and serves again.")
			return nil
		})
	}
	c.moveCleanupEngines(ctx, id, moveStepPlan)
	gone := false
	if job.TargetCreated {
		var err error
		if gone, err = c.moveRemoveTarget(ctx, r, job); err != nil {
			fail(fmt.Errorf("the destination application could not be removed: %s", moveAgentError(err)))
			return
		}
	} else {
		c.moveLog(id, moveStepPlan, "This move created nothing on %s; nothing to remove there.", job.TargetServer)
	}
	job, _ = c.updateMove(id, func(job *MoveJob) error {
		switch {
		case job.TargetCreated && gone:
			job.logf(moveStepPlan, "%s on %s was already gone; nothing left to remove there.", job.AppID, job.TargetServer)
		case job.TargetCreated && !job.Switched:
			job.logf(moveStepPlan, "Removed the application from %s with the data copied there: the move never switched, so the copy is not retained.", job.TargetServer)
		case job.TargetCreated:
			job.logf(moveStepPlan, "Removed the application from %s. Databases copied there stay as retained resources with their backups.", job.TargetServer)
		}
		job.TargetCreated = false
		for i := range job.Steps {
			if s := &job.Steps[i]; s.Status == "pending" || s.Status == "running" || s.Status == "waiting" {
				s.Status = "skipped"
			}
		}
		job.logf(moveStepPlan, "Move aborted; the application keeps running on %s.", job.SourceServer)
		job.State = moveStateAborted
		job.Error, job.Failure, job.Stall = "", nil, nil
		return nil
	})
	c.clearMoveIncident(ctx, job)
	moveAudit(ctx, job, "abort", false)
}

// moveRemoveTarget removes the application the move created on the
// destination. It reports gone when the destination answered that the
// application does not exist; any other failure (the agent restarting or
// unreachable, a busy application) is an error, so the abort is retried
// instead of leaving the destination's folder, units, account and
// databases behind. Before the switch the data there is a copy of the
// source's and is deleted with the application; after it the destination's
// data is retained like any removed application's.
func (c *Control) moveRemoveTarget(ctx context.Context, r *http.Request, job MoveJob) (bool, error) {
	remove := agent.AppRemoveRequest{Origin: moveRetainedOrigin(job, job.Switched), DeleteData: !job.Switched}
	if job.TargetRuntime != moveRuntimeNative {
		return false, c.moveCall(ctx, 6*time.Minute, job.TargetServer, http.MethodDelete, "/v1/apps/"+url.PathEscape(job.AppID), remove, nil)
	}
	path := "/v1/native/apps/" + url.PathEscape(job.AppID)
	err := c.moveCall(ctx, 6*time.Minute, job.TargetServer, http.MethodDelete, path+"?purge=1", remove, nil)
	if err == nil {
		return false, nil
	}
	var status agent.NativeStatus
	lookup := c.agentCall(r, job.TargetServer, http.MethodGet, path, nil, &status)
	if moveNativeAbsent(lookup) {
		return true, nil
	}
	return false, err
}

// moveNativeAbsent reports an agent's answer that a native application is
// not registered there; an unreachable agent is not an answer.
// moveDestinationGone reports that the destination agent definitely no
// longer has the application this move created. An unreachable agent is not
// "gone": the step then fails and retries as usual.
func (c *Control) moveDestinationGone(ctx context.Context, job MoveJob) bool {
	r := moveRequestFor(ctx)
	if job.TargetRuntime == moveRuntimeNative {
		var status agent.NativeStatus
		return moveNativeAbsent(c.agentCall(r, job.TargetServer, http.MethodGet, "/v1/native/apps/"+url.PathEscape(job.AppID), nil, &status))
	}
	var settings agent.Settings
	err := c.agentCall(r, job.TargetServer, http.MethodGet, "/v1/apps/"+url.PathEscape(job.AppID)+"/settings", nil, &settings)
	var answer *agentHTTPError
	return errors.As(err, &answer) && answer.status == http.StatusNotFound
}

func moveNativeAbsent(err error) bool {
	var answer *agentHTTPError
	return errors.As(err, &answer) && (answer.status == http.StatusNotFound || strings.Contains(answer.message, "not registered"))
}

// ---- tick -----------------------------------------------------------------------

// moveTick resumes runners that stopped and polls DNS during cutovers.
func (c *Control) moveTick(ctx context.Context) {
	base := c.moveBase
	if base == nil {
		base = audit.System(context.WithoutCancel(ctx), "move")
	}
	c.mu.Lock()
	resume, cutover, early := []string{}, []string{}, []string{}
	for id, job := range c.data.Moves {
		switch job.State {
		case moveStateRunning, moveStateSyncing, moveStateFinishing, moveStateAborting:
			if !c.moveRunning[id] {
				resume = append(resume, id)
			}
		case moveStateCutover, moveStateReady:
			cutover = append(cutover, id)
		}
		if job.State == moveStateRunning || job.State == moveStateFailed {
			early = append(early, id)
		}
	}
	c.mu.Unlock()
	for _, id := range early {
		c.checkMoveEarlyDNS(ctx, id)
	}
	for _, id := range resume {
		c.startMoveRunner(base, id)
	}
	for _, id := range cutover {
		c.checkMoveDNS(ctx, id, false)
		c.moveReplicationKeepalive(ctx, id)
	}
	// Replication through the panel runs while its move lives, also after
	// a panel restart and while the move waits at the cutover.
	c.moveTunnelSync()
}
