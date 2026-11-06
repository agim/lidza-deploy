package agent

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Moving an application to another server. The control panel orchestrates
// the move between two agents; these endpoints are each agent's part:
//
//   - GET  /v1/move/apps/{id}             what a move of the application involves
//   - GET  /v1/move/apps/{id}/environment the application's environment values
//   - POST /v1/move/apps/{id}/health      an explicit loopback readiness check
//   - GET  /v1/move/host                  this host's public addresses and roles
//   - GET  /v1/move/databases/{id}/tables number of tables in a database
//   - GET  /v1/move/databases/{id}/backups/{backup} a verified dump download
//   - POST /v1/move/databases/{id}/restore restore an uploaded dump
//   - /v1/move/apps/{id}/files...         persistent files (see move_files.go)
//   - /v1/move/uploads/{upload}           resumable uploads (see move_resume.go)
//   - POST /v1/move/probe, GET /v1/move/apps/{id}/diagnostics (move_probe.go)
//   - /v1/move/databases/{id}/replication, .../subscription, .../schema
//     logical replication of a database during a move (move_replication.go)
//
// Like every /v1 route they require the agent API key, which only the control
// panel holds; the control panel never forwards them to a browser. Handlers
// never log environment values or database contents.

// MoveSHA256Header carries the SHA-256 of a database dump in transit.
const MoveSHA256Header = "X-Lidza-Sha256"

// MoveSource describes what moving an application involves. It names
// environment keys but never carries a value.
type MoveSource struct {
	AppID             string   `json:"app_id"`
	Runtime           string   `json:"runtime"` // go, rails, astro, node, static
	Native            bool     `json:"native"`
	Repository        string   `json:"repository"`
	Branch            string   `json:"branch"`
	Domain            string   `json:"domain"`
	AdditionalDomains []string `json:"additional_domains,omitempty"`
	Commit            string   `json:"commit,omitempty"`
	EnvironmentKeys   []string `json:"environment_keys"`
	// EnvironmentSources names where each exported variable comes from
	// (panel, deploy defaults, image); empty when all are panel settings.
	EnvironmentSources map[string]string    `json:"environment_sources,omitempty"`
	Databases          []MoveSourceDatabase `json:"databases"`
	// Redis is a data service the destination must provide again: a host
	// Redis (native) or the managed cache (containers). Its contents are not
	// copied.
	Redis     bool   `json:"redis"`
	CacheMode string `json:"cache_mode,omitempty"`
	// ClickHouse is the application's managed ClickHouse database; its
	// contents are copied (move_clickhouse.go).
	ClickHouse *MoveSourceClickHouse `json:"clickhouse,omitempty"`
	Volumes    []string              `json:"volumes"`
	// Files are the persistent file sets the agent transfers; nil from
	// agents that cannot transfer files.
	Files *MoveFiles `json:"files,omitempty"`
	Tasks []string   `json:"tasks"`
	// CreateSpec recreates a native application elsewhere: no environment
	// and no external database URL.
	CreateSpec *NativeCreateRequest `json:"create_spec,omitempty"`
	// SpecRecorded is false when the application predates recorded creation
	// requests; its process commands are then unknown.
	SpecRecorded bool `json:"spec_recorded,omitempty"`
	// Container recreates a container application elsewhere: no environment.
	Container *App     `json:"container,omitempty"`
	Busy      bool     `json:"busy"`
	Blockers  []string `json:"blockers"`
	Notes     []string `json:"notes"`
	// BuildPeakMiB is the largest memory peak of the application's recent
	// build steps on this host; 0 when none was measured.
	BuildPeakMiB int `json:"build_peak_mib,omitempty"`
	// PublicPorts are the host ports the current release publishes (the
	// stack's app services); the destination publishes them again. Never
	// nil from agents that report them (move_ports.go).
	PublicPorts []StackPort `json:"public_ports"`
	// Protected reports that the current release uses gateway_transport;
	// the destination needs a protected host gateway.
	Protected bool `json:"protected,omitempty"`
}

// MoveSourceDatabase is one database of a moving application.
type MoveSourceDatabase struct {
	ID              string   `json:"id"`
	Engine          string   `json:"engine"` // postgresql or sqlite
	Mode            string   `json:"mode"`   // local (container), host (native) or external
	EnvironmentKeys []string `json:"environment_keys"`
	// Copyable databases are dumped through the backup pipeline and restored
	// on the destination. External databases stay where they are.
	Copyable     bool         `json:"copyable"`
	Tables       int          `json:"tables"` // -1 when unknown
	SizeBytes    int64        `json:"size_bytes,omitempty"`
	Backup       BackupPolicy `json:"backup"`
	LatestBackup *time.Time   `json:"latest_backup,omitempty"`
	Reason       string       `json:"reason,omitempty"`
	// BeforeDeploy is the database's effective pre-deployment backup policy,
	// carried to the destination.
	BeforeDeploy BeforeDeployPolicy `json:"before_deploy,omitzero"`
	// ExcludedTables are tables whose data this agent's backups leave out
	// (backup_exclude_table_data); a move copies them separately or leaves
	// them behind, as the operator chooses (move_table_data.go).
	ExcludedTables []MoveExcludedTable `json:"excluded_tables,omitempty"`
	// Replication reports whether the database can be copied with logical
	// replication (move_replication.go); nil from agents without it.
	Replication *MoveReplicationReadiness `json:"replication,omitempty"`
}

// MoveEnvironment is an application's complete environment for copying to
// its new server. Only the control panel receives it.
type MoveEnvironment struct {
	AppID  string            `json:"app_id"`
	Native bool              `json:"native"`
	Values map[string]string `json:"values"`
	Kinds  map[string]string `json:"kinds"`
	// Sources names where each value comes from: panel, deploy defaults or
	// image (move_environment.go). Native environments are panel-complete.
	Sources map[string]string `json:"sources,omitempty"`
}

// MoveHost describes a possible destination.
type MoveHost struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
	// PrivateIPv4 and PrivateNetwork are the host's preferred private
	// address and its network: a replication relay listens on a private
	// network both hosts share when the destination reaches it there.
	PrivateIPv4    string `json:"private_ipv4,omitempty"`
	PrivateNetwork string `json:"private_network,omitempty"`
	// PrivateNetworks are all the host's private IPv4 addresses with their
	// prefix length ("10.30.0.5/16"), without container bridges and cloud
	// anchor addresses, so the panel finds a network both hosts share.
	PrivateNetworks []string `json:"private_networks,omitempty"`
	// Tunnel reports that this agent carries move replication through the
	// control panel (move_replication_tunnel.go) and probes TCP reachability
	// (POST /v1/move/reach).
	Tunnel bool `json:"tunnel,omitempty"`
	// Subscribe reports that this agent subscribes a destination database to
	// a source with logical replication (move_replication.go).
	Subscribe bool `json:"subscribe,omitempty"`
	Native    bool `json:"native"`
	HostData  bool `json:"host_data"`
	// Files reports that this agent restores transferred file sets.
	Files bool `json:"files,omitempty"`
	// ClickHouse reports that this agent copies managed ClickHouse data.
	ClickHouse bool `json:"clickhouse,omitempty"`
	// Resume reports ranged downloads and resumable uploads
	// (move_resume.go); Probe the disk probe and move diagnostics
	// (move_probe.go).
	Resume bool `json:"resume,omitempty"`
	Probe  bool `json:"probe,omitempty"`
	// TableData reports that this agent dumps and loads the data of tables
	// that backups leave out (move_table_data.go).
	TableData bool `json:"table_data,omitempty"`
	// TableKey reports that this agent derives incremental keys for tables
	// loaded before their key was recorded (move_table_key.go).
	TableKey bool `json:"table_key,omitempty"`
	// ClickHouseFormats are the ClickHouse archive formats this agent
	// restores (directory backups since this field exists); Zstd that it
	// writes and reads Zstandard streams (the zstd tool is installed).
	ClickHouseFormats []string `json:"clickhouse_formats,omitempty"`
	Zstd              bool     `json:"zstd,omitempty"`
	// ListeningPorts are the ports already bound on this host's public
	// addresses with their processes; nil when they could not be listed
	// (move_ports.go). HostFirewall is the managed host firewall's state:
	// active, inactive or empty when unknown. HostMail describes the host's
	// own mail server when it holds a public mail port (host_mail.go).
	ListeningPorts []ListeningPort `json:"listening_ports"`
	HostFirewall   string          `json:"host_firewall,omitempty"`
	HostMail       *MoveHostMail   `json:"host_mail,omitempty"`
	// Gateway is how this host serves protected releases (gateway_transport):
	// configured (a host gateway exists), auto (the agent sets one up itself
	// at the first protected deployment, gateway_auto.go) or unavailable;
	// empty from agents without automatic gateways.
	Gateway string `json:"gateway,omitempty"`
}

// MoveHealth is the result of an explicit readiness check.
type MoveHealth struct {
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
}

const (
	moveMaxEnvironmentKeys  = 500
	moveMaxEnvironmentBytes = 512 << 10
)

// Hooks replaced in tests.
var (
	moveDetectAddresses = DetectAddresses
	moveLoopbackHealth  = func(ctx context.Context, port, domain, path string) error {
		return nativeLoopbackHealth(port, domain, path)(ctx)
	}
)

// moveTransfer marks a long move transfer as running for its duration, so an
// agent upgrade does not restart the agent in the middle of it.
func (m *Manager) moveTransfer(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.moveTransfers.Add(1)
		defer m.moveTransfers.Add(-1)
		h(w, r)
	}
}

func (m *Manager) moveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/move/host", func(w http.ResponseWriter, r *http.Request) {
		addresses := moveDetectAddresses()
		listening := moveListeningPorts(r.Context())
		JSON(w, 200, MoveHost{IPv4: addresses.IPv4, IPv6: addresses.IPv6, PrivateIPv4: addresses.PrivateIPv4, PrivateNetwork: addresses.PrivateNetwork, PrivateNetworks: addresses.PrivateNetworks, Tunnel: !m.cfg.IngressOnly, Subscribe: !m.cfg.IngressOnly,
			Native: m.cfg.Native != nil, HostData: !m.cfg.IngressOnly, Files: true, ClickHouse: !m.cfg.IngressOnly, Resume: true, Probe: true, TableData: true, TableKey: true,
			ClickHouseFormats: []string{clickhouseFormatDirectory, clickhouseFormatBackup, clickhouseFormatNative, clickhouseFormatIncremental}, Zstd: zstdAvailable(),
			ListeningPorts: listening, HostFirewall: moveHostFirewall(r.Context()), HostMail: probeHostMail(r.Context(), listening), Gateway: m.gatewayCapability(r.Context())})
	})
	mux.HandleFunc("GET /v1/move/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		source, err := m.moveSource(r.Context(), r.PathValue("id"))
		if err != nil {
			Fail(w, 404, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, source)
	})
	mux.HandleFunc("GET /v1/move/apps/{id}/environment", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		env, err := m.moveEnvironment(r.Context(), r.PathValue("id"))
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, env)
	})
	mux.HandleFunc("POST /v1/move/apps/{id}/health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		JSON(w, 200, m.moveHealth(ctx, r.PathValue("id")))
	})
	mux.HandleFunc("GET /v1/move/databases/{id}/tables", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		count, err := moveTableCount(ctx, m, r.PathValue("id"))
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]int{"tables": count})
	})
	mux.HandleFunc("GET /v1/move/databases/{id}/backups/{backup}", m.moveTransfer(m.moveBackupDownload))
	mux.HandleFunc("POST /v1/move/databases/{id}/restore", m.moveTransfer(m.moveRestoreRoute))
	m.moveFileRoutes(mux)
	m.moveClickHouseRoutes(mux)
	m.moveUploadRoutes(mux)
	m.moveProbeRoutes(mux)
	m.moveTableDataRoutes(mux)
	m.moveTableDeltaRoutes(mux)
	m.moveTableKeyRoutes(mux)
	m.moveReplicationRoutes(mux)
}

// moveSource inventories an application for a move. Table counts are
// measured outside the state lock with short read-only queries.
func (m *Manager) moveSource(ctx context.Context, id string) (MoveSource, error) {
	if !idPattern.MatchString(id) {
		return MoveSource{}, errors.New("invalid application id")
	}
	m.mu.Lock()
	a, container := m.data.Apps[id]
	var source MoveSource
	if container {
		if a.Retiring {
			m.mu.Unlock()
			return source, errors.New("application unavailable")
		}
		source = m.containerMoveSourceLocked(a)
		m.mu.Unlock()
		// The container runs with more than the panel stores (manifest
		// defaults, image ENV); the plan lists the effective keys.
		effective := m.containerEffectiveEnvironment(ctx, a)
		source.EnvironmentKeys = sortedKeys(effective.Values)
		source.EnvironmentSources = effective.Sources
	} else {
		m.mu.Unlock()
		var err error
		if source, err = m.nativeMoveSource(id); err != nil {
			return source, err
		}
	}
	for i := range source.Databases {
		database := &source.Databases[i]
		if !database.Copyable {
			continue
		}
		measure, cancel := context.WithTimeout(ctx, 10*time.Second)
		if count, err := moveTableCount(measure, m, database.ID); err == nil {
			database.Tables = count
		}
		cancel()
		database.ExcludedTables = m.moveExcludedTables(ctx, database.ID)
		database.Replication = m.moveReplicationReadiness(ctx, database.ID)
	}
	if ch := source.ClickHouse; ch != nil && ch.Copyable {
		if err := m.measureMoveClickHouse(ctx, id, ch); err != nil {
			ch.Copyable, ch.Reason = false, "unreachable: "+err.Error()
			source.Blockers = append(source.Blockers, "ClickHouse database "+ch.Database+" could not be measured on the source: "+boundedErrorText(err.Error(), 300))
		}
	}
	if len(source.Blockers) == 0 || source.Container != nil || source.CreateSpec != nil {
		if files, err := m.moveFileInventory(ctx, id); err == nil {
			source.Files = files
		}
	}
	source.BuildPeakMiB = m.buildPeakOf(id)
	slices.Sort(source.Blockers)
	return source, nil
}

func (m *Manager) containerMoveSourceLocked(a App) MoveSource {
	id := a.ID
	s := MoveSource{AppID: id, Runtime: a.runtimeKind(), Repository: a.Repository, Branch: a.Branch, Domain: a.Domain, AdditionalDomains: slices.Clone(a.AdditionalDomains),
		EnvironmentKeys: []string{}, Databases: []MoveSourceDatabase{}, Volumes: []string{}, Tasks: []string{}, Blockers: []string{}, Notes: []string{}, PublicPorts: movePublicPorts(a.Current)}
	s.Container = &App{ID: id, Repository: a.Repository, Branch: a.Branch, Domain: a.Domain, AdditionalDomains: slices.Clone(a.AdditionalDomains), CustomerHosts: a.CustomerHosts,
		Build: a.Build, Astro: a.Astro, Rails: a.Rails, Runtime: a.Runtime, RuntimeProfile: a.RuntimeProfile}
	if a.Current != nil {
		s.Commit = a.Current.Commit
		s.Protected = a.Current.Stack != nil && a.Current.Stack.Gateway != nil
	} else {
		s.Blockers = append(s.Blockers, "Deploy the application on its current server before moving it.")
	}
	if a.Preview {
		s.Blockers = append(s.Blockers, "Previews are not moved; move the parent application instead.")
	}
	s.Busy = m.busy(id) || a.Restoring || m.lifecycle[id]
	for key := range a.Env {
		if !strings.HasPrefix(key, gatewayEnvPrefix) {
			s.EnvironmentKeys = append(s.EnvironmentKeys, key)
		}
	}
	slices.Sort(s.EnvironmentKeys)
	for dbID, keys := range migrationDatabaseReferences(a) {
		slices.Sort(keys)
		d, found := m.data.Databases[dbID]
		entry := MoveSourceDatabase{ID: dbID, Engine: "postgresql", Mode: d.Mode, EnvironmentKeys: keys, Tables: -1, Backup: d.Backup, BeforeDeploy: m.beforeDeployPolicyLocked(dbID)}
		if m.data.NativeDatabases[dbID].Policy.SQLitePath != "" {
			entry.Engine = "sqlite"
		}
		entry.LatestBackup = latestBackup(d)
		shared := []string{}
		for otherID, other := range m.data.Apps {
			if otherID == id {
				continue
			}
			if _, attached := migrationDatabaseReferences(other)[dbID]; attached {
				shared = append(shared, otherID)
			}
		}
		slices.Sort(shared)
		switch {
		case !found || !d.Ready || d.Operation != "":
			entry.Reason = "unavailable or busy"
			s.Blockers = append(s.Blockers, "Database "+dbID+" is unavailable or has an active operation.")
		case len(shared) > 0 && d.Mode == "local":
			entry.Reason = "shared with " + strings.Join(shared, ", ")
			s.Blockers = append(s.Blockers, "Database "+dbID+" is shared with "+strings.Join(shared, ", ")+"; move every attached application together manually.")
		case entry.Engine == "sqlite":
			entry.Reason = "SQLite files are moved manually"
			s.Blockers = append(s.Blockers, "Database "+dbID+" is a SQLite file; copy it manually.")
		case d.Mode == "local":
			entry.Copyable = true
		default:
			entry.Mode = "external"
			entry.Reason = "external database; its connection is copied unchanged"
		}
		if len(keys) == 0 && entry.Copyable {
			entry.Copyable = false
			entry.Reason = "referenced by the release only, without an environment binding"
			s.Blockers = append(s.Blockers, "Database "+dbID+" has no environment binding; attach it before moving.")
		}
		s.Databases = append(s.Databases, entry)
	}
	slices.SortFunc(s.Databases, func(x, y MoveSourceDatabase) int { return strings.Compare(x.ID, y.ID) })
	cache := m.cacheViewLocked(id)
	s.CacheMode = cache.Mode
	s.Redis = m.data.Caches[id].Mode == "local"
	if m.data.Caches[id].Operation {
		s.Blockers = append(s.Blockers, "Wait for cache provisioning.")
	}
	if volume := m.data.StorageVolumes[id]; volume != "" {
		s.Volumes = append(s.Volumes, "persistent storage volume "+volume)
	}
	for _, volume := range m.data.StackVolumes[id] {
		s.Volumes = append(s.Volumes, "stack volume "+volume)
	}
	if c, found := m.data.ClickHouses[id]; found {
		s.ClickHouse = &MoveSourceClickHouse{Mode: "container", Database: containerClickHouseDatabase, Tables: -1, EnvironmentKeys: slices.Clone(ClickHouseEnvironmentKeys), Copyable: c.Ready}
		if !c.Ready {
			s.ClickHouse.Reason = "not ready"
			s.Blockers = append(s.Blockers, "Managed ClickHouse is not ready; deploy the application on its current server before moving it.")
		}
	}
	for _, task := range m.data.Tasks {
		if task.AppID == id {
			s.Tasks = append(s.Tasks, task.ID)
			if task.Launching || task.Running && task.Mode == "schedule" {
				s.Blockers = append(s.Blockers, "Wait for task "+task.ID+" to finish.")
			}
		}
	}
	slices.Sort(s.Tasks)
	if len(s.Tasks) > 0 {
		s.Notes = append(s.Notes, "Workers and scheduled commands ("+strings.Join(s.Tasks, ", ")+") are not moved; recreate them on the destination.")
	}
	return s
}

func latestBackup(d Database) *time.Time {
	var latest *time.Time
	for _, backup := range d.Backups {
		if backup.SHA256 != "" && backup.Size > 0 && (latest == nil || backup.Created.After(*latest)) {
			stamp := backup.Created
			latest = &stamp
		}
	}
	return latest
}

func (m *Manager) nativeMoveSource(id string) (MoveSource, error) {
	a, err := m.nativeApp(id)
	if err != nil {
		return MoveSource{}, errors.New("application unavailable")
	}
	runtime := a.Runtime
	if runtime == "" {
		runtime = "rails"
	}
	s := MoveSource{AppID: id, Runtime: runtime, Native: true, Repository: a.Repository, Branch: a.Branch, Domain: a.Domain, AdditionalDomains: slices.Clone(a.AdditionalDomains),
		EnvironmentKeys: []string{}, Databases: []MoveSourceDatabase{}, Volumes: []string{}, Tasks: []string{}, Blockers: []string{}, Notes: []string{}, PublicPorts: []StackPort{}}
	m.mu.Lock()
	policy, created := m.data.NativeCreated[id]
	spec, recorded := m.data.NativeCreatedSpecs[id]
	record := m.data.NativeHostData[id]
	s.Busy = m.nativeOperations[id]
	var database Database
	var databaseFound bool
	if record.Postgres != nil {
		database, databaseFound = m.data.Databases[record.Postgres.Resource]
	}
	m.mu.Unlock()
	if !created {
		s.Blockers = append(s.Blockers, "Applications adopted from existing host configuration are moved manually; only applications created by Līdza Deploy move automatically.")
		return s, nil
	}
	if _, _, err := m.nativeReleasePolicy(id); err == nil {
		if values, err := nativeReleaseEnvValues(policy); err == nil {
			s.EnvironmentKeys = slices.Sorted(maps.Keys(values))
		} else {
			s.Blockers = append(s.Blockers, "The application's environment file is unreadable.")
		}
		if view, err := m.nativeReleases(id); err == nil {
			for _, entry := range view.Releases {
				if entry.Current {
					s.Commit = entry.Commit
				}
			}
		}
	} else {
		s.Blockers = append(s.Blockers, "The application's release policy is unavailable: "+err.Error())
	}
	if s.Commit == "" && runtime != "static" {
		s.Blockers = append(s.Blockers, "Deploy the application on its current server before moving it.")
	}
	if recorded {
		spec.ID, spec.Repository, spec.Branch, spec.Domain, spec.AdditionalDomains = id, a.Repository, a.Branch, a.Domain, slices.Clone(a.AdditionalDomains)
		s.SpecRecorded = true
	} else {
		spec = NativeCreateRequest{ID: id, Repository: a.Repository, Branch: a.Branch, Runtime: runtime, Domain: a.Domain, AdditionalDomains: slices.Clone(a.AdditionalDomains),
			HealthPath: policy.HealthPath, StaticOutput: policy.Output, BuildMemoryMiB: policy.BuildMemoryMiB}
		if runtime != "static" {
			s.Notes = append(s.Notes, "This application was created before process commands were recorded; the destination uses the detected or default processes.")
		}
	}
	spec.Env, spec.EnvKinds, spec.Data.ExternalDatabaseURL = nil, nil, ""
	spec.Data.Postgres, spec.Data.Redis = record.Postgres != nil, record.Redis != nil
	spec.Data.ClickHouse, spec.Data.ClickHouseVersion = record.ClickHouse != nil, ""
	if ch := record.ClickHouse; ch != nil {
		s.ClickHouse = &MoveSourceClickHouse{Mode: "host", Database: ch.Name, Tables: -1, EnvironmentKeys: slices.Clone(ClickHouseEnvironmentKeys), Copyable: ch.Ready}
		if !ch.Ready {
			s.ClickHouse.Reason = "not ready"
			s.Blockers = append(s.Blockers, "ClickHouse database "+ch.Name+" is not ready.")
		}
	}
	if pg := record.Postgres; pg != nil {
		entry := MoveSourceDatabase{ID: pg.Resource, Engine: "postgresql", Mode: "host", EnvironmentKeys: []string{"DATABASE_URL"}, Tables: -1, BeforeDeploy: m.beforeDeployPolicyLocked(pg.Resource)}
		if databaseFound {
			entry.Backup, entry.LatestBackup = database.Backup, latestBackup(database)
			spec.Data.BackupHours, spec.Data.Offsite = database.Backup.Hours, database.Backup.Offsite
		}
		if pg.Ready && databaseFound && database.Ready && database.Operation == "" {
			entry.Copyable = true
		} else {
			entry.Reason = "unavailable or busy"
			s.Blockers = append(s.Blockers, "Database "+pg.Resource+" is unavailable or has an active operation.")
		}
		s.Databases = append(s.Databases, entry)
	}
	s.Redis = record.Redis != nil
	if s.Redis {
		s.CacheMode = "host"
	}
	if entries, err := os.ReadDir(filepath.Join(policy.Root, "shared", "storage")); err == nil && len(entries) > 0 {
		s.Volumes = append(s.Volumes, "files in shared/storage")
	}
	s.CreateSpec = &spec
	return s, nil
}

// moveEnvironment returns the values an application runs with. Gateway
// transport keys are agent-owned and never leave the host.
func (m *Manager) moveEnvironment(ctx context.Context, id string) (MoveEnvironment, error) {
	out := MoveEnvironment{AppID: id, Values: map[string]string{}, Kinds: map[string]string{}}
	if !idPattern.MatchString(id) {
		return out, errors.New("invalid application id")
	}
	m.mu.Lock()
	a, container := m.data.Apps[id]
	if container {
		m.mu.Unlock()
		if a.Retiring {
			return out, errors.New("application unavailable")
		}
		// The effective environment: the container's, including manifest
		// defaults and image ENV, minus runtime-owned keys.
		effective := m.containerEffectiveEnvironment(ctx, a)
		kinds := normalizeEnvironmentKinds(effective.Values, a.EnvKinds)
		out.Sources = map[string]string{}
		for key, value := range effective.Values {
			out.Values[key], out.Kinds[key], out.Sources[key] = value, kinds[key], effective.Sources[key]
		}
		return out, moveEnvironmentBounds(out.Values)
	}
	m.mu.Unlock()
	native, policy, err := m.nativeReleasePolicy(id)
	if err != nil {
		return out, errors.New("only container applications and release-managed native applications can export their environment")
	}
	values, err := nativeReleaseEnvValues(policy)
	if err != nil {
		return out, err
	}
	kinds := normalizeEnvironmentKinds(values, m.nativeOverride(native.ID).Kinds)
	out.Native = true
	for key, value := range values {
		out.Values[key], out.Kinds[key] = value, kinds[key]
	}
	return out, moveEnvironmentBounds(out.Values)
}

func moveEnvironmentBounds(values map[string]string) error {
	if len(values) > moveMaxEnvironmentKeys {
		return errors.New("the environment has too many variables to export")
	}
	total := 0
	for key, value := range values {
		total += len(key) + len(value)
	}
	if total > moveMaxEnvironmentBytes {
		return errors.New("the environment is too large to export")
	}
	return nil
}

// moveHealth checks the application where it runs: the container's
// readiness probe, or the native web process on its loopback port with the
// application's hostname. It is an explicit action, never an inventory read.
func (m *Manager) moveHealth(ctx context.Context, id string) MoveHealth {
	m.mu.Lock()
	a, container := m.data.Apps[id]
	m.mu.Unlock()
	if container {
		if a.Retiring || a.Current == nil {
			return MoveHealth{Detail: "no running release"}
		}
		if err := m.runtime.Ready(ctx, a.Current); err != nil {
			return MoveHealth{Detail: "readiness check failed"}
		}
		return MoveHealth{Healthy: true, Detail: "container readiness check passed"}
	}
	_, policy, err := m.nativeReleasePolicy(id)
	if err != nil {
		return MoveHealth{Detail: "application unavailable"}
	}
	if view, err := m.nativeReleases(id); err != nil || view.Current == "" {
		return MoveHealth{Detail: "no release is current"}
	}
	if policy.Runtime == "static" || policy.Port == "" {
		return MoveHealth{Healthy: true, Detail: "the current release is published"}
	}
	if err := moveLoopbackHealth(ctx, policy.Port, policy.Domain, policy.HealthPath); err != nil {
		return MoveHealth{Detail: "the web process did not answer its readiness path on its loopback port"}
	}
	return MoveHealth{Healthy: true, Detail: "the web process answered " + cmpOr(policy.HealthPath, "/") + " on its loopback port"}
}

func cmpOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
