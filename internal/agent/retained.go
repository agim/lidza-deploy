package agent

import (
	"cmp"
	"maps"
	"net/http"
	"slices"
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
}

func (a App) resourceID() string {
	if a.ResourceID != "" {
		return a.ResourceID
	}
	return a.ID
}

// The caller holds m.mu and persists this together with app removal/creation.
func (m *Manager) archiveResourcesLocked(id string) {
	volume, cache, clickhouse := m.data.StorageVolumes[id], m.data.Caches[id], m.data.ClickHouses[id]
	if volume == "" && cache.Mode == "" && clickhouse.Network == "" && len(m.data.StackVolumes[id]) == 0 {
		return
	}
	if m.data.RetainedResources == nil {
		m.data.RetainedResources = map[string]RetainedResource{}
	}
	key := newID()
	m.data.RetainedResources[key] = RetainedResource{StackVolumes: m.data.StackVolumes[id], ID: key, AppID: id, Created: time.Now().UTC(), StorageVolume: volume, Cache: cache, ClickHouse: clickhouse}
	delete(m.data.StackVolumes, id)
	delete(m.data.StorageVolumes, id)
	delete(m.data.Caches, id)
	delete(m.data.ClickHouses, id)
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
	out.Caches = maps.Clone(m.data.Caches)
	out.ClickHouses = maps.Clone(m.data.ClickHouses)
	out.RetainedResources = maps.Clone(m.data.RetainedResources)
	out.RetiredAppIDs = maps.Clone(m.data.RetiredAppIDs)
	out.Tasks = maps.Clone(m.data.Tasks)
	return out
}

func (m *Manager) retainedViews() []RetainedResourceView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []RetainedResourceView{}
	for _, item := range m.data.RetainedResources {
		view := RetainedResourceView{StackVolumes: item.StackVolumes, ID: item.ID, AppID: item.AppID, Created: item.Created, StorageVolume: item.StorageVolume, CacheMode: item.Cache.Mode}
		if item.ClickHouse.Network != "" {
			view.ClickHouseVolume = item.ClickHouse.Network + "-data"
		}
		if item.Cache.Network != "" {
			view.CacheVolume = item.Cache.Network + "-data"
		}
		out = append(out, view)
	}
	// Expose older removals without rewriting their maps during startup. An
	// installer rollback can still read that state until recreation is requested.
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
		cache := m.data.Caches[id]
		view := RetainedResourceView{ID: "legacy-" + id, AppID: id, StorageVolume: m.data.StorageVolumes[id], CacheMode: cache.Mode}
		if c := m.data.ClickHouses[id]; c.Network != "" {
			view.ClickHouseVolume = c.Network + "-data"
		}
		if cache.Network != "" {
			view.CacheVolume = cache.Network + "-data"
		}
		out = append(out, view)
	}
	slices.SortFunc(out, func(a, b RetainedResourceView) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

func (m *Manager) retainedRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/retained-resources", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, m.retainedViews()) })
}
