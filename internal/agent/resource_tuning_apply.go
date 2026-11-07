package agent

// Resource tuning on the host: observation, application of the plan, the
// maintenance-window restart of the host PostgreSQL cluster, and the panel
// API. The cycle runs from the maintenance loop: at agent start (after every
// reboot or resize), hourly, and on request from the panel; sessions per
// role are sampled every five minutes for the connection caps.
//
// What is applied when:
//   - PostgreSQL reload settings, role connection limits, Redis limits and
//     policies, ClickHouse memory limits: at once (live).
//   - PostgreSQL shared_buffers, max_connections, superuser and platform
//     reserved connections, max_worker_processes, wal_buffers: written to
//     the managed conf.d file at once, in effect after a cluster restart.
//     The agent restarts the cluster inside the server's maintenance window
//     when nothing else runs (restartDecision); a reboot applies them too.
//   - Container databases: reload settings at once, the rest at the
//     container's next restart (containers are not restarted for tuning).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	tuningInterval       = time.Hour
	tuningSampleInterval = 5 * time.Minute
	tuningProbeMaxAge    = 24 * time.Hour
	tuningConfName       = "90-lidza-tuning.conf"
	tuningJobKind        = "resource-tuning"
	clickhouseMemoryFile = "/etc/clickhouse-server/config.d/lidza-memory.xml"
)

// tuningController is the runtime coordination of one manager.
type tuningController struct {
	mu        sync.Mutex
	running   bool
	booted    bool
	migrating map[string]bool
	chRatio   float64
}

var tuningControllers sync.Map // *Manager -> *tuningController

func (m *Manager) tuning() *tuningController {
	value, _ := tuningControllers.LoadOrStore(m, &tuningController{})
	return value.(*tuningController)
}

func (tc *tuningController) clickhouseRatio() float64 {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.chRatio
}

// tuningStateLocked returns the persisted state, creating it. Caller holds m.mu.
func (m *Manager) tuningStateLocked() *ResourceTuningState {
	if m.data.ResourceTuning == nil {
		m.data.ResourceTuning = &ResourceTuningState{}
	}
	return m.data.ResourceTuning
}

// resourceTuningTick runs from the maintenance loop once a minute.
func (m *Manager) resourceTuningTick(now time.Time) {
	if m.hostOperationsAllowed() != nil {
		return
	}
	tc := m.tuning()
	m.mu.Lock()
	state := m.tuningStateLocked()
	lastRun, lastSample := state.LastRun, state.LastSample
	pending := state.View != nil && len(state.View.PendingRestart) > 0
	window := m.hostPolicyLocked().RebootWindow
	if tc.chRatio == 0 && state.ClickHouseRatio > 0 {
		tc.mu.Lock()
		tc.chRatio = state.ClickHouseRatio
		tc.mu.Unlock()
	}
	m.mu.Unlock()
	_, inWindow := window.active(now)
	tc.mu.Lock()
	trigger := ""
	switch {
	case !tc.booted:
		trigger = "boot"
	case now.Sub(lastRun) >= tuningInterval:
		trigger = "schedule"
	case pending && inWindow:
		trigger = "window"
	case now.Sub(lastSample) >= tuningSampleInterval:
		trigger = "sample"
	}
	if trigger == "" || tc.running {
		tc.mu.Unlock()
		return
	}
	tc.running, tc.booted = true, true
	tc.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() { tc.mu.Lock(); tc.running = false; tc.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
		defer cancel()
		m.tuningCycle(ctx, trigger, now)
	}()
}

// startTuningCycle runs a cycle at once for the panel.
func (m *Manager) startTuningCycle() error {
	if err := m.hostOperationsAllowed(); err != nil {
		return err
	}
	tc := m.tuning()
	tc.mu.Lock()
	if tc.running {
		tc.mu.Unlock()
		return errors.New("resource tuning is already running")
	}
	tc.running, tc.booted = true, true
	tc.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() { tc.mu.Lock(); tc.running = false; tc.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
		defer cancel()
		m.tuningCycle(ctx, "manual", time.Now().UTC())
	}()
	return nil
}

// tuningCycle samples sessions and, unless only sampling is due, plans and
// applies tuning, then restarts the cluster when the window allows.
func (m *Manager) tuningCycle(ctx context.Context, trigger string, now time.Time) {
	var obs tuningObservation
	var sample map[string]int
	if trigger == "sample" {
		sample = m.sampleTuningSessions(ctx)
	} else {
		obs, sample = m.observeTuning(ctx)
	}
	m.mu.Lock()
	state := m.tuningStateLocked()
	if sample != nil {
		state.Peaks = recordTuningPeaks(state.Peaks, sample, now)
		state.LastSample = now
		if state.FirstSample.IsZero() {
			state.FirstSample = now
		}
	}
	obs.CapsLearning = tuningCapsLearning(state.FirstSample, now)
	overrides, peaks, probes := state.Overrides, state.Peaks, maps.Clone(state.RedisUses)
	m.mu.Unlock()
	if trigger == "sample" {
		m.mu.Lock()
		_ = m.save()
		m.mu.Unlock()
		return
	}
	view := planTuning(obs, overrides, peaks, probes)
	view.Computed = now
	var out strings.Builder
	applied := 0
	failed := []string{}
	hc := m.host()
	hc.mu.Lock()
	otherJob := hc.job
	if otherJob == "" && !overrides.Paused {
		hc.job = tuningJobKind
	}
	hc.mu.Unlock()
	switch {
	case overrides.Paused:
		view.Warnings = append(view.Warnings, "Automatic tuning is paused; targets are computed but not applied.")
	case otherJob != "":
		view.Warnings = append(view.Warnings, "Tuning waits: host "+otherJob+" is running.")
	default:
		applied, failed = m.applyTuning(ctx, obs, &view, &out)
		if restarted, note := m.maybeRestartPostgres(ctx, obs, &view, now, &out); restarted {
			applied++
			// Observe again: the restart applied the pending settings.
			if again, _ := m.observeTuning(ctx); again.PG != nil {
				fresh := planTuning(again, overrides, peaks, probes)
				view.Postgres, view.PendingRestart, view.Changes = fresh.Postgres, fresh.PendingRestart, fresh.Changes
			}
		} else {
			view.RestartNote = note
		}
		hc.mu.Lock()
		hc.job = ""
		hc.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state = m.tuningStateLocked()
	if otherJob == "" {
		state.LastRun = now
	}
	policy := m.hostPolicyLocked()
	view.NextWindow = policy.RebootWindow.next(now)
	if len(view.PendingRestart) > 0 && view.RestartNote == "" {
		view.RestartNote = "applies at the next PostgreSQL restart, in the maintenance window starting " + view.NextWindow.Format(time.RFC3339)
	}
	state.View = &view
	if applied > 0 || len(failed) > 0 {
		job := newHostJob(tuningJobKind, trigger, now)
		finished := time.Now().UTC()
		job.Finished = &finished
		job.Output = strings.TrimSpace(out.String())
		job.Status = "succeeded"
		job.Summary = fmt.Sprintf("Applied %d tuning change%s.", applied, plural(applied))
		if len(view.PendingRestart) > 0 {
			job.Summary += " " + strings.Join(view.PendingRestart, ", ") + " wait for a PostgreSQL restart."
		}
		if len(failed) > 0 {
			job.Status = "failed"
			job.Summary = fmt.Sprintf("Applied %d change%s; %d failed: %s", applied, plural(applied), len(failed), strings.Join(failed, "; "))
		}
		m.recordHostJobLocked(job)
		log.Printf("resource tuning (%s): %s", trigger, job.Summary)
	}
	if err := m.save(); err != nil {
		log.Printf("resource tuning state: %v", err)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---- observation -------------------------------------------------------------

// observeTuning gathers the observation and a session sample per role
// ("port/role" → sessions); the sample is nil without a host cluster.
func (m *Manager) observeTuning(ctx context.Context) (tuningObservation, map[string]int) {
	obs := tuningObservation{Host: observeTuningHost("/")}
	h := nativeHostDataHost(m)
	var pg *tuningPGObs
	var roles map[string][3]int // limit, super, in use
	clusters, _ := h.listClusters(ctx)
	if cluster, err := chooseNativePGCluster(clusters, h.cfg.PostgresVersion, h.cfg.PostgresPort); err == nil {
		pg, roles, err = observeTuningPostgres(ctx, h, cluster)
		if err != nil {
			obs.Warnings = append(obs.Warnings, "PostgreSQL "+cluster.ID()+": "+err.Error())
			pg = nil
		} else if cluster.DataDir != "" {
			if disk := observeTuningHost(cluster.DataDir).DiskGiB; disk > 0 {
				obs.Host.DiskGiB = disk
			}
		}
	}
	obs.PG = pg
	shared, sharedOK := readSharedRedis()
	obs.Apps = m.tuningApps(ctx, obs.Host.CPUs, pg, shared, sharedOK)
	var sample map[string]int
	if pg != nil {
		sample = map[string]int{}
		for role, info := range roles {
			if info[2] > 0 {
				sample[tuningPeakKey(pg.Port, role)] = info[2]
			}
		}
		for i := range obs.Apps {
			a := &obs.Apps[i]
			if info, ok := roles[a.Role]; ok && a.Role != "" {
				a.RoleKnown, a.RoleLimit, a.RoleSuper, a.InUse = true, info[0], info[1] == 1, info[2]
			}
		}
	}
	obs.Redis = m.observeTuningRedis(ctx, h, obs.Apps, shared, sharedOK)
	m.observeTuningClickHouse(ctx, &obs)
	obs.ContainerDBs = m.observeTuningContainerDBs(ctx)
	return obs, sample
}

// tuningCapsLearning reports whether sampling has run for less than a day.
func tuningCapsLearning(first, now time.Time) bool {
	return first.IsZero() || now.Sub(first) < 24*time.Hour
}

// sampleTuningSessions counts the host cluster's sessions per role.
func (m *Manager) sampleTuningSessions(ctx context.Context) map[string]int {
	h := nativeHostDataHost(m)
	clusters, _ := h.listClusters(ctx)
	cluster, err := chooseNativePGCluster(clusters, h.cfg.PostgresVersion, h.cfg.PostgresPort)
	if err != nil {
		return nil
	}
	out, err := h.psql(ctx, cluster.Port, "SELECT usename, count(*) FROM pg_catalog.pg_stat_activity WHERE backend_type = 'client backend' AND usename IS NOT NULL GROUP BY usename;\n")
	if err != nil {
		return nil
	}
	sample := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		role, count, ok := strings.Cut(strings.TrimSpace(line), "|")
		if n, err := strconv.Atoi(count); ok && err == nil && n > 0 {
			sample[tuningPeakKey(cluster.Port, role)] = n
		}
	}
	return sample
}

func observeTuningHost(path string) TuningHost {
	host := TuningHost{CPUs: runtime.NumCPU(), MemoryMiB: hostTotalMemoryMiB()}
	var st syscall.Statfs_t
	if syscall.Statfs(hostPath(path), &st) == nil {
		host.DiskGiB = int(st.Blocks * uint64(st.Bsize) >> 30)
	}
	devices, _ := filepath.Glob(hostPath("/sys/block/*/queue/rotational"))
	rotational, solid := 0, 0
	for _, file := range devices {
		name := filepath.Base(filepath.Dir(filepath.Dir(file)))
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") || strings.HasPrefix(name, "sr") {
			continue
		}
		if raw, err := os.ReadFile(file); err == nil && strings.TrimSpace(string(raw)) == "1" {
			rotational++
		} else {
			solid++
		}
	}
	host.Rotational = rotational > 0 && solid == 0
	return host
}

var tuningIncludeDir = regexp.MustCompile(`(?m)^\s*include_dir\s*=?\s*'conf\.d'`)

// observeTuningPostgres reads the cluster's tuned settings, its conf.d
// include and every role's limit and sessions.
func observeTuningPostgres(ctx context.Context, h nativeHostData, cluster pgCluster) (*tuningPGObs, map[string][3]int, error) {
	names := make([]string, 0, len(tunablePostgres))
	for name := range tunablePostgres {
		names = append(names, "'"+name+"'")
	}
	slices.Sort(names)
	out, err := h.psql(ctx, cluster.Port, "SELECT name, current_setting(name), pending_restart FROM pg_catalog.pg_settings WHERE name IN ("+strings.Join(names, ",")+");\n")
	if err != nil {
		return nil, nil, errors.New("settings are unavailable")
	}
	pg := &tuningPGObs{Cluster: cluster.ID(), Major: cluster.Major, Port: cluster.Port, Current: map[string]string{}, Pending: map[string]bool{}}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) == 3 {
			pg.Current[parts[0]] = parts[1]
			pg.Pending[parts[0]] = parts[2] == "t"
		}
	}
	if file, err := h.psql(ctx, cluster.Port, "SHOW config_file;\n"); err == nil && filepath.IsAbs(file) && filepath.Clean(file) == file {
		pg.ConfFile = filepath.Join(filepath.Dir(file), "conf.d", tuningConfName)
		if raw, err := os.ReadFile(h.path(file)); err == nil {
			pg.IncludeOK = tuningIncludeDir.Match(raw)
		}
	}
	roles := map[string][3]int{}
	if out, err := h.psql(ctx, cluster.Port, "SELECT r.rolname, r.rolconnlimit, CASE WHEN r.rolsuper THEN 1 ELSE 0 END, (SELECT count(*) FROM pg_catalog.pg_stat_activity a WHERE a.usename = r.rolname AND a.backend_type = 'client backend') FROM pg_catalog.pg_roles r WHERE r.rolcanlogin;\n"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			parts := strings.Split(strings.TrimSpace(line), "|")
			if len(parts) != 4 {
				continue
			}
			limit, _ := strconv.Atoi(parts[1])
			super, _ := strconv.Atoi(parts[2])
			used, _ := strconv.Atoi(parts[3])
			roles[parts[0]] = [3]int{limit, super, used}
			pg.InUse += used
		}
	}
	return pg, roles, nil
}

// tuningApps lists hosted applications with their database role, pool
// and Redis uses.
func (m *Manager) tuningApps(ctx context.Context, cpus int, pg *tuningPGObs, shared sharedRedisServer, sharedOK bool) []tuningAppObs {
	m.mu.Lock()
	natives := maps.Clone(m.data.NativeApps)
	records := maps.Clone(m.data.NativeHostData)
	containers := map[string]App{}
	for id, a := range m.data.Apps {
		if a.Native == nil && !a.Preview {
			containers[id] = a
		}
	}
	m.mu.Unlock()
	port := 0
	if pg != nil {
		port = pg.Port
	}
	out := []tuningAppObs{}
	for id, a := range natives {
		app := tuningAppObs{ID: id, Runtime: a.Runtime}
		var env map[string]string
		units := a.Services
		osUser := ""
		if m.cfg.Native != nil {
			osUser = m.cfg.Native.User
		}
		if _, policy, err := m.nativeReleasePolicy(id); err == nil {
			app.ReleaseManaged = len(policy.EnvFiles) > 0
			if policy.Runtime != "" && policy.Runtime != "auto" {
				app.Runtime = policy.Runtime
			}
			if policy.User != "" {
				osUser = policy.User
			}
			units = policy.Units
			env, err = nativeReleaseEnvValues(policy)
			if err != nil {
				app.MoveBlocked = "its environment files are not plain assignments"
			}
		} else if m.cfg.Native != nil {
			env = m.nativeEnvironment(a.Name)
		}
		if app.Runtime == "" {
			app.Runtime = "rails"
		}
		app.Pool, app.PoolSource = appConnectionPool(app.Runtime, env, units, cpus)
		if r, ok := records[id]; ok && r.Postgres != nil && r.Postgres.Port == port {
			app.Role = r.Postgres.Name
		} else if port > 0 {
			app.Role = pgRoleFromURL(env["DATABASE_URL"], port, osUser)
		}
		if sharedOK {
			app.SharedRedis = sharedRedisUses(env, shared)
		}
		out = append(out, app)
	}
	for id, a := range containers {
		app := tuningAppObs{ID: id, Runtime: a.Runtime, MoveBlocked: "container applications have their own cache"}
		app.Pool, app.PoolSource = appConnectionPool(app.Runtime, a.Env, nil, cpus)
		if port > 0 {
			app.Role = pgRoleFromURL(a.Env["DATABASE_URL"], port, "")
		}
		out = append(out, app)
	}
	slices.SortFunc(out, func(a, b tuningAppObs) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// pgRoleFromURL returns the role a DATABASE_URL uses on the host cluster at
// port, or "" when it points elsewhere.
func pgRoleFromURL(raw string, port int, osUser string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return ""
	}
	host := u.Hostname()
	if q := u.Query().Get("host"); q != "" {
		host = q
	}
	local := host == "" || host == "127.0.0.1" || host == "localhost" || host == "::1" || strings.HasPrefix(host, "/") || host == "172.17.0.1" || host == "host.docker.internal"
	if !local {
		return ""
	}
	p := u.Port()
	if q := u.Query().Get("port"); q != "" {
		p = q
	}
	if p == "" {
		p = "5432"
	}
	if p != strconv.Itoa(port) {
		return ""
	}
	if name := u.User.Username(); name != "" {
		return name
	}
	if q := u.Query().Get("user"); q != "" {
		return q
	}
	return osUser
}

// observeTuningRedis measures every Redis the tuner sizes and probes uses
// whose classification is older than a day.
func (m *Manager) observeTuningRedis(ctx context.Context, h nativeHostData, apps []tuningAppObs, shared sharedRedisServer, sharedOK bool) []tuningRedisObs {
	out := []tuningRedisObs{}
	m.mu.Lock()
	probes := maps.Clone(m.tuningStateLocked().RedisUses)
	records := maps.Clone(m.data.NativeHostData)
	caches := map[string]CacheResource{}
	for id, c := range m.data.Caches {
		if c.Mode == "local" && c.Ready && c.Network != "" {
			caches[id] = c
		}
	}
	m.mu.Unlock()
	fresh := map[string]TuningProbe{}
	measure := func(c redisConn, r *tuningRedisObs) {
		info, err := c.do(ctx, "INFO", "memory")
		if err != nil {
			r.Error = "INFO failed"
			return
		}
		text := redisString(info)
		r.UsedBytes, _ = strconv.ParseInt(redisInfoField(text, "used_memory"), 10, 64)
		r.MaxBytes, _ = strconv.ParseInt(redisInfoField(text, "maxmemory"), 10, 64)
		r.Policy = redisInfoField(text, "maxmemory_policy")
	}
	probe := func(c redisConn, app string, dbs []int) {
		if p, ok := probes[app]; ok && time.Since(p.At) < tuningProbeMaxAge {
			return
		}
		best := TuningProbe{At: time.Now().UTC()}
		for _, db := range dbs {
			use, n, err := probeRedisUse(ctx, c, db, 1000)
			if err != nil {
				return
			}
			best.Sampled += n
			if redisUseRank(use) > redisUseRank(best.Use) {
				best.Use = use
			}
		}
		fresh[app] = best
	}
	if sharedOK {
		r := tuningRedisObs{ID: "shared", Kind: "shared", AppDBs: map[string][]int{}, Uses: map[string]TuningProbe{}}
		for _, a := range apps {
			if len(a.SharedRedis) > 0 {
				r.Apps = append(r.Apps, a.ID)
				r.AppDBs[a.ID] = uniqueDBs(a.SharedRedis)
				slices.Sort(r.AppDBs[a.ID])
			}
		}
		if c, err := shared.dial(ctx); err == nil {
			measure(c, &r)
			for _, app := range r.Apps {
				probe(c, app, r.AppDBs[app])
			}
			c.close()
			out = append(out, r)
		} else if len(r.Apps) > 0 {
			r.Error = err.Error()
			out = append(out, r)
		}
	}
	for id, record := range records {
		redis := record.Redis
		if redis == nil || !redis.Ready {
			continue
		}
		r := tuningRedisObs{ID: redis.Unit, Kind: "own", Apps: []string{id}, AppDBs: map[string][]int{}, Uses: map[string]TuningProbe{}}
		password, err := m.unsealHostSecret(redis.Password)
		if err == nil {
			var c redisConn
			if c, err = dialTuningRedis(ctx, "unix", h.path(redis.Socket), password); err == nil {
				measure(c, &r)
				dbs := []int{}
				if ks, kerr := c.do(ctx, "INFO", "keyspace"); kerr == nil {
					for db := range redisKeyspace(redisString(ks)) {
						dbs = append(dbs, db)
					}
				}
				slices.Sort(dbs)
				r.AppDBs[id] = dbs
				probe(c, id, dbs)
				c.close()
			}
		}
		if err != nil {
			r.Error = err.Error()
		}
		out = append(out, r)
	}
	for id, c := range caches {
		r := tuningRedisObs{ID: c.Network, Kind: "container", Apps: []string{id}}
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		info, err := command(probeCtx, "", []string{"REDISCLI_AUTH=" + c.LocalPassword}, "docker", "exec", "--env", "REDISCLI_AUTH", c.Network, "valkey-cli", "INFO", "memory")
		cancel()
		if err != nil {
			r.Error = "INFO failed"
		} else {
			r.UsedBytes, _ = strconv.ParseInt(redisInfoField(info, "used_memory"), 10, 64)
			r.MaxBytes, _ = strconv.ParseInt(redisInfoField(info, "maxmemory"), 10, 64)
			r.Policy = redisInfoField(info, "maxmemory_policy")
		}
		out = append(out, r)
	}
	if len(fresh) > 0 {
		m.mu.Lock()
		state := m.tuningStateLocked()
		if state.RedisUses == nil {
			state.RedisUses = map[string]TuningProbe{}
		}
		maps.Copy(state.RedisUses, fresh)
		m.mu.Unlock()
		for i := range out {
			for _, app := range out[i].Apps {
				if p, ok := fresh[app]; ok {
					out[i].Uses[app] = p
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b tuningRedisObs) int { return strings.Compare(a.Kind+a.ID, b.Kind+b.ID) })
	return out
}

var clickhouseRatioTag = regexp.MustCompile(`<max_server_memory_usage_to_ram_ratio>([0-9.]+)</max_server_memory_usage_to_ram_ratio>`)

func (m *Manager) observeTuningClickHouse(ctx context.Context, obs *tuningObservation) {
	m.mu.Lock()
	server := m.data.NativeClickHouse
	records := maps.Clone(m.data.NativeHostData)
	containers := map[string]ClickHouseResource{}
	for id, c := range m.data.ClickHouses {
		if _, live := m.data.Apps[id]; live && c.Ready && c.Network != "" {
			containers[id] = c
		}
	}
	m.mu.Unlock()
	h := nativeHostDataHost(m)
	if server != nil && server.Installed {
		ch := &tuningCHObs{ID: "host", Query: map[string]int{}}
		if raw, err := os.ReadFile(h.path(nativeCHConfigFile)); err == nil {
			if match := clickhouseRatioTag.FindSubmatch(raw); match != nil {
				ch.Ratio, _ = strconv.ParseFloat(string(match[1]), 64)
			}
		}
		for _, r := range records {
			if r.ClickHouse != nil && r.ClickHouse.Ready {
				ch.Users = append(ch.Users, r.ClickHouse.Name)
				ch.Query[r.ClickHouse.Name] = r.ClickHouse.QueryMemoryMiB
			}
		}
		slices.Sort(ch.Users)
		obs.NativeCH = ch
	}
	ids := slices.Sorted(maps.Keys(containers))
	for _, id := range ids {
		c := containers[id]
		ch := tuningCHObs{ID: c.Network, Users: []string{id}}
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		if have, err := clickhouseDockerCommand(probe, c, "read ClickHouse memory policy", nil, nil, "exec", "--user", "0", c.Network, "cat", clickhouseMemoryFile); err == nil {
			if match := clickhouseRatioTag.FindStringSubmatch(have); match != nil {
				ch.Ratio, _ = strconv.ParseFloat(match[1], 64)
			}
		}
		cancel()
		obs.ContainerCH = append(obs.ContainerCH, ch)
	}
}

func (m *Manager) observeTuningContainerDBs(ctx context.Context) []tuningContainerDBObs {
	m.mu.Lock()
	dbs := []Database{}
	for _, d := range m.data.Databases {
		if d.Mode == "local" && d.Ready && d.Network != "" && !d.Ephemeral {
			dbs = append(dbs, d)
		}
	}
	m.mu.Unlock()
	slices.SortFunc(dbs, func(a, b Database) int { return strings.Compare(a.AppID, b.AppID) })
	quoted := []string{}
	for _, name := range containerPGSettings {
		quoted = append(quoted, "'"+name+"'")
	}
	out := []tuningContainerDBObs{}
	for _, d := range dbs {
		o := tuningContainerDBObs{AppID: d.AppID, Container: d.Network, Current: map[string]string{}, Pending: map[string]bool{}}
		probe, cancel := context.WithTimeout(ctx, 15*time.Second)
		text, err := command(probe, "", nil, "docker", "exec", d.Network, "psql", "-X", "-U", "postgres", "-d", "postgres", "-At", "-c", "SELECT name, current_setting(name), pending_restart FROM pg_catalog.pg_settings WHERE name IN ("+strings.Join(quoted, ",")+")")
		cancel()
		if err != nil {
			o.Error = "the database container did not answer"
		}
		for _, line := range strings.Split(text, "\n") {
			parts := strings.Split(strings.TrimSpace(line), "|")
			if len(parts) == 3 {
				o.Current[parts[0]], o.Pending[parts[0]] = parts[1], parts[2] == "t"
			}
		}
		out = append(out, o)
	}
	return out
}

// ---- application -------------------------------------------------------------

// applyTuning applies the plan's live and reload changes and writes the
// restart-only settings; restart-only values take effect at the next
// cluster restart. It returns the number of applied changes and the
// failures; every change is logged old → new.
func (m *Manager) applyTuning(ctx context.Context, obs tuningObservation, view *ResourceTuningView, out *strings.Builder) (int, []string) {
	applied := 0
	failed := []string{}
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		out.WriteString(line + "\n")
	}
	fail := func(what string, err error) {
		failed = append(failed, what+": "+err.Error())
		logf("FAILED %s: %v", what, err)
	}
	h := nativeHostDataHost(m)
	changed := func(resource, setting string) (TuningChange, bool) {
		for _, c := range view.Changes {
			if c.Resource == resource && c.Setting == setting {
				return c, true
			}
		}
		return TuningChange{}, false
	}
	if pg := obs.PG; pg != nil && view.Postgres != nil && pg.IncludeOK && pg.ConfFile != "" {
		settings := map[string]string{}
		for _, s := range view.Postgres.Settings {
			settings[s.Name] = s.Target
		}
		written, err := m.writePostgresTuning(ctx, h, pg, renderPostgresTuningConf(settings, obs.Host, view.Apps), logf)
		if err != nil {
			fail("PostgreSQL configuration", err)
		} else if written {
			for _, c := range view.Changes {
				if strings.HasPrefix(c.Resource, "postgresql ") {
					logf("%s", c)
					applied++
				}
			}
		}
		for _, c := range view.Caps {
			change, ok := changed("role "+c.Role, "CONNECTION LIMIT")
			if !ok {
				continue
			}
			if change.Apply == "deferred" {
				logf("%s", change)
				continue
			}
			ident, err := pgQuoteIdent(c.Role)
			if err != nil {
				continue
			}
			if _, err := h.psql(ctx, pg.Port, fmt.Sprintf("ALTER ROLE %s CONNECTION LIMIT %d;\n", ident, c.Target)); err != nil {
				fail("connection limit of "+c.Role, err)
				continue
			}
			logf("%s", change)
			applied++
		}
	}
	for _, r := range view.Redis {
		limit, lok := changed("redis "+r.ID, "maxmemory")
		policy, pok := changed("redis "+r.ID, "maxmemory-policy")
		if !lok && !pok {
			continue
		}
		if err := m.applyRedisTarget(ctx, h, obs, r); err != nil {
			fail("Redis "+r.ID, err)
			continue
		}
		for _, c := range []struct {
			change TuningChange
			ok     bool
		}{{limit, lok}, {policy, pok}} {
			if c.ok {
				logf("%s", c.change)
				applied++
			}
		}
	}
	for _, ch := range view.ClickHouse {
		n, err := m.applyClickHouseTarget(ctx, h, ch, view.Changes, logf)
		applied += n
		if err != nil {
			fail("ClickHouse "+ch.ID, err)
		}
	}
	for _, db := range view.Containers {
		n, err := m.applyContainerDBTarget(ctx, db, view.Changes, logf)
		applied += n
		if err != nil {
			fail("container database "+db.AppID, err)
		}
	}
	return applied, failed
}

// writePostgresTuning replaces the managed conf.d file, keeps it only when
// PostgreSQL reads it without errors, resets ALTER SYSTEM values that would
// shadow it and reloads the configuration (no restart).
func (m *Manager) writePostgresTuning(ctx context.Context, h nativeHostData, pg *tuningPGObs, content string, logf func(string, ...any)) (bool, error) {
	path := pg.ConfFile
	old, readErr := os.ReadFile(h.path(path))
	if readErr == nil && string(old) == content {
		return false, nil
	}
	const countErrors = "SELECT count(*) FROM pg_catalog.pg_file_settings WHERE error IS NOT NULL;\n"
	before, err := h.psql(ctx, pg.Port, countErrors)
	if err != nil {
		return false, err
	}
	if _, err = h.writeIfChanged(path, []byte(content), 0o644, 0, 0); err != nil {
		return false, err
	}
	after, err := h.psql(ctx, pg.Port, countErrors)
	if err != nil || after != before {
		if readErr == nil {
			_, _ = h.writeIfChanged(path, old, 0o644, 0, 0)
		} else {
			_ = os.Remove(h.path(path))
		}
		return false, errors.New("PostgreSQL rejected the tuned configuration; the previous file was restored")
	}
	// Values set with ALTER SYSTEM (postgresql.auto.conf) are read after
	// conf.d and would win; the panel's overrides replace them.
	names := []string{}
	for name := range tunablePostgres {
		names = append(names, "'"+name+"'")
	}
	slices.Sort(names)
	shadow, _ := h.psql(ctx, pg.Port, "SELECT name, setting FROM pg_catalog.pg_file_settings WHERE applied AND sourcefile LIKE '%postgresql.auto.conf' AND name IN ("+strings.Join(names, ",")+");\n")
	for _, line := range strings.Split(shadow, "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || !tunablePostgres[name] {
			continue
		}
		if _, err := h.psql(ctx, pg.Port, "ALTER SYSTEM RESET "+name+";\n"); err == nil {
			logf("postgresql %s: removed the ALTER SYSTEM value %s %s so the tuned value applies", pg.Cluster, name, value)
		}
	}
	if _, err = h.psql(ctx, pg.Port, "SELECT pg_catalog.pg_reload_conf();\n"); err != nil {
		return true, err
	}
	logf("postgresql %s: wrote %s and reloaded the configuration", pg.Cluster, path)
	return true, nil
}

// applyRedisTarget sets an instance's limit and policy live; for the
// agent's own instances it also rewrites their configuration and unit
// memory cap so a restart keeps them.
func (m *Manager) applyRedisTarget(ctx context.Context, h nativeHostData, obs tuningObservation, r TuningRedisView) error {
	set := func(c redisConn) error {
		if _, err := c.do(ctx, "CONFIG", "SET", "maxmemory", strconv.Itoa(r.TargetMiB)+"mb"); err != nil {
			return err
		}
		_, err := c.do(ctx, "CONFIG", "SET", "maxmemory-policy", r.TargetPolicy)
		return err
	}
	switch r.Kind {
	case "shared":
		// The distribution's configuration file is not ours: values are set
		// live and set again after every agent start.
		server, ok := readSharedRedis()
		if !ok {
			return errors.New("the shared Redis configuration is unreadable")
		}
		c, err := server.dial(ctx)
		if err != nil {
			return err
		}
		defer c.close()
		return set(c)
	case "own":
		if len(r.Apps) != 1 {
			return errors.New("instance without an application")
		}
		return m.applyOwnRedisLimit(ctx, h, r.Apps[0].AppID, r.TargetMiB, r.TargetPolicy)
	case "container":
		m.mu.Lock()
		var cache CacheResource
		found := false
		for _, c := range m.data.Caches {
			if c.Network == r.ID {
				cache, found = c, true
			}
		}
		m.mu.Unlock()
		if !found {
			return errors.New("the cache container is no longer recorded")
		}
		for _, args := range [][]string{{"CONFIG", "SET", "maxmemory", strconv.Itoa(r.TargetMiB) + "mb"}, {"CONFIG", "SET", "maxmemory-policy", r.TargetPolicy}, {"CONFIG", "REWRITE"}} {
			full := append([]string{"exec", "--env", "REDISCLI_AUTH", cache.Network, "valkey-cli"}, args...)
			if out, err := command(ctx, "", []string{"REDISCLI_AUTH=" + cache.LocalPassword}, "docker", full...); err != nil || strings.HasPrefix(strings.TrimSpace(out), "ERR") {
				return fmt.Errorf("valkey-cli %s failed", strings.Join(args, " "))
			}
		}
		return nil
	}
	return nil
}

// applyOwnRedisLimit sizes lidza-redis@<id>: live, in its configuration
// file, in the unit's memory cap and in the provisioning record.
func (m *Manager) applyOwnRedisLimit(ctx context.Context, h nativeHostData, id string, mib int, policy string) error {
	record, ok := m.hostDataRecord(id)
	if !ok || record.Redis == nil {
		return errors.New("no Redis instance is recorded for " + id)
	}
	redis := *record.Redis
	password, err := m.unsealHostSecret(redis.Password)
	if err != nil {
		return err
	}
	used := int64(0)
	if c, err := dialTuningRedis(ctx, "unix", h.path(redis.Socket), password); err == nil {
		if info, ierr := c.do(ctx, "INFO", "memory"); ierr == nil {
			used, _ = strconv.ParseInt(redisInfoField(redisString(info), "used_memory"), 10, 64)
		}
		if policy == "noeviction" && int64(mib)<<20 < used {
			c.close()
			return fmt.Errorf("refused: %d MiB is below the %d MiB in use and the instance never evicts", mib, used>>20)
		}
		_, err1 := c.do(ctx, "CONFIG", "SET", "maxmemory", strconv.Itoa(mib)+"mb")
		_, err2 := c.do(ctx, "CONFIG", "SET", "maxmemory-policy", policy)
		c.close()
		if err := errors.Join(err1, err2); err != nil {
			return err
		}
	} else if redis.Ready {
		return err
	}
	dir := filepath.Dir(redis.Socket)
	gid := 0
	if account, lerr := nativeLookupUser(redis.User); lerr == nil {
		gid, _ = strconv.Atoi(account.Gid)
		if _, err := h.writeIfChanged(filepath.Join(nativeRedisConfigDir, id+".conf"), []byte(renderNativeRedisConfigPolicy(id, dir, password, mib, policy)), 0o640, 0, gid); err != nil {
			return err
		}
		uid, _ := strconv.Atoi(account.Uid)
		unit := nativeRedisUnit(id)
		dropIn, err := h.writeIfChanged(filepath.Join("/etc/systemd/system", unit+".d", "lidza.conf"), []byte(renderNativeRedisDropIn(NativeAppAccount{User: redis.User, UID: uid, GID: gid}, dir, mib)), 0o644, 0, 0)
		if err != nil {
			return err
		}
		if dropIn {
			// systemd applies a changed MemoryMax to the running unit on reload.
			if out, err := h.run(ctx, "", "/usr/bin/systemctl", "daemon-reload"); err != nil {
				return fmt.Errorf("systemd reload failed: %s", nativeTrimOutput(out))
			}
		}
	}
	return m.updateHostDataRecord(id, func(r *NativeHostDataRecord) {
		if r.Redis != nil {
			r.Redis.MaxMemoryMiB, r.Redis.Policy, r.Redis.Tuned = mib, policy, true
		}
	})
}

// applyClickHouseTarget applies one server's memory ratio and, for the
// native server, every user's per-query limit. ClickHouse reloads changed
// configuration files itself.
func (m *Manager) applyClickHouseTarget(ctx context.Context, h nativeHostData, ch TuningClickHouseView, changes []TuningChange, logf func(string, ...any)) (int, error) {
	applied := 0
	if ch.Kind == "container" {
		for _, c := range changes {
			if c.Resource != "clickhouse "+ch.ID {
				continue
			}
			m.mu.Lock()
			var resource ClickHouseResource
			for _, r := range m.data.ClickHouses {
				if r.Network == ch.ID {
					resource = r
				}
			}
			m.mu.Unlock()
			if resource.Network == "" {
				return 0, errors.New("the ClickHouse container is no longer recorded")
			}
			_, err := clickhouseDockerCommand(ctx, resource, "write ClickHouse memory policy", strings.NewReader(renderClickHouseMemoryConfig(ch.TargetRatio)), nil, "exec", "-i", "--user", "0", resource.Network,
				"sh", "-c", `f="$1"; t="$f.lidza-tmp"; cat > "$t" && chmod 644 "$t" && mv -f "$t" "$f"`, "lidza-memory", clickhouseMemoryFile)
			if err != nil {
				return 0, err
			}
			logf("%s", c)
			applied++
		}
		return applied, nil
	}
	ratioChange := false
	for _, c := range changes {
		if c.Resource == "clickhouse host" {
			ratioChange = true
		}
	}
	m.mu.Lock()
	server := m.data.NativeClickHouse
	records := maps.Clone(m.data.NativeHostData)
	m.mu.Unlock()
	if server == nil {
		return 0, nil
	}
	admin, err := m.unsealHostSecret(server.Password)
	if err != nil {
		return 0, err
	}
	if ratioChange {
		tc := m.tuning()
		tc.mu.Lock()
		tc.chRatio = ch.TargetRatio
		tc.mu.Unlock()
		m.mu.Lock()
		m.tuningStateLocked().ClickHouseRatio = ch.TargetRatio
		m.mu.Unlock()
		h.cfg.ClickHouseMemoryRatio = ch.TargetRatio
		written, err := h.writeClickHouseFiles(*server, admin, false)
		if err != nil {
			return 0, err
		}
		if written {
			if out, err := h.run(ctx, "", "/usr/bin/systemctl", "daemon-reload"); err != nil {
				return 0, fmt.Errorf("systemd reload failed: %s", nativeTrimOutput(out))
			}
		}
		for _, c := range changes {
			if c.Resource == "clickhouse host" {
				logf("%s", c)
				applied++
			}
		}
	}
	client := h.clickhouse(*server, "default", admin)
	for id, r := range records {
		if r.ClickHouse == nil || !r.ClickHouse.Ready || r.ClickHouse.QueryMemoryMiB == ch.QueryMemoryMiB {
			continue
		}
		if !clickhouseNamePattern.MatchString(r.ClickHouse.Name) {
			continue
		}
		statement := fmt.Sprintf("ALTER SETTINGS PROFILE %s SETTINGS max_memory_usage = %d, max_execution_time = %d, max_concurrent_queries_for_user = 32", mustCHIdent(r.ClickHouse.Name), int64(ch.QueryMemoryMiB)<<20, nativeCHMaxExecution)
		if err := client.exec(ctx, statement); err != nil {
			return applied, err
		}
		old := r.ClickHouse.QueryMemoryMiB
		if err := m.updateHostDataRecord(id, func(rec *NativeHostDataRecord) {
			if rec.ClickHouse != nil {
				rec.ClickHouse.QueryMemoryMiB = ch.QueryMemoryMiB
			}
		}); err != nil {
			return applied, err
		}
		logf("clickhouse %s max_memory_usage: %d MiB → %d MiB (live)", r.ClickHouse.Name, old, ch.QueryMemoryMiB)
		applied++
	}
	return applied, nil
}

// applyContainerDBTarget sets a container database's targets with ALTER
// SYSTEM (its own cluster's configuration) and reloads; settings that need
// a restart apply at the container's next restart.
func (m *Manager) applyContainerDBTarget(ctx context.Context, db TuningContainerView, changes []TuningChange, logf func(string, ...any)) (int, error) {
	var script strings.Builder
	selected := []TuningChange{}
	for _, c := range changes {
		if c.Resource != "container database "+db.AppID || !tuningValuePattern.MatchString(c.New) || !slices.Contains(containerPGSettings, c.Setting) {
			continue
		}
		fmt.Fprintf(&script, "ALTER SYSTEM SET %s = '%s';\n", c.Setting, c.New)
		selected = append(selected, c)
	}
	if len(selected) == 0 {
		return 0, nil
	}
	script.WriteString("SELECT pg_catalog.pg_reload_conf();\n")
	if err := dockerStream(ctx, strings.NewReader(script.String()), nil, nil, "exec", "-i", db.Container, "psql", "-X", "-q", "-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1"); err != nil {
		return 0, err
	}
	for _, c := range selected {
		logf("%s", c)
	}
	return len(selected), nil
}

// ---- restart -------------------------------------------------------------------

// maybeRestartPostgres restarts the host cluster in the maintenance window
// to apply pending restart-only settings (restartDecision).
func (m *Manager) maybeRestartPostgres(ctx context.Context, obs tuningObservation, view *ResourceTuningView, now time.Time, out *strings.Builder) (bool, string) {
	if obs.PG == nil || len(view.PendingRestart) == 0 || !obs.PG.IncludeOK {
		return false, ""
	}
	m.mu.Lock()
	policy := m.hostPolicyLocked()
	busy := m.hostBusyLocked()
	if busy == "" && (len(m.data.MoveReplications) > 0 || len(m.data.MoveTunnels) > 0) {
		busy = "a move replicates through this server"
	}
	tc := m.tuning()
	tc.mu.Lock()
	if busy == "" && len(tc.migrating) > 0 {
		busy = "a Redis migration is running"
	}
	tc.mu.Unlock()
	last := m.tuningStateLocked().LastRestart
	m.mu.Unlock()
	_, inWindow := policy.RebootWindow.active(now)
	required, _, _ := rebootRequirement()
	since := -1.0
	if !last.IsZero() {
		since = now.Sub(last).Hours()
	}
	newMax := 0
	if view.Postgres != nil {
		for _, s := range view.Postgres.Settings {
			if s.Name == "max_connections" {
				newMax, _ = strconv.Atoi(s.Target)
			}
		}
	}
	next := policy.RebootWindow.next(now)
	restart, note := restartDecision(restartInput{Pending: true, InWindow: inWindow, NextWindow: next.Format(time.RFC3339), Busy: busy, RebootPlanned: policy.AutoReboot && required, SinceLast: since, InUse: obs.PG.InUse, NewMax: newMax})
	if !restart {
		return false, note
	}
	major, name, _ := strings.Cut(obs.PG.Cluster, "/")
	unit := "postgresql@" + major + "-" + name + ".service"
	h := nativeHostDataHost(m)
	fmt.Fprintf(out, "postgresql %s: restarting %s in the maintenance window to apply %s\n", obs.PG.Cluster, unit, strings.Join(view.PendingRestart, ", "))
	m.mu.Lock()
	m.tuningStateLocked().LastRestart = now
	_ = m.save()
	m.mu.Unlock()
	if text, err := h.run(ctx, "", "/usr/bin/systemctl", "restart", unit); err != nil {
		fmt.Fprintf(out, "FAILED restarting %s: %s\n", unit, nativeTrimOutput(text))
		return false, "the restart failed: " + nativeTrimOutput(text)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := h.psql(ctx, obs.PG.Port, "SELECT 1;\n"); err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			fmt.Fprintf(out, "FAILED: PostgreSQL did not accept connections within two minutes of the restart\n")
			return false, "PostgreSQL did not come back after the restart"
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Fprintf(out, "postgresql %s: restarted; %s now apply\n", obs.PG.Cluster, strings.Join(view.PendingRestart, ", "))
	return true, ""
}

// ---- panel API -------------------------------------------------------------------

// ResourceTuningApply is POST /v1/host/resources/apply.
type ResourceTuningApply struct {
	DryRun bool `json:"dry_run,omitempty"`
}

// RedisMigrationRequest is POST /v1/host/resources/redis/{app}/migrate; the
// panel confirms with the application id.
type RedisMigrationRequest struct {
	Confirm string `json:"confirm"`
}

func (m *Manager) resourceTuningView(ctx context.Context, refresh bool) ResourceTuningView {
	if err := m.hostOperationsAllowed(); err != nil {
		return ResourceTuningView{Reason: err.Error()}
	}
	var view ResourceTuningView
	if refresh {
		obs, _ := m.observeTuning(ctx)
		m.mu.Lock()
		state := m.tuningStateLocked()
		overrides, peaks, probes := state.Overrides, state.Peaks, maps.Clone(state.RedisUses)
		obs.CapsLearning = tuningCapsLearning(state.FirstSample, time.Now())
		m.mu.Unlock()
		view = planTuning(obs, overrides, peaks, probes)
		view.Computed = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.tuningStateLocked()
	if !refresh {
		if state.View != nil {
			view = *state.View
		} else {
			view = ResourceTuningView{Supported: true, Reason: "The first tuning run has not finished yet."}
		}
	}
	view.Overrides, view.Paused = state.Overrides, state.Overrides.Paused
	policy := m.hostPolicyLocked()
	view.NextWindow = policy.RebootWindow.next(time.Now())
	view.Migrations = nil
	for _, mg := range state.Migrations {
		view.Migrations = append(view.Migrations, mg)
	}
	slices.SortFunc(view.Migrations, func(a, b RedisMigration) int { return b.Started.Compare(a.Started) })
	view.Jobs = nil
	for _, job := range m.hostUpdatesStateLocked().Jobs {
		if job.Kind == tuningJobKind {
			view.Jobs = append(view.Jobs, job)
		}
	}
	tc := m.tuning()
	tc.mu.Lock()
	view.Running = tc.running || len(tc.migrating) > 0
	tc.mu.Unlock()
	return view
}

func (m *Manager) setTuningOverrides(o TuningOverrides) error {
	if err := o.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.tuningStateLocked()
	old := state.Overrides
	state.Overrides = o
	if err := m.save(); err != nil {
		state.Overrides = old
		return err
	}
	return nil
}

func (m *Manager) resourceTuningRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/host/resources", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		JSON(w, 200, m.resourceTuningView(ctx, r.URL.Query().Get("refresh") == "1"))
	})
	mux.HandleFunc("PUT /v1/host/resources/overrides", func(w http.ResponseWriter, r *http.Request) {
		var o TuningOverrides
		if err := Decode(w, r, &o); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setTuningOverrides(o); err != nil {
			Fail(w, 400, err)
			return
		}
		// The response is the dry run: what applying the overrides changes.
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		JSON(w, 200, m.resourceTuningView(ctx, true))
	})
	mux.HandleFunc("POST /v1/host/resources/apply", func(w http.ResponseWriter, r *http.Request) {
		var in ResourceTuningApply
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		if in.DryRun {
			JSON(w, 200, m.resourceTuningView(ctx, true))
			return
		}
		if err := m.startTuningCycle(); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, m.resourceTuningView(ctx, false))
	})
	mux.HandleFunc("POST /v1/host/resources/redis/{app}/migrate", func(w http.ResponseWriter, r *http.Request) {
		var in RedisMigrationRequest
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		app := r.PathValue("app")
		if in.Confirm != app {
			Fail(w, 400, errors.New("confirm with the application id"))
			return
		}
		if err := m.startRedisMigration(app); err != nil {
			Fail(w, 409, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		JSON(w, 202, m.resourceTuningView(ctx, false))
	})
}
