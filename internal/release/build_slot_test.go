package release

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingExecutor notes which commands ran inside the build slot.
type recordingExecutor struct {
	held  *bool
	steps []string
}

func (x *recordingExecutor) Run(ctx context.Context, c Command) error {
	if c.Build != *x.held {
		return errors.New(c.Name + ": build flag does not match the build slot")
	}
	if c.Build {
		x.steps = append(x.steps, c.Name)
	}
	return LocalExecutor{}.Run(ctx, c)
}

func TestDeployHoldsTheBuildSlotAroundBuildStepsOnly(t *testing.T) {
	url, commit := gitFixture(t)
	commit("db/migrate/1.rb", "")
	commit("VERSION", "one\n")
	p, _, _, log := testPlan(t, url)
	held, acquired, released := false, 0, 0
	x := &recordingExecutor{held: &held}
	p.Exec = x
	p.SkipBackup = true
	p.BuildSlot = func(context.Context) (func(), error) {
		acquired++
		held = true
		return func() { held = false; released++ }, nil
	}
	if _, err := Deploy(context.Background(), p); err != nil {
		t.Fatal(err, log.String())
	}
	if acquired != 1 || released != 1 || held {
		t.Fatalf("slot acquired %d, released %d", acquired, released)
	}
	if strings.Join(x.steps, ",") != "build" {
		t.Fatalf("build steps %v", x.steps)
	}
	// A slot that cannot be had fails the deploy before any build step.
	p, _, _, _ = testPlan(t, url)
	p.BuildSlot = func(context.Context) (func(), error) { return nil, context.Canceled }
	if _, err := Deploy(context.Background(), p); !errors.Is(err, context.Canceled) {
		t.Fatal("slot error not returned", err)
	}
}
