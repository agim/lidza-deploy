package buildmem

import (
	"strings"
	"testing"
)

func TestReserveAndDefaultScaleWithTheHost(t *testing.T) {
	for total, want := range map[int]int{0: 512, 2048: 512, 8192: 819, 65536: 6553} {
		if got := ReserveMiB(total); got != want {
			t.Errorf("reserve of %d MiB = %d, want %d", total, got, want)
		}
	}
	for total, want := range map[int]int{0: 2048, 4096: 2048, 12288: 3072, 32768: 8192, 131072: 8192} {
		if got := DefaultMiB(total); got != want {
			t.Errorf("default of %d MiB = %d, want %d", total, got, want)
		}
	}
}

func TestDecideBoundsTheCapByFreeMemory(t *testing.T) {
	// 16 GiB host with 12 GiB available: reserve 1638, budget 10650.
	roomy := Host{TotalMiB: 16384, AvailableMiB: 12288}
	if d := Decide(roomy, Request{}); !d.Ready || d.LimitMiB != 4096 || d.NeedMiB != FloorMiB {
		t.Fatalf("unknown step on a roomy host: %+v", d)
	}
	// Only 2 GiB available: the default 4 GiB shrinks to the free budget.
	tight := Host{TotalMiB: 16384, AvailableMiB: 2048}
	if d := Decide(Host{TotalMiB: 16384, AvailableMiB: 3638}, Request{}); !d.Ready || d.LimitMiB != 2000 {
		t.Fatalf("budget bounds the default: %+v", d)
	}
	if d := Decide(tight, Request{}); d.LimitMiB != FloorMiB || d.Ready {
		// 410 MiB of budget is below the floor: the step waits.
		t.Fatalf("budget below the floor must wait at the floor: %+v", d)
	}
	// Unknown host memory: the cap applies without waiting.
	if d := Decide(Host{}, Request{ExplicitMiB: 3000}); !d.Ready || d.LimitMiB != 3000 {
		t.Fatalf("unknown host: %+v", d)
	}
}

func TestDecideHonoursTheExplicitSetting(t *testing.T) {
	roomy := Host{TotalMiB: 32768, AvailableMiB: 30000}
	if d := Decide(roomy, Request{ExplicitMiB: 1024, LearnedPeakMiB: 3000}); d.LimitMiB != 1024 || d.NeedMiB != 1024 || !d.Ready {
		t.Fatalf("explicit setting is the ceiling: %+v", d)
	}
	if d := Decide(roomy, Request{ExplicitMiB: 1024, RetryFromMiB: 1024}); d.LimitMiB != 1024 {
		t.Fatalf("a retry never exceeds the explicit setting: %+v", d)
	}
	small := Host{TotalMiB: 4096, AvailableMiB: 1500}
	if d := Decide(small, Request{ExplicitMiB: 4096}); d.LimitMiB != 1500-512 || !d.Ready {
		t.Fatalf("explicit setting is still bounded by free memory: %+v", d)
	}
}

func TestDecideLearnsFromPeaks(t *testing.T) {
	roomy := Host{TotalMiB: 16384, AvailableMiB: 14000}
	if d := Decide(roomy, Request{LearnedPeakMiB: 1400}); d.LimitMiB != 2100 || d.NeedMiB != 1400 || !d.Ready {
		t.Fatalf("1.5 × learned peak: %+v", d)
	}
	if d := Decide(roomy, Request{LearnedPeakMiB: 100}); d.LimitMiB != FloorMiB || d.NeedMiB != FloorMiB {
		t.Fatalf("small peaks keep the floor: %+v", d)
	}
	// Less free memory than the step is known to need: wait.
	busy := Host{TotalMiB: 16384, AvailableMiB: 2638}
	d := Decide(busy, Request{LearnedPeakMiB: 1400})
	if d.Ready || d.Impossible || d.BudgetMiB != 1000 {
		t.Fatalf("busy host must wait: %+v", d)
	}
	// More than the whole host can ever give: waiting cannot help.
	if d := Decide(Host{TotalMiB: 2048, AvailableMiB: 1800}, Request{LearnedPeakMiB: 1800}); d.Ready || !d.Impossible {
		t.Fatalf("impossible build: %+v", d)
	}
}

func TestDecideRetriesWithTwiceTheMemory(t *testing.T) {
	roomy := Host{TotalMiB: 16384, AvailableMiB: 14000}
	if d := Decide(roomy, Request{RetryFromMiB: 2048}); d.LimitMiB != 4096 || d.NeedMiB != 2560 || !d.Ready {
		t.Fatalf("retry doubles: %+v", d)
	}
	// Room for more than before, but not double: the retry takes what is free.
	partial := Host{TotalMiB: 16384, AvailableMiB: 1638 + 3000}
	if d := Decide(partial, Request{RetryFromMiB: 2048}); d.LimitMiB != 3000 || !d.Ready {
		t.Fatalf("partial retry: %+v", d)
	}
	// No more room than the failed attempt had: the retry waits.
	none := Host{TotalMiB: 16384, AvailableMiB: 1638 + 2100}
	if d := Decide(none, Request{RetryFromMiB: 2048}); d.Ready {
		t.Fatalf("retry without more memory must wait: %+v", d)
	}
}

func TestCPUQuotaLeavesOneCore(t *testing.T) {
	for cpus, want := range map[int]int{0: 100, 1: 100, 2: 100, 4: 300, 8: 700} {
		if got := CPUQuotaPercent(cpus); got != want {
			t.Errorf("%d CPUs: quota %d%%, want %d%%", cpus, got, want)
		}
	}
}

func TestEstimateStacks(t *testing.T) {
	cases := []struct {
		stack Stack
		mib   int
		basis string
	}{
		{Stack{Runtime: "go", Framework: "go"}, 1024, "Go build"},
		{Stack{Runtime: "go", Framework: "lidza"}, 1536, "Līdza build"},
		{Stack{Runtime: "go", Framework: "lidza", Frontend: true}, 2560, "front end"},
		{Stack{Runtime: "go", Framework: "lidza", Frontend: true, Rust: true}, 3584, "Rust packs"},
		{Stack{Runtime: "rails", Framework: "rails", Assets: true}, 1536, "asset precompilation"},
		{Stack{Runtime: "rails", Framework: "rails", Assets: true, Frontend: true}, 2048, "bundler"},
		{Stack{Runtime: "node", Framework: "next", Frontend: true}, 2048, "next"},
		{Stack{Runtime: "static", Framework: "astro", Frontend: true}, 2048, "astro"},
		{Stack{Runtime: "static", Framework: "html"}, 512, "Static"},
		{Stack{Mode: "container"}, 2048, "Container"},
	}
	for _, c := range cases {
		e := EstimateStack(c.stack)
		if e.MiB != c.mib || !strings.Contains(strings.Join(e.Basis, " "), c.basis) {
			t.Errorf("%+v: %+v, want %d MiB mentioning %q", c.stack, e, c.mib, c.basis)
		}
	}
	refined := EstimateStack(Stack{Runtime: "go", Framework: "lidza"}).Refine(2900, "alpha")
	if !refined.Learned || refined.MiB != 2900 || !strings.Contains(refined.Basis[0], "alpha") {
		t.Fatalf("refined %+v", refined)
	}
	if e := EstimateStack(Stack{Runtime: "go"}); e.Refine(0, "").MiB != e.MiB {
		t.Fatal("no learned peak keeps the estimate")
	}
}

func TestAssessWarnsAndBlocks(t *testing.T) {
	e := Estimate{MiB: 2560, Basis: []string{"Līdza build with a front end ~2.5 GiB"}}
	if a := Assess(e, Host{TotalMiB: 2048, AvailableMiB: 1500}, "small"); a.Level != LevelBlocker || !strings.Contains(a.Message, "small has 2.0 GiB in total") {
		t.Fatalf("2 GiB host: %+v", a)
	}
	if a := Assess(e, Host{TotalMiB: 4096, AvailableMiB: 2000}, ""); a.Level != LevelWarning || !strings.Contains(a.Message, "this server has 2.0 GiB free") {
		t.Fatalf("busy 4 GiB host: %+v", a)
	}
	if a := Assess(e, Host{TotalMiB: 16384, AvailableMiB: 12000}, "big"); a.Level != LevelOK || a.ReserveMiB != 1638 {
		t.Fatalf("roomy host: %+v", a)
	}
	if a := Assess(e, Host{}, "unknown"); a.Level != LevelOK || a.Message != "" {
		t.Fatalf("unknown host: %+v", a)
	}
	if Format(512) != "512 MiB" || Format(1536) != "1.5 GiB" {
		t.Fatal("format", Format(512), Format(1536))
	}
}
