package agent

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

// Environment settings are editable; credentials stay masked unless a separate,
// purpose-bound step-up request reveals one. Omitted keys survive and null deletes.
type SettingsPatch struct {
	Build              *BuildConfig       `json:"build,omitempty"`
	Astro              *AstroConfig       `json:"astro,omitempty"`
	RuntimeProfile     *string            `json:"runtime_profile,omitempty"`
	AdditionalDomains  *[]string          `json:"additional_domains,omitempty"`
	CustomerHosts      *bool              `json:"customer_hosts,omitempty"`
	FromDefaults       bool               `json:"from_defaults,omitempty"`
	OnlyMissing        bool               `json:"only_missing,omitempty"`
	BackupBeforeDeploy *bool              `json:"backup_before_deploy,omitempty"`
	Branch             string             `json:"branch"`
	Domain             string             `json:"domain"`
	EnvChanges         map[string]*string `json:"env_changes"`
	EnvKindChanges     map[string]string  `json:"env_kind_changes,omitempty"`
	// Toolchains switches a release-managed native application between the
	// agent's managed toolchains and the account's version manager.
	Toolchains *string `json:"toolchains,omitempty"`
}
type EnvironmentEntry struct {
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Value   string `json:"value,omitempty"`
	Preview string `json:"preview,omitempty"`
	Length  int    `json:"length,omitempty"`
	Source  string `json:"source,omitempty"`
}
type Settings struct {
	Native          *NativeApplication `json:"native,omitempty"`
	ReadonlyEnvKeys []string           `json:"readonly_env_keys,omitempty"`
	// ApplyEffects says what saving each field does (settings_apply.go).
	ApplyEffects   *SettingsApplyEffects `json:"apply_effects,omitempty"`
	ReleaseRuntime string                `json:"release_runtime,omitempty"`
	// Toolchains is "managed" or "account" for release-managed native
	// applications (NativeReleasePolicy.Toolchains); "" elsewhere.
	Toolchains                   string                  `json:"toolchains,omitempty"`
	Capabilities                 *ManagementCapabilities `json:"capabilities,omitempty"`
	BuildConfigurationSupported  bool                    `json:"build_configuration_supported"`
	Astro                        *AstroConfig            `json:"astro,omitempty"`
	Build                        BuildConfig             `json:"build,omitempty,omitzero"`
	Runtime                      string                  `json:"runtime,omitempty"`
	Rails                        *RailsConfig            `json:"rails,omitempty"`
	GatewayTransportSupported    bool                    `json:"gateway_transport_supported"`
	GatewayHostConfigured        bool                    `json:"gateway_host_configured"`
	StackManifestVersions        []int                   `json:"stack_manifest_versions"`
	RuntimeSelection             *StackSelectionSnapshot `json:"runtime_selection,omitempty"`
	PublicPortsSupported         bool                    `json:"public_ports_supported"`
	MasterKeyGenerationSupported bool                    `json:"master_key_generation_supported"`
	RuntimeDeployed              bool                    `json:"runtime_deployed"`
	StackProfilesSupported       bool                    `json:"stack_profiles_supported"`
	RuntimeProfile               string                  `json:"runtime_profile"`
	StackProfiles                []StackProfile          `json:"stack_profiles"`
	AdditionalDomainsSupported   bool                    `json:"additional_domains_supported"`
	AdditionalDomains            []string                `json:"additional_domains"`
	DomainStatuses               []DomainStatus          `json:"domain_statuses,omitempty"`
	AppManagedStorageSupported   bool                    `json:"app_managed_storage_supported"`
	StorageProviderOverride      bool                    `json:"storage_provider_override"`
	CustomerHosts                bool                    `json:"customer_hosts"`
	Stopped                      bool                    `json:"stopped"`
	DefaultsApplied              bool                    `json:"defaults_applied"`
	EnvSources                   map[string]string       `json:"env_sources"`
	PersistentStorage            bool                    `json:"persistent_storage"`
	Cache                        CacheView               `json:"cache"`
	BackupBeforeDeploy           bool                    `json:"backup_before_deploy"`
	Bindings                     map[string]string       `json:"database_bindings"`
	ID                           string                  `json:"id"`
	Branch                       string                  `json:"branch"`
	Domain                       string                  `json:"domain"`
	EnvKeys                      []string                `json:"env_keys"`
	Environment                  []EnvironmentEntry      `json:"environment"`
}

type ManagementCapabilities struct {
	Routing     *bool `json:"routing,omitempty"`
	Resources   *bool `json:"resources,omitempty"`
	RuntimeMode *bool `json:"runtime_mode,omitempty"`
	Configure   bool  `json:"configure"`
	Environment bool  `json:"environment"`
	Deploy      bool  `json:"deploy"`
	Reload      bool  `json:"reload"`
	Rollback    bool  `json:"rollback"`
	Processes   bool  `json:"processes"`
	Logs        bool  `json:"logs"`
	// Remove reports that the panel can remove a native application: the
	// agent created it and removes its units, account and route.
	Remove bool `json:"remove,omitempty"`
	// Lifecycle reports that the application can be stopped, started and
	// restarted from the panel (app_lifecycle.go); LifecycleReason says why
	// not; LifecycleSetting names the server setting that would allow it
	// ("service_control", native_service_control.go).
	Lifecycle        bool   `json:"lifecycle,omitempty"`
	LifecycleReason  string `json:"lifecycle_reason,omitempty"`
	LifecycleSetting string `json:"lifecycle_setting,omitempty"`
	// Adopted marks a native application adopted from the existing host
	// setup (not created by this agent); deleting it keeps the shared
	// deployment account (archive_adopted.go).
	Adopted bool   `json:"adopted,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func (m *Manager) Settings(id string) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok {
		return Settings{}, errors.New("unknown application")
	}
	kinds := normalizeEnvironmentKinds(a.Env, a.EnvKinds)
	if tokenKey := runtimeSelectionTokenKey(a); tokenKey != "" {
		kinds[tokenKey] = EnvironmentCredential
	}
	keys := slices.Sorted(maps.Keys(a.Env))
	environment := make([]EnvironmentEntry, 0, len(keys))
	visibleKeys := []string{}
	for _, key := range keys {
		if strings.HasPrefix(key, gatewayEnvPrefix) {
			continue
		}
		visibleKeys = append(visibleKeys, key)
		value, kind := a.Env[key], kinds[key]
		entry := EnvironmentEntry{Key: key, Kind: kind, Source: a.EnvSources[key]}
		if kind == EnvironmentCredential {
			entry.Length = utf8.RuneCountInString(value)
			runes := []rune(value)
			if len(runes) > 4 {
				runes = runes[:4]
			} else if len(runes) > 0 {
				runes = runes[:len(runes)-1]
			}
			entry.Preview = string(runes)
			if entry.Length > 0 {
				entry.Preview += "…"
			}
		} else {
			entry.Value = value
		}
		environment = append(environment, entry)
	}
	var selection *StackSelectionSnapshot
	if a.Current != nil && a.Current.Stack != nil {
		selection = cloneStackSelection(a.Current.Stack.Selection)
	}
	return Settings{Capabilities: &ManagementCapabilities{Configure: true, Environment: true, Deploy: true, Reload: true, Rollback: true, Processes: true, Logs: true, Remove: true, Lifecycle: true}, BuildConfigurationSupported: true, Astro: a.Astro, Build: a.Build, Runtime: a.runtimeKind(), Rails: a.Rails, GatewayTransportSupported: true, GatewayHostConfigured: m.gatewayReady(), StackManifestVersions: []int{1, 2}, RuntimeSelection: selection, PublicPortsSupported: true, MasterKeyGenerationSupported: a.runtimeKind() == "go", RuntimeDeployed: a.Current != nil, StackProfilesSupported: a.runtimeKind() == "go", RuntimeProfile: a.RuntimeProfile, StackProfiles: stackProfiles, AdditionalDomainsSupported: true, AdditionalDomains: slices.Clone(a.AdditionalDomains), DomainStatuses: m.domainStatusesLocked(a), AppManagedStorageSupported: true, StorageProviderOverride: a.Env["STORAGE_PROVIDER"] != "", CustomerHosts: a.CustomerHosts, Stopped: a.Stopped, DefaultsApplied: a.DefaultsApplied, EnvSources: maps.Clone(a.EnvSources), PersistentStorage: m.data.StorageVolumes[id] != "", Cache: m.cacheViewLocked(id), BackupBeforeDeploy: a.BackupBeforeDeploy == nil || *a.BackupBeforeDeploy, Bindings: maps.Clone(a.Bindings), ID: id, Branch: a.Branch, Domain: a.Domain, EnvKeys: visibleKeys, Environment: environment, ApplyEffects: containerSettingsSubject(a).effects()}, nil
}

func (m *Manager) RevealCredential(id, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok || a.Retiring {
		return "", errors.New("unknown application")
	}
	value, ok := a.Env[key]
	if !ok {
		return "", errors.New("unknown environment variable")
	}
	if key != runtimeSelectionTokenKey(a) && normalizeEnvironmentKinds(a.Env, a.EnvKinds)[key] != EnvironmentCredential {
		return "", errors.New("environment variable is a setting and does not require reveal")
	}
	return value, nil
}

// PatchSettings saves settings and applies them (settings_apply.go).
func (m *Manager) PatchSettings(id string, p SettingsPatch) error {
	_, err := m.PatchSettingsApply(id, p)
	return err
}

// PatchSettingsApply saves settings, applies them as settings_apply.go
// classifies them and says what it did.
func (m *Manager) PatchSettingsApply(id string, p SettingsPatch) (SettingsApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.patchSettingsLocked(id, p)
}

func (m *Manager) patchSettingsLocked(id string, p SettingsPatch) (SettingsApplyResult, error) {
	var none SettingsApplyResult
	old, ok := m.data.Apps[id]
	if !ok {
		return none, errors.New("unknown application")
	}
	if old.Retiring {
		return none, errors.New("application is being removed")
	}
	if m.busy(id) || old.Restoring || m.data.Caches[id].Operation {
		return none, errors.New("application has an active deployment")
	}
	for key := range p.EnvChanges {
		if strings.HasPrefix(key, gatewayEnvPrefix) {
			return none, errors.New("gateway transport settings are managed by the agent")
		}
	}
	for key := range p.EnvKindChanges {
		if strings.HasPrefix(key, gatewayEnvPrefix) {
			return none, errors.New("gateway transport settings are managed by the agent")
		}
	}
	if p.FromDefaults {
		values := map[string]string{}

		for k, v := range p.EnvChanges {
			if v == nil {
				return none, errors.New("defaults cannot delete variables")
			}
			values[k] = *v
		}
		if err := ValidateRuntimeDefaults(old.runtimeKind(), values); err != nil {
			return none, err
		}
	}
	a := old
	if p.Build != nil {
		a.Build = *p.Build
	}
	if p.Astro != nil {
		a.Astro = p.Astro
	}
	if p.RuntimeProfile != nil {
		a.RuntimeProfile = *p.RuntimeProfile
		if old.Current != nil && (a.RuntimeProfile == "") != (old.RuntimeProfile == "") {
			return none, errors.New("changing runtime family requires a new application; existing data is retained")
		}
	}
	if p.CustomerHosts != nil {
		a.CustomerHosts = *p.CustomerHosts
	}
	a.EnvSources = maps.Clone(old.EnvSources)
	if a.EnvSources == nil {
		a.EnvSources = map[string]string{}
	}
	if p.BackupBeforeDeploy != nil {
		a.BackupBeforeDeploy = p.BackupBeforeDeploy
	}
	a.Branch = p.Branch
	a.Domain = p.Domain
	if p.AdditionalDomains != nil {
		a.AdditionalDomains = slices.Clone(*p.AdditionalDomains)
	}
	a.Env = maps.Clone(old.Env)
	a.EnvKinds = normalizeEnvironmentKinds(old.Env, old.EnvKinds)
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	for k, v := range p.EnvChanges {
		if p.FromDefaults && p.OnlyMissing {
			if _, exists := a.Env[k]; exists {
				continue
			}
		}
		delete(a.EnvSources, k)
		if p.FromDefaults {
			a.EnvSources[k] = "workspace"
		}
		if c := m.data.ClickHouses[id]; c.Ready {
			if _, managed := c.env()[k]; managed {
				return none, errors.New("ClickHouse connection is managed by the hosting agent")
			}
		}
		if k == "STORAGE_DIR" && m.data.StorageVolumes[id] != "" {
			return none, errors.New("STORAGE_DIR is managed by the persistent storage attachment")
		}
		if k == "CACHE_URL" && m.data.Caches[id].Mode != "" {
			return none, errors.New("CACHE_URL is managed through Cache settings")
		}
		if k == runtimeSelectionTokenKey(a) && (v == nil || *v == "") {
			return none, errors.New("runtime-selection credential cannot be cleared; preserve the installation's authenticated exchange identity")
		}
		if k == "AUTH_SECRET" && (v == nil || *v == "") {
			return none, errors.New("AUTH_SECRET cannot be removed or cleared; supply a replacement explicitly to rotate authentication tokens")
		}
		if k == "LIDZA_MASTER_KEY" && (v == nil || *v == "") {
			return none, errors.New("LIDZA_MASTER_KEY cannot be removed or cleared; replacing it requires the key matching your encrypted application data")
		}
		if _, managed := a.Bindings[k]; managed {
			return none, errors.New("database variables are managed by database attachments")
		}
		if v == nil {
			delete(a.Env, k)
			delete(a.EnvKinds, k)
		} else {
			a.Env[k] = *v
			if _, exists := a.EnvKinds[k]; !exists {
				a.EnvKinds[k] = environmentKind(k, *v)
			}
		}
	}
	for key, kind := range p.EnvKindChanges {
		if key == runtimeSelectionTokenKey(a) && kind != EnvironmentCredential {
			return none, errors.New("runtime-selection credentials remain masked; they cannot be changed to plain settings")
		}
		if kind != EnvironmentSetting && kind != EnvironmentCredential {
			return none, fmt.Errorf("invalid environment classification for %q", key)
		}
		if _, ok := a.Env[key]; !ok {
			return none, fmt.Errorf("cannot classify missing environment variable %q", key)
		}
		a.EnvKinds[key] = kind
	}
	if err := a.Validate(); err != nil {
		return none, err
	}
	if err := m.checkReservedDomains(a); err != nil {
		return none, err
	}
	if err := m.checkNativeDomainsLocked(a); err != nil {
		return none, err
	}
	if err := m.checkRedirectHostsLocked(a.Hostnames(), ""); err != nil {
		return none, err
	}
	for otherID, other := range m.data.Apps {
		if otherID != id && a.DomainsOverlap(other) {
			return none, errors.New("domain already assigned")
		}
	}
	result, err := m.applyContainerSettingsLocked(old, a, containerSettingsChanges(old, a))
	if err != nil {
		return none, err
	}
	if m.serviceTLS != nil {
		m.serviceTLS.setOwner(a)
	}
	m.wakeDomains()
	return result, nil
}

// applyContainerSettingsLocked stores a's saved settings and applies them
// (settings_apply.go): a restart reloads the current image with them; the
// record is restored when that fails.
func (m *Manager) applyContainerSettingsLocked(old, a App, changes []string) (SettingsApplyResult, error) {
	id := a.ID
	m.data.Apps[id] = a
	effect := containerSettingsSubject(a).classify(changes)
	result := settingsResult(effect, changes)
	var err error
	switch {
	case effect == effectRestart && a.Current != nil && !a.Stopped:
		// The current image is started again with the saved settings: the
		// new container takes traffic once healthy (Restart does the same).
		var d Deployment
		if d, err = m.queueLocked(a, DeployRequest{}, true, changes); err == nil {
			result = withDeployment(SettingsApplyResult{Applied: SettingsAppliedRestart, Message: "Settings saved. Restarting the application with them.", Changes: changes}, d)
		}
	default:
		err = m.save()
		switch {
		case effect < effectRestart:
		case a.Stopped:
			result.Applied, result.Message = SettingsAppliedImmediate, "Settings saved. The application is stopped; starting it applies them."
		case a.Current == nil && effect == effectRestart:
			result.Applied, result.Message = SettingsAppliedImmediate, "Settings saved. The first deployment uses them."
		}
	}
	if err != nil {
		m.data.Apps[id] = old
		return SettingsApplyResult{}, err
	}
	return result, nil
}
