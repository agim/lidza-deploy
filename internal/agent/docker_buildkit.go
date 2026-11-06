package agent

import (
	"context"
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
	if time.Since(buildModeChecked) > 10*time.Minute {
		buildModeCached, buildModeChecked = probeDockerBuild(ctx), time.Now()
	}
	return buildModeCached
}

// dockerBuildInvocation turns `build <flags...>` arguments into the
// command for this host, and the environment it needs.
func dockerBuildInvocation(ctx context.Context, args []string) ([]string, []string) {
	mode := currentDockerBuildMode(ctx)
	if len(args) == 0 || args[0] != "build" {
		return args, nil
	}
	if mode.Buildx {
		return append([]string{"buildx", "build", "--load"}, args[1:]...), nil
	}
	if mode.Env != "" {
		return args, []string{mode.Env}
	}
	return args, nil
}

// dockerBuildxInstalled reports whether the buildx plugin answers.
func dockerBuildxInstalled(ctx context.Context) bool {
	return currentDockerBuildMode(ctx).Buildx
}
