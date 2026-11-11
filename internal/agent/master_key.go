package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
)

// Called only for new app identities. Never backfill a running installation:
// its original encryption key may protect data not visible in the checkout.
func provisionMasterKey(a App) (App, error) {
	if a.isRails() || a.Runtime == "astro" {
		return a, nil
	}
	if a.Env["LIDZA_MASTER_KEY"] != "" {
		return a, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return a, errors.New("could not generate application master key")
	}
	a.Env = maps.Clone(a.Env)
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	a.Env["LIDZA_MASTER_KEY"] = hex.EncodeToString(key)
	a.EnvSources = maps.Clone(a.EnvSources)
	if a.EnvSources == nil {
		a.EnvSources = map[string]string{}
	}
	a.EnvSources["LIDZA_MASTER_KEY"] = "built-in"
	return a, nil
}

// An explicit operator action for older installations. The key is saved and
// applied like any environment change (settings_apply.go): a running
// application restarts with it.
func (m *Manager) GenerateMasterKey(id string) error {
	_, err := m.GenerateMasterKeyApply(id)
	return err
}

// GenerateMasterKeyApply generates and saves the key and says how it was applied.
func (m *Manager) GenerateMasterKeyApply(id string) (SettingsApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok || a.Retiring {
		return SettingsApplyResult{}, errors.New("application unavailable")
	}
	if a.isRails() || a.Runtime == "astro" {
		return SettingsApplyResult{}, errors.New("import the original Rails master key as RAILS_MASTER_KEY; Rails encryption keys are never generated or replaced by the agent")
	}
	if a.Env["LIDZA_MASTER_KEY"] != "" {
		return SettingsApplyResult{}, errors.New("application already has a master key; generation will not replace it")
	}
	if m.busy(id) || a.Restoring || m.upgradePending() {
		return SettingsApplyResult{}, errors.New("wait for the active application operation before generating a master key")
	}
	next, err := provisionMasterKey(a)
	if err != nil {
		return SettingsApplyResult{}, err
	}
	next.EnvKinds = normalizeEnvironmentKinds(next.Env, next.EnvKinds)
	if err = next.Validate(); err != nil {
		return SettingsApplyResult{}, err
	}
	return m.applyContainerSettingsLocked(a, next, []string{"env:LIDZA_MASTER_KEY"})
}

// MasterKeyGenerated answers a key generation: the masked entry and how the
// key was applied.
type MasterKeyGenerated struct {
	EnvironmentEntry
	Apply SettingsApplyResult `json:"apply"`
}
