package agent

import (
	"context"
	"errors"
	"time"
)

// Retire revokes routing and certificate authorization before removing releases.
// A durable tombstone prevents concurrent deployments and lets cleanup be retried.
func (m *Manager) Retire(ctx context.Context, id string) error {
	m.mu.Lock()
	a, ok := m.data.Apps[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	if m.removing[id] {
		m.mu.Unlock()
		return errors.New("application cleanup is already running")
	}
	for _, dbID := range a.Bindings {
		if d, ok := m.data.Databases[dbID]; ok && d.Operation != "" {
			m.mu.Unlock()
			return errors.New("wait for the database operation to finish")
		}
	}
	for _, t := range m.data.Tasks {
		if t.AppID == id && t.Running && t.Mode == "schedule" {
			m.mu.Unlock()
			return errors.New("wait for scheduled command to finish")
		}
	}
	if m.busy(id) || a.Restoring {
		m.mu.Unlock()
		return errors.New("application has an active deployment; retry removal after it finishes")
	}
	if !a.Retiring {
		old := a
		a.Retiring = true
		for key, t := range m.data.Tasks {
			if t.AppID == id {
				t.Enabled = false
				t.RunKey = newID()
				m.data.Tasks[key] = t
			}
		}
		m.data.Apps[id] = a
		if err := m.save(); err != nil {
			m.data.Apps[id] = old
			m.mu.Unlock()
			return err
		}
	}
	if m.removing == nil {
		m.removing = map[string]bool{}
	}
	m.removing[id] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.removing, id); m.mu.Unlock() }()
	cleanup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for _, release := range []*Release{a.Current, a.Previous} {
		if release != nil {
			if err := m.runtime.Remove(cleanup, release); err != nil {
				return errors.New("application disabled; release cleanup failed, retry removal")
			}
		}
	}
	for _, task := range m.taskList(id) {
		if task.Container != "" {
			if _, err := command(cleanup, "", nil, "docker", "rm", "-f", task.Container); err != nil {
				return errors.New("task cleanup failed; retry removal")
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, t := range m.data.Tasks {
		if t.AppID == id {
			delete(m.data.Tasks, key)
		}
	}
	delete(m.data.Apps, id)
	if err := m.save(); err != nil {
		m.data.Apps[id] = a
		return err
	}
	return nil
}
