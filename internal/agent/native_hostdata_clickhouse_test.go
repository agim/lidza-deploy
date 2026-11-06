package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativeClickHouseConfigGolden(t *testing.T) {
	got := renderNativeClickHouseConfig(8123, 9000, 0.25, "/var/lib/clickhouse/lidza-backups")
	want := `<?xml version="1.0"?>
<!-- Managed by Līdza Deploy. Changes are overwritten. -->
<clickhouse>
    <listen_host remove="1"/>
    <listen_host>127.0.0.1</listen_host>
    <listen_host>::1</listen_host>
    <http_port>8123</http_port>
    <tcp_port>9000</tcp_port>
    <mysql_port remove="1"/>
    <postgresql_port remove="1"/>
    <interserver_http_port remove="1"/>
    <max_server_memory_usage_to_ram_ratio>0.25</max_server_memory_usage_to_ram_ratio>
    <cache_size_to_ram_max_ratio>0.125</cache_size_to_ram_max_ratio>
    <mark_cache_size>268435456</mark_cache_size>
    <send_crash_reports><enabled>false</enabled></send_crash_reports>
    <logger>
        <level>warning</level>
        <size>100M</size>
        <count>5</count>
    </logger>
    <query_log><ttl>event_date + INTERVAL 14 DAY DELETE</ttl></query_log>
    <query_thread_log><ttl>event_date + INTERVAL 14 DAY DELETE</ttl></query_thread_log>
    <query_views_log><ttl>event_date + INTERVAL 14 DAY DELETE</ttl></query_views_log>
    <query_metric_log><ttl>event_date + INTERVAL 14 DAY DELETE</ttl></query_metric_log>
    <part_log><ttl>event_date + INTERVAL 14 DAY DELETE</ttl></part_log>
    <text_log><level>warning</level><ttl>event_date + INTERVAL 14 DAY DELETE</ttl></text_log>
    <metric_log><ttl>event_date + INTERVAL 14 DAY DELETE</ttl></metric_log>
    <asynchronous_metric_log><ttl>event_date + INTERVAL 14 DAY DELETE</ttl></asynchronous_metric_log>
    <processors_profile_log><ttl>event_date + INTERVAL 14 DAY DELETE</ttl></processors_profile_log>
    <latency_log><ttl>event_date + INTERVAL 14 DAY DELETE</ttl></latency_log>
    <error_log><ttl>event_date + INTERVAL 14 DAY DELETE</ttl></error_log>
    <trace_log><ttl>event_date + INTERVAL 3 DAY DELETE</ttl></trace_log>
    <storage_configuration>
        <disks>
            <backups>
                <type>local</type>
                <path>/var/lib/clickhouse/lidza-backups/</path>
            </backups>
        </disks>
    </storage_configuration>
    <backups>
        <allowed_disk>backups</allowed_disk>
        <allowed_path>/var/lib/clickhouse/lidza-backups/</allowed_path>
        <remove_backup_files_after_failure>true</remove_backup_files_after_failure>
    </backups>
</clickhouse>
`
	if got != want {
		t.Fatalf("config:\n%s\nwant:\n%s", got, want)
	}
	users := renderNativeClickHouseUsers("admin-secret")
	sum := sha256.Sum256([]byte("admin-secret"))
	for _, part := range []string{`<password remove="1"/>`, "<password_sha256_hex>" + hex.EncodeToString(sum[:]) + "</password_sha256_hex>", `<networks replace="replace">`, "<ip>127.0.0.1</ip>", "<ip>::1</ip>", "<access_management>1</access_management>"} {
		if !strings.Contains(users, part) {
			t.Fatalf("users.d misses %s:\n%s", part, users)
		}
	}
	if strings.Contains(users, "admin-secret") || strings.Contains(users, "::/0") {
		t.Fatal("users.d holds the plain password or a network-wide grant")
	}
	dropIn := renderNativeClickHouseDropIn(1280)
	for _, part := range []string{"MemoryMax=1280M\n", "IPAddressDeny=any\n", "IPAddressAllow=localhost\n", "ProtectSystem=full\n", "NoNewPrivileges=yes\n"} {
		if !strings.Contains(dropIn, part) {
			t.Fatalf("drop-in misses %s", part)
		}
	}
	if got := renderNativeClickHousePreferences("26.8"); got != "# Managed by Līdza Deploy: keep ClickHouse on the 26.8 release series.\nPackage: clickhouse-server clickhouse-client clickhouse-common-static\nPin: version 26.8.*\nPin-Priority: 1001\n" {
		t.Fatalf("preferences %q", got)
	}
	if line, err := nativeClickHouseRepositoryLine("arm64"); err != nil || line != "deb [signed-by=/etc/apt/keyrings/clickhouse.asc arch=arm64] https://packages.clickhouse.com/deb stable main\n" {
		t.Fatalf("repository %q %v", line, err)
	}
	if _, err := nativeClickHouseRepositoryLine("amd64] evil"); err == nil {
		t.Fatal("architecture injection accepted")
	}
}

func TestNativeClickHouseSQLGolden(t *testing.T) {
	hash, salt := strings.Repeat("ab", 32), "0123456789abcdef0123456789abcdef"
	got, err := renderNativeClickHouseProvisionSQL("app_shop_web", hash, salt, 512<<20)
	if err != nil {
		t.Fatal(err)
	}
	identified := "IDENTIFIED WITH sha256_hash BY '" + hash + "' SALT '" + salt + "' HOST IP '127.0.0.1', IP '::1' DEFAULT DATABASE `app_shop_web` SETTINGS PROFILE 'app_shop_web'"
	want := []string{
		"CREATE DATABASE IF NOT EXISTS `app_shop_web` ENGINE = Atomic",
		"CREATE SETTINGS PROFILE IF NOT EXISTS `app_shop_web`",
		"ALTER SETTINGS PROFILE `app_shop_web` SETTINGS max_memory_usage = 536870912, max_execution_time = 300, max_concurrent_queries_for_user = 32",
		"CREATE USER IF NOT EXISTS `app_shop_web` " + identified,
		"ALTER USER `app_shop_web` " + identified,
		"REVOKE ALL ON *.* FROM `app_shop_web`",
		"GRANT SELECT, INSERT, ALTER, CREATE DATABASE, CREATE TABLE, CREATE VIEW, CREATE DICTIONARY, DROP TABLE, DROP VIEW, DROP DICTIONARY, TRUNCATE, OPTIMIZE, SHOW TABLES, SHOW COLUMNS, SHOW DICTIONARIES, dictGet ON `app_shop_web`.* TO `app_shop_web`",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("provision SQL:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, statement := range got {
		if strings.Contains(statement, "GRANT") && !strings.Contains(statement, "ON `app_shop_web`.* TO") {
			t.Fatalf("grant beyond the application's database: %s", statement)
		}
	}
	for _, bad := range [][4]any{{"app`x", hash, salt, int64(512 << 20)}, {"app_x", "plain", salt, int64(512 << 20)}, {"app_x", hash, "'; DROP", int64(512 << 20)}, {"app_x", hash, salt, int64(1)}} {
		if _, err := renderNativeClickHouseProvisionSQL(bad[0].(string), bad[1].(string), bad[2].(string), bad[3].(int64)); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	release, err := renderNativeClickHouseReleaseSQL("app_shop_web")
	if err != nil || !slices.Equal(release, []string{"DROP USER IF EXISTS `app_shop_web`", "KILL QUERY WHERE user = 'app_shop_web' ASYNC", "DROP SETTINGS PROFILE IF EXISTS `app_shop_web`", "DROP DATABASE IF EXISTS `app_shop_web` SYNC"}) {
		t.Fatalf("release SQL %v %v", release, err)
	}
}

func TestClickHouseSeriesAndFormats(t *testing.T) {
	madison := ` clickhouse-server | 26.9.2.1 | https://packages.clickhouse.com/deb stable/main amd64 Packages
 clickhouse-server | 26.8.36.3 | https://packages.clickhouse.com/deb stable/main amd64 Packages
 clickhouse-server | 26.8.15.10 | https://packages.clickhouse.com/deb stable/main amd64 Packages
 clickhouse-server | 26.3.9.8 | https://packages.clickhouse.com/deb stable/main amd64 Packages
 clickhouse-server | 25.12.1.1 | https://packages.clickhouse.com/deb stable/main amd64 Packages
`
	for requested, want := range map[string]string{"": "26.8", "26.3": "26.3", "26.9": "26.9"} {
		if got, err := chooseClickHouseSeries(madison, requested); err != nil || got != want {
			t.Fatalf("series for %q = %q, %v", requested, got, err)
		}
	}
	if _, err := chooseClickHouseSeries(madison, "24.8"); err == nil {
		t.Fatal("unavailable series accepted")
	}
	if got, _ := chooseClickHouseSeries(" clickhouse-server | 26.10.1.1 | x\n clickhouse-server | 26.9.1.1 | x\n", ""); got != "26.10" {
		t.Fatalf("without LTS the newest series: %q", got)
	}
	for _, c := range []struct{ source, target, want string }{
		{"26.8.15.10", "26.8.36.3", clickhouseFormatBackup},
		{"26.8.15.10", "26.10.1.1", clickhouseFormatBackup},
		{"26.8.15.10", "26.3.9.8", clickhouseFormatNative},
		{"26.8.15.10", "", clickhouseFormatNative},
	} {
		if got := ClickHouseMoveFormat(c.source, c.target); got != c.want {
			t.Fatalf("format %s → %s = %s", c.source, c.target, got)
		}
	}
	for in, want := range map[string]string{"events": "`events`", "we`ird": "`we\\`ird`", `back\slash`: "`back\\\\slash`"} {
		if got, err := chIdent(in); err != nil || got != want {
			t.Fatalf("chIdent(%q) = %q", in, got)
		}
	}
	if got, _ := chString(`it's \`); got != `'it\'s \\'` {
		t.Fatalf("chString %q", got)
	}
	if _, err := chIdent("a\x00"); err == nil {
		t.Fatal("NUL identifier accepted")
	}
}

func TestRewriteClickHouseDatabase(t *testing.T) {
	statement := "CREATE MATERIALIZED VIEW telemetry.mv TO `telemetry`.agg AS SELECT 'telemetry.x' AS label, count() FROM telemetry.raw JOIN mytelemetry.other USING id"
	got := rewriteClickHouseDatabase(statement, "telemetry", "app_shop")
	want := "CREATE MATERIALIZED VIEW `app_shop`.mv TO `app_shop`.agg AS SELECT 'telemetry.x' AS label, count() FROM `app_shop`.raw JOIN mytelemetry.other USING id"
	if got != want {
		t.Fatalf("rewrite:\n%s\nwant:\n%s", got, want)
	}
	if rewriteClickHouseDatabase(statement, "telemetry", "telemetry") != statement {
		t.Fatal("same name rewritten")
	}
}

func TestVerifyClickHouseCounts(t *testing.T) {
	manifest := ClickHouseManifest{Tables: []ClickHouseManifestTable{
		{Name: "events", Engine: "MergeTree", Data: true, RowsBefore: 100, RowsAfter: 110},
		{Name: "latest", Engine: "ReplacingMergeTree", Data: true, RowsBefore: 50, RowsAfter: 50},
		{Name: "recent", Engine: "View"},
		{Name: ".inner_id.1", Engine: "AggregatingMergeTree", Data: true, RowsBefore: 5, RowsAfter: 5},
	}}
	present := map[string]bool{"events": true, "latest": true, "recent": true, ".inner_id.2": true}
	ok := map[string]uint64{"events": 105, "latest": 40, ".inner_id.2": 5}
	if err := verifyClickHouseCounts(manifest, ok, present); err != nil {
		t.Fatal(err)
	}
	for name, counts := range map[string]map[string]uint64{
		"fewer rows":         {"events": 99, "latest": 40, ".inner_id.2": 5},
		"more rows":          {"events": 111, "latest": 40, ".inner_id.2": 5},
		"collapsing above":   {"events": 100, "latest": 51, ".inner_id.2": 5},
		"collapsing emptied": {"events": 100, "latest": 0, ".inner_id.2": 5},
		"inner emptied":      {"events": 100, "latest": 40, ".inner_id.2": 0},
	} {
		if err := verifyClickHouseCounts(manifest, counts, present); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	missing := map[string]bool{"events": true, "latest": true, ".inner_id.2": true}
	if err := verifyClickHouseCounts(manifest, ok, missing); err == nil || !strings.Contains(err.Error(), "recent") {
		t.Fatalf("missing view: %v", err)
	}
}

// clickhouseHostFixture is a host without ClickHouse, a recorded runner that
// "installs" it and a fake server answering on its loopback address.
type clickhouseHostFixture struct {
	mu        sync.Mutex
	root      string
	commands  []string
	installed []bool // config files present when apt-get install ran
	gpg       string
	ch        *fakeClickHouse
}

const clickhouseTestKey = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nfixture\n-----END PGP PUBLIC KEY BLOCK-----\n"

func (f *clickhouseHostFixture) run(_ context.Context, _ string, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	command := name + " " + strings.Join(args, " ")
	f.commands = append(f.commands, command)
	switch {
	case name == "/usr/bin/gpg":
		return f.gpg, nil
	case name == "/usr/bin/dpkg":
		return "amd64\n", nil
	case name == "/usr/bin/apt-cache":
		return " clickhouse-server | 26.8.36.3 | https://packages.clickhouse.com/deb stable/main amd64 Packages\n clickhouse-server | 26.3.9.8 | https://packages.clickhouse.com/deb stable/main amd64 Packages\n", nil
	case name == "/usr/bin/apt-get" && slices.Contains(args, "clickhouse-server"):
		_, err := os.Stat(filepath.Join(f.root, nativeCHUsersFile))
		f.installed = append(f.installed, err == nil)
		path := filepath.Join(f.root, nativeCHServerBinary)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		return "", os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755)
	}
	return "", nil
}

func (f *clickhouseHostFixture) ran(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, command := range f.commands {
		if strings.HasPrefix(command, prefix) {
			n++
		}
	}
	return n
}

func newClickHouseHostFixture(t *testing.T, fingerprint string) (*Manager, *clickhouseHostFixture) {
	t.Helper()
	f := &clickhouseHostFixture{root: t.TempDir(), gpg: "pub:-:4096:1:8919F6BD2B48D754:1637580480:::-:::scESC::::::23::0:\nfpr:::::::::" + fingerprint + ":\nsub:-:4096:1:B5487D377C749E91:1637580480::::::e::::::23:\nfpr:::::::::4597E33F990AACD1BE58964CB5487D377C749E91:\n"}
	_ = os.MkdirAll(filepath.Join(f.root, "proc"), 0o755)
	_ = os.WriteFile(filepath.Join(f.root, "proc/meminfo"), []byte("MemTotal:        8388608 kB\n"), 0o644)
	m := &Manager{cfg: Config{DataDir: t.TempDir()}, key: bytes.Repeat([]byte{7}, 32), ctx: context.Background(), databaseSlots: make(chan struct{}, 1), data: diskState{Apps: map[string]App{}}}
	// The administrator password is created by the agent; the fake learns it
	// from the users.d file it writes.
	f.ch = newFakeClickHouse(t, "")
	f.ch.disk = filepath.Join(f.root, nativeCHBackupDir)
	previous := nativeHostDataHost
	nativeHostDataHost = func(*Manager) nativeHostData {
		return nativeHostData{root: f.root, run: f.run, clickhouseURL: f.ch.srv.URL, download: func(context.Context, string) ([]byte, error) { return []byte(clickhouseTestKey), nil }}
	}
	t.Cleanup(func() { nativeHostDataHost = previous })
	previousPoll := clickhousePollInterval
	clickhousePollInterval = time.Millisecond
	t.Cleanup(func() { clickhousePollInterval = previousPoll })
	return m, f
}

// learnAdmin gives the fake server the agent's administrator password once
// the agent recorded it.
func (f *clickhouseHostFixture) learnAdmin(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	server := m.data.NativeClickHouse
	m.mu.Unlock()
	if server == nil {
		t.Fatal("no server record")
	}
	admin, err := m.unsealHostSecret(server.Password)
	if err != nil {
		t.Fatal(err)
	}
	f.ch.mu.Lock()
	f.ch.users["default"] = admin
	f.ch.mu.Unlock()
}

func TestNativeClickHouseInstallsVerifiedAndProvisionsIdempotently(t *testing.T) {
	m, f := newClickHouseHostFixture(t, nativeCHKeyFingerprint)
	account := NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The first attempt installs, then waits for a server that refuses the
	// (not yet known) password; the retry continues with the same secret.
	short, stop := context.WithTimeout(ctx, 2*time.Second)
	if _, err := provisionNativeData(short, m, "shop", account, NativeDataRequest{ClickHouse: true, ClickHouseVersion: "26.8"}); err == nil {
		t.Fatal("a server that rejects the agent's password was accepted")
	}
	stop()
	f.learnAdmin(t, m)
	grant, err := provisionNativeData(ctx, m, "shop", account, NativeDataRequest{ClickHouse: true, ClickHouseVersion: "26.8"})
	if err != nil {
		t.Fatal(err)
	}
	if f.ran("/usr/bin/apt-get -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confold install -y -q --no-install-recommends clickhouse-server clickhouse-client") != 1 || !slices.Equal(f.installed, []bool{true}) {
		t.Fatalf("installation: %v (config before install: %v)", f.commands, f.installed)
	}
	for name, want := range map[string]string{
		nativeCHPreferences: "Pin: version 26.8.*",
		nativeCHSourceList:  "signed-by=/etc/apt/keyrings/clickhouse.asc arch=amd64] https://packages.clickhouse.com/deb stable main",
		nativeCHKeyring:     "BEGIN PGP PUBLIC KEY BLOCK",
		nativeCHConfigFile:  "<listen_host>127.0.0.1</listen_host>\n    <listen_host>::1</listen_host>",
		nativeCHUsersFile:   "<password_sha256_hex>",
		nativeCHDropIn:      "MemoryMax=2560M",
	} {
		raw, err := os.ReadFile(filepath.Join(f.root, name))
		if err != nil || !strings.Contains(string(raw), want) {
			t.Fatalf("%s: %v\n%s", name, err, raw)
		}
	}
	config, _ := os.ReadFile(filepath.Join(f.root, nativeCHConfigFile))
	if strings.Contains(string(config), "0.0.0.0") || strings.Contains(string(config), "<listen_host>::</listen_host>") {
		t.Fatal("ClickHouse listens beyond loopback")
	}
	if info, _ := os.Stat(filepath.Join(f.root, nativeCHUsersFile)); info.Mode().Perm() != 0o640 {
		t.Fatalf("users.d mode %v", info.Mode())
	}
	password := grant.Env["CLICKHOUSE_PASSWORD"]
	if grant.Env["CLICKHOUSE_URL"] != "http://127.0.0.1:8123" || grant.Env["CLICKHOUSE_DATABASE"] != "app_shop" || grant.Env["CLICKHOUSE_USER"] != "app_shop" || !nativeHexDigest(password, 64) || len(grant.Databases) != 1 {
		t.Fatalf("grant %+v", grant)
	}
	resource := grant.Databases[0]
	grants := 0
	for _, statement := range f.ch.statements() {
		if strings.HasPrefix(statement, "GRANT ") {
			grants++
			if !strings.HasSuffix(statement, " ON `app_shop`.* TO `app_shop`") {
				t.Fatalf("grant %s", statement)
			}
		}
		if strings.Contains(statement, password) {
			t.Fatal("the application password reached ClickHouse in plain text")
		}
	}
	if grants != 1 {
		t.Fatalf("grants %d", grants)
	}
	again, err := provisionNativeData(ctx, m, "shop", account, NativeDataRequest{ClickHouse: true})
	if err != nil || again.Env["CLICKHOUSE_PASSWORD"] != password || again.Databases[0] != resource {
		t.Fatalf("re-provisioning: %v %+v", err, again)
	}
	if f.ran("/usr/bin/apt-get -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confold install -y -q --no-install-recommends clickhouse-server") != 1 {
		t.Fatal("ClickHouse installed twice")
	}
	m.mu.Lock()
	binding := m.data.NativeDatabases[resource]
	d := m.data.Databases[resource]
	inUse := m.nativeDatabaseInUseLocked(resource)
	m.mu.Unlock()
	if !inUse || binding.Policy.ClickHouseDatabase != "app_shop" || !slices.Equal(binding.Policy.Roles, []string{"analytics"}) || d.URL != "clickhouse://app_shop@127.0.0.1:8123/app_shop" || strings.Contains(d.URL, password) || d.Backup.Hours != 24 {
		t.Fatalf("resource %+v %+v", binding, d)
	}
	if views := m.databaseViews(); len(views) != 1 || views[0].Engine != "clickhouse" {
		t.Fatalf("views %+v", views)
	}
	view, err := m.nativeHostDataView(ctx, "shop")
	if err != nil || view.ClickHouse == nil || !view.ClickHouse.Ready || view.ClickHouse.Version != "26.8.15.10" || view.ClickHouse.QueryMemoryMiB != 1024 || !view.ClickHouse.BackupCovered {
		t.Fatalf("view %+v %v", view.ClickHouse, err)
	}
	if record, _ := m.hostDataRecord("shop"); record.ClickHouse == nil || strings.Contains(record.ClickHouse.Password, password) {
		t.Fatal("the record holds the plain password")
	}

	// Scheduled backups and the pre-release gate use the backup pipeline.
	f.ch.seed("app_shop", map[string]uint64{"events": 40, "sessions": 7})
	record, err := m.dumpDatabase(ctx, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(m.backupDir(resource), record.ID+".dump"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	// The dump is a tar stream, Zstandard-compressed when the tool is
	// installed: read it through the same detection the restore uses.
	plain, _, err := decompressedReader(ctx, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := readClickHouseManifest(tar.NewReader(plain))
	plain.Close()
	if err != nil || hex.EncodeToString(sum[:]) != record.SHA256 || manifest.Format != clickhouseFormatDirectory || manifest.Database != "app_shop" || len(manifest.Tables) != 2 {
		t.Fatalf("backup %+v %v", manifest, err)
	}
	// The pre-release gate covers an application whose only database is
	// ClickHouse (the analytics role).
	m.data.NativeApps = map[string]NativeApplication{"shop": {ID: "shop", Name: "shop"}}
	m.nativeOperations = map[string]bool{"shop": true}
	proofs, err := m.backupBeforeNativeRailsRelease(ctx, "shop", []string{"analytics"})
	if err != nil || len(proofs) != 1 || proofs[0].Resource != resource || !nativeHexDigest(proofs[0].SHA256, 64) {
		t.Fatalf("pre-release gate: %v %+v", err, proofs)
	}
	delete(m.nativeOperations, "shop")

	// Release removes the user and the database; the server stops when unused.
	m.data.NativeCreated = map[string]NativeReleasePolicy{"shop": {Root: t.TempDir()}}
	if err := releaseNativeHostData(ctx, m, "shop"); err != nil {
		t.Fatal(err)
	}
	statements := f.ch.statements()
	if !slices.Contains(statements, "DROP USER IF EXISTS `app_shop`") || !slices.Contains(statements, "DROP DATABASE IF EXISTS `app_shop` SYNC") {
		t.Fatalf("release statements %v", statements[len(statements)-4:])
	}
	if f.ran("/usr/bin/systemctl disable --now clickhouse-server.service") != 1 {
		t.Fatal("unused server kept running")
	}
	if _, ok := m.hostDataRecord("shop"); ok {
		t.Fatal("record retained")
	}
}

func TestNativeClickHouseRefusesUnverifiedOrForeignInstallations(t *testing.T) {
	m, f := newClickHouseHostFixture(t, "0000000000000000000000000000000000000000")
	account := NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}
	ctx := context.Background()
	if _, err := provisionNativeData(ctx, m, "shop", account, NativeDataRequest{ClickHouse: true}); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("mismatching key: %v", err)
	}
	if f.ran("/usr/bin/apt-get -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confold install -y -q --no-install-recommends clickhouse-server") != 0 {
		t.Fatal("packages installed with an unverified key")
	}
	if _, err := os.Stat(filepath.Join(f.root, nativeCHSourceList)); err == nil {
		t.Fatal("repository added with an unverified key")
	}
	// A ClickHouse the agent did not install is left alone.
	other, g := newClickHouseHostFixture(t, nativeCHKeyFingerprint)
	_ = os.MkdirAll(filepath.Join(g.root, "usr/bin"), 0o755)
	_ = os.WriteFile(filepath.Join(g.root, nativeCHServerBinary), []byte("#!/bin/sh\n"), 0o755)
	if _, err := provisionNativeData(ctx, other, "shop", account, NativeDataRequest{ClickHouse: true}); err == nil || !strings.Contains(err.Error(), "not managed") {
		t.Fatalf("foreign installation: %v", err)
	}
	if len(g.commands) != 0 {
		t.Fatalf("foreign installation touched: %v", g.commands)
	}
	if err := (NativeHostDataConfig{ClickHouseMemoryRatio: 0.95}).validate(); err == nil {
		t.Fatal("memory ratio above 0.9 accepted")
	}
	if err := (NativeHostDataConfig{ClickHouseHTTPPort: 9000}).validate(); err == nil {
		t.Fatal("equal ports accepted")
	}
}
