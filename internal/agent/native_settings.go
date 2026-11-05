package agent

import (
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"
)

// Native settings share the ordinary application schema. Reading them never
// sources an environment file. Ordinary settings are readable; credential
// values remain absent from normal responses, as with container applications.
func (m *Manager) nativeSettings(id string) (Settings, error) {
	a, err := m.nativeApp(id)
	if err != nil {
		return Settings{}, err
	}
	runtime := a.Runtime
	if runtime == "" {
		runtime = "rails"
	}
	values := m.nativeEnvironment(a.Name)
	source := "existing host configuration"
	override := m.nativeOverride(id)
	if runtime == "astro" {
		values = applyNativeEnvironment(values, override)
	}
	kinds := normalizeEnvironmentKinds(values, override.Kinds)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	environment := make([]EnvironmentEntry, 0, len(keys))
	bindings := map[string]string{}
	for _, key := range keys {
		value := values[key]
		kind := kinds[key]
		// Imported shell expressions are not evaluated or expanded into the UI.
		if strings.ContainsAny(value, "$`\r\n") {
			kind = EnvironmentCredential
		}
		entry := EnvironmentEntry{Key: key, Kind: kind, Source: source}
		if kind == EnvironmentCredential {
			entry.Length = utf8.RuneCountInString(value)
		} else {
			entry.Value = value
		}
		environment = append(environment, entry)
		if key == "DATABASE_URL" || strings.HasSuffix(key, "_DATABASE_URL") {
			bindings[key] = "Existing host database"
		}
	}
	settings := Settings{Native: &a, ID: a.ID, Runtime: runtime, Branch: a.Branch, Domain: a.Domain, AdditionalDomainsSupported: true, AdditionalDomains: slices.Clone(a.AdditionalDomains), EnvKeys: keys, Environment: environment, Bindings: bindings, RuntimeDeployed: true, Capabilities: m.nativeManagementCapabilities(a)}
	m.mu.Lock()
	for resource, binding := range m.data.NativeDatabases {
		if binding.Policy.AppID == id && binding.Policy.EnvironmentKey != "" {
			bindings[binding.Policy.EnvironmentKey] = resource
		}
	}
	m.mu.Unlock()
	if runtime == "rails" {
		settings.Rails = &RailsConfig{HealthPath: a.HealthPath}
	}
	// Applications deployed by the release engine (any runtime) keep their
	// environment in the policy's environment files.
	if _, policy, err := m.nativeReleasePolicy(id); err == nil {
		if err := m.nativeReleaseSettings(&settings, policy, override); err != nil {
			return Settings{}, err
		}
		return settings, nil
	}
	if runtime != "rails" {
		settings.Astro = &AstroConfig{Mode: "static", OutputDir: a.DocumentRoot, HealthPath: a.HealthPath}
	}
	if settings.Capabilities.Configure {
		settings.SettingsApplyOnDeploy = true
		settings.BackupBeforeDeploy = true
		if override.Branch != "" {
			settings.Branch = override.Branch
		}
		policy, _ := m.nativeStaticPolicy(a)
		settings.Astro.OutputDir = policy.OutputDirectory
		if override.OutputDirectory != "" {
			settings.Astro.OutputDir = override.OutputDirectory
		}
		for _, key := range keys {
			if nativeManagedEnvironment(key) {
				settings.ReadonlyEnvKeys = append(settings.ReadonlyEnvKeys, key)
			}
		}
	}
	return settings, nil
}

func (m *Manager) nativeManagementCapabilities(a NativeApplication) *ManagementCapabilities {
	capability := &ManagementCapabilities{Logs: len(a.Services) > 0, Processes: true, Reason: "Existing runtime and deployment ownership are preserved. Configuration and deployment takeover require the reviewed host change plan."}
	if m.nativeReleaseManaged(a.ID) {
		// The release policy owns branch, processes, schedule and backups;
		// deploys and rollbacks run through the release engine.
		no := false
		capability.Routing, capability.Resources, capability.RuntimeMode = &no, &no, &no
		capability.Deploy, capability.Rollback, capability.Logs = true, true, true
		capability.Configure, capability.Environment = true, true
		capability.Reason = "Environment variables are saved to the application's environment file on the server and the branch to its release policy; both apply on the next deploy. Hostnames stay with the existing web server configuration."
		return capability
	}
	if _, err := m.nativeStaticPolicy(a); err == nil {
		capability.Configure = true
		capability.Environment = true
		no := false
		capability.Routing, capability.Resources, capability.RuntimeMode = &no, &no, &no
		capability.Deploy = true
		capability.Rollback = true
		capability.Reason = "Branch, build output and application environment changes apply on the next deployment. Existing hostnames and Node version are preserved."
	}
	return capability
}

func (m *Manager) nativeSettingsRoute(w http.ResponseWriter, r *http.Request) {
	settings, err := m.nativeSettings(r.PathValue("id"))
	if err != nil {
		Fail(w, 409, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, 200, settings)
}
