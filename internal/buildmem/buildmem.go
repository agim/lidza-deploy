// Package buildmem decides how much memory and CPU a build may use on a
// host, and estimates how much memory an application's build needs.
//
// It is pure: callers pass host snapshots (MemTotal/MemAvailable from
// /proc/meminfo) and learned peaks, so the same policy serves the agent that
// runs builds, the control panel that warns before creating or moving an
// application, and any host that builds for another one.
package buildmem

import (
	"fmt"
	"strings"
)

const (
	// FloorMiB is the smallest memory cap a build step ever gets.
	FloorMiB = 512
	// DefaultMinMiB and DefaultMaxMiB bound the cap of a step whose memory
	// use is not known yet: a quarter of the host's memory between them.
	DefaultMinMiB = 2048
	DefaultMaxMiB = 8192
	// LearnedHeadroom scales a learned peak into the next build's cap.
	LearnedHeadroom = 1.5
)

// ReserveMiB is the memory kept free for running applications while a
// build runs: 10% of the host's memory, at least 512 MiB.
func ReserveMiB(totalMiB int) int { return max(512, totalMiB/10) }

// DefaultMiB is the cap of a build step whose memory use is not known yet.
func DefaultMiB(totalMiB int) int { return min(max(totalMiB/4, DefaultMinMiB), DefaultMaxMiB) }

// Host is a snapshot of a host's memory, in MiB. A zero TotalMiB means the
// memory could not be read; the policy then applies caps without waiting.
type Host struct {
	TotalMiB     int `json:"total_mib"`
	AvailableMiB int `json:"available_mib"`
	SwapTotalMiB int `json:"swap_total_mib,omitempty"`
	SwapFreeMiB  int `json:"swap_free_mib,omitempty"`
}

// Known reports whether the snapshot holds real numbers.
func (h Host) Known() bool { return h.TotalMiB > 0 }

// BudgetMiB is the memory a build may take now without crowding the
// running applications: available memory minus the reserve.
func (h Host) BudgetMiB() int { return max(0, h.AvailableMiB-ReserveMiB(h.TotalMiB)) }

// CapacityMiB is the most a build could get on this host with every other
// application idle: total memory minus the reserve.
func (h Host) CapacityMiB() int { return max(0, h.TotalMiB-ReserveMiB(h.TotalMiB)) }

// HostFromBytes converts byte counts (as the agent's server health reports
// them) into a snapshot.
func HostFromBytes(total, available, swapTotal, swapFree uint64) Host {
	return Host{TotalMiB: int(total >> 20), AvailableMiB: int(available >> 20), SwapTotalMiB: int(swapTotal >> 20), SwapFreeMiB: int(swapFree >> 20)}
}

// Request is what one build step asks for.
type Request struct {
	// ExplicitMiB is the operator's setting; the cap never exceeds it.
	ExplicitMiB int
	// LearnedPeakMiB is the largest recent peak of this step; 0 is unknown.
	LearnedPeakMiB int
	// RetryFromMiB is the cap of an attempt that ran out of memory: the
	// retry asks for twice as much and needs clearly more than before.
	RetryFromMiB int
}

// Decision is the cap for one attempt of a step.
type Decision struct {
	// LimitMiB is the memory cap to apply.
	LimitMiB int `json:"limit_mib"`
	// WantMiB is the cap the step would get with enough free memory.
	WantMiB int `json:"want_mib"`
	// NeedMiB is what the step is known to need; below it the step waits.
	NeedMiB int `json:"need_mib"`
	// BudgetMiB is the free memory minus the reserve when decided.
	BudgetMiB int `json:"budget_mib"`
	// Ready is false while the host lacks NeedMiB of free memory.
	Ready bool `json:"ready"`
	// Impossible is set when even an idle host could not give NeedMiB:
	// waiting cannot help.
	Impossible bool `json:"impossible,omitempty"`
}

// Decide computes the cap of one build step on host h.
//
// The cap is the operator's explicit setting when there is one, otherwise
// 1.5 × the step's learned peak, otherwise the default for the host's size;
// a retry after running out of memory asks for twice the previous cap. The
// cap is then bounded by the free memory (available minus the reserve) and
// never below FloorMiB. The step must wait while the free memory is below
// what it is known to need (its learned peak, more than the failed cap on a
// retry, and at least FloorMiB).
func Decide(h Host, r Request) Decision {
	want := DefaultMiB(h.TotalMiB)
	if r.LearnedPeakMiB > 0 {
		want = max(FloorMiB, int(float64(r.LearnedPeakMiB)*LearnedHeadroom+0.5))
	}
	need := max(FloorMiB, r.LearnedPeakMiB)
	if r.RetryFromMiB > 0 {
		want = max(want, 2*r.RetryFromMiB)
		need = max(need, r.RetryFromMiB+max(256, r.RetryFromMiB/4))
	}
	if r.ExplicitMiB > 0 {
		want = r.ExplicitMiB
	}
	need = min(need, want)
	d := Decision{WantMiB: want, NeedMiB: need, LimitMiB: want, Ready: true}
	if !h.Known() {
		return d
	}
	d.BudgetMiB = h.BudgetMiB()
	d.LimitMiB = max(FloorMiB, min(want, d.BudgetMiB))
	d.Ready = d.BudgetMiB >= need
	d.Impossible = h.CapacityMiB() < need
	return d
}

// CPUQuotaPercent is the CPU quota of a build step: every core but one, so
// one core always stays for running applications, and at least one whole
// core. Builds run one at a time on a host and get low CPU and I/O weights,
// so running applications still win under contention.
func CPUQuotaPercent(cpus int) int { return max(100, (cpus-1)*100) }

// Format shows MiB as "512 MiB" or "1.5 GiB".
func Format(mib int) string {
	if mib < 1024 {
		return fmt.Sprintf("%d MiB", max(0, mib))
	}
	return fmt.Sprintf("%.1f GiB", float64(mib)/1024)
}

// Stack describes what a build compiles, as detected from the repository.
type Stack struct {
	Mode      string // "native" or "container"
	Runtime   string // go, rails, node, static
	Framework string // lidza, go, rails, astro, next, …
	// Frontend: a JavaScript front end is bundled (package.json with a
	// build, or a Līdza application with a package.json).
	Frontend bool
	// Rust: Rust code is compiled (Cargo.toml, Līdza Rust packs).
	Rust bool
	// Assets: Rails assets are precompiled.
	Assets bool
}

// Estimate is the memory a build is expected to need at its largest step.
type Estimate struct {
	MiB int `json:"mib"`
	// Basis explains the estimate in plain language.
	Basis []string `json:"basis"`
	// Learned is set when the estimate comes from measured builds.
	Learned bool `json:"learned,omitempty"`
}

// Typical memory peaks of the largest build step of each stack, from
// measured builds. Steps run one after another, so they do not add up,
// except inside one step (a Līdza build bundles its front end, compiles
// its Rust packs and links the Go binary).
const (
	goMiB          = 1024
	lidzaMiB       = 1536
	lidzaFrontMiB  = 2560
	rustMiB        = 1024
	railsMiB       = 1024
	railsAssetsMiB = 1536
	railsFrontMiB  = 2048
	nodeMiB        = 1024
	nodeBuildMiB   = 2048
	staticMiB      = FloorMiB
	containerMiB   = 2048
)

// EstimateStack estimates the build memory of a detected stack.
func EstimateStack(s Stack) Estimate {
	add := func(e *Estimate, mib int, basis string) {
		e.MiB += mib
		e.Basis = append(e.Basis, fmt.Sprintf("%s ~%s", basis, Format(mib)))
	}
	var e Estimate
	switch {
	case s.Runtime == "go" && s.Framework == "lidza" && s.Frontend:
		add(&e, lidzaFrontMiB, "Līdza build with a front end")
	case s.Runtime == "go" && s.Framework == "lidza":
		add(&e, lidzaMiB, "Līdza build")
	case s.Runtime == "go":
		add(&e, goMiB, "Go build")
	case s.Runtime == "rails" && s.Frontend:
		add(&e, railsFrontMiB, "Rails assets with a JavaScript bundler")
	case s.Runtime == "rails" && s.Assets:
		add(&e, railsAssetsMiB, "Rails asset precompilation")
	case s.Runtime == "rails":
		add(&e, railsMiB, "Rails bundle install")
	case (s.Runtime == "node" || s.Runtime == "static") && s.Frontend:
		add(&e, nodeBuildMiB, "JavaScript build ("+frameworkName(s.Framework)+")")
	case s.Runtime == "node":
		add(&e, nodeMiB, "Node.js dependencies")
	case s.Runtime == "static":
		add(&e, staticMiB, "Static files")
	default:
		add(&e, containerMiB, "Container image build")
	}
	if s.Rust {
		if s.Runtime == "go" && s.Framework == "lidza" {
			add(&e, rustMiB, "+ Rust packs")
		} else {
			add(&e, rustMiB, "+ Rust code")
		}
	}
	return e
}

func frameworkName(framework string) string {
	if framework == "" || framework == "node" || framework == "html" {
		return "Node.js"
	}
	return framework
}

// Refine replaces an estimate with a measured peak when one is known.
func (e Estimate) Refine(learnedPeakMiB int, where string) Estimate {
	if learnedPeakMiB <= 0 {
		return e
	}
	basis := "peak of earlier builds"
	if where != "" {
		basis += " on " + where
	}
	return Estimate{MiB: max(FloorMiB, learnedPeakMiB), Basis: []string{fmt.Sprintf("%s: %s", basis, Format(learnedPeakMiB))}, Learned: true}
}

// Assessment levels.
const (
	LevelOK      = "ok"
	LevelWarning = "warning"
	LevelBlocker = "blocker"
)

// Assessment compares a build estimate with a server's memory.
type Assessment struct {
	Estimate     Estimate `json:"estimate"`
	TotalMiB     int      `json:"total_mib,omitempty"`
	AvailableMiB int      `json:"available_mib,omitempty"`
	ReserveMiB   int      `json:"reserve_mib,omitempty"`
	// Level is ok, warning (the server is likely too small: builds wait
	// for memory and may fail) or blocker (it certainly is: the estimate
	// exceeds its memory minus the reserve). Unknown host memory is ok.
	Level   string `json:"level"`
	Message string `json:"message,omitempty"`
}

// Assess compares estimate e with host h.
func Assess(e Estimate, h Host, server string) Assessment {
	a := Assessment{Estimate: e, Level: LevelOK}
	if !h.Known() || e.MiB <= 0 {
		return a
	}
	if server == "" {
		server = "this server"
	}
	a.TotalMiB, a.AvailableMiB, a.ReserveMiB = h.TotalMiB, h.AvailableMiB, ReserveMiB(h.TotalMiB)
	need := Format(e.MiB)
	switch {
	case e.MiB > h.CapacityMiB():
		a.Level = LevelBlocker
		a.Message = fmt.Sprintf("Building needs about %s of memory, but %s has %s in total and keeps %s free for running applications. Choose a server with more memory.",
			need, server, Format(h.TotalMiB), Format(a.ReserveMiB))
	case e.MiB > h.BudgetMiB():
		a.Level = LevelWarning
		a.Message = fmt.Sprintf("Building needs about %s of memory; %s has %s free now (%s total, %s kept for running applications). Builds wait for memory to free up and may fail.",
			need, server, Format(h.AvailableMiB), Format(h.TotalMiB), Format(a.ReserveMiB))
	default:
		a.Message = fmt.Sprintf("Building needs about %s of memory; %s has %s free.", need, server, Format(h.AvailableMiB))
	}
	return a
}

// Summary is a one-line description of an estimate.
func (e Estimate) Summary() string {
	if len(e.Basis) == 0 {
		return Format(e.MiB)
	}
	return Format(e.MiB) + " (" + strings.Join(e.Basis, ", ") + ")"
}
