package agent

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Host-level ClickHouse for applications the agent creates. One server per
// host, installed from the official ClickHouse apt repository only when the
// first application asks for it:
//
//   - the repository key is verified against its published fingerprint and
//     the packages are pinned to one release series (the source's when an
//     application moves here, otherwise the newest LTS series);
//   - the server listens on loopback only (127.0.0.1 and ::1; HTTP and native
//     TCP, no MySQL/PostgreSQL/interserver ports), caps its memory relative to
//     the host's RAM, rotates its logs and expires its system logs;
//   - the default user keeps only loopback access with a random password
//     that only the agent holds (sealed in its state);
//   - every application gets a database, a user and a settings profile named
//     app_<id>: the user reaches the server from loopback only, is granted
//     privileges on its own database only and runs with per-query memory and
//     execution-time limits;
//   - the database is registered as a protected database resource (engine
//     clickhouse), so scheduled and pre-release backups, local retention,
//     verified off-site copies and restores cover it. Backups use ClickHouse's
//     BACKUP DATABASE to a dedicated backups disk.
//
// The application receives the variables the managed container service
// provides, so applications do not change between runtimes: CLICKHOUSE_URL,
// CLICKHOUSE_DATABASE, CLICKHOUSE_USER and CLICKHOUSE_PASSWORD.

const (
	nativeCHConfigFile      = "/etc/clickhouse-server/config.d/lidza.xml"
	nativeCHUsersFile       = "/etc/clickhouse-server/users.d/lidza.xml"
	nativeCHDropIn          = "/etc/systemd/system/clickhouse-server.service.d/lidza.conf"
	nativeCHKeyring         = "/etc/apt/keyrings/clickhouse.asc"
	nativeCHSourceList      = "/etc/apt/sources.list.d/clickhouse.list"
	nativeCHPreferences     = "/etc/apt/preferences.d/lidza-clickhouse"
	nativeCHServerBinary    = "/usr/bin/clickhouse-server"
	nativeCHBackupDir       = "/var/lib/clickhouse/lidza-backups"
	nativeCHUnit            = "clickhouse-server.service"
	nativeCHKeyURL          = "https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key"
	nativeCHKeyFingerprint  = "3A9EA1193A97B548BE1457D48919F6BD2B48D754" // ClickHouse Inc. Repositories Key
	nativeCHDefaultHTTPPort = 8123
	nativeCHDefaultTCPPort  = 9000
	nativeCHDefaultRatio    = 0.25
	nativeCHMaxExecution    = 300 // seconds per query
	nativeCHBackupSubdir    = "agent"
)

// NativeClickHouseServer is the host server's record (diskState.NativeClickHouse).
type NativeClickHouseServer struct {
	Series    string    `json:"series,omitempty"`
	Version   string    `json:"version,omitempty"`
	HTTPPort  int       `json:"http_port"`
	TCPPort   int       `json:"tcp_port"`
	Password  string    `json:"password_sealed"` // the default (administrator) user
	Installed bool      `json:"installed"`
	Updated   time.Time `json:"updated"`
}

// NativeHostClickHouse is one application's database on the host server.
type NativeHostClickHouse struct {
	Name           string `json:"name"`     // database, user and settings profile
	Resource       string `json:"resource"` // protected database resource id
	HTTPPort       int    `json:"http_port"`
	QueryMemoryMiB int    `json:"query_memory_mib"`
	Password       string `json:"password_sealed"`
	Ready          bool   `json:"ready"`
}

func nativeClickHouseEnv(name, password string, port int) map[string]string {
	return map[string]string{"CLICKHOUSE_URL": "http://127.0.0.1:" + strconv.Itoa(port), "CLICKHOUSE_DATABASE": name, "CLICKHOUSE_USER": name, "CLICKHOUSE_PASSWORD": password}
}

// nativeClickHouseResourceURL identifies the resource; it carries no secret.
func nativeClickHouseResourceURL(name string, port int) string {
	return fmt.Sprintf("clickhouse://%s@127.0.0.1:%d/%s", name, port, name)
}

// nativeHostClickHouseDigest is the binding's configuration identity.
func nativeHostClickHouseDigest(port int, name string) string {
	sum := sha256.Sum256([]byte("lidza-host-clickhouse\x00" + strconv.Itoa(port) + "\x00" + name))
	return hex.EncodeToString(sum[:])
}

func nativeHostClickHouseResourceID(id string) string {
	base := strings.TrimRight(id[:min(len(id), 37)], "-")
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-ch%x", base, b[:])
}

func (c NativeHostDataConfig) clickhousePorts() (int, int) {
	return cmp.Or(c.ClickHouseHTTPPort, nativeCHDefaultHTTPPort), cmp.Or(c.ClickHouseTCPPort, nativeCHDefaultTCPPort)
}

// ClickHouse falls back to these ranges when something else already holds
// a default port (an application listening on 9000 is common).
const (
	nativeCHFallbackHTTPPort = 18123
	nativeCHFallbackTCPPort  = 19000
	nativeCHFallbackSpan     = 200
)

// chooseClickHousePorts picks the host server's loopback ports. Ports set in
// the configuration are used as given and refused when taken; otherwise the
// defaults are used when free, and the first free ports of the fallback
// ranges when not. taken reports a port that is listening or reserved.
func (c NativeHostDataConfig) chooseClickHousePorts(taken func(int) bool) (int, int, error) {
	pick := func(configured, preferred, fallback, other int) (int, error) {
		if configured != 0 {
			if taken(configured) {
				return 0, fmt.Errorf("the configured ClickHouse port %d is used by another process or application", configured)
			}
			return configured, nil
		}
		if preferred != other && !taken(preferred) {
			return preferred, nil
		}
		for port := fallback; port < fallback+nativeCHFallbackSpan; port++ {
			if port != other && !taken(port) {
				return port, nil
			}
		}
		return 0, fmt.Errorf("no free loopback port is left for ClickHouse near %d", fallback)
	}
	httpPort, err := pick(c.ClickHouseHTTPPort, nativeCHDefaultHTTPPort, nativeCHFallbackHTTPPort, 0)
	if err != nil {
		return 0, 0, err
	}
	tcpPort, err := pick(c.ClickHouseTCPPort, nativeCHDefaultTCPPort, nativeCHFallbackTCPPort, httpPort)
	if err != nil {
		return 0, 0, err
	}
	return httpPort, tcpPort, nil
}

// clickhousePortTaken reports whether a port is listening or reserved by an
// application of this agent.
func (m *Manager) clickhousePortTaken() func(int) bool {
	m.mu.Lock()
	reserved := m.reservedPortsLocked()
	m.mu.Unlock()
	return func(port int) bool { return reserved[strconv.Itoa(port)] || !nativePortListenable(port) }
}

func (c NativeHostDataConfig) clickhouseRatio() float64 {
	if c.ClickHouseMemoryRatio == 0 {
		return nativeCHDefaultRatio
	}
	return c.ClickHouseMemoryRatio
}

// ---- renderers --------------------------------------------------------------------

// renderNativeClickHouseConfig is config.d/lidza.xml.
func renderNativeClickHouseConfig(httpPort, tcpPort int, ratio float64, backupDir string) string {
	dir := strings.TrimRight(backupDir, "/") + "/"
	return fmt.Sprintf(`<?xml version="1.0"?>
<!-- Managed by Līdza Deploy. Changes are overwritten. -->
<clickhouse>
    <listen_host remove="1"/>
    <listen_host>127.0.0.1</listen_host>
    <listen_host>::1</listen_host>
    <http_port>%d</http_port>
    <tcp_port>%d</tcp_port>
    <mysql_port remove="1"/>
    <postgresql_port remove="1"/>
    <interserver_http_port remove="1"/>
    <max_server_memory_usage_to_ram_ratio>%s</max_server_memory_usage_to_ram_ratio>
    <cache_size_to_ram_max_ratio>%s</cache_size_to_ram_max_ratio>
    <mark_cache_size>268435456</mark_cache_size>
    <send_crash_reports><enabled>false</enabled></send_crash_reports>
    <logger>
        <level>warning</level>
        <size>100M</size>
        <count>5</count>
    </logger>
%s    <storage_configuration>
        <disks>
            <backups>
                <type>local</type>
                <path>%s</path>
            </backups>
        </disks>
    </storage_configuration>
    <backups>
        <allowed_disk>backups</allowed_disk>
        <allowed_path>%s</allowed_path>
        <remove_backup_files_after_failure>true</remove_backup_files_after_failure>
    </backups>
</clickhouse>
`, httpPort, tcpPort, strconv.FormatFloat(ratio, 'f', -1, 64), strconv.FormatFloat(ratio/2, 'f', -1, 64), renderClickHouseSystemLogs(), dir, dir)
}

// renderNativeClickHouseUsers is users.d/lidza.xml: the default user keeps
// loopback access only, with the agent's random password (as a SHA-256).
func renderNativeClickHouseUsers(adminPassword string) string {
	sum := sha256.Sum256([]byte(adminPassword))
	return fmt.Sprintf(`<?xml version="1.0"?>
<!-- Managed by Līdza Deploy. Changes are overwritten. -->
<clickhouse>
    <users>
        <default>
            <password remove="1"/>
            <password_sha256_hex>%s</password_sha256_hex>
            <networks replace="replace">
                <ip>127.0.0.1</ip>
                <ip>::1</ip>
            </networks>
            <access_management>1</access_management>
        </default>
    </users>
</clickhouse>
`, hex.EncodeToString(sum[:]))
}

// renderNativeClickHouseDropIn caps the service's memory above ClickHouse's
// own limit and hardens the packaged unit: no outbound network beyond
// loopback, read-only system directories and no home directories.
func renderNativeClickHouseDropIn(memoryMaxMiB int) string {
	return fmt.Sprintf(`# Managed by Līdza Deploy.
[Service]
MemoryMax=%dM
MemorySwapMax=0
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
PrivateTmp=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
IPAddressDeny=any
IPAddressAllow=localhost
`, memoryMaxMiB)
}

// renderNativeClickHousePreferences pins the packages to one release series.
func renderNativeClickHousePreferences(series string) string {
	return fmt.Sprintf(`# Managed by Līdza Deploy: keep ClickHouse on the %s release series.
Package: clickhouse-server clickhouse-client clickhouse-common-static
Pin: version %s.*
Pin-Priority: 1001
`, series, series)
}

func nativeClickHouseRepositoryLine(arch string) (string, error) {
	if arch == "" || strings.ContainsAny(arch, " \t\n/]") {
		return "", errors.New("the package architecture could not be determined")
	}
	return "deb [signed-by=" + nativeCHKeyring + " arch=" + arch + "] https://packages.clickhouse.com/deb stable main\n", nil
}

// clickhouseGrants are the application user's privileges, on its own
// database only. CREATE DATABASE on it lets migrations run
// "CREATE DATABASE IF NOT EXISTS" for their own name.
const clickhouseGrants = "SELECT, INSERT, ALTER, CREATE DATABASE, CREATE TABLE, CREATE VIEW, CREATE DICTIONARY, DROP TABLE, DROP VIEW, DROP DICTIONARY, TRUNCATE, OPTIMIZE, SHOW TABLES, SHOW COLUMNS, SHOW DICTIONARIES, dictGet"

var nativeCHSaltPattern = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// renderNativeClickHouseProvisionSQL creates or repairs one application's
// database, settings profile and user, one statement per element (the HTTP
// interface runs one statement per request). The password is sent as a
// salted SHA-256, never in plain text.
func renderNativeClickHouseProvisionSQL(name, passwordHash, salt string, queryMemoryBytes int64) ([]string, error) {
	if !clickhouseNamePattern.MatchString(name) || !nativeHexDigest(passwordHash, 64) || !nativeCHSaltPattern.MatchString(salt) || queryMemoryBytes < 64<<20 {
		return nil, errors.New("invalid ClickHouse provisioning input")
	}
	ident, literal := mustCHIdent(name), mustCHString(name)
	identified := fmt.Sprintf("IDENTIFIED WITH sha256_hash BY '%s' SALT '%s' HOST IP '127.0.0.1', IP '::1' DEFAULT DATABASE %s SETTINGS PROFILE %s", passwordHash, salt, ident, literal)
	return []string{
		"CREATE DATABASE IF NOT EXISTS " + ident + " ENGINE = Atomic",
		"CREATE SETTINGS PROFILE IF NOT EXISTS " + ident,
		fmt.Sprintf("ALTER SETTINGS PROFILE %s SETTINGS max_memory_usage = %d, max_execution_time = %d, max_concurrent_queries_for_user = 32", ident, queryMemoryBytes, nativeCHMaxExecution),
		"CREATE USER IF NOT EXISTS " + ident + " " + identified,
		"ALTER USER " + ident + " " + identified,
		"REVOKE ALL ON *.* FROM " + ident,
		"GRANT " + clickhouseGrants + " ON " + ident + ".* TO " + ident,
	}, nil
}

// renderNativeClickHouseReleaseSQL removes the user first, so no new session
// starts, then its queries, profile and database. Every statement tolerates
// an already removed object.
func renderNativeClickHouseReleaseSQL(name string) ([]string, error) {
	if !clickhouseNamePattern.MatchString(name) {
		return nil, errors.New("invalid ClickHouse name")
	}
	ident := mustCHIdent(name)
	return []string{
		"DROP USER IF EXISTS " + ident,
		"KILL QUERY WHERE user = " + mustCHString(name) + " ASYNC",
		"DROP SETTINGS PROFILE IF EXISTS " + ident,
		"DROP DATABASE IF EXISTS " + ident + " SYNC",
	}, nil
}

// chooseClickHouseSeries picks the release series to install from
// `apt-cache madison clickhouse-server` output: the requested one, or the
// newest LTS series (x.3 and x.8 releases), or the newest series at all.
func chooseClickHouseSeries(madison, requested string) (string, error) {
	available := map[string]bool{}
	for _, line := range strings.Split(madison, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 2 {
			continue
		}
		if version := strings.TrimSpace(fields[1]); clickhouseVersionPattern.MatchString(version) {
			available[clickhouseSeries(version)] = true
		}
	}
	if requested != "" {
		if !available[requested] {
			return "", fmt.Errorf("ClickHouse %s is not available from the ClickHouse repository", requested)
		}
		return requested, nil
	}
	best, bestLTS := "", ""
	for series := range available {
		if best == "" || compareClickHouseSeries(series, best) > 0 {
			best = series
		}
		if minor := series[strings.Index(series, ".")+1:]; (minor == "3" || minor == "8") && (bestLTS == "" || compareClickHouseSeries(series, bestLTS) > 0) {
			bestLTS = series
		}
	}
	if best == "" {
		return "", errors.New("the ClickHouse repository lists no server release")
	}
	return cmp.Or(bestLTS, best), nil
}

// ---- host operations --------------------------------------------------------------

// clickhouse returns a client for the host server as user.
func (h nativeHostData) clickhouse(server NativeClickHouseServer, user, password string) clickhouseClient {
	base := h.clickhouseURL
	if base == "" {
		base = "http://127.0.0.1:" + strconv.Itoa(server.HTTPPort)
	}
	return clickhouseClient{base: base, user: user, password: password}
}

// memoryMiB reads the host's total memory.
func (h nativeHostData) memoryMiB() int {
	file, err := os.Open(h.path("/proc/meminfo"))
	if err != nil {
		return 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, _ := strconv.Atoi(fields[1])
			return kib / 1024
		}
	}
	return 0
}

// clickhouseMemory is the server's own cap, its unit's hard limit and each
// application's per-query limit, in MiB.
func (h nativeHostData) clickhouseMemory() (serverMiB, unitMiB, queryMiB int) {
	total := h.memoryMiB()
	if total <= 0 {
		total = 4096
	}
	serverMiB = max(int(float64(total)*h.cfg.clickhouseRatio()), 512)
	unitMiB = serverMiB + max(serverMiB/4, 256)
	queryMiB = max(serverMiB/2, 256)
	return serverMiB, unitMiB, queryMiB
}

func (h nativeHostData) clickhouseGroup() (int, error) {
	if !h.chown {
		return 0, nil
	}
	group, err := user.LookupGroup("clickhouse")
	if err != nil {
		return 0, errors.New("the clickhouse group does not exist after installation")
	}
	return strconv.Atoi(group.Gid)
}

// writeClickHouseFiles writes the managed configuration. Before the packages
// exist (preinstall) the files are root-only, so a server the package might
// start cannot run with a passwordless default user.
func (h nativeHostData) writeClickHouseFiles(server NativeClickHouseServer, adminPassword string, preinstall bool) (bool, error) {
	_, unitMiB, _ := h.clickhouseMemory()
	mode, gid := os.FileMode(0o600), 0
	if !preinstall {
		var err error
		if gid, err = h.clickhouseGroup(); err != nil {
			return false, err
		}
		mode = 0o640
	}
	changed := false
	for _, file := range []struct {
		path string
		data string
	}{
		{nativeCHConfigFile, renderNativeClickHouseConfig(server.HTTPPort, server.TCPPort, h.cfg.clickhouseRatio(), h.path(nativeCHBackupDir))},
		{nativeCHUsersFile, renderNativeClickHouseUsers(adminPassword)},
	} {
		written, err := h.writeIfChanged(file.path, []byte(file.data), mode, 0, gid)
		if err != nil {
			return false, err
		}
		changed = changed || written
	}
	written, err := h.writeIfChanged(nativeCHDropIn, []byte(renderNativeClickHouseDropIn(unitMiB)), 0o644, 0, 0)
	return changed || written, err
}

// installClickHouse adds the verified repository, pins the series and
// installs the server and client. It returns the installed series.
func (h nativeHostData) installClickHouse(ctx context.Context, series string, server NativeClickHouseServer, adminPassword string) (string, error) {
	if !h.fileExists("/usr/bin/gpg") {
		if out, err := h.aptInstall(ctx, "gnupg", "ca-certificates"); err != nil {
			if _, updateErr := h.run(ctx, "", "/usr/bin/apt-get", "-o", "DPkg::Lock::Timeout=300", "update", "-q"); updateErr != nil {
				return "", fmt.Errorf("gnupg installation failed: %s", nativeTrimOutput(out))
			}
			if out, err = h.aptInstall(ctx, "gnupg", "ca-certificates"); err != nil {
				return "", fmt.Errorf("gnupg installation failed: %s", nativeTrimOutput(out))
			}
		}
	}
	download := h.download
	if download == nil {
		download = downloadSigningKey
	}
	key, err := download(ctx, nativeCHKeyURL)
	if err != nil {
		return "", err
	}
	staged := nativeCHKeyring + ".download"
	if _, err := h.writeIfChanged(staged, key, 0o644, 0, 0); err != nil {
		return "", err
	}
	defer os.Remove(h.path(staged))
	out, err := h.run(ctx, "", "/usr/bin/gpg", "--batch", "--no-default-keyring", "--show-keys", "--with-colons", "--with-fingerprint", h.path(staged))
	if err != nil {
		return "", errors.New("the ClickHouse signing key could not be read with gpg")
	}
	if fingerprints := parseGPGFingerprints(out); len(fingerprints) != 1 || !strings.EqualFold(fingerprints[0], nativeCHKeyFingerprint) {
		return "", fmt.Errorf("the ClickHouse signing key fingerprint %v does not match the published %s; nothing was installed", fingerprints, nativeCHKeyFingerprint)
	}
	arch, err := h.run(ctx, "", "/usr/bin/dpkg", "--print-architecture")
	if err != nil {
		return "", errors.New("the package architecture could not be determined")
	}
	line, err := nativeClickHouseRepositoryLine(strings.TrimSpace(arch))
	if err != nil {
		return "", err
	}
	if _, err := h.writeIfChanged(nativeCHKeyring, key, 0o644, 0, 0); err != nil {
		return "", err
	}
	if _, err := h.writeIfChanged(nativeCHSourceList, []byte(line), 0o644, 0, 0); err != nil {
		return "", err
	}
	if out, err := h.run(ctx, "", "/usr/bin/apt-get", "-o", "DPkg::Lock::Timeout=300", "update", "-q"); err != nil {
		return "", fmt.Errorf("package index update failed: %s", nativeTrimOutput(out))
	}
	madison, err := h.run(ctx, "", "/usr/bin/apt-cache", "madison", "clickhouse-server")
	if err != nil {
		return "", errors.New("the ClickHouse repository could not be read")
	}
	if series, err = chooseClickHouseSeries(madison, series); err != nil {
		return "", err
	}
	if _, err := h.writeIfChanged(nativeCHPreferences, []byte(renderNativeClickHousePreferences(series)), 0o644, 0, 0); err != nil {
		return "", err
	}
	if _, err := h.writeClickHouseFiles(server, adminPassword, true); err != nil {
		return "", err
	}
	if out, err := h.aptInstall(ctx, "clickhouse-server", "clickhouse-client"); err != nil {
		return "", fmt.Errorf("ClickHouse %s installation failed: %s", series, nativeTrimOutput(out))
	}
	return series, nil
}

func (m *Manager) saveClickHouseServer(server NativeClickHouseServer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.data.NativeClickHouse
	server.Updated = time.Now().UTC()
	m.data.NativeClickHouse = &server
	if err := m.save(); err != nil {
		m.data.NativeClickHouse = previous
		return err
	}
	return nil
}

// ensureHostClickHouse installs, configures and starts the host server and
// returns it with the administrator password. A ClickHouse the agent did not
// install is never taken over.
func (m *Manager) ensureHostClickHouse(ctx context.Context, h nativeHostData, series string) (NativeClickHouseServer, string, error) {
	m.mu.Lock()
	var server NativeClickHouseServer
	known := m.data.NativeClickHouse != nil
	if known {
		server = *m.data.NativeClickHouse
	}
	m.mu.Unlock()
	installed := h.fileExists(nativeCHServerBinary)
	if !known {
		if installed {
			return server, "", errors.New("ClickHouse is already installed on this host and is not managed by Līdza Deploy; it is left unchanged")
		}
		sealed, err := m.sealHostSecret(nativeRandomSecret())
		if err != nil {
			return server, "", err
		}
		server.HTTPPort, server.TCPPort, err = h.cfg.chooseClickHousePorts(m.clickhousePortTaken())
		if err != nil {
			return server, "", err
		}
		server.Password = sealed
		// Recorded before anything is installed: a retry continues with the
		// same administrator password.
		if err := m.saveClickHouseServer(server); err != nil {
			return server, "", err
		}
	}
	if known && server.Version == "" {
		// The server never became ready, so it holds no data yet: if another
		// process took one of its ports, it moves to free ones instead of
		// failing to start forever.
		if err := m.reassignClickHousePorts(ctx, h, &server); err != nil {
			return server, "", err
		}
	}
	admin, err := m.unsealHostSecret(server.Password)
	if err != nil {
		return server, "", err
	}
	if !installed {
		chosen, err := h.installClickHouse(ctx, cmp.Or(series, h.cfg.ClickHouseVersion), server, admin)
		if err != nil {
			return server, "", err
		}
		server.Series, server.Installed = chosen, true
		if err := m.saveClickHouseServer(server); err != nil {
			return server, "", err
		}
	}
	changed, err := h.writeClickHouseFiles(server, admin, false)
	if err != nil {
		return server, "", err
	}
	if out, err := h.run(ctx, "", "/usr/bin/systemctl", "daemon-reload"); err != nil {
		return server, "", fmt.Errorf("systemd reload failed: %s", nativeTrimOutput(out))
	}
	if out, err := h.run(ctx, "", "/usr/bin/systemctl", "enable", nativeCHUnit); err != nil {
		return server, "", fmt.Errorf("enabling ClickHouse failed: %s", nativeTrimOutput(out))
	}
	action := "start"
	if changed {
		action = "restart"
	}
	if out, err := h.run(ctx, "", "/usr/bin/systemctl", action, nativeCHUnit); err != nil {
		return server, "", fmt.Errorf("starting ClickHouse failed: %s", nativeTrimOutput(out))
	}
	version, err := h.clickhouse(server, "default", admin).waitReady(ctx, 2*time.Minute)
	if err != nil {
		return server, "", err
	}
	server.Version, server.Series, server.Installed = version, clickhouseSeries(version), true
	if err := m.saveClickHouseServer(server); err != nil {
		return server, "", err
	}
	return server, admin, nil
}

// reassignClickHousePorts moves a server that never started off ports that
// another process holds. The unit is stopped first, so its own restart
// attempts do not count as another process.
func (m *Manager) reassignClickHousePorts(ctx context.Context, h nativeHostData, server *NativeClickHouseServer) error {
	if server.Installed {
		_, _ = h.run(ctx, "", "/usr/bin/systemctl", "stop", nativeCHUnit)
	}
	taken := m.clickhousePortTaken()
	if !taken(server.HTTPPort) && !taken(server.TCPPort) {
		return nil
	}
	httpPort, tcpPort, err := h.cfg.chooseClickHousePorts(taken)
	if err != nil {
		return err
	}
	server.HTTPPort, server.TCPPort = httpPort, tcpPort
	return m.saveClickHouseServer(*server)
}

// provisionNativeClickHouse creates or repairs the application's database,
// profile and user and registers the protected resource. Re-provisioning
// keeps the password and resource.
func (m *Manager) provisionNativeClickHouse(ctx context.Context, h nativeHostData, id string, backup BackupPolicy, series string) (map[string]string, string, error) {
	name, err := nativePGName(id)
	if err != nil {
		return nil, "", err
	}
	if series != "" && !clickhouseSeriesPattern.MatchString(series) {
		return nil, "", errors.New("the ClickHouse version must be a release series such as 26.8")
	}
	if backup.Offsite {
		m.mu.Lock()
		configured := m.data.BackupStorage != nil
		m.mu.Unlock()
		if !configured {
			return nil, "", errors.New("configure off-site backup storage before requesting off-site database backups")
		}
	}
	record, _ := m.hostDataRecord(id)
	password, resource := nativeRandomSecret(), nativeHostClickHouseResourceID(id)
	if existing := record.ClickHouse; existing != nil {
		if password, err = m.unsealHostSecret(existing.Password); err != nil {
			return nil, "", err
		}
		resource = existing.Resource
	}
	server, admin, err := m.ensureHostClickHouse(ctx, h, series)
	if err != nil {
		return nil, "", err
	}
	_, _, queryMiB := h.clickhouseMemory()
	sealed, err := m.sealHostSecret(password)
	if err != nil {
		return nil, "", err
	}
	ch := NativeHostClickHouse{Name: name, Resource: resource, HTTPPort: server.HTTPPort, QueryMemoryMiB: queryMiB, Password: sealed, Ready: record.ClickHouse != nil && record.ClickHouse.Ready}
	if err = m.updateHostDataRecord(id, func(r *NativeHostDataRecord) { r.ClickHouse = &ch }); err != nil {
		return nil, "", err
	}
	var salt [16]byte
	_, _ = rand.Read(salt[:])
	saltHex := hex.EncodeToString(salt[:])
	sum := sha256.Sum256([]byte(password + saltHex))
	statements, err := renderNativeClickHouseProvisionSQL(name, hex.EncodeToString(sum[:]), saltHex, int64(queryMiB)<<20)
	if err != nil {
		return nil, "", err
	}
	client := h.clickhouse(server, "default", admin)
	for _, statement := range statements {
		if err := client.exec(ctx, statement); err != nil {
			return nil, "", fmt.Errorf("ClickHouse database/user provisioning failed: %w", err)
		}
	}
	// Prove the application's own credential before handing it out.
	who, err := h.clickhouse(server, name, password).query(ctx, "SELECT currentUser(), currentDatabase()")
	if err != nil || who != name+"\t"+name {
		return nil, "", errors.New("the application's ClickHouse user could not sign in")
	}
	raw := nativeClickHouseResourceURL(name, server.HTTPPort)
	policy := NativeDatabasePolicy{Enabled: true, AppID: id, EnvironmentKey: "CLICKHOUSE_URL", Roles: []string{"analytics"}, Host: "127.0.0.1", Port: strconv.Itoa(server.HTTPPort),
		ConfigSHA256: nativeHostClickHouseDigest(server.HTTPPort, name), ClickHouseDatabase: name, Backup: backup}
	if err = validateNativeDatabasePolicy(resource, policy); err != nil {
		return nil, "", err
	}
	if err = m.registerHostResource(id, resource, raw, policy, func(r *NativeHostDataRecord) bool {
		if r.ClickHouse == nil || r.ClickHouse.Resource != resource {
			return false
		}
		next := *r.ClickHouse
		next.Ready = true
		r.ClickHouse = &next
		return true
	}); err != nil {
		return nil, "", err
	}
	return nativeClickHouseEnv(name, password, server.HTTPPort), resource, nil
}

// releaseNativeClickHouse takes a final backup (unless the application was
// never released), then removes the user, profile and database. The server
// stops when no application uses it any more; its packages and data
// directory stay.
func (m *Manager) releaseNativeClickHouse(ctx context.Context, h nativeHostData, id string, ch NativeHostClickHouse, finalBackup bool) error {
	m.mu.Lock()
	d, exists := m.data.Databases[ch.Resource]
	binding, bound := m.data.NativeDatabases[ch.Resource]
	server := m.data.NativeClickHouse
	m.mu.Unlock()
	if finalBackup && exists && bound && binding.Policy.AppID == id && d.Ready && ch.Ready && !m.nativeCreatedNeverReleased(id) {
		if err := m.freshBackupBeforeRelease(ctx, []string{ch.Resource}); err != nil {
			return fmt.Errorf("final ClickHouse backup failed; the database is retained: %w", err)
		}
	}
	if err := m.unbindHostDatabase(id, ch.Resource); err != nil {
		return err
	}
	if server != nil && server.Installed {
		admin, err := m.unsealHostSecret(server.Password)
		if err != nil {
			return err
		}
		statements, err := renderNativeClickHouseReleaseSQL(ch.Name)
		if err != nil {
			return err
		}
		client := h.clickhouse(*server, "default", admin)
		for _, statement := range statements {
			if err := client.exec(ctx, statement); err != nil {
				return fmt.Errorf("removing the application's ClickHouse database failed; retry the removal: %w", err)
			}
		}
	}
	if err := m.updateHostDataRecord(id, func(r *NativeHostDataRecord) { r.ClickHouse = nil }); err != nil {
		return err
	}
	m.mu.Lock()
	inUse := false
	for _, record := range m.data.NativeHostData {
		inUse = inUse || record.ClickHouse != nil
	}
	m.mu.Unlock()
	if !inUse && server != nil && server.Installed {
		if out, err := h.run(ctx, "", "/usr/bin/systemctl", "disable", "--now", nativeCHUnit); err != nil {
			return fmt.Errorf("stopping the unused ClickHouse server failed: %s", nativeTrimOutput(out))
		}
	}
	return nil
}

// ---- backups -----------------------------------------------------------------------

// hostBackupFiles are BACKUP files on the server's backups disk.
// hostBackupFiles are BACKUP files on the host server's backups disk. Files
// the agent writes there belong to the server's user: the agent runs with a
// private umask, and a root-only file cannot be restored. The data directory
// itself is private to that user.
type hostBackupFiles struct {
	dir      string
	uid, gid int
	chown    bool
}

func (f hostBackupFiles) ref(name string) string {
	return "Disk('backups', '" + nativeCHBackupSubdir + "/" + name + "')"
}

func (f hostBackupFiles) path(name string) (string, error) {
	if !clickhouseBackupFileName.MatchString(name) {
		return "", errors.New("invalid ClickHouse backup file name")
	}
	return filepath.Join(f.dir, nativeCHBackupSubdir, name), nil
}

func (f hostBackupFiles) open(_ context.Context, name string) (io.ReadCloser, int64, error) {
	path, err := f.path(name)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, errors.New("the ClickHouse backup file is missing")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		file.Close()
		return nil, 0, errors.New("the ClickHouse backup file is invalid")
	}
	return file, info.Size(), nil
}

func (f hostBackupFiles) create(_ context.Context, name string, data io.Reader, size int64) error {
	path, err := f.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return errors.New("the ClickHouse backups disk is unavailable")
	}
	_ = os.Chmod(filepath.Dir(path), 0o750)
	if f.chown {
		if err := os.Chown(filepath.Dir(path), f.uid, f.gid); err != nil {
			return errors.New("the ClickHouse backups disk could not be handed to the server")
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return errors.New("the ClickHouse backup file could not be created")
	}
	if err := file.Chmod(0o640); err == nil && f.chown {
		err = file.Chown(f.uid, f.gid)
		if err != nil {
			file.Close()
			os.Remove(path)
			return errors.New("the ClickHouse backup file could not be handed to the server")
		}
	}
	n, err := io.Copy(file, data)
	if err == nil && n != size {
		err = errors.New("the ClickHouse backup file is incomplete")
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

func (f hostBackupFiles) remove(_ context.Context, name string) error {
	path, err := f.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// hostClickHouseService is the application's database on the host server,
// operated as the administrator (BACKUP and RESTORE need it).
func (m *Manager) hostClickHouseService(id string) (clickhouseService, NativeHostClickHouse, error) {
	m.mu.Lock()
	record, ok := m.data.NativeHostData[id]
	server := m.data.NativeClickHouse
	m.mu.Unlock()
	if !ok || record.ClickHouse == nil || server == nil || !server.Installed {
		return clickhouseService{}, NativeHostClickHouse{}, errors.New("no ClickHouse database is provisioned for this application")
	}
	admin, err := m.unsealHostSecret(server.Password)
	if err != nil {
		return clickhouseService{}, NativeHostClickHouse{}, err
	}
	h := nativeHostDataHost(m)
	files, err := h.clickhouseBackupFiles()
	if err != nil {
		return clickhouseService{}, NativeHostClickHouse{}, err
	}
	return clickhouseService{mode: "host", database: record.ClickHouse.Name, client: h.clickhouse(*server, "default", admin), files: files}, *record.ClickHouse, nil
}

// clickhouseBackupFiles is the host server's backups disk, with files owned by
// the server's user on a real host.
func (h nativeHostData) clickhouseBackupFiles() (hostBackupFiles, error) {
	files := hostBackupFiles{dir: h.path(nativeCHBackupDir)}
	if !h.chown {
		return files, nil
	}
	account, err := user.Lookup("clickhouse")
	if err != nil {
		return files, errors.New("the clickhouse user does not exist on this host")
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return files, err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return files, err
	}
	files.uid, files.gid, files.chown = uid, gid, true
	return files, nil
}

// clickhouseResourceApp returns the application whose ClickHouse database is
// the resource.
func (m *Manager) clickhouseResourceApp(resource string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, record := range m.data.NativeHostData {
		if record.ClickHouse != nil && record.ClickHouse.Resource == resource {
			return id, true
		}
	}
	return "", false
}

// dumpHostClickHouse writes a backup archive of an agent-provisioned
// ClickHouse database (the backup pipeline's dump step).
func (m *Manager) dumpHostClickHouse(ctx context.Context, d Database, p NativeDatabasePolicy, output io.Writer) error {
	app, ok := m.clickhouseResourceApp(d.AppID)
	if !ok || app != p.AppID {
		return errors.New("the ClickHouse database binding changed; backup refused")
	}
	service, _, err := m.hostClickHouseService(app)
	if err != nil {
		return err
	}
	if service.database != p.ClickHouseDatabase {
		return errors.New("the ClickHouse database binding changed; backup refused")
	}
	// A directory BACKUP streamed as one tar (Zstandard when the tool is
	// installed): it restores in seconds where a .zip archive of many
	// parts takes hours. Older .zip backups keep restoring
	// (clickhouse_archive.go).
	spool := m.backupDir(d.AppID)
	compression := compressionNone
	if zstdAvailable() {
		compression = compressionZstd
	}
	compressing, cancel := context.WithCancel(ctx)
	defer cancel()
	writer, err := compressedWriter(compressing, output, compression)
	if err != nil {
		return err
	}
	_, err = exportClickHouseArchive(ctx, service, clickhouseFormatDirectory, spool, writer)
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	return err
}

// hostClickHouseSize is the database's size from system.parts.
func (m *Manager) hostClickHouseSize(ctx context.Context, resource string) (int64, error) {
	app, ok := m.clickhouseResourceApp(resource)
	if !ok {
		return 0, errors.New("unknown ClickHouse database")
	}
	service, _, err := m.hostClickHouseService(app)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	summary, err := service.summary(ctx)
	return summary.SizeBytes, err
}

// ---- view -------------------------------------------------------------------------

type NativeHostClickHouseView struct {
	Engine         string       `json:"engine"` // clickhouse
	Version        string       `json:"version,omitempty"`
	Series         string       `json:"series,omitempty"`
	HTTPPort       int          `json:"http_port"`
	Database       string       `json:"database"`
	User           string       `json:"user"`
	Resource       string       `json:"resource"`
	Ready          bool         `json:"ready"`
	QueryMemoryMiB int          `json:"query_memory_mib"`
	SizeBytes      int64        `json:"size_bytes,omitempty"`
	Tables         int          `json:"tables"`
	Rows           uint64       `json:"rows"`
	Backup         BackupPolicy `json:"backup"`
	NextBackup     time.Time    `json:"next_backup,omitzero"`
	LastBackup     time.Time    `json:"last_backup,omitzero"`
	Backups        int          `json:"backups"`
	BackupError    string       `json:"backup_error,omitempty"`
	MeasureError   string       `json:"measure_error,omitempty"`
	BackupCovered  bool         `json:"backup_covered"`
}

func (m *Manager) nativeHostClickHouseView(ctx context.Context, id string, ch NativeHostClickHouse) *NativeHostClickHouseView {
	v := &NativeHostClickHouseView{Engine: "clickhouse", HTTPPort: ch.HTTPPort, Database: ch.Name, User: ch.Name, Resource: ch.Resource, Ready: ch.Ready, QueryMemoryMiB: ch.QueryMemoryMiB}
	m.mu.Lock()
	if server := m.data.NativeClickHouse; server != nil {
		v.Version, v.Series = server.Version, server.Series
	}
	d, exists := m.data.Databases[ch.Resource]
	v.BackupCovered = m.nativeDatabaseInUseLocked(ch.Resource)
	m.mu.Unlock()
	if exists {
		v.Backup, v.NextBackup, v.Backups, v.BackupError = d.Backup, d.NextBackup, len(d.Backups), d.Error
		for _, b := range d.Backups {
			if b.Created.After(v.LastBackup) {
				v.LastBackup = b.Created
			}
		}
	}
	if ch.Ready {
		if service, _, err := m.hostClickHouseService(id); err == nil {
			measure, cancel := context.WithTimeout(ctx, 5*time.Second)
			summary, err := service.summary(measure)
			cancel()
			if err == nil {
				v.SizeBytes, v.Tables, v.Rows, v.Version = summary.SizeBytes, summary.Tables, summary.Rows, summary.Version
			} else {
				v.MeasureError = "ClickHouse is unavailable"
			}
		}
	}
	return v
}
