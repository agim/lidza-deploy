package agent

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agim/lidza/packs/storage"
)

// Removing retained resources. A removed application (or the source of a
// finished move) leaves its databases, caches, ClickHouse service, file
// volume and stack volumes on this host as retained resources. They are
// listed with their sizes and the last backups of their databases, and an
// operator can remove one (DELETE /v1/retained-resources/{id}):
//
//   - nothing attached to a live application on this agent is touched;
//   - every database needs a verified backup taken after its application
//     stopped writing, unless the request takes a final backup first (and
//     uploads it off-site when off-site storage is configured);
//   - containers are stopped and removed, then their networks, then the
//     volumes; every object must carry the agent's ownership label;
//   - backup records and off-site objects stay: the database record keeps
//     its backup history (marked removed), and off-site copies follow the
//     retention policy;
//   - each removal is a host job in the agent's job history.

const (
	retainedKindApplication = "application"
	retainedKindDatabase    = "database"

	// retainedDatabasePrefix names a retained database that no removal
	// record holds (older removals, restored copies).
	retainedDatabasePrefix = "database-"
	retainedLegacyPrefix   = "legacy-"

	retainedSizeTTL     = 10 * time.Minute
	retainedSizeTimeout = 2 * time.Minute
	retainedRemoveLimit = 3 * time.Hour
	finalBackupKind     = "final"
)

// RetainedComponent is one Docker volume of a retained resource.
type RetainedComponent struct {
	Kind      string `json:"kind"` // database, cache, clickhouse, files, volume, redis
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	Measured  bool   `json:"measured"`
	// Host marks a data service on the host itself (a native application's
	// PostgreSQL database, Redis instance or ClickHouse database) rather
	// than a Docker volume; it is not measured as a volume.
	Host bool `json:"host,omitempty"`
}

// RetainedDatabaseView is one retained database with its newest backups.
type RetainedDatabaseView struct {
	ID          string        `json:"id"`
	Volume      string        `json:"volume"`
	LastLocal   *BackupRecord `json:"last_local_backup,omitempty"`
	LastOffsite *BackupRecord `json:"last_offsite_backup,omitempty"`
	// Covered reports a verified backup taken after the application stopped
	// writing; without one, removal needs a final backup.
	Covered    bool     `json:"covered"`
	AttachedTo []string `json:"attached_to,omitempty"`
	// Engine is postgresql or clickhouse; SizeBytes is its data volume's
	// measured size (0 for a host database or until measured).
	Engine    string `json:"engine"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// RetainedRemoveRequest is the body of DELETE /v1/retained-resources/{id}.
type RetainedRemoveRequest struct {
	// Confirm repeats the resource id.
	Confirm string `json:"confirm"`
	// FinalBackup takes a backup of every database first.
	FinalBackup bool `json:"final_backup"`
	// MoveLeftover removes data a move left (a resource with an origin)
	// without a covering backup: the panel sends it when the application
	// the data was copied from is deleted.
	MoveLeftover bool `json:"move_leftover,omitempty"`
}

type dockerObject struct {
	kind  string // container, network or volume
	name  string
	label string // ownership label key
	owner string // expected label value; "" accepts any non-empty value
	role  string // component kind of a volume
}

// retainedItem is one removable retained resource, captured under m.mu.
type retainedItem struct {
	id            string
	kind          string
	appID         string
	created       time.Time
	writesStopped time.Time
	record        bool // a RetainedResources entry
	legacy        bool
	databases     []string
	objects       []dockerObject
	// native is the application whose provisioned host data services the
	// item holds (RetainedResource.NativeData), and host lists them.
	native string
	host   []RetainedComponent
	// origin is the move the data came from; copy marks a never-switched
	// move's copy, which needs no covering backup.
	origin *RetainedOrigin
	copy   bool
}

func (it retainedItem) volumes() []dockerObject {
	var out []dockerObject
	for _, o := range it.objects {
		if o.kind == "volume" {
			out = append(out, o)
		}
	}
	return out
}

// retainedRuntime is per-manager runtime state: removals in progress and
// measured volume sizes.
type retainedRuntime struct {
	mu        sync.Mutex
	removing  map[string]bool
	sizes     map[string]retainedSize
	measuring bool
}

type retainedSize struct {
	bytes   int64
	at      time.Time
	ok      bool
	created time.Time // the volume's creation time, when Docker reports it
}

var retainedRuntimes sync.Map // *Manager -> *retainedRuntime

func (m *Manager) retainedState() *retainedRuntime {
	value, _ := retainedRuntimes.LoadOrStore(m, &retainedRuntime{removing: map[string]bool{}, sizes: map[string]retainedSize{}})
	return value.(*retainedRuntime)
}

// appDatabaseIDsLocked lists the databases an application uses: bound or
// recorded by its current and previous release.
func (m *Manager) appDatabaseIDsLocked(a App) []string {
	var out []string
	add := func(id string) {
		if _, ok := m.data.Databases[id]; ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	for _, id := range a.Bindings {
		add(id)
	}
	for _, release := range []*Release{a.Current, a.Previous} {
		if release != nil {
			for _, id := range release.DatabaseIDs {
				add(id)
			}
		}
	}
	slices.Sort(out)
	return out
}

// removableDatabaseLocked reports a database whose data lives in a
// container on this host and is still there.
func (m *Manager) removableDatabaseLocked(id string) bool {
	d, ok := m.data.Databases[id]
	if !ok || !d.Removed.IsZero() || d.Mode != "local" || d.Network == "" || d.Ephemeral {
		return false
	}
	_, native := m.data.NativeDatabases[id]
	return !native
}

func databaseObjects(d Database) []dockerObject {
	return []dockerObject{
		{kind: "container", name: d.Network, label: "io.lidza.database", owner: d.AppID},
		{kind: "network", name: d.Network, label: "io.lidza.database", owner: d.AppID},
		{kind: "volume", name: d.Network + "-data", label: "io.lidza.database", owner: d.AppID, role: "database"},
	}
}

func cacheObjects(c CacheResource) []dockerObject {
	var out []dockerObject
	for _, cache := range []*CacheResource{&c, c.Previous} {
		if cache == nil || cache.Mode != "local" || cache.Network == "" {
			continue
		}
		out = append(out,
			dockerObject{kind: "container", name: cache.Network, label: "io.lidza.cache", owner: cache.Network},
			dockerObject{kind: "network", name: cache.Network, label: "io.lidza.cache", owner: cache.Network},
			dockerObject{kind: "volume", name: cache.Network + "-data", label: "io.lidza.cache", owner: cache.Network, role: "cache"})
	}
	return out
}

func clickhouseObjects(c ClickHouseResource) []dockerObject {
	if c.Network == "" {
		return nil
	}
	return []dockerObject{
		{kind: "container", name: c.Network, label: "io.lidza.clickhouse", owner: c.Network},
		{kind: "network", name: c.Network, label: "io.lidza.clickhouse", owner: c.Network},
		{kind: "volume", name: c.Network + "-data", label: "io.lidza.clickhouse", owner: c.Network, role: "clickhouse"},
		{kind: "volume", name: c.Network + "-logs", label: "io.lidza.clickhouse", owner: c.Network, role: "clickhouse"},
	}
}

func appDataObjects(storageVolume string, stack []string, cache CacheResource, clickhouse ClickHouseResource) []dockerObject {
	out := append(cacheObjects(cache), clickhouseObjects(clickhouse)...)
	if storageVolume != "" {
		out = append(out, dockerObject{kind: "volume", name: storageVolume, label: "io.lidza.storage", role: "files"})
	}
	for _, volume := range stack {
		out = append(out, dockerObject{kind: "volume", name: volume, label: "io.lidza.stack", role: "volume"})
	}
	return out
}

// retainedItemsLocked lists every removable retained resource; the data
// of deleted applications is managed through their archives.
func (m *Manager) retainedItemsLocked() []retainedItem { return m.retainedItemsFilterLocked(false) }

func (m *Manager) retainedItemsFilterLocked(includeArchived bool) []retainedItem {
	var out []retainedItem
	held := map[string]bool{}
	for _, r := range m.data.RetainedResources {
		it := retainedItem{id: r.ID, kind: retainedKindApplication, appID: r.AppID, created: r.Created, writesStopped: r.WritesStopped, record: true,
			objects: appDataObjects(r.StorageVolume, r.StackVolumes, r.Cache, r.ClickHouse), origin: r.Origin, copy: r.Origin.copied()}
		for _, id := range r.Databases {
			if m.removableDatabaseLocked(id) {
				held[id] = true
				it.databases = append(it.databases, id)
				it.objects = append(databaseObjects(m.data.Databases[id]), it.objects...)
			}
		}
		if r.NativeData {
			m.retainedNativeItemLocked(&it)
		}
		if r.Archive != "" && !includeArchived {
			continue
		}
		out = append(out, it)
	}
	legacy := map[string]bool{}
	for id := range m.data.StorageVolumes {
		legacy[id] = true
	}
	for id := range m.data.ClickHouses {
		legacy[id] = true
	}
	for id := range m.data.Caches {
		legacy[id] = true
	}
	for id := range legacy {
		if _, live := m.data.Apps[id]; live {
			continue
		}
		out = append(out, retainedItem{id: retainedLegacyPrefix + id, kind: retainedKindApplication, appID: id, legacy: true,
			objects: appDataObjects(m.data.StorageVolumes[id], m.data.StackVolumes[id], m.data.Caches[id], m.data.ClickHouses[id])})
	}
	for id, d := range m.data.Databases {
		if held[id] || !m.removableDatabaseLocked(id) || m.databaseInUseLocked(id) {
			continue
		}
		out = append(out, retainedItem{id: retainedDatabasePrefix + id, kind: retainedKindDatabase, appID: id, databases: []string{id}, objects: databaseObjects(d)})
	}
	slices.SortFunc(out, func(a, b retainedItem) int { return cmp.Compare(a.id, b.id) })
	return out
}

// attachedLocked lists the live applications that use any part of item.
func (m *Manager) attachedLocked(it retainedItem) []string {
	var out []string
	add := func(id string) {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	for _, dbID := range it.databases {
		for _, ref := range m.attachmentsLocked(dbID) {
			add(ref.AppID)
		}
		for _, a := range m.data.Apps {
			if !a.Retiring && a.Current != nil && slices.Contains(a.Current.DatabaseIDs, dbID) {
				add(a.ID)
			}
		}
	}
	for appID, a := range m.data.Apps {
		if a.Retiring {
			continue
		}
		used := map[string]bool{}
		for _, o := range appDataObjects(m.data.StorageVolumes[appID], m.data.StackVolumes[appID], m.data.Caches[appID], m.data.ClickHouses[appID]) {
			used[o.kind+"/"+o.name] = true
		}
		for _, o := range it.objects {
			if used[o.kind+"/"+o.name] {
				add(appID)
			}
		}
	}
	if _, live := m.data.NativeApps[it.native]; it.native != "" && live {
		// The application has not finished its removal, or was registered
		// again under the same id: its data services are in use.
		add(it.native)
	}
	slices.Sort(out)
	return out
}

// retainedNativeItemLocked adds the data services a removed native
// application left on the host to its retained item: the databases that
// still exist (for the backup checks) and one host component per service.
func (m *Manager) retainedNativeItemLocked(it *retainedItem) {
	it.native = it.appID
	record, ok := m.data.NativeHostData[it.appID]
	if !ok {
		return
	}
	for _, resource := range record.databaseResources() {
		if d, exists := m.data.Databases[resource]; exists && d.Removed.IsZero() && !slices.Contains(it.databases, resource) {
			it.databases = append(it.databases, resource)
		}
	}
	if pg := record.Postgres; pg != nil {
		it.host = append(it.host, RetainedComponent{Kind: "database", Name: pg.Name, Host: true})
	}
	if redis := record.Redis; redis != nil {
		it.host = append(it.host, RetainedComponent{Kind: "redis", Name: redis.Unit, Host: true})
	}
	if ch := record.ClickHouse; ch != nil {
		it.host = append(it.host, RetainedComponent{Kind: "clickhouse", Name: ch.Name, Host: true})
	}
}

// verifiedBackup reports whether a backup record is usable: an uploaded
// off-site copy, or a local dump of the recorded size.
func (m *Manager) verifiedBackup(d Database, b BackupRecord) bool {
	if b.ID == "" || b.SHA256 == "" {
		return false
	}
	if b.Offsite && b.ObjectKey != "" {
		return true
	}
	return m.localBackupPresent(d, b)
}

func (m *Manager) localBackupPresent(d Database, b BackupRecord) bool {
	if b.ID == "" || filepath.Base(b.ID) != b.ID {
		return false
	}
	info, err := os.Lstat(filepath.Join(m.backupDir(d.AppID), b.ID+".dump"))
	return err == nil && info.Mode().IsRegular() && info.Size() == b.Size && b.Size > 0
}

// newestBackups returns the newest local and off-site backups.
func (m *Manager) newestBackups(d Database) (local, offsite *BackupRecord) {
	for i := range d.Backups {
		b := d.Backups[i]
		if b.Offsite && b.ObjectKey != "" && (offsite == nil || b.Created.After(offsite.Created)) {
			offsite = &b
		}
		if m.localBackupPresent(d, b) && (local == nil || b.Created.After(local.Created)) {
			local = &b
		}
	}
	return local, offsite
}

// coveredLocked reports a verified backup taken at or after writesStopped,
// or a verified final backup (taken for the removal, after the data's
// application was gone). Without a known time (older records) only a
// final backup covers.
func (m *Manager) coveredLocked(id string, writesStopped time.Time) bool {
	d := m.data.Databases[id]
	for _, b := range d.Backups {
		if (b.Kind == finalBackupKind || !writesStopped.IsZero() && !b.Created.Before(writesStopped)) && m.verifiedBackup(d, b) {
			return true
		}
	}
	return false
}

func (m *Manager) retainedViews() []RetainedResourceView {
	rt := m.retainedState()
	m.mu.Lock()
	items := m.retainedItemsLocked()
	out := make([]RetainedResourceView, 0, len(items))
	for _, it := range items {
		view := RetainedResourceView{ID: it.id, AppID: it.appID, Created: it.created, Kind: it.kind, WritesStopped: it.writesStopped, Components: []RetainedComponent{}, AttachedTo: m.attachedLocked(it),
			Origin: it.origin, Copy: it.copy}
		switch {
		case it.record:
			r := m.data.RetainedResources[it.id]
			view.StackVolumes, view.StorageVolume, view.CacheMode, view.Error = r.StackVolumes, r.StorageVolume, r.Cache.Mode, r.Error
			if r.ClickHouse.Network != "" {
				view.ClickHouseVolume = r.ClickHouse.Network + "-data"
			}
			if r.Cache.Network != "" {
				view.CacheVolume = r.Cache.Network + "-data"
			}
		case it.legacy:
			cache := m.data.Caches[it.appID]
			view.StorageVolume, view.CacheMode, view.StackVolumes = m.data.StorageVolumes[it.appID], cache.Mode, m.data.StackVolumes[it.appID]
			if c := m.data.ClickHouses[it.appID]; c.Network != "" {
				view.ClickHouseVolume = c.Network + "-data"
			}
			if cache.Network != "" {
				view.CacheVolume = cache.Network + "-data"
			}
		default:
			view.Error = m.data.Databases[it.appID].Error
		}
		for _, id := range it.databases {
			d := m.data.Databases[id]
			db := RetainedDatabaseView{ID: id, Covered: m.coveredLocked(id, it.writesStopped), Engine: "postgresql"}
			if m.data.NativeDatabases[id].Policy.ClickHouseDatabase != "" {
				db.Engine = "clickhouse"
			}
			if d.Network != "" {
				db.Volume = d.Network + "-data"
			}
			db.LastLocal, db.LastOffsite = m.newestBackups(d)
			for _, ref := range m.attachmentsLocked(id) {
				db.AttachedTo = append(db.AttachedTo, ref.AppID)
			}
			if !db.Covered && !it.copy {
				view.FinalBackupRequired = true
			}
			view.Databases = append(view.Databases, db)
		}
		for _, o := range it.volumes() {
			view.Components = append(view.Components, RetainedComponent{Kind: o.role, Name: o.name})
		}
		view.Components = append(view.Components, it.host...)
		out = append(out, view)
	}
	m.mu.Unlock()
	rt.mu.Lock()
	for i := range out {
		out[i].Removing = rt.removing[out[i].ID]
		for j := range out[i].Components {
			c := &out[i].Components[j]
			if c.Host {
				continue
			}
			size, measured := rt.sizes[c.Name]
			switch {
			case !measured:
				out[i].SizePending = true
			case size.ok:
				c.SizeBytes, c.Measured = size.bytes, true
				out[i].SizeBytes += size.bytes
			}
			if !size.created.IsZero() && (out[i].DataSince.IsZero() || size.created.Before(out[i].DataSince)) {
				out[i].DataSince = size.created
			}
			for j := range out[i].Databases {
				if out[i].Databases[j].Volume == c.Name {
					out[i].Databases[j].SizeBytes = c.SizeBytes
				}
			}
		}
	}
	rt.mu.Unlock()
	return out
}

// ---- sizes ----------------------------------------------------------------------

// dockerVolumeSize measures a volume by walking its mount point.
var dockerVolumeSize = func(ctx context.Context, name string) (int64, error) {
	mount, err := command(ctx, "", nil, "docker", "volume", "inspect", "--format", "{{.Mountpoint}}", name)
	if err != nil {
		return 0, errors.New("volume unavailable")
	}
	mount = strings.TrimSpace(mount)
	if !filepath.IsAbs(mount) {
		return 0, errors.New("volume has no local mount point")
	}
	var total int64
	err = filepath.WalkDir(mount, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
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

// dockerVolumeCreated is a volume's creation time, or zero.
var dockerVolumeCreated = func(ctx context.Context, name string) time.Time {
	out, err := command(ctx, "", nil, "docker", "volume", "inspect", "--format", "{{.CreatedAt}}", name)
	if err != nil {
		return time.Time{}
	}
	created, err := time.Parse(time.RFC3339, strings.TrimSpace(out))
	if err != nil {
		return time.Time{}
	}
	return created.UTC()
}

// measureRetainedSizes measures the volumes whose sizes are missing or
// stale, one at a time.
func (m *Manager) measureRetainedSizes(ctx context.Context, force bool) {
	rt := m.retainedState()
	var names []string
	now := time.Now()
	for _, view := range m.retainedViews() {
		for _, c := range view.Components {
			if c.Host {
				continue
			}
			rt.mu.Lock()
			size, ok := rt.sizes[c.Name]
			rt.mu.Unlock()
			if force || !ok || now.Sub(size.at) > retainedSizeTTL {
				names = append(names, c.Name)
			}
		}
	}
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		bounded, cancel := context.WithTimeout(ctx, retainedSizeTimeout)
		bytes, err := dockerVolumeSize(bounded, name)
		created := dockerVolumeCreated(bounded, name)
		cancel()
		rt.mu.Lock()
		rt.sizes[name] = retainedSize{bytes: bytes, at: time.Now(), ok: err == nil, created: created}
		rt.mu.Unlock()
	}
}

// refreshRetainedSizes measures stale sizes in the background.
func (m *Manager) refreshRetainedSizes() {
	rt := m.retainedState()
	rt.mu.Lock()
	if rt.measuring || m.ctx.Err() != nil {
		rt.mu.Unlock()
		return
	}
	rt.measuring = true
	rt.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() { rt.mu.Lock(); rt.measuring = false; rt.mu.Unlock() }()
		m.measureRetainedSizes(m.ctx, false)
	}()
}

// ---- removal --------------------------------------------------------------------

// removeRetained validates a removal and starts it in the background; the
// refusals are returned before anything changes.
func (m *Manager) removeRetained(id string, req RetainedRemoveRequest) error {
	it, job, target, err := m.beginRetainedRemoval(id, req, "manual")
	if err != nil {
		return err
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, retainedRemoveLimit)
		defer cancel()
		output, err := m.runRetainedRemoval(ctx, it, req.FinalBackup, target)
		m.finishRetainedRemoval(it, job, output, err)
	}()
	return nil
}

// removeRetainedCopy removes, before it returns, a retained resource that a
// never-switched move left: its data is a copy of the source's, so no
// backup is taken or required. Application removal with
// AppRemoveRequest.DeleteData uses it right after retaining the data.
func (m *Manager) removeRetainedCopy(ctx context.Context, id string) error {
	it, job, target, err := m.beginRetainedRemoval(id, RetainedRemoveRequest{Confirm: id}, "move")
	if err != nil {
		return err
	}
	if !it.copy {
		m.finishRetainedRemoval(it, job, "", errors.New("the data is not a move's copy"))
		return errors.New("the data is not a move's copy; it stays retained")
	}
	output, err := m.runRetainedRemoval(ctx, it, false, target)
	m.finishRetainedRemoval(it, job, output, err)
	return err
}

// beginRetainedRemoval checks a removal and records its start: the item,
// its host job and the off-site target for final backups. The refusals are
// returned before anything changes.
func (m *Manager) beginRetainedRemoval(id string, req RetainedRemoveRequest, trigger string) (retainedItem, HostJob, *storage.Config, error) {
	if req.Confirm != id {
		return retainedItem{}, HostJob{}, nil, errors.New("confirm the retained resource id to remove it")
	}
	if req.MoveLeftover && req.FinalBackup {
		return retainedItem{}, HostJob{}, nil, errors.New("a move's leftover is removed without a final backup")
	}
	if m.upgradePending() {
		return retainedItem{}, HostJob{}, nil, errors.New("agent upgrade in progress")
	}
	rt := m.retainedState()
	m.mu.Lock()
	defer m.mu.Unlock()
	var item *retainedItem
	for _, it := range m.retainedItemsLocked() {
		if it.id == id {
			item = &it
			break
		}
	}
	if item == nil {
		return retainedItem{}, HostJob{}, nil, errRetainedNotFound
	}
	if req.MoveLeftover {
		if item.origin == nil || item.origin.MoveID == "" {
			return retainedItem{}, HostJob{}, nil, errors.New("this data was not left by a move; it needs a covering backup")
		}
		item.copy = true
	}
	rt.mu.Lock()
	busy := rt.removing[id]
	rt.mu.Unlock()
	if busy {
		return retainedItem{}, HostJob{}, nil, errors.New("this retained resource is already being removed")
	}
	if attached := m.attachedLocked(*item); len(attached) > 0 {
		return retainedItem{}, HostJob{}, nil, fmt.Errorf("attached to %s on this server; detach it first", strings.Join(attached, ", "))
	}
	for _, dbID := range item.databases {
		d := m.data.Databases[dbID]
		if d.Operation != "" {
			return retainedItem{}, HostJob{}, nil, fmt.Errorf("database %s has a %s running; retry after it finishes", dbID, d.Operation)
		}
		if req.FinalBackup && !d.Ready {
			return retainedItem{}, HostJob{}, nil, fmt.Errorf("database %s is not ready, so a final backup cannot be taken", dbID)
		}
		if !req.FinalBackup && !item.copy && !m.coveredLocked(dbID, item.writesStopped) {
			return retainedItem{}, HostJob{}, nil, fmt.Errorf("database %s has no verified backup taken after its application stopped writing; take a final backup first", dbID)
		}
	}
	previous := map[string]Database{}
	for _, dbID := range item.databases {
		d := m.data.Databases[dbID]
		previous[dbID] = d
		d.Operation = "remove"
		d.Error = ""
		d.Event = newID()
		m.data.Databases[dbID] = d
	}
	job := newHostJob("retained-removal", trigger, time.Now())
	job.Summary = "Removing retained " + item.kind + " " + item.appID + " (" + id + ")."
	m.recordHostJobLocked(job)
	if err := m.save(); err != nil {
		for dbID, d := range previous {
			m.data.Databases[dbID] = d
		}
		return retainedItem{}, HostJob{}, nil, err
	}
	rt.mu.Lock()
	rt.removing[id] = true
	rt.mu.Unlock()
	var target *storage.Config
	if m.data.BackupStorage != nil {
		copy := *m.data.BackupStorage
		target = &copy
	}
	return *item, job, target, nil
}

var errRetainedNotFound = errors.New("no such retained resource on this server")

// runRetainedRemoval takes the final backups, checks every database is
// covered, and removes the Docker objects.
func (m *Manager) runRetainedRemoval(ctx context.Context, it retainedItem, final bool, target *storage.Config) (string, error) {
	var log strings.Builder
	logf := func(format string, args ...any) { fmt.Fprintf(&log, format+"\n", args...) }
	if it.copy && !final && it.origin != nil {
		logf("The data is a copy made by move %s that never switched; no backup is required.", it.origin.MoveID)
	}
	for _, dbID := range it.databases {
		m.mu.Lock()
		d := m.data.Databases[dbID]
		m.mu.Unlock()
		if final {
			record, err := m.finalBackup(ctx, d, target)
			if record.ID != "" {
				m.mu.Lock()
				latest := m.data.Databases[dbID]
				latest.Backups = append(latest.Backups, record)
				m.data.Databases[dbID] = latest
				saveErr := m.save()
				m.mu.Unlock()
				if saveErr != nil && err == nil {
					err = saveErr
				}
			}
			if err != nil {
				return log.String(), fmt.Errorf("final backup of %s failed; nothing was removed: %w", dbID, err)
			}
			where := "local disk"
			if record.Offsite {
				where = "local disk and off-site storage"
			}
			logf("Final backup of %s: %s, %d bytes, on %s.", dbID, record.ID, record.Size, where)
			m.mu.Lock()
			d = m.data.Databases[dbID]
			m.mu.Unlock()
		}
		if final || !it.copy {
			if err := m.verifyCoveringBackup(d, it.writesStopped, final); err != nil {
				return log.String(), fmt.Errorf("database %s: %w; nothing was removed", dbID, err)
			}
		}
	}
	for _, kind := range []string{"container", "network", "volume"} {
		for _, o := range it.objects {
			if o.kind != kind {
				continue
			}
			removed, err := removeDockerObject(ctx, o)
			if err != nil {
				return log.String(), err
			}
			if removed {
				logf("Removed %s %s.", o.kind, o.name)
			}
		}
	}
	if it.native != "" && len(it.host) > 0 {
		if err := removeRetainedNativeHostData(ctx, m, it.native); err != nil {
			return log.String(), fmt.Errorf("removing the data services of %s: %w", it.native, err)
		}
		for _, c := range it.host {
			logf("Removed %s %s.", c.Kind, c.Name)
		}
	}
	return log.String(), nil
}

// finalBackup dumps a retained database, uploading the dump off-site when
// storage is configured.
func (m *Manager) finalBackup(ctx context.Context, d Database, target *storage.Config) (BackupRecord, error) {
	d.Backup.Offsite = target != nil
	select {
	case m.databaseSlots <- struct{}{}:
		defer func() { <-m.databaseSlots }()
	case <-ctx.Done():
		return BackupRecord{}, ctx.Err()
	}
	return m.dumpDatabase(ctx, d, target, finalBackupKind)
}

// verifyCoveringBackup checks the backup that allows removal: the newest
// covering record must be off-site, or a local dump whose SHA-256 still
// matches. A final backup taken by this removal always covers.
func (m *Manager) verifyCoveringBackup(d Database, writesStopped time.Time, final bool) error {
	var candidates []BackupRecord
	for _, b := range d.Backups {
		if b.Kind == finalBackupKind || (!writesStopped.IsZero() && !b.Created.Before(writesStopped)) {
			candidates = append(candidates, b)
		}
	}
	slices.SortFunc(candidates, func(a, b BackupRecord) int { return b.Created.Compare(a.Created) })
	for _, b := range candidates {
		if b.Offsite && b.ObjectKey != "" && b.SHA256 != "" {
			return nil
		}
		if m.localBackupPresent(d, b) && backupDigest(filepath.Join(m.backupDir(d.AppID), b.ID+".dump")) == b.SHA256 {
			return nil
		}
	}
	return errors.New("no verified backup taken after its application stopped writing")
}

func backupDigest(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// removeDockerObject removes one container, network or volume after
// checking its ownership label. A missing object is not an error.
func removeDockerObject(ctx context.Context, o dockerObject) (bool, error) {
	var found string
	var err error
	labelFormat := `{{index .Labels "` + o.label + `"}}`
	switch o.kind {
	case "container":
		found, err = command(ctx, "", nil, "docker", "ps", "-a", "--filter", "name=^/"+o.name+"$", "--format", "{{.ID}}")
		labelFormat = `{{index .Config.Labels "` + o.label + `"}}`
	default:
		found, err = command(ctx, "", nil, "docker", o.kind, "ls", "--filter", "name=^"+o.name+"$", "--format", "{{.Name}}")
	}
	if err != nil {
		return false, fmt.Errorf("could not inspect %s %s", o.kind, o.name)
	}
	if strings.TrimSpace(found) == "" {
		return false, nil
	}
	inspect := []string{o.kind, "inspect", "--format", labelFormat, o.name}
	if o.kind == "container" {
		inspect = []string{"inspect", "--format", labelFormat, o.name}
	}
	owner, err := command(ctx, "", nil, "docker", inspect...)
	owner = strings.TrimSpace(owner)
	if err != nil || owner == "" || owner == "<no value>" || (o.owner != "" && owner != o.owner) {
		return false, fmt.Errorf("%s %s is not owned by this resource; it was left in place", o.kind, o.name)
	}
	switch o.kind {
	case "container":
		if _, err := command(ctx, "", nil, "docker", "stop", "--time", "30", o.name); err != nil {
			return false, fmt.Errorf("could not stop container %s", o.name)
		}
		if _, err := command(ctx, "", nil, "docker", "rm", o.name); err != nil {
			return false, fmt.Errorf("could not remove container %s", o.name)
		}
	default:
		if _, err := command(ctx, "", nil, "docker", o.kind, "rm", o.name); err != nil {
			return false, fmt.Errorf("could not remove %s %s; check that nothing else uses it", o.kind, o.name)
		}
	}
	return true, nil
}

// finishRetainedRemoval records the outcome: on success the resource is
// forgotten (databases keep their backup history, marked removed); on
// failure it stays listed with the error.
func (m *Manager) finishRetainedRemoval(it retainedItem, job HostJob, output string, err error) {
	rt := m.retainedState()
	defer func() { rt.mu.Lock(); delete(rt.removing, it.id); rt.mu.Unlock() }()
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, dbID := range it.databases {
		d, ok := m.data.Databases[dbID]
		if !ok {
			continue
		}
		d.Operation = ""
		d.Event = newID()
		if err != nil {
			d.Error = "removal failed: " + err.Error()
		} else {
			d.Error, d.Ready, d.Removed = "", false, now
			d.URL, d.AdminPassword, d.Network = "", "", ""
			d.Backup.Hours, d.Backup.Cron, d.Backup.Timezone = 0, "", ""
		}
		m.data.Databases[dbID] = d
	}
	if it.record {
		if r, ok := m.data.RetainedResources[it.id]; ok {
			if err != nil {
				r.Error = err.Error()
				m.data.RetainedResources[it.id] = r
			} else {
				delete(m.data.RetainedResources, it.id)
			}
		}
	}
	if it.legacy && err == nil {
		delete(m.data.StorageVolumes, it.appID)
		delete(m.data.StackVolumes, it.appID)
		delete(m.data.Caches, it.appID)
		delete(m.data.ClickHouses, it.appID)
	}
	job.Finished = &now
	job.Output = strings.TrimSpace(output)
	job.Status = "succeeded"
	job.Summary = "Removed retained " + it.kind + " " + it.appID + " (" + it.id + "); backups are kept."
	if err != nil {
		job.Status = "failed"
		job.Summary = err.Error()
	}
	m.recordHostJobLocked(job)
	if saveErr := m.save(); saveErr != nil {
		log.Printf("retained resource %s: state not saved after removal: %v", it.id, saveErr)
	}
	if err == nil {
		rt.mu.Lock()
		for _, o := range it.volumes() {
			delete(rt.sizes, o.name)
		}
		rt.mu.Unlock()
	}
}

func (m *Manager) retainedRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/retained-resources", func(w http.ResponseWriter, r *http.Request) {
		views := m.retainedViews()
		if len(views) > 0 {
			m.refreshRetainedSizes()
		}
		JSON(w, 200, views)
	})
	mux.HandleFunc("DELETE /v1/retained-resources/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req RetainedRemoveRequest
		if err := Decode(w, r, &req); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.removeRetained(r.PathValue("id"), req); err != nil {
			status := 409
			if errors.Is(err, errRetainedNotFound) {
				status = 404
			}
			Fail(w, status, err)
			return
		}
		JSON(w, 202, map[string]string{"status": "removing"})
	})
}
