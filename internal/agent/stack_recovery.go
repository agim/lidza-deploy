package agent

import (
	"context"
	"errors"
	"maps"
	"time"
)

type StackTransition struct {
	Previous  *Release `json:"previous,omitempty"`
	Candidate *Release `json:"candidate"`
	Reuse     bool     `json:"reuse,omitempty"`
}

func (m *Manager) recordStackTransition(id string, previous, candidate *Release, reuse bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, exists := m.data.StackTransitions[id]
	if candidate == nil {
		delete(m.data.StackTransitions, id)
	} else {
		m.data.StackTransitions[id] = StackTransition{previous, candidate, reuse}
	}
	if err := m.save(); err != nil {
		if exists {
			m.data.StackTransitions[id] = old
		} else {
			delete(m.data.StackTransitions, id)
		}
		return err
	}
	return nil
}
func (m *Manager) recoverStackTransitions() error {
	d, ok := m.runtime.(*Docker)
	if !ok {
		return nil
	}
	m.mu.Lock()
	pending := maps.Clone(m.data.StackTransitions)
	m.mu.Unlock()
	for id, t := range pending {
		m.mu.Lock()
		a := m.data.Apps[id]
		m.mu.Unlock()
		if t.Candidate == nil {
			return errors.New("invalid pending stack transition")
		}
		if a.Current != nil && a.Current.ID == t.Candidate.ID {
			if err := m.recordStackTransition(id, nil, nil, false); err != nil {
				return err
			}
			continue
		}
		ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
		var err error
		if t.Reuse {
			err = d.setReleaseRunning(ctx, t.Candidate, false)
		} else {
			err = d.Remove(ctx, t.Candidate)
		}
		if err == nil && t.Previous != nil && a.ID != "" && !a.Stopped && !a.Retiring {
			err = d.setReleaseRunning(ctx, t.Previous, true)
			if err == nil && t.Previous.Stack != nil {
				var restored *Release
				restored, err = d.refreshStackPorts(ctx, t.Previous)
				if err == nil {
					err = d.waitStackReady(ctx, restored)
				}
				if err == nil {
					m.mu.Lock()
					current := m.data.Apps[id]
					if current.Current != nil && current.Current.ID == restored.ID {
						current.Current = restored
						m.data.Apps[id] = current
					}
					m.mu.Unlock()
				}
			}
		}
		cancel()
		if err != nil {
			return errors.New("interrupted stack activation could not recover; pending recovery retained")
		}
		if err = m.recordStackTransition(id, nil, nil, false); err != nil {
			return err
		}
	}
	return nil
}

// Caller holds the manager lock, preventing competing app lifecycle operations.
func (m *Manager) rollbackStackLocked(ctx context.Context, a App) error {
	d, ok := m.runtime.(*Docker)
	if !ok {
		return errors.New("runtime does not support stack rollback")
	}
	_, ok = profileByID(a.Previous.Stack.Profile)
	if !ok {
		return errors.New("previous stack has an unknown resource profile")
	}
	if err := validateRollbackSelection(ctx, a); err != nil {
		return err
	}
	if a.Previous.Stack.Manifest.Version == 2 {
		previousApp := a
		previousApp.RuntimeProfile = a.Previous.Stack.Profile
		previousApp.DatabaseBudgets = nil
		seen := map[string]bool{}
		for _, id := range a.Previous.DatabaseIDs {
			db, exists := m.data.Databases[id]
			if !exists || !db.Ready || db.Operation != "" {
				return errors.New("previous release database attachment is unavailable or busy")
			}
			if db.Mode == "local" && !seen[id] {
				previousApp.DatabaseBudgets = append(previousApp.DatabaseBudgets, databaseResourceBudget(db))
				seen[id] = true
			}
		}
		if err := a.Previous.Stack.Manifest.validate(); err != nil {
			return err
		}
		if err := a.Previous.Stack.Manifest.validatePlan(previousApp, a.Previous.Stack.Selection); err != nil {
			return err
		}
	}
	m.data.StackTransitions[a.ID] = StackTransition{Previous: a.Current, Candidate: a.Previous, Reuse: true}
	if err := m.save(); err != nil {
		delete(m.data.StackTransitions, a.ID)
		return err
	}
	restore := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = d.setReleaseRunning(cleanup, a.Previous, false)
		if d.setReleaseRunning(cleanup, a.Current, true) == nil {
			delete(m.data.StackTransitions, a.ID)
			_ = m.save()
		}
	}
	if err := d.setReleaseRunning(ctx, a.Current, false); err != nil {
		restore()
		return err
	}
	if err := d.setReleaseRunning(ctx, a.Previous, true); err != nil {
		restore()
		return err
	}
	previous, err := d.refreshStackPorts(ctx, a.Previous)
	if err != nil {
		restore()
		return err
	}
	if err = d.waitStackReady(ctx, previous); err != nil {
		restore()
		return err
	}
	old := a
	a.Current, a.Previous = previous, a.Current
	m.data.Apps[a.ID] = a
	delete(m.data.StackTransitions, a.ID)
	if err = m.save(); err != nil {
		m.data.Apps[a.ID] = old
		m.data.StackTransitions[a.ID] = StackTransition{Previous: old.Current, Candidate: old.Previous, Reuse: true}
		a = old
		restore()
		return err
	}
	return nil
}
func (d *Docker) refreshStackPorts(ctx context.Context, r *Release) (*Release, error) {
	next := *r
	stack := *r.Stack
	next.Stack = &stack
	stack.Routes = append([]StackRoute{}, r.Stack.Routes...)
	var err error
	next.Port, err = d.stackPublishedPort(ctx, r.Container, 3000)
	if err != nil {
		return nil, err
	}
	if stack.Gateway != nil {
		copy := *stack.Gateway
		stack.Gateway = &copy
		stack.Gateway.Port, err = d.stackPublishedPort(ctx, r.Container, 3001)
		if err != nil {
			return nil, err
		}
	}
	for i, route := range stack.Routes {
		stack.Routes[i].Published, err = d.stackPublishedPort(ctx, r.Container, route.Port)
		if err != nil {
			return nil, err
		}
	}
	return &next, nil
}
func stackRoutesEqual(a, b *Release) bool {
	if (a.Stack.Gateway == nil) != (b.Stack.Gateway == nil) {
		return false
	}
	if a.Stack.Gateway != nil && *a.Stack.Gateway != *b.Stack.Gateway {
		return false
	}
	if a.Port != b.Port || len(a.Stack.Routes) != len(b.Stack.Routes) {
		return false
	}
	for i, r := range a.Stack.Routes {
		if r != b.Stack.Routes[i] {
			return false
		}
	}
	return true
}
