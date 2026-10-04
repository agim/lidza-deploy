package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/agim/lidza/pkg/credentials"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	reload     bool
	backupIDs  []string
	app        App
	deployment string
	token      string
}
type Manager struct {
	cpuTotal, cpuIdle float64
	domainWake        chan struct{}
	domains           map[string]DomainStatus
	databaseSlots     chan struct{}
	mu                sync.Mutex
	removing          map[string]bool
	key               []byte
	cfg               Config
	data              diskState
	runtime           Runtime
	queue             chan job
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
}

func NewManager(parent context.Context, cfg Config, rt Runtime) (*Manager, error) {
	ctx, cancel := context.WithCancel(parent)
	m := &Manager{domains: map[string]DomainStatus{}, domainWake: make(chan struct{}, 1), databaseSlots: make(chan struct{}, 2), cfg: cfg, runtime: rt, queue: make(chan job, 16), ctx: ctx, cancel: cancel, data: diskState{Apps: map[string]App{}}}
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
	if m.data.Tasks == nil {
		m.data.Tasks = map[string]Task{}
	}
	for key, t := range m.data.Tasks {
		if t.Running && t.Mode == "schedule" {
			t.Running = false
			t.Error = "agent restarted during command; inspect before retry"
			m.data.Tasks[key] = t
		}
	}
	if m.data.Databases == nil {
		m.data.Databases = map[string]Database{}
	}
	for id, d := range m.data.Databases {
		if d.Operation != "" {
			d.Operation = ""
			d.Error = "agent restarted during database operation; retry"
			d.Event = newID()
			m.data.Databases[id] = d
		}
	}
	for id, a := range m.data.Apps {
		if a.Restoring {
			a.Restoring = false
			m.data.Apps[id] = a
		}
		if a.Bindings == nil {
			a.Bindings = map[string]string{}
			if _, ok := m.data.Databases[id]; ok && m.data.BindingsVersion == 0 {
				a.Bindings["DATABASE_URL"] = id
			}
			if a.Current != nil && a.Current.DatabaseIDs == nil {
				copy := *a.Current
				copy.DatabaseIDs = slices.Compact(slices.Sorted(maps.Values(a.Bindings)))
				a.Current = &copy
			}
			m.data.Apps[id] = a
		}
	}
	m.data.BindingsVersion = 1
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
	if cfg.TLSListen != "" {
		m.wg.Add(1)
		go m.domainLoop()
	}
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
		a.DomainStatus = nil
		if m.cfg.TLSListen != "" {
			status, ok := m.domains[a.ID]
			if !ok || status.Domain != a.Domain {
				status = DomainStatus{Domain: a.Domain, State: "waiting_dns"}
			}
			a.DomainStatus = &status
		}
		out = append(out, a)
	}
	return out
}
func (m *Manager) Deployments() []Deployment {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]Deployment{}, m.data.Deployments...)
	for i := range out {
		out[i].Duration = deploymentDuration(out[i])
	}
	return out
}
func (m *Manager) Upsert(a App) error {
	if err := a.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy(a.ID) || m.data.Apps[a.ID].Restoring {
		return errors.New("application has an active deployment")
	}
	old, exists := m.data.Apps[a.ID]
	if _, retained := m.data.Databases[a.ID]; retained && !exists {
		return errors.New("application ID is reserved by a database resource")
	}
	for key := range old.Bindings {
		if a.Env != nil && a.Env[key] != old.Env[key] {
			return errors.New("database variables are managed by database attachments")
		}
	}

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
	a.DomainStatus = nil
	a.Restoring = old.Restoring
	a.Maintenance = old.Maintenance
	a.Bindings = old.Bindings
	if exists {
		a.BackupBeforeDeploy = old.BackupBeforeDeploy
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
		return Deployment{}, errors.New("application unavailable")
	}
	return m.queueLocked(a, req, false)
}
func (m *Manager) queueLocked(a App, req DeployRequest, reload bool, changes ...[]string) (Deployment, error) {
	if m.upgradePending() {
		return Deployment{}, errors.New("agent upgrade in progress")
	}
	if a.Restoring {
		return Deployment{}, errors.New("wait for database restore")
	}
	if len(req.Key) > 200 || len(req.Token) > 4096 {
		return Deployment{}, errors.New("request exceeds limit")
	}
	if req.Key != "" {
		for _, d := range m.data.Deployments {
			if d.AppID == a.ID && d.Key == req.Key {
				return d, nil
			}
		}
	}
	if reload && m.busy(a.ID) {
		return Deployment{}, errors.New("application has an active deployment or reload")
	}
	if reload && a.Current == nil {
		return Deployment{}, nil
	}
	if reload {
		if _, ok := m.runtime.(interface {
			Reload(context.Context, App, string) (*Release, error)
		}); !ok {
			return Deployment{}, errors.New("runtime does not support reload")
		}
	}
	var ids []string
	networks := map[string]bool{}
	for _, id := range a.Bindings {
		d, ok := m.data.Databases[id]
		if !ok || !d.Ready || d.Operation != "" {
			return Deployment{}, errors.New("attached database not ready or busy")
		}
		ids = append(ids, id)
		if d.Network != "" {
			networks[d.Network] = true
		}
	}
	if a.Current != nil {
		ids = append(ids, a.Current.DatabaseIDs...)
	}
	a.Networks = slices.Sorted(maps.Keys(networks))
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(m.queue) == cap(m.queue) {
		return Deployment{}, errors.New("deployment queue full; retry later")
	}
	d := Deployment{ID: newID(), AppID: a.ID, Status: "queued", Created: time.Now().UTC(), Key: req.Key, Kind: "deploy", Branch: a.Branch, Domain: a.Domain, Changes: changeKeys(m.data.Apps[a.ID], a)}
	if len(changes) > 0 {
		d.Changes = slices.Clone(changes[0])
	}
	if reload {
		d.Kind = "reload"
	}
	before := slices.Clone(m.data.Deployments)
	m.data.Deployments = append(m.data.Deployments, d)
	if len(m.data.Deployments) > 500 {
		m.data.Deployments = m.data.Deployments[1:]
	}
	if err := m.save(); err != nil {
		m.data.Deployments = before
		return Deployment{}, err
	}
	m.queue <- job{app: a, deployment: d.ID, token: req.Token, reload: reload, backupIDs: ids}
	return d, nil
}
func (m *Manager) Reload(id string) (Deployment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok || a.Retiring {
		return Deployment{}, errors.New("application unavailable")
	}
	if a.Current == nil {
		return Deployment{}, errors.New("deploy the application before reloading")
	}
	return m.queueLocked(a, DeployRequest{}, true)
}
func (m *Manager) update(id, status string, release *Release, err error) error {
	for i := range m.data.Deployments {
		d := &m.data.Deployments[i]
		if d.ID != id {
			continue
		}
		d.Status = status
		if status == "building" {
			now := time.Now().UTC()
			d.Started = &now
		}
		if release != nil {
			d.Commit = release.Commit
			d.CommitMessage = release.CommitMessage
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
			ctx, cancel := context.WithTimeout(m.ctx, 2*time.Hour)
			ctx = m.deploymentDiagnostics(ctx, j)
			var release *Release
			if j.app.BackupBeforeDeploy == nil || *j.app.BackupBeforeDeploy {
				err = m.backupBeforeRelease(ctx, j.backupIDs)
			}
			if err == nil {
				runCtx, stopRun := context.WithTimeout(ctx, 20*time.Minute)
				if j.reload {
					release, err = m.runtime.(interface {
						Reload(context.Context, App, string) (*Release, error)
					}).Reload(runCtx, j.app, j.deployment)
				} else {
					release, err = m.runtime.Deploy(runCtx, j.app, j.deployment, j.token)
				}
				stopRun()
			}
			if release != nil {
				release.DatabaseIDs = slices.Compact(slices.Sorted(maps.Values(j.app.Bindings)))
			}
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
			if err == nil {
				m.wakeDomains()
			}
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
