package agent

// Moving an application from a database of the shared host Redis to its own
// lidza-redis@<id> instance (runRedisMigration holds the steps; this file
// binds them to the host).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// startRedisMigration checks the application can move and runs the
// migration in the background as a resource-tuning host job.
func (m *Manager) startRedisMigration(app string) error {
	if err := m.hostOperationsAllowed(); err != nil {
		return err
	}
	if !idPattern.MatchString(app) {
		return errors.New("invalid application id")
	}
	ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
	obs, _ := m.observeTuning(ctx)
	cancel()
	m.mu.Lock()
	state := m.tuningStateLocked()
	overrides, peaks, probes := state.Overrides, state.Peaks, state.RedisUses
	m.mu.Unlock()
	view := planTuning(obs, overrides, peaks, probes)
	var entry *TuningRedisApp
	for _, r := range view.Redis {
		if r.Kind != "shared" {
			continue
		}
		for i := range r.Apps {
			if r.Apps[i].AppID == app {
				entry = &r.Apps[i]
			}
		}
	}
	if entry == nil {
		return errors.New("the application does not use the shared Redis")
	}
	if !entry.CanMove {
		return errors.New("the application cannot move: " + entry.MoveBlocked)
	}
	var self tuningAppObs
	for _, a := range obs.Apps {
		if a.ID == app {
			self = a
		}
	}
	_, policy, err := m.nativeReleasePolicy(app)
	if err != nil {
		return err
	}
	account, err := m.redisMigrationAccount(policy)
	if err != nil {
		return err
	}
	// Size the new instance before keys arrive: its share of the budget,
	// and for data that must not be evicted at least 1.5 × what the old
	// databases hold.
	use := entry.Use
	limit := max(64, computeTuningBudget(obs.Host, len(obs.Apps), 0).RedisMiB/max(1, len(view.Redis)+1))
	policyName := redisPolicyFor([]string{use})
	if use != tuningRedisUseCache {
		limit = max(limit, m.sharedRedisShareMiB(ctx, entry.DBs)*3/2)
	}

	tc := m.tuning()
	hc := m.host()
	m.mu.Lock()
	if busy := m.hostBusyLocked(); busy != "" {
		m.mu.Unlock()
		return errors.New("the move waits: " + busy)
	}
	if m.nativeOperations[app] {
		m.mu.Unlock()
		return errors.New("a deployment of this application is running")
	}
	hc.mu.Lock()
	if hc.job != "" {
		hc.mu.Unlock()
		m.mu.Unlock()
		return fmt.Errorf("host %s is running", hc.job)
	}
	hc.job = tuningJobKind
	hc.mu.Unlock()
	tc.mu.Lock()
	if tc.migrating == nil {
		tc.migrating = map[string]bool{}
	}
	tc.migrating[app] = true
	tc.mu.Unlock()
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[app] = true
	now := time.Now().UTC()
	record := RedisMigration{AppID: app, DBs: entry.DBs, Use: use, Status: "running", Started: now}
	state = m.tuningStateLocked()
	if state.Migrations == nil {
		state.Migrations = map[string]RedisMigration{}
	}
	state.Migrations[app] = record
	job := newHostJob(tuningJobKind, "manual", now)
	job.Summary = "Moving " + app + " to its own Redis"
	m.recordHostJobLocked(job)
	_ = m.save()
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		var out strings.Builder
		logf := func(format string, args ...any) {
			fmt.Fprintf(&out, format+"\n", args...)
		}
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Minute)
		defer cancel()
		steps := m.redisMigrationSteps(app, policy, account, limit, policyName, logf)
		err := runRedisMigration(ctx, steps, self.SharedRedis, use, &record)
		finished := time.Now().UTC()
		record.Finished = finished
		job.Finished = &finished
		job.Status, record.Status = "succeeded", "succeeded"
		job.Summary = fmt.Sprintf("Moved %s to its own Redis (%s, %s); %d keys copied.", app, use, policyName, record.Copied)
		if err != nil {
			job.Status, record.Status, record.Error = "failed", "failed", err.Error()
			job.Summary = "Moving " + app + " to its own Redis failed: " + err.Error()
		}
		job.Output = strings.TrimSpace(out.String())
		tc.mu.Lock()
		delete(tc.migrating, app)
		tc.mu.Unlock()
		hc.mu.Lock()
		hc.job = ""
		hc.mu.Unlock()
		m.mu.Lock()
		delete(m.nativeOperations, app)
		state := m.tuningStateLocked()
		state.Migrations[app] = record
		if err == nil {
			// The new instance holds the use the application had.
			if state.Overrides.RedisUse == nil {
				state.Overrides.RedisUse = map[string]string{}
			}
			if _, set := state.Overrides.RedisUse[app]; !set {
				state.Overrides.RedisUse[app] = use
			}
		}
		m.recordHostJobLocked(job)
		if saveErr := m.save(); saveErr != nil {
			log.Printf("redis migration state: %v", saveErr)
		}
		m.mu.Unlock()
		log.Printf("redis migration of %s: %s", app, job.Summary)
	}()
	return nil
}

// redisMigrationAccount is the account and root the application runs as;
// its own Redis runs as that account.
func (m *Manager) redisMigrationAccount(policy NativeReleasePolicy) (NativeAppAccount, error) {
	account, err := m.nativeReleaseAccount(policy)
	if err != nil {
		return NativeAppAccount{}, err
	}
	out := NativeAppAccount{User: account.Name, UID: account.UID, GID: account.GID, Home: account.Home, Root: policy.Root}
	if err := validateNativeAppAccount(out); err != nil {
		return NativeAppAccount{}, err
	}
	return out, nil
}

// sharedRedisShareMiB estimates the memory the databases hold on the shared
// server from its used memory and their share of its keys.
func (m *Manager) sharedRedisShareMiB(ctx context.Context, dbs []int) int {
	server, ok := readSharedRedis()
	if !ok {
		return 0
	}
	c, err := server.dial(ctx)
	if err != nil {
		return 0
	}
	defer c.close()
	memory, err1 := c.do(ctx, "INFO", "memory")
	keyspace, err2 := c.do(ctx, "INFO", "keyspace")
	if err1 != nil || err2 != nil {
		return 0
	}
	used, _ := strconv.ParseInt(redisInfoField(redisString(memory), "used_memory"), 10, 64)
	var total, mine int64
	for db, counts := range redisKeyspace(redisString(keyspace)) {
		total += counts[0]
		for _, d := range dbs {
			if d == db {
				mine += counts[0]
			}
		}
	}
	if total == 0 {
		return 0
	}
	return int(used * mine / total >> 20)
}

// redisMigrationSteps binds the migration to the host: the shared server,
// the application's own instance, its units and environment files.
func (m *Manager) redisMigrationSteps(app string, policy NativeReleasePolicy, account NativeAppAccount, limitMiB int, evict string, logf func(string, ...any)) redisMigrationSteps {
	h := nativeHostDataHost(m)
	var password, socket string
	active := []string{}
	return redisMigrationSteps{
		Log: logf,
		Source: func(ctx context.Context) (redisConn, error) {
			server, ok := readSharedRedis()
			if !ok {
				return nil, errors.New("the shared Redis configuration is unreadable")
			}
			return server.dial(ctx)
		},
		Provision: func(ctx context.Context) (func(int) string, error) {
			nativeHostDataMu.Lock()
			var err error
			password, socket, err = m.provisionNativeRedis(ctx, h, app, account)
			nativeHostDataMu.Unlock()
			if err != nil {
				return nil, err
			}
			if err := m.applyOwnRedisLimit(ctx, h, app, limitMiB, evict); err != nil {
				return nil, err
			}
			logf("lidza-redis@%s runs with maxmemory %d MiB and %s", app, limitMiB, evict)
			base := strings.TrimSuffix(nativeRedisURL(socket, password), "?db=0")
			return func(db int) string { return base + "?db=" + strconv.Itoa(db) }, nil
		},
		Target: func(ctx context.Context) (redisConn, error) {
			return dialTuningRedis(ctx, "unix", h.path(socket), password)
		},
		Stop: func(ctx context.Context) error {
			if err := m.recordActiveUnits(ctx, app, policy, &active); err != nil {
				return err
			}
			for _, unit := range active {
				if err := m.nativeServiceActionFor(ctx, app, unit, "stop"); err != nil {
					return err
				}
			}
			return nil
		},
		Start: func(ctx context.Context) error {
			if len(active) == 0 {
				if err := m.recordActiveUnits(ctx, app, policy, &active); err != nil {
					return err
				}
			}
			for _, unit := range active {
				if err := m.nativeServiceActionFor(ctx, app, unit, "restart"); err != nil {
					return err
				}
			}
			return nil
		},
		SwitchEnv: func(ctx context.Context, changes map[string]string) (func(context.Context) error, error) {
			current, err := nativeReleaseEnvValues(policy)
			if err != nil {
				return nil, err
			}
			next := map[string]*string{}
			previous := map[string]*string{}
			for key, value := range changes {
				v := value
				next[key] = &v
				old := current[key]
				previous[key] = &old
			}
			if err := rewriteNativeReleaseEnvFiles(policy.EnvFiles, next); err != nil {
				return nil, err
			}
			logf("Updated %s in the application's environment", strings.Join(sortedKeys(changes), ", "))
			return func(context.Context) error { return rewriteNativeReleaseEnvFiles(policy.EnvFiles, previous) }, nil
		},
		Healthy: func(ctx context.Context) error {
			deadline := time.Now().Add(90 * time.Second)
			for {
				status, err := m.nativeStatus(ctx, app)
				down := []string{}
				if err == nil {
					for _, unit := range active {
						for _, s := range status.Services {
							if s.Unit == unit && s.Active != "active" {
								down = append(down, unit)
							}
						}
					}
				}
				if err == nil && len(down) == 0 {
					return nil
				}
				if time.Now().After(deadline) || ctx.Err() != nil {
					if err != nil {
						return err
					}
					return fmt.Errorf("%s did not stay active", strings.Join(down, ", "))
				}
				time.Sleep(3 * time.Second)
			}
		},
	}
}

// recordActiveUnits lists the application's units that run now, in the
// release policy's order and without the ones the operator keeps stopped.
func (m *Manager) recordActiveUnits(ctx context.Context, app string, policy NativeReleasePolicy, active *[]string) error {
	status, err := m.nativeStatus(ctx, app)
	if err != nil {
		return err
	}
	running := map[string]bool{}
	for _, s := range status.Services {
		running[s.Unit] = s.Active == "active"
	}
	*active = (*active)[:0]
	units := policy.Units
	if len(units) == 0 {
		for _, s := range status.Services {
			units = append(units, s.Unit)
		}
	}
	for _, unit := range units {
		if running[unit] && !containsString(policy.Preserve, unit) {
			*active = append(*active, unit)
		}
	}
	if len(*active) == 0 {
		return errors.New("none of the application's services is running")
	}
	return nil
}

func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
