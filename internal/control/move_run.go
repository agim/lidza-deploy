package control

import (
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
// restart of the control panel.

var errMoveTimeout = errors.New("timed out")

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
		if !ok {
			return
		}
		switch job.State {
		case moveStateRunning:
			key := nextMoveStep(job)
			if key == "" || key == moveStepCutover {
				c.enterMoveCutover(ctx, id)
				return
			}
			if _, err := c.updateMove(id, func(job *MoveJob) error { job.setStep(key, "running"); return nil }); err != nil {
				return
			}
			var err error
			switch key {
			case moveStepPrepare:
				err = c.movePrepare(ctx, id)
			case moveStepEnvironment:
				err = c.moveCopyEnvironment(ctx, id)
			case moveStepData:
				err = c.moveCopyData(ctx, id, moveStepData)
			case moveStepDeploy:
				err = c.moveDeploy(ctx, id, moveStepDeploy, false)
			default:
				err = fmt.Errorf("unknown step %s", key)
			}
			if err != nil {
				if ctx.Err() != nil {
					// Shutting down: the step stays running and the tick
					// resumes it after the restart.
					return
				}
				c.failMove(ctx, id, key, err)
				return
			}
			if _, err := c.updateMove(id, func(job *MoveJob) error { job.setStep(key, "done"); return nil }); err != nil {
				return
			}
		case moveStateSyncing:
			c.runFinalSync(ctx, id)
			return
		case moveStateFinishing:
			c.runMoveFinish(ctx, id)
			return
		case moveStateAborting:
			c.runMoveAbort(ctx, id)
			return
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

func (c *Control) failMove(ctx context.Context, id, key string, cause error) {
	job, _ := c.updateMove(id, func(job *MoveJob) error {
		job.setStep(key, "failed")
		job.logf(key, "Failed: %s", cause)
		job.State = moveStateFailed
		job.Error = cause.Error()
		return nil
	})
	moveAudit(ctx, job, "step."+key, true)
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
		createErr := c.moveCall(ctx, 6*time.Minute, job.TargetServer, http.MethodPost, "/v1/native/apps", spec, nil)
		if c.agentCall(r, job.TargetServer, http.MethodGet, path, nil, &status) != nil || status.Application.ID != job.AppID {
			if createErr != nil {
				return fmt.Errorf("the destination could not create the application: %s", moveAgentError(createErr))
			}
			return errors.New("the destination did not report the created application")
		}
		c.moveLog(job.ID, moveStepPrepare, "Created the application on %s.", job.TargetServer)
	} else {
		c.moveLog(job.ID, moveStepPrepare, "The application already exists on %s from an earlier attempt.", job.TargetServer)
	}
	native := status.Application
	if job.NativeSpec.Data.Postgres || job.NativeSpec.Data.Redis {
		var view agent.NativeHostDataView
		if err := c.movePoll(ctx, 15*time.Minute, func() (bool, error) {
			if err := c.agentCall(r, job.TargetServer, http.MethodGet, path+"/data", nil, &view); err != nil {
				return false, nil
			}
			return (!job.NativeSpec.Data.Postgres || view.Postgres != nil && view.Postgres.Ready) && (!job.NativeSpec.Data.Redis || view.Redis != nil && view.Redis.Ready), nil
		}); err != nil {
			return fmt.Errorf("the destination's data services are not ready: %w", err)
		}
		if view.Postgres != nil {
			c.moveLog(job.ID, moveStepPrepare, "PostgreSQL %d database %s is ready on the destination.", view.Postgres.Version, view.Postgres.Resource)
		}
		if view.Redis != nil {
			c.moveLog(job.ID, moveStepPrepare, "Redis is ready on the destination.")
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
						return false, errors.New(v.Error)
					}
				}
			}
			return false, nil
		}); err != nil {
			return fmt.Errorf("database %s on the destination is not ready: %w", bound, err)
		}
		c.moveLog(job.ID, moveStepPrepare, "Database %s is ready on the destination.", bound)
		if _, err := c.updateMove(job.ID, func(job *MoveJob) error { job.Databases[i].TargetID = bound; return nil }); err != nil {
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

// ---- environment ---------------------------------------------------------------

func (c *Control) moveCopyEnvironment(ctx context.Context, id string) error {
	job, _ := c.moveJob(id)
	r := moveRequestFor(ctx)
	path := "/v1/move/apps/" + url.PathEscape(job.AppID) + "/environment"
	var source agent.MoveEnvironment
	if err := c.agentCall(r, job.SourceServer, http.MethodGet, path, nil, &source); err != nil {
		return fmt.Errorf("the source environment could not be read: %s", moveAgentError(err))
	}
	var target *agent.MoveEnvironment
	if job.TargetRuntime == moveRuntimeNative {
		var values agent.MoveEnvironment
		if err := c.agentCall(r, job.TargetServer, http.MethodGet, path, nil, &values); err != nil {
			return fmt.Errorf("the destination environment could not be read: %s", moveAgentError(err))
		}
		target = &values
	}
	changes, kinds, report := moveEnvironmentChanges(job, source, target)
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
		err := c.agentCall(r, job.TargetServer, http.MethodPatch, settingsPath, patch, nil)
		for key := range changes {
			changes[key] = nil
		}
		if err != nil {
			return fmt.Errorf("the destination rejected the environment: %s", moveAgentError(err))
		}
	}
	_, err := c.updateMove(id, func(job *MoveJob) error {
		job.logf(moveStepEnvironment, "Copied %d variables: %s.", len(report.copied), moveKeyList(report.copied))
		if len(report.mapped) > 0 {
			job.logf(moveStepEnvironment, "Pointed %s at the destination's own data services.", moveKeyList(report.mapped))
		}
		if len(report.owned) > 0 {
			job.logf(moveStepEnvironment, "Kept the destination's values for %s.", moveKeyList(report.owned))
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
}

var moveNativeReservedKeys = []string{"PATH", "HOME", "USER", "SHELL", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "BASH_ENV", "ENV", "BUNDLE_GEMFILE", "PWD", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "NODE_VERSION"}

// moveDestinationOwned reports keys the destination sets itself: ports,
// storage paths, host-managed and agent-owned variables, and the connections
// of data services it provisions.
func moveDestinationOwned(key string, job MoveJob) bool {
	switch {
	case key == "PORT" || key == "LIDZA_ADDR" || key == "STORAGE_DIR":
		return true
	case strings.HasPrefix(key, "LIDZA_") && key != "LIDZA_MASTER_KEY":
		return true
	case strings.HasPrefix(key, "DEPLOY_GATEWAY_"):
		return true
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
	var report moveEnvironmentReport
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
		default:
			copy := value
			changes[key] = &copy
			if kind := source.Kinds[key]; kind == agent.EnvironmentSetting || kind == agent.EnvironmentCredential {
				kinds[key] = kind
			}
			report.copied = append(report.copied, key)
		}
	}
	slices.Sort(report.copied)
	slices.Sort(report.mapped)
	slices.Sort(report.owned)
	return changes, kinds, report
}

// ---- data -----------------------------------------------------------------------

func (c *Control) moveCopyData(ctx context.Context, id, step string) error {
	job, _ := c.moveJob(id)
	r := moveRequestFor(ctx)
	for i, d := range job.Databases {
		if !d.Copyable {
			if d.Mode == "external" {
				c.moveLog(id, step, "Database %s is external; the destination uses the same connection.", d.SourceID)
			}
			continue
		}
		var count struct {
			Tables int `json:"tables"`
		}
		if err := c.agentCall(r, job.SourceServer, http.MethodGet, "/v1/move/databases/"+url.PathEscape(d.SourceID)+"/tables", nil, &count); err != nil {
			return fmt.Errorf("database %s could not be measured: %s", d.SourceID, moveAgentError(err))
		}
		if count.Tables == 0 {
			if _, err := c.updateMove(id, func(job *MoveJob) error {
				job.Databases[i].Skipped = "no tables"
				job.logf(step, "Database %s has no tables; nothing to copy.", d.SourceID)
				return nil
			}); err != nil {
				return err
			}
			continue
		}
		if d.TargetID == "" {
			return fmt.Errorf("the destination database for %s is unknown; prepare the destination again", d.SourceID)
		}
		c.moveLog(id, step, "Taking a fresh backup of %s (%d tables) on %s.", d.SourceID, count.Tables, job.SourceServer)
		record, err := c.moveFreshBackup(ctx, job.SourceServer, d.SourceID)
		if err != nil {
			return fmt.Errorf("the backup of %s failed: %w", d.SourceID, err)
		}
		c.moveLog(id, step, "Backup %s is ready (%s, SHA-256 %s…); streaming it to %s.", record.ID, moveSize(record.Size), record.SHA256[:12], job.TargetServer)
		if err := c.moveTransferDump(ctx, job, d, record); err != nil {
			return fmt.Errorf("copying %s failed: %w", d.SourceID, err)
		}
		if _, err := c.updateMove(id, func(job *MoveJob) error {
			job.Databases[i].Copied, job.Databases[i].Bytes, job.Databases[i].Skipped = record.SHA256, record.Size, ""
			job.logf(step, "Restored %s into %s on %s; the SHA-256 matched on both ends.", d.SourceID, d.TargetID, job.TargetServer)
			return nil
		}); err != nil {
			return err
		}
	}
	if job.Redis {
		c.moveLog(id, step, "Redis contents are not copied; the destination starts with an empty cache and queues.")
	}
	return nil
}

func moveSize(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// moveFreshBackup takes a backup through the source's normal backup
// pipeline and returns its record once it is finished.
func (c *Control) moveFreshBackup(ctx context.Context, serverID, database string) (agent.BackupRecord, error) {
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
	job, _ := c.moveJob(id)
	r := moveRequestFor(ctx)
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
	c.moveLog(id, step, "Deployment %s is %s on %s (commit %s).", deployment.ID, deployment.Status, job.TargetServer, moveShortCommit(deployment.Commit))
	if job.Commit != "" && deployment.Commit != "" && deployment.Commit != job.Commit {
		c.moveLog(id, step, "Note: the destination runs commit %s; the source runs %s.", moveShortCommit(deployment.Commit), moveShortCommit(job.Commit))
	}
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
			job.logf(moveStepCutover, "%s %s %s", record.Hostname, record.Type, record.Value)
		}
		job.logf(moveStepCutover, "Remove every other A and AAAA record of these hostnames. The move continues when DNS resolves to the destination.")
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

func (c *Control) runFinalSync(ctx context.Context, id string) {
	job, err := c.updateMove(id, func(job *MoveJob) error {
		job.setStep(moveStepFinalSync, "running")
		job.logf(moveStepFinalSync, "Putting the source in maintenance to stop writes.")
		job.SourceFrozen = true
		return nil
	})
	if err != nil {
		return
	}
	err = c.moveFreeze(ctx, job, true)
	if err == nil {
		c.moveLog(id, moveStepFinalSync, "The source is in maintenance; copying the databases again.")
		err = c.moveCopyData(ctx, id, moveStepFinalSync)
	}
	if err == nil {
		err = c.moveDeploy(ctx, id, moveStepFinalSync, true)
	}
	if err != nil {
		thaw := c.moveFreeze(ctx, job, false)
		job, _ = c.updateMove(id, func(job *MoveJob) error {
			job.setStep(moveStepFinalSync, "failed")
			job.logf(moveStepFinalSync, "Failed: %s", err)
			if thaw == nil {
				job.SourceFrozen = false
				job.logf(moveStepFinalSync, "The source left maintenance and serves again.")
			} else {
				job.logf(moveStepFinalSync, "The source could not leave maintenance (%s); abort or retry the final sync.", thaw)
			}
			job.State = moveStateCutover
			job.Error = "final sync failed: " + err.Error()
			return nil
		})
		moveAudit(ctx, job, "final_sync", true)
		return
	}
	job, _ = c.updateMove(id, func(job *MoveJob) error {
		job.setStep(moveStepFinalSync, "done")
		job.FinalSynced = true
		job.logf(moveStepFinalSync, "The destination has the source's final data. The source stays in maintenance; switch DNS now.")
		job.State = moveStateCutover
		return nil
	})
	moveAudit(ctx, job, "final_sync", false)
	c.checkMoveDNS(ctx, id, false)
}

// moveFreeze puts the source in maintenance (containers) or stops its
// processes (native applications), or undoes that.
func (c *Control) moveFreeze(ctx context.Context, job MoveJob, freeze bool) error {
	r := moveRequestFor(ctx)
	if job.SourceRuntime == moveRuntimeContainer {
		return c.agentCall(r, job.SourceServer, http.MethodPut, "/v1/apps/"+url.PathEscape(job.AppID)+"/maintenance", agent.Maintenance{Enabled: freeze, Message: moveMaintenanceMessage}, nil)
	}
	var status agent.NativeStatus
	path := "/v1/native/apps/" + url.PathEscape(job.AppID)
	if err := c.agentCall(r, job.SourceServer, http.MethodGet, path, nil, &status); err != nil {
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
		if err := c.moveCall(ctx, time.Minute, job.SourceServer, http.MethodPost, path+"/services/"+url.PathEscape(unit)+"/"+action, map[string]string{"confirm": status.Application.Name}, nil); err != nil {
			return fmt.Errorf("%s %s: %s", action, unit, moveAgentError(err))
		}
	}
	return nil
}

// ---- finish ---------------------------------------------------------------------

func (c *Control) runMoveFinish(ctx context.Context, id string) {
	job, err := c.updateMove(id, func(job *MoveJob) error { job.setStep(moveStepFinish, "running"); return nil })
	if err != nil {
		return
	}
	r := moveRequestFor(ctx)
	if !job.Switched {
		if job.TargetRuntime == moveRuntimeNative {
			var status agent.NativeStatus
			if err := c.agentCall(r, job.TargetServer, http.MethodGet, "/v1/native/apps/"+url.PathEscape(job.AppID), nil, &status); err != nil || status.Application.ID != job.AppID {
				c.failMove(ctx, id, moveStepFinish, errors.New("the destination application is unavailable"))
				return
			}
			job.Target.Native = &status.Application
		}
		if err := c.switchMovedApplication(job); err != nil {
			c.failMove(ctx, id, moveStepFinish, err)
			return
		}
		job, _ = c.moveJob(id)
		moveAudit(ctx, job, "switch", false)
	}
	var retireErr error
	if job.SourceRuntime == moveRuntimeNative {
		path := "/v1/native/apps/" + url.PathEscape(job.AppID)
		retireErr = c.moveCall(ctx, 6*time.Minute, job.SourceServer, http.MethodDelete, path, nil, nil)
		if retireErr != nil {
			var status agent.NativeStatus
			if c.agentCall(r, job.SourceServer, http.MethodGet, path, nil, &status) != nil {
				retireErr = nil // already removed
			}
		}
	} else {
		retireErr = c.moveCall(ctx, 6*time.Minute, job.SourceServer, http.MethodDelete, "/v1/apps/"+url.PathEscape(job.AppID), nil, nil)
	}
	if retireErr != nil {
		c.failMove(ctx, id, moveStepFinish, fmt.Errorf("the application now runs on %s, but retiring it on %s failed: %s; continue to retry", job.TargetServer, job.SourceServer, moveAgentError(retireErr)))
		return
	}
	job, _ = c.updateMove(id, func(job *MoveJob) error {
		job.logf(moveStepFinish, "Retired the application on %s. Its databases and last backups stay there as retained resources.", job.SourceServer)
		job.setStep(moveStepFinish, "done")
		if s := job.step(moveStepFinalSync); s != nil && s.Status == "pending" {
			s.Status = "skipped"
		}
		job.State = moveStateFinished
		job.SourceFrozen = false
		return nil
	})
	moveAudit(ctx, job, "finish", false)
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
	if job.TargetCreated {
		var err error
		if job.TargetRuntime == moveRuntimeNative {
			path := "/v1/native/apps/" + url.PathEscape(job.AppID)
			err = c.moveCall(ctx, 6*time.Minute, job.TargetServer, http.MethodDelete, path+"?purge=1", nil, nil)
			if err != nil {
				var status agent.NativeStatus
				if c.agentCall(r, job.TargetServer, http.MethodGet, path, nil, &status) != nil {
					err = nil
				}
			}
		} else {
			err = c.moveCall(ctx, 6*time.Minute, job.TargetServer, http.MethodDelete, "/v1/apps/"+url.PathEscape(job.AppID), nil, nil)
		}
		if err != nil {
			fail(fmt.Errorf("the destination application could not be removed: %s", moveAgentError(err)))
			return
		}
	}
	job, _ = c.updateMove(id, func(job *MoveJob) error {
		if job.TargetCreated {
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
		job.Error = ""
		return nil
	})
	moveAudit(ctx, job, "abort", false)
}

// ---- tick -----------------------------------------------------------------------

// moveTick resumes runners that stopped and polls DNS during cutovers.
func (c *Control) moveTick(ctx context.Context) {
	base := c.moveBase
	if base == nil {
		base = audit.System(context.WithoutCancel(ctx), "move")
	}
	c.mu.Lock()
	resume, cutover := []string{}, []string{}
	for id, job := range c.data.Moves {
		switch job.State {
		case moveStateRunning, moveStateSyncing, moveStateFinishing, moveStateAborting:
			if !c.moveRunning[id] {
				resume = append(resume, id)
			}
		case moveStateCutover, moveStateReady:
			cutover = append(cutover, id)
		}
	}
	c.mu.Unlock()
	for _, id := range resume {
		c.startMoveRunner(base, id)
	}
	for _, id := range cutover {
		c.checkMoveDNS(ctx, id, false)
	}
}
