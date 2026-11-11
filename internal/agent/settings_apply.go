package agent

import (
	"slices"
	"strings"
)

// What saving an application's settings does to the running application.
// Every save answers with one SettingsApplyResult, for every runtime:
//
//   - restart: the change is read when processes start (environment
//     variables and credentials, including the master key, and database
//     attachments). The agent rewrites what the release reads (the
//     container's environment, the native environment files) and restarts
//     the application the way Restart does: a container application
//     reloads its current image into a new container that takes traffic
//     once healthy; a native application restarts its units in order and
//     checks health (release.Restart), the restart a deploy does.
//   - deploy: the change affects the build (repository directory, branch,
//     Astro output, stack profile, toolchains, variables read at build
//     time). The control panel starts a deployment and returns it.
//   - immediate: the change takes effect without touching processes
//     (additional hostnames, customer hostnames, deployment backups,
//     whether a variable is shown as a setting or a credential), or there
//     is nothing running to restart (never deployed, stopped).
//   - saved_restart_required: a restart is needed but the panel cannot
//     restart this application (service control is off for an adopted
//     application); the change is saved and Message says why.
//
// The classification is this file's table; nothing else decides it.

const (
	SettingsAppliedImmediate       = "immediate"
	SettingsAppliedRestart         = "restart"
	SettingsAppliedDeploy          = "deploy"
	SettingsAppliedRestartRequired = "saved_restart_required"
)

// SettingsApplyResult is what one settings save did. Message never contains
// a variable's value, only its name.
type SettingsApplyResult struct {
	Applied string `json:"applied"`
	// Deployment is the restart (a reload or restart job) or the
	// deployment the save started; DeploymentID repeats its id.
	Deployment   *Deployment `json:"deployment,omitempty"`
	DeploymentID string      `json:"deployment_id,omitempty"`
	Message      string      `json:"message"`
	// Error is set when the save succeeded but the restart or deployment
	// could not start; the settings stay saved.
	Error string `json:"error,omitempty"`
	// Changes names what changed: fields, and env:NAME for variables.
	Changes []string `json:"changes,omitempty"`
}

// SettingsApplyEffects tells the console, before a save, what a change of
// each field does (immediate, restart or deploy), so the Save button can
// say "Save and deploy". It is derived from the same table as the save.
type SettingsApplyEffects struct {
	Fields map[string]string `json:"fields"`
	// Env is the effect of changing a variable's value, except those
	// matching BuildEnvPrefixes, which are read at build time (deploy).
	Env              string   `json:"env"`
	BuildEnvPrefixes []string `json:"build_env_prefixes,omitempty"`
}

type settingsEffect int

const (
	effectNone settingsEffect = iota
	effectImmediate
	effectRestart
	effectDeploy
)

func (e settingsEffect) String() string {
	return [...]string{"", SettingsAppliedImmediate, SettingsAppliedRestart, SettingsAppliedDeploy}[e]
}

// settingsFieldEffects classifies every settings field but environment
// values (settingsSubject.envEffect) and the primary hostname
// (settingsSubject.effect).
var settingsFieldEffects = map[string]settingsEffect{
	// Build-affecting: a new build is needed.
	"branch":          effectDeploy,
	"build":           effectDeploy,
	"astro":           effectDeploy,
	"runtime_profile": effectDeploy,
	"toolchains":      effectDeploy,
	// Read when processes start.
	"database_bindings": effectRestart,
	// Served by the agent or the web server as soon as they are saved.
	"additional_domains":   effectImmediate,
	"customer_hosts":       effectImmediate,
	"backup_before_deploy": effectImmediate,
	"env_kind":             effectImmediate,
}

// settingsSubject is what classification needs to know about an application.
type settingsSubject struct {
	// Runtime is go, rails or astro for container applications; rails,
	// node, go, static or auto for release-managed native applications;
	// static for native static publications.
	Runtime string
	Native  bool
	// AgentAppURL: the agent sets APP_URL from the primary hostname when
	// the container starts (the application sets no APP_URL itself).
	AgentAppURL bool
}

// buildEnvPrefixes are the variables a runtime reads at build time: Astro
// (and other Vite-based Node builds) inline PUBLIC_ variables into the
// client bundle. A static site reads every variable at build time only.
func (s settingsSubject) buildEnvPrefixes() []string {
	switch s.Runtime {
	case "static":
		return []string{""}
	case "astro", "node", "auto":
		return []string{"PUBLIC_"}
	}
	return nil
}

func (s settingsSubject) envEffect(key string) settingsEffect {
	for _, prefix := range s.buildEnvPrefixes() {
		if strings.HasPrefix(key, prefix) {
			return effectDeploy
		}
	}
	return effectRestart
}

// effect classifies one change key: a field name, env:NAME or env_kind:NAME.
func (s settingsSubject) effect(change string) settingsEffect {
	if key, ok := strings.CutPrefix(change, "env:"); ok {
		return s.envEffect(key)
	}
	if strings.HasPrefix(change, "env_kind:") {
		return settingsFieldEffects["env_kind"]
	}
	if change == "domain" {
		// Routing follows at once; the process reads the hostname only
		// through the APP_URL the agent sets when the container starts.
		if !s.Native && s.AgentAppURL {
			return effectRestart
		}
		return effectImmediate
	}
	if e, ok := settingsFieldEffects[change]; ok {
		return e
	}
	// A field this table does not know yet is applied the safe way.
	return effectDeploy
}

// classify returns the strongest effect of changes: a deploy also restarts,
// a restart also applies immediate changes.
func (s settingsSubject) classify(changes []string) settingsEffect {
	strongest := effectNone
	for _, change := range changes {
		strongest = max(strongest, s.effect(change))
	}
	return strongest
}

// effects describes the table for the console.
func (s settingsSubject) effects() *SettingsApplyEffects {
	out := &SettingsApplyEffects{Fields: map[string]string{}, Env: effectRestart.String(), BuildEnvPrefixes: s.buildEnvPrefixes()}
	for field := range settingsFieldEffects {
		out.Fields[field] = s.effect(field).String()
	}
	out.Fields["domain"] = s.effect("domain").String()
	if slices.Contains(out.BuildEnvPrefixes, "") {
		out.Env, out.BuildEnvPrefixes = effectDeploy.String(), nil
	}
	return out
}

// containerSettingsSubject is the subject of a container application.
func containerSettingsSubject(a App) settingsSubject {
	return settingsSubject{Runtime: a.runtimeKind(), AgentAppURL: a.Env["APP_URL"] == ""}
}

// containerSettingsChanges names what a settings save changed.
func containerSettingsChanges(old, a App) []string {
	changes := changeKeys(old, a)
	if old.Build != a.Build {
		changes = append(changes, "build")
	}
	if !astroConfigEqual(old.Astro, a.Astro) {
		changes = append(changes, "astro")
	}
	if (old.BackupBeforeDeploy == nil || *old.BackupBeforeDeploy) != (a.BackupBeforeDeploy == nil || *a.BackupBeforeDeploy) {
		changes = append(changes, "backup_before_deploy")
	}
	oldKinds, kinds := normalizeEnvironmentKinds(old.Env, old.EnvKinds), normalizeEnvironmentKinds(a.Env, a.EnvKinds)
	for key, kind := range kinds {
		if _, existed := old.Env[key]; existed && oldKinds[key] != kind {
			changes = append(changes, "env_kind:"+key)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(changes)))
}

func astroConfigEqual(a, b *AstroConfig) bool {
	var x, y AstroConfig
	if a != nil {
		x = *a
	}
	if b != nil {
		y = *b
	}
	return x == y
}

// envValueChanges names the variables whose value changes from before to after.
func envValueChanges(before, after map[string]string) []string {
	changes := []string{}
	for key, value := range after {
		if old, ok := before[key]; !ok || old != value {
			changes = append(changes, "env:"+key)
		}
	}
	for key := range before {
		if _, ok := after[key]; !ok {
			changes = append(changes, "env:"+key)
		}
	}
	return changes
}

// settingsResult builds the result of a save whose effect needs nothing
// more from the agent (immediate, or a deploy the control panel starts).
func settingsResult(effect settingsEffect, changes []string) SettingsApplyResult {
	result := SettingsApplyResult{Applied: SettingsAppliedImmediate, Message: "Settings saved.", Changes: slices.Clone(changes)}
	if effect == effectDeploy {
		result.Applied, result.Message = SettingsAppliedDeploy, "Settings saved. A deployment builds and releases them."
	}
	return result
}

func withDeployment(result SettingsApplyResult, d Deployment) SettingsApplyResult {
	if d.ID != "" {
		result.Deployment, result.DeploymentID = &d, d.ID
	}
	return result
}
