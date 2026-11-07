package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/retention"
	"github.com/agim/lidza/packs/storage"
)

// archiveObjectStore is the off-site storage interface the archive uses:
// listing and deleting objects. Tests replace newArchiveObjectStore.
type archiveObjectStore interface {
	List(ctx context.Context, prefix string, limit int) ([]storage.Object, error)
	Delete(ctx context.Context, key string) error
}

var newArchiveObjectStore = func(cfg storage.Config) (archiveObjectStore, error) { return storage.New(cfg) }

// archiveCaddyStorage is the running Caddy's certificate storage root, or ""
// when it has none on disk. Tests replace it.
var archiveCaddyStorage = func(ctx context.Context, m *Manager) string { return m.caddyStorage(ctx) }

const archiveListLimit = 1000

func cloneJSON[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

// DeleteApp deletes an application into a restorable archive (archive.go).
// A deletion that failed part way is finished by calling it again.
func (m *Manager) DeleteApp(ctx context.Context, id string, req AppArchiveRequest) (AppArchiveView, error) {
	if m.upgradePending() {
		return AppArchiveView{}, errors.New("agent upgrade in progress; delete the application after it finishes")
	}
	adopted, err := m.adoptedSnapshotFor(ctx, id)
	if err != nil {
		return AppArchiveView{}, err
	}
	archiveID, err := m.beginArchive(id, req, adopted)
	if err != nil {
		return AppArchiveView{}, err
	}
	if !m.claimArchive(archiveID) {
		return AppArchiveView{}, errors.New("this application's deletion is already running")
	}
	defer m.releaseArchive(archiveID)
	if err := m.runArchiveDelete(ctx, archiveID, req); err != nil {
		_ = m.updateArchive(archiveID, func(a *AppArchive) { a.Error = err.Error() })
		m.archiveLog(archiveID, "Deletion stopped: %v. Delete the application again to finish.", err)
		return AppArchiveView{}, fmt.Errorf("the application is disabled but its deletion is not finished; retry: %w", err)
	}
	_ = m.updateArchive(archiveID, func(a *AppArchive) { a.Phase, a.Error, a.Steps = archivePhaseArchived, "", nil })
	m.archiveLog(archiveID, "Deleted; restorable until %s.", m.archiveExpiry(archiveID).Format(time.DateOnly))
	m.wakeDomains()
	m.measureArchiveSizes()
	m.mu.Lock()
	defer m.mu.Unlock()
	a, _ := m.archiveLocked(archiveID)
	return a.view(), nil
}

func (m *Manager) archiveExpiry(id string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data.AppArchives[id].ExpiresAt
}

// beginArchive records the archive with the application's snapshot, or
// returns the deletion of id that is not finished.
func (m *Manager) beginArchive(id string, req AppArchiveRequest, adopted *ArchivedAdopted) (string, error) {
	if len(req.Reason) > 1000 || len(req.DeletedBy) > 200 {
		return "", errors.New("the reason is limited to 1000 characters")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.data.AppArchives {
		if a.AppID == id && a.Phase == archivePhaseDeleting {
			return a.ID, nil
		}
	}
	now := time.Now().UTC()
	archive := AppArchive{ID: "archive-" + newID(), AppID: id, DeletedAt: now, ExpiresAt: now.Add(AppArchiveRetention), Phase: archivePhaseDeleting,
		DeletedBy: strings.TrimSpace(req.DeletedBy), Reason: strings.TrimSpace(req.Reason)}
	if a, ok := m.data.Apps[id]; ok {
		if a.Preview {
			return "", errors.New("previews are removed with their pull request, not archived")
		}
		if err := m.retireBlockedLocked(id, a); err != nil {
			return "", err
		}
		snapshot := cloneJSON(a)
		snapshot.Retiring = false
		archive.App = &snapshot
		archive.Kind = archiveKindContainer
		if a.RuntimeProfile != "" || a.Current != nil && a.Current.Stack != nil {
			archive.Kind = archiveKindStack
		}
		archive.Runtime = a.Runtime
		if a.Current != nil {
			archive.Commit = a.Current.Commit
		}
		if selection := m.data.StackSelections[id]; selection != nil {
			s := cloneJSON(*selection)
			archive.StackSelection = &s
		}
		if c, ok := m.data.Caches[id]; ok && c.Mode != "" {
			c = cloneJSON(c)
			c.Previous, c.Operation, c.Error = nil, false, ""
			archive.Cache = &c
		}
		archive.Hostnames = a.Hostnames()
	} else if policy, ok := m.data.NativeCreated[id]; ok {
		if m.nativeOperations[id] {
			return "", errors.New("an operation on this application is running; delete it after it finishes")
		}
		if policy.Root != filepath.Join(m.nativeAppsRoot(), id) || policy.User != nativeAppUser(id) {
			return "", errors.New("the application's stored policy does not match its account; review it on the host")
		}
		native := ArchivedNative{App: cloneJSON(m.data.NativeApps[id]), Policy: cloneJSON(policy), Spec: cloneJSON(m.data.NativeCreatedSpecs[id]),
			Bindings: maps.Clone(m.data.NativeBindings[id])}
		if s, ok := m.data.NativeSettings[id]; ok {
			s = cloneJSON(s)
			native.Settings = &s
		}
		if r, ok := m.data.NativeReleases[id]; ok {
			native.Releases = &r
			archive.Commit = r.Current.Commit
		}
		archive.Native = &native
		archive.Kind = archiveKindNative
		archive.Runtime = policy.Runtime
		archive.Hostnames = append([]string{native.App.Domain}, native.App.AdditionalDomains...)
	} else if app, ok := m.data.NativeApps[id]; ok {
		if m.nativeOperations[id] || m.busy(id) {
			return "", errors.New("an operation on this application is running; delete it after it finishes")
		}
		if adopted == nil {
			return "", errors.New("the application's host setup could not be read; retry")
		}
		native := ArchivedNative{App: cloneJSON(app), Bindings: maps.Clone(m.data.NativeBindings[id]), Adopted: adopted}
		if s, ok := m.data.NativeSettings[id]; ok {
			s = cloneJSON(s)
			native.Settings = &s
		}
		if r, ok := m.data.NativeReleases[id]; ok {
			native.Releases = &r
			archive.Commit = r.Current.Commit
		}
		archive.Native = &native
		archive.Kind = archiveKindAdopted
		archive.Runtime = cmpOr(app.Runtime, "rails")
		archive.Hostnames = append([]string{app.Domain}, app.AdditionalDomains...)
		for _, resource := range slices.Sorted(maps.Keys(m.data.NativeDatabases)) {
			if m.data.NativeDatabases[resource].Policy.AppID == id {
				archive.Databases = append(archive.Databases, resource)
			}
		}
	} else {
		return "", errArchiveNotFound
	}
	archive.Redirects = slices.Clone(m.data.Redirects[id])
	for _, host := range RedirectHosts(archive.Redirects) {
		if !slices.Contains(archive.Hostnames, host) {
			archive.Hostnames = append(archive.Hostnames, host)
		}
	}
	for _, t := range m.data.Tasks {
		if t.AppID == id {
			archive.Tasks = append(archive.Tasks, cloneJSON(t))
		}
	}
	slices.SortFunc(archive.Tasks, func(a, b Task) int { return strings.Compare(a.ID, b.ID) })
	settings := ArchiveItem{Kind: "settings", Name: id}
	if archive.App != nil {
		settings.Detail = fmt.Sprintf("%d environment variables, %d hostnames, %d redirect domains, %d scheduled tasks", len(archive.App.Env), len(archive.App.Hostnames()), len(archive.Redirects), len(archive.Tasks))
	} else if archive.Native.Adopted != nil {
		settings.Detail = fmt.Sprintf("%s application adopted from the host setup, %d units, %d hostnames, %d redirect domains", archive.Runtime, len(archive.Native.Adopted.Units), len(archive.Hostnames)-len(archive.Redirects), len(archive.Redirects))
	} else {
		settings.Detail = fmt.Sprintf("%s application, %d processes, %d hostnames, %d redirect domains", archive.Runtime, len(archive.Native.Spec.Processes), len(archive.Hostnames)-len(archive.Redirects), len(archive.Redirects))
	}
	archive.Kept = []ArchiveItem{settings}
	archive.Log = []ArchiveEvent{{At: now, Message: "Deletion started."}}
	if m.data.AppArchives == nil {
		m.data.AppArchives = map[string]AppArchive{}
	}
	m.data.AppArchives[archive.ID] = archive
	if err := m.save(); err != nil {
		delete(m.data.AppArchives, archive.ID)
		return "", err
	}
	return archive.ID, nil
}

// retireBlockedLocked refuses removal while work on the application runs.
func (m *Manager) retireBlockedLocked(id string, a App) error {
	if m.removing[id] {
		return errors.New("application cleanup is already running")
	}
	for _, dbID := range a.Bindings {
		if d, ok := m.data.Databases[dbID]; ok && d.Operation != "" {
			return errors.New("wait for the database operation to finish")
		}
	}
	for _, t := range m.data.Tasks {
		if t.AppID == id && t.Running && t.Mode == "schedule" {
			return errors.New("wait for scheduled command to finish")
		}
	}
	if m.busy(id) || a.Restoring || m.data.Caches[id].Operation {
		return errors.New("application has an active deployment; delete it after it finishes")
	}
	return nil
}

func (m *Manager) archiveSnapshot(id string) AppArchive {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneJSON(m.data.AppArchives[id])
}

// runArchiveDelete runs the deletion's steps; each records its completion.
func (m *Manager) runArchiveDelete(ctx context.Context, archiveID string, req AppArchiveRequest) error {
	a := m.archiveSnapshot(archiveID)
	if a.Kind == archiveKindAdopted {
		if err := m.archiveAdoptedApp(ctx, archiveID); err != nil {
			return err
		}
	} else if a.Kind == archiveKindNative {
		if err := m.archiveNativeApp(ctx, archiveID); err != nil {
			return err
		}
	} else if err := m.archiveStep(archiveID, "application", func() error { return m.archiveContainerApp(ctx, archiveID) }); err != nil {
		return err
	}
	steps := []struct {
		name string
		run  func() error
	}{
		{"cache", func() error { return m.archiveDeleteCache(ctx, archiveID) }},
		{"databases", func() error { return m.archiveStopDatabases(ctx, archiveID) }},
		{"backups", func() error { return m.archiveDeleteBackups(ctx, archiveID, req.ExclusiveBackupPrefixes) }},
		{"records", func() error { return m.archiveAdoptedRecords(archiveID) }},
		{"ingress", func() error { return m.archiveWithdrawIngress(ctx, archiveID) }},
		{"certificates", func() error { return m.archiveDeleteCertificates(ctx, archiveID) }},
		{"ports", func() error { return m.archiveReleasePorts(archiveID) }},
		{"move-copies", func() error { return m.archiveDeleteMoveCopies(ctx, archiveID) }},
		{"kept", func() error { return m.archiveRecordKept(archiveID) }},
	}
	for _, step := range steps {
		if err := m.archiveStep(archiveID, step.name, step.run); err != nil {
			return err
		}
	}
	return nil
}

// archiveContainerApp removes a container or stack application with the
// removal every application has (retire.go): routes, releases with their
// containers and images, task containers, service TLS copies, the gateway
// identity. Its data becomes a retained resource that the archive holds.
func (m *Manager) archiveContainerApp(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	key, err := m.retire(ctx, a.AppID, nil)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if key == "" {
		// A resumed deletion: retire ran before and the application is gone.
		for k, r := range m.data.RetainedResources {
			if r.AppID == a.AppID && r.Archive == "" && r.Origin == nil && !r.Created.Before(a.DeletedAt.Add(-time.Minute)) {
				key = k
			}
		}
	}
	return m.holdRetainedLocked(archiveID, key)
}

// holdRetainedLocked makes the retained resource key the archive's data.
func (m *Manager) holdRetainedLocked(archiveID, key string) error {
	archive := m.data.AppArchives[archiveID]
	previous := archive
	r, ok := m.data.RetainedResources[key]
	if key != "" && ok {
		archive.Retained = key
		archive.Databases = slices.Clone(r.Databases)
		if r.NativeData {
			if record, ok := m.data.NativeHostData[archive.AppID]; ok {
				for _, resource := range record.databaseResources() {
					if !slices.Contains(archive.Databases, resource) {
						archive.Databases = append(archive.Databases, resource)
					}
				}
			}
		}
		r.Archive = archiveID
		m.data.RetainedResources[key] = r
	}
	m.data.AppArchives[archiveID] = archive
	if err := m.save(); err != nil {
		m.data.AppArchives[archiveID] = previous
		if ok {
			r.Archive = ""
			m.data.RetainedResources[key] = r
		}
		return err
	}
	return nil
}

// archiveNativeApp removes a native application this agent created: its
// units, route, account and state. The data services stay with logins
// disabled; the root moves under the apps root's .archived directory.
func (m *Manager) archiveNativeApp(ctx context.Context, archiveID string) error {
	nativeCreateMu.Lock()
	defer nativeCreateMu.Unlock()
	a := m.archiveSnapshot(archiveID)
	id, native := a.AppID, a.Native
	m.mu.Lock()
	if m.nativeOperations[id] {
		m.mu.Unlock()
		return errors.New("an operation on this application is running")
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[id] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.nativeOperations, id); m.mu.Unlock() }()

	steps := []struct {
		name string
		run  func() error
	}{
		{"units", func() error {
			if err := m.removeNativeUnits(ctx, native.Policy.Units); err != nil {
				return fmt.Errorf("could not remove the application's units: %w", err)
			}
			m.archiveDeleted(archiveID, ArchiveItem{Kind: "units", Name: strings.Join(native.Policy.Units, ", "), Count: len(native.Policy.Units), Detail: "systemd units"})
			return nil
		}},
		{"route", func() error {
			return m.withdrawNativeCaddyRoute(ctx, id, append([]string{native.App.Domain}, native.App.AdditionalDomains...))
		}},
		{"data", func() error {
			key, err := retainNativeData(ctx, m, id, nil)
			if err != nil && !errors.Is(err, errNativeCreateUnavailable) {
				return fmt.Errorf("data services: %w", err)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.holdRetainedLocked(archiveID, key)
		}},
		{"logins", func() error { return m.archiveDisableHostLogins(ctx, archiveID) }},
		{"account", func() error {
			if err := m.removeNativeAppAccount(ctx, id); err != nil {
				return err
			}
			m.archiveDeleted(archiveID, ArchiveItem{Kind: "account", Name: nativeAppUser(id), Detail: "system user and its crontab"})
			return nil
		}},
		{"files", func() error { return m.archiveNativeFiles(archiveID) }},
		{"state", func() error {
			m.mu.Lock()
			for _, table := range []func(){
				func() { delete(m.data.NativeCreated, id) }, func() { delete(m.data.NativeCreatedSpecs, id) }, func() { delete(m.data.NativeApps, id) },
				func() { delete(m.data.NativeSettings, id) }, func() { delete(m.data.NativeReleases, id) }, func() { delete(m.data.NativeBindings, id) },
				func() { delete(m.data.NativeTransitions, id) }, func() { delete(m.data.NativeRuby, id) }, func() { delete(m.data.BuildPeaks, id) },
				func() { delete(m.data.NativeStopped, id) },
			} {
				table()
			}
			for key, t := range m.data.Tasks {
				if t.AppID == id {
					delete(m.data.Tasks, key)
				}
			}
			dropped := m.dropRedirectsLocked(id)
			err := m.save()
			m.mu.Unlock()
			if err != nil {
				return err
			}
			if dropped {
				m.withdrawRedirects(ctx, id)
			}
			if m.serviceTLS != nil {
				if err := m.serviceTLS.retire(id); err != nil {
					return errors.New("copied service certificates could not be removed")
				}
			}
			return nil
		}},
	}
	for _, step := range steps {
		if err := m.archiveStep(archiveID, "native-"+step.name, step.run); err != nil {
			return err
		}
	}
	return nil
}

// archiveNativeFiles moves the application root out of the apps root,
// into a directory only root can enter, so nothing can use the files while
// the account is gone and a new application cannot pick them up.
func (m *Manager) archiveNativeFiles(archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	root := a.Native.Policy.Root
	holder := filepath.Join(m.nativeAppsRoot(), ".archived", archiveID)
	target := filepath.Join(holder, "root")
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(target); err == nil {
			return m.updateArchive(archiveID, func(x *AppArchive) { x.Native.Files = target })
		}
		return nil
	}
	if err := os.MkdirAll(holder, 0o700); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Dir(holder), holder} {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	if err := os.Rename(root, target); err != nil {
		return fmt.Errorf("could not move the application files: %w", err)
	}
	if err := m.updateArchive(archiveID, func(x *AppArchive) { x.Native.Files = target }); err != nil {
		return err
	}
	// The Redis data is the application's cache: deleted, the instance is
	// recreated empty on restore.
	m.mu.Lock()
	record, hasRedis := m.data.NativeHostData[a.AppID]
	m.mu.Unlock()
	if hasRedis && record.Redis != nil {
		h := nativeHostDataHost(m)
		ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
		defer cancel()
		if err := h.removeRedis(ctx, a.AppID); err != nil {
			return err
		}
		if err := os.RemoveAll(nativeRedisDir(target)); err != nil {
			return fmt.Errorf("could not delete the Redis data: %w", err)
		}
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "cache", Name: nativeRedisUnit(a.AppID), Detail: "Redis instance and its data"})
	}
	m.archiveKept(archiveID, ArchiveItem{Kind: "directory", Name: target, Detail: "application files (" + root + ")"})
	return nil
}

// archiveDisableHostLogins disables the logins of the host databases a
// native application had: its PostgreSQL role and ClickHouse user. Their
// sessions are ended; the data stays.
func (m *Manager) archiveDisableHostLogins(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	m.mu.Lock()
	record, ok := m.data.NativeHostData[a.AppID]
	server := m.data.NativeClickHouse
	m.mu.Unlock()
	if !ok {
		return nil
	}
	h := nativeHostDataHost(m)
	if pg := record.Postgres; pg != nil {
		script, err := renderNativePGLoginSQL(pg.Name, false)
		if err != nil {
			return err
		}
		if _, err := h.psql(ctx, pg.Port, script); err != nil {
			return errors.New("disabling the PostgreSQL login failed; retry")
		}
		m.archiveKept(archiveID, ArchiveItem{Kind: "database", Name: pg.Resource, Detail: "host PostgreSQL database " + pg.Name + ", login disabled"})
	}
	if ch := record.ClickHouse; ch != nil {
		if server != nil && server.Installed {
			admin, err := m.unsealHostSecret(server.Password)
			if err != nil {
				return err
			}
			client := h.clickhouse(*server, "default", admin)
			for _, statement := range renderNativeClickHouseLoginSQL(ch.Name, false) {
				if err := client.exec(ctx, statement); err != nil {
					return fmt.Errorf("disabling the ClickHouse login failed; retry: %w", err)
				}
			}
		}
		m.archiveKept(archiveID, ArchiveItem{Kind: "clickhouse", Name: ch.Resource, Detail: "host ClickHouse database " + ch.Name + ", login disabled"})
	}
	// Not ready: no backup or release uses the databases while archived.
	return m.updateHostDataRecord(a.AppID, func(r *NativeHostDataRecord) {
		if r.Postgres != nil {
			r.Postgres.Ready = false
		}
		if r.ClickHouse != nil {
			r.ClickHouse.Ready = false
		}
	})
}

// renderNativePGLoginSQL disables (and ends the sessions of) or enables the
// login of an application's PostgreSQL role.
func renderNativePGLoginSQL(name string, login bool) (string, error) {
	ident, err := pgQuoteIdent(name)
	if err != nil {
		return "", err
	}
	literal, _ := pgQuoteLiteral(name)
	if login {
		return fmt.Sprintf("ALTER ROLE %s LOGIN;\n", ident), nil
	}
	return fmt.Sprintf("ALTER ROLE %s NOLOGIN;\nSELECT count(pg_catalog.pg_terminate_backend(pid)) FROM pg_catalog.pg_stat_activity WHERE datname = %s OR usename = %s;\n", ident, literal, literal), nil
}

// renderNativeClickHouseLoginSQL disables a user (no host may connect, its
// queries end) or allows it again from loopback, as provisioned.
func renderNativeClickHouseLoginSQL(name string, login bool) []string {
	ident := mustCHIdent(name)
	if login {
		return []string{"ALTER USER " + ident + " HOST IP '127.0.0.1', IP '::1'"}
	}
	return []string{"ALTER USER " + ident + " HOST NONE", "KILL QUERY WHERE user = " + mustCHString(name) + " ASYNC"}
}

// archiveDeleteCache deletes a container application's local cache (its
// Valkey container, network and volume); an external cache only loses the
// connection, which the settings keep for a restore.
func (m *Manager) archiveDeleteCache(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	if a.Kind == archiveKindNative || a.Kind == archiveKindAdopted {
		return nil
	}
	m.mu.Lock()
	r, ok := m.data.RetainedResources[a.Retained]
	m.mu.Unlock()
	if !ok || r.Cache.Mode == "" {
		return nil
	}
	if r.Cache.Mode == "local" {
		for _, kind := range []string{"container", "network", "volume"} {
			for _, o := range cacheObjects(r.Cache) {
				if o.kind != kind {
					continue
				}
				if _, err := removeDockerObject(ctx, o); err != nil {
					return err
				}
			}
		}
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "cache", Name: r.Cache.Network, Detail: "Valkey container, network and data volume"})
	} else {
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "cache", Name: "external", Detail: "connection to the external cache; the service itself is not managed here"})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.data.RetainedResources[a.Retained]
	previous := current
	current.Cache = CacheResource{}
	m.data.RetainedResources[a.Retained] = current
	if err := m.save(); err != nil {
		m.data.RetainedResources[a.Retained] = previous
		return err
	}
	return nil
}

// archiveStopDatabases stops the containers of a container application's
// databases and ClickHouse: nothing can log in, and the data stays.
func (m *Manager) archiveStopDatabases(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	if a.Kind == archiveKindNative || a.Kind == archiveKindAdopted {
		return nil
	}
	m.mu.Lock()
	r := m.data.RetainedResources[a.Retained]
	var containers []dockerObject
	var kept []ArchiveItem
	for _, dbID := range a.Databases {
		d, ok := m.data.Databases[dbID]
		switch {
		case !ok:
		case d.Mode == "local" && d.Network != "":
			containers = append(containers, databaseObjects(d)[0])
			kept = append(kept, ArchiveItem{Kind: "volume", Name: d.Network + "-data", Detail: "PostgreSQL database " + dbID + ", container stopped"})
		default:
			kept = append(kept, ArchiveItem{Kind: "database", Name: dbID, Detail: "external database; not managed here, its connection stays in the settings"})
		}
	}
	if r.ClickHouse.Network != "" {
		containers = append(containers, clickhouseObjects(r.ClickHouse)[0])
		kept = append(kept, ArchiveItem{Kind: "volume", Name: r.ClickHouse.Network + "-data", Detail: "ClickHouse data, container stopped"})
	}
	if r.StorageVolume != "" {
		kept = append(kept, ArchiveItem{Kind: "volume", Name: r.StorageVolume, Detail: "application files"})
	}
	for _, volume := range r.StackVolumes {
		kept = append(kept, ArchiveItem{Kind: "volume", Name: volume, Detail: "stack volume"})
	}
	m.mu.Unlock()
	for _, o := range containers {
		if err := stopDockerContainer(ctx, o); err != nil {
			return err
		}
	}
	return m.updateArchive(archiveID, func(x *AppArchive) {
		for _, item := range kept {
			addArchiveItem(&x.Kept, item)
		}
	})
}

// stopDockerContainer stops a container this agent owns; a missing one is
// already stopped.
func stopDockerContainer(ctx context.Context, o dockerObject) error {
	found, err := command(ctx, "", nil, "docker", "ps", "-a", "--filter", "name=^/"+o.name+"$", "--format", "{{.ID}}")
	if err != nil {
		return fmt.Errorf("could not inspect container %s", o.name)
	}
	if strings.TrimSpace(found) == "" {
		return nil
	}
	if _, err := command(ctx, "", nil, "docker", "update", "--restart", "no", o.name); err != nil {
		return fmt.Errorf("could not disable the restart policy of %s", o.name)
	}
	if _, err := command(ctx, "", nil, "docker", "stop", "--time", "30", o.name); err != nil {
		return fmt.Errorf("could not stop container %s", o.name)
	}
	return nil
}

// archiveDeleteBackups deletes every backup of the application's
// databases: the local dumps and the off-site objects. A database whose
// off-site folder the panel confirmed as this server's alone loses the
// whole folder; others the objects their records name.
func (m *Manager) archiveDeleteBackups(ctx context.Context, archiveID string, exclusive []string) error {
	a := m.archiveSnapshot(archiveID)
	m.mu.Lock()
	var target *storage.Config
	if m.data.BackupStorage != nil {
		copy := *m.data.BackupStorage
		target = &copy
	}
	m.mu.Unlock()
	var store archiveObjectStore
	if target != nil {
		var err error
		if store, err = newArchiveObjectStore(*target); err != nil {
			return errors.New("off-site storage configuration unavailable")
		}
	}
	for _, dbID := range a.Databases {
		m.mu.Lock()
		d, ok := m.data.Databases[dbID]
		m.mu.Unlock()
		if !ok {
			continue
		}
		var localFiles, offsite int
		var localBytes, offsiteBytes int64
		for _, dir := range []string{m.backupDir(d.AppID), m.backupDir(dbID)} {
			entries, err := os.ReadDir(dir)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("could not read the backups of %s", dbID)
			}
			for _, entry := range entries {
				if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
					localFiles++
					localBytes += info.Size()
				}
			}
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("could not delete the local backups of %s", dbID)
			}
		}
		var missed []string
		named := map[string]bool{}
		for _, b := range d.Backups {
			if b.Offsite && b.ObjectKey != "" {
				named[b.ObjectKey] = true
			}
		}
		if store != nil {
			if slices.Contains(exclusive, dbID) {
				for pass := 0; pass < 100; pass++ {
					var objects []storage.Object
					if err := withStorageRetry(ctx, func() (err error) {
						objects, err = store.List(ctx, retention.Prefix(dbID), archiveListLimit)
						return err
					}); err != nil {
						return offsiteFailure(err, "list the off-site backups of "+dbID, "ListBucket")
					}
					if len(objects) == 0 {
						break
					}
					for _, o := range objects {
						if err := withStorageRetry(ctx, func() error { return store.Delete(ctx, o.Key) }); err != nil && !errors.Is(err, storage.ErrNotFound) {
							return offsiteFailure(err, "delete the off-site backup "+o.Key, "DeleteObject")
						}
						delete(named, o.Key)
						offsite++
						offsiteBytes += o.Size
					}
				}
			}
			for _, key := range slices.Sorted(maps.Keys(named)) {
				if db, _, ok := retention.ParseKey(key); !ok || db != dbID {
					continue
				}
				if err := withStorageRetry(ctx, func() error { return store.Delete(ctx, key) }); err != nil && !errors.Is(err, storage.ErrNotFound) {
					return offsiteFailure(err, "delete the off-site backup "+key, "DeleteObject")
				}
				offsite++
				for _, b := range d.Backups {
					if b.ObjectKey == key {
						offsiteBytes += b.Size
					}
				}
			}
		} else {
			missed = slices.Sorted(maps.Keys(named))
		}
		m.mu.Lock()
		current := m.data.Databases[dbID]
		previous := current
		current.Backups = nil
		current.NextBackup = time.Time{}
		m.data.Databases[dbID] = current
		err := m.save()
		if err != nil {
			m.data.Databases[dbID] = previous
		}
		m.mu.Unlock()
		if err != nil {
			return err
		}
		detail := fmt.Sprintf("%d local (%s), %d off-site (%s)", localFiles, retentionSize(localBytes), offsite, retentionSize(offsiteBytes))
		if len(missed) > 0 {
			detail += fmt.Sprintf("; %d off-site objects could not be reached because this server has no off-site storage configured: %s", len(missed), strings.Join(missed, ", "))
		}
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "backups", Name: dbID, Count: localFiles + offsite, SizeBytes: localBytes + offsiteBytes, Detail: detail})
	}
	// Its backups are gone, so are the lock files that serialized them.
	return m.archiveRemoveBackupLocks(archiveID)
}

// archiveWithdrawIngress removes the application's Caddy route and TLS
// subjects, and its redirect routes, for every runtime: whatever
// published them (the gateway, a native release, redirects).
func (m *Manager) archiveWithdrawIngress(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	bounded, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	own := slices.DeleteFunc(slices.Clone(a.Hostnames), func(h string) bool { return slices.Contains(RedirectHosts(a.Redirects), h) })
	if err := m.withdrawNativeCaddyRoute(bounded, a.AppID, own); err != nil {
		return fmt.Errorf("could not withdraw the Caddy route: %w", err)
	}
	if caddy, ok := m.nativeCaddy(bounded); ok {
		if err := caddy.withdrawStoppedRoute(bounded, a.AppID); err != nil {
			return fmt.Errorf("could not withdraw the stopped page: %w", err)
		}
	}
	if _, err := m.syncRedirectIngress(bounded, a.AppID); err != nil {
		return fmt.Errorf("could not withdraw the redirect route: %w", err)
	}
	m.archiveDeleted(archiveID, ArchiveItem{Kind: "route", Name: nativeCaddyRouteID(a.AppID), Count: len(own), Detail: "web route and TLS subjects of " + strings.Join(own, ", ")})
	if len(a.Redirects) > 0 {
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "redirects", Name: redirectRoutePrefix + a.AppID, Count: len(a.Redirects), Detail: strings.Join(RedirectHosts(a.Redirects), ", ")})
	}
	return nil
}

// archiveDeleteCertificates deletes the certificates of the application's
// hostnames and redirect hosts from Caddy's storage, except those another
// application, redirect or the control panel still uses.
func (m *Manager) archiveDeleteCertificates(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	root := archiveCaddyStorage(ctx, m)
	if root == "" {
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "certificates", Name: "caddy", Detail: "Caddy's certificate storage was not found on disk; the certificates expire on their own"})
		return nil
	}
	control := m.controlState()
	issuers, err := os.ReadDir(filepath.Join(root, "certificates"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("could not read Caddy's certificate storage")
	}
	for _, host := range a.Hostnames {
		host = strings.ToLower(host)
		if !controlHostnamePattern.MatchString(host) {
			continue
		}
		m.mu.Lock()
		owner := m.hostnameOwnerLocked(host, "", control)
		m.mu.Unlock()
		if owner != "" {
			m.archiveDeleted(archiveID, ArchiveItem{Kind: "certificate", Name: host, Detail: "kept: also used by " + owner})
			continue
		}
		removed := 0
		for _, issuer := range issuers {
			if !issuer.IsDir() || !controlIssuerPattern.MatchString(issuer.Name()) {
				continue
			}
			dir := filepath.Join(root, "certificates", issuer.Name(), host)
			info, err := os.Lstat(dir)
			if err != nil || !info.IsDir() {
				continue
			}
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("could not delete the certificate of %s", host)
			}
			removed++
		}
		if removed > 0 {
			m.archiveDeleted(archiveID, ArchiveItem{Kind: "certificate", Name: host, Count: removed, Detail: "deleted from Caddy's storage"})
		}
	}
	return nil
}

// archiveReleasePorts unregisters published ports: their containers are
// gone, and the registry follows the live applications.
func (m *Manager) archiveReleasePorts(archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	if a.App == nil {
		return nil
	}
	var ports []string
	for _, reg := range releasePortRegistrations(a.App.Current) {
		for _, p := range reg.Ports {
			proto := p.Protocol
			if proto == "" {
				proto = "tcp"
			}
			ports = append(ports, strconv.Itoa(p.Port)+"/"+proto)
		}
	}
	if len(ports) == 0 {
		return nil
	}
	m.reconcilePublicPorts()
	m.archiveDeleted(archiveID, ArchiveItem{Kind: "ports", Name: strings.Join(ports, ", "), Count: len(ports), Detail: "published ports and their firewall openings"})
	return nil
}

// archiveDeleteMoveCopies deletes the data moves left of the application on
// this server: copies, never the live data the archive keeps.
func (m *Manager) archiveDeleteMoveCopies(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	m.mu.Lock()
	var keys []string
	for key, r := range m.data.RetainedResources {
		if r.AppID == a.AppID && key != a.Retained && r.Archive == "" && r.Origin != nil && r.Origin.MoveID != "" {
			keys = append(keys, key)
		}
	}
	m.mu.Unlock()
	slices.Sort(keys)
	for _, key := range keys {
		if err := m.removeRetainedForced(ctx, key, "archive", false); err != nil {
			return fmt.Errorf("deleting the move copy %s: %w", key, err)
		}
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "move-copy", Name: key, Detail: "data a move left on this server"})
	}
	return nil
}

// archiveRecordKept lists the kept host data of a native application.
func (m *Manager) archiveRecordKept(archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	m.archiveDeleted(archiveID, ArchiveItem{Kind: "releases", Name: a.AppID, Detail: "release containers, images and build output; the next deploy rebuilds them"})
	if len(a.Tasks) > 0 {
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "tasks", Name: a.AppID, Count: len(a.Tasks), Detail: "scheduled tasks and workers (kept in the settings for a restore)"})
	}
	return nil
}
