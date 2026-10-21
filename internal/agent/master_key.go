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

// An explicit operator action for older installations. Save without restarting
// the running app; deployment/reload will use the new key.
func (m *Manager) GenerateMasterKey(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok || a.Retiring {
		return errors.New("application unavailable")
	}
	if a.Env["LIDZA_MASTER_KEY"] != "" {
		return errors.New("application already has a master key; generation will not replace it")
	}
	if m.busy(id) || a.Restoring || m.upgradePending() {
		return errors.New("wait for the active application operation before generating a master key")
	}
	next, err := provisionMasterKey(a)
	if err != nil {
		return err
	}
	next.EnvKinds = normalizeEnvironmentKinds(next.Env, next.EnvKinds)
	if err = next.Validate(); err != nil {
		return err
	}
	m.data.Apps[id] = next
	if err = m.save(); err != nil {
		m.data.Apps[id] = a
		return err
	}
	return nil
}
