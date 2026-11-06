package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agim/lidza/pkg/credentials"
)

// Data services for applications the agent creates: one PostgreSQL role and
// database on the host's cluster, one private Redis server and one database
// on the host's ClickHouse server per application (see
// native_hostdata_postgres.go, native_hostdata_redis.go and
// native_hostdata_clickhouse.go).
//
// The database is registered as a protected native database resource, so the
// existing backup machinery (scheduled and pre-release dumps through the
// isolated pg_dump helper, local retention and verified off-site copies)
// covers it like an adopted database. The agent is the authority for this
// resource: its binding is validated against the provisioning record here
// instead of against a reviewed host configuration entry.

// NativeHostDataConfig tunes host-level data services (native.host_data).
type NativeHostDataConfig struct {
	// PostgresVersion installs this major version from the PostgreSQL
	// project's repository when no cluster of that version exists. 0 uses
	// the existing default cluster, or the distribution's PostgreSQL.
	PostgresVersion int `json:"postgres_version,omitempty"`
	// PostgresPort selects an existing cluster by port.
	PostgresPort int `json:"postgres_port,omitempty"`
	// RedisMaxMemoryMiB is each application's Redis maxmemory (default 128).
	RedisMaxMemoryMiB int `json:"redis_max_memory_mib,omitempty"`
	// ClickHouseVersion is the release series (such as 26.8) installed when
	// an application needs ClickHouse and none is requested; empty selects
	// the newest LTS series. An application moving here requests its
	// source's series.
	ClickHouseVersion string `json:"clickhouse_version,omitempty"`
	// ClickHouseHTTPPort and ClickHouseTCPPort are the loopback ports of the
	// host ClickHouse server (default 8123 and 9000).
	ClickHouseHTTPPort int `json:"clickhouse_http_port,omitempty"`
	ClickHouseTCPPort  int `json:"clickhouse_tcp_port,omitempty"`
	// ClickHouseMemoryRatio caps the server's memory as a share of the
	// host's RAM (default 0.25).
	ClickHouseMemoryRatio float64 `json:"clickhouse_memory_ratio,omitempty"`
}

func (c NativeHostDataConfig) validate() error {
	if c.PostgresVersion != 0 && (c.PostgresVersion < nativePGMinMajor || c.PostgresVersion > nativePGMaxMajor) {
		return fmt.Errorf("native.host_data.postgres_version must be %d–%d", nativePGMinMajor, nativePGMaxMajor)
	}
	if c.PostgresPort != 0 && (c.PostgresPort < 1024 || c.PostgresPort > 65535) {
		return errors.New("native.host_data.postgres_port must be between 1024 and 65535")
	}
	if c.RedisMaxMemoryMiB != 0 && (c.RedisMaxMemoryMiB < 16 || c.RedisMaxMemoryMiB > 65536) {
		return errors.New("native.host_data.redis_max_memory_mib must be between 16 and 65536")
	}
	if c.ClickHouseVersion != "" && !clickhouseSeriesPattern.MatchString(c.ClickHouseVersion) {
		return errors.New("native.host_data.clickhouse_version must be a release series such as 26.8")
	}
	for _, port := range []int{c.ClickHouseHTTPPort, c.ClickHouseTCPPort} {
		if port != 0 && (port < 1024 || port > 65535) {
			return errors.New("native.host_data ClickHouse ports must be between 1024 and 65535")
		}
	}
	if http, tcp := c.clickhousePorts(); http == tcp {
		return errors.New("native.host_data ClickHouse HTTP and TCP ports must differ")
	}
	if c.ClickHouseMemoryRatio != 0 && (c.ClickHouseMemoryRatio < 0.05 || c.ClickHouseMemoryRatio > 0.9) {
		return errors.New("native.host_data.clickhouse_memory_ratio must be between 0.05 and 0.9")
	}
	return nil
}

// NativeHostDataRecord is the agent's provisioning record for one
// application (diskState.NativeHostData). Passwords are sealed individually
// with the agent key in addition to the sealed state file, so a record that
// is copied, logged or returned by mistake never carries a usable secret.
type NativeHostDataRecord struct {
	Postgres   *NativeHostPostgres   `json:"postgres,omitempty"`
	Redis      *NativeHostRedis      `json:"redis,omitempty"`
	ClickHouse *NativeHostClickHouse `json:"clickhouse,omitempty"`
	Updated    time.Time             `json:"updated"`
}

type NativeHostPostgres struct {
	Cluster  string `json:"cluster"` // <major>/<name>, from pg_lsclusters
	Major    int    `json:"major"`
	Port     int    `json:"port"`
	Name     string `json:"name"`     // role and database
	Resource string `json:"resource"` // protected database resource id
	Password string `json:"password_sealed"`
	Ready    bool   `json:"ready"`
}

type NativeHostRedis struct {
	Unit         string `json:"unit"`
	Socket       string `json:"socket"`
	User         string `json:"user"`
	MaxMemoryMiB int    `json:"max_memory_mib"`
	Password     string `json:"password_sealed"`
	Ready        bool   `json:"ready"`
}

func init() {
	provisionNativeData = provisionNativeHostData
	releaseNativeData = releaseNativeHostData
	retainNativeData = retainNativeHostData
}

// nativeHostDataMu serializes host data changes: package installation,
// pg_hba.conf rewrites and per-application provisioning.
var nativeHostDataMu sync.Mutex

// nativeHostData performs host operations. Tests replace nativeHostDataHost
// to run against a fixture directory with recorded commands.
type nativeHostData struct {
	cfg   NativeHostDataConfig
	chown bool
	root  string // fixture prefix for host files; empty on a real host
	run   func(ctx context.Context, stdin, name string, args ...string) (string, error)
	// digest and redisInfo default to nativePGDumpDigest and nativeRedisInfo.
	digest    func(path string) (string, error)
	redisInfo func(ctx context.Context, socket, password string) (nativeRedisMemory, error)
	// clickhouseURL overrides the host ClickHouse server's address and
	// download fetches the repository signing key (tests replace both).
	clickhouseURL string
	download      func(ctx context.Context, url string) ([]byte, error)
}

func (h nativeHostData) dumpDigest(path string) (string, error) {
	if h.digest != nil {
		return h.digest(path)
	}
	return nativePGDumpDigest(path)
}

func (h nativeHostData) redisMemory(ctx context.Context, socket, password string) (nativeRedisMemory, error) {
	if h.redisInfo != nil {
		return h.redisInfo(ctx, socket, password)
	}
	return nativeRedisInfo(ctx, socket, password)
}

var nativeHostDataHost = func(m *Manager) nativeHostData {
	return nativeHostData{cfg: m.nativeHostDataConfig(), chown: true, run: nativeHostRun}
}

func (h nativeHostData) path(name string) string {
	if h.root == "" {
		return name
	}
	return filepath.Join(h.root, name)
}

func (h nativeHostData) fileExists(name string) bool {
	_, err := os.Stat(h.path(name))
	return err == nil
}

// nativeHostRun runs a host tool with a fixed environment, from /, with
// optional stdin, and returns its bounded combined output. Package
// operations keep existing configuration and never restart services.
func nativeHostRun(ctx context.Context, stdin, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = "/"
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l", "NEEDRESTART_SUSPEND=1", "APT_LISTCHANGES_FRONTEND=none", "PGAPPNAME=lidza-host-data"}
	if stdin != "" {
		command.Stdin = strings.NewReader(stdin)
	}
	var output limitedBuffer
	command.Stdout = &output
	command.Stderr = &output
	command.WaitDelay = 5 * time.Second
	err := command.Run()
	return output.String(), err
}

func (m *Manager) nativeHostDataConfig() NativeHostDataConfig {
	if m.cfg.Native != nil && m.cfg.Native.HostData != nil {
		return *m.cfg.Native.HostData
	}
	return NativeHostDataConfig{}
}

func (m *Manager) sealHostSecret(secret string) (string, error) {
	return credentials.Encrypt(m.key, []byte(secret))
}

func (m *Manager) unsealHostSecret(sealed string) (string, error) {
	plain, err := credentials.Decrypt(m.key, sealed)
	if err != nil {
		return "", errors.New("sealed data service credential could not be opened")
	}
	return string(plain), nil
}

// nativeHostDataBackupPolicy maps the request's schedule onto the native
// database policy: daily by default, one local copy, optional off-site copy.
func nativeHostDataBackupPolicy(req NativeDataRequest) (BackupPolicy, error) {
	hours := req.BackupHours
	if hours == 0 {
		hours = 24
	}
	policy := BackupPolicy{Hours: hours, Keep: 1, Offsite: req.Offsite}
	if hours != 1 && hours != 24 && hours != 168 {
		return policy, errors.New("database backups run hourly (1), daily (24) or weekly (168)")
	}
	return policy, policy.Validate()
}

// nativeHostResourceID names a new incarnation's database resource: the app
// id and a random suffix, so retained backups of a removed application never
// block reusing its id.
func nativeHostResourceID(id string) string {
	// "-db" and eight hex digits follow, within the 48-character id limit.
	base := strings.TrimRight(id[:min(len(id), 37)], "-")
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-db%x", base, b[:])
}

var nativeHostRolePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func validateNativeAppAccount(account NativeAppAccount) error {
	if !nativeHostRolePattern.MatchString(account.User) || account.User == "root" || account.UID <= 0 || account.GID <= 0 {
		return errors.New("data services need the application's isolated account")
	}
	return validNativeRedisPath(account.Root)
}

func provisionNativeHostData(ctx context.Context, m *Manager, id string, account NativeAppAccount, req NativeDataRequest) (NativeDataGrant, error) {
	grant := NativeDataGrant{Env: map[string]string{}}
	if !idPattern.MatchString(id) {
		return grant, errors.New("invalid application id")
	}
	if m.cfg.IngressOnly {
		return grant, errors.New("this agent role cannot provision data services")
	}
	if req.ExternalDatabaseURL != "" {
		if req.Postgres {
			return grant, errors.New("choose a host database or an external database URL, not both")
		}
		if err := (DatabaseRequest{Mode: "external", URL: req.ExternalDatabaseURL, Backup: BackupPolicy{Keep: 1}}).Validate(); err != nil {
			return grant, err
		}
		grant.Env["DATABASE_URL"] = req.ExternalDatabaseURL
	}
	if !req.Postgres && !req.Redis && !req.ClickHouse {
		return grant, nil
	}
	if err := validateNativeAppAccount(account); err != nil {
		return grant, err
	}
	h := nativeHostDataHost(m)
	if err := h.cfg.validate(); err != nil {
		return grant, err
	}
	nativeHostDataMu.Lock()
	defer nativeHostDataMu.Unlock()
	if req.Postgres {
		backup, err := nativeHostDataBackupPolicy(req)
		if err != nil {
			return grant, err
		}
		raw, resource, err := m.provisionNativePostgres(ctx, h, id, backup)
		if err != nil {
			return grant, err
		}
		grant.Env["DATABASE_URL"] = raw
		grant.Databases = []string{resource}
	}
	if req.Redis {
		password, socket, err := m.provisionNativeRedis(ctx, h, id, account)
		if err != nil {
			return grant, err
		}
		grant.Env["REDIS_URL"] = nativeRedisURL(socket, password)
		grant.Env["REDIS_SOCKET"] = socket
		grant.Env["REDIS_PASSWORD"] = password
	}
	if req.ClickHouse {
		backup, err := nativeHostDataBackupPolicy(req)
		if err != nil {
			return grant, err
		}
		env, resource, err := m.provisionNativeClickHouse(ctx, h, id, backup, req.ClickHouseVersion)
		if err != nil {
			return grant, err
		}
		maps.Copy(grant.Env, env)
		grant.Databases = append(grant.Databases, resource)
	}
	return grant, nil
}

// updateHostDataRecord applies change to the application's record and saves
// state, restoring the previous record when the save fails.
func (m *Manager) updateHostDataRecord(id string, change func(*NativeHostDataRecord)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous, existed := m.data.NativeHostData[id]
	next := previous
	if previous.Postgres != nil {
		copy := *previous.Postgres
		next.Postgres = &copy
	}
	if previous.Redis != nil {
		copy := *previous.Redis
		next.Redis = &copy
	}
	if previous.ClickHouse != nil {
		copy := *previous.ClickHouse
		next.ClickHouse = &copy
	}
	change(&next)
	next.Updated = time.Now().UTC()
	if m.data.NativeHostData == nil {
		m.data.NativeHostData = map[string]NativeHostDataRecord{}
	}
	if next.Postgres == nil && next.Redis == nil && next.ClickHouse == nil {
		delete(m.data.NativeHostData, id)
	} else {
		m.data.NativeHostData[id] = next
	}
	if err := m.save(); err != nil {
		if existed {
			m.data.NativeHostData[id] = previous
		} else {
			delete(m.data.NativeHostData, id)
		}
		return err
	}
	return nil
}

func (m *Manager) hostDataRecord(id string) (NativeHostDataRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.data.NativeHostData[id]
	return record, ok
}

// provisionNativePostgres creates or repairs the application's role and
// database and registers the protected resource. Re-provisioning keeps the
// password, cluster and resource; it never moves a database to another
// cluster.
func (m *Manager) provisionNativePostgres(ctx context.Context, h nativeHostData, id string, backup BackupPolicy) (string, string, error) {
	name, err := nativePGName(id)
	if err != nil {
		return "", "", err
	}
	if backup.Offsite {
		m.mu.Lock()
		configured := m.data.BackupStorage != nil
		m.mu.Unlock()
		if !configured {
			return "", "", errors.New("configure off-site backup storage before requesting off-site database backups")
		}
	}
	record, _ := m.hostDataRecord(id)
	var cluster pgCluster
	password := nativeRandomSecret()
	resource := nativeHostResourceID(id)
	if existing := record.Postgres; existing != nil {
		if password, err = m.unsealHostSecret(existing.Password); err != nil {
			return "", "", err
		}
		clusters, _ := h.listClusters(ctx)
		if cluster, err = chooseNativePGCluster(clusters, existing.Major, existing.Port); err != nil {
			return "", "", fmt.Errorf("the application's PostgreSQL cluster %s on port %d is unavailable: %w", existing.Cluster, existing.Port, err)
		}
		resource = existing.Resource
	} else if cluster, err = h.ensurePostgres(ctx); err != nil {
		return "", "", err
	}
	sealed, err := m.sealHostSecret(password)
	if err != nil {
		return "", "", err
	}
	pg := NativeHostPostgres{Cluster: cluster.ID(), Major: cluster.Major, Port: cluster.Port, Name: name, Resource: resource, Password: sealed, Ready: record.Postgres != nil && record.Postgres.Ready}
	// Record the credential before it exists on the server, so an
	// interrupted provisioning is repaired or released with the same secret.
	if err = m.updateHostDataRecord(id, func(r *NativeHostDataRecord) { r.Postgres = &pg }); err != nil {
		return "", "", err
	}
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	verifier, err := nativeSCRAMVerifier(password, salt, nativeSCRAMIterates)
	if err != nil {
		return "", "", err
	}
	script, err := renderNativePGProvisionSQL(name, verifier)
	if err != nil {
		return "", "", err
	}
	if _, err = h.psql(ctx, cluster.Port, script); err != nil {
		return "", "", errors.New("PostgreSQL role/database provisioning failed")
	}
	if err = h.ensureHBA(ctx, cluster.Port, m.hostDataHBAEntries(cluster.Port, "")); err != nil {
		return "", "", err
	}
	h.ensureLogicalWAL(ctx, cluster)
	digest, err := h.dumpDigest(h.path(nativePGDumpPath(cluster.Major)))
	if err != nil {
		return "", "", err
	}
	raw := nativePGURL(name, password, cluster.Port)
	policy := NativeDatabasePolicy{Enabled: true, AppID: id, EnvironmentKey: "DATABASE_URL", Roles: []string{"primary"}, Host: "127.0.0.1", Port: fmt.Sprint(cluster.Port),
		ConfigSHA256: nativeHostDataPolicyDigest(cluster.ID(), cluster.Port, name), PostgresMajor: cluster.Major, DumpSHA256: digest, Backup: backup}
	if err = validateNativeDatabasePolicy(resource, policy); err != nil {
		return "", "", err
	}
	if err = m.registerHostDatabase(id, resource, raw, policy); err != nil {
		return "", "", err
	}
	return raw, resource, nil
}

// registerHostDatabase binds the database as a protected native resource.
// An existing resource keeps its backups and the schedule set since.
func (m *Manager) registerHostDatabase(id, resource, raw string, policy NativeDatabasePolicy) error {
	return m.registerHostResource(id, resource, raw, policy, func(r *NativeHostDataRecord) bool {
		if r.Postgres == nil || r.Postgres.Resource != resource {
			return false
		}
		pg := *r.Postgres
		pg.Ready = true
		r.Postgres = &pg
		return true
	})
}

// registerHostResource binds a provisioned database (PostgreSQL or
// ClickHouse) as a protected resource. claim checks that the record still
// owns the resource and marks it ready.
func (m *Manager) registerHostResource(id, resource, raw string, policy NativeDatabasePolicy, claim func(*NativeHostDataRecord) bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.data.NativeHostData[id]
	if !ok || !claim(&record) {
		return errors.New("database provisioning record changed")
	}
	if _, app := m.data.Apps[resource]; app {
		return errors.New("database resource id conflicts with an application")
	}
	previousBinding, bound := m.data.NativeDatabases[resource]
	previous, exists := m.data.Databases[resource]
	if exists && (!bound || previousBinding.Policy.AppID != id) {
		return errors.New("database resource id is already in use")
	}
	if !exists && len(m.data.Databases) >= 200 {
		return errors.New("agent database limit reached")
	}
	d := Database{AppID: resource, Mode: "external", Backup: policy.Backup, NextBackup: policy.Backup.NextBackupAfter(time.Now())}
	adopted := time.Now().UTC()
	if exists {
		d = previous
		policy.Backup = previous.Backup
		policy.Backup.Keep = 1
		adopted = previousBinding.Adopted
	}
	d.URL, d.Ready = raw, true
	if m.data.NativeDatabases == nil {
		m.data.NativeDatabases = map[string]NativeDatabaseBinding{}
	}
	if m.data.Databases == nil {
		m.data.Databases = map[string]Database{}
	}
	record.Updated = time.Now().UTC()
	m.data.NativeDatabases[resource] = NativeDatabaseBinding{Policy: policy, Adopted: adopted}
	m.data.Databases[resource] = d
	m.data.NativeHostData[id] = record
	if err := m.save(); err != nil {
		if bound {
			m.data.NativeDatabases[resource] = previousBinding
		} else {
			delete(m.data.NativeDatabases, resource)
		}
		if exists {
			m.data.Databases[resource] = previous
		} else {
			delete(m.data.Databases, resource)
		}
		return err
	}
	return nil
}

// hostDataHBAEntries lists the applications provisioned on the cluster at
// port, except the one being released.
func (m *Manager) hostDataHBAEntries(port int, except string) []nativeHBAEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := []nativeHBAEntry{}
	for id, record := range m.data.NativeHostData {
		if id != except && record.Postgres != nil && record.Postgres.Port == port {
			entries = append(entries, nativeHBAEntry{Role: record.Postgres.Name, Database: record.Postgres.Name})
		}
	}
	return append(entries, m.moveReplicationHBAEntriesLocked(port)...)
}

func (m *Manager) provisionNativeRedis(ctx context.Context, h nativeHostData, id string, account NativeAppAccount) (string, string, error) {
	record, _ := m.hostDataRecord(id)
	password := nativeRandomSecret()
	if record.Redis != nil {
		var err error
		if password, err = m.unsealHostSecret(record.Redis.Password); err != nil {
			return "", "", err
		}
	}
	maxMiB := h.cfg.RedisMaxMemoryMiB
	if maxMiB == 0 {
		maxMiB = nativeRedisDefaultMiB
	}
	sealed, err := m.sealHostSecret(password)
	if err != nil {
		return "", "", err
	}
	socket := nativeRedisSocket(account.Root)
	redis := NativeHostRedis{Unit: nativeRedisUnit(id), Socket: socket, User: account.User, MaxMemoryMiB: maxMiB, Password: sealed}
	if err = m.updateHostDataRecord(id, func(r *NativeHostDataRecord) { r.Redis = &redis }); err != nil {
		return "", "", err
	}
	if err = h.ensureRedis(ctx, id, account, password, maxMiB); err != nil {
		return "", "", err
	}
	redis.Ready = true
	if err = m.updateHostDataRecord(id, func(r *NativeHostDataRecord) { r.Redis = &redis }); err != nil {
		return "", "", err
	}
	return password, socket, nil
}

// releaseNativeHostData takes a final backup, then drops the database and
// role and removes the Redis instance. The database resource stays as a
// retained entry with its backups (no credential); the application root,
// including Redis data, is purged by the caller.
func releaseNativeHostData(ctx context.Context, m *Manager, id string) error {
	return releaseNativeHostDataRecord(ctx, m, id, true)
}

// releaseNativeHostDataRecord drops the application's data services. With
// finalBackup it first takes (or reuses) a fresh backup of each database;
// without it the caller has already verified a covering backup (retained
// resource removal).
func releaseNativeHostDataRecord(ctx context.Context, m *Manager, id string, finalBackup bool) error {
	if !idPattern.MatchString(id) {
		return errors.New("invalid application id")
	}
	nativeHostDataMu.Lock()
	defer nativeHostDataMu.Unlock()
	record, ok := m.hostDataRecord(id)
	if !ok {
		return nil
	}
	h := nativeHostDataHost(m)
	if pg := record.Postgres; pg != nil {
		m.mu.Lock()
		d, exists := m.data.Databases[pg.Resource]
		binding, bound := m.data.NativeDatabases[pg.Resource]
		m.mu.Unlock()
		// A database whose application was never released holds no
		// application data (migrations run during releases), so undoing a
		// failed creation does not need, or wait on, a final backup.
		if finalBackup && exists && bound && binding.Policy.AppID == id && d.Ready && pg.Ready && !m.nativeCreatedNeverReleased(id) {
			if err := m.freshBackupBeforeRelease(ctx, []string{pg.Resource}); err != nil {
				return fmt.Errorf("final database backup failed; the database is retained: %w", err)
			}
		}
		if err := m.unbindHostDatabase(id, pg.Resource); err != nil {
			return err
		}
		script, err := renderNativePGReleaseSQL(pg.Name)
		if err != nil {
			return err
		}
		if _, err = h.psql(ctx, pg.Port, script); err != nil {
			return errors.New("dropping the application's PostgreSQL database failed; retry the removal")
		}
		if err = h.ensureHBA(ctx, pg.Port, m.hostDataHBAEntries(pg.Port, id)); err != nil {
			return err
		}
		if err = m.updateHostDataRecord(id, func(r *NativeHostDataRecord) { r.Postgres = nil }); err != nil {
			return err
		}
	}
	if record.Redis != nil {
		if err := h.removeRedis(ctx, id); err != nil {
			return err
		}
		if !finalBackup {
			// A retained instance's data directory stayed in the
			// application root; removing the resource deletes it.
			if err := os.RemoveAll(h.path(nativeRedisDir(filepath.Join(m.nativeAppsRoot(), id)))); err != nil {
				return fmt.Errorf("could not delete the Redis data of %s: %w", id, err)
			}
		}
		if err := m.updateHostDataRecord(id, func(r *NativeHostDataRecord) { r.Redis = nil }); err != nil {
			return err
		}
	}
	if record.ClickHouse != nil {
		if err := m.releaseNativeClickHouse(ctx, h, id, *record.ClickHouse, finalBackup); err != nil {
			return err
		}
	}
	return nil
}

// unbindHostDatabase removes the protected binding and the credential from
// the database resource; its backups stay available as a retained resource.
func (m *Manager) unbindHostDatabase(id, resource string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	binding, bound := m.data.NativeDatabases[resource]
	if !bound {
		return nil
	}
	if binding.Policy.AppID != id {
		return errors.New("database resource belongs to another application")
	}
	d, exists := m.data.Databases[resource]
	previous := d
	delete(m.data.NativeDatabases, resource)
	if exists {
		if u, err := url.Parse(d.URL); err == nil && u.User != nil {
			u.User = url.User(u.User.Username())
			d.URL = u.String()
		}
		m.data.Databases[resource] = d
	}
	if err := m.save(); err != nil {
		m.data.NativeDatabases[resource] = binding
		if exists {
			m.data.Databases[resource] = previous
		}
		return err
	}
	return nil
}

// ---- hooks used by the native database backup path -------------------------

// nativeHostDataBoundLocked reports whether resource is a database this agent
// provisioned and still holds for the application in p. Caller holds m.mu.
func (m *Manager) nativeHostDataBoundLocked(resource string, p NativeDatabasePolicy) bool {
	record, ok := m.data.NativeHostData[p.AppID]
	if ok && p.ClickHouseDatabase != "" {
		ch := record.ClickHouse
		return ch != nil && ch.Ready && ch.Resource == resource && ch.Name == p.ClickHouseDatabase && p.ConfigSHA256 == nativeHostClickHouseDigest(ch.HTTPPort, ch.Name) &&
			p.Host == "127.0.0.1" && p.Port == strconv.Itoa(ch.HTTPPort) && validateNativeDatabasePolicy(resource, p) == nil
	}
	if !ok || record.Postgres == nil {
		return false
	}
	pg := record.Postgres
	return pg.Ready && pg.Resource == resource && p.ConfigSHA256 == nativeHostDataPolicyDigest(pg.Cluster, pg.Port, pg.Name) &&
		p.PostgresMajor == pg.Major && p.Port == fmt.Sprint(pg.Port) && p.Host == "127.0.0.1" && validateNativeDatabasePolicy(resource, p) == nil
}

// nativeHostDataSource returns the connection for an agent-provisioned
// database policy; managed is false for every other policy.
func (m *Manager) nativeHostDataSource(p NativeDatabasePolicy) (raw string, managed bool, err error) {
	m.mu.Lock()
	record, ok := m.data.NativeHostData[p.AppID]
	m.mu.Unlock()
	if ok && p.ClickHouseDatabase != "" {
		ch := record.ClickHouse
		if ch == nil || ch.Name != p.ClickHouseDatabase || p.ConfigSHA256 != nativeHostClickHouseDigest(ch.HTTPPort, ch.Name) {
			return "", false, nil
		}
		return nativeClickHouseResourceURL(ch.Name, ch.HTTPPort), true, nil
	}
	if !ok || record.Postgres == nil || p.ConfigSHA256 != nativeHostDataPolicyDigest(record.Postgres.Cluster, record.Postgres.Port, record.Postgres.Name) {
		return "", false, nil
	}
	password, err := m.unsealHostSecret(record.Postgres.Password)
	if err != nil {
		return "", true, err
	}
	return nativePGURL(record.Postgres.Name, password, record.Postgres.Port), true, nil
}

var nativePGClientPackage = regexp.MustCompile(`^postgresql-client-\d+$`)

// refreshNativeHostDataTool follows package updates of pg_dump for databases
// the agent provisioned: the new binary is accepted when the package manager
// verifies it unchanged from its package. Otherwise the pinned digest stays
// and the backup fails closed.
func (m *Manager) refreshNativeHostDataTool(ctx context.Context, resource string) {
	m.mu.Lock()
	binding, ok := m.data.NativeDatabases[resource]
	bound := ok && m.nativeHostDataBoundLocked(resource, binding.Policy) && binding.Policy.ClickHouseDatabase == ""
	m.mu.Unlock()
	if !bound {
		return
	}
	h := nativeHostDataHost(m)
	path := nativePGDumpPath(binding.Policy.PostgresMajor)
	digest, err := h.dumpDigest(h.path(path))
	if err != nil || digest == binding.Policy.DumpSHA256 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	owner, err := h.run(ctx, "", "/usr/bin/dpkg-query", "-S", path)
	pkg, _, _ := strings.Cut(strings.TrimSpace(owner), ":")
	if err != nil || !nativePGClientPackage.MatchString(pkg) {
		return
	}
	verify, err := h.run(ctx, "", "/usr/bin/dpkg", "--verify", pkg)
	if err != nil || slices.ContainsFunc(strings.Split(verify, "\n"), func(line string) bool { return strings.HasSuffix(strings.TrimSpace(line), " "+path) }) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.data.NativeDatabases[resource]
	if !ok || current.Policy.DumpSHA256 != binding.Policy.DumpSHA256 {
		return
	}
	updated := current
	updated.Policy.Roles = slices.Clone(current.Policy.Roles)
	updated.Policy.DumpSHA256 = digest
	m.data.NativeDatabases[resource] = updated
	if m.save() != nil {
		m.data.NativeDatabases[resource] = current
	}
}

// nativeCreatedNeverReleased reports whether id is an application this agent
// created that has no release yet.
func (m *Manager) nativeCreatedNeverReleased(id string) bool {
	m.mu.Lock()
	policy, created := m.data.NativeCreated[id]
	m.mu.Unlock()
	if !created {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(policy.Root, "releases"))
	return errors.Is(err, os.ErrNotExist) || err == nil && len(entries) == 0
}

// retainNativeHostData keeps the data services of a removed application as
// a retained resource, as removing a container application keeps its
// volumes: the PostgreSQL database and role, the ClickHouse database and the
// Redis data stay, with their backups and the ability to take a final one.
// The Redis instance
// runs as the application's account, which the removal deletes, so it is
// stopped and disabled; its configuration and data directory stay until
// the retained resource is removed (removeRetainedNativeHostData).
func retainNativeHostData(ctx context.Context, m *Manager, id string, origin *RetainedOrigin) (string, error) {
	if !idPattern.MatchString(id) {
		return "", errors.New("invalid application id")
	}
	nativeHostDataMu.Lock()
	defer nativeHostDataMu.Unlock()
	record, ok := m.hostDataRecord(id)
	if !ok {
		return "", nil
	}
	if record.Redis != nil {
		h := nativeHostDataHost(m)
		unit := nativeRedisUnit(id)
		if out, err := h.run(ctx, "", "/usr/bin/systemctl", "disable", "--now", unit); err != nil && !strings.Contains(out, "not loaded") && !strings.Contains(out, "does not exist") {
			return "", fmt.Errorf("stopping %s failed: %s", unit, nativeTrimOutput(out))
		}
		if err := m.updateHostDataRecord(id, func(r *NativeHostDataRecord) { r.Redis.Ready = false }); err != nil {
			return "", err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, retained := range m.data.RetainedResources {
		if retained.NativeData && retained.AppID == id {
			return retained.ID, nil
		}
	}
	databases := []string{}
	for _, resource := range record.databaseResources() {
		if _, exists := m.data.Databases[resource]; exists {
			databases = append(databases, resource)
		}
	}
	if m.data.RetainedResources == nil {
		m.data.RetainedResources = map[string]RetainedResource{}
	}
	now := time.Now().UTC()
	key := newID()
	m.data.RetainedResources[key] = RetainedResource{ID: key, AppID: id, Created: now, WritesStopped: now, Databases: databases, NativeData: true, Origin: origin}
	if err := m.save(); err != nil {
		delete(m.data.RetainedResources, key)
		return "", err
	}
	return key, nil
}

// databaseResources lists the database resources of the record's
// PostgreSQL and ClickHouse databases.
func (r NativeHostDataRecord) databaseResources() []string {
	out := []string{}
	if r.Postgres != nil && r.Postgres.Resource != "" {
		out = append(out, r.Postgres.Resource)
	}
	if r.ClickHouse != nil && r.ClickHouse.Resource != "" {
		out = append(out, r.ClickHouse.Resource)
	}
	return out
}

// removeRetainedNativeHostData deletes retained native data services after
// the retained resource removal verified a covering backup of each
// database: the PostgreSQL database and role, the Redis instance with its
// data directory, and the ClickHouse database, user and profile. Backup
// records and off-site copies stay.
var removeRetainedNativeHostData = func(ctx context.Context, m *Manager, id string) error {
	return releaseNativeHostDataRecord(ctx, m, id, false)
}
