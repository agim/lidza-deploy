package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Deleted applications (archive*.go). Deleting an application keeps what
// cannot be recreated and deletes everything that can, so its hostnames are
// free at once and it can be restored for AppArchiveRetention:
//
//   - kept, where they are: the settings snapshot (this record), the files
//     (storage and stack volumes; a native application's root, moved under
//     the apps root's .archived directory, private to root), the live
//     databases with every login disabled (container databases and
//     ClickHouse stopped; host PostgreSQL roles NOLOGIN; host ClickHouse
//     users HOST NONE);
//   - deleted: the cache (container Valkey with its volume; a native
//     application's Redis data), every backup of its databases on this
//     server and off-site, the Caddy route, TLS subjects, redirect routes and
//     the certificates of hostnames nothing else uses, service TLS copies,
//     the gateway identity, published ports, scheduled tasks, releases with
//     their containers and images, native units and the account, and the
//     data a move left of the application on this server.
//
// The record is a tombstone: every step tolerates having run before, so a
// failed deletion, restore or purge is finished by retrying it. A daily
// loop purges archives past ExpiresAt even when the panel is down.

// AppArchiveRetention is how long a deleted application can be restored.
const AppArchiveRetention = 90 * 24 * time.Hour

const (
	archivePhaseDeleting  = "deleting"
	archivePhaseArchived  = "archived"
	archivePhaseRestoring = "restoring"
	archivePhasePurging   = "purging"

	archiveKindContainer = "container"
	archiveKindStack     = "stack"
	archiveKindNative    = "native"

	archiveOperationLimit = 9 * time.Minute
	archivePurgedHistory  = 50
	archiveLogLimit       = 200
)

// AppArchive is a deleted application.
type AppArchive struct {
	ID        string    `json:"id"`
	AppID     string    `json:"app_id"`
	Kind      string    `json:"kind"` // container, stack, native or adopted
	Runtime   string    `json:"runtime,omitempty"`
	DeletedAt time.Time `json:"deleted_at"`
	DeletedBy string    `json:"deleted_by,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	// Phase is deleting, archived, restoring or purging; a phase other
	// than archived is an operation that a retry finishes.
	Phase string `json:"phase"`
	Error string `json:"error,omitempty"`
	// Hostnames are the application's hostnames and redirect hosts when it
	// was deleted; they were released at once.
	Hostnames []string `json:"hostnames"`
	// ReleasedReservations are the reserved_domains hostnames its deletion
	// released; a restore reserves them again.
	ReleasedReservations []string `json:"released_reservations,omitempty"`
	// Commit is the release that was serving.
	Commit  string         `json:"commit,omitempty"`
	Kept    []ArchiveItem  `json:"kept"`
	Deleted []ArchiveItem  `json:"deleted"`
	Log     []ArchiveEvent `json:"log,omitempty"`

	// The snapshot, never in views.
	App            *App                    `json:"app,omitempty"`
	StackSelection *StackSelectionSnapshot `json:"stack_selection,omitempty"`
	Cache          *CacheResource          `json:"cache,omitempty"`
	Native         *ArchivedNative         `json:"native_snapshot,omitempty"`
	Redirects      []RedirectDomain        `json:"redirects,omitempty"`
	Tasks          []Task                  `json:"tasks,omitempty"`
	Retained       string                  `json:"retained,omitempty"` // RetainedResources key holding the data
	Databases      []string                `json:"databases,omitempty"`
	Steps          map[string]bool         `json:"steps,omitempty"`
	RestoreRequest *ArchiveRestoreRequest  `json:"restore_request,omitempty"`
	Restored       *ArchiveRestoreResult   `json:"restored,omitempty"`
}

// ArchivedNative is the snapshot of a native application: one this agent
// created, or (Adopted) one it adopted from the existing host setup.
type ArchivedNative struct {
	App      NativeApplication       `json:"app"`
	Policy   NativeReleasePolicy     `json:"policy"`
	Spec     NativeCreateRequest     `json:"spec"`
	Settings *NativeSettingsOverride `json:"settings,omitempty"`
	Bindings map[string]string       `json:"bindings,omitempty"`
	Releases *NativeReleaseHistory   `json:"releases,omitempty"`
	// Files is where the application root is kept while archived; UID and
	// GID are its removed account's, so a restore hands the files to the
	// recreated account.
	Files string `json:"files,omitempty"`
	UID   int    `json:"uid"`
	GID   int    `json:"gid"`
	// Adopted is set for an application that existed on the host before
	// this agent (archive_adopted.go); Policy and Spec are then empty.
	Adopted *ArchivedAdopted `json:"adopted,omitempty"`
	// Leftovers is what the leftover sweep removed (archive_leftovers.go).
	Leftovers *ArchivedLeftovers `json:"leftovers,omitempty"`
}

// ArchiveItem is one kept or deleted part of a deleted application.
type ArchiveItem struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Role      string `json:"role,omitempty"` // a database's role
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Measured  bool   `json:"measured,omitempty"`
	Count     int    `json:"count,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// ArchiveEvent is one line of an archive's history.
type ArchiveEvent struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

// ArchivePurge records an archive that was purged (by its expiry or on
// request), so the panel can show and audit it after the record is gone.
type ArchivePurge struct {
	ArchiveID string        `json:"archive_id"`
	AppID     string        `json:"app_id"`
	At        time.Time     `json:"at"`
	Trigger   string        `json:"trigger"` // expiry or manual
	By        string        `json:"by,omitempty"`
	Deleted   []ArchiveItem `json:"deleted"`
}

// AppArchiveRequest is the body of POST /v1/apps/{id}/archive.
type AppArchiveRequest struct {
	DeletedBy string `json:"deleted_by,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// ExclusiveBackupPrefixes are the databases whose off-site folder
	// (databases/<id>/) holds this server's backups only, as the panel
	// verified across the fleet: the whole folder is deleted. Other
	// databases lose the off-site objects their records name.
	ExclusiveBackupPrefixes []string `json:"exclusive_backup_prefixes,omitempty"`
}

// ArchiveRestoreRequest is the body of POST /v1/archives/{id}/restore.
type ArchiveRestoreRequest struct {
	// NewID restores a container application under another id when an
	// application now uses its own.
	NewID string `json:"new_id,omitempty"`
	// Domain replaces the primary hostname when another application took it.
	Domain string `json:"domain,omitempty"`
	By     string `json:"by,omitempty"`
	// Taken are hostnames applications on other servers use, as the panel
	// knows them, with their owner: they are skipped like local ones.
	Taken map[string]string `json:"taken,omitempty"`
}

// ArchiveRestoreResult is what a restore did.
type ArchiveRestoreResult struct {
	AppID string `json:"app_id"`
	// Hostnames were published again; Skipped were taken meanwhile, with
	// the application that has each.
	Hostnames []string          `json:"hostnames"`
	Skipped   map[string]string `json:"skipped,omitempty"`
	Commit    string            `json:"commit,omitempty"`
	Native    bool              `json:"native,omitempty"`
	// Adopted reports an application adopted from the existing host setup:
	// its processes were started again as they were, so the panel does not
	// deploy it.
	Adopted bool      `json:"adopted,omitempty"`
	At      time.Time `json:"at"`
}

// ArchivePurgeRequest is the body of DELETE /v1/archives/{id}.
type ArchivePurgeRequest struct {
	Confirm string `json:"confirm"` // the application id
	By      string `json:"by,omitempty"`
}

// AppArchiveView is an archive without its snapshot.
type AppArchiveView struct {
	ID          string                `json:"id"`
	AppID       string                `json:"app_id"`
	Kind        string                `json:"kind"`
	Runtime     string                `json:"runtime,omitempty"`
	Repository  string                `json:"repository,omitempty"`
	Branch      string                `json:"branch,omitempty"`
	DeletedAt   time.Time             `json:"deleted_at"`
	DeletedBy   string                `json:"deleted_by,omitempty"`
	Reason      string                `json:"reason,omitempty"`
	ExpiresAt   time.Time             `json:"expires_at"`
	Phase       string                `json:"phase"`
	Error       string                `json:"error,omitempty"`
	Hostnames   []string              `json:"hostnames"`
	Commit      string                `json:"commit,omitempty"`
	Kept        []ArchiveItem         `json:"kept"`
	Deleted     []ArchiveItem         `json:"deleted"`
	KeptBytes   int64                 `json:"kept_bytes"`
	SizePending bool                  `json:"size_pending,omitempty"`
	Log         []ArchiveEvent        `json:"log,omitempty"`
	Restored    *ArchiveRestoreResult `json:"restored,omitempty"`
	IDAvailable bool                  `json:"id_available"`
}

// AppArchivesView is GET /v1/archives.
type AppArchivesView struct {
	Archives  []AppArchiveView `json:"archives"`
	Purged    []ArchivePurge   `json:"purged"`
	Retention string           `json:"retention"`
}

var errArchiveNotFound = errors.New("no such deleted application on this server")

// archiveRuntime holds the archives whose operation is running now.
type archiveRuntime struct {
	mu      sync.Mutex
	running map[string]bool
	sizing  bool
}

var archiveRuntimes sync.Map // *Manager -> *archiveRuntime

func (m *Manager) archiveState() *archiveRuntime {
	value, _ := archiveRuntimes.LoadOrStore(m, &archiveRuntime{running: map[string]bool{}})
	return value.(*archiveRuntime)
}

// claimArchive marks an archive's operation as running; false when one is.
func (m *Manager) claimArchive(id string) bool {
	rt := m.archiveState()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.running[id] {
		return false
	}
	rt.running[id] = true
	return true
}

func (m *Manager) releaseArchive(id string) {
	rt := m.archiveState()
	rt.mu.Lock()
	delete(rt.running, id)
	rt.mu.Unlock()
}

// archiveLocked returns the archive; the caller holds m.mu.
func (m *Manager) archiveLocked(id string) (AppArchive, bool) {
	a, ok := m.data.AppArchives[id]
	return a, ok
}

// updateArchive changes the stored archive and saves.
func (m *Manager) updateArchive(id string, change func(*AppArchive)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.AppArchives[id]
	if !ok {
		return errArchiveNotFound
	}
	previous := a
	change(&a)
	m.data.AppArchives[id] = a
	if err := m.save(); err != nil {
		m.data.AppArchives[id] = previous
		return err
	}
	return nil
}

// archiveLog appends a line to the archive's history and the agent log.
func (m *Manager) archiveLog(id, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	log.Printf("archive %s: %s", id, message)
	_ = m.updateArchive(id, func(a *AppArchive) {
		a.Log = append(a.Log, ArchiveEvent{At: time.Now().UTC(), Message: message})
		if len(a.Log) > archiveLogLimit {
			a.Log = a.Log[len(a.Log)-archiveLogLimit:]
		}
	})
}

// archiveStep runs one named step of an operation once: a step that
// finished before (a resumed operation) is skipped.
func (m *Manager) archiveStep(id, step string, run func() error) error {
	m.mu.Lock()
	done := m.data.AppArchives[id].Steps[step]
	m.mu.Unlock()
	if done {
		return nil
	}
	if err := run(); err != nil {
		return fmt.Errorf("%s: %w", step, err)
	}
	err := m.updateArchive(id, func(a *AppArchive) {
		if a.Steps == nil {
			a.Steps = map[string]bool{}
		}
		a.Steps[step] = true
	})
	if errors.Is(err, errArchiveNotFound) {
		return nil // the step's own work forgot the archive (a finished restore)
	}
	return err
}

func addArchiveItem(list *[]ArchiveItem, item ArchiveItem) {
	for i, existing := range *list {
		if existing.Kind == item.Kind && existing.Name == item.Name {
			(*list)[i] = item
			return
		}
	}
	*list = append(*list, item)
}

func archiveItemNamed(items []ArchiveItem, kind, name string) (ArchiveItem, bool) {
	for _, item := range items {
		if item.Kind == kind && item.Name == name {
			return item, true
		}
	}
	return ArchiveItem{}, false
}

func (m *Manager) archiveDeleted(id string, item ArchiveItem) {
	_ = m.updateArchive(id, func(a *AppArchive) { addArchiveItem(&a.Deleted, item) })
}

func (m *Manager) archiveKept(id string, item ArchiveItem) {
	_ = m.updateArchive(id, func(a *AppArchive) { addArchiveItem(&a.Kept, item) })
}

// ---- hostnames ---------------------------------------------------------------

// hostnameOwnerLocked names what serves host on this agent besides the
// application except: a live application, a redirect, the control panel
// or a reserved hostname; "" when nothing does.
func (m *Manager) hostnameOwnerLocked(host, except string, control *ControlHostState) string {
	host = strings.ToLower(host)
	if m.hostReserved(host) {
		return "a reserved application"
	}
	if control != nil && strings.EqualFold(control.Hostname, host) {
		return "the control panel"
	}
	for id, a := range m.data.Apps {
		if id != except && !a.Retiring && a.HasDomain(host) {
			return id
		}
	}
	for id, a := range m.data.NativeApps {
		if id != except && (App{Domain: a.Domain, AdditionalDomains: a.AdditionalDomains}).HasDomain(host) {
			return id
		}
	}
	for id, list := range m.data.Redirects {
		if id == except {
			continue
		}
		for _, d := range list {
			if strings.EqualFold(d.Host, host) {
				return id + " (redirect)"
			}
		}
	}
	return ""
}

// ---- views and routes --------------------------------------------------------

func (a AppArchive) view() AppArchiveView {
	v := AppArchiveView{ID: a.ID, AppID: a.AppID, Kind: a.Kind, Runtime: a.Runtime, DeletedAt: a.DeletedAt, DeletedBy: a.DeletedBy, Reason: a.Reason,
		ExpiresAt: a.ExpiresAt, Phase: a.Phase, Error: a.Error, Hostnames: slices.Clone(a.Hostnames), Commit: a.Commit,
		Kept: slices.Clone(a.Kept), Deleted: slices.Clone(a.Deleted), Log: slices.Clone(a.Log), Restored: a.Restored}
	if v.Hostnames == nil {
		v.Hostnames = []string{}
	}
	if v.Kept == nil {
		v.Kept = []ArchiveItem{}
	}
	if v.Deleted == nil {
		v.Deleted = []ArchiveItem{}
	}
	switch {
	case a.App != nil:
		v.Repository, v.Branch = a.App.Repository, a.App.Branch
	case a.Native != nil:
		v.Repository, v.Branch = a.Native.App.Repository, a.Native.App.Branch
	}
	for _, item := range a.Kept {
		v.KeptBytes += item.SizeBytes
		if !item.Measured && archiveMeasurable(item.Kind) {
			v.SizePending = true
		}
	}
	return v
}

func (m *Manager) archiveViews() AppArchivesView {
	m.mu.Lock()
	out := AppArchivesView{Archives: []AppArchiveView{}, Purged: slices.Clone(m.data.ArchivePurges), Retention: AppArchiveRetention.String()}
	pending := false
	for _, a := range m.data.AppArchives {
		v := a.view()
		_, container := m.data.Apps[a.AppID]
		_, native := m.data.NativeApps[a.AppID]
		v.IDAvailable = !container && !native
		pending = pending || v.SizePending
		out.Archives = append(out.Archives, v)
	}
	m.mu.Unlock()
	if out.Purged == nil {
		out.Purged = []ArchivePurge{}
	}
	slices.SortFunc(out.Archives, func(a, b AppArchiveView) int { return b.DeletedAt.Compare(a.DeletedAt) })
	if pending {
		m.measureArchiveSizes()
	}
	return out
}

func (m *Manager) archiveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/apps/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		var req AppArchiveRequest
		if r.ContentLength != 0 {
			if err := Decode(w, r, &req); err != nil {
				Fail(w, 400, err)
				return
			}
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(archiveOperationLimit + time.Minute))
		ctx, cancel := context.WithTimeout(r.Context(), archiveOperationLimit)
		defer cancel()
		archive, err := m.DeleteApp(ctx, r.PathValue("id"), req)
		if err != nil {
			status := 409
			if errors.Is(err, errArchiveNotFound) {
				status = 404
			}
			Fail(w, status, err)
			return
		}
		JSON(w, 200, archive)
	})
	mux.HandleFunc("GET /v1/archives", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, 200, m.archiveViews())
	})
	mux.HandleFunc("POST /v1/archives/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		var req ArchiveRestoreRequest
		if r.ContentLength != 0 {
			if err := Decode(w, r, &req); err != nil {
				Fail(w, 400, err)
				return
			}
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(archiveOperationLimit + time.Minute))
		ctx, cancel := context.WithTimeout(r.Context(), archiveOperationLimit)
		defer cancel()
		result, err := m.RestoreArchive(ctx, r.PathValue("id"), req)
		if err != nil {
			status := 409
			if errors.Is(err, errArchiveNotFound) {
				status = 404
			}
			Fail(w, status, err)
			return
		}
		JSON(w, 200, result)
	})
	mux.HandleFunc("DELETE /v1/archives/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req ArchivePurgeRequest
		if err := Decode(w, r, &req); err != nil {
			Fail(w, 400, err)
			return
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(archiveOperationLimit + time.Minute))
		ctx, cancel := context.WithTimeout(r.Context(), archiveOperationLimit)
		defer cancel()
		purge, err := m.PurgeArchive(ctx, r.PathValue("id"), req, "manual")
		if err != nil {
			status := 409
			if errors.Is(err, errArchiveNotFound) {
				status = 404
			}
			Fail(w, status, err)
			return
		}
		JSON(w, 200, purge)
	})
}

// ---- expiry -----------------------------------------------------------------------

var archiveExpiryInterval = time.Hour

// archiveLoop purges expired archives; it checks hourly, so an archive is
// purged within an hour of its expiry, and also when the panel is down.
func (m *Manager) archiveLoop() {
	defer m.wg.Done()
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-timer.C:
		}
		m.purgeExpiredArchives(m.ctx, time.Now())
		timer.Reset(archiveExpiryInterval)
	}
}

// purgeExpiredArchives purges every archive past its expiry that no
// operation holds; an interrupted purge is resumed. It returns the purged
// archive ids.
func (m *Manager) purgeExpiredArchives(ctx context.Context, now time.Time) []string {
	m.mu.Lock()
	var due []AppArchive
	for _, a := range m.data.AppArchives {
		if (a.Phase == archivePhaseArchived || a.Phase == archivePhasePurging) && !now.Before(a.ExpiresAt) {
			due = append(due, a)
		}
	}
	m.mu.Unlock()
	var purged []string
	for _, a := range due {
		bounded, cancel := context.WithTimeout(ctx, archiveOperationLimit)
		_, err := m.PurgeArchive(bounded, a.ID, ArchivePurgeRequest{Confirm: a.AppID, By: "expiry"}, "expiry")
		cancel()
		if err != nil {
			log.Printf("archive %s (%s): expiry purge failed: %v", a.ID, a.AppID, err)
			continue
		}
		purged = append(purged, a.ID)
	}
	return purged
}
