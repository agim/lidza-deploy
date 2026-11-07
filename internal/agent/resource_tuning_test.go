package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func mib(value string) int {
	n := pgNormalize(value)
	kb, _ := strconv.ParseFloat(strings.TrimSuffix(n, "kB"), 64)
	return int(kb / 1024)
}

func TestTuningPostgresFormulasAtMachineSizes(t *testing.T) {
	cases := []struct {
		name            string
		host            TuningHost
		apps, chUsers   int
		maxConn         int
		sbLo, sbHi      int
		workLo, workHi  int
		perGather       string
		maxWorkers      string
		parallelWorkers string
	}{
		{"2GB/1vCPU", TuningHost{CPUs: 1, MemoryMiB: 2048, DiskGiB: 50}, 3, 0, 76, 128, 512, 4, 8, "0", "8", "1"},
		{"8GB/4vCPU", TuningHost{CPUs: 4, MemoryMiB: 8192, DiskGiB: 160}, 8, 0, 250, 512, 2048, 4, 16, "1", "8", "4"},
		{"32GB/8vCPU", TuningHost{CPUs: 8, MemoryMiB: 32768, DiskGiB: 320}, 20, 2, 550, 2048, 8192, 8, 32, "2", "8", "8"},
		{"128GB/32vCPU", TuningHost{CPUs: 32, MemoryMiB: 131072, DiskGiB: 2000}, 40, 4, 1000, 8192, 32768, 16, 64, "4", "32", "32"},
	}
	for _, c := range cases {
		b := computeTuningBudget(c.host, c.apps, c.chUsers)
		got := computePostgresTuning(pgTuningInput{Host: c.host, SliceMiB: b.PostgresMiB, Share: 1, Apps: c.apps})
		s := got.Settings
		t.Logf("%s, %d apps: budget redis=%d MiB clickhouse=%d MiB (ratio %.3f) reserve=%d MiB postgres=%d MiB; %v; app pool %d",
			c.name, c.apps, b.RedisMiB, b.ClickHouseMiB, b.CHRatio, b.ReserveMiB, b.PostgresMiB, s, got.AppPool)
		mc, _ := strconv.Atoi(s["max_connections"])
		if mc != c.maxConn {
			t.Errorf("%s: max_connections = %d, want %d", c.name, mc, c.maxConn)
		}
		sb := mib(s["shared_buffers"])
		if sb < c.sbLo || sb > c.sbHi || sb > c.host.MemoryMiB/4 {
			t.Errorf("%s: shared_buffers = %s outside %d–%d MiB", c.name, s["shared_buffers"], c.sbLo, c.sbHi)
		}
		if w := mib(s["work_mem"]); w < c.workLo || w > c.workHi {
			t.Errorf("%s: work_mem = %s", c.name, s["work_mem"])
		}
		ecs := mib(s["effective_cache_size"])
		if ecs < sb || ecs > c.host.MemoryMiB*3/4 {
			t.Errorf("%s: effective_cache_size = %s", c.name, s["effective_cache_size"])
		}
		if s["max_parallel_workers_per_gather"] != c.perGather || s["max_worker_processes"] != c.maxWorkers || s["max_parallel_workers"] != c.parallelWorkers {
			t.Errorf("%s: parallel settings %v", c.name, s)
		}
		sr, _ := strconv.Atoi(s["superuser_reserved_connections"])
		rc, _ := strconv.Atoi(s["reserved_connections"])
		if sr < 3 || sr > 10 || rc < 10 || rc > 50 || got.AppPool != mc-sr-rc {
			t.Errorf("%s: reserved %d/%d pool %d", c.name, sr, rc, got.AppPool)
		}
		if mwm := mib(s["maintenance_work_mem"]); mwm < 64 || mwm > 2048 {
			t.Errorf("%s: maintenance_work_mem = %s", c.name, s["maintenance_work_mem"])
		}
		for name, value := range s {
			if !tuningValuePattern.MatchString(value) || !tunablePostgresKnown(name) {
				t.Errorf("%s: %s = %q is not renderable", c.name, name, value)
			}
		}
	}
	// More applications on the same machine: more connections, bounded.
	host := TuningHost{CPUs: 8, MemoryMiB: 32768, DiskGiB: 320}
	last := 0
	for _, apps := range []int{1, 5, 20, 60} {
		s := computePostgresTuning(pgTuningInput{Host: host, SliceMiB: computeTuningBudget(host, apps, 0).PostgresMiB, Share: 1, Apps: apps}).Settings
		mc, _ := strconv.Atoi(s["max_connections"])
		if mc < last || mc > 1000 || mc < 100 {
			t.Fatalf("%d apps: max_connections %d after %d", apps, mc, last)
		}
		last = mc
	}
	// Rotational disks get the classic planner costs.
	if s := computePostgresTuning(pgTuningInput{Host: TuningHost{CPUs: 2, MemoryMiB: 4096, Rotational: true}, SliceMiB: 1024, Share: 1, Apps: 1}).Settings; s["random_page_cost"] != "4" || s["effective_io_concurrency"] != "2" {
		t.Fatalf("rotational: %v", s)
	}
}

func tunablePostgresKnown(name string) bool { _, ok := tunablePostgres[name]; return ok }

func TestTuningPostgresConfRendering(t *testing.T) {
	conf := renderPostgresTuningConf(map[string]string{"shared_buffers": "3610MB", "max_connections": "550", "checkpoint_timeout": "15min", "checkpoint_completion_target": "0.9", "bogus": "1; DROP"}, TuningHost{CPUs: 8, MemoryMiB: 32768}, 20)
	want := []string{"checkpoint_completion_target = 0.9\n", "checkpoint_timeout = '15min'\n", "max_connections = 550\n", "shared_buffers = '3610MB'\n"}
	last := -1
	for _, line := range want {
		i := strings.Index(conf, line)
		if i < 0 || i < last {
			t.Fatalf("missing or unsorted %q in\n%s", line, conf)
		}
		last = i
	}
	if strings.Contains(conf, "bogus") || !strings.HasPrefix(conf, "# Managed by Līdza Deploy") || !strings.Contains(conf, "8 CPUs, 32768 MiB RAM and 20 applications") {
		t.Fatalf("conf:\n%s", conf)
	}
	if conf != renderPostgresTuningConf(map[string]string{"shared_buffers": "3610MB", "max_connections": "550", "checkpoint_timeout": "15min", "checkpoint_completion_target": "0.9"}, TuningHost{CPUs: 8, MemoryMiB: 32768}, 20) {
		t.Fatal("rendering is not stable")
	}
	for in, want := range map[string]string{"2GB": "2097152kB", "2048MB": "2097152kB", "'128MB'": "131072kB", "15min": "900000ms", "900s": "900000ms", "0.90": "0.9", "200": "200"} {
		if got := pgNormalize(in); got != want {
			t.Errorf("pgNormalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTuningOverridesValidate(t *testing.T) {
	good := TuningOverrides{Postgres: map[string]string{"shared_buffers": "4GB", "max_connections": "300"}, ConnectionCaps: map[string]int{"shop": 40, "blog": -1}, RedisUse: map[string]string{"shop": "cache"}, RedisMaxMemoryMiB: map[string]int{"shared": 512}, ClickHouseMemoryRatio: 0.3}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []TuningOverrides{
		{Postgres: map[string]string{"wal_level": "logical"}},
		{Postgres: map[string]string{"shared_buffers": "4GB'; DROP"}},
		{Postgres: map[string]string{"max_connections": "5"}},
		{ConnectionCaps: map[string]int{"shop": 0}},
		{ConnectionCaps: map[string]int{"Bad ID": 5}},
		{RedisUse: map[string]string{"shop": "sessions"}},
		{RedisMaxMemoryMiB: map[string]int{"shared": 4}},
		{ClickHouseMemoryRatio: 0.95},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestTuningConnectionCapAllocation(t *testing.T) {
	sum := func(caps map[string]int) int {
		total := 0
		for _, n := range caps {
			if n > 0 {
				total += n
			}
		}
		return total
	}
	// Weighted shares, floors and the two job sessions.
	caps := allocateConnectionCaps(160, []capRequest{{Key: "big", Weight: 60, Peak: 40}, {Key: "mid", Weight: 20, Peak: 15}, {Key: "small", Weight: 2}, {Key: "idle", Weight: 0}})
	if caps["big"] <= caps["mid"] || caps["mid"] <= caps["small"] || caps["small"] < tuningCapFloor+tuningJobsSessions || caps["idle"] > caps["small"] {
		t.Fatalf("weights not respected: %v", caps)
	}
	if caps["big"] > tuningCapCeiling+tuningJobsSessions {
		t.Fatalf("ceiling exceeded: %v", caps)
	}
	if total := sum(caps); total > 160 {
		t.Fatalf("caps %v sum to %d over the pool", caps, total)
	}
	// Twenty applications sharing 495 connections never overcommit.
	requests := []capRequest{}
	for i := 0; i < 20; i++ {
		requests = append(requests, capRequest{Key: fmt.Sprintf("app%02d", i), Weight: 3 + i*2, Peak: 3 + i})
	}
	caps = allocateConnectionCaps(495, requests)
	if total := sum(caps); total > 495 {
		t.Fatalf("20 apps sum to %d", total)
	}
	for i := 1; i < 20; i++ {
		if caps[fmt.Sprintf("app%02d", i)] < caps[fmt.Sprintf("app%02d", i-1)] {
			t.Fatalf("not monotonic in weight: %v", caps)
		}
	}
	// One dominant application is cut at the ceiling and the others share
	// what it leaves.
	caps = allocateConnectionCaps(1000, []capRequest{{Key: "huge", Weight: 10000}, {Key: "a", Weight: 10}, {Key: "b", Weight: 10}})
	if caps["huge"] != tuningCapCeiling+tuningJobsSessions || caps["a"] <= tuningCapFloor+tuningJobsSessions || caps["a"] != caps["b"] {
		t.Fatalf("ceiling redistribution: %v", caps)
	}
	// A cap never falls below 1.2 × the observed peak.
	caps = allocateConnectionCaps(60, []capRequest{{Key: "busy", Weight: 50, Peak: 50}, {Key: "quiet", Weight: 1}})
	if caps["busy"] < 60+tuningJobsSessions {
		t.Fatalf("peak protection: %v", caps)
	}
	// Small pools lower the floor instead of overcommitting.
	small := []capRequest{}
	for i := 0; i < 10; i++ {
		small = append(small, capRequest{Key: strconv.Itoa(i), Weight: 1})
	}
	caps = allocateConnectionCaps(50, small)
	if total := sum(caps); total > 50 || caps["0"] < 1+tuningJobsSessions {
		t.Fatalf("small pool: %v (%d)", caps, total)
	}
	// Overrides are fixed, -1 stays uncapped, the rest shares the remainder.
	caps = allocateConnectionCaps(100, []capRequest{{Key: "fixed", Override: 40}, {Key: "free", Override: -1}, {Key: "auto", Weight: 5}})
	if caps["fixed"] != 40 || caps["free"] != -1 || caps["auto"] < tuningCapFloor || caps["auto"] > 100-38 {
		t.Fatalf("overrides: %v", caps)
	}
}

func TestTuningAppConnectionPool(t *testing.T) {
	cases := []struct {
		runtime string
		env     map[string]string
		units   []string
		want    int
	}{
		{"rails", map[string]string{"RAILS_MAX_THREADS": "5", "WEB_CONCURRENCY": "2"}, []string{"shop-web.service", "shop-sidekiq.service"}, 15},
		{"rails", map[string]string{"DB_POOL": "10", "SIDEKIQ_CONCURRENCY": "20"}, []string{"puma.service", "sidekiq.service"}, 30},
		{"rails", map[string]string{}, []string{"web.service"}, 5},
		{"rails", map[string]string{"DATABASE_URL": "postgres://u@127.0.0.1/db?pool=12"}, nil, 12},
		{"go", map[string]string{"DB_MAX_CONNS": "25"}, nil, 25},
		{"go", map[string]string{}, nil, 8},
		{"node", map[string]string{}, nil, 10},
	}
	for _, c := range cases {
		if got, source := appConnectionPool(c.runtime, c.env, c.units, 8); got != c.want || source == "" {
			t.Errorf("%s %v: %d (%s), want %d", c.runtime, c.env, got, source, c.want)
		}
	}
	for raw, want := range map[string]string{
		"postgres://app_shop:pw@127.0.0.1:5432/app_shop": "app_shop",
		"postgresql://deploy@localhost/shop":             "deploy",
		"postgres:///shop?host=/var/run/postgresql":      "deploy",
		"postgres://u@db.example.com:5432/x":             "",
		"postgres://u@127.0.0.1:5433/x":                  "",
		"mysql://u@127.0.0.1/x":                          "",
	} {
		if got := pgRoleFromURL(raw, 5432, "deploy"); got != want {
			t.Errorf("pgRoleFromURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestTuningRedisClassificationAndBudget(t *testing.T) {
	for _, c := range []struct {
		keys     []string
		expiring int
		want     string
	}{
		{[]string{"queue:default", "stat:processed", "user:1"}, 0, "queue"},
		{[]string{"rack::attack:123:ip", "cache:views/a", "cache:b"}, 0, "cache"},
		{[]string{"views/home", "views/about", "session:1"}, 0, "cache"},
		{[]string{"a", "b", "c", "d", "e"}, 5, "cache"},
		{[]string{"coverband_hash:report", "view_tracking:1", "x"}, 0, "persistent"},
		{nil, 0, "cache"},
	} {
		if got := classifyRedisKeys(c.keys, c.expiring); got != c.want {
			t.Errorf("classify %v = %s, want %s", c.keys, got, c.want)
		}
	}
	if redisPolicyFor([]string{"cache"}) != "allkeys-lru" || redisPolicyFor([]string{"cache", "queue"}) != "noeviction" || redisPolicyFor([]string{"persistent"}) != "noeviction" {
		t.Fatal("policy by use")
	}
	targets := allocateRedisMemory(3276, []redisTuningRequest{
		{ID: "shared", UsedBytes: 1100 << 20, Uses: []string{"cache", "queue"}},
		{ID: "lidza-redis@a.service", UsedBytes: 10 << 20, Uses: []string{"cache"}},
		{ID: "lidza-redis@b.service", UsedBytes: 300 << 20, Uses: []string{"queue"}, Override: 200},
	})
	if targets["lidza-redis@a.service"].MaxMemoryMiB < 64 || targets["lidza-redis@a.service"].Policy != "allkeys-lru" {
		t.Fatalf("cache instance: %+v", targets)
	}
	// noeviction never below 1.5 × used, even against an override.
	if b := targets["lidza-redis@b.service"]; b.MaxMemoryMiB < 450 || b.Policy != "noeviction" || b.Note == "" {
		t.Fatalf("queue instance: %+v", b)
	}
	if s := targets["shared"]; s.MaxMemoryMiB < 1650 || s.Policy != "noeviction" {
		t.Fatalf("shared instance: %+v", s)
	}
	if !redisAlert("noeviction", 81, 100) || redisAlert("noeviction", 79, 100) || redisAlert("allkeys-lru", 99, 100) {
		t.Fatal("alert threshold")
	}
	server, ok := parseSharedRedisConfig("bind 127.0.0.1\nport 6379\n# requirepass old\nrequirepass \"s3cret\"\nunixsocket /var/run/redis/redis-server.sock\n")
	if !ok || server.Port != 6379 || server.Password != "s3cret" || server.Socket != "/var/run/redis/redis-server.sock" {
		t.Fatalf("shared config: %+v", server)
	}
	uses := sharedRedisUses(map[string]string{"REDIS_URL": "redis://localhost:6379/3", "REDIS_CACHE_URL": "redis://:pw@127.0.0.1:6379/4", "OTHER": "redis://cache.example.com:6379/1", "SOCK": "unix:///var/run/redis/redis-server.sock?db=5", "PLAIN": "hello"}, server)
	slices.SortFunc(uses, func(a, b redisEnvUse) int { return a.DB - b.DB })
	if len(uses) != 3 || uses[0] != (redisEnvUse{"REDIS_URL", 3}) || uses[1] != (redisEnvUse{"REDIS_CACHE_URL", 4}) || uses[2] != (redisEnvUse{"SOCK", 5}) {
		t.Fatalf("uses: %+v", uses)
	}
}

func TestTuningClickHouseLimits(t *testing.T) {
	native, container := computeClickHouseTuning(TuningHost{MemoryMiB: 32768}, 2, 1, 0)
	if native.Ratio <= container.Ratio || native.Ratio+container.Ratio > 0.41 || container.ServerMiB < 1024 {
		t.Fatalf("shares: %+v %+v", native, container)
	}
	if native.QueryMemoryMiB != native.ServerMiB/2 || native.QueryMemoryMiB < 256 {
		t.Fatalf("query limit: %+v", native)
	}
	// Small hosts still give a server 1 GiB; more users raise the total.
	small, _ := computeClickHouseTuning(TuningHost{MemoryMiB: 2048}, 1, 0, 0)
	if small.ServerMiB < 1024 {
		t.Fatalf("small host: %+v", small)
	}
	if clickhouseTuningRatio(65536, 1) >= clickhouseTuningRatio(65536, 5) || clickhouseTuningRatio(65536, 100) > 0.4 {
		t.Fatal("ratio by users")
	}
	many, _ := computeClickHouseTuning(TuningHost{MemoryMiB: 65536}, 8, 0, 0)
	if many.QueryMemoryMiB != many.ServerMiB/8 {
		t.Fatalf("per-user query limit: %+v", many)
	}
	if over, _ := computeClickHouseTuning(TuningHost{MemoryMiB: 65536}, 2, 0, 0.3); over.Ratio != 0.3 {
		t.Fatalf("override: %+v", over)
	}
	if conf := renderClickHouseMemoryConfig(0.125); !strings.Contains(conf, "<max_server_memory_usage_to_ram_ratio>0.125</max_server_memory_usage_to_ram_ratio>") {
		t.Fatal(conf)
	}
}

func TestTuningRestartScheduling(t *testing.T) {
	base := restartInput{Pending: true, InWindow: true, SinceLast: -1, InUse: 100, NewMax: 550}
	if ok, _ := restartDecision(base); !ok {
		t.Fatal("a pending restart in a quiet window must run")
	}
	for name, in := range map[string]restartInput{
		"nothing pending": {InWindow: true, SinceLast: -1},
		"outside window":  {Pending: true, NextWindow: "2026-11-08T03:00:00Z", SinceLast: -1},
		"backup running":  {Pending: true, InWindow: true, Busy: "database backups are running", SinceLast: -1},
		"reboot planned":  {Pending: true, InWindow: true, RebootPlanned: true, SinceLast: -1},
		"restarted 2h":    {Pending: true, InWindow: true, SinceLast: 2},
		"too many open":   {Pending: true, InWindow: true, SinceLast: -1, InUse: 120, NewMax: 100},
	} {
		if ok, note := restartDecision(in); ok || (in.Pending && note == "") {
			t.Errorf("%s: restart %v (%q)", name, ok, note)
		}
	}
	if _, note := restartDecision(restartInput{Pending: true, NextWindow: "2026-11-08T03:00:00Z", SinceLast: -1}); !strings.Contains(note, "2026-11-08T03:00:00Z") {
		t.Fatalf("note names the window: %q", note)
	}
	if ok, _ := restartDecision(restartInput{Pending: true, InWindow: true, SinceLast: 7}); !ok {
		t.Fatal("six hours later the restart may run")
	}
}

func TestTuningPlanForProductionShapedHost(t *testing.T) {
	obs := tuningObservation{
		Host: TuningHost{CPUs: 8, MemoryMiB: 32768, DiskGiB: 320},
		PG: &tuningPGObs{Cluster: "17/main", Major: 17, Port: 5432, IncludeOK: true, InUse: 108, ConfFile: "/etc/postgresql/17/main/conf.d/90-lidza-tuning.conf",
			Current: map[string]string{"max_connections": "200", "shared_buffers": "128MB", "superuser_reserved_connections": "3", "reserved_connections": "0", "work_mem": "4MB", "checkpoint_timeout": "5min"},
			Pending: map[string]bool{}},
	}
	for i := 0; i < 20; i++ {
		obs.Apps = append(obs.Apps, tuningAppObs{ID: fmt.Sprintf("app%02d", i), Runtime: "rails", Role: fmt.Sprintf("role%02d", i), RoleKnown: true, RoleLimit: -1, InUse: 5, Pool: 10, ReleaseManaged: true})
	}
	obs.Apps[0].RoleSuper = true
	obs.Apps[1].InUse = 60
	obs.Apps[2].Pool = 300
	obs.Apps[6].InUse = 40 // a burst far above its share and its (unsampled) peak
	obs.Apps[3].SharedRedis = []redisEnvUse{{"REDIS_URL", 1}}
	obs.Apps[4].SharedRedis = []redisEnvUse{{"REDIS_URL", 1}}
	obs.Apps[5].SharedRedis = []redisEnvUse{{"REDIS_URL", 2}}
	obs.Redis = []tuningRedisObs{{ID: "shared", Kind: "shared", Apps: []string{"app03", "app04", "app05"}, AppDBs: map[string][]int{"app03": {1}, "app04": {1}, "app05": {2}},
		Uses: map[string]TuningProbe{"app05": {Use: "cache"}}, UsedBytes: 1100 << 20, Policy: "noeviction"}}
	view := planTuning(obs, TuningOverrides{}, map[string][]TuningPeak{"5432/role01": {{Day: "2026-11-06", Max: 70}}}, nil)
	byName := map[string]TuningSettingView{}
	for _, s := range view.Postgres.Settings {
		byName[s.Name] = s
	}
	if byName["max_connections"].Target != "550" || !byName["max_connections"].Restart || !slices.Contains(view.PendingRestart, "shared_buffers") {
		t.Fatalf("postgres: %+v pending %v", byName["max_connections"], view.PendingRestart)
	}
	// Until the restart, caps fit the 200 connections running now.
	if view.Postgres.AppPool != 197 {
		t.Fatalf("pool before restart: %d", view.Postgres.AppPool)
	}
	caps := map[string]TuningCapView{}
	total := 0
	for _, c := range view.Caps {
		caps[c.Role] = c
		if c.Target > 0 {
			total += c.Target
		}
	}
	if !caps["role00"].Superuser || caps["role00"].Warning == "" || caps["role00"].Target != -1 {
		t.Fatalf("superuser: %+v", caps["role00"])
	}
	if c := caps["role01"]; c.Target < 84 || c.Peak != 70 {
		t.Fatalf("peak protection: %+v", c)
	}
	if c := caps["role02"]; c.Warning == "" || c.Pool <= c.Target {
		t.Fatalf("pool warning: %+v", c)
	}
	var deferred bool
	for _, c := range view.Changes {
		if c.Resource == "role role06" && c.Apply == "deferred" {
			deferred = true
		}
		if c.Resource == "postgresql 17/main" && c.Setting == "checkpoint_timeout" && c.Apply != "reload" {
			t.Fatalf("reload change: %+v", c)
		}
	}
	if !deferred {
		t.Fatal("capping role06 below its 40 open sessions must wait")
	}
	// Only role01's protected peak may push the total over the pool, and
	// the plan says so.
	if total > 197+(84+2-tuningCapFloor) {
		t.Fatalf("caps sum %d", total)
	}
	// Shared Redis: app03 and app04 share database 1 and cannot move; app05
	// can. The cache next to queues forces noeviction and a note.
	shared := view.Redis[0]
	moves := map[string]TuningRedisApp{}
	for _, a := range shared.Apps {
		moves[a.AppID] = a
	}
	if moves["app03"].CanMove || !strings.Contains(moves["app03"].MoveBlocked, "app04") || !moves["app05"].CanMove || moves["app05"].Use != "cache" || moves["app03"].Use != "queue" || moves["app03"].UseSource != "default" {
		t.Fatalf("moves: %+v", moves)
	}
	if shared.TargetPolicy != "noeviction" || shared.TargetMiB < 1375 || !strings.Contains(shared.Note, "own Redis") {
		t.Fatalf("shared: %+v", shared)
	}
	// While peaks are being learned, computed caps wait; overrides do not.
	obs.CapsLearning = true
	learning := planTuning(obs, TuningOverrides{ConnectionCaps: map[string]int{"app07": 25}}, nil, nil)
	for _, c := range learning.Changes {
		if c.Resource == "role role07" && c.Apply != "live" || c.Resource == "role role08" && (c.Apply != "deferred" || !strings.Contains(c.Note, "learning")) {
			t.Fatalf("learning: %+v", c)
		}
	}
	obs.CapsLearning = false
	// Small moves of a target do not churn.
	if tuningSignificant(100, 105) || !tuningSignificant(100, 120) || !tuningSignificant(-1, 40) || tuningSignificant(10, 11) {
		t.Fatal("hysteresis")
	}
	// Overrides win and are reported.
	view = planTuning(obs, TuningOverrides{Postgres: map[string]string{"shared_buffers": "4GB"}, ConnectionCaps: map[string]int{"app05": 33}, RedisUse: map[string]string{"app03": "cache", "app04": "cache", "app05": "cache"}}, nil, nil)
	for _, s := range view.Postgres.Settings {
		if s.Name == "shared_buffers" && (s.Target != "4GB" || s.Override != "4GB" || s.Computed == "4GB") {
			t.Fatalf("override: %+v", s)
		}
	}
	for _, c := range view.Caps {
		if c.Role == "role05" && (c.Target != 33 || c.Override != 33) {
			t.Fatalf("cap override: %+v", c)
		}
	}
	if view.Redis[0].TargetPolicy != "allkeys-lru" {
		t.Fatalf("all-cache shared Redis evicts: %+v", view.Redis[0])
	}
	// Without the conf.d include nothing is written.
	obs.PG.IncludeOK = false
	view = planTuning(obs, TuningOverrides{}, nil, nil)
	if view.Postgres.Note == "" || len(view.Warnings) == 0 {
		t.Fatal("missing include warning")
	}
}

func TestTuningPeaksKeepSevenDays(t *testing.T) {
	var peaks map[string][]TuningPeak
	start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for day := 0; day < 10; day++ {
		peaks = recordTuningPeaks(peaks, map[string]int{"5432/a": day + 1}, start.AddDate(0, 0, day))
		peaks = recordTuningPeaks(peaks, map[string]int{"5432/a": 1}, start.AddDate(0, 0, day).Add(time.Hour))
	}
	if len(peaks["5432/a"]) != 7 || tuningPeak(peaks, "5432/a") != 10 || peaks["5432/a"][0].Max != 4 {
		t.Fatalf("peaks: %+v", peaks)
	}
	peaks = recordTuningPeaks(peaks, map[string]int{"5432/b": 3}, start.AddDate(0, 0, 30))
	if _, kept := peaks["5432/a"]; kept {
		t.Fatal("stale roles are forgotten")
	}
}

// ---- fake Redis ------------------------------------------------------------

type fakeRedisEntry struct {
	value string
	ttl   int64
}

type fakeRedis struct {
	mu      sync.Mutex
	dbs     map[int]map[string]fakeRedisEntry
	clients []string // CLIENT LIST lines
	log     []string
	failOn  string
}

func newFakeRedis() *fakeRedis { return &fakeRedis{dbs: map[int]map[string]fakeRedisEntry{}} }

type fakeRedisConn struct {
	r  *fakeRedis
	db int
}

func (f *fakeRedis) conn() redisConn { return &fakeRedisConn{r: f} }

func (c *fakeRedisConn) close() error { return nil }

func (c *fakeRedisConn) do(_ context.Context, args ...string) (any, error) {
	f := c.r
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := strings.ToUpper(args[0])
	f.log = append(f.log, strings.Join(args, " "))
	if f.failOn != "" && cmd == f.failOn {
		return nil, redisError("ERR injected")
	}
	db := f.dbs[c.db]
	if db == nil {
		db = map[string]fakeRedisEntry{}
		f.dbs[c.db] = db
	}
	switch cmd {
	case "SELECT":
		c.db, _ = strconv.Atoi(args[1])
		return "OK", nil
	case "SCAN":
		keys := []any{}
		for k := range db {
			keys = append(keys, k)
		}
		return []any{"0", keys}, nil
	case "PTTL":
		e, ok := db[args[1]]
		if !ok {
			return int64(-2), nil
		}
		if e.ttl == 0 {
			return int64(-1), nil
		}
		return e.ttl, nil
	case "DUMP":
		e, ok := db[args[1]]
		if !ok {
			return nil, nil
		}
		return "dump:" + e.value, nil
	case "RESTORE":
		ttl, _ := strconv.ParseInt(args[2], 10, 64)
		if _, exists := db[args[1]]; exists && (len(args) < 5 || args[4] != "REPLACE") {
			return nil, redisError("BUSYKEY")
		}
		db[args[1]] = fakeRedisEntry{value: strings.TrimPrefix(args[3], "dump:"), ttl: ttl}
		return "OK", nil
	case "FLUSHDB":
		f.dbs[c.db] = map[string]fakeRedisEntry{}
		return "OK", nil
	case "DBSIZE":
		return int64(len(db)), nil
	case "CLIENT":
		if strings.ToUpper(args[1]) == "ID" {
			return int64(1), nil
		}
		return strings.Join(f.clients, "\n"), nil
	}
	return nil, errors.New("fake redis: unsupported " + cmd)
}

func TestTuningRedisProbe(t *testing.T) {
	r := newFakeRedis()
	r.dbs[3] = map[string]fakeRedisEntry{"queue:default": {value: "x"}, "stat:processed": {value: "1"}}
	r.dbs[4] = map[string]fakeRedisEntry{"views/a": {value: "x", ttl: 1000}, "views/b": {value: "y", ttl: 2000}}
	if use, n, err := probeRedisUse(context.Background(), r.conn(), 3, 1000); err != nil || use != "queue" || n != 2 {
		t.Fatalf("db 3: %s %d %v", use, n, err)
	}
	if use, _, err := probeRedisUse(context.Background(), r.conn(), 4, 1000); err != nil || use != "cache" {
		t.Fatalf("db 4: %s %v", use, err)
	}
}

type migrationHarness struct {
	source, target *fakeRedis
	events         []string
	env            map[string]string
	healthy        error
	stopErr        error
}

func (h *migrationHarness) steps() redisMigrationSteps {
	return redisMigrationSteps{
		Source: func(context.Context) (redisConn, error) { return h.source.conn(), nil },
		Target: func(context.Context) (redisConn, error) { return h.target.conn(), nil },
		Provision: func(context.Context) (func(int) string, error) {
			h.events = append(h.events, "provision")
			return func(db int) string { return "unix://:pw@/sock?db=" + strconv.Itoa(db) }, nil
		},
		Stop:  func(context.Context) error { h.events = append(h.events, "stop"); return h.stopErr },
		Start: func(context.Context) error { h.events = append(h.events, "start"); return nil },
		SwitchEnv: func(_ context.Context, changes map[string]string) (func(context.Context) error, error) {
			old := map[string]string{}
			for k, v := range changes {
				old[k] = h.env[k]
				h.env[k] = v
			}
			h.events = append(h.events, "switch")
			return func(context.Context) error {
				for k, v := range old {
					h.env[k] = v
				}
				h.events = append(h.events, "restore")
				return nil
			}, nil
		},
		Healthy: func(context.Context) error { h.events = append(h.events, "verify"); return h.healthy },
	}
}

func TestTuningRedisMigrationCopiesQueuesAndClearsOnlyAfterVerification(t *testing.T) {
	redisClientPoll = time.Millisecond
	h := &migrationHarness{source: newFakeRedis(), target: newFakeRedis(), env: map[string]string{"REDIS_URL": "redis://127.0.0.1:6379/3"}}
	h.source.dbs[3] = map[string]fakeRedisEntry{"queue:default": {value: "jobs"}, "schedule": {value: "z", ttl: 5000}}
	h.source.dbs[5] = map[string]fakeRedisEntry{"other-app": {value: "keep"}}
	h.target.dbs[3] = map[string]fakeRedisEntry{"stale": {value: "from a failed attempt"}}
	record := &RedisMigration{}
	err := runRedisMigration(context.Background(), h.steps(), []redisEnvUse{{"REDIS_URL", 3}}, "queue", record)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"provision", "stop", "switch", "start", "verify"}; !slices.Equal(h.events, want) {
		t.Fatalf("events %v, want %v", h.events, want)
	}
	if h.env["REDIS_URL"] != "unix://:pw@/sock?db=3" || record.Copied != 2 {
		t.Fatalf("env %v copied %d", h.env, record.Copied)
	}
	if got := h.target.dbs[3]; len(got) != 2 || got["queue:default"].value != "jobs" || got["schedule"].ttl != 5000 {
		t.Fatalf("target: %+v", got)
	}
	if len(h.source.dbs[3]) != 0 || len(h.source.dbs[5]) != 1 {
		t.Fatalf("source after: %+v", h.source.dbs)
	}
}

func TestTuningRedisMigrationOfACacheStartsEmpty(t *testing.T) {
	redisClientPoll = time.Millisecond
	h := &migrationHarness{source: newFakeRedis(), target: newFakeRedis(), env: map[string]string{"REDIS_CACHE_URL": "redis://127.0.0.1:6379/1"}}
	h.source.dbs[1] = map[string]fakeRedisEntry{"views/a": {value: "x"}}
	record := &RedisMigration{}
	if err := runRedisMigration(context.Background(), h.steps(), []redisEnvUse{{"REDIS_CACHE_URL", 1}}, "cache", record); err != nil {
		t.Fatal(err)
	}
	if want := []string{"provision", "switch", "start", "verify"}; !slices.Equal(h.events, want) {
		t.Fatalf("a cache move never stops the application: %v", h.events)
	}
	if record.Copied != 0 || len(h.target.dbs[1]) != 0 || len(h.source.dbs[1]) != 0 {
		t.Fatalf("cache: copied %d target %v source %v", record.Copied, h.target.dbs[1], h.source.dbs[1])
	}
}

func TestTuningRedisMigrationRollsBackAndKeepsOldData(t *testing.T) {
	redisClientPoll = time.Millisecond
	h := &migrationHarness{source: newFakeRedis(), target: newFakeRedis(), env: map[string]string{"REDIS_URL": "redis://127.0.0.1:6379/2"}, healthy: errors.New("web failed")}
	h.source.dbs[2] = map[string]fakeRedisEntry{"coverband": {value: "data"}}
	err := runRedisMigration(context.Background(), h.steps(), []redisEnvUse{{"REDIS_URL", 2}}, "persistent", &RedisMigration{})
	if err == nil || !strings.Contains(err.Error(), "web failed") {
		t.Fatalf("err = %v", err)
	}
	if h.env["REDIS_URL"] != "redis://127.0.0.1:6379/2" || h.events[len(h.events)-2] != "restore" || h.events[len(h.events)-1] != "start" {
		t.Fatalf("rollback: env %v events %v", h.env, h.events)
	}
	if len(h.source.dbs[2]) != 1 {
		t.Fatal("the old database must be untouched after a failed verification")
	}
	// A client left on the old database (another process, another app)
	// blocks the flush, and the migration rolls back.
	h = &migrationHarness{source: newFakeRedis(), target: newFakeRedis(), env: map[string]string{"REDIS_URL": "redis://127.0.0.1:6379/2"}}
	h.source.dbs[2] = map[string]fakeRedisEntry{"k": {value: "v"}}
	h.source.clients = []string{"id=1 addr=127.0.0.1:1 db=2", "id=7 addr=127.0.0.1:2 db=2", "id=8 addr=127.0.0.1:3 db=0"}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := runRedisMigration(ctx, h.steps(), []redisEnvUse{{"REDIS_URL", 2}}, "queue", &RedisMigration{}); err == nil {
		t.Fatal("a remaining client must block the flush")
	}
	if len(h.source.dbs[2]) != 1 || h.env["REDIS_URL"] != "redis://127.0.0.1:6379/2" {
		t.Fatal("the old database is kept and the old URL restored")
	}
	// A failed copy restarts the stopped application on the old Redis.
	h = &migrationHarness{source: newFakeRedis(), target: newFakeRedis(), env: map[string]string{"REDIS_URL": "redis://127.0.0.1:6379/2"}}
	h.source.dbs[2] = map[string]fakeRedisEntry{"k": {value: "v"}}
	h.target.failOn = "RESTORE"
	if err := runRedisMigration(context.Background(), h.steps(), []redisEnvUse{{"REDIS_URL", 2}}, "queue", &RedisMigration{}); err == nil {
		t.Fatal("copy failure")
	}
	if !slices.Equal(h.events, []string{"provision", "stop", "start"}) || h.env["REDIS_URL"] != "redis://127.0.0.1:6379/2" {
		t.Fatalf("copy failure: %v %v", h.events, h.env)
	}
}

func TestTuningRESPParsing(t *testing.T) {
	if got := redisKeyspace("# Keyspace\r\ndb0:keys=10,expires=2,avg_ttl=0\r\ndb3:keys=690000,expires=12,avg_ttl=5\r\n"); got[3] != [2]int64{690000, 12} || got[0] != [2]int64{10, 2} {
		t.Fatalf("keyspace: %v", got)
	}
	if n := redisClientsOn("id=3 addr=a db=1 cmd=get\nid=4 addr=b db=1\nid=5 db=2", "4", []int{1}); n != 1 {
		t.Fatalf("clients: %d", n)
	}
}

// ---- the cycle on a fixture host --------------------------------------------

type tuningHostFixture struct {
	root    string
	mu      sync.Mutex
	limits  map[string]int
	scripts []string
	redis   *fakeRedis
	config  map[string]string // CONFIG SET values on the shared Redis
}

func (f *tuningHostFixture) run(_ context.Context, stdin, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(name, "pg_lsclusters"):
		return "17 main 5432 online postgres /var/lib/postgresql/17/main /var/log/postgresql/postgresql-17-main.log\n", nil
	case strings.HasSuffix(name, "runuser"):
		f.scripts = append(f.scripts, stdin)
		switch {
		case strings.Contains(stdin, "FROM pg_catalog.pg_settings"):
			// Reload settings follow the managed file once written;
			// restart-only settings keep their running value.
			written := map[string]string{}
			if raw, err := os.ReadFile(filepath.Join(f.root, "etc/postgresql/17/main/conf.d", tuningConfName)); err == nil {
				for _, line := range strings.Split(string(raw), "\n") {
					if k, v, ok := strings.Cut(line, " = "); ok {
						written[k] = strings.Trim(v, "'")
					}
				}
			}
			var b strings.Builder
			for name, restart := range tunablePostgres {
				value, pending := written[name], "f"
				running := map[string]string{"max_connections": "200", "shared_buffers": "128MB", "superuser_reserved_connections": "3", "reserved_connections": "0", "max_worker_processes": "8", "wal_buffers": "4MB"}[name]
				if restart {
					if value != "" && pgNormalize(value) != pgNormalize(running) {
						pending = "t"
					}
					value = running
				} else if value == "" {
					value = "1"
				}
				fmt.Fprintf(&b, "%s|%s|%s\n", name, value, pending)
			}
			return b.String(), nil
		case strings.Contains(stdin, "SHOW config_file"):
			return "/etc/postgresql/17/main/postgresql.conf", nil
		case strings.Contains(stdin, "r.rolcanlogin"):
			return fmt.Sprintf("app_shop|%d|0|4\npostgres|-1|1|1\n", f.limits["app_shop"]), nil
		case strings.Contains(stdin, "ALTER ROLE"):
			fields := strings.Fields(stdin)
			n, _ := strconv.Atoi(strings.TrimSuffix(fields[len(fields)-1], ";"))
			f.limits[strings.Trim(fields[2], `"`)] = n
			return "", nil
		case strings.Contains(stdin, "error IS NOT NULL"):
			return "0", nil
		}
		return "", nil
	}
	return "", nil
}

func TestTuningCycleAppliesOnceAndRecordsAHostJob(t *testing.T) {
	root := t.TempDir()
	f := &tuningHostFixture{root: root, limits: map[string]int{"app_shop": -1}, redis: newFakeRedis(), config: map[string]string{}}
	for path, body := range map[string]string{
		"usr/bin/pg_lsclusters":                  "",
		"etc/postgresql/17/main/postgresql.conf": "max_connections = 100\ninclude_dir = 'conf.d'\n",
		"etc/postgresql/17/main/conf.d/.keep":    "",
		"etc/redis/redis.conf":                   "port 6379\n",
	} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755)
		_ = os.WriteFile(filepath.Join(root, path), []byte(body), 0o644)
	}
	previousHost, previousPath, previousDial := nativeHostDataHost, hostPath, dialTuningRedis
	nativeHostDataHost = func(*Manager) nativeHostData { return nativeHostData{root: root, run: f.run} }
	hostPath = func(path string) string { return filepath.Join(root, path) }
	dialTuningRedis = func(context.Context, string, string, string) (redisConn, error) {
		return &configRedis{fakeRedisConn{r: f.redis}, f}, nil
	}
	t.Cleanup(func() { nativeHostDataHost, hostPath, dialTuningRedis = previousHost, previousPath, previousDial })
	m := &Manager{cfg: Config{DataDir: t.TempDir()}, key: make([]byte, 32), ctx: context.Background(), data: diskState{Apps: map[string]App{},
		NativeApps:     map[string]NativeApplication{"shop": {ID: "shop", Name: "shop", Runtime: "rails"}},
		NativeHostData: map[string]NativeHostDataRecord{"shop": {Postgres: &NativeHostPostgres{Name: "app_shop", Port: 5432, Ready: true}}}}}
	now := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	// A fresh agent learns peaks for a day before it caps anything.
	m.tuningCycle(context.Background(), "boot", now.Add(-48*time.Hour))
	if f.limits["app_shop"] != -1 {
		t.Fatalf("caps applied while learning: %v", f.limits)
	}
	m.mu.Lock()
	m.data.HostUpdates.Jobs = nil
	m.mu.Unlock()
	m.tuningCycle(context.Background(), "boot", now)

	conf, err := os.ReadFile(filepath.Join(root, "etc/postgresql/17/main/conf.d", tuningConfName))
	if err != nil || !strings.Contains(string(conf), "shared_buffers = '") || !strings.Contains(string(conf), "max_connections = ") {
		t.Fatalf("conf.d: %v\n%s", err, conf)
	}
	if !slices.ContainsFunc(f.scripts, func(s string) bool { return strings.Contains(s, "pg_reload_conf") }) {
		t.Fatal("the configuration was not reloaded")
	}
	if f.limits["app_shop"] <= 0 {
		t.Fatalf("connection limit not applied: %v", f.limits)
	}
	if f.config["maxmemory-policy"] == "" || f.config["maxmemory"] == "" {
		t.Fatalf("shared Redis not sized: %v", f.config)
	}
	m.mu.Lock()
	state := m.data.ResourceTuning
	jobs := slices.Clone(m.data.HostUpdates.Jobs)
	m.mu.Unlock()
	if state == nil || state.View == nil || len(state.View.PendingRestart) == 0 || !strings.Contains(state.View.RestartNote, "maintenance window") {
		t.Fatalf("view: %+v", state.View)
	}
	if len(jobs) != 1 || jobs[0].Kind != tuningJobKind || jobs[0].Trigger != "boot" || jobs[0].Status != "succeeded" || !strings.Contains(jobs[0].Output, "→") {
		t.Fatalf("jobs: %+v", jobs)
	}
	// Idempotent: a second run with nothing new changes nothing and records
	// no job.
	scripts := len(f.scripts)
	m.tuningCycle(context.Background(), "schedule", now.Add(time.Hour))
	m.mu.Lock()
	jobs = slices.Clone(m.data.HostUpdates.Jobs)
	m.mu.Unlock()
	if len(jobs) != 1 {
		t.Fatalf("second run recorded jobs: %+v", jobs)
	}
	for _, s := range f.scripts[scripts:] {
		if strings.Contains(s, "ALTER ROLE") || strings.Contains(s, "pg_reload_conf") {
			t.Fatalf("second run changed something: %q", s)
		}
	}
	// Paused overrides compute but never apply.
	if err := m.setTuningOverrides(TuningOverrides{Paused: true, ConnectionCaps: map[string]int{"shop": 33}}); err != nil {
		t.Fatal(err)
	}
	m.tuningCycle(context.Background(), "schedule", now.Add(2*time.Hour))
	if f.limits["app_shop"] == 33 {
		t.Fatalf("paused tuning applied an override: %v %q", f.limits, f.scripts[len(f.scripts)-3:])
	}
	view := m.resourceTuningView(context.Background(), false)
	if !view.Paused || len(view.Jobs) != 1 || view.Caps[0].Override != 33 {
		t.Fatalf("view: paused %v jobs %d caps %+v", view.Paused, len(view.Jobs), view.Caps)
	}
}

// configRedis records CONFIG SET and answers INFO for the shared server.
type configRedis struct {
	fakeRedisConn
	f *tuningHostFixture
}

func (c *configRedis) do(ctx context.Context, args ...string) (any, error) {
	switch strings.ToUpper(args[0]) {
	case "INFO":
		c.f.mu.Lock()
		defer c.f.mu.Unlock()
		policy := c.f.config["maxmemory-policy"]
		if policy == "" {
			policy = "noeviction"
		}
		max := int64(0)
		if v := c.f.config["maxmemory"]; v != "" {
			n, _ := strconv.ParseInt(strings.TrimSuffix(v, "mb"), 10, 64)
			max = n << 20
		}
		return fmt.Sprintf("# Memory\r\nused_memory:%d\r\nmaxmemory:%d\r\nmaxmemory_policy:%s\r\n", 100<<20, max, policy), nil
	case "CONFIG":
		c.f.mu.Lock()
		defer c.f.mu.Unlock()
		c.f.config[args[2]] = args[3]
		return "OK", nil
	}
	return c.fakeRedisConn.do(ctx, args...)
}
