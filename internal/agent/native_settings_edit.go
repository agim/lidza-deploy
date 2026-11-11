package agent

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// Overrides live in the agent's sealed state. Saving never modifies the serving
// release, imported shell files, routing, runtime pins, or service state.
type NativeSettingsOverride struct {
	Branch               string             `json:"branch,omitempty"`
	OutputDirectory      string             `json:"output_directory,omitempty"`
	Environment          map[string]*string `json:"environment,omitempty"`
	InheritedEnvironment map[string]string  `json:"inherited_environment,omitempty"`
	Kinds                map[string]string  `json:"kinds,omitempty"`
	// Toolchains replaces the release policy's toolchains choice ("managed"
	// or "account") for release-managed applications. The next deploy
	// builds with it; the serving release is untouched.
	Toolchains string `json:"toolchains,omitempty"`
}

func nativeManagedEnvironment(key string) bool {
	return strings.HasPrefix(key, "ASDF_") || strings.HasPrefix(key, "NVM_") || strings.HasPrefix(key, "LIDZA_") || slices.Contains([]string{"PATH", "HOME", "USER", "SHELL", "NODE_VERSION", "NODE_OPTIONS", "LD_PRELOAD", "LD_LIBRARY_PATH", "BASH_ENV", "ENV", "PORT"}, key)
}

func applyNativeEnvironment(values map[string]string, o NativeSettingsOverride) map[string]string {
	base := maps.Clone(o.InheritedEnvironment)
	if base == nil {
		base = map[string]string{}
	}
	maps.Copy(base, values)
	values = base
	for key, value := range o.Environment {
		if value == nil {
			delete(values, key)
		} else {
			values[key] = *value
		}
	}
	return values
}

func (m *Manager) nativeOverride(id string) NativeSettingsOverride {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.data.NativeSettings[id]
	o.Environment = maps.Clone(o.Environment)
	o.Kinds = maps.Clone(o.Kinds)
	o.InheritedEnvironment = maps.Clone(o.InheritedEnvironment)
	return o
}

func (m *Manager) nativeStaticEnvironment(a NativeApplication) (map[string]string, error) {
	values, err := m.nativeBuildEnvironment(a.Name)
	if err != nil {
		return nil, err
	}
	return applyNativeEnvironment(values, m.nativeOverride(a.ID)), nil
}

// patchNativeSettings saves a native application's settings and applies
// them (native_settings_apply.go).
func (m *Manager) patchNativeSettings(id string, p SettingsPatch) (SettingsApplyResult, error) {
	a, err := m.nativeApp(id)
	if err != nil {
		return SettingsApplyResult{}, err
	}
	if m.nativeReleaseManaged(id) {
		return m.patchNativeReleaseSettings(a, p)
	}
	changes, err := m.saveNativeStaticSettings(a, p)
	if err != nil {
		return SettingsApplyResult{}, err
	}
	return m.applyNativeSettings(id, settingsSubject{Runtime: "static", Native: true}, changes), nil
}

// saveNativeStaticSettings saves a static publication's settings and names
// what changed.
func (m *Manager) saveNativeStaticSettings(a NativeApplication, p SettingsPatch) ([]string, error) {
	id := a.ID
	policy, err := m.nativeStaticPolicy(a)
	if err != nil {
		return nil, err
	}
	if p.Domain != "" && p.Domain != a.Domain || p.AdditionalDomains != nil && !slices.Equal(*p.AdditionalDomains, a.AdditionalDomains) || p.CustomerHosts != nil && *p.CustomerHosts {
		return nil, errors.New("hostname and routing changes require a reviewed host change")
	}
	if p.Build != nil && p.Build.SourceDirectory != "" || p.RuntimeProfile != nil && *p.RuntimeProfile != "" || p.BackupBeforeDeploy != nil && !*p.BackupBeforeDeploy || p.Toolchains != nil {
		return nil, errors.New("this native publisher preserves the existing runtime and backup setup")
	}
	if p.FromDefaults {
		defaults := map[string]string{}
		for key, value := range p.EnvChanges {
			if value == nil {
				return nil, errors.New("defaults cannot delete variables")
			}
			defaults[key] = *value
		}
		if err := ValidateRuntimeDefaults("astro", defaults); err != nil {
			return nil, err
		}
	}
	values, err := m.nativeBuildEnvironment(a.Name)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nativeOperations[id] {
		return nil, errors.New("native deployment is active; save settings after it finishes")
	}
	if _, pending := m.data.NativeTransitions[id]; pending {
		return nil, errors.New("native publication recovery requires review")
	}
	old := m.data.NativeSettings[id]
	next := old
	if next.InheritedEnvironment == nil {
		next.InheritedEnvironment = maps.Clone(values)
	}
	next.Environment = maps.Clone(old.Environment)
	if next.Environment == nil {
		next.Environment = map[string]*string{}
	}
	next.Kinds = maps.Clone(old.Kinds)
	if next.Kinds == nil {
		next.Kinds = map[string]string{}
	}
	if p.Branch != "" {
		next.Branch = p.Branch
	}
	branch := next.Branch
	if branch == "" {
		branch = a.Branch
	}
	if p.Astro != nil {
		if p.Astro.Mode != "" && p.Astro.Mode != "static" || p.Astro.NodeMajor != "" || p.Astro.HealthPath != "" && p.Astro.HealthPath != a.HealthPath {
			return nil, errors.New("runtime changes require review; the existing static runtime is preserved")
		}
		if p.Astro.OutputDir != "" {
			next.OutputDirectory = p.Astro.OutputDir
		}
	}
	output := next.OutputDirectory
	if output == "" {
		output = policy.OutputDirectory
	}
	if !cleanRelativeDirectory(output) || output == "." || output == "" {
		return nil, errors.New("build output must be a directory inside the release")
	}
	// Workspace defaults applied as missing-only keep every variable the
	// application already has, imported or saved, even when empty.
	present := applyNativeEnvironment(values, next)
	for key, value := range p.EnvChanges {
		if nativeManagedEnvironment(key) {
			return nil, errors.New("runtime and hosting environment variables are managed by the host")
		}
		if _, exists := present[key]; exists && p.FromDefaults && p.OnlyMissing {
			continue
		}
		if value != nil {
			copy := *value
			next.Environment[key] = &copy
		} else {
			next.Environment[key] = nil
			delete(next.Kinds, key)
		}
	}
	inherited := values
	values = applyNativeEnvironment(values, next)
	applied := envValueChanges(applyNativeEnvironment(inherited, old), values)
	kinds := normalizeEnvironmentKinds(applyNativeEnvironment(inherited, old), old.Kinds)
	if branch != cmpOr(old.Branch, a.Branch) {
		applied = append(applied, "branch")
	}
	if output != cmpOr(old.OutputDirectory, policy.OutputDirectory) {
		applied = append(applied, "astro")
	}
	for key, kind := range p.EnvKindChanges {
		if nativeManagedEnvironment(key) {
			return nil, errors.New("hosting environment classifications are managed by the host")
		}
		if _, ok := values[key]; !ok {
			return nil, errors.New("environment classification references a missing variable")
		}
		if kind != EnvironmentSetting && kind != EnvironmentCredential {
			return nil, errors.New("invalid environment classification")
		}
		if kinds[key] != kind {
			applied = append(applied, "env_kind:"+key)
		}
		next.Kinds[key] = kind
	}
	for key, value := range values {
		if strings.Contains(key, "DATABASE") && strings.Contains(key, "URL") || strings.HasPrefix(value, "postgres:") || strings.HasPrefix(value, "postgresql:") || strings.HasPrefix(value, "mysql:") {
			return nil, errors.New("native database ownership requires a reviewed binding")
		}
	}
	spec := App{ID: a.ID, Runtime: "astro", Repository: a.Repository, Branch: branch, Domain: a.Domain, Astro: &AstroConfig{Mode: "static", OutputDir: output}, Env: values, EnvKinds: next.Kinds}
	// Imported platform variables are retained by the host, never submitted as
	// application-owned variables to the shared validation layer.
	spec.Env = maps.Clone(values)
	for key := range spec.Env {
		if nativeManagedEnvironment(key) {
			delete(spec.Env, key)
		}
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if m.data.NativeSettings == nil {
		m.data.NativeSettings = map[string]NativeSettingsOverride{}
	}
	m.data.NativeSettings[id] = next
	if err := m.save(); err != nil {
		m.data.NativeSettings[id] = old
		return nil, err
	}
	return slices.Compact(slices.Sorted(slices.Values(applied))), nil
}

func (m *Manager) patchNativeSettingsRoute(w http.ResponseWriter, r *http.Request) {
	var p SettingsPatch
	if err := Decode(w, r, &p); err != nil {
		Fail(w, 400, err)
		return
	}
	result, err := m.patchNativeSettings(r.PathValue("id"), p)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	settings, err := m.nativeSettings(r.PathValue("id"))
	if err != nil {
		Fail(w, 409, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, 200, NativeSettingsSaved{Settings: settings, Apply: result})
}

// NativeSettingsSaved answers a native settings save: the saved settings
// and what the save applied.
type NativeSettingsSaved struct {
	Settings
	Apply SettingsApplyResult `json:"apply"`
}

func (m *Manager) revealNativeCredential(w http.ResponseWriter, r *http.Request) {
	a, err := m.nativeApp(r.PathValue("id"))
	if err != nil {
		Fail(w, 409, err)
		return
	}
	if m.nativeReleaseManaged(a.ID) {
		value, err := m.nativeReleaseCredential(a, r.PathValue("key"))
		if err != nil {
			Fail(w, 409, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, map[string]string{"key": r.PathValue("key"), "value": value})
		return
	}
	if _, err = m.nativeStaticPolicy(a); err != nil {
		Fail(w, 409, err)
		return
	}
	values, err := m.nativeStaticEnvironment(a)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	key := r.PathValue("key")
	value, ok := values[key]
	if !ok || nativeManagedEnvironment(key) || normalizeEnvironmentKinds(values, m.nativeOverride(a.ID).Kinds)[key] != EnvironmentCredential {
		Fail(w, 409, errors.New("no revealable application credential"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, 200, map[string]string{"key": key, "value": value})
}
