package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Resource usage of native application processes. Every process of a native
// application is one systemd unit, so its usage is the unit's cgroup
// accounting as systemd reports it: current memory, accumulated CPU time and
// task count, plus its state, the time it entered that state and how often
// systemd restarted it. Reading it never starts, stops or connects to a
// unit, and it works on hosts without Docker.

// NativeServiceUsage is one unit of a native application with its usage.
// Values systemd does not account for (for example memory of a user unit
// without cgroup delegation) are left out rather than reported as zero.
type NativeServiceUsage struct {
	Unit             string    `json:"unit"`
	Active           string    `json:"active"`
	State            string    `json:"state"`
	UnitFileState    string    `json:"unit_file_state,omitempty"`
	Since            time.Time `json:"since,omitzero"`
	MainPID          int       `json:"main_pid,omitempty"`
	MemoryBytes      *int64    `json:"memory_bytes,omitempty"`
	MemoryLimitBytes *int64    `json:"memory_limit_bytes,omitempty"`
	CPUPercent       *float64  `json:"cpu_percent,omitempty"`
	Tasks            *int64    `json:"tasks,omitempty"`
	Restarts         int       `json:"restarts"`
	// Preserved units are kept as they are by deploys (the release policy's
	// preserve list); an operator can still restart them explicitly.
	Preserved bool `json:"preserved,omitempty"`
	// Restartable reports whether the panel may restart the unit: service
	// control is enabled for the application.
	Restartable bool `json:"restartable"`
}

// NativeProcessInventory is the reply of GET /v1/native/apps/{id}/processes.
type NativeProcessInventory struct {
	CheckedAt time.Time            `json:"checked_at"`
	Services  []NativeServiceUsage `json:"services"`
}

var nativeUsageProperties = "--property=Id,ActiveState,SubState,UnitFileState,ActiveEnterTimestamp,MainPID,MemoryCurrent,MemoryMax,CPUUsageNSec,TasksCurrent,NRestarts"

// nativeCPUSample is the last CPU reading of one unit, for the usage
// percentage between two readings.
type nativeCPUSample struct {
	nanoseconds uint64
	at          time.Time
}

var (
	nativeCPUMu      sync.Mutex
	nativeCPUSamples = map[string]nativeCPUSample{}
	// nativeUsageClock and nativeUsageResample are replaced by tests.
	nativeUsageClock    = time.Now
	nativeUsageResample = 400 * time.Millisecond
)

// nativeUsageQuery reads the usage properties of units through the
// application's service manager: the system manager for applications with
// their own account, the deployment user's manager otherwise.
func (m *Manager) nativeUsageQuery(ctx context.Context, id string, units []string) (string, error) {
	for _, unit := range units {
		if !nativeUnitPattern.MatchString(unit) {
			return "", errors.New("native services contain an invalid identity")
		}
	}
	args := []string{"systemctl", "show", nativeUsageProperties, "--timestamp=unix", "--no-pager"}
	if m.nativeCreatedSystemUnits(id) {
		return m.nativeRootExec(ctx, append(args, units...)...)
	}
	args = []string{"systemctl", "--user", "show", nativeUsageProperties, "--timestamp=unix", "--no-pager"}
	return m.nativeExec(ctx, append(args, units...)...)
}

// systemdNumber parses a systemd integer property. "[not set]", "infinity"
// and the unsigned maximum mean the value is not accounted.
func systemdNumber(value string) (uint64, bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil || n == math.MaxUint64 {
		return 0, false
	}
	return n, true
}

func systemdTimestamp(value string) time.Time {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "@") {
		if seconds, err := strconv.ParseInt(value[1:], 10, 64); err == nil && seconds > 0 {
			return time.Unix(seconds, 0).UTC()
		}
		return time.Time{}
	}
	// Older systemd ignores --timestamp=unix: "Thu 2026-11-05 10:00:00 UTC".
	for _, layout := range []string{"Mon 2006-01-02 15:04:05 MST", "Mon 2006-01-02 15:04:05 -0700"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// parseNativeUsage reads `systemctl show` blocks into usage rows, keeping
// the order of units and the CPU time of each for the percentage.
func parseNativeUsage(out string, units []string) (map[string]NativeServiceUsage, map[string]uint64) {
	rows := map[string]NativeServiceUsage{}
	cpu := map[string]uint64{}
	for _, block := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n\n") {
		var s NativeServiceUsage
		var cpuTime uint64
		cpuKnown := false
		for _, line := range strings.Split(block, "\n") {
			key, value, _ := strings.Cut(line, "=")
			switch key {
			case "Id":
				s.Unit = value
			case "ActiveState":
				s.Active = value
			case "SubState":
				s.State = value
			case "UnitFileState":
				s.UnitFileState = value
			case "ActiveEnterTimestamp":
				s.Since = systemdTimestamp(value)
			case "MainPID":
				s.MainPID, _ = strconv.Atoi(value)
			case "MemoryCurrent":
				if n, ok := systemdNumber(value); ok {
					v := int64(n)
					s.MemoryBytes = &v
				}
			case "MemoryMax":
				if n, ok := systemdNumber(value); ok {
					v := int64(n)
					s.MemoryLimitBytes = &v
				}
			case "TasksCurrent":
				if n, ok := systemdNumber(value); ok {
					v := int64(n)
					s.Tasks = &v
				}
			case "CPUUsageNSec":
				cpuTime, cpuKnown = systemdNumber(value)
			case "NRestarts":
				s.Restarts, _ = strconv.Atoi(value)
			}
		}
		if !slices.Contains(units, s.Unit) {
			continue
		}
		if s.Active != "active" {
			// A stopped unit has no live usage; systemd may still report
			// the last values of its cgroup.
			s.MemoryBytes, s.Tasks, s.MainPID, cpuKnown = nil, nil, 0, false
		}
		rows[s.Unit] = s
		if cpuKnown {
			cpu[s.Unit] = cpuTime
		}
	}
	return rows, cpu
}

// nativeCPUPercent turns two CPU-time readings into a percentage of one
// core, the scale docker stats reports.
func nativeCPUPercent(key string, nanoseconds uint64, at time.Time) (float64, bool) {
	nativeCPUMu.Lock()
	defer nativeCPUMu.Unlock()
	previous, ok := nativeCPUSamples[key]
	nativeCPUSamples[key] = nativeCPUSample{nanoseconds, at}
	elapsed := at.Sub(previous.at)
	if !ok || elapsed <= 0 || nanoseconds < previous.nanoseconds {
		return 0, false
	}
	return 100 * float64(nanoseconds-previous.nanoseconds) / float64(elapsed.Nanoseconds()), true
}

// nativeServiceUsage samples the units of one registered application. When
// a unit has no earlier reading, it reads again shortly after so the first
// view already has a CPU percentage.
func (m *Manager) nativeServiceUsage(ctx context.Context, id string) (NativeProcessInventory, error) {
	a, err := m.nativeApp(id)
	if err != nil {
		return NativeProcessInventory{}, err
	}
	inventory := NativeProcessInventory{CheckedAt: nativeUsageClock().UTC(), Services: []NativeServiceUsage{}}
	if len(a.Services) == 0 {
		return inventory, nil
	}
	preserved := []string{}
	if _, policy, err := m.nativeReleasePolicy(id); err == nil {
		preserved = policy.Preserve
	}
	_, created := m.nativeCreatedPolicy(id)
	control := created || m.cfg.Native != nil && m.cfg.Native.ServiceControl
	read := func() (map[string]NativeServiceUsage, map[string]uint64, time.Time, error) {
		out, err := m.nativeUsageQuery(ctx, id, a.Services)
		at := nativeUsageClock()
		if err != nil {
			return nil, nil, at, errors.New("native service usage unavailable")
		}
		rows, cpu := parseNativeUsage(out, a.Services)
		return rows, cpu, at, nil
	}
	rows, cpu, at, err := read()
	if err != nil {
		return inventory, err
	}
	key := func(unit string) string { return fmt.Sprintf("%p/%s", m, unit) }
	percents := map[string]float64{}
	sample := func(cpu map[string]uint64, at time.Time) bool {
		complete := true
		for unit, value := range cpu {
			if percent, ok := nativeCPUPercent(key(unit), value, at); ok {
				percents[unit] = percent
			} else {
				complete = false
			}
		}
		return complete
	}
	if !sample(cpu, at) && nativeUsageResample > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(nativeUsageResample):
			if next, nextCPU, nextAt, err := read(); err == nil {
				rows, cpu, at = next, nextCPU, nextAt
				sample(cpu, at)
			}
		}
	}
	for _, unit := range a.Services {
		row, ok := rows[unit]
		if !ok {
			return inventory, errors.New("native service usage response is incomplete")
		}
		if percent, ok := percents[unit]; ok && row.Active == "active" {
			row.CPUPercent = &percent
		}
		row.Preserved = slices.Contains(preserved, unit)
		row.Restartable = control
		inventory.Services = append(inventory.Services, row)
	}
	return inventory, nil
}

func (m *Manager) nativeUsageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/native/apps/{id}/processes", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		out, err := m.nativeServiceUsage(ctx, r.PathValue("id"))
		if err != nil {
			Fail(w, 409, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, out)
	})
}

// nativeProcessRows are the inventory rows of every registered native
// application's units, sampled concurrently. An application whose units
// cannot be read is named in the warning instead of being reported as
// stopped.
func (m *Manager) nativeProcessRows(ctx context.Context) ([]ContainerView, string) {
	m.mu.Lock()
	ids := slices.Sorted(maps.Keys(m.data.NativeApps))
	m.mu.Unlock()
	if len(ids) == 0 || m.cfg.Native == nil {
		return nil, ""
	}
	probe, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	results := make([]NativeProcessInventory, len(ids))
	failures := make([]bool, len(ids))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			inventory, err := m.nativeServiceUsage(probe, id)
			results[i], failures[i] = inventory, err != nil
		}()
	}
	wg.Wait()
	rows := []ContainerView{}
	failed := []string{}
	for i, id := range ids {
		if failures[i] {
			failed = append(failed, id)
			continue
		}
		for _, service := range results[i].Services {
			rows = append(rows, nativeProcessRow(id, service))
		}
	}
	warning := ""
	if len(failed) > 0 {
		warning = "Service state of " + strings.Join(failed, ", ") + " is unavailable; review its bindings on the server."
	}
	return rows, warning
}

// nativeProcessRow is one unit in the shape of a container row.
func nativeProcessRow(appID string, s NativeServiceUsage) ContainerView {
	role := "Service"
	if s.Preserved {
		role = "Service · preserved"
	}
	restarts := s.Restarts
	view := ContainerView{Name: s.Unit, Unit: s.Unit, AppID: appID, Role: role, Runtime: "native", State: s.Active, Status: s.State, Since: s.Since,
		CPUPercent: s.CPUPercent, MemoryBytes: s.MemoryBytes, MemoryLimitBytes: s.MemoryLimitBytes, RestartCount: &restarts, Restartable: s.Restartable}
	if s.Active == "active" && s.State == "running" {
		view.State = "running"
	}
	if s.Tasks != nil {
		view.PIDs = strconv.FormatInt(*s.Tasks, 10)
	}
	if s.MemoryBytes != nil && s.MemoryLimitBytes != nil && *s.MemoryLimitBytes > 0 {
		view.MemoryPercent = strconv.FormatFloat(100*float64(*s.MemoryBytes)/float64(*s.MemoryLimitBytes), 'f', 2, 64) + "%"
	}
	// A preserved unit is expected to be stopped; any other stopped or
	// failed unit needs attention.
	view.NeedsAttention = s.Active == "failed" || !s.Preserved && s.Active != "active" && s.UnitFileState == "enabled"
	return view
}

// nativeApplicationHealth is the health of native applications from their
// units' states, never from an HTTP probe: a connection could start a
// socket-activated service. An application with a failed unit is
// unhealthy; one whose units are all stopped is stopped, which is the
// operator's choice and no incident.
func (m *Manager) nativeApplicationHealth(ctx context.Context) []AppHealth {
	m.mu.Lock()
	ids := slices.Sorted(maps.Keys(m.data.NativeApps))
	releases := maps.Clone(m.data.NativeReleases)
	m.mu.Unlock()
	out := []AppHealth{}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		health := AppHealth{AppID: id, Release: releases[id].Current.ID, State: "unknown"}
		if _, stopped := m.nativeStopped(id); stopped {
			// Stopped from the panel: no incident, nothing to recover.
			health.State = "stopped"
			out = append(out, health)
			continue
		}
		status, err := m.nativeStatus(ctx, id)
		if err != nil {
			out = append(out, health)
			continue
		}
		if len(status.Services) == 0 {
			// Static sites have no process to fail.
			continue
		}
		health.State = "stopped"
		for _, service := range status.Services {
			if service.Active == "failed" {
				health.State = "unhealthy"
				break
			}
			if service.Active == "active" {
				health.State = "healthy"
			}
		}
		out = append(out, health)
	}
	return out
}
