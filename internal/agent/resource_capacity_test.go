package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestAutomaticCapacityUsesLiveMemoryNotSumOfCeilings(t *testing.T) {
	policy := defaultResourcePolicy(7941, 4)
	policy.BuildMemoryMB = 1024 // Reproduce the saved settings from the report.
	policy.Services["clickhouse"] = ServiceResources{MemoryMB: 1024, CPUs: 1}
	state := resourceState{Apps: map[string]App{}, Policy: &policy}
	for i := 0; i < 3; i++ {
		a := testApp(fmt.Sprintf("app-%d", i))
		state.Apps[a.ID] = a
	}
	containers := []ContainerResource{}
	for i := 0; i < 4; i++ {
		containers = append(containers, ContainerResource{Name: fmt.Sprintf("pg-%d", i), Kind: "database", Running: true, MemoryLimit: 1024 * mib})
	}
	for i := 0; i < 2; i++ {
		containers = append(containers, ContainerResource{Name: fmt.Sprintf("cache-%d", i), Kind: "cache", Running: true, MemoryLimit: 256 * mib})
	}
	containers = append(containers, ContainerResource{Name: "clickhouse", Kind: "clickhouse", Running: true, MemoryLimit: 1024 * mib})
	pending := map[string]resourceLaunch{"candidate": {Name: "candidate", AppID: "app-0", Kind: "web", MemoryMB: 512}}
	state = capacityState(state, containers, pending, true, 6553)
	report, err := makeResourceReport(state, containers, pending, true, 7941, 4)
	if err != nil || report.ReservedMemoryMB != 11776 || report.AvailableMemoryMB != 2969 {
		t.Fatal("incorrect ceiling/live headroom accounting", report, err)
	}
	if err := checkResourceCapacity(report, state); err != nil {
		t.Fatal("unused RAM ceilings blocked a safe automatic launch", err)
	}
	idle := capacityState(state, containers, nil, false, 6553)
	idleReport, err := makeResourceReport(idle, containers, nil, false, 7941, 4)
	if err != nil || idleReport.AvailableMemoryMB != 4505 {
		t.Fatal("idle build capacity is still permanently reserved", idleReport, err)
	}
	// Older saved policies without a mode automatically adopt the new default.
	policy.MemoryMode = ""
	if legacy, err := makeResourceReport(state, containers, pending, true, 7941, 4); err != nil || legacy.CapacityMode != "automatic" {
		t.Fatal("old policy still requires manual resource management", legacy, err)
	}
	policy.MemoryMode = "strict"
	strict, err := makeResourceReport(state, containers, pending, true, 7941, 4)
	if err != nil || strict.AvailableMemoryMB != -3835 || checkResourceCapacity(strict, state) == nil {
		t.Fatal("explicit strict reservation protection lost", strict, err)
	}
	policy.MemoryMode = "automatic"
	state = capacityState(state, containers, pending, true, 1024)
	low, err := makeResourceReport(state, containers, pending, true, 7941, 4)
	var pressure *resourceMemoryPressure
	if err != nil || !errors.As(checkResourceCapacity(low, state), &pressure) || pressure.ShortfallMB != 2560 {
		t.Fatal("unsafe launch admitted under actual memory pressure", low, err)
	}
	state.HostAvailableMB = nil
	unknown, err := makeResourceReport(state, containers, pending, true, 7941, 4)
	if err != nil || checkResourceCapacity(unknown, state) == nil {
		t.Fatal("unknown live memory admitted new work", err)
	}
}

func TestAutomaticCPUAllocationDoesNotReserveIdleApplications(t *testing.T) {
	state := resourceState{Apps: map[string]App{}}
	for i := 0; i < 6; i++ {
		a := testApp(fmt.Sprintf("app-%d", i))
		l := defaultAppResources()
		l.CPUs = 2
		a.Resources = &l
		state.Apps[a.ID] = a
	}
	containers := []ContainerResource{{Name: "live", AppID: "app-0", Kind: "web", Running: true, MemoryLimit: 512 * mib}}
	report, err := makeResourceReport(state, containers, nil, false, 8192, 4)
	if err != nil || report.Apps[0].AllocatedCPUs != 2 || report.Apps[1].AllocatedCPUs != 0 {
		t.Fatal("registered idle apps took CPU from live apps", report, err)
	}
}

func TestAutomaticBuildDefaultsScaleToHost(t *testing.T) {
	for _, row := range []struct{ host, build int64 }{{2048, 1024}, {7941, 2048}, {8192, 2048}, {32768, 4096}} {
		p := defaultResourcePolicy(row.host, 4)
		if p.BuildMemoryMB != row.build || p.MemoryMode != "automatic" {
			t.Fatal("build defaults do not match host capacity", row, p)
		}
	}
}

func TestAutomaticCapacityAccountsConcurrentPendingAndStoppedContainers(t *testing.T) {
	state := resourceState{BuilderName: "builder"}
	containers := []ContainerResource{{Name: "existing", Running: true}, {Name: "stopped", Running: false}, {Name: "builder", Running: true}}
	pending := map[string]resourceLaunch{"existing": {Name: "existing", MemoryMB: 1024}, "stopped": {Name: "stopped", MemoryMB: 256}, "new": {Name: "new", MemoryMB: 512}}
	state = capacityState(state, containers, pending, true, 4000)
	if state.PendingMemoryMB != 768 || !state.StartingWork {
		t.Fatal("pending capacity double counted running service or omitted a stopped/new launch", state)
	}
	state = capacityState(state, containers, map[string]resourceLaunch{"existing": pending["existing"]}, true, 4000)
	if state.PendingMemoryMB != 0 || state.StartingWork {
		t.Fatal("running work was treated as another launch", state)
	}
}

func TestResourceCapacityWaitRecoversWithoutUserRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	attempts := 0
	value, done, err := waitResourceCapacity(ctx, func(context.Context) (int, func(), error) {
		attempts++
		if attempts == 1 {
			return 0, nil, &resourceMemoryPressure{ShortfallMB: 128}
		}
		return 42, func() {}, nil
	})
	if err != nil || value != 42 || done == nil || attempts != 2 {
		t.Fatal("pressure did not automatically recover", value, attempts, err)
	}
	done()
}

func TestResourceCapacityWaitCancellationAndPermanentErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, done, err := waitResourceCapacity(ctx, func(context.Context) (int, func(), error) { return 0, nil, &resourceMemoryPressure{ShortfallMB: 128} })
	if err == nil || done != nil {
		t.Fatal("canceled pressure wait returned a launch reservation", err)
	}
	attempts := 0
	permanent := errors.New("Docker access refused")
	_, _, err = waitResourceCapacity(context.Background(), func(context.Context) (int, func(), error) { attempts++; return 0, nil, permanent })
	if !errors.Is(err, permanent) || attempts != 1 {
		t.Fatal("permanent failure retried as memory pressure", attempts, err)
	}
}
