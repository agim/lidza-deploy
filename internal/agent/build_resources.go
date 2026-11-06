package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agim/lidza-deploy/internal/buildmem"
)

// Build resources: every build on this host (native releases, static
// sites, container images) runs one at a time behind a FIFO build slot,
// each build step gets a memory cap sized from the host's free memory and
// the step's learned peak (internal/buildmem), waits a bounded time when
// memory is short, records its peak, and is retried once with more memory
// when it runs out. Release, publish and restart work runs outside the slot.

const (
	// buildMemoryWait bounds how long a step waits for free memory.
	buildMemoryWait = 10 * time.Minute
	// buildPeaksPerApp bounds the learned peaks kept per application.
	buildPeaksPerApp = 48
	// buildPeakWindow is how many recent records of a step count.
	buildPeakWindow = 5
)

// BuildPeak is the measured memory peak of one build step attempt.
type BuildPeak struct {
	Step        string    `json:"step"`
	PeakMiB     int       `json:"peak_mib"`
	LimitMiB    int       `json:"limit_mib"`
	OutOfMemory bool      `json:"out_of_memory,omitempty"`
	At          time.Time `json:"at"`
}

// buildLimits are the resources of one step attempt.
type buildLimits struct {
	MemoryMiB int
	// Swap allows the step to use swap (MemorySwapMax unset); steps
	// normally run without, so running out of memory fails fast.
	Swap bool
	// CPUQuota is the step's CPU quota in percent of one core.
	CPUQuota int
}

// buildStep describes one command to run under build resources.
type buildStep struct {
	App      string // application whose peaks are learned
	Step     string // step name, for example "npm ci"
	Explicit int    // the operator's build memory setting in MiB; 0 adapts
	// Build marks a build-phase step: it waits for free memory, records
	// its peak and is retried once when it runs out of memory. Other
	// commands (fetches, migrations) only get a cap.
	Build bool
	Log   io.Writer // where cap, peak, wait and retry lines go
}

// buildRun runs one attempt with limits and reports its peak in MiB (0 if
// unknown). It wraps errBuildOutOfMemory when the attempt ran out of memory.
type buildRun func(ctx context.Context, limits buildLimits) (int, error)

var errBuildOutOfMemory = errors.New("ran out of memory")

// buildResources is the host-wide build state of a Manager. The zero value
// reads the host's memory and uses the default timings.
type buildResources struct {
	gate buildGate
	// Test hooks.
	memory    func() buildmem.Host
	cpus      func() int
	wait      time.Duration // longest wait for memory
	poll      time.Duration // memory re-check interval
	heartbeat time.Duration // interval of "still waiting" lines
}

func (r *buildResources) host() buildmem.Host {
	if r.memory != nil {
		return r.memory()
	}
	return readHostMemory()
}

func (r *buildResources) cpuQuota() int {
	cpus := runtime.NumCPU()
	if r.cpus != nil {
		cpus = r.cpus()
	}
	return buildmem.CPUQuotaPercent(cpus)
}

func durationOr(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

// readHostMemory reads the host's memory from /proc/meminfo; tests replace it.
var readHostMemory = func() buildmem.Host {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return buildmem.Host{}
	}
	return parseMeminfo(string(raw))
}

func parseMeminfo(raw string) buildmem.Host {
	var h buildmem.Host
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kib, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			h.TotalMiB = kib / 1024
		case "MemAvailable:":
			h.AvailableMiB = kib / 1024
		case "SwapTotal:":
			h.SwapTotalMiB = kib / 1024
		case "SwapFree:":
			h.SwapFreeMiB = kib / 1024
		}
	}
	return h
}

// buildGate is a FIFO semaphore of one: the host's build slot.
type buildGate struct {
	mu     sync.Mutex
	holder *buildTicket
	queue  []*buildTicket
}

type buildTicket struct {
	app, deployment string
	ready           chan struct{}
}

// acquire takes the slot in arrival order. waiting is called with the
// holder's application when the caller has to wait.
func (g *buildGate) acquire(ctx context.Context, app, deployment string, waiting func(holder string)) (func(), error) {
	t := &buildTicket{app: app, deployment: deployment, ready: make(chan struct{})}
	g.mu.Lock()
	if g.holder == nil {
		g.holder = t
		g.mu.Unlock()
		return g.releaser(t), nil
	}
	g.queue = append(g.queue, t)
	holder := g.holder.app
	g.mu.Unlock()
	if waiting != nil {
		waiting(holder)
	}
	select {
	case <-t.ready:
		return g.releaser(t), nil
	case <-ctx.Done():
		g.mu.Lock()
		if g.holder == t {
			// Granted while giving up: pass the slot on.
			g.mu.Unlock()
			g.releaser(t)()
		} else {
			g.queue = slices.DeleteFunc(g.queue, func(other *buildTicket) bool { return other == t })
			g.mu.Unlock()
		}
		return nil, ctx.Err()
	}
}

func (g *buildGate) releaser(t *buildTicket) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.holder != t {
				return
			}
			g.holder = nil
			if len(g.queue) > 0 {
				g.holder, g.queue = g.queue[0], g.queue[1:]
				close(g.holder.ready)
			}
		})
	}
}

// current returns the application and deployment holding the slot.
func (g *buildGate) current() (app, deployment string, held bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.holder == nil {
		return "", "", false
	}
	return g.holder.app, g.holder.deployment, true
}

func deploymentProgressKey(id string) string { return "deployment/" + id }

func deploymentFrom(ctx context.Context) string {
	id, _ := ctx.Value(deploymentIDKey{}).(string)
	return id
}

func buildWaitingPhase(app string) string {
	return "waiting for another build on this server (deploying " + app + ")"
}

// acquireBuildSlot waits for the host's build slot, reporting the wait in
// log and in the deployment's live progress.
func (m *Manager) acquireBuildSlot(ctx context.Context, app string, log io.Writer) (func(), error) {
	deployment := deploymentFrom(ctx)
	started := time.Now()
	var done func()
	release, err := m.build.gate.acquire(ctx, app, deployment, func(holder string) {
		phase := buildWaitingPhase(holder)
		buildLogf(log, "Waiting for another build on this server (deploying %s)", holder)
		if deployment != "" {
			_, done = m.progress.track(deploymentProgressKey(deployment), phase)
		}
	})
	if done != nil {
		done()
		if err == nil {
			buildLogf(log, "Build slot free after %s", time.Since(started).Round(time.Second))
		}
	}
	if err == nil {
		// A build writes layers, dependencies and a release tree: refuse it
		// rather than fill the disk the running applications share.
		if full := diskFits(m.cfg.DataDir, buildDiskHeadroom, "a build"); full != nil {
			release()
			return nil, full
		}
	}
	return release, err
}

func buildLogf(log io.Writer, format string, args ...any) {
	if log != nil {
		fmt.Fprintf(log, format+"\n", args...)
	}
}

// buildSlotKey carries a build-slot function to runtimes that do not hold
// the Manager (container image builds).
type buildSlotKey struct{}

// withBuildSlot makes slot available to builds started with ctx.
func withBuildSlot(ctx context.Context, slot func(context.Context) (func(), error)) context.Context {
	return context.WithValue(ctx, buildSlotKey{}, slot)
}

// holdBuildSlot takes the build slot carried by ctx; without one it is a no-op.
func holdBuildSlot(ctx context.Context) (func(), error) {
	if slot, ok := ctx.Value(buildSlotKey{}).(func(context.Context) (func(), error)); ok && slot != nil {
		return slot(ctx)
	}
	return func() {}, nil
}

// containerBuildSlot is the build slot of a container image build: the
// host's slot, then free memory for an image build. Image builds run in
// the Docker daemon's builder, which per-build memory limits do not reach,
// so they are only admitted when memory is free.
func (m *Manager) containerBuildSlot(app string) func(context.Context) (func(), error) {
	return func(ctx context.Context) (func(), error) {
		log := deploymentNoteWriter{ctx}
		release, err := m.acquireBuildSlot(ctx, app, log)
		if err != nil {
			return nil, err
		}
		if _, err := m.admitBuildStep(ctx, buildStep{App: app, Step: "image build", Build: true, Log: log}, buildmem.Request{LearnedPeakMiB: m.learnedBuildPeak(app, "image build")}); err != nil {
			release()
			return nil, err
		}
		return release, nil
	}
}

// deploymentNoteWriter writes lines into the deployment log of ctx.
type deploymentNoteWriter struct{ ctx context.Context }

func (w deploymentNoteWriter) Write(p []byte) (int, error) {
	if log, ok := w.ctx.Value(diagnosticKey{}).(func(string)); ok {
		log(string(p))
	}
	return len(p), nil
}

// learnedBuildPeak is the largest recent peak of an application's step.
func (m *Manager) learnedBuildPeak(app, step string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	peak, seen := 0, 0
	records := m.data.BuildPeaks[app]
	for i := len(records) - 1; i >= 0 && seen < buildPeakWindow; i-- {
		if records[i].Step == step {
			peak = max(peak, records[i].PeakMiB)
			seen++
		}
	}
	return peak
}

// buildPeakOf is the largest recent peak of any step of an application:
// what a build of it needs at its largest step.
func (m *Manager) buildPeakOf(app string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return buildPeakOfLocked(m.data.BuildPeaks[app])
}

func buildPeakOfLocked(records []BuildPeak) int {
	peak, seen := 0, map[string]int{}
	for i := len(records) - 1; i >= 0; i-- {
		if seen[records[i].Step] < buildPeakWindow {
			peak = max(peak, records[i].PeakMiB)
			seen[records[i].Step]++
		}
	}
	return peak
}

// recordBuildPeak keeps a step's measured peak, bounded per application.
func (m *Manager) recordBuildPeak(app string, peak BuildPeak) {
	if app == "" || peak.PeakMiB <= 0 {
		return
	}
	if peak.At.IsZero() {
		peak.At = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.BuildPeaks == nil {
		m.data.BuildPeaks = map[string][]BuildPeak{}
	}
	records := append(m.data.BuildPeaks[app], peak)
	if len(records) > buildPeaksPerApp {
		records = slices.Clone(records[len(records)-buildPeaksPerApp:])
	}
	m.data.BuildPeaks[app] = records
	// Peaks only tune later caps; a failed save leaves the build unaffected.
	_ = m.save()
}

// buildMemoryShortage is the error of a step that cannot get the memory it needs.
func buildMemoryShortage(s buildStep, d buildmem.Decision, h buildmem.Host, learned int) error {
	detail := ""
	if learned > 0 {
		detail = ", peak of earlier builds " + buildmem.Format(learned)
	}
	return fmt.Errorf("this server does not have enough free memory to build %s (needs ~%s for %s, available %s of %s RAM after keeping %s for running applications%s)",
		s.App, buildmem.Format(d.NeedMiB), s.Step, buildmem.Format(d.BudgetMiB), buildmem.Format(h.TotalMiB), buildmem.Format(buildmem.ReserveMiB(h.TotalMiB)), detail)
}

// admitBuildStep decides a step's limits. A build step whose need exceeds
// the free memory waits, up to buildMemoryWait, with heartbeat lines and a
// live-progress phase, then fails with the shortage.
func (m *Manager) admitBuildStep(ctx context.Context, s buildStep, req buildmem.Request) (buildLimits, error) {
	limits := buildLimits{CPUQuota: m.build.cpuQuota()}
	h := m.build.host()
	d := buildmem.Decide(h, req)
	if d.Ready || !s.Build {
		limits.MemoryMiB = d.LimitMiB
		return limits, nil
	}
	if d.Impossible {
		return limits, buildMemoryShortage(s, d, h, req.LearnedPeakMiB)
	}
	started := time.Now()
	wait, poll, beat := durationOr(m.build.wait, buildMemoryWait), durationOr(m.build.poll, 5*time.Second), durationOr(m.build.heartbeat, 30*time.Second)
	phase := func(d buildmem.Decision) string {
		return fmt.Sprintf("waiting for memory to build %s (%s needs ~%s, %s free)", s.App, s.Step, buildmem.Format(d.NeedMiB), buildmem.Format(d.BudgetMiB))
	}
	buildLogf(s.Log, "Waiting for memory: %s needs ~%s, %s is free (%s kept for running applications)", s.Step, buildmem.Format(d.NeedMiB), buildmem.Format(d.BudgetMiB), buildmem.Format(buildmem.ReserveMiB(h.TotalMiB)))
	var tracker *ProgressTracker
	if id := deploymentFrom(ctx); id != "" {
		var done func()
		tracker, done = m.progress.track(deploymentProgressKey(id), phase(d))
		defer done()
	}
	lastBeat := started
	for {
		select {
		case <-ctx.Done():
			return limits, ctx.Err()
		case <-time.After(poll):
		}
		h = m.build.host()
		d = buildmem.Decide(h, req)
		if d.Ready {
			buildLogf(s.Log, "Memory free after %s", time.Since(started).Round(time.Second))
			limits.MemoryMiB = d.LimitMiB
			return limits, nil
		}
		if d.Impossible || time.Since(started) >= wait {
			return limits, buildMemoryShortage(s, d, h, req.LearnedPeakMiB)
		}
		if time.Since(lastBeat) >= beat {
			lastBeat = time.Now()
			buildLogf(s.Log, "Still waiting for memory: %s free, %s needs ~%s (%s)", buildmem.Format(d.BudgetMiB), s.Step, buildmem.Format(d.NeedMiB), time.Since(started).Round(time.Second))
			tracker.Stage(phase(d), 0, nil)
		}
	}
}

// runBuildStep runs one command under build resources: admission, cap,
// peak recording and one retry with more memory after running out.
func (m *Manager) runBuildStep(ctx context.Context, s buildStep, run buildRun) error {
	req := buildmem.Request{ExplicitMiB: s.Explicit}
	if s.Build {
		req.LearnedPeakMiB = m.learnedBuildPeak(s.App, s.Step)
	}
	limits, err := m.admitBuildStep(ctx, s, req)
	if err != nil {
		return err
	}
	peak, err := run(ctx, limits)
	oom := errors.Is(err, errBuildOutOfMemory)
	if !s.Build {
		if oom {
			return fmt.Errorf("%s ran out of memory (limit %s)", s.Step, buildmem.Format(limits.MemoryMiB))
		}
		return err
	}
	m.noteBuildAttempt(s, limits, peak, oom, err)
	if !oom {
		return err
	}
	h := m.build.host()
	swap := h.SwapTotalMiB > 0
	if s.Explicit > 0 && limits.MemoryMiB >= s.Explicit && !swap {
		return fmt.Errorf("%s ran out of memory at the application's build memory setting (%s); raise the build memory and deploy again", s.Step, buildmem.Format(limits.MemoryMiB))
	}
	// The retry asks for twice the memory (never above an explicit
	// setting, which then adds only swap) and needs more than before.
	previous := limits.MemoryMiB
	req.RetryFromMiB = previous
	req.LearnedPeakMiB = max(req.LearnedPeakMiB, previous)
	retry, err := m.admitBuildStep(ctx, s, req)
	if err != nil {
		return fmt.Errorf("%s ran out of memory at %s; %w", s.Step, buildmem.Format(previous), err)
	}
	retry.Swap = swap
	note := ""
	if swap {
		note = " and swap"
	}
	buildLogf(s.Log, "%s ran out of memory at %s; retrying once with %s%s", s.Step, buildmem.Format(previous), buildmem.Format(retry.MemoryMiB), note)
	peak, err = run(ctx, retry)
	oom = errors.Is(err, errBuildOutOfMemory)
	m.noteBuildAttempt(s, retry, peak, oom, err)
	if oom {
		learned := max(req.LearnedPeakMiB, peak, retry.MemoryMiB)
		return fmt.Errorf("%s ran out of memory again at %s: this server does not have enough free memory to build %s (needs more than ~%s; this server has %s of RAM)",
			s.Step, buildmem.Format(retry.MemoryMiB), s.App, buildmem.Format(learned), buildmem.Format(h.TotalMiB))
	}
	return err
}

// noteBuildAttempt records and reports one attempt's peak.
func (m *Manager) noteBuildAttempt(s buildStep, limits buildLimits, peak int, oom bool, err error) {
	if oom {
		// The step needed at least its cap.
		peak = max(peak, limits.MemoryMiB)
	}
	m.recordBuildPeak(s.App, BuildPeak{Step: s.Step, PeakMiB: peak, LimitMiB: limits.MemoryMiB, OutOfMemory: oom})
	switch {
	case oom:
	case peak > 0 && err == nil:
		buildLogf(s.Log, "%s: peak %s of %s", s.Step, buildmem.Format(peak), buildmem.Format(limits.MemoryMiB))
	case err == nil:
		buildLogf(s.Log, "%s: memory limit %s", s.Step, buildmem.Format(limits.MemoryMiB))
	}
}

// buildStepName names a command for logs and learned peaks: its leading
// words up to the first option, at most three ("npm ci", "npm run build").
func buildStepName(args []string) string {
	words := []string{}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") || len(words) == 3 {
			break
		}
		words = append(words, arg)
	}
	return strings.Join(words, " ")
}
