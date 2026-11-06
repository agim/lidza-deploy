package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Retire revokes routing and certificate authorization before removing releases.
// A durable tombstone prevents concurrent deployments and lets cleanup be retried.
// The application's data stays as a retained resource.
func (m *Manager) Retire(ctx context.Context, id string) error {
	return m.RetireWith(ctx, id, AppRemoveRequest{})
}

// RetireWith is Retire with the removal's options: the move the
// application belonged to, recorded on its retained resource, and whether
// a never-switched move's copy is deleted with it.
func (m *Manager) RetireWith(ctx context.Context, id string, req AppRemoveRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	key, err := m.retire(ctx, id, req.Origin)
	if err != nil || key == "" || !req.DeleteData {
		return err
	}
	if err := m.removeRetainedCopy(ctx, key); err != nil {
		return fmt.Errorf("the application was removed, but deleting the data copied by move %s failed: %w; it stays as a retained resource", req.Origin.MoveID, err)
	}
	return nil
}

// retire removes the application and returns the key of the retained
// resource its data became, or "" when it had none.
func (m *Manager) retire(ctx context.Context, id string, origin *RetainedOrigin) (string, error) {
	m.mu.Lock()
	a, ok := m.data.Apps[id]
	if !ok {
		m.mu.Unlock()
		return "", nil
	}
	if m.removing[id] {
		m.mu.Unlock()
		return "", errors.New("application cleanup is already running")
	}
	for _, dbID := range a.Bindings {
		if d, ok := m.data.Databases[dbID]; ok && d.Operation != "" {
			m.mu.Unlock()
			return "", errors.New("wait for the database operation to finish")
		}
	}
	for _, t := range m.data.Tasks {
		if t.AppID == id && t.Running && t.Mode == "schedule" {
			m.mu.Unlock()
			return "", errors.New("wait for scheduled command to finish")
		}
	}
	if m.busy(id) || a.Restoring || m.data.Caches[id].Operation {
		m.mu.Unlock()
		return "", errors.New("application has an active deployment; retry removal after it finishes")
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
			return "", err
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
				return "", errors.New("application disabled; release cleanup failed, retry removal")
			}
		}
	}
	for _, task := range m.taskList(id) {
		if task.Container != "" {
			if _, err := command(cleanup, "", nil, "docker", "rm", "-f", task.Container); err != nil {
				return "", errors.New("task cleanup failed; retry removal")
			}
		}
	}
	if m.serviceTLS != nil {
		if err := m.serviceTLS.retire(a.resourceID()); err != nil {
			return "", errors.New("application disabled; copied service certificates could not be removed, retry removal")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	before := m.resourceSnapshotLocked()
	for key, t := range m.data.Tasks {
		if t.AppID == id {
			delete(m.data.Tasks, key)
		}
	}
	gatewayID := m.data.Apps[id].resourceID()
	extras := retainedExtras{writesStopped: time.Now().UTC(), origin: origin}
	if live := m.data.Apps[id]; live.Maintenance.Enabled && !live.Maintenance.Since.IsZero() {
		extras.writesStopped = live.Maintenance.Since
	}
	candidates := m.appDatabaseIDsLocked(m.data.Apps[id])
	delete(m.data.GatewayPKI, gatewayID)
	delete(m.data.Apps, id)
	delete(m.data.StackSelections, id)
	if m.data.RetiredAppIDs == nil {
		m.data.RetiredAppIDs = map[string]bool{}
	}
	m.data.RetiredAppIDs[id] = true
	for _, dbID := range candidates {
		if !m.databaseInUseLocked(dbID) {
			extras.databases = append(extras.databases, dbID)
		}
	}
	key := m.archiveResourcesLocked(id, extras)
	if err := m.save(); err != nil {
		m.data = before
		return "", err
	}
	m.gatewayMu.Lock()
	if client := m.gatewayClients[gatewayID]; client != nil {
		client.CloseIdleConnections()
		delete(m.gatewayClients, gatewayID)
	}
	delete(m.gatewayKeys, gatewayID)
	m.gatewayMu.Unlock()
	return key, nil
}
