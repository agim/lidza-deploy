package control

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/buildmem"
)

func TestMovePlanComparesBuildMemoryWithTheDestination(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	req := httptest.NewRequest("GET", "/", nil)
	plan, _, err := fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if err != nil || plan.BuildMemory == nil {
		t.Fatal(err, plan.BuildMemory)
	}
	// 4 GiB with 2 GiB free easily builds a Go application.
	if plan.BuildMemory.Level != buildmem.LevelOK || plan.BuildMemory.Estimate.MiB != 1024 || plan.BuildMemory.TotalMiB != 4096 {
		t.Fatalf("Go build on 4 GiB: %+v", plan.BuildMemory)
	}
	// A peak measured on the source replaces the estimate: above the
	// destination's typical free memory is a warning.
	fx.a.source.BuildPeakMiB = 2600
	plan, _, _ = fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if plan.BuildMemory.Level != buildmem.LevelWarning || !plan.BuildMemory.Estimate.Learned || !strings.Contains(plan.BuildMemory.Estimate.Basis[0], "Alpha") {
		t.Fatalf("learned peak: %+v", plan.BuildMemory)
	}
	if !slices.ContainsFunc(plan.Warnings, func(w string) bool {
		return strings.Contains(w, "Building needs about 2.5 GiB") && strings.Contains(w, "Beta")
	}) {
		t.Fatalf("warnings %v", plan.Warnings)
	}
	// More than the destination's memory minus the reserve blocks the move.
	fx.b.memory = []uint64{2 << 30, 1 << 30}
	plan, _, _ = fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if plan.BuildMemory.Level != buildmem.LevelBlocker || !slices.ContainsFunc(plan.Blockers, func(b string) bool { return strings.Contains(b, "Choose a server with more memory") }) {
		t.Fatalf("blockers %v (%+v)", plan.Blockers, plan.BuildMemory)
	}
}

func TestMoveBuildEstimateFallsBackToTheRuntime(t *testing.T) {
	for runtime, want := range map[string]int{"rails": 1536, "astro": 2048, "node": 2048, "go": 1024, "static": 512, "": 2048} {
		if got := moveBuildEstimate(agent.MoveSource{Runtime: runtime}, nil).MiB; got != want {
			t.Errorf("%q: %d MiB, want %d", runtime, got, want)
		}
	}
	detected := &RepositoryDetection{}
	detected.BuildMemory = &buildmem.Estimate{MiB: 3584}
	if moveBuildEstimate(agent.MoveSource{Runtime: "go"}, detected).MiB != 3584 {
		t.Fatal("detection wins over the runtime")
	}
}
