package agent

import (
	"context"
	"errors"
	"github.com/agim/lidza/packs/storage"
	"maps"
	"strings"
)

type DatabaseAttachment struct {
	AppID  string `json:"app_id"`
	EnvKey string `json:"env_key"`
}

func validDatabaseKey(key string) bool {
	return len(key) <= 128 && envPattern.MatchString(key) && (key == "DATABASE_URL" || strings.HasSuffix(key, "_DATABASE_URL")) && !strings.HasPrefix(key, "LIDZA_")
}
func (m *Manager) attachmentsLocked(id string) []DatabaseAttachment {
	out := []DatabaseAttachment{}
	for _, a := range m.data.Apps {
		for key, db := range a.Bindings {
			if db == id {
				out = append(out, DatabaseAttachment{a.ID, key})
			}
		}
	}
	return out
}
func (m *Manager) bindDatabaseLocked(appID, key, id string) error {
	a := m.data.Apps[appID]
	d, ok := m.data.Databases[id]
	if !ok || !d.Ready || d.Operation != "" {
		return errors.New("database unavailable or busy")
	}
	if !validDatabaseKey(key) {
		return errors.New("invalid database variable")
	}
	old := a
	a.Bindings = maps.Clone(a.Bindings)
	if a.Bindings == nil {
		a.Bindings = map[string]string{}
	}
	a.Bindings[key] = id
	a.Env = maps.Clone(a.Env)
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	a.Env[key] = d.URL
	if err := a.Validate(); err != nil {
		return err
	}
	m.data.Apps[appID] = a
	var err error
	if a.Current != nil {
		_, err = m.queueLocked(a, DeployRequest{}, true)
	} else {
		err = m.save()
	}
	if err != nil {
		m.data.Apps[appID] = old
	}
	return err
}

// Backups gate both deploys and runtime reloads. A failed copy never activates
// a candidate. Include the previous primary database when switching connections.
func (m *Manager) backupBeforeRelease(ctx context.Context, ids []string) error {
	for _, id := range ids {
		m.mu.Lock()
		d, ok := m.data.Databases[id]
		if !ok || !d.Ready || d.Operation != "" {
			m.mu.Unlock()
			return errors.New("pre-deployment database backup unavailable or busy")
		}
		d.Operation = "backup"
		m.data.Databases[id] = d
		if err := m.save(); err != nil {
			d.Operation = ""
			m.data.Databases[id] = d
			m.mu.Unlock()
			return err
		}
		var target *storage.Config
		if m.data.BackupStorage != nil {
			cfg := *m.data.BackupStorage
			target = &cfg
		}
		m.mu.Unlock()
		var record BackupRecord
		var err error
		select {
		case m.databaseSlots <- struct{}{}:
			record, err = m.dumpDatabase(ctx, d, target, predeploymentBackup)
			<-m.databaseSlots
		case <-ctx.Done():
			err = ctx.Err()
		}
		m.mu.Lock()
		d = m.data.Databases[id]
		d.Operation = ""
		d.Event = newID()
		if record.ID != "" {
			d.Backups = append(d.Backups, record)
		}
		if err != nil {
			d.Error = "pre-deployment backup failed: " + err.Error()
		} else {
			d.Error = ""
		}
		m.data.Databases[id] = d
		saveErr := m.save()
		if record.ID != "" && saveErr == nil {
			saveErr = m.pruneBackupsLocked(id)
		}
		m.mu.Unlock()
		if err != nil {
			return errors.New("pre-deployment backup failed; current release retained")
		}
		if saveErr != nil {
			return errors.New("pre-deployment backup could not be finalized; current release retained")
		}
	}
	return nil
}
