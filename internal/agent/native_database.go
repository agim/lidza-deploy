package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// This policy contains no URL/password. Installing it is an explicit reviewed
// resource adoption, independently of service/deployment ownership. The common
// backup scheduler may run after adoption; no Rails process is booted or probed.
type NativeDatabasePolicy struct {
	Enabled        bool     `json:"enabled"`
	AppID          string   `json:"app_id"`
	EnvironmentKey string   `json:"environment_key"`
	Roles          []string `json:"roles"`
	Host           string   `json:"host"`
	Port           string   `json:"port"`
	// MaintenancePort is the direct PostgreSQL port behind a loopback
	// connection pooler (for example PgBouncer on Port). The application keeps
	// its pooled URL; dumps and identity checks use the same host on this port.
	MaintenancePort string       `json:"maintenance_port,omitempty"`
	ConfigSHA256    string       `json:"config_sha256"`
	PostgresMajor   int          `json:"postgres_major"`
	DumpSHA256      string       `json:"dump_sha256"`
	SQLitePath      string       `json:"sqlite_path,omitempty"` // Existing file relative to the application's shared directory.
	ArchiveDatabase string       `json:"archive_database,omitempty"`
	Backup          BackupPolicy `json:"backup"`
}

type NativeDatabaseBinding struct {
	Policy  NativeDatabasePolicy `json:"policy"`
	Adopted time.Time            `json:"adopted"`
}

// Rails database roles. "secondary" is a separately configured database the
// application reaches through its own environment key (any name other than
// DATABASE_URL), declared in a policy of its own.
func validNativeDatabaseRole(role string) bool {
	return slices.Contains([]string{"primary", "queue", "cache", "cable", "secondary"}, role)
}

// validNativeMaintenancePort reports whether p names a direct server port that
// differs from the pooled port of a loopback connection.
func validNativeMaintenancePort(p NativeDatabasePolicy) bool {
	port, err := strconv.Atoi(p.MaintenancePort)
	ip := net.ParseIP(p.Host)
	return err == nil && port >= 1 && port <= 65535 && strconv.Itoa(port) == p.MaintenancePort && p.MaintenancePort != p.Port && ip != nil && ip.IsLoopback()
}

func validateNativeDatabasePolicy(id string, p NativeDatabasePolicy) error {
	if !idPattern.MatchString(id) || !p.Enabled || !nativeHexDigest(p.ConfigSHA256, 64) || !nativeHexDigest(p.DumpSHA256, 64) {
		return errors.New("native database requires a reviewed exact resource policy")
	}
	if p.ArchiveDatabase != "" {
		return validateNativeArchiveDatabase(id, p)
	}
	if !idPattern.MatchString(p.AppID) || len(p.Roles) == 0 || len(p.Roles) > 5 {
		return errors.New("native database requires a reviewed exact application and role policy")
	}
	if p.SQLitePath != "" {
		if !validNativeSQLitePath(p.SQLitePath) || p.Host != "" || p.Port != "" || p.MaintenancePort != "" || p.PostgresMajor != 0 || p.EnvironmentKey != "" && !validDatabaseKey(p.EnvironmentKey) {
			return errors.New("native SQLite database requires an exact shared file policy")
		}
	} else {
		port, err := strconv.Atoi(p.Port)
		if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != p.Port {
			return errors.New("native database port must be exact")
		}
		if p.Host == "" || strings.ContainsAny(p.Host, "/ \t\r\n") || net.ParseIP(p.Host) == nil && validateHostname(p.Host) != nil ||
			!validDatabaseKey(p.EnvironmentKey) || p.PostgresMajor < 16 || p.PostgresMajor > 18 {
			return errors.New("native database requires one exact PostgreSQL connection")
		}
		if p.MaintenancePort != "" && !validNativeMaintenancePort(p) {
			return errors.New("native PostgreSQL maintenance port requires a distinct direct port behind a loopback pool")
		}
	}
	seen := map[string]bool{}
	for _, role := range p.Roles {
		if !validNativeDatabaseRole(role) || seen[role] {
			return errors.New("native database roles are invalid")
		}
		seen[role] = true
	}
	if seen["secondary"] && (p.EnvironmentKey == "DATABASE_URL" || len(p.Roles) != 1 || p.SQLitePath != "") {
		return errors.New("a secondary native database requires its own separate connection key")
	}
	if p.Backup.Keep != 1 {
		return errors.New("native database keeps exactly one finalized local backup")
	}
	return p.Backup.Validate()
}

func sameNativeDatabasePolicy(a, b NativeDatabasePolicy) bool {
	return a.Enabled == b.Enabled && a.AppID == b.AppID && a.EnvironmentKey == b.EnvironmentKey && slices.Equal(a.Roles, b.Roles) && a.Host == b.Host && a.Port == b.Port && a.MaintenancePort == b.MaintenancePort && a.ConfigSHA256 == b.ConfigSHA256 && a.PostgresMajor == b.PostgresMajor && a.DumpSHA256 == b.DumpSHA256 && a.SQLitePath == b.SQLitePath && a.ArchiveDatabase == b.ArchiveDatabase && a.Backup == b.Backup
}

var nativePostgresDatabasePath = regexp.MustCompile(`^/[A-Za-z0-9_][A-Za-z0-9_.-]{0,62}$`)

func validateNativeDatabaseURL(raw string, p NativeDatabasePolicy) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.User == nil || u.User.Username() == "" || u.Hostname() == "" || u.Hostname() != p.Host || u.Fragment != "" || !nativePostgresDatabasePath.MatchString(u.Path) || len(raw) > 8192 {
		return errors.New("existing native database URL differs from its approved resource")
	}
	if p.ArchiveDatabase != "" && u.Path != "/"+p.ArchiveDatabase {
		return errors.New("standalone native backup database name changed")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	if port != p.Port {
		return errors.New("existing native database port changed")
	}
	// Preserve the original SSL mode, including a reviewed private network URL.
	// No URL options may execute or select an alternate host/service/file.
	for key, values := range u.Query() {
		if !slices.Contains([]string{"sslmode", "channel_binding", "connect_timeout", "application_name"}, key) || len(values) != 1 {
			return errors.New("native database URL options require review")
		}
	}
	return nil
}

// Read the registered app's literal configuration; never boot Rails, interpolate
// ERB/shell or infer multi-database roles. The reviewed file hash declares roles.
func (m *Manager) nativeDatabaseSource(p NativeDatabasePolicy) (string, error) {
	if raw, managed, err := m.nativeHostDataSource(p); managed {
		return raw, err // provisioned by this agent; see native_hostdata.go
	}
	if p.ArchiveDatabase != "" {
		return nativeArchiveMetadataURL(p.ArchiveDatabase), nil
	}
	a, err := m.nativeApp(p.AppID)
	// Before multi-runtime inventory, Rails registrations omitted runtime. Keep
	// that legacy meaning, as preparation, serving and worker authority do; do
	// not rewrite registrations merely to adopt their existing database.
	if err != nil || a.Runtime != "rails" && a.Runtime != "" {
		return "", errors.New("native database requires a registered Rails application")
	}
	data := m.nativeMetadataFile(filepath.Join(a.Name, "current/config/database.yml"))
	digest := sha256.Sum256(data)
	if len(data) == 0 || hex.EncodeToString(digest[:]) != p.ConfigSHA256 {
		return "", errors.New("Rails database configuration changed; adoption needs review")
	}
	if p.SQLitePath != "" {
		return m.nativeSQLiteSource(a, p)
	}
	values, err := m.nativeBuildEnvironment(a.Name)
	if err != nil {
		return "", err
	}
	raw := values[p.EnvironmentKey]
	if err := validateNativeDatabaseURL(raw, p); err != nil {
		return "", err
	}
	return raw, nil
}

func probeNativeDatabase(ctx context.Context, raw string, major int) error {
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		return errors.New("native PostgreSQL connection is invalid")
	}
	cfg.ConnectTimeout = 8 * time.Second
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["statement_timeout"] = "5000"
	cfg.RuntimeParams["lock_timeout"] = "1000"
	cfg.RuntimeParams["application_name"] = "lidza-native-database-adoption"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return errors.New("native PostgreSQL read-only connection failed")
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		conn.Close(closeCtx)
	}()
	var version int
	var readOnly string
	if err = conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer,current_setting('transaction_read_only')").Scan(&version, &readOnly); err != nil || version/10000 != major || readOnly != "on" {
		return errors.New("native PostgreSQL version/read-only validation failed")
	}
	return nil
}

func (m *Manager) adoptNativeDatabase(ctx context.Context, id string) error {
	if m.cfg.Native == nil || m.cfg.NativeOnly || m.cfg.IngressOnly {
		return errors.New("this role cannot adopt native databases")
	}
	p, ok := m.cfg.Native.Databases[id]
	if !ok {
		return errors.New("native database adoption is not approved")
	}
	if err := validateNativeDatabasePolicy(id, p); err != nil {
		return err
	}
	raw, err := m.nativeDatabaseSource(p)
	if err != nil {
		return err
	}
	if err = checkNativeBackupSource(ctx, raw, p); err != nil {
		return err
	}
	// Recheck after the network operation. A concurrent settings/release change
	// cannot bind the now-stale credentials to the resource silently.
	after, err := m.nativeDatabaseSource(p)
	if err != nil || after != raw {
		return errors.New("native database source changed during adoption")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nativeOperations[p.AppID] {
		return errors.New("native application operation is active")
	}
	if _, exists := m.data.Apps[id]; exists {
		return errors.New("native database identity conflicts with a container application")
	}
	if prior, exists := m.data.Databases[id]; exists {
		binding, bound := m.data.NativeDatabases[id]
		if bound && sameNativeDatabasePolicy(binding.Policy, p) && prior.URL == raw && prior.Ready && prior.Mode == "external" {
			return nil
		}
		return errors.New("native database identity already exists; existing resource retained")
	}
	if len(m.data.Databases) >= 200 {
		return errors.New("agent database limit reached")
	}
	for resource, binding := range m.data.NativeDatabases {
		if binding.Policy.AppID == p.AppID && resource != id {
			if p.EnvironmentKey != "" && binding.Policy.EnvironmentKey == p.EnvironmentKey {
				return errors.New("native database variable is already bound")
			}
			for _, role := range p.Roles {
				if slices.Contains(binding.Policy.Roles, role) {
					return errors.New("native database role is already bound")
				}
			}
		}
	}
	for resource, existing := range m.data.Databases {
		if resource != id && (existing.URL == raw || nativeArchiveMatchesURL(p, existing.URL)) {
			return errors.New("this native connection is already tracked; duplicate resource refused")
		}
	}
	if p.Backup.Offsite && m.data.BackupStorage == nil {
		return errors.New("reviewed off-site storage must be configured before native adoption")
	}
	if m.data.NativeDatabases == nil {
		m.data.NativeDatabases = map[string]NativeDatabaseBinding{}
	}
	if m.data.Databases == nil {
		m.data.Databases = map[string]Database{}
	}
	p.Roles = slices.Clone(p.Roles)
	p.Backup.Keep = 1
	next := time.Time{}
	if p.Backup.Cron != "" {
		next = p.Backup.NextBackupAfter(time.Now())
	} else if p.Backup.Hours > 0 {
		next = time.Now().UTC()
	}
	m.data.NativeDatabases[id] = NativeDatabaseBinding{Policy: p, Adopted: time.Now().UTC()}
	m.data.Databases[id] = Database{AppID: id, Mode: "external", URL: raw, Ready: true, Backup: p.Backup, NextBackup: next}
	if err = m.save(); err != nil {
		delete(m.data.NativeDatabases, id)
		delete(m.data.Databases, id)
		return err
	}
	return nil
}

// Caller holds m.mu; no filesystem/database access or app health probe here.
func (m *Manager) nativeDatabaseInUseLocked(id string) bool {
	binding, ok := m.data.NativeDatabases[id]
	if ok && m.nativeHostDataBoundLocked(id, binding.Policy) {
		return true // provisioned by this agent; see native_hostdata.go
	}
	if !ok || m.cfg.Native == nil {
		return false
	}
	p, approved := m.cfg.Native.Databases[id]
	a, registered := m.data.NativeApps[p.AppID]
	return approved && (p.ArchiveDatabase != "" || registered && (a.Runtime == "rails" || a.Runtime == "")) && validateNativeDatabasePolicy(id, p) == nil && sameNativeDatabasePolicy(binding.Policy, p)
}

func (m *Manager) nativeDatabaseRoute(w http.ResponseWriter, r *http.Request) {
	if err := m.adoptNativeDatabase(r.Context(), r.PathValue("resource")); err != nil {
		Fail(w, 409, err)
		return
	}
	JSON(w, 200, map[string]string{"status": "adopted"})
}

// An immutable trusted binary pin is checked both before adoption and in the
// isolated helper. No repository executable, shell or asdf loader is invoked.
func verifyNativeDumpTool(major int, expected string) error {
	file, err := openNativeDumpTool(major, expected)
	if err != nil {
		return err
	}
	return file.Close()
}

func openNativeDumpTool(major int, expected string) (*os.File, error) {
	if major < 16 || major > 18 || !nativeHexDigest(expected, 64) {
		return nil, errors.New("native pg_dump requires an approved version and binary hash")
	}
	path := filepath.Join("/usr/lib/postgresql", strconv.Itoa(major), "bin/pg_dump")
	return openNativeBackupTool(path, expected)
}

func openNativeBackupTool(path, expected string) (*os.File, error) {
	if !nativeHexDigest(expected, 64) {
		return nil, errors.New("native backup tool requires an approved binary hash")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("native backup tool is unavailable")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0022 != 0 || info.Size() > 64<<20 {
		file.Close()
		return nil, errors.New("native backup tool is unavailable or untrusted")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
		file.Close()
		return nil, errors.New("native backup tool must be owned by root")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, (64<<20)+1))
	if err != nil || n != info.Size() || hex.EncodeToString(hash.Sum(nil)) != expected {
		file.Close()
		return nil, errors.New("native backup tool changed; review its binary before backup")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
