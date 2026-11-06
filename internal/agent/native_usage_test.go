package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUsage answers `systemctl show` usage queries with the given CPU time
// per call and records every command.
type fakeUsage struct {
	mu       sync.Mutex
	commands [][]string
	cpu      uint64
	active   map[string]string
}

func (f *fakeUsage) run(_ context.Context, args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, slices.Clone(args))
	if len(args) > 2 && args[0] == "systemctl" && slices.Contains(args, "show") && slices.Contains(args, nativeUsageProperties) {
		var out []string
		for _, unit := range args {
			if !strings.HasSuffix(unit, ".service") {
				continue
			}
			state := f.active[unit]
			if state == "" {
				state = "active"
			}
			sub := "running"
			if state != "active" {
				sub = "dead"
			}
			out = append(out, fmt.Sprintf("Id=%s\nActiveState=%s\nSubState=%s\nUnitFileState=enabled\nActiveEnterTimestamp=@1730800000\nMainPID=4242\nMemoryCurrent=104857600\nMemoryMax=infinity\nCPUUsageNSec=%d\nTasksCurrent=7\nNRestarts=2", unit, state, sub, f.cpu))
		}
		f.cpu += 500_000_000
		return strings.Join(out, "\n\n"), nil
	}
	if args[0] == "systemctl" && slices.Contains(args, "show") {
		var out []string
		for _, unit := range args {
			if strings.HasSuffix(unit, ".service") {
				state := f.active[unit]
				if state == "" {
					state = "active"
				}
				out = append(out, "Id="+unit+"\nActiveState="+state+"\nSubState=running\nUnitFileState=enabled")
			}
		}
		return strings.Join(out, "\n\n"), nil
	}
	if args[0] == "journalctl" {
		return "journal of " + strings.Join(args[1:], " "), nil
	}
	return "", nil
}

func (f *fakeUsage) ran(prefix ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.commands, func(c []string) bool { return len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) })
}

func registeredNativeFixture(t *testing.T) (*Manager, *fakeUsage, NativeApplication) {
	t.Helper()
	m, _ := nativeFixture(t)
	a, err := m.nativeRegistration(NativeApplication{ID: "rails", Name: "MyRails", Domain: "rails.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeUsage{active: map[string]string{}}
	m.nativeCommand = f.run
	clock := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	oldClock, oldResample := nativeUsageClock, nativeUsageResample
	nativeUsageClock = func() time.Time { clock = clock.Add(time.Second); return clock }
	nativeUsageResample = time.Millisecond
	t.Cleanup(func() { nativeUsageClock, nativeUsageResample = oldClock, oldResample })
	return m, f, a
}

// The usage of a native application's units comes from their systemd
// cgroup accounting: memory, CPU time between two readings, tasks, the
// time the unit started and its restart count. The first read samples
// twice, so it already has a CPU percentage.
func TestNativeServiceUsageReadsSystemdAccounting(t *testing.T) {
	m, f, a := registeredNativeFixture(t)
	f.active["myrails-worker.service"] = "inactive"
	inventory, err := m.nativeServiceUsage(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Services) != 2 {
		t.Fatalf("services = %+v", inventory.Services)
	}
	web := inventory.Services[0]
	if web.Unit != "myrails-server.service" || web.Active != "active" || web.MemoryBytes == nil || *web.MemoryBytes != 100<<20 || web.MemoryLimitBytes != nil || web.Tasks == nil || *web.Tasks != 7 || web.Restarts != 2 || web.MainPID != 4242 || !web.Since.Equal(time.Unix(1730800000, 0)) {
		t.Fatalf("web usage = %+v", web)
	}
	// 0.5 s of CPU time over one second of wall time is half a core.
	if web.CPUPercent == nil || *web.CPUPercent < 49.9 || *web.CPUPercent > 50.1 {
		t.Fatalf("cpu = %v", web.CPUPercent)
	}
	worker := inventory.Services[1]
	if worker.Active != "inactive" || worker.MemoryBytes != nil || worker.CPUPercent != nil || worker.Tasks != nil {
		t.Fatalf("a stopped unit has no live usage: %+v", worker)
	}
	if !f.ran("systemctl", "--user", "show", nativeUsageProperties) {
		t.Fatalf("usage must read the deployment user's units: %v", f.commands)
	}
	if web.Restartable || worker.Restartable {
		t.Fatal("restart requires approved service control")
	}
	m.cfg.Native.ServiceControl = true
	if inventory, err = m.nativeServiceUsage(context.Background(), a.ID); err != nil || !inventory.Services[0].Restartable {
		t.Fatalf("service control enables restarts: %+v %v", inventory, err)
	}
	for _, c := range f.commands {
		if slices.Contains(c, "restart") || slices.Contains(c, "start") || slices.Contains(c, "stop") || c[0] == "docker" {
			t.Fatalf("reading usage changed a service or called Docker: %v", c)
		}
	}
}

func TestParseNativeUsageIgnoresUnaccountedValues(t *testing.T) {
	out := "Id=a.service\nActiveState=active\nSubState=running\nMemoryCurrent=[not set]\nMemoryMax=18446744073709551615\nCPUUsageNSec=[not set]\nTasksCurrent=18446744073709551615\nActiveEnterTimestamp=Thu 2026-11-05 10:00:00 UTC\n\nId=other.service\nActiveState=active"
	rows, cpu := parseNativeUsage(out, []string{"a.service"})
	row := rows["a.service"]
	if len(rows) != 1 || row.MemoryBytes != nil || row.MemoryLimitBytes != nil || row.Tasks != nil || len(cpu) != 0 {
		t.Fatalf("unaccounted values must be absent: %+v %v", rows, cpu)
	}
	if !row.Since.Equal(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("since = %v", row.Since)
	}
}

// Without a selected unit the runtime log interleaves every process.
func TestNativeLogsCombineEveryUnit(t *testing.T) {
	m, f, a := registeredNativeFixture(t)
	out, err := m.nativeLogs(context.Background(), a.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !f.ran("journalctl", "--user-unit=myrails-server.service", "--user-unit=myrails-worker.service", "--no-pager", "--lines=400", "--output=short-iso") || !strings.Contains(out, "journal of") {
		t.Fatalf("combined journal not read: %v", f.commands)
	}
	if _, err := m.nativeLogs(context.Background(), a.ID, "myrails-worker.service"); err != nil || !f.ran("journalctl", "--user-unit=myrails-worker.service", "--no-pager", "--lines=200", "--output=cat") {
		t.Fatalf("one unit's journal: %v %v", err, f.commands)
	}
	if _, err := m.nativeLogs(context.Background(), a.ID, "unrelated.service"); err == nil {
		t.Fatal("an unregistered unit must be refused")
	}
}

// The server process inventory lists native units in the container row
// shape, and needs no Docker on a host without containers.
func TestProcessInventoryIncludesNativeUnitsWithoutDocker(t *testing.T) {
	m, f, a := registeredNativeFixture(t)
	mux := http.NewServeMux()
	m.containerRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/containers", nil))
	var inventory ContainerInventory
	if err := json.Unmarshal(w.Body.Bytes(), &inventory); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(inventory.Containers) != 2 || inventory.Warning != "" {
		t.Fatalf("inventory = %+v", inventory)
	}
	row := inventory.Containers[0]
	if row.Runtime != "native" || row.AppID != a.ID || row.Unit != "myrails-server.service" || row.State != "running" || row.MemoryBytes == nil || row.CPUPercent == nil || row.PIDs != "7" || row.RestartCount == nil || *row.RestartCount != 2 || row.Restartable {
		t.Fatalf("native row = %+v", row)
	}
	for _, c := range f.commands {
		if c[0] == "docker" {
			t.Fatal("a native-only host must not call Docker")
		}
	}
	// Agent routes for usage.
	mux = http.NewServeMux()
	m.nativeRoutes(mux)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/native/apps/rails/processes", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"unit":"myrails-server.service"`) || !strings.Contains(w.Body.String(), `"memory_bytes":104857600`) {
		t.Fatalf("processes route: %d %s", w.Code, w.Body)
	}
}

// Units of applications with their own account are system units: usage
// and logs read the system manager, and a unit the release policy
// preserves is left alone by deploys but can be restarted explicitly.
func TestNativeSystemUnitUsageAndPreservedRestart(t *testing.T) {
	f := newNativeCreateFixture(t)
	if w := f.do(t, "POST", "/v1/native/apps", `{"id":"shop","repository":"acme/shop","runtime":"go","domain":"shop.example.com","data":{},"processes":[{"name":"web","command":"bin/shop","web":true},{"name":"jobs","command":"bin/jobs"}]}`); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	usage := &fakeUsage{active: map[string]string{}}
	f.m.nativeCommand = usage.run
	oldResample := nativeUsageResample
	nativeUsageResample = time.Millisecond
	t.Cleanup(func() { nativeUsageResample = oldResample })
	f.m.mu.Lock()
	policy := f.m.data.NativeCreated["shop"]
	policy.Units, policy.Preserve = []string{"lidza-app-shop-web.service"}, []string{"lidza-app-shop-jobs.service"}
	f.m.data.NativeCreated["shop"] = policy
	f.m.mu.Unlock()
	inventory, err := f.m.nativeServiceUsage(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	byUnit := map[string]NativeServiceUsage{}
	for _, s := range inventory.Services {
		byUnit[s.Unit] = s
	}
	// A preserved unit is left alone by deploys, not by the operator.
	if !byUnit["lidza-app-shop-web.service"].Restartable || !byUnit["lidza-app-shop-jobs.service"].Restartable || !byUnit["lidza-app-shop-jobs.service"].Preserved {
		t.Fatalf("restartable = %+v", inventory.Services)
	}
	if !usage.ran("systemctl", "show", nativeUsageProperties) || usage.ran("systemctl", "--user") {
		t.Fatalf("system units are read from the system manager: %v", usage.commands)
	}
	if err := f.m.nativeServiceAction(context.Background(), "shop", "lidza-app-shop-jobs.service", "restart", "shop"); err != nil || !usage.ran("systemctl", "restart", "--no-block", "lidza-app-shop-jobs.service") {
		t.Fatalf("an explicit restart of a preserved unit = %v %v", err, usage.commands)
	}
	if err := f.m.nativeServiceAction(context.Background(), "shop", "lidza-app-shop-web.service", "restart", "shop"); err != nil || !usage.ran("systemctl", "restart", "--no-block", "lidza-app-shop-web.service") {
		t.Fatalf("restart = %v %v", err, usage.commands)
	}
	if _, err := f.m.nativeLogs(context.Background(), "shop", ""); err != nil || !usage.ran("journalctl", "--unit=lidza-app-shop-jobs.service") && !usage.ran("journalctl", "--unit=lidza-app-shop-web.service") {
		t.Fatalf("system journal: %v %v", err, usage.commands)
	}
}

func TestJournalDiskUsage(t *testing.T) {
	for in, want := range map[string]int64{
		"Archived and active journals take up 1.5G in the file system.":  1610612736,
		"Archived and active journals take up 24.0M in the file system.": 25165824,
		"Journals take up 512B on disk.":                                 0,
		"no journal":                                                     0,
	} {
		if got := journalDiskUsage(in); got != want && !(in == "Journals take up 512B on disk." && got == 512) {
			t.Fatalf("%q = %d, want %d", in, got, want)
		}
	}
}

// The native health section reports unit states, failed units, the
// journal and the releases' disk space without Docker.
func TestNativeHealthSection(t *testing.T) {
	m, f, a := registeredNativeFixture(t)
	base := f.run
	m.nativeCommand = func(ctx context.Context, args []string) (string, error) {
		switch args[0] {
		case "journalctl":
			if slices.Contains(args, "--disk-usage") {
				return "Archived and active journals take up 2.0M in the file system.", nil
			}
		case "systemctl":
			if slices.Contains(args, "list-units") {
				return "nginx.service loaded failed failed A web server\n", nil
			}
		}
		return base(ctx, args)
	}
	release := filepath.Join(m.cfg.Native.Root, a.Name, "releases", "one")
	if err := os.WriteFile(filepath.Join(release, "app.bin"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	oldSync := releaseDiskSync
	releaseDiskSync = true
	t.Cleanup(func() { releaseDiskSync = oldSync })
	health, ok := m.nativeHealth(context.Background())
	if !ok {
		t.Fatal("native section missing")
	}
	if health.Applications != 1 || health.Units != 2 || health.Active != 2 || !slices.Equal(health.FailedHostUnits, []string{"nginx.service"}) || health.JournalBytes != 2<<20 {
		t.Fatalf("health = %+v", health)
	}
	if len(health.Releases) != 1 || health.Releases[0].AppID != a.ID || health.Releases[0].Releases != 1 || health.Releases[0].Bytes < 4096 {
		t.Fatalf("releases = %+v", health.Releases)
	}
	oldDocker := dockerInstalled
	dockerInstalled = func() bool { return false }
	t.Cleanup(func() { dockerInstalled = oldDocker })
	var out ServerHealth
	m.runtimeHealth(context.Background(), &out)
	if out.Docker != nil || out.Native == nil {
		t.Fatalf("a host without Docker has only the native section: %+v", out)
	}
}

// Native application health comes from unit states, never an HTTP probe.
func TestNativeApplicationHealthFromUnitStates(t *testing.T) {
	m, f, a := registeredNativeFixture(t)
	for _, tc := range []struct{ web, worker, want string }{
		{"active", "active", "healthy"},
		{"active", "failed", "unhealthy"},
		{"inactive", "inactive", "stopped"},
	} {
		f.active["myrails-server.service"], f.active["myrails-worker.service"] = tc.web, tc.worker
		health := m.nativeApplicationHealth(context.Background())
		if len(health) != 1 || health[0].AppID != a.ID || health[0].State != tc.want {
			t.Fatalf("%+v: %+v", tc, health)
		}
	}
}
