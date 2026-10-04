package agent

import (
	"errors"
	"maps"
	"slices"
)

// Environment values are write-only over the settings API. Omitted keys survive;
// null deletes a key, including secrets that the browser has never received.
type SettingsPatch struct {
	Branch     string             `json:"branch"`
	Domain     string             `json:"domain"`
	EnvChanges map[string]*string `json:"env_changes"`
}
type Settings struct {
	ID      string   `json:"id"`
	Branch  string   `json:"branch"`
	Domain  string   `json:"domain"`
	EnvKeys []string `json:"env_keys"`
}

func (m *Manager) Settings(id string) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok {
		return Settings{}, errors.New("unknown application")
	}
	return Settings{ID: id, Branch: a.Branch, Domain: a.Domain, EnvKeys: slices.Sorted(maps.Keys(a.Env))}, nil
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
	if m.busy(id) {
		return errors.New("application has an active deployment")
	}
	a := old
	a.Branch = p.Branch
	a.Domain = p.Domain
	a.Env = maps.Clone(old.Env)
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	for k, v := range p.EnvChanges {
		if v == nil {
			delete(a.Env, k)
		} else {
			a.Env[k] = *v
		}
	}
	if err := a.Validate(); err != nil {
		return err
	}
	for otherID, other := range m.data.Apps {
		if otherID != id && other.Domain == a.Domain {
			return errors.New("domain already assigned")
		}
	}
	m.data.Apps[id] = a
	if err := m.save(); err != nil {
		m.data.Apps[id] = old
		return err
	}
	return nil
}
