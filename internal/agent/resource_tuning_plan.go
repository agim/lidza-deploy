package agent

// The tuning plan: what the agent observed on the host, turned into the
// view the panel shows and the list of changes to apply. planTuning is pure
// so every rule is tested without a host.

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ResourceTuningState is the persisted part of resource tuning.
type ResourceTuningState struct {
	Overrides TuningOverrides `json:"overrides"`
	// Peaks are the daily maxima of each role's sessions ("port/role"),
	// the newest seven days.
	Peaks      map[string][]TuningPeak `json:"peaks,omitempty"`
	LastSample time.Time               `json:"last_sample,omitzero"`
	// FirstSample is when session sampling began; computed caps are
	// enforced only after a day of samples (tuningCapsLearning).
	FirstSample time.Time `json:"first_sample,omitzero"`
	LastRun     time.Time `json:"last_run,omitzero"`
	// LastRestart is when tuning last restarted the host cluster.
	LastRestart time.Time `json:"last_restart,omitzero"`
	// RedisUses caches the probed use of each application's Redis.
	RedisUses map[string]TuningProbe `json:"redis_uses,omitempty"`
	// ClickHouseRatio is the applied native server ratio.
	ClickHouseRatio float64                   `json:"clickhouse_ratio,omitempty"`
	View            *ResourceTuningView       `json:"view,omitempty"`
	Migrations      map[string]RedisMigration `json:"migrations,omitempty"`
}

// TuningPeak is one day's peak of a role's sessions.
type TuningPeak struct {
	Day string `json:"day"`
	Max int    `json:"max"`
}

// TuningProbe is a probed Redis use.
type TuningProbe struct {
	Use     string    `json:"use"`
	Sampled int       `json:"sampled"`
	At      time.Time `json:"at"`
}

// ResourceTuningView is GET /v1/host/resources.
type ResourceTuningView struct {
	Supported      bool                   `json:"supported"`
	Reason         string                 `json:"reason,omitempty"`
	Host           TuningHost             `json:"host"`
	Apps           int                    `json:"apps"`
	Computed       time.Time              `json:"computed,omitzero"`
	Paused         bool                   `json:"paused,omitempty"`
	Overrides      TuningOverrides        `json:"overrides"`
	Postgres       *TuningPostgresView    `json:"postgres,omitempty"`
	Caps           []TuningCapView        `json:"caps,omitempty"`
	Redis          []TuningRedisView      `json:"redis,omitempty"`
	ClickHouse     []TuningClickHouseView `json:"clickhouse,omitempty"`
	Containers     []TuningContainerView  `json:"containers,omitempty"`
	Changes        []TuningChange         `json:"changes,omitempty"`
	PendingRestart []string               `json:"pending_restart,omitempty"`
	RestartNote    string                 `json:"restart_note,omitempty"`
	NextWindow     time.Time              `json:"next_window,omitzero"`
	Warnings       []string               `json:"warnings,omitempty"`
	Running        bool                   `json:"running,omitempty"`
	Migrations     []RedisMigration       `json:"migrations,omitempty"`
	Jobs           []HostJob              `json:"jobs,omitempty"`
}

// TuningSettingView is one setting: what runs now, what the formula gives,
// the operator's override and the resulting target.
type TuningSettingView struct {
	Name     string `json:"name"`
	Current  string `json:"current,omitempty"`
	Computed string `json:"computed"`
	Override string `json:"override,omitempty"`
	Target   string `json:"target"`
	Restart  bool   `json:"restart,omitempty"`
	Pending  bool   `json:"pending,omitempty"` // configured, waits for a restart
	Formula  string `json:"formula,omitempty"`
}

type TuningPostgresView struct {
	Cluster  string              `json:"cluster"`
	Port     int                 `json:"port"`
	Settings []TuningSettingView `json:"settings"`
	AppPool  int                 `json:"app_pool"`
	Headroom int                 `json:"headroom"`
	InUse    int                 `json:"in_use"`
	ConfFile string              `json:"conf_file,omitempty"`
	Note     string              `json:"note,omitempty"`
}

type TuningCapView struct {
	AppID      string `json:"app_id"`
	Role       string `json:"role"`
	Current    int    `json:"current"` // -1 unlimited
	Computed   int    `json:"computed"`
	Override   int    `json:"override,omitempty"`
	Target     int    `json:"target"`
	InUse      int    `json:"in_use"`
	Peak       int    `json:"peak"`
	Pool       int    `json:"pool,omitempty"`
	PoolSource string `json:"pool_source,omitempty"`
	Superuser  bool   `json:"superuser,omitempty"`
	Deferred   bool   `json:"deferred,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

type TuningRedisApp struct {
	AppID       string `json:"app_id"`
	DBs         []int  `json:"dbs,omitempty"`
	Use         string `json:"use"`
	UseSource   string `json:"use_source"` // override, probe or default
	CanMove     bool   `json:"can_move,omitempty"`
	MoveBlocked string `json:"move_blocked,omitempty"`
}

type TuningRedisView struct {
	ID           string           `json:"id"`
	Kind         string           `json:"kind"` // shared, own or container
	Apps         []TuningRedisApp `json:"apps"`
	UsedBytes    int64            `json:"used_bytes"`
	MaxBytes     int64            `json:"max_bytes"`
	Policy       string           `json:"policy,omitempty"`
	TargetMiB    int              `json:"target_mib"`
	TargetPolicy string           `json:"target_policy"`
	Override     int              `json:"override,omitempty"`
	Alert        bool             `json:"alert,omitempty"`
	Note         string           `json:"note,omitempty"`
	Error        string           `json:"error,omitempty"`
}

type TuningClickHouseView struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"` // native or container
	Users          int     `json:"users"`
	CurrentRatio   float64 `json:"current_ratio,omitempty"`
	TargetRatio    float64 `json:"target_ratio"`
	ServerMiB      int     `json:"server_mib"`
	QueryMemoryMiB int     `json:"query_memory_mib"`
	Override       float64 `json:"override,omitempty"`
}

type TuningContainerView struct {
	AppID     string              `json:"app_id"`
	Kind      string              `json:"kind"` // postgres
	Container string              `json:"container"`
	Settings  []TuningSettingView `json:"settings"`
	Note      string              `json:"note,omitempty"`
}

// TuningChange is one difference between what runs and the target.
type TuningChange struct {
	Resource string `json:"resource"`
	Setting  string `json:"setting"`
	Old      string `json:"old"`
	New      string `json:"new"`
	// Apply is reload, restart (maintenance window), live, next-restart
	// (container databases) or deferred (waits for a safe moment).
	Apply string `json:"apply"`
	Note  string `json:"note,omitempty"`
}

func (c TuningChange) String() string {
	line := fmt.Sprintf("%s %s: %s → %s (%s)", c.Resource, c.Setting, c.Old, c.New, c.Apply)
	if c.Note != "" {
		line += ": " + c.Note
	}
	return line
}

// ---- observation -------------------------------------------------------------

// tuningObservation is everything planTuning needs, gathered from the host.
type tuningObservation struct {
	// CapsLearning holds computed connection caps back until a day of
	// session samples shows each application's real peak.
	CapsLearning    bool
	Host            TuningHost
	Apps            []tuningAppObs
	PG              *tuningPGObs
	Redis           []tuningRedisObs
	NativeCH        *tuningCHObs
	ContainerCH     []tuningCHObs
	ContainerDBs    []tuningContainerDBObs
	Warnings        []string
	ClickHouseUsers []string
}

type tuningAppObs struct {
	ID             string
	Runtime        string
	Role           string // host cluster role, empty when the app uses none
	RoleKnown      bool
	RoleSuper      bool
	RoleLimit      int
	InUse          int
	Pool           int
	PoolSource     string
	SharedRedis    []redisEnvUse
	ReleaseManaged bool
	MoveBlocked    string
}

type tuningPGObs struct {
	Cluster   string
	Major     int
	Port      int
	Current   map[string]string
	Pending   map[string]bool
	InUse     int
	ConfFile  string
	IncludeOK bool
}

type tuningRedisObs struct {
	ID        string // "shared", unit name or container name
	Kind      string
	Apps      []string
	AppDBs    map[string][]int
	Uses      map[string]TuningProbe // probed per application
	UsedBytes int64
	MaxBytes  int64
	Policy    string
	Error     string
}

type tuningCHObs struct {
	ID    string
	Ratio float64
	Users []string
	Query map[string]int // per user, MiB
}

type tuningContainerDBObs struct {
	AppID     string
	Container string
	Current   map[string]string
	Pending   map[string]bool
	Error     string
}

// containerPGSettings are the settings tuning sets on container databases
// (each serves one application).
var containerPGSettings = []string{"shared_buffers", "effective_cache_size", "work_mem", "maintenance_work_mem", "max_wal_size", "random_page_cost", "effective_io_concurrency", "max_parallel_workers_per_gather"}

// tuningPeak is the highest daily peak of key.
func tuningPeak(peaks map[string][]TuningPeak, key string) int {
	best := 0
	for _, p := range peaks[key] {
		best = max(best, p.Max)
	}
	return best
}

// recordTuningPeaks folds one sample (sessions per key) into the daily peaks.
func recordTuningPeaks(peaks map[string][]TuningPeak, sample map[string]int, now time.Time) map[string][]TuningPeak {
	if peaks == nil {
		peaks = map[string][]TuningPeak{}
	}
	day := now.UTC().Format("2006-01-02")
	for key, n := range sample {
		days := peaks[key]
		if len(days) > 0 && days[len(days)-1].Day == day {
			days[len(days)-1].Max = max(days[len(days)-1].Max, n)
		} else {
			days = append(days, TuningPeak{Day: day, Max: n})
		}
		if len(days) > 7 {
			days = days[len(days)-7:]
		}
		peaks[key] = days
	}
	// Forget roles not seen for a week.
	cutoff := now.UTC().AddDate(0, 0, -7).Format("2006-01-02")
	for key, days := range peaks {
		if len(days) == 0 || days[len(days)-1].Day < cutoff {
			delete(peaks, key)
		}
	}
	return peaks
}

func tuningPeakKey(port int, role string) string { return strconv.Itoa(port) + "/" + role }

// ---- plan --------------------------------------------------------------------

// planTuning computes targets and changes from an observation.
func planTuning(obs tuningObservation, o TuningOverrides, peaks map[string][]TuningPeak, probes map[string]TuningProbe) ResourceTuningView {
	view := ResourceTuningView{Supported: true, Host: obs.Host, Apps: len(obs.Apps), Overrides: o, Paused: o.Paused, Warnings: slices.Clone(obs.Warnings)}
	hostApps := 0
	for _, a := range obs.Apps {
		if a.Role != "" {
			hostApps++
		}
	}
	nativeCHUsers := 0
	if obs.NativeCH != nil {
		nativeCHUsers = len(obs.NativeCH.Users)
	}
	budget := computeTuningBudget(obs.Host, len(obs.Apps), nativeCHUsers+len(obs.ContainerCH))
	weights := hostApps + len(obs.ContainerDBs)
	if obs.PG == nil {
		weights = len(obs.ContainerDBs)
	}

	// Host PostgreSQL and per-application caps.
	if pg := obs.PG; pg != nil {
		share := 1.0
		if weights > 0 && hostApps > 0 {
			share = float64(hostApps) / float64(weights)
		}
		t := computePostgresTuning(pgTuningInput{Host: obs.Host, SliceMiB: int(float64(budget.PostgresMiB) * share), Share: share, Apps: hostApps})
		if pg.Major > 0 && pg.Major < 16 {
			delete(t.Settings, "reserved_connections")
		}
		pv := &TuningPostgresView{Cluster: pg.Cluster, Port: pg.Port, Headroom: t.Headroom, InUse: pg.InUse, ConfFile: pg.ConfFile}
		names := make([]string, 0, len(t.Settings))
		for name := range t.Settings {
			names = append(names, name)
		}
		sort.Strings(names)
		targets := map[string]string{}
		for _, name := range names {
			s := TuningSettingView{Name: name, Current: pg.Current[name], Computed: t.Settings[name], Override: o.Postgres[name], Restart: pgRestartSetting(name), Formula: t.Formulas[name]}
			s.Target = s.Computed
			if s.Override != "" {
				s.Target = s.Override
			}
			targets[name] = s.Target
			s.Pending = pg.Pending[name]
			if s.Current != "" && pgNormalize(s.Current) != pgNormalize(s.Target) {
				apply := "reload"
				if s.Restart {
					apply = "restart"
				}
				if !pg.IncludeOK {
					apply = "deferred"
				}
				view.Changes = append(view.Changes, TuningChange{Resource: "postgresql " + pg.Cluster, Setting: name, Old: s.Current, New: s.Target, Apply: apply})
			}
			if s.Restart && (s.Pending || s.Current != "" && pgNormalize(s.Current) != pgNormalize(s.Target)) {
				view.PendingRestart = append(view.PendingRestart, name)
			}
			pv.Settings = append(pv.Settings, s)
		}
		if !pg.IncludeOK {
			pv.Note = "postgresql.conf has no include_dir for conf.d; tuning is computed but not written. Restore the distribution's include_dir = 'conf.d' line."
			view.Warnings = append(view.Warnings, pv.Note)
		}
		// The pool applications share follows the target configuration.
		mc, _ := strconv.Atoi(targets["max_connections"])
		sr, _ := strconv.Atoi(targets["superuser_reserved_connections"])
		rc, _ := strconv.Atoi(targets["reserved_connections"])
		pv.AppPool = max(mc-sr-rc, 0)
		pv.Headroom = rc
		// Until a restart applies a lower max_connections, caps must also
		// fit what runs now.
		if cur, err := strconv.Atoi(pg.Current["max_connections"]); err == nil && cur > 0 {
			curSR, _ := strconv.Atoi(pg.Current["superuser_reserved_connections"])
			curRC, _ := strconv.Atoi(pg.Current["reserved_connections"])
			pv.AppPool = min(pv.AppPool, max(cur-curSR-curRC, 0))
		}
		view.Postgres = pv
		var capWarnings []string
		view.Caps, view.Changes, capWarnings = planCaps(obs, o, peaks, pv.AppPool, view.Changes)
		view.Warnings = append(view.Warnings, capWarnings...)
	}

	// Redis instances.
	requests := []redisTuningRequest{}
	uses := map[string][]string{}
	for i, r := range obs.Redis {
		rv := TuningRedisView{ID: r.ID, Kind: r.Kind, UsedBytes: r.UsedBytes, MaxBytes: r.MaxBytes, Policy: r.Policy, Override: o.RedisMaxMemoryMiB[r.ID], Error: r.Error}
		for _, app := range r.Apps {
			use, source := tuningRedisUse(app, r, o, probes)
			ra := TuningRedisApp{AppID: app, DBs: r.AppDBs[app], Use: use, UseSource: source}
			if r.Kind == "shared" {
				ra.MoveBlocked = tuningMoveBlocked(obs, app)
				ra.CanMove = ra.MoveBlocked == ""
			}
			rv.Apps = append(rv.Apps, ra)
			uses[r.ID] = append(uses[r.ID], use)
		}
		view.Redis = append(view.Redis, rv)
		requests = append(requests, redisTuningRequest{ID: r.ID, UsedBytes: r.UsedBytes, Uses: uses[r.ID], Override: o.RedisMaxMemoryMiB[r.ID]})
		_ = i
	}
	targets := allocateRedisMemory(budget.RedisMiB, requests)
	for i := range view.Redis {
		rv := &view.Redis[i]
		t := targets[rv.ID]
		rv.TargetMiB, rv.TargetPolicy, rv.Note = t.MaxMemoryMiB, t.Policy, t.Note
		limit := rv.MaxBytes
		if limit == 0 {
			limit = int64(rv.TargetMiB) << 20
		}
		rv.Alert = redisAlert(rv.TargetPolicy, rv.UsedBytes, limit)
		if rv.Alert {
			view.Warnings = append(view.Warnings, fmt.Sprintf("Redis %s uses more than 80%% of its limit and never evicts; its applications' writes fail when it is full.", rv.ID))
		}
		if rv.Kind == "shared" && rv.TargetPolicy == "noeviction" && slices.Contains(uses[rv.ID], tuningRedisUseCache) {
			rv.Note = strings.TrimSpace(rv.Note + " Caches share this instance with queues or persistent data, so nothing is evicted; move them to their own Redis.")
		}
		if rv.Error != "" {
			continue
		}
		if rv.MaxBytes != int64(rv.TargetMiB)<<20 && (rv.Override > 0 || tuningSignificant(rv.MaxBytes>>20, int64(rv.TargetMiB))) {
			view.Changes = append(view.Changes, TuningChange{Resource: "redis " + rv.ID, Setting: "maxmemory", Old: redisMiBText(rv.MaxBytes), New: strconv.Itoa(rv.TargetMiB) + "mb", Apply: "live"})
		}
		if rv.Policy != rv.TargetPolicy {
			view.Changes = append(view.Changes, TuningChange{Resource: "redis " + rv.ID, Setting: "maxmemory-policy", Old: rv.Policy, New: rv.TargetPolicy, Apply: "live"})
		}
	}

	// ClickHouse servers.
	native, container := computeClickHouseTuning(obs.Host, nativeCHUsers, len(obs.ContainerCH), o.ClickHouseMemoryRatio)
	if ch := obs.NativeCH; ch != nil && nativeCHUsers > 0 {
		view.ClickHouse = append(view.ClickHouse, TuningClickHouseView{ID: "host", Kind: "native", Users: nativeCHUsers, CurrentRatio: ch.Ratio, TargetRatio: native.Ratio, ServerMiB: native.ServerMiB, QueryMemoryMiB: native.QueryMemoryMiB, Override: o.ClickHouseMemoryRatio})
		if ch.Ratio != native.Ratio {
			view.Changes = append(view.Changes, TuningChange{Resource: "clickhouse host", Setting: "max_server_memory_usage_to_ram_ratio", Old: ratioText(ch.Ratio), New: ratioText(native.Ratio), Apply: "live"})
		}
		for _, user := range ch.Users {
			if ch.Query[user] != native.QueryMemoryMiB {
				view.Changes = append(view.Changes, TuningChange{Resource: "clickhouse " + user, Setting: "max_memory_usage", Old: strconv.Itoa(ch.Query[user]) + " MiB", New: strconv.Itoa(native.QueryMemoryMiB) + " MiB", Apply: "live"})
			}
		}
	}
	for _, ch := range obs.ContainerCH {
		view.ClickHouse = append(view.ClickHouse, TuningClickHouseView{ID: ch.ID, Kind: "container", Users: 1, CurrentRatio: ch.Ratio, TargetRatio: container.Ratio, ServerMiB: container.ServerMiB, QueryMemoryMiB: container.QueryMemoryMiB})
		if ch.Ratio != container.Ratio {
			view.Changes = append(view.Changes, TuningChange{Resource: "clickhouse " + ch.ID, Setting: "max_server_memory_usage_to_ram_ratio", Old: ratioText(ch.Ratio), New: ratioText(container.Ratio), Apply: "live"})
		}
	}

	// Container databases: one application each.
	if n := len(obs.ContainerDBs); n > 0 && weights > 0 {
		share := 1 / float64(weights)
		t := computePostgresTuning(pgTuningInput{Host: obs.Host, SliceMiB: int(float64(budget.PostgresMiB) * share), Share: share, Apps: 1})
		for _, db := range obs.ContainerDBs {
			cv := TuningContainerView{AppID: db.AppID, Kind: "postgres", Container: db.Container, Note: db.Error}
			for _, name := range containerPGSettings {
				s := TuningSettingView{Name: name, Current: db.Current[name], Computed: t.Settings[name], Target: t.Settings[name], Restart: pgRestartSetting(name), Pending: db.Pending[name], Formula: t.Formulas[name]}
				if db.Error == "" && s.Current != "" && pgNormalize(s.Current) != pgNormalize(s.Target) {
					apply := "reload"
					if s.Restart {
						apply = "next-restart"
					}
					view.Changes = append(view.Changes, TuningChange{Resource: "container database " + db.AppID, Setting: name, Old: s.Current, New: s.Target, Apply: apply})
				}
				cv.Settings = append(cv.Settings, s)
			}
			view.Containers = append(view.Containers, cv)
		}
	}
	return view
}

// planCaps allocates the per-application caps on the host cluster.
func planCaps(obs tuningObservation, o TuningOverrides, peaks map[string][]TuningPeak, pool int, changes []TuningChange) ([]TuningCapView, []TuningChange, []string) {
	warnings := []string{}
	// Several applications can share one role; they share its cap.
	byRole := map[string][]tuningAppObs{}
	roles := []string{}
	for _, a := range obs.Apps {
		if a.Role == "" || !a.RoleKnown {
			continue
		}
		if _, seen := byRole[a.Role]; !seen {
			roles = append(roles, a.Role)
		}
		byRole[a.Role] = append(byRole[a.Role], a)
	}
	sort.Strings(roles)
	requests := []capRequest{}
	for _, role := range roles {
		apps := byRole[role]
		weight, override := tuningPeak(peaks, tuningPeakKey(obs.PG.Port, role)), 0
		for _, a := range apps {
			weight = max(weight, a.Pool)
			if v, ok := o.ConnectionCaps[a.ID]; ok {
				override = v
			}
		}
		if apps[0].RoleSuper {
			continue // superusers ignore CONNECTION LIMIT
		}
		requests = append(requests, capRequest{Key: role, Weight: weight, Peak: tuningPeak(peaks, tuningPeakKey(obs.PG.Port, role)), Override: override})
	}
	alloc := allocateConnectionCaps(pool, requests)
	computed := allocateConnectionCaps(pool, stripOverrides(requests))
	if total := capTotal(alloc); total > pool {
		warnings = append(warnings, fmt.Sprintf("Observed connection peaks need %d connections and applications share %d: raise max_connections or lower application pools.", total, pool))
	}
	out := []TuningCapView{}
	for _, role := range roles {
		apps := byRole[role]
		a := apps[0]
		ids := []string{}
		pool := 0
		sources := []string{}
		for _, app := range apps {
			ids = append(ids, app.ID)
			pool += app.Pool
			if app.PoolSource != "" {
				sources = append(sources, app.PoolSource)
			}
		}
		cv := TuningCapView{AppID: strings.Join(ids, ", "), Role: role, Current: a.RoleLimit, InUse: a.InUse, Peak: tuningPeak(peaks, tuningPeakKey(obs.PG.Port, role)), Pool: pool, PoolSource: strings.Join(sources, "; "), Superuser: a.RoleSuper}
		if a.RoleSuper {
			cv.Target, cv.Computed = -1, -1
			cv.Warning = "This role is a superuser: PostgreSQL ignores connection limits for it. Give the application a role without SUPERUSER to cap it."
			out = append(out, cv)
			continue
		}
		cv.Computed, cv.Target = computed[role], alloc[role]
		if v, ok := o.ConnectionCaps[a.ID]; ok {
			cv.Override = v
		}
		if cv.Target > 0 && cv.Pool > cv.Target {
			cv.Warning = fmt.Sprintf("The application's configured pool (%d) exceeds its cap (%d); lower its pool or raise the cap.", cv.Pool, cv.Target)
		}
		_, overridden := o.ConnectionCaps[a.ID]
		if cv.Target != cv.Current && (overridden || tuningSignificant(int64(cv.Current), int64(cv.Target))) {
			change := TuningChange{Resource: "role " + role, Setting: "CONNECTION LIMIT", Old: capText(cv.Current), New: capText(cv.Target), Apply: "live"}
			if !overridden && obs.CapsLearning {
				cv.Deferred = true
				change.Apply = "deferred"
				change.Note = "learning the application's peak for a day before capping it"
			} else if cv.Target > 0 && (cv.Current < 0 || cv.Target < cv.Current) && cv.InUse > cv.Target {
				cv.Deferred = true
				change.Apply = "deferred"
				change.Note = fmt.Sprintf("%d sessions are open; the cap is lowered once fewer are", cv.InUse)
			}
			changes = append(changes, change)
		}
		out = append(out, cv)
	}
	return out, changes, warnings
}

func capTotal(caps map[string]int) int {
	total := 0
	for _, n := range caps {
		total += max(n, 0)
	}
	return total
}

func stripOverrides(requests []capRequest) []capRequest {
	out := slices.Clone(requests)
	for i := range out {
		out[i].Override = 0
	}
	return out
}

// tuningRedisUse resolves an application's Redis use: the panel's choice,
// then the probe, then cache for containers and queue otherwise (the safe
// default: nothing is evicted).
func tuningRedisUse(app string, r tuningRedisObs, o TuningOverrides, probes map[string]TuningProbe) (string, string) {
	if use := o.RedisUse[app]; use != "" {
		return use, "override"
	}
	if p, ok := r.Uses[app]; ok && p.Use != "" {
		return p.Use, "probe"
	}
	if p, ok := probes[app]; ok && p.Use != "" {
		return p.Use, "probe"
	}
	if r.Kind == "container" {
		return tuningRedisUseCache, "default"
	}
	return tuningRedisUseQueue, "default"
}

// tuningMoveBlocked explains why an application cannot move off the shared
// Redis, or returns "".
func tuningMoveBlocked(obs tuningObservation, app string) string {
	var self *tuningAppObs
	for i := range obs.Apps {
		if obs.Apps[i].ID == app {
			self = &obs.Apps[i]
		}
	}
	if self == nil {
		return "the application is not registered on this agent"
	}
	if self.MoveBlocked != "" {
		return self.MoveBlocked
	}
	if !self.ReleaseManaged {
		return "only applications whose releases this agent manages can move; their environment files are the agent's to change"
	}
	for _, other := range obs.Apps {
		if other.ID == app {
			continue
		}
		for _, mine := range self.SharedRedis {
			for _, theirs := range other.SharedRedis {
				if mine.DB == theirs.DB {
					return fmt.Sprintf("database %d is also used by %s", mine.DB, other.ID)
				}
			}
		}
	}
	return ""
}

func capText(n int) string {
	if n < 0 {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

func redisMiBText(bytes int64) string {
	if bytes <= 0 {
		return "unlimited"
	}
	return strconv.FormatInt(bytes>>20, 10) + "mb"
}

func ratioText(r float64) string {
	if r <= 0 {
		return "default"
	}
	return strconv.FormatFloat(r, 'f', 3, 64)
}
