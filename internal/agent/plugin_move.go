package agent

// Moving a plugin's service between servers (docs/plugins-proposal.md): the
// source takes a backup through the plugin's backup hook, the control panel
// relays it like a database dump, and the destination hands its files to
// the same plugin's restore hook. Both ends need the plugin.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// MoveSourcePlugin is a plugin's service of an application a move starts from.
type MoveSourcePlugin struct {
	Service string `json:"service"`
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	// Resource is the protected resource its backups are kept under.
	Resource string `json:"resource,omitempty"`
	// Copyable services are backed up here and restored on the destination.
	Copyable bool   `json:"copyable"`
	Reason   string `json:"reason,omitempty"`
	// SizeBytes is the size of the newest backup, when there is one.
	SizeBytes int64        `json:"size_bytes,omitempty"`
	Backup    BackupPolicy `json:"backup"`
	// Exports says the plugin implements export, so a move's final sync
	// can copy only what changed.
	Exports bool `json:"exports,omitempty"`
}

// NativeHostPluginView is a plugin's service in an application's data view.
type NativeHostPluginView struct {
	Service  string `json:"service"`
	Plugin   string `json:"plugin"`
	Version  string `json:"version"`
	Resource string `json:"resource,omitempty"`
	Ready    bool   `json:"ready"`
}

// pluginServiceViews lists an application's plugin services.
func (m *Manager) pluginServiceViews(record NativeHostDataRecord) []NativeHostPluginView {
	m.mu.Lock()
	defer m.mu.Unlock()
	views := []NativeHostPluginView{}
	for _, service := range slices.Sorted(maps.Keys(record.Plugins)) {
		entry := record.Plugins[service]
		view := NativeHostPluginView{Service: service, Plugin: entry.Plugin, Version: entry.Version, Resource: entry.Resource, Ready: true}
		if entry.Resource != "" {
			view.Ready = m.data.Databases[entry.Resource].Ready
		}
		views = append(views, view)
	}
	return views
}

// moveSourcePlugins lists the plugin services of an application for a
// move, and what stops one from being copied.
func (m *Manager) moveSourcePlugins(record NativeHostDataRecord) (services []MoveSourcePlugin, blockers []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, service := range slices.Sorted(maps.Keys(record.Plugins)) {
		entry := record.Plugins[service]
		out := MoveSourcePlugin{Service: service, Plugin: entry.Plugin, Version: entry.Version, Resource: entry.Resource}
		installed, known := m.data.Plugins[entry.Plugin]
		d, exists := m.data.Databases[entry.Resource]
		switch {
		case entry.Resource == "" || !known || !slices.Contains(installed.Hooks, "backup"):
			out.Reason = "its plugin cannot back it up"
			blockers = append(blockers, "The "+service+" service of the "+entry.Plugin+" plugin cannot be copied: the plugin on this server has no backup. Move its data by hand.")
		case !exists || !d.Ready || d.Operation != "":
			out.Reason = "unavailable or busy"
			blockers = append(blockers, "The "+service+" service is unavailable or has an active operation.")
		default:
			out.Copyable, out.Backup, out.Exports = true, d.Backup, slices.Contains(installed.Hooks, "export")
			for _, backup := range d.Backups {
				out.SizeBytes = max(out.SizeBytes, backup.Size)
			}
		}
		services = append(services, out)
	}
	return services, blockers
}

func (m *Manager) pluginMoveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/move/plugin-services/{id}/restore", m.moveTransfer(m.movePluginRestoreRoute))
	m.pluginExportRoutes(mux)
}

// movePluginRestoreRoute receives a backup of a plugin's service taken on
// another server and restores it into the application's service here.
func (m *Manager) movePluginRestoreRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// A move's export goes to the plugin's import hook, with its move and
	// phase; a plain backup goes to its restore hook.
	var move *pluginMove
	if query := r.URL.Query(); query.Get("move") != "" {
		if !pluginMoveID.MatchString(query.Get("move")) || !pluginMovePhase.MatchString(query.Get("phase")) {
			Fail(w, 400, errors.New("invalid move or phase"))
			return
		}
		if _, _, ok := m.pluginImplements(id, "import"); !ok {
			Fail(w, http.StatusNotImplemented, errors.New("this service's plugin has no import for moves"))
			return
		}
		move = &pluginMove{ID: query.Get("move"), Phase: query.Get("phase"), Incremental: query.Get("incremental") == "1"}
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(6 * time.Hour))
	_ = controller.SetWriteDeadline(time.Now().Add(6*time.Hour + time.Minute))
	app, err := m.beginPluginMoveRestore(id)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	var result error
	defer func() { m.endMoveRestore(id, result) }()
	progress, endProgress := m.progress.track(databaseProgressKey(id), "Receiving the backup")
	defer endProgress()
	staged, err := m.receiveMoveUpload(r, m.backupDir(id), moveMaxDumpSize, "dump", progress)
	if err != nil {
		var upload *moveUploadError
		if !errors.As(err, &upload) || !errors.Is(err, errMoveUploadIncomplete) {
			result = err
		}
		failMoveUpload(w, err)
		return
	}
	restored := false
	defer func() { staged.Done(restored) }()
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	if _, err = staged.File.Seek(0, io.SeekStart); err == nil {
		err = m.restorePluginArchive(withProgress(ctx, progress), app, id, staged.File, move)
	}
	if err != nil {
		result = err
		Fail(w, 409, err)
		return
	}
	restored = true
	w.Header().Set(MoveVerifiedHeader, staged.SHA256)
	JSON(w, 200, map[string]any{"status": "restored", "bytes": staged.Size, "sha256": staged.SHA256})
}

// beginPluginMoveRestore reserves the service's resource like any database
// operation, so backups and other restores wait.
func (m *Manager) beginPluginMoveRestore(resource string) (string, error) {
	if m.upgradePending() {
		return "", errors.New("agent upgrade in progress")
	}
	app, _, ok := m.pluginResourceApp(resource)
	if !ok {
		return "", errors.New("this is not a plugin's service of an application on this server")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, exists := m.data.Databases[resource]
	switch {
	case !exists || !d.Ready:
		return "", errors.New("the destination service is not ready")
	case d.Operation != "":
		return "", errors.New("an operation on the service is already running")
	case m.nativeOperations[app]:
		return "", errors.New("wait for the application's active deployment")
	}
	old := d
	d.Operation, d.Error = "restore", ""
	m.data.Databases[resource] = d
	if err := m.save(); err != nil {
		m.data.Databases[resource] = old
		return "", err
	}
	return app, nil
}

// ---- exports: a move's own copies, incremental when the plugin can --------------

// PluginExport is the state of one export of a plugin's service for a move.
type PluginExport struct {
	State string `json:"state"` // running, ready, failed or missing
	// SHA256 and Size describe the archive of a ready export.
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	// Incremental says the archive holds only what changed since the bulk phase.
	Incremental bool   `json:"incremental,omitempty"`
	Error       string `json:"error,omitempty"`
}

var (
	pluginMoveID    = regexp.MustCompile(`^[a-z0-9]{8,40}$`)
	pluginMovePhase = regexp.MustCompile(`^(bulk|final)$`)
)

func (m *Manager) pluginExportPath(resource, move, phase string) string {
	return filepath.Join(m.backupDir(resource), ".export-"+move+"-"+phase+".dump")
}

// pluginImplements reports whether the plugin behind a protected resource
// implements a hook.
func (m *Manager) pluginImplements(resource, hook string) (app, service string, ok bool) {
	app, service, found := m.pluginResourceApp(resource)
	if !found {
		return "", "", false
	}
	record, _ := m.hostDataRecord(app)
	m.mu.Lock()
	defer m.mu.Unlock()
	return app, service, slices.Contains(m.data.Plugins[record.Plugins[service].Plugin].Hooks, hook)
}

// beginPluginOperation reserves the service's resource for an operation.
func (m *Manager) beginPluginOperation(resource, app, operation string) error {
	if m.upgradePending() {
		return errors.New("agent upgrade in progress")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, exists := m.data.Databases[resource]
	switch {
	case !exists || !d.Ready:
		return errors.New("the service is not ready")
	case d.Operation != "":
		return errors.New("an operation on the service is already running")
	}
	old := d
	d.Operation, d.Error = operation, ""
	m.data.Databases[resource] = d
	if err := m.save(); err != nil {
		m.data.Databases[resource] = old
		return err
	}
	return nil
}

func (m *Manager) endPluginOperation(resource, failed string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data.Databases[resource]
	if !ok {
		return
	}
	d.Operation, d.Event = "", newID()
	if err != nil {
		d.Error = failed + err.Error()
	}
	m.data.Databases[resource] = d
	_ = m.save()
}

const pluginExportFailed = "Export for the move failed: "

// startPluginExport runs the plugin's export hook for one phase of a move in
// the background and archives what it leaves in its directory.
func (m *Manager) startPluginExport(resource string, move pluginMove) error {
	app, service, ok := m.pluginImplements(resource, "export")
	if !ok {
		return errPluginExportUnsupported
	}
	if err := m.beginPluginOperation(resource, app, "export"); err != nil {
		return err
	}
	target := m.pluginExportPath(resource, move.ID, move.Phase)
	_ = os.Remove(target)
	_ = os.Remove(target + ".json")
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 6*time.Hour)
		defer cancel()
		ctx, endProgress := m.trackDatabase(ctx, resource, "Waiting for an available database operation slot")
		defer endProgress()
		var err error
		select {
		case m.databaseSlots <- struct{}{}:
			err = m.runPluginExport(ctx, app, service, resource, move, target)
			<-m.databaseSlots
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err != nil {
			_ = os.Remove(target)
			_ = os.Remove(target + ".json")
		}
		m.endPluginOperation(resource, pluginExportFailed, err)
	}()
	return nil
}

var errPluginExportUnsupported = errors.New("this service's plugin has no export for moves")

func (m *Manager) runPluginExport(ctx context.Context, app, service, resource string, move pluginMove, target string) error {
	progress := progressFrom(ctx)
	entry, input, err := m.pluginMoveInput(app, service, resource, &move)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.backupDir(resource), 0o700); err != nil {
		return err
	}
	work, err := os.MkdirTemp(m.backupDir(resource), ".plugin-*")
	if err != nil {
		return errors.New("cannot create the plugin's export directory")
	}
	defer os.RemoveAll(work)
	progress.Stage("Exporting the "+service+" service through its plugin", 0, nil)
	result, err := m.runPluginHookWork(ctx, entry.Plugin, "export", input, work)
	if err != nil {
		return err
	}
	if !result.Answer.OK {
		return fmt.Errorf("the %s plugin could not export the service: %s", entry.Plugin, cmpMessages(result.Answer.Messages))
	}
	var exported struct {
		Incremental bool `json:"incremental"`
	}
	_ = json.Unmarshal(result.Answer.Output, &exported)
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	counted := &countingWriter{w: io.MultiWriter(file, hash)}
	progress.Stage("Archiving the export", 0, counted.n.Load)
	compression := compressionNone
	if zstdAvailable() {
		compression = compressionZstd
	}
	compressing, cancel := context.WithCancel(ctx)
	defer cancel()
	writer, err := compressedWriter(compressing, counted, compression)
	if err != nil {
		return err
	}
	err = tarPluginDirectory(work, writer)
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		return err
	}
	state, _ := json.Marshal(PluginExport{State: "ready", SHA256: hex.EncodeToString(hash.Sum(nil)), Size: counted.n.Load(), Incremental: exported.Incremental && move.Phase == "final"})
	return os.WriteFile(target+".json", state, 0o600)
}

type countingWriter struct {
	w io.Writer
	n atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// pluginExportState reports one export of a move.
func (m *Manager) pluginExportState(resource, move, phase string) PluginExport {
	if raw, err := os.ReadFile(m.pluginExportPath(resource, move, phase) + ".json"); err == nil {
		var state PluginExport
		if json.Unmarshal(raw, &state) == nil && state.State == "ready" {
			return state
		}
	}
	m.mu.Lock()
	d := m.data.Databases[resource]
	m.mu.Unlock()
	switch {
	case d.Operation == "export":
		return PluginExport{State: "running"}
	case strings.HasPrefix(d.Error, pluginExportFailed):
		return PluginExport{State: "failed", Error: strings.TrimPrefix(d.Error, pluginExportFailed)}
	}
	return PluginExport{State: "missing"}
}

// endPluginMove removes what a move left on this server for a service: its
// export archives, and whatever the plugin kept (its export and import
// hooks are told the move ended).
func (m *Manager) endPluginMove(ctx context.Context, resource, move string) error {
	for _, phase := range []string{"bulk", "final"} {
		_ = os.Remove(m.pluginExportPath(resource, move, phase))
		_ = os.Remove(m.pluginExportPath(resource, move, phase) + ".json")
	}
	var failed error
	for _, hook := range []string{"export", "import"} {
		app, service, ok := m.pluginImplements(resource, hook)
		if !ok {
			continue
		}
		entry, input, err := m.pluginMoveInput(app, service, resource, &pluginMove{ID: move, Phase: "end"})
		if err != nil {
			return err
		}
		result, err := m.runPluginHook(ctx, entry.Plugin, hook, input)
		if err == nil && !result.Answer.OK {
			err = fmt.Errorf("the %s plugin could not clean up after the move: %s", entry.Plugin, cmpMessages(result.Answer.Messages))
		}
		if err != nil {
			failed = err
		}
	}
	return failed
}

func (m *Manager) pluginExportRoutes(mux *http.ServeMux) {
	valid := func(w http.ResponseWriter, r *http.Request) (resource, move, phase string, ok bool) {
		resource, move, phase = r.PathValue("id"), r.PathValue("move"), r.PathValue("phase")
		if _, _, found := m.pluginResourceApp(resource); !found || !pluginMoveID.MatchString(move) || phase != "" && !pluginMovePhase.MatchString(phase) {
			Fail(w, 404, errors.New("unknown plugin service or move"))
			return "", "", "", false
		}
		return resource, move, phase, true
	}
	mux.HandleFunc("POST /v1/move/plugin-services/{id}/exports/{move}/{phase}", func(w http.ResponseWriter, r *http.Request) {
		resource, move, phase, ok := valid(w, r)
		if !ok {
			return
		}
		err := m.startPluginExport(resource, pluginMove{ID: move, Phase: phase, Full: r.URL.Query().Get("full") == "1"})
		switch {
		case errors.Is(err, errPluginExportUnsupported):
			Fail(w, http.StatusNotImplemented, err)
		case err != nil:
			Fail(w, 409, err)
		default:
			JSON(w, 202, map[string]string{"status": "exporting"})
		}
	})
	mux.HandleFunc("GET /v1/move/plugin-services/{id}/exports/{move}/{phase}", func(w http.ResponseWriter, r *http.Request) {
		if resource, move, phase, ok := valid(w, r); ok {
			JSON(w, 200, m.pluginExportState(resource, move, phase))
		}
	})
	mux.HandleFunc("GET /v1/move/plugin-services/{id}/exports/{move}/{phase}/archive", m.moveTransfer(func(w http.ResponseWriter, r *http.Request) {
		resource, move, phase, ok := valid(w, r)
		if !ok {
			return
		}
		state := m.pluginExportState(resource, move, phase)
		file, err := os.Open(m.pluginExportPath(resource, move, phase))
		if state.State != "ready" || err != nil {
			Fail(w, 404, errors.New("the export is not ready"))
			return
		}
		defer file.Close()
		if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() != state.Size {
			Fail(w, 409, errors.New("the export does not match its record"))
			return
		}
		serveMoveArtifact(w, r, file, state.Size, state.SHA256, "application/octet-stream", 6*time.Hour)
	}))
	mux.HandleFunc("DELETE /v1/move/plugin-services/{id}/moves/{move}", func(w http.ResponseWriter, r *http.Request) {
		resource, move, _, ok := valid(w, r)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		if err := m.endPluginMove(ctx, resource, move); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "removed"})
	})
}
