package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/agim/lidza/pkg/credentials"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Runtime interface {
	Deploy(context.Context, App, string, string) (*Release, error)
	Remove(context.Context, *Release) error
	Ready(context.Context, *Release) error
	Logs(context.Context, *Release) (string, error)
}
type job struct {
	app        App
	deployment string
	token      string
}
type Manager struct {
	mu       sync.Mutex
	removing map[string]bool
	key      []byte
	cfg      Config
	data     diskState
	runtime  Runtime
	queue    chan job
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func NewManager(parent context.Context, cfg Config, rt Runtime) (*Manager, error) {
	ctx, cancel := context.WithCancel(parent)
	m := &Manager{cfg: cfg, runtime: rt, queue: make(chan job, 16), ctx: ctx, cancel: cancel, data: diskState{Apps: map[string]App{}}}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		cancel()
		return nil, err
	}
	if _, err := credentials.Generate(cfg.DataDir); err != nil {
		cancel()
		return nil, err
	}
	key, err := credentials.Key(cfg.DataDir)
	if err != nil {
		cancel()
		return nil, err
	}
	m.key = key
	if err := m.load(); err != nil {
		cancel()
		return nil, err
	}
	if m.data.Apps == nil {
		m.data.Apps = map[string]App{}
	}
	for i := range m.data.Deployments {
		d := &m.data.Deployments[i]
		if d.Status == "queued" || d.Status == "building" {
			d.Status = "failed"
			d.Error = "agent restarted; retry deployment"
			now := time.Now().UTC()
			d.Finished = &now
		}
	}
	if err := m.save(); err != nil {
		cancel()
		return nil, err
	}
	m.wg.Add(1)
	go m.work()
	return m, nil
}
func (m *Manager) path() string { return filepath.Join(m.cfg.DataDir, "state.json") }
func (m *Manager) Close()       { m.cancel(); m.wg.Wait() }
func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (m *Manager) Apps() []App {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]App, 0, len(m.data.Apps))
	for _, a := range m.data.Apps {
		a.Env = nil
		out = append(out, a)
	}
	return out
}
func (m *Manager) Deployments() []Deployment {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Deployment{}, m.data.Deployments...)
}
func (m *Manager) Upsert(a App) error {
	if err := a.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy(a.ID) {
		return errors.New("application has an active deployment")
	}
	old, exists := m.data.Apps[a.ID]
	if old.Retiring || a.Retiring {
		return errors.New("application is being removed")
	}
	if !exists && len(m.data.Apps) >= 100 {
		return errors.New("agent application limit reached")
	}
	for id, other := range m.data.Apps {
		if id != a.ID && other.Domain == a.Domain {
			return errors.New("domain already assigned")
		}
	}
	a.Current = old.Current
	a.Previous = old.Previous
	if a.Env == nil {
		a.Env = old.Env
	}
	m.data.Apps[a.ID] = a
	if err := m.save(); err != nil {
		if exists {
			m.data.Apps[a.ID] = old
		} else {
			delete(m.data.Apps, a.ID)
		}
		return err
	}
	return nil
}
func (m *Manager) busy(id string) bool {
	for _, d := range m.data.Deployments {
		if d.AppID == id && (d.Status == "queued" || d.Status == "building") {
			return true
		}
	}
	return false
}
func (m *Manager) Enqueue(id string, req DeployRequest) (Deployment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok || a.Retiring {
		return Deployment{}, errors.New("unknown application or removal in progress")
	}
	if len(req.Key) > 200 || len(req.Token) > 4096 {
		return Deployment{}, errors.New("request exceeds limit")
	}
	if req.Key != "" {
		for _, d := range m.data.Deployments {
			if d.AppID == id && d.Key == req.Key {
				return d, nil
			}
		}
	}
	if len(m.queue) == cap(m.queue) {
		return Deployment{}, errors.New("deployment queue full; retry later")
	}
	d := Deployment{ID: newID(), AppID: id, Status: "queued", Created: time.Now().UTC(), Key: req.Key}
	before := append([]Deployment{}, m.data.Deployments...)
	m.data.Deployments = append(m.data.Deployments, d)
	if len(m.data.Deployments) > 500 {
		m.data.Deployments = m.data.Deployments[1:]
	}
	if err := m.save(); err != nil {
		m.data.Deployments = before
		return Deployment{}, err
	}
	m.queue <- job{app: a, deployment: d.ID, token: req.Token}
	return d, nil
}
func (m *Manager) update(id, status string, release *Release, err error) error {
	for i := range m.data.Deployments {
		d := &m.data.Deployments[i]
		if d.ID != id {
			continue
		}
		d.Status = status
		if release != nil {
			d.Commit = release.Commit
		}
		if err != nil {
			d.Error = err.Error()
		}
		if status == "live" || status == "failed" {
			now := time.Now().UTC()
			d.Finished = &now
		}
		break
	}
	return m.save()
}
func (m *Manager) work() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case j := <-m.queue:
			m.mu.Lock()
			err := m.update(j.deployment, "building", nil, nil)
			m.mu.Unlock()
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(m.ctx, 20*time.Minute)
			release, err := m.runtime.Deploy(ctx, j.app, j.deployment, j.token)
			j.token = ""
			m.mu.Lock()
			var retired *Release
			if err == nil {
				old := m.data.Apps[j.app.ID]
				a := old
				retired = a.Previous
				a.Previous = a.Current
				a.Current = release
				m.data.Apps[a.ID] = a
				if saveErr := m.update(j.deployment, "live", release, nil); saveErr != nil {
					m.data.Apps[a.ID] = old
					err = fmt.Errorf("persist activation: %w", saveErr)
				}
			}
			if err != nil {
				_ = m.update(j.deployment, "failed", nil, err)
			}
			m.mu.Unlock()
			cancel()
			cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
			if err != nil && release != nil {
				_ = m.runtime.Remove(cleanup, release)
			}
			if err == nil && retired != nil {
				_ = m.runtime.Remove(cleanup, retired)
			}
			stop()
		}
	}
}
func (m *Manager) Rollback(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok || a.Retiring || a.Previous == nil {
		return errors.New("no previous release")
	}
	if m.busy(id) {
		return errors.New("deployment active")
	}
	if err := m.runtime.Ready(ctx, a.Previous); err != nil {
		return fmt.Errorf("previous release is unhealthy: %w", err)
	}
	old := a
	a.Current, a.Previous = a.Previous, a.Current
	m.data.Apps[id] = a
	if err := m.save(); err != nil {
		m.data.Apps[id] = old
		return err
	}
	return nil
}
func (m *Manager) Current(id string) *Release {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.data.Apps[id]
	return a.Current
}
func (m *Manager) Target(host string) *Release {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.data.Apps {
		if a.Domain == host && !a.Retiring {
			return a.Current
		}
	}
	return nil
}
