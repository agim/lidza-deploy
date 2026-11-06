package agent

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Image builds use BuildKit. With the buildx plugin, builds run through
// `docker buildx build --load` on Docker's default builder; a Docker client
// older than 23 without the plugin has BuildKit built in (DOCKER_BUILDKIT=1).
// Only a newer client without the plugin falls back to the legacy builder,
// and then the agent's own Docker install adds the plugin
// (docker-buildx-plugin from Docker's repository).

// dockerBuildMode is how this host builds images.
type dockerBuildMode struct {
	Buildx bool   // docker buildx is available
	Env    string // extra environment for docker build, e.g. DOCKER_BUILDKIT=1
}

var (
	buildModeMu      sync.Mutex
	buildModeCached  dockerBuildMode
	buildModeChecked time.Time
)

// probeDockerBuild reads the docker client; tests replace it.
var probeDockerBuild = func(ctx context.Context) dockerBuildMode {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := command(bounded, "", nil, "docker", "buildx", "version"); err == nil {
		return dockerBuildMode{Buildx: true}
	}
	version, err := command(bounded, "", nil, "docker", "version", "--format", "{{.Client.Version}}")
	if err == nil {
		if major, err := strconv.Atoi(strings.SplitN(strings.TrimSpace(version), ".", 2)[0]); err == nil && major < 23 {
			return dockerBuildMode{Env: "DOCKER_BUILDKIT=1"}
		}
	}
	return dockerBuildMode{}
}

// currentDockerBuildMode caches the probe for ten minutes, so a plugin
// installed meanwhile is picked up without restarting the agent.
func currentDockerBuildMode(ctx context.Context) dockerBuildMode {
	buildModeMu.Lock()
	defer buildModeMu.Unlock()
	// Only a BuildKit-capable answer is cached: a host without buildx is
	// probed again on every build, so a plugin installed meanwhile (by the
	// agent or an operator) is used at once.
	if !buildModeCapable(buildModeCached) || time.Since(buildModeChecked) > 10*time.Minute {
		buildModeCached, buildModeChecked = probeDockerBuild(ctx), time.Now()
	}
	return buildModeCached
}

func buildModeCapable(mode dockerBuildMode) bool { return mode.Buildx || mode.Env != "" }

// ensureBuildx installs the buildx plugin when this host's Docker lacks it
// and the agent may install packages; builds then use BuildKit instead of
// the deprecated legacy builder. Tests replace it.
var ensureBuildx = func(ctx context.Context) {
	if os.Geteuid() != 0 {
		return
	}
	if ok, _ := aptHost(); !ok {
		return
	}
	buildxInstallMu.Lock()
	defer buildxInstallMu.Unlock()
	if _, err := command(ctx, "", nil, "docker", "buildx", "version"); err == nil {
		return
	}
	commandDiagnostic(ctx, "deployment", "Docker on this server has no buildx plugin; installing it so images build with BuildKit.")
	out, err := installBuildx(ctx)
	if err != nil {
		commandDiagnostic(ctx, "deployment", "Installing the buildx plugin failed ("+lastLines(out)+"); building with the legacy builder.")
		return
	}
	commandDiagnostic(ctx, "deployment", "Installed the Docker buildx plugin.")
}

var buildxInstallMu sync.Mutex

// legacyBuildArgs drops the flags the legacy builder rejects (--progress).
func legacyBuildArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--progress" && i+1 < len(args):
			i++
		case strings.HasPrefix(args[i], "--progress="):
		default:
			out = append(out, args[i])
		}
	}
	return out
}

// dockerBuildInvocation turns `build <flags...>` arguments into the
// command for this host, and the environment it needs.
func dockerBuildInvocation(ctx context.Context, args []string) ([]string, []string) {
	mode := currentDockerBuildMode(ctx)
	if len(args) == 0 || args[0] != "build" {
		return args, nil
	}
	if !buildModeCapable(mode) {
		ensureBuildx(ctx)
		buildModeMu.Lock()
		buildModeChecked = time.Time{}
		buildModeMu.Unlock()
		mode = currentDockerBuildMode(ctx)
	}
	if mode.Buildx {
		return append([]string{"buildx", "build", "--load"}, args[1:]...), nil
	}
	if mode.Env != "" {
		return args, []string{mode.Env}
	}
	return legacyBuildArgs(args), nil
}

// dockerBuildxInstalled reports whether the buildx plugin answers.
func dockerBuildxInstalled(ctx context.Context) bool {
	return currentDockerBuildMode(ctx).Buildx
}
