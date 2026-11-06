package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func withBuildMode(t *testing.T, probe func(context.Context) dockerBuildMode, ensure func(context.Context)) {
	t.Helper()
	previousProbe, previousEnsure := probeDockerBuild, ensureBuildx
	t.Cleanup(func() {
		probeDockerBuild, ensureBuildx = previousProbe, previousEnsure
		buildModeMu.Lock()
		buildModeChecked = time.Time{}
		buildModeMu.Unlock()
	})
	probeDockerBuild, ensureBuildx = probe, ensure
	buildModeMu.Lock()
	buildModeChecked = time.Time{}
	buildModeMu.Unlock()
}

// A host without buildx gets the plugin before the build, which then runs
// through BuildKit instead of the deprecated legacy builder.
func TestDockerBuildInstallsBuildxInsteadOfTheLegacyBuilder(t *testing.T) {
	installed := false
	withBuildMode(t, func(context.Context) dockerBuildMode {
		return dockerBuildMode{Buildx: installed}
	}, func(context.Context) { installed = true })
	args, env := dockerBuildInvocation(context.Background(), []string{"build", "--progress=plain", "-t", "app:1", "."})
	if !installed || !slices.Equal(args, []string{"buildx", "build", "--load", "--progress=plain", "-t", "app:1", "."}) || env != nil {
		t.Fatalf("args %q env %q installed %t", args, env, installed)
	}
}

// Where the plugin cannot be installed the legacy builder still builds:
// flags it rejects are dropped.
func TestDockerBuildLegacyFallbackDropsProgressFlags(t *testing.T) {
	probes := 0
	withBuildMode(t, func(context.Context) dockerBuildMode { probes++; return dockerBuildMode{} }, func(context.Context) {})
	args, _ := dockerBuildInvocation(context.Background(), []string{"build", "--progress", "plain", "--progress=rawjson", "-t", "app:1", "."})
	if !slices.Equal(args, []string{"build", "-t", "app:1", "."}) {
		t.Fatalf("args %q", args)
	}
	// The negative answer is not cached: the next build probes again.
	before := probes
	dockerBuildInvocation(context.Background(), []string{"build", "."})
	if probes <= before {
		t.Fatalf("a host without buildx must be probed again: %d → %d", before, probes)
	}
}

// The distribution's docker.io pairs with docker-buildx; Docker's own
// packages with docker-buildx-plugin.
func TestBuildxPackageMatchesTheInstalledDocker(t *testing.T) {
	previous := hostCommand
	t.Cleanup(func() { hostCommand = previous })
	status := "install ok installed"
	hostCommand = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "dpkg-query" && strings.Contains(strings.Join(args, " "), "docker.io") {
			return status, nil
		}
		return "", nil
	}
	if got := buildxPackage(context.Background()); got != "docker-buildx" {
		t.Fatalf("docker.io host: %s", got)
	}
	status = ""
	if got := buildxPackage(context.Background()); got != "docker-buildx-plugin" {
		t.Fatalf("docker-ce host: %s", got)
	}
}
