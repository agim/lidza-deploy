package agent

import (
	"errors"
	"maps"
	"slices"
)

// Environment values are write-only over the settings API. Omitted keys survive;
// null deletes a key, including secrets that the browser has never received.
type SettingsPatch struct {
	BackupBeforeDeploy *bool              `json:"backup_before_deploy,omitempty"`
	Branch             string             `json:"branch"`
	Domain             string             `json:"domain"`
	EnvChanges         map[string]*string `json:"env_changes"`
}
type Settings struct {
	BackupBeforeDeploy bool              `json:"backup_before_deploy"`
	Bindings           map[string]string `json:"database_bindings"`
	ID                 string            `json:"id"`
	Branch             string            `json:"branch"`
	Domain             string            `json:"domain"`
	EnvKeys            []string          `json:"env_keys"`
}

func (m *Manager) Settings(id string) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok {
		return Settings{}, errors.New("unknown application")
	}
	return Settings{BackupBeforeDeploy: a.BackupBeforeDeploy == nil || *a.BackupBeforeDeploy, Bindings: maps.Clone(a.Bindings), ID: id, Branch: a.Branch, Domain: a.Domain, EnvKeys: slices.Sorted(maps.Keys(a.Env))}, nil
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
	if m.busy(id) || old.Restoring {
		return errors.New("application has an active deployment")
	}
	a := old
	if p.BackupBeforeDeploy != nil {
		a.BackupBeforeDeploy = p.BackupBeforeDeploy
	}
	a.Branch = p.Branch
	a.Domain = p.Domain
	a.Env = maps.Clone(old.Env)
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	for k, v := range p.EnvChanges {
		if k == "AUTH_SECRET" && (v == nil || *v == "") {
			return errors.New("AUTH_SECRET cannot be removed or cleared; supply a replacement explicitly to rotate authentication tokens")
		}
		if _, managed := a.Bindings[k]; managed {
			return errors.New("database variables are managed by database attachments")
		}
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
	var err error
	if a.Current != nil {
		_, err = m.queueLocked(a, DeployRequest{}, true, changeKeys(old, a))
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
