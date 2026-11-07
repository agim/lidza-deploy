package agent

// Resource tuning: the agent sizes the host's data services from the
// machine it runs on and the applications it hosts. It recomputes at agent
// start (so after every reboot or resize) and hourly, applies what can be
// applied live, and leaves settings that need a service restart for the
// maintenance window (resource_tuning_apply.go). Operators can override any
// computed value from the panel; overrides live in agent state.
//
// This file holds the pure part: inputs, formulas and renderers. Every
// formula is conservative (it leaves room for the applications'
// own processes), proportional to the machine and bounded at both ends.
//
// Memory budgets, with R the host's RAM in MiB and n the number of hosted
// applications:
//
//	redis       = 10% of R, shared by every Redis/Valkey instance on the host
//	clickhouse  = ratio × R when any ClickHouse user exists (see below)
//	app reserve = 384 MiB per application, at most 50% of R
//	postgres    = R − redis − clickhouse − app reserve (at least 10% of R),
//	              split between the host cluster and container databases
//	              in proportion to the applications they serve
//
// PostgreSQL (per cluster, with P its share of the postgres budget, s the
// cluster's share of applications and a its application count):
//
//	shared_buffers       = P/4, within [128 MiB, R/4, 32 GiB]          (restart)
//	effective_cache_size = max(2 × shared_buffers, 0.6 × R × s), ≤ 0.75 × R × s
//	max_connections      = min(1000, max(100, 25a + 50)), but at most
//	                       0.3 × R × s / 8 MiB per backend, never below 40 (restart)
//	superuser_reserved   = max_connections/100 within [3, 10]           (restart)
//	reserved_connections = platform headroom = min(50, max(10, max_connections/5)) (restart)
//	work_mem             = (R×s − shared_buffers)/(3 × max_connections), within [4, 64] MiB
//	maintenance_work_mem = R/16 within [64 MiB, 2 GiB]
//	autovacuum_work_mem  = R/64 within [64 MiB, 1 GiB]
//	max_wal_size         = disk/50 within [1, 8] GiB; min_wal_size a quarter of it (≥ 80 MiB)
//	checkpoint_timeout 15min, checkpoint_completion_target 0.9, wal_buffers 16MB (restart)
//	random_page_cost 1.1 and effective_io_concurrency 200 on SSD (4 and 2 on rotational disks)
//	max_worker_processes = max(8, CPUs) (restart); max_parallel_workers = CPUs;
//	max_parallel_workers_per_gather and max_parallel_maintenance_workers = CPUs/4 within [1, 4]
//	(per gather 0 on one CPU)
//
// Per-application connection caps (ALTER ROLE … CONNECTION LIMIT): the pool
// max_connections − superuser_reserved − reserved_connections, less two
// sessions per application for its own backup and restore jobs (they run as
// the application's role), is shared by water-filling: every application
// gets the floor (10, or pool/apps when smaller), the rest goes in
// proportion to its weight max(observed 7-day peak, configured pool), a
// share above the ceiling min(200, pool/2) is cut there and its excess is
// shared among the others. A cap is never below 1.2 × the observed peak
// (the plan warns when peaks no longer fit the pool). The two job
// sessions are then added back.
//
// Redis: the redis budget is split between instances by weight
// max(used memory, 64 MiB); each instance gets at least 64 MiB. A cache
// instance evicts (allkeys-lru); a queue or persistent-data instance never
// evicts (noeviction), never gets a limit below 1.5 × its used memory (the
// hourly run raises it as the data grows) and raises an alert above 80% of
// its limit. Limits and caps change only when the target moves by more than
// a tenth, so hourly runs do not churn.
//
// ClickHouse: with u users on the host (native databases and containers),
// the budget ratio is 0.20 + 0.025 × (u − 1), at most 0.40, raised so a
// server gets at least 1 GiB, at most 0.5. The native server takes the
// share of its users, each container one user's share; each native user's
// per-query max_memory_usage is server/max(2, users) within [256 MiB, server/2].

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// TuningHost is the machine the agent measured.
type TuningHost struct {
	CPUs       int  `json:"cpus"`
	MemoryMiB  int  `json:"memory_mib"`
	DiskGiB    int  `json:"disk_gib"`
	Rotational bool `json:"rotational,omitempty"`
}

// TuningOverrides are the operator's per-resource choices from the panel.
// Empty values mean "use the computed value".
type TuningOverrides struct {
	// Paused stops automatic application; targets are still computed.
	Paused bool `json:"paused,omitempty"`
	// Postgres overrides host cluster settings by name.
	Postgres map[string]string `json:"postgres,omitempty"`
	// ConnectionCaps overrides an application's cap; -1 removes the cap.
	ConnectionCaps map[string]int `json:"connection_caps,omitempty"`
	// RedisMaxMemoryMiB overrides an instance's limit by instance id.
	RedisMaxMemoryMiB map[string]int `json:"redis_max_memory_mib,omitempty"`
	// RedisUse classifies an application's Redis use: cache, queue or persistent.
	RedisUse map[string]string `json:"redis_use,omitempty"`
	// ClickHouseMemoryRatio overrides the native server's memory ratio.
	ClickHouseMemoryRatio float64 `json:"clickhouse_memory_ratio,omitempty"`
}

// tunablePostgres lists the host cluster settings the tuner manages, with
// whether a change needs a restart.
var tunablePostgres = map[string]bool{
	"shared_buffers": true, "max_connections": true, "superuser_reserved_connections": true, "reserved_connections": true,
	"max_worker_processes": true, "wal_buffers": true,
	"effective_cache_size": false, "work_mem": false, "maintenance_work_mem": false, "autovacuum_work_mem": false,
	"max_wal_size": false, "min_wal_size": false, "checkpoint_timeout": false, "checkpoint_completion_target": false,
	"random_page_cost": false, "effective_io_concurrency": false, "max_parallel_workers": false,
	"max_parallel_workers_per_gather": false, "max_parallel_maintenance_workers": false,
}

var tuningValuePattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(kB|MB|GB|TB|ms|s|min|h|d)?$`)

const (
	tuningRedisUseCache      = "cache"
	tuningRedisUseQueue      = "queue"
	tuningRedisUsePersistent = "persistent"
)

// Validate checks overrides before they are stored.
func (o TuningOverrides) Validate() error {
	for name, value := range o.Postgres {
		if _, ok := tunablePostgres[name]; !ok {
			return fmt.Errorf("%s is not a tuned PostgreSQL setting", name)
		}
		if !tuningValuePattern.MatchString(value) {
			return fmt.Errorf("%s: %q is not a plain number or size", name, value)
		}
		if name == "max_connections" {
			if n, _ := strconv.Atoi(value); n < 20 || n > 5000 {
				return errors.New("max_connections must be between 20 and 5000")
			}
		}
	}
	for app, limit := range o.ConnectionCaps {
		if !idPattern.MatchString(app) {
			return errors.New("connection caps name applications by id")
		}
		if limit != -1 && (limit < 1 || limit > 5000) {
			return fmt.Errorf("%s: a connection cap is 1–5000, or -1 for no cap", app)
		}
	}
	for id, mib := range o.RedisMaxMemoryMiB {
		if len(id) > 100 || mib < 16 || mib > 1<<20 {
			return fmt.Errorf("%s: Redis limits are 16 MiB or more", id)
		}
	}
	for app, use := range o.RedisUse {
		if !idPattern.MatchString(app) || (use != tuningRedisUseCache && use != tuningRedisUseQueue && use != tuningRedisUsePersistent) {
			return errors.New("Redis use is cache, queue or persistent for an application id")
		}
	}
	if o.ClickHouseMemoryRatio != 0 && (o.ClickHouseMemoryRatio < 0.05 || o.ClickHouseMemoryRatio > 0.9) {
		return errors.New("the ClickHouse memory ratio must be between 0.05 and 0.9")
	}
	return nil
}

// ---- budgets ---------------------------------------------------------------

type tuningBudget struct {
	RedisMiB      int
	ClickHouseMiB int
	ReserveMiB    int
	PostgresMiB   int
	CHRatio       float64
}

// clickhouseTuningRatio is the share of RAM all ClickHouse servers on the
// host may use together, for users ClickHouse users.
func clickhouseTuningRatio(memoryMiB, users int) float64 {
	if users <= 0 {
		return 0
	}
	ratio := min(0.40, 0.20+0.025*float64(users-1))
	if memoryMiB > 0 {
		ratio = max(ratio, min(0.5, 1024/float64(memoryMiB)))
	}
	return math.Round(ratio*1000) / 1000
}

func computeTuningBudget(host TuningHost, apps, clickhouseUsers int) tuningBudget {
	r := max(host.MemoryMiB, 512)
	b := tuningBudget{RedisMiB: r / 10, CHRatio: clickhouseTuningRatio(r, clickhouseUsers)}
	b.ClickHouseMiB = int(float64(r) * b.CHRatio)
	b.ReserveMiB = min(384*apps, r/2)
	b.PostgresMiB = max(r-b.RedisMiB-b.ClickHouseMiB-b.ReserveMiB, r/10)
	return b
}

// ---- PostgreSQL ------------------------------------------------------------

// pgTuningInput sizes one PostgreSQL cluster: the host cluster or one
// container database.
type pgTuningInput struct {
	Host     TuningHost
	SliceMiB int     // this cluster's part of the postgres budget
	Share    float64 // this cluster's share of the applications (0–1]
	Apps     int     // applications using this cluster
}

// PGTuning is the computed configuration of one cluster.
type PGTuning struct {
	Settings  map[string]string `json:"settings"`
	Headroom  int               `json:"headroom"` // reserved_connections for platform operations
	AppPool   int               `json:"app_pool"` // connections applications share
	Formulas  map[string]string `json:"formulas"`
	ConnBytes int               `json:"-"`
}

func clampInt(v, lo, hi int) int { return max(lo, min(hi, v)) }

func pgMB(mib int) string {
	if mib >= 1024 && mib%1024 == 0 {
		return strconv.Itoa(mib/1024) + "GB"
	}
	return strconv.Itoa(mib) + "MB"
}

func computePostgresTuning(in pgTuningInput) PGTuning {
	r := max(in.Host.MemoryMiB, 512)
	share := in.Share
	if share <= 0 || share > 1 {
		share = 1
	}
	cpus := max(in.Host.CPUs, 1)
	apps := max(in.Apps, 1)
	rs := int(float64(r) * share)

	sb := clampInt(in.SliceMiB/4, 128, min(r/4, 32768))
	ecs := min(max(2*sb, int(0.6*float64(rs))), int(0.75*float64(rs)))
	ecs = max(ecs, sb)
	ramCap := int(0.3 * float64(rs) / 8)
	mc := max(40, min(1000, max(100, 25*apps+50), ramCap))
	sr := clampInt(mc/100, 3, 10)
	headroom := min(50, max(10, mc/5))
	work := clampInt((rs-sb)/(3*mc), 4, 64)
	mwm := clampInt(r/16, 64, 2048)
	avwm := clampInt(r/64, 64, 1024)
	disk := in.Host.DiskGiB
	if disk <= 0 {
		disk = 50
	}
	maxWal := clampInt(disk/50, 1, 8) * 1024
	minWal := max(80, maxWal/4)
	perGather := clampInt(cpus/4, 1, 4)
	if cpus == 1 {
		perGather = 0
	}
	rpc, eio := "1.1", "200"
	if in.Host.Rotational {
		rpc, eio = "4", "2"
	}
	settings := map[string]string{
		"shared_buffers":                   pgMB(sb),
		"effective_cache_size":             pgMB(ecs),
		"max_connections":                  strconv.Itoa(mc),
		"superuser_reserved_connections":   strconv.Itoa(sr),
		"reserved_connections":             strconv.Itoa(headroom),
		"work_mem":                         pgMB(work),
		"maintenance_work_mem":             pgMB(mwm),
		"autovacuum_work_mem":              pgMB(avwm),
		"max_wal_size":                     pgMB(maxWal),
		"min_wal_size":                     pgMB(minWal),
		"checkpoint_timeout":               "15min",
		"checkpoint_completion_target":     "0.9",
		"wal_buffers":                      "16MB",
		"random_page_cost":                 rpc,
		"effective_io_concurrency":         eio,
		"max_worker_processes":             strconv.Itoa(max(8, cpus)),
		"max_parallel_workers":             strconv.Itoa(cpus),
		"max_parallel_workers_per_gather":  strconv.Itoa(perGather),
		"max_parallel_maintenance_workers": strconv.Itoa(clampInt(cpus/4, 1, 4)),
	}
	formulas := map[string]string{
		"shared_buffers":                 "postgres budget/4, within 128 MiB … RAM/4",
		"effective_cache_size":           "max(2 × shared_buffers, 60% of RAM share), ≤ 75%",
		"max_connections":                "min(1000, max(100, 25 × apps + 50)), ≤ 30% of RAM share / 8 MiB",
		"superuser_reserved_connections": "max_connections/100 within 3–10",
		"reserved_connections":           "platform headroom: max_connections/5 within 10–50",
		"work_mem":                       "(RAM share − shared_buffers) / (3 × max_connections), 4–64 MiB",
		"maintenance_work_mem":           "RAM/16 within 64 MiB … 2 GiB",
		"autovacuum_work_mem":            "RAM/64 within 64 MiB … 1 GiB",
		"max_wal_size":                   "disk/50 within 1–8 GiB",
		"min_wal_size":                   "max_wal_size/4, at least 80 MiB",
		"max_worker_processes":           "max(8, CPUs)",
		"max_parallel_workers":           "CPUs",
	}
	return PGTuning{Settings: settings, Headroom: headroom, AppPool: mc - sr - headroom, Formulas: formulas}
}

// pgRestartSetting reports whether a change of name needs a restart.
func pgRestartSetting(name string) bool { return tunablePostgres[name] }

// renderPostgresTuningConf renders the managed conf.d include. Settings are
// sorted so the file is stable; the agent compares it byte for byte.
func renderPostgresTuningConf(settings map[string]string, host TuningHost, apps int) string {
	names := make([]string, 0, len(settings))
	for name := range settings {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# Managed by Līdza Deploy resource tuning. Changes are overwritten;\n")
	b.WriteString("# set overrides from the server's Resources section in the panel.\n")
	fmt.Fprintf(&b, "# Sized for %d CPUs, %d MiB RAM and %d applications.\n", host.CPUs, host.MemoryMiB, apps)
	for _, name := range names {
		value := settings[name]
		if !tuningValuePattern.MatchString(value) {
			continue // validated earlier; never write anything else
		}
		if strings.ContainsAny(value, "kMGTmshd") {
			value = "'" + value + "'"
		}
		fmt.Fprintf(&b, "%s = %s\n", name, value)
	}
	return b.String()
}

// pgNormalize turns a setting value into a comparable form: memory in kB,
// durations in milliseconds, everything else as written.
func pgNormalize(value string) string {
	value = strings.Trim(strings.TrimSpace(value), "'")
	match := regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*(kB|MB|GB|TB|ms|s|min|h|d)$`).FindStringSubmatch(value)
	if match == nil {
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return value
	}
	number, _ := strconv.ParseFloat(match[1], 64)
	factor := map[string]float64{"kB": 1, "MB": 1024, "GB": 1 << 20, "TB": 1 << 30, "ms": 1, "s": 1000, "min": 60000, "h": 3600000, "d": 86400000}[match[2]]
	kind := "ms"
	if strings.HasSuffix(match[2], "B") {
		kind = "kB"
	}
	return strconv.FormatFloat(number*factor, 'f', -1, 64) + kind
}

// ---- connection caps -------------------------------------------------------

// capRequest is one application's claim on the cluster's connections.
type capRequest struct {
	Key      string
	Weight   int
	Peak     int
	Override int // > 0 fixed cap, -1 uncapped, 0 computed
}

const (
	tuningCapFloor     = 10
	tuningCapCeiling   = 200
	tuningJobsSessions = 2
)

// allocateConnectionCaps shares pool connections between requests (see the
// file comment). Overridden caps are taken first; an uncapped (-1) request
// gets -1.
func allocateConnectionCaps(pool int, requests []capRequest) map[string]int {
	out := map[string]int{}
	active := []capRequest{}
	for _, r := range requests {
		switch {
		case r.Override == -1:
			out[r.Key] = -1
		case r.Override > 0:
			out[r.Key] = r.Override
			pool -= max(r.Override-tuningJobsSessions, 0)
		default:
			active = append(active, r)
		}
	}
	if len(active) == 0 {
		return out
	}
	pool -= tuningJobsSessions * len(active)
	pool = max(pool, len(active))
	floor := clampInt(pool/len(active), 1, tuningCapFloor)
	ceiling := max(floor, min(tuningCapCeiling, pool/2))
	if len(active) == 1 {
		ceiling = max(floor, min(tuningCapCeiling*2, pool))
	}
	for i := range active {
		active[i].Weight = max(active[i].Weight, 1)
	}
	// Every application gets the floor; the rest is shared by weight, and
	// a share above the ceiling is cut there and its excess shared again.
	extra := pool - floor*len(active)
	free := slices.Clone(active)
	for len(free) > 0 {
		total := 0
		for _, r := range free {
			total += r.Weight
		}
		next := free[:0:0]
		for _, r := range free {
			if floor+int(float64(max(extra, 0))*float64(r.Weight)/float64(total)) >= ceiling {
				out[r.Key] = ceiling
				continue
			}
			next = append(next, r)
		}
		if len(next) == len(free) {
			for _, r := range next {
				out[r.Key] = floor + int(float64(max(extra, 0))*float64(r.Weight)/float64(total))
			}
			break
		}
		for _, r := range free {
			if out[r.Key] == ceiling {
				extra -= ceiling - floor
			}
		}
		free = next
	}
	for _, r := range active {
		// Never below the observed peak with a fifth to spare: a cap that
		// refuses the application's normal load is worse than none.
		out[r.Key] = max(out[r.Key], (r.Peak*6+4)/5) + tuningJobsSessions
	}
	return out
}

// appConnectionPool estimates how many connections an application opens
// at most from its environment: Rails pool × web processes plus the job
// worker's concurrency, Go's DB_MAX_CONNS (pgx's default otherwise), Node's
// pool maximum (node-postgres defaults to 10).
func appConnectionPool(runtime string, env map[string]string, units []string, cpus int) (int, string) {
	number := func(keys ...string) (int, string) {
		for _, key := range keys {
			if n, err := strconv.Atoi(strings.TrimSpace(env[key])); err == nil && n > 0 {
				return n, key
			}
		}
		return 0, ""
	}
	if raw := env["DATABASE_URL"]; raw != "" {
		if match := regexp.MustCompile(`[?&](?:pool|pool_max_conns|connection_limit)=([0-9]+)`).FindStringSubmatch(raw); match != nil {
			if n, _ := strconv.Atoi(match[1]); n > 0 && runtime != "rails" {
				return n, "DATABASE_URL pool " + match[1]
			}
		}
	}
	switch runtime {
	case "rails", "":
		per, key := number("DB_POOL", "RAILS_MAX_THREADS", "MAX_THREADS")
		if per == 0 {
			per, key = 5, "Rails default pool"
		}
		if raw := env["DATABASE_URL"]; raw != "" {
			if match := regexp.MustCompile(`[?&]pool=([0-9]+)`).FindStringSubmatch(raw); match != nil {
				if n, _ := strconv.Atoi(match[1]); n > 0 {
					per, key = n, "DATABASE_URL pool"
				}
			}
		}
		web, webKey := number("WEB_CONCURRENCY", "PUMA_WORKERS")
		if web == 0 {
			web = 1
		}
		total := per * web
		source := fmt.Sprintf("%s %d", key, per)
		if webKey != "" {
			source += fmt.Sprintf(" × %s %d", webKey, web)
		}
		worker := slices.ContainsFunc(units, func(u string) bool {
			u = strings.ToLower(u)
			return strings.Contains(u, "sidekiq") || strings.Contains(u, "worker") || strings.Contains(u, "good_job") || strings.Contains(u, "solid_queue") || strings.Contains(u, "jobs")
		})
		if worker {
			jobs, jobsKey := number("SIDEKIQ_CONCURRENCY", "SIDEKIQ_THREADS", "JOB_CONCURRENCY")
			if jobs == 0 {
				jobs, jobsKey = per, "worker"
			}
			total += jobs
			source += fmt.Sprintf(" + %s %d", jobsKey, jobs)
		}
		return total, source
	case "go":
		if n, key := number("DB_MAX_CONNS", "DATABASE_MAX_CONNS", "PGX_MAX_CONNS", "DB_POOL"); n > 0 {
			return n, key
		}
		return max(4, cpus), "pgx default max(4, CPUs)"
	case "node", "astro":
		if n, key := number("DB_POOL_MAX", "PG_POOL_MAX", "DATABASE_POOL_MAX", "DB_POOL"); n > 0 {
			return n, key
		}
		return 10, "node-postgres default 10"
	}
	return 0, ""
}

// ---- Redis -----------------------------------------------------------------

// redisUseRank orders uses by how much losing keys hurts.
func redisUseRank(use string) int {
	switch use {
	case tuningRedisUseQueue:
		return 3
	case tuningRedisUsePersistent:
		return 2
	case tuningRedisUseCache:
		return 1
	}
	return 0
}

// redisPolicyFor is the eviction policy for an instance holding uses.
func redisPolicyFor(uses []string) string {
	strongest := ""
	for _, use := range uses {
		if redisUseRank(use) > redisUseRank(strongest) {
			strongest = use
		}
	}
	if strongest == tuningRedisUseCache {
		return "allkeys-lru"
	}
	return "noeviction"
}

// classifyRedisKeys decides an application's use from a sample of its key
// names and how many of them expire. Sidekiq and job-queue keys win (a
// queue must never be evicted); cache namespaces or mostly expiring keys
// mean cache; everything else is persistent data.
func classifyRedisKeys(keys []string, expiring int) string {
	if len(keys) == 0 {
		return tuningRedisUseCache
	}
	queue, cache := 0, 0
	for _, key := range keys {
		k := strings.ToLower(key)
		switch {
		case strings.HasPrefix(k, "queue:") || strings.HasPrefix(k, "stat:") || k == "queues" || k == "schedule" || k == "retry" || k == "dead" || k == "processes" ||
			strings.HasPrefix(k, "sidekiq") || strings.HasPrefix(k, "bull:") || strings.HasPrefix(k, "resque:") || strings.Contains(k, ":queue:"):
			queue++
		case strings.HasPrefix(k, "rack::attack") || strings.HasPrefix(k, "cache:") || strings.HasPrefix(k, "views/") || strings.Contains(k, ":cache:") ||
			strings.HasPrefix(k, "rails_cache") || strings.HasPrefix(k, "cache/") || strings.HasPrefix(k, "_cache"):
			cache++
		}
	}
	if queue > 0 {
		return tuningRedisUseQueue
	}
	if cache*2 >= len(keys) || expiring*5 >= len(keys)*4 {
		return tuningRedisUseCache
	}
	return tuningRedisUsePersistent
}

// redisTuningRequest is one instance's claim on the Redis budget.
type redisTuningRequest struct {
	ID        string
	UsedBytes int64
	Uses      []string
	Override  int // MiB
}

// RedisTarget is one instance's computed limit and policy.
type RedisTarget struct {
	MaxMemoryMiB int    `json:"max_memory_mib"`
	Policy       string `json:"policy"`
	Note         string `json:"note,omitempty"`
}

// allocateRedisMemory splits budgetMiB between instances (see file comment).
func allocateRedisMemory(budgetMiB int, requests []redisTuningRequest) map[string]RedisTarget {
	out := map[string]RedisTarget{}
	total := 0.0
	weights := map[string]float64{}
	for _, r := range requests {
		w := max(float64(r.UsedBytes)/(1<<20), 64)
		weights[r.ID] = w
		total += w
	}
	for _, r := range requests {
		policy := redisPolicyFor(r.Uses)
		limit := max(64, int(float64(budgetMiB)*weights[r.ID]/max(total, 1)))
		note := ""
		if r.Override > 0 {
			limit, note = r.Override, "operator override"
		}
		if policy == "noeviction" {
			floor := int(math.Ceil(float64(r.UsedBytes) * 1.5 / (1 << 20)))
			if limit < floor {
				limit = floor
				note = "raised to 1.5 × used memory: a queue or persistent instance must never refuse writes"
			}
		}
		out[r.ID] = RedisTarget{MaxMemoryMiB: limit, Policy: policy, Note: note}
	}
	return out
}

// tuningSignificant reports whether target differs from current enough to
// change it: always from unlimited or unset, otherwise by more than a tenth
// (and at least two units).
func tuningSignificant(current, target int64) bool {
	if current <= 0 || target <= 0 {
		return current != target
	}
	diff := current - target
	if diff < 0 {
		diff = -diff
	}
	return diff >= 2 && diff*10 > current
}

// redisAlert reports a noeviction instance above 80% of its limit.
func redisAlert(policy string, used, limit int64) bool {
	return policy == "noeviction" && limit > 0 && used*5 > limit*4
}

// ---- ClickHouse ------------------------------------------------------------

// ClickHouseTarget is one ClickHouse server's computed limits.
type ClickHouseTarget struct {
	Ratio          float64 `json:"ratio"`
	ServerMiB      int     `json:"server_mib"`
	QueryMemoryMiB int     `json:"query_memory_mib"`
}

// computeClickHouseTuning sizes the native server (nativeUsers databases)
// and each container (one user each) from the host budget.
func computeClickHouseTuning(host TuningHost, nativeUsers, containers int, override float64) (native ClickHouseTarget, container ClickHouseTarget) {
	users := nativeUsers + containers
	r := max(host.MemoryMiB, 512)
	total := clickhouseTuningRatio(r, users)
	if users == 0 {
		return
	}
	per := total / float64(users)
	floor := min(0.5, 1024/float64(r))
	if nativeUsers > 0 {
		ratio := math.Round(max(per*float64(nativeUsers), floor)*1000) / 1000
		if override > 0 {
			ratio = override
		}
		server := int(float64(r) * ratio)
		native = ClickHouseTarget{Ratio: ratio, ServerMiB: server, QueryMemoryMiB: clampInt(server/max(2, nativeUsers), 256, max(256, server/2))}
	}
	if containers > 0 {
		ratio := math.Round(max(per, floor)*1000) / 1000
		server := int(float64(r) * ratio)
		container = ClickHouseTarget{Ratio: ratio, ServerMiB: server, QueryMemoryMiB: max(256, server/2)}
	}
	return
}

// renderClickHouseMemoryConfig is the container config.d file that caps a
// container ClickHouse server's memory (containers run without cgroup caps,
// so ClickHouse would otherwise size itself to 90% of the host's RAM).
func renderClickHouseMemoryConfig(ratio float64) string {
	return fmt.Sprintf(`<?xml version="1.0"?>
<!-- Managed by Līdza Deploy resource tuning. Changes are overwritten. -->
<clickhouse>
    <max_server_memory_usage_to_ram_ratio>%s</max_server_memory_usage_to_ram_ratio>
</clickhouse>
`, strconv.FormatFloat(ratio, 'f', 3, 64))
}

// ---- restart scheduling ------------------------------------------------------

// restartDecision says whether the cluster restarts now to apply pending
// restart-only settings. It restarts only inside the maintenance window,
// never while other work runs (deployments, backups, restores, moves), not
// when the scheduled reboot will apply the settings anyway, at most once
// every six hours, and never when the new max_connections would not fit
// the sessions open now.
type restartInput struct {
	Pending       bool
	InWindow      bool
	NextWindow    string
	Busy          string
	RebootPlanned bool
	SinceLast     float64 // hours since the last tuning restart; < 0 never
	InUse         int
	NewMax        int
}

func restartDecision(in restartInput) (bool, string) {
	switch {
	case !in.Pending:
		return false, ""
	case !in.InWindow:
		return false, "waits for the maintenance window" + prefixed(" starting ", in.NextWindow)
	case in.Busy != "":
		return false, "waits: " + in.Busy
	case in.RebootPlanned:
		return false, "the scheduled reboot in this window applies it"
	case in.SinceLast >= 0 && in.SinceLast < 6:
		return false, "PostgreSQL was restarted for tuning less than six hours ago"
	case in.NewMax > 0 && in.InUse >= in.NewMax:
		return false, fmt.Sprintf("refused: %d sessions are open and the new max_connections is %d", in.InUse, in.NewMax)
	}
	return true, "restarting in the maintenance window"
}

func prefixed(prefix, value string) string {
	if value == "" {
		return ""
	}
	return prefix + value
}
