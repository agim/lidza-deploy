package agent

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"
)

// The environment a container application runs with is wider than what the
// panel stores: the deploy merges the manifest's deploy.env defaults, the
// agent adds runtime variables, and the image's own ENV applies underneath.
// A move exports the effective environment, so the destination (a native
// host in particular, which has no image) starts with the same values, and
// names where each variable came from so the plan can say "14 from settings,
// 6 from deploy defaults, 2 from image". Values are never logged.

const (
	EnvironmentSourcePanel    = "panel"
	EnvironmentSourceDefaults = "deploy defaults"
	EnvironmentSourceImage    = "image"
)

// moveInspectEnvironment reads Config.Env of a container or an image;
// replaced in tests. kind is "container" or "image".
var moveInspectEnvironment = func(ctx context.Context, kind, name string) (map[string]string, error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{"inspect", "--format", "{{json .Config.Env}}", name}
	if kind == "image" {
		args = append([]string{"image"}, args...)
	}
	out, err := command(bounded, "", nil, "docker", args...)
	if err != nil {
		return nil, err
	}
	var entries []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &entries); err != nil {
		return nil, err
	}
	return parseEnvironmentEntries(entries), nil
}

func parseEnvironmentEntries(entries []string) map[string]string {
	values := map[string]string{}
	for _, entry := range entries {
		key, value, _ := strings.Cut(entry, "=")
		if envPattern.MatchString(key) {
			values[key] = value
		}
	}
	return values
}

// moveRuntimeOwnedKeys are variables of the container host or image that a
// destination sets itself or never needs: process plumbing, toolchain
// markers of official images, and the connections of data services the
// destination provisions. They are dropped unless the panel sets them.
var moveRuntimeOwnedKeys = map[string]bool{
	"PATH": true, "HOME": true, "HOSTNAME": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "LANG": true, "LC_ALL": true, "TERM": true, "SHLVL": true, "PWD": true, "TMPDIR": true,
	"LIDZA_ADDR": true, "PORT": true, "HOST": true, "AUTH_OWNER_CLAIM_DIR": true,
	"DATABASE_URL": true, "REDIS_URL": true, "CACHE_URL": true, "STORAGE_DIR": true,
	"GOLANG_VERSION": true, "GOPATH": true, "GOTOOLCHAIN": true, "GOFLAGS": true,
	"NODE_VERSION": true, "YARN_VERSION": true, "RUBY_VERSION": true, "RUBY_DOWNLOAD_SHA256": true, "RUBY_DOWNLOAD_URL": true,
	"GEM_HOME": true, "BUNDLE_APP_CONFIG": true, "BUNDLE_PATH": true, "BUNDLE_SILENCE_ROOT_WARNING": true,
	"PYTHON_VERSION": true, "PYTHON_SHA256": true, "DEBIAN_FRONTEND": true,
	"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_CACHE_HOME": true,
}

func moveRuntimeOwnedKey(key string) bool {
	switch {
	case moveRuntimeOwnedKeys[key]:
		return true
	case strings.HasPrefix(key, "GOSU_"), strings.HasPrefix(key, "CLICKHOUSE_"), strings.HasPrefix(key, "LC_"), strings.HasPrefix(key, "NPM_CONFIG_"), strings.HasPrefix(key, "PIP_"), strings.HasPrefix(key, gatewayEnvPrefix):
		return true
	}
	return false
}

// containerEnvironment is a container application's effective environment
// with the source of each variable.
type containerEnvironment struct {
	Values  map[string]string
	Sources map[string]string
}

// agentRuntimeEnvironment is what the agent itself adds to a container at
// run time (runtimeArgs and the APP_URL default), used when no container
// exists to inspect.
func agentRuntimeEnvironment(a App) map[string]string {
	values := map[string]string{}
	args := runtimeArgs(a)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--env" {
			key, value, _ := strings.Cut(args[i+1], "=")
			values[key] = value
			i++
		}
	}
	if a.Env["APP_URL"] == "" && a.Domain != "" {
		values["APP_URL"] = "https://" + a.Domain
	}
	return values
}

// containerEffectiveEnvironment returns the environment the application's
// current container runs with (running or stopped), or, without one, the
// image's ENV under the agent's runtime variables and the stored settings.
// Gateway transport keys never leave the host; runtime-owned keys are
// dropped unless the panel sets them.
func (m *Manager) containerEffectiveEnvironment(ctx context.Context, a App) containerEnvironment {
	stored := map[string]string{}
	for key, value := range a.Env {
		if !strings.HasPrefix(key, gatewayEnvPrefix) {
			stored[key] = value
		}
	}
	var imageEnv, containerEnv map[string]string
	if a.Current != nil {
		if a.Current.Image != "" {
			imageEnv, _ = moveInspectEnvironment(ctx, "image", a.Current.Image)
		}
		if a.Current.Container != "" {
			containerEnv, _ = moveInspectEnvironment(ctx, "container", a.Current.Container)
		}
	}
	effective := containerEnv
	if effective == nil {
		effective = maps.Clone(imageEnv)
		if effective == nil {
			effective = map[string]string{}
		}
		maps.Copy(effective, agentRuntimeEnvironment(a))
		maps.Copy(effective, stored)
	}
	out := containerEnvironment{Values: map[string]string{}, Sources: map[string]string{}}
	for key, value := range effective {
		if strings.HasPrefix(key, gatewayEnvPrefix) || !envPattern.MatchString(key) {
			continue
		}
		source := EnvironmentSourceDefaults
		switch imageValue, fromImage := imageEnv[key]; {
		case stored[key] != "" || hasKey(stored, key):
			source, value = EnvironmentSourcePanel, stored[key]
		case fromImage && imageValue == value:
			source = EnvironmentSourceImage
		}
		if source != EnvironmentSourcePanel && moveRuntimeOwnedKey(key) {
			continue
		}
		out.Values[key], out.Sources[key] = value, source
	}
	// Stored settings the container predates (set after the last deploy)
	// still belong to the application.
	for key, value := range stored {
		if _, present := out.Values[key]; !present {
			out.Values[key], out.Sources[key] = value, EnvironmentSourcePanel
		}
	}
	return out
}

func hasKey(values map[string]string, key string) bool {
	_, ok := values[key]
	return ok
}

// sortedKeys returns the keys of values in order.
func sortedKeys(values map[string]string) []string {
	return slices.Sorted(maps.Keys(values))
}
