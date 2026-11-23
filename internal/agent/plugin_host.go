package agent

// Plugins as part of the server: their status in server health, their checks
// and repairs in server readiness, and their hourly maintenance. This is what
// lets a plugin, not the agent, know when the software it manages is unwell
// and what to do about it.

import (
	"context"
	"encoding/json"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agim/lidza-deploy/internal/plugin"
)

// PluginHealth is a plugin's last status answer.
type PluginHealth struct {
	Name     string          `json:"name"`
	Version  string          `json:"version"`
	Provides string          `json:"provides"`
	OK       bool            `json:"ok"`
	Messages []string        `json:"messages,omitempty"`
	Status   json.RawMessage `json:"status,omitempty"`
	Checked  time.Time       `json:"checked"`
}

const (
	pluginStatusInterval   = 5 * time.Minute
	pluginMaintainInterval = time.Hour
	// pluginCheckBudget bounds a plugin's check when it is part of a report
	// people wait for.
	pluginCheckBudget = 20 * time.Second
)

// pluginHostState is the agent's memory of plugin housekeeping.
type pluginHostState struct {
	mu         sync.Mutex
	health     map[string]PluginHealth
	statusAt   time.Time
	maintained map[string]time.Time
	running    bool
}

func (m *Manager) pluginHost() *pluginHostState {
	m.pluginHostOnce.Do(func() {
		m.pluginHostState = &pluginHostState{health: map[string]PluginHealth{}, maintained: map[string]time.Time{}}
	})
	return m.pluginHostState
}

// installedPlugins lists the installed plugins with their manifests.
func (m *Manager) installedPlugins() []struct {
	Record   PluginRecord
	Manifest plugin.Manifest
} {
	m.mu.Lock()
	records := make([]PluginRecord, 0, len(m.data.Plugins))
	for _, record := range m.data.Plugins {
		records = append(records, record)
	}
	m.mu.Unlock()
	slices.SortFunc(records, func(a, b PluginRecord) int { return strings.Compare(a.Name, b.Name) })
	out := []struct {
		Record   PluginRecord
		Manifest plugin.Manifest
	}{}
	for _, record := range records {
		if manifest, err := plugin.ParseManifest(record.Manifest); err == nil {
			out = append(out, struct {
				Record   PluginRecord
				Manifest plugin.Manifest
			}{record, manifest})
		}
	}
	return out
}

// pluginHealth is what server health reports: the last status of each plugin.
func (m *Manager) pluginHealth() []PluginHealth {
	state := m.pluginHost()
	state.mu.Lock()
	defer state.mu.Unlock()
	out := make([]PluginHealth, 0, len(state.health))
	for _, h := range state.health {
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b PluginHealth) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// pluginTick refreshes plugin status every few minutes and runs each
// plugin's maintain hook hourly, in the background and one run at a time.
func (m *Manager) pluginTick(now time.Time) {
	state := m.pluginHost()
	state.mu.Lock()
	if state.running || now.Sub(state.statusAt) < pluginStatusInterval {
		state.mu.Unlock()
		return
	}
	state.running, state.statusAt = true, now
	state.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			state.mu.Lock()
			state.running = false
			state.mu.Unlock()
		}()
		m.pluginHousekeeping(now)
	}()
}

func (m *Manager) pluginHousekeeping(now time.Time) {
	state := m.pluginHost()
	seen := map[string]bool{}
	for _, p := range m.installedPlugins() {
		seen[p.Record.Name] = true
		if p.Manifest.Implements("maintain") {
			state.mu.Lock()
			due := now.Sub(state.maintained[p.Record.Name]) >= pluginMaintainInterval
			if due {
				state.maintained[p.Record.Name] = now
			}
			state.mu.Unlock()
			if due {
				result, err := m.runPluginHook(m.ctx, p.Record.Name, "maintain", nil)
				switch {
				case err != nil:
					log.Printf("plugin %s: maintenance failed: %v", p.Record.Name, err)
				case len(result.Answer.Messages) > 0:
					log.Printf("plugin %s: maintenance: %s", p.Record.Name, strings.Join(result.Answer.Messages, " "))
				}
			}
		}
		health := PluginHealth{Name: p.Record.Name, Version: p.Record.Version, Provides: p.Record.Provides, OK: true, Checked: now}
		if p.Manifest.Implements("status") {
			result, err := m.runPluginHook(m.ctx, p.Record.Name, "status", nil)
			if err != nil {
				health.OK, health.Messages = false, []string{err.Error()}
			} else {
				health.OK, health.Messages, health.Status = result.Answer.OK, result.Answer.Messages, result.Answer.Output
			}
		}
		state.mu.Lock()
		state.health[p.Record.Name] = health
		state.mu.Unlock()
	}
	state.mu.Lock()
	for name := range state.health {
		if !seen[name] {
			delete(state.health, name)
			delete(state.maintained, name)
		}
	}
	state.mu.Unlock()
}

// pluginReadinessChecks asks every plugin that can check the server whether
// what it manages is ready, with no application in mind.
func (m *Manager) pluginReadinessChecks(ctx context.Context) []HostReadinessCheck {
	checks := []HostReadinessCheck{}
	for _, p := range m.installedPlugins() {
		if !p.Manifest.Implements("check") {
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, pluginCheckBudget)
		result, err := m.runPluginHook(bounded, p.Record.Name, "check", nil)
		cancel()
		if err != nil {
			checks = append(checks, HostReadinessCheck{ID: "plugin-" + p.Record.Name, Title: p.Record.Provides, Detail: "The " + p.Record.Name + " plugin could not check this server: " + err.Error() + "."})
			continue
		}
		canRepair := p.Manifest.Implements("repair")
		for _, c := range result.Answer.Checks {
			checks = append(checks, HostReadinessCheck{ID: "plugin-" + p.Record.Name + "-" + c.ID, Title: c.Title, OK: c.OK, Detail: c.Detail, Repairable: c.Repairable && canRepair, Caution: c.Caution})
		}
		if len(result.Answer.Checks) == 0 && !result.Answer.OK {
			checks = append(checks, HostReadinessCheck{ID: "plugin-" + p.Record.Name, Title: p.Record.Provides, Detail: cmpMessages(result.Answer.Messages)})
		}
	}
	return checks
}

// repairPlugins runs the repair hook of every plugin whose check reports
// something it can repair, and returns what was done and what failed.
func (m *Manager) repairPlugins(ctx context.Context) (repaired, failed []string) {
	for _, p := range m.installedPlugins() {
		if !p.Manifest.Implements("check") || !p.Manifest.Implements("repair") {
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, pluginCheckBudget)
		result, err := m.runPluginHook(bounded, p.Record.Name, "check", nil)
		cancel()
		if err != nil || !slices.ContainsFunc(result.Answer.Checks, func(c plugin.Check) bool { return !c.OK && c.Repairable }) {
			continue
		}
		answer, err := m.runPluginHook(ctx, p.Record.Name, "repair", nil)
		switch {
		case err != nil:
			failed = append(failed, "the "+p.Record.Name+" plugin could not repair: "+err.Error())
		case !answer.Answer.OK:
			failed = append(failed, "the "+p.Record.Name+" plugin could not repair: "+cmpMessages(answer.Answer.Messages))
		default:
			repaired = append(repaired, p.Record.Name+": "+cmpMessages(answer.Answer.Messages))
		}
	}
	return repaired, failed
}
