package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza/packs/jobs"
)

type MaintenancePolicy struct {
	Enabled       bool `json:"enabled"`
	IntervalHours int  `json:"interval_hours"`
	BuildCache    bool `json:"build_cache"`
	Logs          bool `json:"logs"`
	OlderThanDays int  `json:"older_than_days"`
}
type CleanupRequest struct {
	Backups    bool `json:"backups"`
	BuildCache bool `json:"build_cache"`
	Logs       bool `json:"logs"`
}
type MaintenanceState struct {
	Policy            MaintenancePolicy `json:"policy"`
	Running           bool              `json:"running"`
	NextRun           time.Time         `json:"next_run"`
	NextBackupCleanup time.Time         `json:"next_backup_cleanup"`
	LastRun           time.Time         `json:"last_run"`
	Messages          []string          `json:"messages,omitempty"`
	Error             string            `json:"error,omitempty"`
}

func defaultMaintenancePolicy() MaintenancePolicy {
	return MaintenancePolicy{Enabled: true, IntervalHours: 24, BuildCache: true, Logs: true, OlderThanDays: 7}
}
func (p MaintenancePolicy) Validate() error {
	if p.IntervalHours != 1 && p.IntervalHours != 6 && p.IntervalHours != 24 && p.IntervalHours != 168 {
		return errors.New("choose hourly, every six hours, daily or weekly cleanup")
	}
	if p.OlderThanDays < 1 || p.OlderThanDays > 365 {
		return errors.New("cleanup age must be between 1 and 365 days")
	}
	return nil
}
func (m *Manager) maintenanceStatus() MaintenanceState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := *m.data.Maintenance
	out.Messages = slices.Clone(out.Messages)
	return out
}
func (m *Manager) setMaintenancePolicy(p MaintenancePolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old := *m.data.Maintenance
	m.data.Maintenance.Policy = p
	m.data.Maintenance.NextRun = jobs.Every(time.Duration(p.IntervalHours) * time.Hour).Next(time.Now().UTC())
	if err := m.save(); err != nil {
		*m.data.Maintenance = old
		return err
	}
	return nil
}
func (m *Manager) cleanupBusyLocked() bool {
	if len(m.data.StackTransitions) > 0 {
		return true
	}
	if m.upgradePending() {
		return true
	}
	for _, d := range m.data.Deployments {
		if d.Status == "queued" || d.Status == "building" {
			return true
		}
	}
	for _, d := range m.data.Databases {
		if d.Operation != "" {
			return true
		}
	}
	return false
}

// Runs independently of the control panel. Framework schedules calculate the next
// due time; persisted deadlines coalesce missed runs after a host restart.
// StartMaintenance enables host-wide maintenance in the serving agent process.
// Merely constructing a manager (for inspection or integration fixtures) must
// never invoke host-wide Docker pruning or journal vacuuming.
func (m *Manager) StartMaintenance() {
	m.maintenanceOnce.Do(func() { m.wg.Add(1); go m.maintenanceLoop() })
}

func (m *Manager) maintenanceLoop() {
	defer m.wg.Done()
	m.maintenanceTick(time.Now().UTC())
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.maintenanceTick(now.UTC())
		}
	}
}
func (m *Manager) maintenanceTick(now time.Time) {
	m.mu.Lock()
	state := *m.data.Maintenance
	request := CleanupRequest{Backups: !state.NextBackupCleanup.After(now)}
	if state.Policy.Enabled && !state.NextRun.After(now) {
		request.BuildCache = state.Policy.BuildCache
		request.Logs = state.Policy.Logs
	}
	m.mu.Unlock()
	if !request.Backups && !request.BuildCache && !request.Logs {
		return
	}
	if err := m.startMaintenance(request, now); err != nil {
		log.Printf("server maintenance deferred: %v", err)
	}
}
func (m *Manager) startMaintenance(request CleanupRequest, now time.Time) error {
	if !request.Backups && !request.BuildCache && !request.Logs {
		return errors.New("select at least one cleanup category")
	}
	m.mu.Lock()
	if m.data.Maintenance.Running {
		m.mu.Unlock()
		return errors.New("cleanup is already running")
	}
	if m.cleanupBusyLocked() {
		m.mu.Unlock()
		return errors.New("cleanup waits for deployments, backups, restores and upgrades to finish")
	}
	previous := *m.data.Maintenance
	m.data.Maintenance.Running = true
	m.data.Maintenance.Error = ""
	m.data.Maintenance.Messages = nil
	if err := m.save(); err != nil {
		*m.data.Maintenance = previous
		m.mu.Unlock()
		return err
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
		defer cancel()
		messages, err := m.performMaintenance(ctx, request)
		m.mu.Lock()
		defer m.mu.Unlock()
		s := m.data.Maintenance
		s.Running = false
		s.LastRun = time.Now().UTC()
		s.Messages = messages
		if err != nil {
			s.Error = err.Error()
		} else {
			s.Error = ""
		}
		// Failures remain due and retry on the next minute, without losing records.
		if err == nil {
			if request.Backups {
				s.NextBackupCleanup = jobs.Every(time.Hour).Next(now)
			}
			if request.BuildCache || request.Logs {
				s.NextRun = jobs.Every(time.Duration(s.Policy.IntervalHours) * time.Hour).Next(now)
			}
		}
		if saveErr := m.save(); saveErr != nil {
			s.Error = "Cleanup completed but its status could not be saved; inspect before retrying"
			log.Printf("maintenance status: %v", saveErr)
		}
	}()
	return nil
}
func (m *Manager) performMaintenance(ctx context.Context, request CleanupRequest) ([]string, error) {
	var messages []string
	var failures []error
	if request.Backups {
		m.mu.Lock()
		removed := 0
		for id, d := range m.data.Databases {
			if d.Operation != "" {
				failures = append(failures, fmt.Errorf("database %s is busy; local cleanup deferred", id))
				continue
			}
			before := len(d.Backups)
			if err := m.pruneBackupsLocked(id); err != nil {
				failures = append(failures, fmt.Errorf("database %s: %w", id, err))
			}
			removed += before - len(m.data.Databases[id].Backups)
		}
		m.mu.Unlock()
		messages = append(messages, fmt.Sprintf("Removed %d older local backup records; newest usable dump per database retained. S3 objects unchanged.", removed))
	}
	m.mu.Lock()
	policy := m.data.Maintenance.Policy
	runner := m.maintenanceRun
	m.mu.Unlock()
	if runner == nil {
		runner = maintenanceCommand
	}
	run := func(name string, args ...string) {
		output, err := runner(ctx, name, args...)
		output = strings.TrimSpace(output)
		if len(output) > 4096 {
			output = output[:4096] + "…"
		}
		if output != "" {
			messages = append(messages, output)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s cleanup failed: %w", name, err))
		}
	}
	if request.BuildCache {
		run("docker", "builder", "prune", "--all", "--force", "--filter", "until="+strconv.Itoa(policy.OlderThanDays*24)+"h")
	}
	if request.Logs {
		// Use journald's supported rotation/vacuum API. Never truncate live Docker
		// JSON log files or remove arbitrary files under /var/log.
		run("journalctl", "--rotate")
		run("journalctl", "--vacuum-time="+strconv.Itoa(policy.OlderThanDays)+"d")
	}
	return messages, errors.Join(failures...)
}
func maintenanceCommand(ctx context.Context, name string, args ...string) (string, error) {
	// Bounded output avoids buffering a daemon's unbounded response in the agent.
	out := &cappedConsoleBuffer{}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	return out.String(), err
}
func (m *Manager) maintenanceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/maintenance", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, m.maintenanceStatus()) })
	mux.HandleFunc("PATCH /v1/maintenance", func(w http.ResponseWriter, r *http.Request) {
		var p MaintenancePolicy
		if err := Decode(w, r, &p); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setMaintenancePolicy(p); err != nil {
			Fail(w, 400, err)
			return
		}
		JSON(w, 200, m.maintenanceStatus())
	})
	mux.HandleFunc("POST /v1/maintenance/run", func(w http.ResponseWriter, r *http.Request) {
		var input CleanupRequest
		if err := Decode(w, r, &input); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.startMaintenance(input, time.Now().UTC()); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, m.maintenanceStatus())
	})
}
