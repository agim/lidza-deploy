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
	if m.busy(id) {
		m.mu.Unlock()
		return errors.New("application has an active deployment; retry removal after it finishes")
	}
	if !a.Retiring {
		old := a
		a.Retiring = true
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
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data.Apps, id)
	if err := m.save(); err != nil {
		m.data.Apps[id] = a
		return err
	}
	return nil
}
