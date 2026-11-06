package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests run a real ClickHouse server from the binary named by
// LIDZA_TEST_CLICKHOUSE (for example the clickhouse binary of the
// clickhouse-common-static package) with the agent's own rendered
// configuration, and exercise provisioning, grants, backups, in-place
// restores and both move archive formats against it.

type realClickHouse struct {
	root   string
	host   nativeHostData
	server NativeClickHouseServer
	admin  string
	run    *recordingRunner
}

type recordingRunner struct {
	mu       sync.Mutex
	commands []string
}

func (r *recordingRunner) run(_ context.Context, _ string, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, name+" "+strings.Join(args, " "))
	return "", nil
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startRealClickHouse(t *testing.T) *realClickHouse {
	t.Helper()
	binary := os.Getenv("LIDZA_TEST_CLICKHOUSE")
	if binary == "" {
		t.Skip("set LIDZA_TEST_CLICKHOUSE to a clickhouse binary to run against a real server")
	}
	root := t.TempDir()
	fx := &realClickHouse{root: root, admin: nativeRandomSecret(), run: &recordingRunner{}}
	fx.server = NativeClickHouseServer{HTTPPort: freePort(t), TCPPort: freePort(t), Installed: true}
	fx.host = nativeHostData{root: root, run: fx.run.run, clickhouseURL: fmt.Sprintf("http://127.0.0.1:%d", fx.server.HTTPPort)}
	etc := filepath.Join(root, "etc/clickhouse-server")
	files := map[string]string{
		"etc/clickhouse-server/config.xml": fmt.Sprintf(`<clickhouse>
    <logger><level>warning</level><log>%[1]s/log/server.log</log><errorlog>%[1]s/log/server.err.log</errorlog></logger>
    <listen_host>0.0.0.0</listen_host>
    <http_port>1</http_port>
    <tcp_port>2</tcp_port>
    <mysql_port>%[2]d</mysql_port>
    <path>%[1]s/data/</path>
    <tmp_path>%[1]s/data/tmp/</tmp_path>
    <user_files_path>%[1]s/data/user_files/</user_files_path>
    <format_schema_path>%[1]s/data/format_schemas/</format_schema_path>
    <user_directories>
        <users_xml><path>users.xml</path></users_xml>
        <local_directory><path>%[1]s/data/access/</path></local_directory>
    </user_directories>
    <default_profile>default</default_profile>
    <default_database>default</default_database>
    <query_log><database>system</database><table>query_log</table></query_log>
</clickhouse>
`, root, freePort(t)),
		"etc/clickhouse-server/users.xml": `<clickhouse>
    <profiles><default/></profiles>
    <users><default><password></password><networks><ip>::/0</ip></networks><profile>default</profile><quota>default</quota></default></users>
    <quotas><default/></quotas>
</clickhouse>
`,
		"proc/meminfo":                 "MemTotal:        4194304 kB\n",
		"usr/bin/clickhouse-server":    "#!/bin/sh\n",
		"var/lib/clickhouse/.keep":     "",
		"var/log/clickhouse-server/.k": "",
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fx.host.writeClickHouseFiles(fx.server, fx.admin, false); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(root, "stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "server", "--config-file="+filepath.Join(etc, "config.xml"))
	cmd.Dir, cmd.Stdout, cmd.Stderr = root, logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		logFile.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	version, err := fx.client().waitReady(ctx, 80*time.Second)
	if err != nil {
		out, _ := os.ReadFile(filepath.Join(root, "stdout.log"))
		errs, _ := os.ReadFile(filepath.Join(root, "log/server.err.log"))
		t.Fatalf("server did not start: %v\n%s\n%s", err, out, errs)
	}
	// A server that reported its version keeps its ports: they are the
	// running test server's.
	fx.server.Version = version
	return fx
}

func (fx *realClickHouse) client() clickhouseClient {
	return fx.host.clickhouse(fx.server, "default", fx.admin)
}

func (fx *realClickHouse) manager(t *testing.T) *Manager {
	t.Helper()
	previous := nativeHostDataHost
	nativeHostDataHost = func(*Manager) nativeHostData { return fx.host }
	t.Cleanup(func() { nativeHostDataHost = previous })
	m := &Manager{cfg: Config{DataDir: t.TempDir()}, key: bytes.Repeat([]byte{7}, 32), ctx: context.Background(), databaseSlots: make(chan struct{}, 1), data: diskState{Apps: map[string]App{}}}
	sealed, err := m.sealHostSecret(fx.admin)
	if err != nil {
		t.Fatal(err)
	}
	server := fx.server
	server.Password = sealed
	m.data.NativeClickHouse = &server
	return m
}

func mustExec(t *testing.T, ctx context.Context, c clickhouseClient, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if err := c.exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func mustQuery(t *testing.T, ctx context.Context, c clickhouseClient, sql string) string {
	t.Helper()
	out, err := c.query(ctx, sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return out
}

func TestRealClickHouseConfigIsLoopbackOnly(t *testing.T) {
	fx := startRealClickHouse(t)
	preprocessed, err := os.ReadFile(filepath.Join(fx.root, "data/preprocessed_configs/config.xml"))
	if err != nil {
		t.Fatal(err)
	}
	hosts := regexp.MustCompile(`<listen_host>([^<]*)</listen_host>`).FindAllStringSubmatch(string(preprocessed), -1)
	if len(hosts) != 2 || hosts[0][1] != "127.0.0.1" || hosts[1][1] != "::1" {
		t.Fatalf("listen hosts %v", hosts)
	}
	for _, removed := range []string{"<mysql_port>", "<postgresql_port>", "<interserver_http_port>"} {
		if strings.Contains(string(preprocessed), removed) {
			t.Fatalf("%s still configured", removed)
		}
	}
	ctx := context.Background()
	if _, err := (clickhouseClient{base: fx.host.clickhouseURL, user: "default"}).version(ctx); err == nil {
		t.Fatal("the default user signs in without the agent's password")
	}
	ratio := mustQuery(t, ctx, fx.client(), "SELECT value FROM system.server_settings WHERE name = 'max_server_memory_usage_to_ram_ratio'")
	if ratio != "0.25" {
		t.Fatalf("memory ratio %q", ratio)
	}
	disks := mustQuery(t, ctx, fx.client(), "SELECT path FROM system.disks WHERE name = 'backups'")
	if disks != filepath.Join(fx.root, nativeCHBackupDir)+"/" {
		t.Fatalf("backups disk %q", disks)
	}
}

// seedTelemetry creates tables of every kind the archive handles.
func seedTelemetry(t *testing.T, ctx context.Context, c clickhouseClient, db string, rows int, inner bool) {
	t.Helper()
	q := mustCHIdent(db)
	mustExec(t, ctx, c,
		"CREATE TABLE "+q+".events (id UInt64, kind LowCardinality(String), at DateTime DEFAULT now(), day Date MATERIALIZED toDate(at)) ENGINE = MergeTree ORDER BY id",
		"CREATE TABLE "+q+".per_kind (kind String, n UInt64) ENGINE = SummingMergeTree ORDER BY kind",
		"CREATE MATERIALIZED VIEW "+q+".per_kind_mv TO "+q+".per_kind AS SELECT kind, count() AS n FROM "+q+".events GROUP BY kind",
		"CREATE VIEW "+q+".recent AS SELECT id, kind FROM "+q+".events WHERE id > 10",
		"CREATE TABLE "+q+".notes (k String, v String) ENGINE = TinyLog",
	)
	if inner {
		mustExec(t, ctx, c, "CREATE MATERIALIZED VIEW "+q+".totals ENGINE = SummingMergeTree ORDER BY kind AS SELECT kind, count() AS n FROM "+q+".events GROUP BY kind")
	}
	mustExec(t, ctx, c,
		fmt.Sprintf("INSERT INTO %s.events (id, kind) SELECT number, ['view', 'click', 'error'][number %% 3 + 1] FROM numbers(%d)", q, rows),
		"INSERT INTO "+q+".notes VALUES ('a', 'first'), ('b', 'second')",
	)
}

func archiveManifest(t *testing.T, archive []byte) ClickHouseManifest {
	t.Helper()
	plain, _, err := decompressedReader(context.Background(), bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	manifest, err := readClickHouseManifest(tar.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestRealClickHouseProvisioningBackupsAndMoves(t *testing.T) {
	fx := startRealClickHouse(t)
	m := fx.manager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	account := NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}
	grant, err := provisionNativeData(ctx, m, "shop", account, NativeDataRequest{ClickHouse: true})
	if err != nil {
		t.Fatal(err)
	}
	password := grant.Env["CLICKHOUSE_PASSWORD"]
	if grant.Env["CLICKHOUSE_URL"] != fmt.Sprintf("http://127.0.0.1:%d", fx.server.HTTPPort) || grant.Env["CLICKHOUSE_DATABASE"] != "app_shop" || grant.Env["CLICKHOUSE_USER"] != "app_shop" || !nativeHexDigest(password, 64) || len(grant.Databases) != 1 {
		t.Fatalf("grant %+v %v", grant.Env, grant.Databases)
	}
	resource := grant.Databases[0]
	// Idempotent: the same password and resource again.
	again, err := provisionNativeData(ctx, m, "shop", account, NativeDataRequest{ClickHouse: true})
	if err != nil || again.Env["CLICKHOUSE_PASSWORD"] != password || again.Databases[0] != resource {
		t.Fatalf("re-provisioning changed the credential or resource: %v %+v", err, again)
	}
	app := clickhouseClient{base: fx.host.clickhouseURL, user: "app_shop", password: password}
	admin := fx.client()
	grants := mustQuery(t, ctx, admin, "SHOW GRANTS FOR app_shop")
	if !strings.Contains(grants, "ON app_shop.* TO app_shop") || strings.Contains(grants, "*.*") {
		t.Fatalf("grants %s", grants)
	}
	if limit := mustQuery(t, ctx, app, "SELECT getSetting('max_memory_usage'), getSetting('max_execution_time')"); limit != fmt.Sprintf("%d\t%d", int64(512)<<20, nativeCHMaxExecution) {
		t.Fatalf("profile limits %q", limit)
	}
	var denied *clickhouseError
	if err := app.exec(ctx, "CREATE DATABASE other"); !errors.As(err, &denied) || denied.code() != 497 {
		t.Fatalf("the application may create other databases: %v", err)
	}
	mustExec(t, ctx, admin, "CREATE DATABASE private", "CREATE TABLE private.secrets (v String) ENGINE = Memory")
	if _, err := app.query(ctx, "SELECT * FROM private.secrets"); !errors.As(err, &denied) || denied.code() != 497 {
		t.Fatalf("the application reads another database: %v", err)
	}
	if err := app.exec(ctx, "CREATE DATABASE IF NOT EXISTS app_shop"); err != nil {
		t.Fatalf("migrations may ensure their own database: %v", err)
	}
	seedTelemetry(t, ctx, app, "app_shop", 3000, true)
	m.mu.Lock()
	if !m.nativeDatabaseInUseLocked(resource) || m.data.NativeDatabases[resource].Policy.ClickHouseDatabase != "app_shop" {
		m.mu.Unlock()
		t.Fatal("the database is not a protected resource")
	}
	d := m.data.Databases[resource]
	m.mu.Unlock()
	if views := m.databaseViews(); len(views) != 1 || views[0].Engine != "clickhouse" || views[0].Retained {
		t.Fatalf("Data & Storage %+v", views)
	}
	if size, err := m.hostClickHouseSize(ctx, resource); err != nil || size <= 0 {
		t.Fatalf("size %d %v", size, err)
	}

	// The backup pipeline: a verified archive in the database's backup directory.
	record, err := m.dumpDatabase(ctx, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(filepath.Join(m.backupDir(resource), record.ID+".dump"))
	if err != nil || int64(len(archive)) != record.Size {
		t.Fatal(err)
	}
	manifest := archiveManifest(t, archive)
	if manifest.Format != clickhouseFormatDirectory || manifest.Database != "app_shop" || len(manifest.Tables) != 7 || manifest.Files == 0 {
		t.Fatalf("manifest %+v", manifest)
	}
	if leftovers, _ := os.ReadDir(filepath.Join(fx.root, nativeCHBackupDir, nativeCHBackupSubdir)); len(leftovers) != 0 {
		t.Fatalf("server-side backup files left behind: %v", leftovers)
	}
	// In-place restore of that backup replaces later writes.
	mustExec(t, ctx, app, "INSERT INTO app_shop.events (id, kind) SELECT number + 100000, 'late' FROM numbers(50)")
	m.data.Databases[resource] = Database{AppID: d.AppID, Mode: d.Mode, URL: d.URL, Ready: true, Backup: d.Backup, Backups: []BackupRecord{record}}
	if err := m.restoreClickHouseBackup(ctx, "shop", resource, record); err != nil {
		t.Fatal(err)
	}
	if got := mustQuery(t, ctx, app, "SELECT count() FROM app_shop.events"); got != "3000" {
		t.Fatalf("restored events %s", got)
	}

	// A destination database under another name.
	target, err := provisionNativeData(ctx, m, "shop-two", NativeAppAccount{User: "app-shop-two", UID: 1002, GID: 1002, Root: "/srv/apps/shop-two"}, NativeDataRequest{ClickHouse: true})
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := m.hostClickHouseService("shop")
	if err != nil {
		t.Fatal(err)
	}
	destination, _, err := m.hostClickHouseService("shop-two")
	if err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if _, err := exportClickHouseArchive(ctx, source, clickhouseFormatBackup, t.TempDir(), &exported); err != nil {
		t.Fatal(err)
	}
	result, err := restoreClickHouseArchive(ctx, destination, bytes.NewReader(exported.Bytes()), true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Database != "app_shop_two" || result.Source != "app_shop" || result.Counts["events"] != 3000 || result.Counts["notes"] != 2 {
		t.Fatalf("restore %+v", result)
	}
	targetApp := clickhouseClient{base: fx.host.clickhouseURL, user: "app_shop_two", password: target.Env["CLICKHOUSE_PASSWORD"]}
	if create := mustQuery(t, ctx, targetApp, "SELECT create_table_query FROM system.tables WHERE database = 'app_shop_two' AND name = 'per_kind_mv'"); !strings.Contains(create, "app_shop_two.per_kind") || strings.Contains(create, "app_shop.") {
		t.Fatalf("view not renamed: %s", create)
	}
	// The destination's own user keeps its grants after the database was replaced.
	if got := mustQuery(t, ctx, targetApp, "SELECT count() FROM recent"); got != "2989" {
		t.Fatalf("view after restore %s", got)
	}
	// New inserts flow through the restored materialized view.
	mustExec(t, ctx, targetApp, "INSERT INTO app_shop_two.events (id, kind) VALUES (900000, 'view')")
	if got := mustQuery(t, ctx, targetApp, "SELECT sum(n) FROM per_kind"); got != "3001" {
		t.Fatalf("materialized view after restore %s", got)
	}

	// A per-table archive refuses inner tables, then moves everything else.
	if _, err := exportClickHouseArchive(ctx, source, clickhouseFormatNative, t.TempDir(), io.Discard); err == nil || !strings.Contains(err.Error(), "inner table") {
		t.Fatalf("native export with an inner table: %v", err)
	}
	mustExec(t, ctx, app, "DROP VIEW app_shop.totals SYNC")
	exported.Reset()
	if _, err := exportClickHouseArchive(ctx, source, clickhouseFormatNative, t.TempDir(), &exported); err != nil {
		t.Fatal(err)
	}
	if manifest := archiveManifest(t, exported.Bytes()); manifest.Format != clickhouseFormatNative || manifest.Tables[0].File == "" || manifest.Tables[len(manifest.Tables)-1].Engine != "MaterializedView" {
		t.Fatalf("native manifest order %+v", manifest.Tables)
	}
	result, err = restoreClickHouseArchive(ctx, destination, bytes.NewReader(exported.Bytes()), true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != clickhouseFormatNative || result.Counts["events"] != 3000 || result.Counts["per_kind"] != 3 || result.Counts["notes"] != 2 {
		t.Fatalf("native restore %+v", result)
	}
	if got := mustQuery(t, ctx, targetApp, "SELECT count() FROM app_shop_two.events WHERE day = today()"); got != "3000" {
		t.Fatalf("materialized column after the native copy: %s", got)
	}
	if !strings.Contains(mustQuery(t, ctx, targetApp, "SELECT create_table_query FROM system.tables WHERE database = 'app_shop_two' AND name = 'per_kind_mv'"), "app_shop_two.events") {
		t.Fatal("native copy did not rename the view's source")
	}
	// Without replace a database with tables is refused.
	if _, err := restoreClickHouseArchive(ctx, destination, bytes.NewReader(exported.Bytes()), false); err == nil {
		t.Fatal("restore over existing tables without replace")
	}

	// Release takes a final backup, then removes the user and the database.
	if err := releaseNativeHostData(ctx, m, "shop-two"); err != nil {
		t.Fatal(err)
	}
	if users := mustQuery(t, ctx, admin, "SELECT count() FROM system.users WHERE name = 'app_shop_two'"); users != "0" {
		t.Fatal("user retained after release")
	}
	if dbs := mustQuery(t, ctx, admin, "SELECT count() FROM system.databases WHERE name = 'app_shop_two'"); dbs != "0" {
		t.Fatal("database retained after release")
	}
	m.mu.Lock()
	retained := m.data.Databases[target.Databases[0]]
	_, bound := m.data.NativeDatabases[target.Databases[0]]
	m.mu.Unlock()
	if bound || len(retained.Backups) != 1 || retained.Backups[0].Kind != predeploymentBackup {
		t.Fatalf("final backup not retained: %+v bound=%t", retained, bound)
	}
}

// A database of thousands of small parts moves as a directory BACKUP and
// restores in seconds; a .zip archive of the same parts restores at a few
// files per second. LIDZA_TEST_CLICKHOUSE_PARTS overrides the part count.
func TestRealClickHouseDirectoryBackupOfManyParts(t *testing.T) {
	fx := startRealClickHouse(t)
	m := fx.manager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	parts := 5000
	if n, err := strconv.Atoi(os.Getenv("LIDZA_TEST_CLICKHOUSE_PARTS")); err == nil && n > 0 {
		parts = n
	}
	for _, id := range []string{"shop", "shop-two"} {
		account := NativeAppAccount{User: "app-" + id, UID: 1001, GID: 1001, Root: "/srv/apps/" + id}
		if _, err := provisionNativeData(ctx, m, id, account, NativeDataRequest{ClickHouse: true}); err != nil {
			t.Fatal(err)
		}
	}
	admin := fx.client()
	// One partition per row: every row stays its own part.
	mustExec(t, ctx, admin,
		"CREATE TABLE app_shop.events (id UInt64, kind String) ENGINE = MergeTree PARTITION BY id ORDER BY id",
		"CREATE VIEW app_shop.recent AS SELECT id FROM app_shop.events WHERE id > 10",
		fmt.Sprintf("INSERT INTO app_shop.events SELECT number, 'view' FROM numbers(%d) SETTINGS max_partitions_per_insert_block = 0, max_insert_block_size = 1000000", parts))
	if got := mustQuery(t, ctx, admin, "SELECT count() FROM system.parts WHERE database = 'app_shop' AND table = 'events' AND active"); got != strconv.Itoa(parts) {
		t.Fatalf("parts %s", got)
	}
	source, _, err := m.hostClickHouseService("shop")
	if err != nil {
		t.Fatal(err)
	}
	destination, _, err := m.hostClickHouseService("shop-two")
	if err != nil {
		t.Fatal(err)
	}
	compression := compressionNone
	if zstdAvailable() {
		compression = compressionZstd
	}
	archive := filepath.Join(t.TempDir(), "archive")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	writer, err := compressedWriter(ctx, file, compression)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := exportClickHouseArchive(ctx, source, clickhouseFormatDirectory, t.TempDir(), writer)
	if err == nil {
		err = writer.Close()
	}
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	exported := time.Since(started)
	info, _ := os.Stat(archive)
	if manifest.Format != clickhouseFormatDirectory || manifest.Files < int64(parts) {
		t.Fatalf("manifest %s with %d files", manifest.Format, manifest.Files)
	}
	input, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	started = time.Now()
	result, err := restoreClickHouseArchive(ctx, destination, input, true)
	if err != nil {
		t.Fatal(err)
	}
	restored := time.Since(started)
	t.Logf("%d parts, %d backup files: export %s (%s, %.1f MiB), restore %s", parts, manifest.Files, exported.Round(time.Millisecond), cmpOr(compression, "plain tar"), float64(info.Size())/(1<<20), restored.Round(time.Millisecond))
	if result.Format != clickhouseFormatDirectory || result.Counts["events"] != uint64(parts) {
		t.Fatalf("restore %+v", result)
	}
	if restored > 60*time.Second {
		t.Fatalf("the restore of %d parts took %s", parts, restored)
	}
	if got := mustQuery(t, ctx, admin, "SELECT count() FROM app_shop_two.recent"); got != strconv.Itoa(parts-11) {
		t.Fatalf("view after restore %s", got)
	}
	// Only directory backups were used: no .zip archive was written or read.
	if names := mustQuery(t, ctx, admin, "SELECT groupArray(name) FROM system.backups"); strings.Contains(names, ".zip") || !strings.Contains(names, "agent/lidza-") {
		t.Fatalf("backups %s", names)
	}
	if leftovers, _ := os.ReadDir(filepath.Join(fx.root, nativeCHBackupDir, nativeCHBackupSubdir)); len(leftovers) != 0 {
		t.Fatalf("backup directories left behind: %v", leftovers)
	}
	if dbs := mustQuery(t, ctx, admin, "SELECT count() FROM system.databases WHERE name LIKE '%"+clickhousePreviousSuffix+"'"); dbs != "0" {
		t.Fatal("the replaced database was kept")
	}
	// An abort kills a running BACKUP instead of waiting for it.
	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	if _, err := exportClickHouseArchive(short, source, clickhouseFormatDirectory, t.TempDir(), io.Discard); err == nil {
		t.Fatal("a cancelled export succeeded")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		status := mustQuery(t, ctx, admin, "SELECT status FROM system.backups ORDER BY start_time DESC LIMIT 1")
		if status == "BACKUP_CANCELLED" || status == "BACKUP_FAILED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cancelled backup is %s", status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
