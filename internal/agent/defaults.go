package agent

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Shared defaults deliberately exclude credentials and host-specific attachments.
func ValidateAppDefaults(values map[string]string) error {
	for k, v := range values {
		switch k {
		case "DB_MIGRATE", "ADMIN_USERS", "AUTH_OWNER_CLAIM", "MAIL_FROM", "MAIL_PROVIDER", "LOG_LEVEL":
		default:
			return errors.New("unsupported application default: " + k)
		}
		if len(v) > 2048 || strings.ContainsAny(v, "\r\n\x00") {
			return errors.New("invalid application default: " + k)
		}
		if (k == "DB_MIGRATE" || k == "AUTH_OWNER_CLAIM") && v != "" && v != "true" && v != "false" {
			return errors.New(k + " must be true or false")
		}
	}
	return nil
}

// DefaultsRuntimes lists the runtimes with their own workspace defaults, in
// display order. "go" is the Līdza runtime and keeps its strict allowlist.
var DefaultsRuntimes = []string{"go", "rails", "astro", "node"}

// MaxRuntimeDefaults bounds the shared profile of one non-Go runtime.
const MaxRuntimeDefaults = 50

// runtimeDefaultsHostKey reports names owned by the host or the agent: they
// describe the process, toolchain or account, never the application.
func runtimeDefaultsHostKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, prefix := range []string{"LD_", "XDG_", "LIDZA_", gatewayEnvPrefix} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return slices.Contains([]string{"PATH", "HOME", "USER", "SHELL", "PORT", "PWD", "BUNDLE_GEMFILE", "NODE_OPTIONS", "BASH_ENV", "ENV"}, upper) ||
		nativeReleaseHostKey(key) || nativeManagedEnvironment(key)
}

// runtimeDefaultsCredentialKey reports names that look like credentials or
// per-application service attachments. Databases, caches and providers
// belong to each application, so they are never shared through defaults.
func runtimeDefaultsCredentialKey(key string) bool {
	upper := strings.ToUpper(key)
	// Whole name segments, so MASTER_KEY or API_TOKEN are refused while
	// MONKEY_MODE is not; plurals and common forms are included.
	for _, segment := range strings.Split(upper, "_") {
		switch segment {
		case "KEY", "KEYS", "SECRET", "SECRETS", "TOKEN", "TOKENS", "PASSWORD", "PASSWD", "PASS", "PWD", "CREDENTIAL", "CREDENTIALS", "PRIVATE", "SALT", "CERT", "PEM":
			return true
		}
	}
	return strings.HasSuffix(upper, "_URL") || strings.HasSuffix(upper, "_DSN")
}

// ValidateRuntimeDefaults validates the shared defaults of one runtime. The
// Līdza (Go) runtime keeps its allowlist; Rails, Astro and Node.js accept
// ordinary settings that are safe to store literally in a native
// environment file.
func ValidateRuntimeDefaults(runtime string, values map[string]string) error {
	switch runtime {
	case "", "go":
		return ValidateAppDefaults(values)
	case "rails", "astro", "node":
	default:
		return errors.New("unsupported application runtime: " + runtime)
	}
	if len(values) > MaxRuntimeDefaults {
		return fmt.Errorf("at most %d defaults per runtime", MaxRuntimeDefaults)
	}
	for _, k := range slices.Sorted(maps.Keys(values)) {
		v := values[k]
		switch {
		case !envPattern.MatchString(k) || len(k) > 128:
			return fmt.Errorf("invalid environment variable name %q", k)
		case runtimeDefaultsHostKey(k):
			return fmt.Errorf("%s is managed by the host and cannot be a workspace default", k)
		case runtimeDefaultsCredentialKey(k):
			return fmt.Errorf("%s looks like a credential or service connection; configure it on each application", k)
		case len(v) > 2048:
			return fmt.Errorf("%s must be at most 2048 characters", k)
		case strings.ContainsAny(v, "\r\n\x00"):
			return fmt.Errorf("%s cannot contain line breaks", k)
		case strings.Contains(v, "'"):
			return fmt.Errorf("%s cannot contain single quotes", k)
		}
	}
	return nil
}

// defaultsFor reports which workspace defaults the request carries for this
// application, or nil when they belong to another runtime. Requests without
// a runtime carry the Līdza (Go) set.
func defaultsFor(a App, req DeployRequest) map[string]string {
	runtime := req.DefaultsRuntime
	if runtime == "" {
		runtime = "go"
	}
	if runtime != a.runtimeKind() {
		return nil
	}
	return req.Defaults
}

// Called under the manager lock; the snapshot is saved together with the queue.
// Values must already belong to the application's runtime (see defaultsFor).
func (m *Manager) initialDefaults(a App, values map[string]string) App {
	if a.DefaultsApplied {
		return a
	}
	a.DefaultsApplied = true
	if a.Current != nil {
		return a
	}
	for _, d := range m.data.Deployments {
		if d.AppID == a.ID {
			return a
		}
	}
	a.Env = maps.Clone(a.Env)
	a.EnvSources = maps.Clone(a.EnvSources)
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	if a.EnvSources == nil {
		a.EnvSources = map[string]string{}
	}
	for k, v := range values {
		if v == "" {
			continue
		}
		if _, exists := a.Env[k]; exists && a.EnvSources[k] != "built-in" {
			continue
		}
		a.Env[k] = v
		a.EnvSources[k] = "workspace"
	}
	return a
}
