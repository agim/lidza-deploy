package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/buildmem"
	"github.com/agim/lidza-deploy/internal/release"
)

// syncBuffer is a log that concurrent steps can write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// fakeHost is host memory that a test changes while a step waits.
type fakeHost struct {
	mu sync.Mutex
	h  buildmem.Host
}

func (f *fakeHost) get() buildmem.Host  { f.mu.Lock(); defer f.mu.Unlock(); return f.h }
func (f *fakeHost) set(h buildmem.Host) { f.mu.Lock(); f.h = h; f.mu.Unlock() }

func fastBuildWaits(m *Manager, host *fakeHost) {
	m.build.memory = host.get
	m.build.poll = 2 * time.Millisecond
	m.build.heartbeat = 5 * time.Millisecond
	m.build.wait = 200 * time.Millisecond
}

func TestParseMeminfo(t *testing.T) {
	h := parseMeminfo("MemTotal:       16384000 kB\nMemFree:  1 kB\nMemAvailable:    8192000 kB\nSwapTotal: 2048000 kB\nSwapFree: 1024000 kB\n")
	if h.TotalMiB != 16000 || h.AvailableMiB != 8000 || h.SwapTotalMiB != 2000 || h.SwapFreeMiB != 1000 {
		t.Fatalf("%+v", h)
	}
}

func TestBuildStepCapFollowsFreeMemoryAndLearnedPeaks(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	host := &fakeHost{h: buildmem.Host{TotalMiB: 16384, AvailableMiB: 12288}}
	fastBuildWaits(m, host)
	var limits []buildLimits
	run := func(peak int) buildRun {
		return func(_ context.Context, l buildLimits) (int, error) { limits = append(limits, l); return peak, nil }
	}
	var log bytes.Buffer
	step := buildStep{App: "shop", Step: "npm ci", Build: true, Log: &log}
	// Nothing learned: a quarter of the host, with most of its CPU.
	if err := m.runBuildStep(context.Background(), step, run(1400)); err != nil {
		t.Fatal(err)
	}
	if limits[0].MemoryMiB != 4096 || limits[0].CPUQuota != 300 || limits[0].Swap {
		t.Fatalf("first build %+v", limits[0])
	}
	if !strings.Contains(log.String(), "npm ci: peak 1.4 GiB of 4.0 GiB") {
		t.Fatalf("peak line missing:\n%s", log.String())
	}
	// The learned peak sizes the next cap: 1.5 × 1400.
	if err := m.runBuildStep(context.Background(), step, run(1000)); err != nil {
		t.Fatal(err)
	}
	if limits[1].MemoryMiB != 2100 {
		t.Fatalf("learned cap %+v", limits[1])
	}
	// The explicit setting is the ceiling.
	explicit := step
	explicit.Explicit = 1024
	if err := m.runBuildStep(context.Background(), explicit, run(900)); err != nil {
		t.Fatal(err)
	}
	if limits[2].MemoryMiB != 1024 {
		t.Fatalf("explicit cap %+v", limits[2])
	}
	// Free memory bounds the cap: 1638 MiB reserve, 1800 MiB budget.
	host.set(buildmem.Host{TotalMiB: 16384, AvailableMiB: 3438})
	if err := m.runBuildStep(context.Background(), step, run(1000)); err != nil {
		t.Fatal(err)
	}
	if limits[3].MemoryMiB != 1800 {
		t.Fatalf("free-memory cap %+v", limits[3])
	}
	// Non-build commands get a cap without learning or waiting.
	host.set(buildmem.Host{TotalMiB: 16384, AvailableMiB: 100})
	if err := m.runBuildStep(context.Background(), buildStep{App: "shop", Step: "git fetch"}, run(50)); err != nil {
		t.Fatal(err)
	}
	if limits[4].MemoryMiB != buildmem.FloorMiB || m.learnedBuildPeak("shop", "git fetch") != 0 {
		t.Fatalf("non-build command %+v", limits[4])
	}
}

func TestBuildStepWaitsForMemoryThenFailsClearly(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	host := &fakeHost{h: buildmem.Host{TotalMiB: 8192, AvailableMiB: 1000}}
	fastBuildWaits(m, host)
	m.recordBuildPeak("shop", BuildPeak{Step: "lidza build", PeakMiB: 2500, LimitMiB: 4096})
	var log syncBuffer
	step := buildStep{App: "shop", Step: "lidza build", Build: true, Log: &log}
	ran := 0
	run := func(_ context.Context, l buildLimits) (int, error) { ran++; return 2000, nil }
	// Memory frees up while the step waits.
	go func() { time.Sleep(20 * time.Millisecond); host.set(buildmem.Host{TotalMiB: 8192, AvailableMiB: 7000}) }()
	if err := m.runBuildStep(context.Background(), step, run); err != nil || ran != 1 {
		t.Fatal(ran, err)
	}
	for _, want := range []string{"Waiting for memory: lidza build needs ~2.4 GiB", "Memory free after"} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, log.String())
		}
	}
	// It never frees up: the bounded wait ends with the shortage.
	host.set(buildmem.Host{TotalMiB: 8192, AvailableMiB: 1000})
	err := m.runBuildStep(context.Background(), step, run)
	if err == nil || ran != 1 || !strings.Contains(err.Error(), "this server does not have enough free memory to build shop (needs ~2.4 GiB") || !strings.Contains(err.Error(), "8.0 GiB RAM") {
		t.Fatal("expected a shortage", err)
	}
	if !strings.Contains(log.String(), "Still waiting for memory") {
		t.Fatalf("no heartbeat:\n%s", log.String())
	}
	// More than the whole host can give fails without waiting.
	m.recordBuildPeak("shop", BuildPeak{Step: "lidza build", PeakMiB: 9000, LimitMiB: 9000})
	started := time.Now()
	if err := m.runBuildStep(context.Background(), step, run); err == nil || time.Since(started) > 100*time.Millisecond {
		t.Fatal("impossible build must fail at once", err)
	}
}

func TestBuildStepRetriesOnceWithMoreMemoryAfterRunningOut(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	host := &fakeHost{h: buildmem.Host{TotalMiB: 16384, AvailableMiB: 14000, SwapTotalMiB: 2048}}
	fastBuildWaits(m, host)
	var log bytes.Buffer
	var limits []buildLimits
	run := func(_ context.Context, l buildLimits) (int, error) {
		limits = append(limits, l)
		if len(limits) == 1 {
			return 0, fmt.Errorf("npm ci %w", errBuildOutOfMemory)
		}
		return 5000, nil
	}
	step := buildStep{App: "shop", Step: "npm ci", Build: true, Log: &log}
	if err := m.runBuildStep(context.Background(), step, run); err != nil {
		t.Fatal(err)
	}
	if len(limits) != 2 || limits[1].MemoryMiB != 2*limits[0].MemoryMiB || !limits[1].Swap || limits[0].Swap {
		t.Fatalf("retry limits %+v", limits)
	}
	if !strings.Contains(log.String(), "npm ci ran out of memory at 4.0 GiB; retrying once with 8.0 GiB and swap") {
		t.Fatalf("retry not logged:\n%s", log.String())
	}
	records := m.data.BuildPeaks["shop"]
	if len(records) != 2 || !records[0].OutOfMemory || records[0].PeakMiB != 4096 || records[1].PeakMiB != 5000 {
		t.Fatalf("records %+v", records)
	}
	// Out of memory twice: a clear failure with the peak and the RAM.
	limits = nil
	alwaysOOM := func(_ context.Context, l buildLimits) (int, error) {
		limits = append(limits, l)
		return 0, errBuildOutOfMemory
	}
	err := m.runBuildStep(context.Background(), step, alwaysOOM)
	if len(limits) != 2 || err == nil || !strings.Contains(err.Error(), "ran out of memory again") || !strings.Contains(err.Error(), "16.0 GiB of RAM") {
		t.Fatal(len(limits), err)
	}
	// No room for more memory: no retry, the shortage explains why.
	host.set(buildmem.Host{TotalMiB: 4096, AvailableMiB: 3000})
	limits = nil
	err = m.runBuildStep(context.Background(), buildStep{App: "blog", Step: "npm run build", Build: true, Log: &log}, alwaysOOM)
	if len(limits) != 1 || err == nil || !strings.Contains(err.Error(), "does not have enough free memory to build blog") {
		t.Fatal(len(limits), err)
	}
	// An explicit setting without swap has nothing to retry with.
	limits = nil
	host.set(buildmem.Host{TotalMiB: 16384, AvailableMiB: 14000})
	err = m.runBuildStep(context.Background(), buildStep{App: "blog", Step: "npm run build", Build: true, Explicit: 1024, Log: &log}, alwaysOOM)
	if len(limits) != 1 || err == nil || !strings.Contains(err.Error(), "raise the build memory") {
		t.Fatal(len(limits), err)
	}
}

func TestNativeReleaseExecutorRetriesBuildStepsOnly(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	calls := map[string]int{}
	m.nativeReleaseRun = func(_ context.Context, c release.Command) error {
		calls[c.Name]++
		if calls[c.Name] == 1 {
			return fmt.Errorf("%s %w", c.Name, errBuildOutOfMemory)
		}
		return nil
	}
	var log bytes.Buffer
	x := nativeReleaseExecutor{m: m, app: "shop", root: "/srv/apps/shop"}
	if err := x.Run(context.Background(), release.Command{Name: "assets:precompile", Build: true, Stderr: &log}); err != nil || calls["assets:precompile"] != 2 {
		t.Fatal(calls, err)
	}
	if err := x.Run(context.Background(), release.Command{Name: "db:prepare", Stderr: &log}); err == nil || calls["db:prepare"] != 1 {
		t.Fatal("release steps are not retried", calls, err)
	}
	if m.learnedBuildPeak("shop", "assets:precompile") == 0 || m.learnedBuildPeak("shop", "db:prepare") != 0 {
		t.Fatal("peaks are learned for build steps only", m.data.BuildPeaks)
	}
}

func TestBuildPeaksPersistAndStayBounded(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DataDir: dir, APIKey: strings.Repeat("a", 32)}
	m, err := NewManager(context.Background(), cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < buildPeaksPerApp+10; i++ {
		m.recordBuildPeak("shop", BuildPeak{Step: "npm ci", PeakMiB: 100 + i, LimitMiB: 2048})
	}
	m.recordBuildPeak("shop", BuildPeak{Step: "go build", PeakMiB: 900, LimitMiB: 2048})
	m.Close()
	again, err := NewManager(context.Background(), cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	records := again.data.BuildPeaks["shop"]
	if len(records) != buildPeaksPerApp || records[len(records)-1].Step != "go build" || records[0].At.IsZero() {
		t.Fatalf("%d records after reload", len(records))
	}
	// The learned peak is the largest of the step's recent records.
	if peak := again.learnedBuildPeak("shop", "npm ci"); peak != 100+buildPeaksPerApp+9 {
		t.Fatal("learned peak", peak)
	}
	if again.buildPeakOf("shop") != 900 || again.buildPeakOf("other") != 0 {
		t.Fatal("application peak", again.buildPeakOf("shop"))
	}
}

func TestBuildSlotSerializesBuildsAndShowsTheWait(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.data.Deployments = append(m.data.Deployments,
		Deployment{ID: "d-shop", AppID: "shop", Status: "building", Created: time.Now().UTC()},
		Deployment{ID: "d-blog", AppID: "blog", Status: "building", Created: time.Now().UTC()},
		Deployment{ID: "d-wiki", AppID: "wiki", Status: "queued", Created: time.Now().UTC()})
	ctx := func(id string) context.Context { return context.WithValue(context.Background(), deploymentIDKey{}, id) }
	var shopLog, blogLog syncBuffer
	release, err := m.acquireBuildSlot(ctx("d-shop"), "shop", &shopLog)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	order := []string{"shop"}
	acquired := make(chan struct{})
	go func() {
		release2, err := m.acquireBuildSlot(ctx("d-blog"), "blog", &blogLog)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		order = append(order, "blog")
		mu.Unlock()
		release2()
		close(acquired)
	}()
	deadline := time.Now().Add(2 * time.Second)
	var waiting *Progress
	for time.Now().Before(deadline) && waiting == nil {
		for _, d := range m.Deployments() {
			if d.ID == "d-blog" && d.Progress != nil {
				waiting = d.Progress
			}
		}
		time.Sleep(time.Millisecond)
	}
	if waiting == nil || waiting.Phase != "waiting for another build on this server (deploying shop)" {
		t.Fatalf("waiting phase %+v", waiting)
	}
	for _, d := range m.Deployments() {
		if d.ID == "d-wiki" && (d.Progress == nil || !strings.Contains(d.Progress.Phase, "deploying shop")) {
			t.Fatalf("queued deployment does not show the build it waits for: %+v", d.Progress)
		}
	}
	select {
	case <-acquired:
		t.Fatal("two builds overlapped")
	case <-time.After(20 * time.Millisecond):
	}
	mu.Lock()
	order = append(order, "shop done")
	mu.Unlock()
	release()
	release() // releasing twice is harmless
	<-acquired
	if strings.Join(order, ",") != "shop,shop done,blog" {
		t.Fatal("order", order)
	}
	if !strings.Contains(blogLog.String(), "Waiting for another build on this server (deploying shop)") || !strings.Contains(blogLog.String(), "Build slot free after") {
		t.Fatalf("blog log:\n%s", blogLog.String())
	}
	for _, d := range m.Deployments() {
		if d.ID == "d-blog" && d.Progress != nil {
			t.Fatal("waiting phase outlived the wait")
		}
	}
	// A waiter that gives up leaves the queue; the next one still gets the slot.
	release, _ = m.acquireBuildSlot(context.Background(), "shop", nil)
	cancelled, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error)
	go func() { _, err := m.acquireBuildSlot(cancelled, "blog", nil); gaveUp <- err }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	release()
	next, err := m.acquireBuildSlot(context.Background(), "wiki", nil)
	if err != nil {
		t.Fatal(err)
	}
	next()
}

func TestContainerImageBuildsHoldTheSlot(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	ctx := withBuildSlot(context.Background(), m.containerBuildSlot("shop"))
	release, err := holdBuildSlot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if app, _, held := m.build.gate.current(); !held || app != "shop" {
		t.Fatal("container build does not hold the slot", app, held)
	}
	release()
	if _, _, held := m.build.gate.current(); held {
		t.Fatal("slot not released")
	}
	if release, err := holdBuildSlot(context.Background()); err != nil || release == nil {
		t.Fatal("no slot in context is a no-op")
	}
}

func TestUnitMemoryFromJournalAndCgroup(t *testing.T) {
	out := parseUnitJournal("lidza-x.service: Main process exited, code=killed, status=9/KILL\nlidza-x.service: Failed with result 'oom-kill'.\nlidza-x.service: Consumed 12.1s CPU time, 1.5G memory peak, 0B memory swap peak.\n")
	if !out.OutOfMemory || out.PeakBytes != 1536<<20 {
		t.Fatalf("%+v", out)
	}
	if out := parseUnitJournal("Consumed 1s CPU time, 245.1M memory peak.\n"); out.OutOfMemory || out.PeakBytes>>20 != 245 {
		t.Fatalf("%+v", out)
	}
	if out := parseUnitJournal("Consumed 1s CPU time.\n"); out.PeakBytes != 0 {
		t.Fatal("old systemd logs no peak", out)
	}
	root := t.TempDir()
	group := filepath.Join(root, "system.slice", "lidza-x.service")
	if err := os.MkdirAll(group, 0o755); err != nil {
		t.Fatal(err)
	}
	oldRoot, oldGroup, oldInterval := cgroupRoot, unitControlGroup, unitSampleInterval
	defer func() { cgroupRoot, unitControlGroup, unitSampleInterval = oldRoot, oldGroup, oldInterval }()
	cgroupRoot, unitSampleInterval = root, time.Millisecond
	unitControlGroup = func(string) string { return "/system.slice/lidza-x.service" }
	// Without memory.peak the largest memory.current sample counts.
	os.WriteFile(filepath.Join(group, "memory.current"), []byte("1000\n"), 0o644)
	stop := watchUnitMemory("lidza-x.service")
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(filepath.Join(group, "memory.current"), []byte("5000\n"), 0o644)
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(filepath.Join(group, "memory.current"), []byte("2000\n"), 0o644)
	time.Sleep(10 * time.Millisecond)
	if peak := stop(); peak != 5000 {
		t.Fatal("sampled peak", peak)
	}
	os.WriteFile(filepath.Join(group, "memory.peak"), []byte("9000\n"), 0o644)
	if sampleControlGroup("/system.slice/lidza-x.service") != 9000 || sampleControlGroup("/../etc") != 0 {
		t.Fatal("memory.peak is preferred; paths stay inside the cgroup tree")
	}
}

func TestBuildStepNames(t *testing.T) {
	for args, want := range map[string]string{"npm ci --no-audit": "npm ci", "npm run build": "npm run build", "bundle exec rails assets:precompile": "bundle exec rails"} {
		if got := buildStepName(strings.Fields(args)); got != want {
			t.Errorf("%q: %q", args, got)
		}
	}
}
