package agent

// Plugins: application-specific software behind a fixed contract
// (internal/plugin, docs/plugins-proposal.md). This is stage 1: the agent
// trusts keys, installs a signed plugin and runs its read-only hooks. Nothing
// in the agent uses a plugin yet.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agim/lidza-deploy/internal/plugin"
	"github.com/agim/lidza-deploy/internal/updates"
)

// PluginRecord is an installed plugin.
type PluginRecord struct {
	Name      string          `json:"name"`
	Version   string          `json:"version"`
	Provides  string          `json:"provides"`
	Summary   string          `json:"summary,omitempty"`
	Hooks     []string        `json:"hooks"`
	KeyID     string          `json:"key_id"`
	Installed time.Time       `json:"installed"`
	Manifest  json.RawMessage `json:"manifest"`
}

// PluginsView is what GET /v1/plugins answers.
type PluginsView struct {
	Plugins []PluginRecord `json:"plugins"`
	// Keys are the ids of the signing keys this server trusts for plugins:
	// the keys pinned in the agent and the ones an operator added.
	Keys   []string `json:"keys"`
	Pinned int      `json:"pinned"`
	// OperatorKeys are the public keys an operator added on this server, in
	// full, so the list can be edited; public keys are not secrets.
	OperatorKeys []string `json:"operator_keys"`
	Arch         string   `json:"arch"`
}

const maxPluginKeys = 8

// pluginStaging holds verified manifests between the two steps of an install.
type pluginStaging struct {
	mu      sync.Mutex
	pending map[string]pluginPending
	running map[string]*sync.Mutex // one hook at a time per plugin
}

type pluginPending struct {
	manifest plugin.Manifest
	raw      []byte
	keyID    string
	at       time.Time
}

// readOnlyPluginHooks are the hooks stage 1 exposes: they change nothing.
var readOnlyPluginHooks = []string{"describe", "status", "check"}

func (m *Manager) pluginRoot() string { return filepath.Join(m.cfg.DataDir, "plugins") }
func (m *Manager) pluginState(name string) string {
	return filepath.Join(m.cfg.DataDir, "plugin-state", name)
}

// pluginTrust returns the keys that may sign a plugin for this server.
func (m *Manager) pluginTrust() []ed25519.PublicKey {
	keys := slices.Clone(updates.Pinned().Keys)
	m.mu.Lock()
	added := slices.Clone(m.data.PluginKeys)
	m.mu.Unlock()
	for _, encoded := range added {
		if key, err := plugin.ParsePublicKey(encoded); err == nil {
			keys = append(keys, key)
		}
	}
	return keys
}

func (m *Manager) pluginsView() PluginsView {
	view := PluginsView{Plugins: []PluginRecord{}, Keys: []string{}, Pinned: len(updates.Pinned().Keys), Arch: runtime.GOARCH}
	for _, key := range m.pluginTrust() {
		view.Keys = append(view.Keys, plugin.KeyID(key))
	}
	slices.Sort(view.Keys)
	m.mu.Lock()
	view.OperatorKeys = append([]string{}, m.data.PluginKeys...)
	for _, record := range m.data.Plugins {
		view.Plugins = append(view.Plugins, record)
	}
	m.mu.Unlock()
	slices.SortFunc(view.Plugins, func(a, b PluginRecord) int { return strings.Compare(a.Name, b.Name) })
	return view
}

// setPluginKeys replaces the keys an operator trusts on this server.
func (m *Manager) setPluginKeys(keys []string) error {
	if len(keys) > maxPluginKeys {
		return fmt.Errorf("at most %d plugin signing keys", maxPluginKeys)
	}
	clean := []string{}
	for _, encoded := range keys {
		key, err := plugin.ParsePublicKey(encoded)
		if err != nil {
			return err
		}
		if value := fmt.Sprintf("%x", []byte(key)); !slices.Contains(clean, value) {
			clean = append(clean, value)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.data.PluginKeys
	m.data.PluginKeys = clean
	if err := m.save(); err != nil {
		m.data.PluginKeys = previous
		return err
	}
	return nil
}

func (m *Manager) pluginStaging() *pluginStaging {
	m.pluginOnce.Do(func() {
		m.plugins = &pluginStaging{pending: map[string]pluginPending{}, running: map[string]*sync.Mutex{}}
	})
	return m.plugins
}

// stagePluginManifest verifies a manifest and remembers it for the archive.
func (m *Manager) stagePluginManifest(manifest string, signature json.RawMessage) (plugin.Manifest, error) {
	parsed, keyID, err := plugin.Verify([]byte(manifest), signature, m.pluginTrust())
	if err != nil {
		return plugin.Manifest{}, err
	}
	if _, err := parsed.Archive(runtime.GOARCH); err != nil {
		return plugin.Manifest{}, err
	}
	s := m.pluginStaging()
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, pending := range s.pending {
		if time.Since(pending.at) > 30*time.Minute {
			delete(s.pending, name)
		}
	}
	s.pending[parsed.Name] = pluginPending{manifest: parsed, raw: []byte(manifest), keyID: keyID, at: time.Now()}
	return parsed, nil
}

// installPlugin unpacks the archive of a staged manifest, asks the plugin to
// describe itself and records it.
func (m *Manager) installPlugin(ctx context.Context, name string, archive interface{ Read([]byte) (int, error) }) (PluginRecord, error) {
	s := m.pluginStaging()
	s.mu.Lock()
	pending, ok := s.pending[name]
	s.mu.Unlock()
	if !ok || time.Since(pending.at) > 30*time.Minute {
		return PluginRecord{}, errors.New("send the plugin's signed manifest first")
	}
	dir, err := plugin.Install(m.pluginRoot(), pending.manifest, runtime.GOARCH, archive)
	if err != nil {
		return PluginRecord{}, err
	}
	// A plugin that cannot even describe itself is not kept.
	result, err := m.runPluginHookIn(ctx, dir, pending.manifest, "describe", nil)
	if err == nil && !result.Answer.OK {
		err = errors.New("the plugin's describe hook reported a problem: " + strings.Join(result.Answer.Messages, " "))
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return PluginRecord{}, err
	}
	record := PluginRecord{Name: pending.manifest.Name, Version: pending.manifest.Version, Provides: pending.manifest.Provides, Summary: pending.manifest.Summary,
		Hooks: slices.Clone(pending.manifest.Hooks), KeyID: pending.keyID, Installed: time.Now().UTC(), Manifest: json.RawMessage(pending.raw)}
	m.mu.Lock()
	if m.data.Plugins == nil {
		m.data.Plugins = map[string]PluginRecord{}
	}
	previous, had := m.data.Plugins[name]
	m.data.Plugins[name] = record
	if err := m.save(); err != nil {
		if had {
			m.data.Plugins[name] = previous
		} else {
			delete(m.data.Plugins, name)
		}
		m.mu.Unlock()
		return PluginRecord{}, err
	}
	m.mu.Unlock()
	s.mu.Lock()
	delete(s.pending, name)
	s.mu.Unlock()
	// The version it replaces is no longer needed.
	if had && previous.Version != record.Version {
		_ = os.RemoveAll(plugin.Dir(m.pluginRoot(), name, previous.Version))
	}
	return record, nil
}

func (m *Manager) pluginServer(name string) plugin.Server {
	server := plugin.Server{Arch: runtime.GOARCH}
	if raw, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if key, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) == "flags" {
				server.CPUFlags = strings.Fields(value)
				break
			}
		}
	}
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		var kib uint64
		for _, line := range strings.Split(string(raw), "\n") {
			if _, err := fmt.Sscanf(line, "MemTotal: %d kB", &kib); err == nil {
				server.MemoryBytes = kib << 10
				break
			}
		}
	}
	state := m.pluginState(name)
	for dir := state; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if disk, err := diskHealth(dir); err == nil {
			server.StateFreeBytes = disk.Available
			break
		}
	}
	return server
}

func (m *Manager) runPluginHookIn(ctx context.Context, dir string, manifest plugin.Manifest, hook string, input json.RawMessage) (plugin.Result, error) {
	if !manifest.Implements(hook) {
		return plugin.Result{}, fmt.Errorf("plugin %s does not implement the %s hook", manifest.Name, hook)
	}
	s := m.pluginStaging()
	s.mu.Lock()
	lock := s.running[manifest.Name]
	if lock == nil {
		lock = &sync.Mutex{}
		s.running[manifest.Name] = lock
	}
	s.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	return plugin.Run(ctx, dir, plugin.Request{Hook: hook, Server: m.pluginServer(manifest.Name), StateDir: m.pluginState(manifest.Name), Input: input})
}

// runPluginHook runs a hook of an installed plugin.
func (m *Manager) runPluginHook(ctx context.Context, name, hook string, input json.RawMessage) (plugin.Result, error) {
	m.mu.Lock()
	record, ok := m.data.Plugins[name]
	m.mu.Unlock()
	if !ok {
		return plugin.Result{}, errors.New("no such plugin on this server")
	}
	manifest, err := plugin.ParseManifest(record.Manifest)
	if err != nil {
		return plugin.Result{}, err
	}
	return m.runPluginHookIn(ctx, plugin.Dir(m.pluginRoot(), record.Name, record.Version), manifest, hook, input)
}

// removePlugin forgets a plugin and deletes its program. Its state directory
// stays: it may hold what the plugin manages.
func (m *Manager) removePlugin(name string) error {
	m.mu.Lock()
	record, ok := m.data.Plugins[name]
	if !ok {
		m.mu.Unlock()
		return errors.New("no such plugin on this server")
	}
	delete(m.data.Plugins, name)
	if err := m.save(); err != nil {
		m.data.Plugins[name] = record
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	return os.RemoveAll(filepath.Join(m.pluginRoot(), name))
}

func (m *Manager) pluginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/plugins", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, m.pluginsView()) })
	mux.HandleFunc("PUT /v1/plugins/keys", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Keys []string `json:"keys"`
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setPluginKeys(in.Keys); err != nil {
			Fail(w, 400, err)
			return
		}
		JSON(w, 200, m.pluginsView())
	})
	mux.HandleFunc("POST /v1/plugins/manifest", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Manifest  string          `json:"manifest"`
			Signature json.RawMessage `json:"signature"`
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		manifest, err := m.stagePluginManifest(in.Manifest, in.Signature)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]any{"name": manifest.Name, "version": manifest.Version, "arch": runtime.GOARCH})
	})
	mux.HandleFunc("PUT /v1/plugins/{name}/archive", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(20 * time.Minute))
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(20 * time.Minute))
		if r.Header.Get("Content-Type") != "application/octet-stream" {
			Fail(w, 415, errors.New("plugin archives are uploaded as application/octet-stream"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, plugin.MaxArchiveSize+1)
		record, err := m.installPlugin(r.Context(), r.PathValue("name"), r.Body)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 201, record)
	})
	mux.HandleFunc("POST /v1/plugins/{name}/hooks/{hook}", func(w http.ResponseWriter, r *http.Request) {
		hook := r.PathValue("hook")
		if !slices.Contains(readOnlyPluginHooks, hook) {
			Fail(w, 400, errors.New("only the describe, status and check hooks can be run on request"))
			return
		}
		var input json.RawMessage
		if err := Decode(w, r, &input); err != nil {
			Fail(w, 400, err)
			return
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(plugin.HookTimeout(hook) + 10*time.Second))
		result, err := m.runPluginHook(r.Context(), r.PathValue("name"), hook, input)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]any{"answer": result.Answer, "log": result.Log, "duration_ms": result.Duration.Milliseconds()})
	})
	mux.HandleFunc("DELETE /v1/plugins/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.removePlugin(r.PathValue("name")); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "removed"})
	})
}
