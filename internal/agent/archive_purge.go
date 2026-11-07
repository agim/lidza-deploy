package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza/packs/storage"
)

// PurgeArchive deletes a deleted application for good: its databases with
// their roles and containers, its volumes and files, the objects of its S3
// application storage, and the archive record. trigger is expiry or
// manual. An archive whose restore is running, or whose deletion is not
// finished, is refused.
func (m *Manager) PurgeArchive(ctx context.Context, archiveID string, req ArchivePurgeRequest, trigger string) (ArchivePurge, error) {
	if !m.claimArchive(archiveID) {
		return ArchivePurge{}, errors.New("an operation on this deleted application is running; purge it after it finishes")
	}
	defer m.releaseArchive(archiveID)
	m.mu.Lock()
	a, ok := m.data.AppArchives[archiveID]
	if !ok {
		m.mu.Unlock()
		return ArchivePurge{}, errArchiveNotFound
	}
	if req.Confirm != a.AppID {
		m.mu.Unlock()
		return ArchivePurge{}, errors.New("confirm the application id to delete it permanently")
	}
	switch a.Phase {
	case archivePhaseRestoring:
		m.mu.Unlock()
		return ArchivePurge{}, errors.New("a restore of this application is in progress; it is not purged")
	case archivePhaseDeleting:
		m.mu.Unlock()
		return ArchivePurge{}, errors.New("the deletion is not finished; delete the application again to finish it first")
	}
	if a.Phase != archivePhasePurging {
		previous := a
		a.Phase, a.Error, a.Steps = archivePhasePurging, "", nil
		m.data.AppArchives[archiveID] = a
		if err := m.save(); err != nil {
			m.data.AppArchives[archiveID] = previous
			m.mu.Unlock()
			return ArchivePurge{}, err
		}
	}
	m.mu.Unlock()
	m.archiveLog(archiveID, "Permanent deletion started (%s).", trigger)
	purge, err := m.runArchivePurge(ctx, archiveID)
	if err != nil {
		_ = m.updateArchive(archiveID, func(x *AppArchive) { x.Error = err.Error() })
		m.archiveLog(archiveID, "Permanent deletion stopped: %v; it is retried.", err)
		return ArchivePurge{}, err
	}
	purge.Trigger, purge.By, purge.At = trigger, req.By, time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data.AppArchives, archiveID)
	m.data.ArchivePurges = append(m.data.ArchivePurges, purge)
	if len(m.data.ArchivePurges) > archivePurgedHistory {
		m.data.ArchivePurges = m.data.ArchivePurges[len(m.data.ArchivePurges)-archivePurgedHistory:]
	}
	job := newHostJob("archive-purge", trigger, purge.At)
	finished := purge.At
	job.Finished, job.Status = &finished, "succeeded"
	job.Summary = "Deleted application " + purge.AppID + " permanently (" + archiveID + ")."
	var lines []string
	for _, item := range purge.Deleted {
		lines = append(lines, item.Kind+" "+item.Name+" "+item.Detail)
	}
	job.Output = strings.Join(lines, "\n")
	m.recordHostJobLocked(job)
	if err := m.save(); err != nil {
		return ArchivePurge{}, err
	}
	return purge, nil
}

func (m *Manager) runArchivePurge(ctx context.Context, archiveID string) (ArchivePurge, error) {
	a := m.archiveSnapshot(archiveID)
	purge := ArchivePurge{ArchiveID: archiveID, AppID: a.AppID}
	// S3 objects first: a native application's settings live in its files.
	if err := m.archiveStep(archiveID, "purge-storage", func() error {
		item, err := m.purgeAppStorage(ctx, a)
		if item.Kind != "" {
			m.archiveLog(archiveID, "%s %s: %s", item.Kind, item.Name, item.Detail)
			_ = m.updateArchive(archiveID, func(x *AppArchive) { addArchiveItem(&x.Deleted, item) })
		}
		return err
	}); err != nil {
		return purge, err
	}
	if err := m.archiveStep(archiveID, "purge-data", func() error {
		if a.Retained == "" {
			return nil
		}
		m.mu.Lock()
		_, held := m.data.RetainedResources[a.Retained]
		m.mu.Unlock()
		if !held {
			return nil
		}
		return m.removeRetainedForced(ctx, a.Retained, "archive-purge", true)
	}); err != nil {
		return purge, err
	}
	if err := m.archiveStep(archiveID, "purge-records", func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, dbID := range a.Databases {
			if d, ok := m.data.Databases[dbID]; ok && !d.Removed.IsZero() && !m.databaseInUseLocked(dbID) {
				delete(m.data.Databases, dbID)
				delete(m.data.NativeDatabases, dbID)
			}
		}
		if _, live := m.data.NativeCreated[a.AppID]; a.Kind == archiveKindNative && !live {
			delete(m.data.NativeHostData, a.AppID)
		}
		return m.save()
	}); err != nil {
		return purge, err
	}
	if err := m.archiveStep(archiveID, "purge-adopted-databases", func() error { return m.purgeAdoptedDatabases(ctx, archiveID) }); err != nil {
		return purge, err
	}
	if err := m.archiveStep(archiveID, "purge-files", func() error {
		if a.Native == nil || a.Native.Files == "" {
			return nil
		}
		if err := os.RemoveAll(filepath.Dir(a.Native.Files)); err != nil {
			return fmt.Errorf("could not delete the application files: %w", err)
		}
		return nil
	}); err != nil {
		return purge, err
	}
	final := m.archiveSnapshot(archiveID)
	for _, item := range final.Kept {
		if item.Kind == "settings" {
			continue
		}
		if a.Kind == archiveKindAdopted && item.Kind == "database" {
			if dropped, ok := archiveItemNamed(final.Deleted, "database", item.Name); ok {
				item = dropped
			} else {
				// Shared with a live application: the host's PostgreSQL keeps it.
				item.Detail = "left on the host PostgreSQL (" + item.Detail + ")"
			}
		}
		purge.Deleted = append(purge.Deleted, item)
	}
	purge.Deleted = append(purge.Deleted, ArchiveItem{Kind: "settings", Name: a.AppID, Detail: "the settings snapshot"})
	for _, item := range final.Deleted {
		if item.Kind == "s3-objects" {
			purge.Deleted = append(purge.Deleted, item)
		}
	}
	return purge, nil
}

// archiveAppEnvironment is the application's environment from its
// snapshot, or a native application's environment file.
func archiveAppEnvironment(a AppArchive) map[string]string {
	if a.App != nil {
		return a.App.Env
	}
	if a.Native == nil || a.Native.Files == "" || a.Native.Adopted != nil {
		// An adopted application's storage settings are its own; its
		// bucket is not emptied.
		return nil
	}
	data, err := os.ReadFile(filepath.Join(a.Native.Files, "shared", ".env"))
	if err != nil {
		return nil
	}
	values := map[string]string{}
	if parseNativeLiteralEnvironment(data, values) != nil {
		return nil
	}
	return values
}

// purgeAppStorage deletes the objects of the application's S3 storage
// (STORAGE_PROVIDER other than local). Only a store with its own folder
// (STORAGE_PREFIX) is emptied: a bucket without one may hold other data,
// and a folder the backups or another application on this server use is
// never touched.
func (m *Manager) purgeAppStorage(ctx context.Context, a AppArchive) (ArchiveItem, error) {
	env := archiveAppEnvironment(a)
	provider := strings.ToLower(strings.TrimSpace(env["STORAGE_PROVIDER"]))
	if provider == "" || provider == "local" || env["STORAGE_BUCKET"] == "" {
		return ArchiveItem{}, nil
	}
	cfg := storage.Config{Provider: provider, Bucket: env["STORAGE_BUCKET"], Endpoint: env["STORAGE_ENDPOINT"], Region: env["STORAGE_REGION"],
		AccessKey: env["STORAGE_ACCESS_KEY"], SecretKey: env["STORAGE_SECRET_KEY"], Prefix: strings.Trim(env["STORAGE_PREFIX"], "/"), Timeout: time.Minute}
	if v, err := strconv.ParseBool(env["STORAGE_PATH_STYLE"]); err == nil {
		cfg.PathStyle = &v
	}
	name := cfg.Bucket
	if cfg.Prefix != "" {
		name += "/" + cfg.Prefix
	}
	item := ArchiveItem{Kind: "s3-objects", Name: name}
	if cfg.Prefix == "" {
		item.Detail = "kept: the bucket has no STORAGE_PREFIX folder for this application alone; empty it with the storage provider"
		return item, nil
	}
	if owner := m.appStorageSharedWith(a.AppID, cfg); owner != "" {
		item.Detail = "kept: " + owner + " uses the same bucket folder"
		return item, nil
	}
	store, err := newArchiveObjectStore(cfg)
	if err != nil {
		return item, errors.New("the application's storage configuration is invalid")
	}
	var bytes int64
	for pass := 0; pass < 1000; pass++ {
		objects, err := store.List(ctx, "", archiveListLimit)
		if err != nil {
			return item, errors.New("could not list the application's S3 storage; check its credentials")
		}
		if len(objects) == 0 {
			break
		}
		for _, o := range objects {
			if err := store.Delete(ctx, o.Key); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return item, fmt.Errorf("could not delete %s from the application's S3 storage", o.Key)
			}
			item.Count++
			bytes += o.Size
		}
	}
	item.SizeBytes = bytes
	item.Detail = fmt.Sprintf("deleted %d objects (%s)", item.Count, retentionSize(bytes))
	return item, nil
}

// appStorageSharedWith names a user of the same bucket folder: the backups
// or a live application on this server.
func (m *Manager) appStorageSharedWith(appID string, cfg storage.Config) string {
	overlap := func(bucket, endpoint, prefix string) bool {
		prefix = strings.Trim(prefix, "/")
		return bucket == cfg.Bucket && (endpoint == "" || cfg.Endpoint == "" || endpoint == cfg.Endpoint) &&
			(prefix == "" || prefix == cfg.Prefix || strings.HasPrefix(cfg.Prefix, prefix+"/") || strings.HasPrefix(prefix, cfg.Prefix+"/"))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.data.BackupStorage; b != nil && overlap(b.Bucket, b.Endpoint, b.Prefix) {
		return "the off-site backups"
	}
	for id, other := range m.data.Apps {
		if id != appID && other.Env["STORAGE_BUCKET"] != "" && overlap(other.Env["STORAGE_BUCKET"], other.Env["STORAGE_ENDPOINT"], other.Env["STORAGE_PREFIX"]) {
			return id
		}
	}
	return ""
}

// removeRetainedForced removes a retained resource without requiring a
// covering backup: a move's leftover (a copy of data that lives on), or
// the data of an archive being purged (archived, includeArchived).
func (m *Manager) removeRetainedForced(ctx context.Context, key, trigger string, includeArchived bool) error {
	rt := m.retainedState()
	m.mu.Lock()
	var item *retainedItem
	for _, it := range m.retainedItemsFilterLocked(includeArchived) {
		if it.id == key {
			item = &it
			break
		}
	}
	if item == nil {
		m.mu.Unlock()
		return nil
	}
	if attached := m.attachedLocked(*item); len(attached) > 0 {
		m.mu.Unlock()
		return fmt.Errorf("attached to %s on this server", strings.Join(attached, ", "))
	}
	rt.mu.Lock()
	busy := rt.removing[key]
	if !busy {
		rt.removing[key] = true
	}
	rt.mu.Unlock()
	if busy {
		m.mu.Unlock()
		return errors.New("this data is already being removed")
	}
	item.copy = true
	job := newHostJob("retained-removal", trigger, time.Now())
	job.Summary = "Removing " + item.kind + " data of " + item.appID + " (" + key + ")."
	m.recordHostJobLocked(job)
	m.mu.Unlock()
	output, err := m.runRetainedRemoval(ctx, *item, false, nil)
	m.finishRetainedRemoval(*item, job, output, err)
	return err
}

// ---- sizes ------------------------------------------------------------------------

func archiveMeasurable(kind string) bool { return kind == "volume" || kind == "directory" }

var archiveDirectorySize = func(ctx context.Context, path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

// measureArchiveSizes measures the kept volumes and directories that are
// not measured yet, in the background; the sizes are stored in the archive.
func (m *Manager) measureArchiveSizes() {
	rt := m.archiveState()
	rt.mu.Lock()
	if rt.sizing || m.ctx.Err() != nil {
		rt.mu.Unlock()
		return
	}
	rt.sizing = true
	rt.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() { rt.mu.Lock(); rt.sizing = false; rt.mu.Unlock() }()
		m.mu.Lock()
		type pending struct{ archive, kind, name string }
		var todo []pending
		for id, a := range m.data.AppArchives {
			for _, item := range a.Kept {
				if archiveMeasurable(item.Kind) && !item.Measured {
					todo = append(todo, pending{id, item.Kind, item.Name})
				}
			}
		}
		m.mu.Unlock()
		for _, p := range todo {
			ctx, cancel := context.WithTimeout(m.ctx, retainedSizeTimeout)
			var size int64
			var err error
			if p.kind == "volume" {
				size, err = dockerVolumeSize(ctx, p.name)
			} else {
				size, err = archiveDirectorySize(ctx, p.name)
			}
			cancel()
			if m.ctx.Err() != nil {
				return
			}
			_ = m.updateArchive(p.archive, func(a *AppArchive) {
				for i := range a.Kept {
					if a.Kept[i].Kind == p.kind && a.Kept[i].Name == p.name {
						a.Kept[i].Measured = true
						if err == nil {
							a.Kept[i].SizeBytes = size
						} else {
							a.Kept[i].Detail = strings.TrimSuffix(a.Kept[i].Detail, " (size unavailable)") + " (size unavailable)"
						}
					}
				}
			})
		}
	}()
}
