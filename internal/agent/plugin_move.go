package agent

// Moving a plugin's service between servers (docs/plugins-proposal.md): the
// source takes a backup through the plugin's backup hook, the control panel
// relays it like a database dump, and the destination hands its files to
// the same plugin's restore hook. Both ends need the plugin.

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
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
			out.Copyable, out.Backup = true, d.Backup
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
}

// movePluginRestoreRoute receives a backup of a plugin's service taken on
// another server and restores it into the application's service here.
func (m *Manager) movePluginRestoreRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
		err = m.restorePluginArchive(withProgress(ctx, progress), app, id, staged.File)
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
