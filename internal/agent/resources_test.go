package agent

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResourceBudgetIncludesWorkersServicesAndRollout(t *testing.T) {
	a := testApp("one")
	b := testApp("two")
	state := resourceState{Apps: map[string]App{a.ID: a, b.ID: b}, Tasks: []Task{{AppID: a.ID, Enabled: true}, {AppID: a.ID, Enabled: false}}}
	report, err := makeResourceReport(state, []ContainerResource{{Name: "pg", Kind: "database", Running: true, MemoryLimit: 1024 * mib}, {Name: "cache", Kind: "cache", Running: true, MemoryLimit: 256 * mib}}, nil, false, 8192, 4)
	if err != nil {
		t.Fatal(err)
	}
	if report.Apps[0].RequiredMemoryMB != 1280 {
		t.Fatal("missing rollout/worker reservation", report.Apps[0])
	}
	if err = checkResourceCapacity(report, state); err == nil || !strings.Contains(err.Error(), "1280") {
		t.Fatal("accepted worker beyond app budget", err)
	}
	l := a.resourceLimits()
	l.MemoryMB = 1280
	a.Resources = &l
	state.Apps[a.ID] = a
	report, err = makeResourceReport(state, report.Services, nil, false, 8192, 4)
	if err != nil || checkResourceCapacity(report, state) != nil {
		t.Fatal(err, report)
	}
	expected := report.Policy.HostMemoryMB + report.Policy.BuildMemoryMB + report.Policy.OperationMemoryMB + 1280 + 1024 + 1024 + 256
	if report.ReservedMemoryMB != expected {
		t.Fatalf("reserved %d, want %d", report.ReservedMemoryMB, expected)
	}
	report, err = makeResourceReport(state, report.Services, nil, false, 2048, 2)
	if err != nil {
		t.Fatal(err)
	}
	if checkResourceCapacity(report, state) == nil {
		t.Fatal("accepted host overcommit")
	}
}
func TestResourceCPUFairnessIsPerAppAndReservesHostAndBuild(t *testing.T) {
	a, b := testApp("one"), testApp("two")
	state := resourceState{Apps: map[string]App{a.ID: a, b.ID: b}}
	containers := []ContainerResource{{Name: "one-web", AppID: a.ID, Kind: "web", Running: true, MemoryLimit: 512 * mib}, {Name: "two-web", AppID: b.ID, Kind: "web", Running: true, MemoryLimit: 512 * mib}, {Name: "one-worker", AppID: a.ID, Kind: "worker", Running: true, MemoryLimit: 256 * mib}}
	report, err := makeResourceReport(state, containers, nil, true, 8192, 2)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(report.Apps[0].AllocatedCPUs-report.Apps[1].AllocatedCPUs) > 0.000001 {
		t.Fatal("worker added extra app CPU share", report.Apps)
	}
	sum := report.Apps[0].AllocatedCPUs + report.Apps[1].AllocatedCPUs
	if sum > 2-report.Policy.HostCPUs-report.Policy.BuildCPUs-report.Policy.OperationCPUs+0.000001 {
		t.Fatal("lost CPU headroom", sum)
	}
	l := a.resourceLimits()
	l.Weight = 2048
	a.Resources = &l
	state.Apps[a.ID] = a
	report, _ = makeResourceReport(state, containers, nil, true, 8192, 2)
	if math.Abs(report.Apps[0].AllocatedCPUs/report.Apps[1].AllocatedCPUs-2) > 0.000001 {
		t.Fatal("priority ratio lost", report.Apps)
	}
}
func TestResourceMeasurementsDistinguishMissingSamplesAndStoppedApps(t *testing.T) {
	a := testApp("one")
	state := resourceState{Apps: map[string]App{a.ID: a}}
	c := ContainerResource{Name: "one-web", AppID: a.ID, Kind: "web", Running: true, MemoryLimit: 512 * mib, CPUQuota: 1}
	report, err := makeResourceReport(state, []ContainerResource{c}, nil, false, 8192, 4)
	if err != nil {
		t.Fatal(err)
	}
	if report.Apps[0].MemoryBytes != nil || report.Apps[0].CPUPercent != nil {
		t.Fatal("missing sample presented as zero")
	}
	used, cpu := int64(40*mib), float64(12)
	c.MemoryBytes = &used
	c.CPUPercent = &cpu
	report, _ = makeResourceReport(state, []ContainerResource{c}, nil, false, 8192, 4)
	if *report.Apps[0].MemoryBytes != used || *report.Apps[0].CPUPercent != cpu {
		t.Fatal(report.Apps[0])
	}
	a.Stopped = true
	state.Apps[a.ID] = a
	report, _ = makeResourceReport(state, []ContainerResource{c}, nil, false, 8192, 4)
	if report.Apps[0].AllocatedCPUs == 0 {
		t.Fatal("running process of failed stop was not accounted")
	}
	c.Running = false
	report, _ = makeResourceReport(state, []ContainerResource{c}, nil, false, 8192, 4)
	if report.Apps[0].AllocatedCPUs != 0 || *report.Apps[0].MemoryBytes != 0 {
		t.Fatal("stopped container consumes app runtime budget", report)
	}
}
func TestResourceServiceOverridesAndIdleBuildersRemainAccounted(t *testing.T) {
	a := testApp("one")
	policy := defaultResourcePolicy(8192, 4)
	policy.Services["pg"] = ServiceResources{256, 0.25}
	state := resourceState{Apps: map[string]App{a.ID: a}, Policy: &policy, BuilderName: "own-builder"}
	report, err := makeResourceReport(state, []ContainerResource{{Name: "pg", Kind: "database", Running: true, MemoryLimit: 1024 * mib}, {Name: "own-builder", Kind: "build", Running: true, MemoryLimit: 1024 * mib}, {Name: "other-builder", Kind: "build", Running: true, MemoryLimit: 512 * mib, CPUQuota: 0.5}}, nil, false, 8192, 4)
	if err != nil {
		t.Fatal(err)
	}
	expected := policy.HostMemoryMB + policy.BuildMemoryMB + policy.OperationMemoryMB + 1024 + 256 + 512
	if report.ReservedMemoryMB != expected || !report.Building {
		t.Fatal("missing retained service or another builder", report)
	}
}
func TestResourceMemoryParsingAndValidation(t *testing.T) {
	for raw, want := range map[string]int64{"0B": 0, "1.5GiB": 1536 * mib, "512 MiB": 512 * mib, "128kB": 128000} {
		got, err := parseMemory(raw)
		if err != nil || got != want {
			t.Fatal(raw, got, err)
		}
	}
	for _, raw := range []string{"", "NaNMiB", "-1GiB", "99unknown", "1e99GiB"} {
		if _, err := parseMemory(raw); err == nil {
			t.Fatal("invalid measurement", raw)
		}
	}
	for _, cpu := range []float64{0, -1, math.NaN(), math.Inf(1), 129} {
		l := defaultAppResources()
		l.CPUs = cpu
		if l.Validate() == nil {
			t.Fatal("invalid CPU limit", cpu)
		}
	}
	args := replaceResourceArgs([]string{"create", "--memory", "512m", "--cpus", "1", "--read-only", "--name", "fixture"}, resourceLimitsArgs(128, 0.25, 512))
	if strings.Count(strings.Join(args, " "), "--memory ") != 1 || !strings.Contains(strings.Join(args, " "), "--cpus 0.25") {
		t.Fatal(args)
	}
}
func TestLegacyResourceMigrationPreservesWorkerMemoryAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	a := testApp("one")
	data := diskState{Apps: map[string]App{a.ID: a}, Tasks: map[string]Task{"one:worker": {ID: "worker", AppID: "one", Enabled: true}}}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "state.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(context.Background(), Config{DataDir: dir}, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	l := m.data.Apps["one"].resourceLimits()
	if l.MemoryMB != 1536 || l.WorkerMemoryMB != 512 {
		t.Fatal("legacy memory caps were reduced", l)
	}
	m.Close()
	m, err = NewManager(context.Background(), Config{DataDir: dir}, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.data.Apps["one"].resourceLimits() != l {
		t.Fatal("budgets lost on restart")
	}
}
func FuzzResourceCPUAllocation(f *testing.F) {
	f.Add(uint16(4), uint16(1024), uint16(1024), uint16(100))
	f.Fuzz(func(t *testing.T, poolRaw, aRaw, bRaw, capRaw uint16) {
		pool := float64(poolRaw%1600+1) / 100
		cap := float64(capRaw%1600+1) / 100
		wanted := map[string]float64{"a": cap, "b": cap}
		weights := map[string]int64{"a": int64(aRaw%8192 + 1), "b": int64(bRaw%8192 + 1)}
		out := fairCPU(pool, wanted, weights)
		sum := float64(0)
		for key, value := range out {
			if math.IsNaN(value) || value < 0 || value > wanted[key]+0.0000001 {
				t.Fatal(out)
			}
			sum += value
		}
		if sum > pool+0.0000001 {
			t.Fatal("allocation exceeds pool", pool, out)
		}
	})
}

func TestResourceOperationalPoolBoundsConcurrentCommands(t *testing.T) {
	m := &Manager{cfg: Config{DataDir: t.TempDir()}}
	done := []func(){}
	for i := 0; i < 4; i++ {
		args, release, err := m.prepareResourceOperation(context.Background(), "lidza-operation-fixture-"+newID(), 128)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(args, " "), "--cpus 0.12") {
			t.Fatal("operation CPU does not fit shared pool", args)
		}
		done = append(done, release)
		defer release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := m.prepareResourceOperation(ctx, "lidza-operation-fixture-"+newID(), 128); err == nil {
		t.Fatal("operational pool overcommitted")
	}
	done[0]()
	done[0]()
	if m.helperMemory != 384 {
		t.Fatal("pool release not idempotent", m.helperMemory)
	}
	_, release, err := m.prepareResourceOperation(context.Background(), "lidza-operation-fixture-"+newID(), 128)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if m.helperMemory != 512 {
		t.Fatal("released slot cannot be reused", m.helperMemory)
	}
}
func TestResourceCPUUpdateSupportsBothDockerControllerModes(t *testing.T) {
	for _, c := range []ContainerResource{{NanoCPUs: 1e9}, {CPUPeriod: 100000, RawQuota: 100000}} {
		args := strings.Join(resourceUpdateArgs(c, 128, 0.25, 512), " ")
		if c.NanoCPUs > 0 {
			if !strings.Contains(args, "--cpus 0.25") || strings.Contains(args, "--cpu-quota") {
				t.Fatal("mixed Docker CPU controller modes", args)
			}
		} else if strings.Contains(args, "--cpus ") || !strings.Contains(args, "--cpu-quota 25000") {
			t.Fatal("BuildKit CPU controller cannot be updated", args)
		}
	}
}

func TestResourceAccountingIncludesManagedClickHouse(t *testing.T) {
	a := testApp("one")
	state := resourceState{Apps: map[string]App{a.ID: a}}
	containers := []ContainerResource{{Name: "lidza-clickhouse-one", Kind: "clickhouse", Running: true, MemoryLimit: 2048 * mib}}
	report, err := makeResourceReport(state, containers, nil, false, 8192, 4)
	if err != nil {
		t.Fatal(err)
	}
	expected := report.Policy.HostMemoryMB + report.Policy.BuildMemoryMB + report.Policy.OperationMemoryMB + 1024 + 2048
	if report.ReservedMemoryMB != expected {
		t.Fatal("ClickHouse RAM not reserved", report.ReservedMemoryMB, expected)
	}
	if l := serviceLimit(report.Policy, containers[0].Name, "clickhouse"); l.MemoryMB != 2048 || l.CPUs != 1 {
		t.Fatal("ClickHouse default limits", l)
	}
	policy := report.Policy
	policy.Services = map[string]ServiceResources{containers[0].Name: {MemoryMB: 1024, CPUs: 0.5}}
	state.Policy = &policy
	report, err = makeResourceReport(state, containers, nil, false, 8192, 4)
	if err != nil || report.ReservedMemoryMB != expected-1024 {
		t.Fatal("ClickHouse override not accounted", report, err)
	}
}
