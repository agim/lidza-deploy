package agent

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"
)

// Environment settings are editable; credentials stay masked unless a separate,
// purpose-bound step-up request reveals one. Omitted keys survive and null deletes.
type SettingsPatch struct {
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
	StackProfilesSupported     bool               `json:"stack_profiles_supported"`
	RuntimeProfile             string             `json:"runtime_profile"`
	StackProfiles              []StackProfile     `json:"stack_profiles"`
	AdditionalDomainsSupported bool               `json:"additional_domains_supported"`
	AdditionalDomains          []string           `json:"additional_domains"`
	DomainStatuses             []DomainStatus     `json:"domain_statuses,omitempty"`
	AppManagedStorageSupported bool               `json:"app_managed_storage_supported"`
	StorageProviderOverride    bool               `json:"storage_provider_override"`
	CustomerHosts              bool               `json:"customer_hosts"`
	Stopped                    bool               `json:"stopped"`
	DefaultsApplied            bool               `json:"defaults_applied"`
	EnvSources                 map[string]string  `json:"env_sources"`
	PersistentStorage          bool               `json:"persistent_storage"`
	Cache                      CacheView          `json:"cache"`
	BackupBeforeDeploy         bool               `json:"backup_before_deploy"`
	Bindings                   map[string]string  `json:"database_bindings"`
	ID                         string             `json:"id"`
	Branch                     string             `json:"branch"`
	Domain                     string             `json:"domain"`
	EnvKeys                    []string           `json:"env_keys"`
	Environment                []EnvironmentEntry `json:"environment"`
}

func (m *Manager) Settings(id string) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok {
		return Settings{}, errors.New("unknown application")
	}
	kinds := normalizeEnvironmentKinds(a.Env, a.EnvKinds)
	keys := slices.Sorted(maps.Keys(a.Env))
	environment := make([]EnvironmentEntry, 0, len(keys))
	for _, key := range keys {
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
	return Settings{StackProfilesSupported: true, RuntimeProfile: a.RuntimeProfile, StackProfiles: stackProfiles, AdditionalDomainsSupported: true, AdditionalDomains: slices.Clone(a.AdditionalDomains), DomainStatuses: m.domainStatusesLocked(a), AppManagedStorageSupported: true, StorageProviderOverride: a.Env["STORAGE_PROVIDER"] != "", CustomerHosts: a.CustomerHosts, Stopped: a.Stopped, DefaultsApplied: a.DefaultsApplied, EnvSources: maps.Clone(a.EnvSources), PersistentStorage: m.data.StorageVolumes[id] != "", Cache: m.cacheViewLocked(id), BackupBeforeDeploy: a.BackupBeforeDeploy == nil || *a.BackupBeforeDeploy, Bindings: maps.Clone(a.Bindings), ID: id, Branch: a.Branch, Domain: a.Domain, EnvKeys: keys, Environment: environment}, nil
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
	if normalizeEnvironmentKinds(a.Env, a.EnvKinds)[key] != EnvironmentCredential {
		return "", errors.New("environment variable is a setting and does not require reveal")
	}
	return value, nil
}
func (m *Manager) PatchSettings(id string, p SettingsPatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.data.Apps[id]
	if !ok {
		return errors.New("unknown application")
	}
	if old.Retiring {
		return errors.New("application is being removed")
	}
	if m.busy(id) || old.Restoring || m.data.Caches[id].Operation {
		return errors.New("application has an active deployment")
	}
	if p.FromDefaults {
		values := map[string]string{}
		for k, v := range p.EnvChanges {
			if v == nil {
				return errors.New("defaults cannot delete variables")
			}
			values[k] = *v
		}
		if err := ValidateAppDefaults(values); err != nil {
			return err
		}
	}
	a := old
	if p.RuntimeProfile != nil {
		a.RuntimeProfile = *p.RuntimeProfile
		if old.Current != nil && (a.RuntimeProfile == "") != (old.RuntimeProfile == "") {
			return errors.New("changing runtime family requires a new application; existing data is retained")
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
				return errors.New("ClickHouse connection is managed by the hosting agent")
			}
		}
		if k == "STORAGE_DIR" && m.data.StorageVolumes[id] != "" {
			return errors.New("STORAGE_DIR is managed by the persistent storage attachment")
		}
		if k == "CACHE_URL" && m.data.Caches[id].Mode != "" {
			return errors.New("CACHE_URL is managed through Cache settings")
		}
		if k == "AUTH_SECRET" && (v == nil || *v == "") {
			return errors.New("AUTH_SECRET cannot be removed or cleared; supply a replacement explicitly to rotate authentication tokens")
		}
		if _, managed := a.Bindings[k]; managed {
			return errors.New("database variables are managed by database attachments")
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
		if kind != EnvironmentSetting && kind != EnvironmentCredential {
			return fmt.Errorf("invalid environment classification for %q", key)
		}
		if _, ok := a.Env[key]; !ok {
			return fmt.Errorf("cannot classify missing environment variable %q", key)
		}
		a.EnvKinds[key] = kind
	}
	if err := a.Validate(); err != nil {
		return err
	}
	for otherID, other := range m.data.Apps {
		if otherID != id && a.DomainsOverlap(other) {
			return errors.New("domain already assigned")
		}
	}
	m.data.Apps[id] = a
	var err error
	changes := changeKeys(old, a)
	if a.Current != nil && !(len(changes) == 1 && changes[0] == "additional_domains") {
		_, err = m.queueLocked(a, DeployRequest{}, true, changes)
	} else {
		err = m.save()
	}
	if err != nil {
		m.data.Apps[id] = old
		return err
	}
	m.wakeDomains()
	return nil
}
