package agent

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

// MigrationPlan is a reviewable inventory, never an export of application secrets
// or an instruction to change the source host. Execution requires a fresh plan.
type MigrationPlan struct {
	Version            int                 `json:"version"`
	InventoryLimited   bool                `json:"inventory_limited,omitempty"`
	AppID              string              `json:"app_id"`
	Checked            time.Time           `json:"checked"`
	Commit             string              `json:"commit,omitempty"`
	Domains            []string            `json:"domains"`
	EnvironmentKeys    []string            `json:"environment_keys"`
	Databases          []MigrationDatabase `json:"databases"`
	PersistentFiles    bool                `json:"persistent_files"`
	CacheMode          string              `json:"cache_mode,omitempty"`
	Tasks              []string            `json:"tasks"`
	Blockers           []string            `json:"blockers"`
	Steps              []string            `json:"steps"`
	ExecutionSupported bool                `json:"execution_supported"`
}

type MigrationDatabase struct {
	ID              string     `json:"id"`
	Mode            string     `json:"mode"`
	EnvironmentKeys []string   `json:"environment_keys"`
	SharedWith      []string   `json:"shared_with"`
	LatestBackup    *time.Time `json:"latest_backup,omitempty"`
}

// Older live releases may retain database IDs without explicit app bindings.
// Include both the deployed dependencies and next-deploy bindings for review.
// An empty key list means release evidence only; never infer environment edits.
func migrationDatabaseReferences(a App) map[string][]string {
	refs := map[string][]string{}
	if a.Current != nil {
		for _, id := range a.Current.DatabaseIDs {
			if id != "" {
				refs[id] = []string{}
			}
		}
	}
	for key, id := range a.Bindings {
		if id != "" {
			refs[id] = append(refs[id], key)
		}
	}
	return refs
}

func (m *Manager) MigrationPlan(id string) (MigrationPlan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok || a.Retiring {
		return MigrationPlan{}, errors.New("application unavailable")
	}
	p := MigrationPlan{Version: 1, AppID: id, Checked: time.Now().UTC(), Domains: a.Hostnames(), EnvironmentKeys: []string{}, Databases: []MigrationDatabase{}, Tasks: []string{}, Blockers: []string{}, PersistentFiles: m.data.StorageVolumes[id] != "" || len(m.data.StackVolumes[id]) > 0}
	if a.Current != nil {
		p.Commit = a.Current.Commit
	} else {
		p.Blockers = append(p.Blockers, "Deploy and verify the source application before planning a move.")
	}
	if m.busy(id) || a.Restoring || m.lifecycle[id] {
		p.Blockers = append(p.Blockers, "Wait for the active application operation.")
	}
	if a.Preview {
		p.Blockers = append(p.Blockers, "Move the parent application; recreate previews on the destination.")
	}
	if a.Current != nil && a.Current.Stack != nil {
		p.Blockers = append(p.Blockers, "Review stack service volumes, platform constraints and runtime selections individually before transfer.")
	}
	for key := range a.Env {
		p.EnvironmentKeys = append(p.EnvironmentKeys, key)
	}
	slices.Sort(p.EnvironmentKeys)
	bindings := migrationDatabaseReferences(a)
	for dbID, keys := range bindings {
		slices.Sort(keys)
		db, found := m.data.Databases[dbID]
		entry := MigrationDatabase{ID: dbID, Mode: db.Mode, EnvironmentKeys: keys, SharedWith: []string{}}
		if !found || !db.Ready || db.Operation != "" {
			p.Blockers = append(p.Blockers, "Database "+dbID+" is unavailable or has an active operation.")
		}
		for otherID, other := range m.data.Apps {
			if otherID == id {
				continue
			}
			if _, attached := migrationDatabaseReferences(other)[dbID]; attached {
				entry.SharedWith = append(entry.SharedWith, otherID)
			}
		}
		slices.Sort(entry.SharedWith)
		if db.Mode == "local" && len(entry.SharedWith) > 0 {
			p.Blockers = append(p.Blockers, "Local database "+dbID+" is shared; coordinate every attached application before moving it.")
		}
		for _, backup := range db.Backups {
			if backup.SHA256 != "" && backup.Size > 0 && (entry.LatestBackup == nil || backup.Created.After(*entry.LatestBackup)) {
				stamp := backup.Created
				entry.LatestBackup = &stamp
			}
		}
		p.Databases = append(p.Databases, entry)
	}
	slices.SortFunc(p.Databases, func(a, b MigrationDatabase) int { return strings.Compare(a.ID, b.ID) })
	for key := range a.Env {
		if validDatabaseKey(key) && a.Bindings[key] == "" {
			p.Blockers = append(p.Blockers, "Review the manually configured database connection in "+key+" for destination reachability and backup ownership.")
		}
	}
	p.CacheMode = m.cacheViewLocked(id).Mode
	if m.data.Caches[id].Operation {
		p.Blockers = append(p.Blockers, "Wait for cache provisioning.")
	}
	if p.CacheMode == "local" {
		p.Blockers = append(p.Blockers, "Review local cache contents; queues and durable cache data require a coordinated transfer.")
	}
	if _, found := m.data.ClickHouses[id]; found {
		p.Blockers = append(p.Blockers, "Review the managed ClickHouse data and backup transfer separately.")
	}
	for _, task := range m.data.Tasks {
		if task.AppID == id {
			p.Tasks = append(p.Tasks, task.ID)
			if task.Launching || task.Running && task.Mode == "schedule" {
				p.Blockers = append(p.Blockers, "Wait for task "+task.ID+" to finish.")
			}
		}
	}
	slices.Sort(p.Tasks)
	slices.Sort(p.Blockers)
	if a.isRails() {
		p.Blockers = append(p.Blockers, "Preserve all Rails encryption keys, database configurations, shared storage and public uploads; review SQLite WAL files and queue ownership before cutover.")
	}
	p.Steps = []string{
		"Verify destination capacity, runtime compatibility, database access and private application ports.",
		"Copy encrypted settings through an authenticated channel; preserve application encryption keys.",
		"Prepare the destination without routing traffic or starting workers and schedules.",
		"Quiesce source writers, workers and schedules; take and verify final database and file backups.",
		"Restore databases and persistent files; verify application readiness and data on the destination.",
		"Switch DNS and HTTPS routing, then enable workers and schedules on the destination only.",
		"Retain the stopped source and backups for rollback; never run writers on both hosts.",
	}
	return p, nil
}

func (m *Manager) migrationPlanRoute(w http.ResponseWriter, r *http.Request) {
	p, err := m.MigrationPlan(r.PathValue("id"))
	if err != nil {
		Fail(w, 404, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, 200, p)
}
