package control

import (
	"context"
	"encoding/json"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

// effectiveSourceEnvironment is a container's effective environment as the
// agent exports it: panel settings, manifest deploy defaults and image ENV.
func effectiveSourceEnvironment() agent.MoveEnvironment {
	return agent.MoveEnvironment{AppID: "shop",
		Values: map[string]string{"APP_SECRET": moveTestSecret, "PUBLIC_NAME": "Shop", "DATABASE_URL": "postgres://app:source-db-password@lidza-db-shop:5432/app",
			"DB_POOL": "20", "APP_URL": "https://shop.example.com", "LIDZA_MODE": "production", "TZ": "UTC"},
		Kinds: map[string]string{"APP_SECRET": "credential", "PUBLIC_NAME": "setting", "DATABASE_URL": "credential", "DB_POOL": "setting", "APP_URL": "setting", "LIDZA_MODE": "setting", "TZ": "setting"},
		Sources: map[string]string{"APP_SECRET": agent.EnvironmentSourcePanel, "PUBLIC_NAME": agent.EnvironmentSourcePanel, "DATABASE_URL": agent.EnvironmentSourcePanel,
			"DB_POOL": agent.EnvironmentSourceDefaults, "APP_URL": agent.EnvironmentSourceDefaults, "LIDZA_MODE": agent.EnvironmentSourceImage, "TZ": agent.EnvironmentSourceImage}}
}

func TestMoveEnvironmentChangesCountSourcesAndKeepTheFrameworkMode(t *testing.T) {
	source := effectiveSourceEnvironment()
	job := MoveJob{TargetRuntime: moveRuntimeNative, Databases: []MoveDatabase{{SourceID: "shop", Copyable: true, EnvKeys: []string{"DATABASE_URL"}}}}
	changes, _, report := moveEnvironmentChanges(job, source, &agent.MoveEnvironment{Values: map[string]string{"DATABASE_URL": "postgres://destination"}})
	// The native host sets the framework mode itself (and refuses it as a
	// setting); the other deploy defaults and image variables are copied.
	if changes["LIDZA_MODE"] != nil || changes["TZ"] == nil || changes["DB_POOL"] == nil {
		t.Fatalf("a native destination needs the deploy defaults and image variables except the framework mode: %v", report)
	}
	if want := map[string]int{agent.EnvironmentSourcePanel: 2, agent.EnvironmentSourceDefaults: 2, agent.EnvironmentSourceImage: 1}; !maps.Equal(report.sources, want) {
		t.Fatalf("sources %v", report.sources)
	}
	if got := moveSourceSummary(report.sources); got != " (2 from settings, 2 from deploy defaults, 1 from image)" {
		t.Fatalf("summary %q", got)
	}
	// Agent-owned LIDZA_ keys stay with the destination.
	source.Values["LIDZA_ADDR"], source.Values["LIDZA_GATEWAY"] = "0.0.0.0:3000", "x"
	changes, _, _ = moveEnvironmentChanges(job, source, nil)
	if changes["LIDZA_ADDR"] != nil || changes["LIDZA_GATEWAY"] != nil {
		t.Fatal("agent-owned LIDZA_ keys copied")
	}
	// A container destination builds the same manifest and image: their
	// variables are not pinned as settings.
	job.TargetRuntime = moveRuntimeContainer
	changes, _, report = moveEnvironmentChanges(job, source, nil)
	if changes["LIDZA_MODE"] != nil || changes["DB_POOL"] != nil || changes["APP_SECRET"] == nil {
		t.Fatalf("container destination: %+v", report)
	}
	if !slices.Equal(report.rebuilt, []string{"APP_URL", "DB_POOL", "LIDZA_MODE", "TZ"}) || moveSourceSummary(report.sources) != "" {
		t.Fatalf("rebuilt %v sources %v", report.rebuilt, report.sources)
	}
}

func TestMoveCopiesImageVariablesToANativeDestination(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.a.env = effectiveSourceEnvironment()
	fx.b.targetEnv = agent.MoveEnvironment{AppID: "shop", Native: true, Values: map[string]string{"DATABASE_URL": "postgres://app_shop:destination@127.0.0.1:5432/app_shop", "PORT": "4101"}}
	job := MoveJob{ID: "m1", AppID: "shop", SourceServer: "alpha", TargetServer: "beta", SourceRuntime: moveRuntimeContainer, TargetRuntime: moveRuntimeNative,
		Databases: []MoveDatabase{{SourceID: "shop", Copyable: true, EnvKeys: []string{"DATABASE_URL"}}}, Steps: []MoveStep{{Key: moveStepEnvironment, Title: "Copy environment"}}}
	fx.c.mu.Lock()
	if fx.c.data.Moves == nil {
		fx.c.data.Moves = map[string]MoveJob{}
	}
	fx.c.data.Moves[job.ID] = job
	fx.c.mu.Unlock()
	if err := fx.c.moveCopyEnvironment(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	fx.b.with(func() {
		if len(fx.b.patches) != 1 {
			t.Fatalf("patches %+v", fx.b.patches)
		}
		patch := fx.b.patches[0]
		if patch.EnvChanges["LIDZA_MODE"] != nil || patch.EnvChanges["TZ"] == nil || *patch.EnvChanges["DB_POOL"] != "20" {
			t.Fatalf("the destination lacks image and deploy-default variables (and must not receive the host-set framework mode): %v", slices.Sorted(maps.Keys(patch.EnvChanges)))
		}
		if value := patch.EnvChanges["DATABASE_URL"]; value != nil && strings.Contains(*value, "source-db-password") {
			t.Fatal("the source's database connection was copied")
		}
	})
	stored, _ := fx.c.moveJob(job.ID)
	lines := []string{}
	for _, line := range stored.step(moveStepEnvironment).Log {
		lines = append(lines, line.Message)
	}
	log := strings.Join(lines, "\n")
	if !strings.Contains(log, "Copied 5 variables (2 from settings, 2 from deploy defaults, 1 from image)") {
		t.Fatalf("log:\n%s", log)
	}
	if strings.Contains(log, moveTestSecret) || strings.Contains(log, "production") {
		t.Fatalf("the log carries values:\n%s", log)
	}
}

func TestMovePlanCountsEnvironmentSources(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.a.source.EnvironmentKeys = []string{"APP_SECRET", "DATABASE_URL", "DB_POOL", "LIDZA_MODE", "PUBLIC_NAME"}
	fx.a.source.EnvironmentSources = map[string]string{"APP_SECRET": agent.EnvironmentSourcePanel, "PUBLIC_NAME": agent.EnvironmentSourcePanel, "DATABASE_URL": agent.EnvironmentSourcePanel,
		"DB_POOL": agent.EnvironmentSourceDefaults, "LIDZA_MODE": agent.EnvironmentSourceImage}
	plan, status, err := fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta"})
	if err != nil || status != 200 {
		t.Fatal(status, err)
	}
	// Without repository detection the destination keeps containers: the
	// image and manifest variables come from its own build.
	if plan.TargetRuntime != moveRuntimeContainer || !slices.Equal(plan.CopiedKeys, []string{"APP_SECRET", "PUBLIC_NAME"}) || plan.CopiedSources != nil {
		t.Fatalf("plan copied %v sources %v", plan.CopiedKeys, plan.CopiedSources)
	}
	if !slices.Contains(plan.DestinationKeys, "LIDZA_MODE") || !slices.Contains(plan.DestinationKeys, "DB_POOL") {
		t.Fatalf("destination keys %v", plan.DestinationKeys)
	}
	raw, _ := json.Marshal(plan)
	if strings.Contains(string(raw), "copied_sources") {
		t.Fatalf("settings-only plans omit the breakdown: %s", raw)
	}
}
