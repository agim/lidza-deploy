package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"regexp"
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
	// Preview is the dry run of the next cleanup, on request only
	// (GET /v1/maintenance?preview=1); it is never stored.
	Preview *CleanupPreview `json:"preview,omitempty"`
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
	if m.cfg.IngressOnly {
		return
	}
	m.maintenanceOnce.Do(func() { m.wg.Add(1); go m.maintenanceLoop() })
}

func (m *Manager) maintenanceLoop() {
	defer m.wg.Done()
	// Transfers interrupted by the previous agent process leave their
	// temporary files behind; nothing of this process can own them yet.
	m.sweepMoveTempsNow(time.Now().UTC())
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
	if m.cfg.IngressOnly {
		return
	}
	m.hostTick(now)                // host updates, reboot window and firewall rollback (host_common.go)
	m.moveReplicationSweep(now)    // publications and slots of moves that never tore them down (move_replication.go)
	m.startClickHouseLogsTick(now) // system log retention (clickhouse_system_logs.go)
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
	if m.cfg.IngressOnly {
		return errors.New("host-wide maintenance is disabled for this agent role")
	}
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
	if m.cfg.IngressOnly {
		return nil, errors.New("host-wide maintenance is disabled for this agent role")
	}
	// Every step reports one summarised line; raw command output appears
	// only when a step fails.
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
		if removed == 0 {
			messages = append(messages, "Local backups: nothing to prune")
		} else {
			messages = append(messages, fmt.Sprintf("Local backups: removed %d older backup records; the newest usable dump of every database is kept and S3 objects are unchanged", removed))
		}
		if message := m.sweepMoveTempsNow(time.Now().UTC()); message != "" {
			messages = append(messages, "Move transfers: "+message)
		}
	}
	m.mu.Lock()
	policy := m.data.Maintenance.Policy
	runner := m.maintenanceRun
	m.mu.Unlock()
	if runner == nil {
		runner = maintenanceCommand
	}
	if request.BuildCache {
		docker := dockerCleanupAvailability(ctx)
		if docker.ok {
			output, err := runner(ctx, "docker", "builder", "prune", "--all", "--force", "--filter", "until="+strconv.Itoa(policy.OlderThanDays*24)+"h")
			messages = append(messages, summarizeBuilderPrune(output, err))
			if err != nil {
				failures = append(failures, fmt.Errorf("docker build cache cleanup failed: %w", err))
			}
		} else {
			messages = append(messages, docker.reason)
			if docker.warning {
				log.Print(docker.reason)
			}
		}
		// Old release images, stopped builders, anonymous volumes, native
		// release leftovers and the package cache (docker_hygiene.go,
		// native_hygiene.go).
		items, notes, planFailures := m.planDiskHygiene(ctx, runner, time.Now().UTC(), docker)
		for _, note := range notes {
			log.Print(note)
		}
		messages = append(messages, notes...)
		removed, removeFailures := applyCleanupItems(ctx, runner, items)
		for _, message := range removed {
			log.Print(message)
		}
		if len(items) == 0 && len(planFailures) == 0 {
			removed = append(removed, "Disk hygiene: nothing to remove")
		}
		messages = append(messages, removed...)
		failures = append(append(failures, planFailures...), removeFailures...)
	}
	if request.Logs {
		// Use journald's supported rotation/vacuum API. Never truncate live Docker
		// JSON log files or remove arbitrary files under /var/log.
		message, err := vacuumJournal(ctx, runner, policy.OlderThanDays)
		messages = append(messages, message)
		if err != nil {
			failures = append(failures, err)
		}
	}
	return messages, errors.Join(failures...)
}

// legacyBuilderNotice is Docker's deprecation banner of its legacy builder;
// it says nothing about the cleanup.
const legacyBuilderNotice = "DEPRECATED: The legacy builder is deprecated"

// withoutLegacyNotice drops Docker's legacy-builder banner and the lines
// that continue it.
func withoutLegacyNotice(output string) string {
	var kept []string
	skip := false
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, legacyBuilderNotice):
			skip = true
			continue
		case skip && (strings.HasPrefix(trimmed, "Install the buildx component") || strings.HasPrefix(trimmed, "https://docs.docker.com/go/buildx")):
			continue
		}
		skip = false
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

var reclaimedSpace = regexp.MustCompile(`(?m)^Total(?: reclaimed space)?:\s*(\S+)`)

// summarizeBuilderPrune turns `docker builder prune` output into one line.
func summarizeBuilderPrune(output string, err error) string {
	output = withoutLegacyNotice(output)
	if err != nil {
		return "Docker build cache: failed: " + boundedErrorText(output, 2000)
	}
	if match := reclaimedSpace.FindStringSubmatch(output); match != nil && match[1] != "0B" {
		return "Docker build cache: freed " + match[1]
	}
	return "Docker build cache: nothing to free"
}

var journalFreed = regexp.MustCompile(`freed ([0-9.]+)([KMGT]?)B?`)
var journalUsage = regexp.MustCompile(`take up ([0-9.]+[KMGT]?B?)`)

// vacuumJournal rotates the journal, vacuums archives older than days and
// reports one line.
func vacuumJournal(ctx context.Context, run commandRunner, days int) (string, error) {
	if out, err := run(ctx, "journalctl", "--rotate"); err != nil {
		return "Journal: rotation failed: " + boundedErrorText(strings.TrimSpace(out), 2000), fmt.Errorf("journalctl cleanup failed: %w", err)
	}
	out, err := run(ctx, "journalctl", "--vacuum-time="+strconv.Itoa(days)+"d")
	if err != nil {
		return "Journal: vacuum failed: " + boundedErrorText(strings.TrimSpace(out), 2000), fmt.Errorf("journalctl cleanup failed: %w", err)
	}
	var freed float64
	units := map[string]float64{"": 1, "K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40}
	for _, match := range journalFreed.FindAllStringSubmatch(out, -1) {
		value, _ := strconv.ParseFloat(match[1], 64)
		freed += value * units[match[2]]
	}
	if freed > 0 {
		return "Journal: freed " + bytesText(int64(freed)) + " of archives older than " + strconv.Itoa(days) + " days", nil
	}
	usage := ""
	if out, err := run(ctx, "journalctl", "--disk-usage"); err == nil {
		if match := journalUsage.FindStringSubmatch(out); match != nil {
			usage = " (" + match[1] + " in use)"
		}
	}
	return "Journal: nothing older than " + strconv.Itoa(days) + " days to vacuum" + usage, nil
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
	mux.HandleFunc("GET /v1/maintenance", func(w http.ResponseWriter, r *http.Request) {
		status := m.maintenanceStatus()
		if r.URL.Query().Get("preview") == "1" && !m.cfg.IngressOnly {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			preview := m.cleanupPreview(ctx)
			cancel()
			status.Preview = &preview
		}
		JSON(w, 200, status)
	})
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
