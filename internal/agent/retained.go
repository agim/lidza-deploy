package agent

import (
	"errors"
	"maps"
	"net/http"
	"time"
)

// Retained resource credentials remain in the encrypted agent state. Recreated
// applications get a different resource identity and never inherit these keys.
type RetainedResource struct {
	StackVolumes  []string           `json:"stack_volumes,omitempty"`
	ClickHouse    ClickHouseResource `json:"clickhouse,omitempty"`
	ID            string             `json:"id"`
	AppID         string             `json:"app_id"`
	Created       time.Time          `json:"created"`
	StorageVolume string             `json:"storage_volume,omitempty"`
	Cache         CacheResource      `json:"cache,omitempty"`
	// Databases are the application's local databases that no other
	// application used when it was removed (retained_remove.go).
	Databases []string `json:"databases,omitempty"`
	// WritesStopped is when the application stopped writing to its data:
	// the start of its maintenance mode when it was removed in maintenance
	// (a move's final sync), else the removal. A backup taken at or after
	// it holds the data's final state. Zero for older records.
	WritesStopped time.Time `json:"writes_stopped,omitzero"`
	// NativeData marks the data services a removed native application had
	// on this host (diskState.NativeHostData, still keyed by AppID): its
	// PostgreSQL, Redis and ClickHouse (native_hostdata.go).
	NativeData bool `json:"native_data,omitempty"`
	// Origin is the move the removed application belonged to, when the
	// control plane said so; nil for an application removed outside a move
	// or by an older control plane.
	Origin *RetainedOrigin `json:"origin,omitempty"`
	// Error is why the last removal attempt failed.
	Error string `json:"error,omitempty"`
	// Archive is the deleted application (archive.go) that holds this
	// data; such a resource is managed through its archive only.
	Archive string `json:"archive,omitempty"`
}

// RetainedOrigin records the move a retained resource came from. A
// destination that never switched holds a partial copy of data that still
// lives on the source: removing it needs no backup. Once a move switched,
// or when the origin is unknown, the data counts as the application's own.
type RetainedOrigin struct {
	MoveID       string `json:"move_id"`
	SourceServer string `json:"source_server,omitempty"`
	Switched     bool   `json:"switched"`
}

// copied reports data that is a never-switched move's copy.
func (o *RetainedOrigin) copied() bool {
	return o != nil && o.MoveID != "" && !o.Switched
}

// AppRemoveRequest is the optional body of DELETE /v1/apps/{id} and
// DELETE /v1/native/apps/{id}. Without it the application's data is
// retained and nothing records where it came from.
type AppRemoveRequest struct {
	// Origin is recorded on the retained resource the removal leaves.
	Origin *RetainedOrigin `json:"origin,omitempty"`
	// DeleteData removes the data services and volumes together with the
	// application instead of retaining them. It is accepted only with an
	// Origin that never switched, whose data is a copy of the source's.
	DeleteData bool `json:"delete_data,omitempty"`
}

func (r AppRemoveRequest) validate() error {
	if r.DeleteData && !r.Origin.copied() {
		return errors.New("data is deleted with the application only on the destination of a move that never switched; otherwise it is retained")
	}
	return nil
}

// decodeAppRemoveRequest reads the optional removal body.
func decodeAppRemoveRequest(w http.ResponseWriter, r *http.Request) (AppRemoveRequest, error) {
	var req AppRemoveRequest
	if r.ContentLength == 0 {
		return req, nil
	}
	if err := Decode(w, r, &req); err != nil {
		return req, err
	}
	return req, req.validate()
}

type RetainedResourceView struct {
	StackVolumes     []string  `json:"stack_volumes,omitempty"`
	ClickHouseVolume string    `json:"clickhouse_volume,omitempty"`
	ID               string    `json:"id"`
	AppID            string    `json:"app_id"`
	Created          time.Time `json:"created"`
	StorageVolume    string    `json:"storage_volume,omitempty"`
	CacheMode        string    `json:"cache_mode,omitempty"`
	CacheVolume      string    `json:"cache_volume,omitempty"`

	// Kind is application (what a removed application left) or database
	// (a database no removal record holds).
	Kind          string                 `json:"kind"`
	WritesStopped time.Time              `json:"writes_stopped,omitzero"`
	Components    []RetainedComponent    `json:"components"`
	Databases     []RetainedDatabaseView `json:"databases,omitempty"`
	// SizeBytes sums the measured volumes; SizePending is set until every
	// volume was measured once.
	SizeBytes   int64 `json:"size_bytes"`
	SizePending bool  `json:"size_pending,omitempty"`
	// AttachedTo lists live applications that use part of the resource;
	// removal is refused while it is not empty.
	AttachedTo []string `json:"attached_to,omitempty"`
	// Origin is the move the data came from, when known; Copy marks a
	// never-switched move's partial copy, which is removed without a backup.
	Origin *RetainedOrigin `json:"origin,omitempty"`
	Copy   bool            `json:"copy,omitempty"`
	// FinalBackupRequired is set when a database has no verified backup
	// taken after its application stopped writing (never for a Copy).
	FinalBackupRequired bool `json:"final_backup_required"`
	// DataSince is the earliest creation time of its volumes, when Docker
	// reports one: when the data appeared here, for records without Created.
	DataSince time.Time `json:"data_since,omitzero"`
	Removing  bool      `json:"removing,omitempty"`
	Error     string    `json:"error,omitempty"`
}

func (a App) resourceID() string {
	if a.ResourceID != "" {
		return a.ResourceID
	}
	return a.ID
}

// The caller holds m.mu and persists this together with app removal/creation.
// It returns the new record's key, or "" when nothing was retained.
func (m *Manager) archiveResourcesLocked(id string, retained ...retainedExtras) string {
	volume, cache, clickhouse := m.data.StorageVolumes[id], m.data.Caches[id], m.data.ClickHouses[id]
	var extras retainedExtras
	if len(retained) > 0 {
		extras = retained[0]
	}
	if volume == "" && cache.Mode == "" && clickhouse.Network == "" && len(m.data.StackVolumes[id]) == 0 && len(extras.databases) == 0 {
		return ""
	}
	if m.data.RetainedResources == nil {
		m.data.RetainedResources = map[string]RetainedResource{}
	}
	key := newID()
	m.data.RetainedResources[key] = RetainedResource{StackVolumes: m.data.StackVolumes[id], ID: key, AppID: id, Created: time.Now().UTC(), StorageVolume: volume, Cache: cache, ClickHouse: clickhouse,
		Databases: extras.databases, WritesStopped: extras.writesStopped, Origin: extras.origin}
	delete(m.data.StackVolumes, id)
	delete(m.data.StorageVolumes, id)
	delete(m.data.Caches, id)
	delete(m.data.ClickHouses, id)
	return key
}

// retainedExtras is what a removal knows beyond the resource maps: the
// databases the application leaves behind and when its writes stopped.
type retainedExtras struct {
	databases     []string
	writesStopped time.Time
	origin        *RetainedOrigin
}

func (m *Manager) hasRetainedResourcesLocked(id string) bool {
	if m.data.RetiredAppIDs[id] {
		return true
	}
	if m.data.StorageVolumes[id] != "" || m.data.Caches[id].Mode != "" || m.data.ClickHouses[id].Network != "" {
		return true
	}
	if _, exists := m.data.Databases[id]; exists {
		return true
	}
	for _, retained := range m.data.RetainedResources {
		if retained.AppID == id {
			return true
		}
	}
	// Older versions retained deployment history even when an app had no managed
	// data resources. Give a recreated app a fresh private setup directory too.
	for _, deployment := range m.data.Deployments {
		if deployment.AppID == id {
			return true
		}
	}
	return false
}

// Snapshot the affected maps for rollback if atomic encrypted persistence fails.
func (m *Manager) resourceSnapshotLocked() diskState {
	out := m.data
	out.Apps = maps.Clone(m.data.Apps)
	out.StorageVolumes = maps.Clone(m.data.StorageVolumes)
	out.StackVolumes = maps.Clone(m.data.StackVolumes)
	out.StackSelections = maps.Clone(m.data.StackSelections)
	out.GatewayPKI = maps.Clone(m.data.GatewayPKI)
	out.Caches = maps.Clone(m.data.Caches)
	out.ClickHouses = maps.Clone(m.data.ClickHouses)
	out.RetainedResources = maps.Clone(m.data.RetainedResources)
	out.RetiredAppIDs = maps.Clone(m.data.RetiredAppIDs)
	out.Tasks = maps.Clone(m.data.Tasks)
	out.Redirects = maps.Clone(m.data.Redirects)
	return out
}
