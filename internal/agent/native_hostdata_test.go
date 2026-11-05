package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativeHostDataQuoting(t *testing.T) {
	for in, want := range map[string]string{`app_shop`: `"app_shop"`, `we"ird`: `"we""ird"`} {
		if got, err := pgQuoteIdent(in); err != nil || got != want {
			t.Fatalf("pgQuoteIdent(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "a\x00b", strings.Repeat("a", 64)} {
		if _, err := pgQuoteIdent(bad); err == nil {
			t.Fatalf("pgQuoteIdent(%q) accepted", bad)
		}
	}
	for in, want := range map[string]string{`plain`: `'plain'`, `it's`: `'it''s'`, `back\slash'`: `E'back\\slash'''`, ``: `''`} {
		if got, err := pgQuoteLiteral(in); err != nil || got != want {
			t.Fatalf("pgQuoteLiteral(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := pgQuoteLiteral("a\x00"); err == nil {
		t.Fatal("NUL literal accepted")
	}
	if name, err := nativePGName("shop-web-2"); err != nil || name != "app_shop_web_2" {
		t.Fatalf("nativePGName = %q, %v", name, err)
	}
	for _, bad := range []string{"", "Shop", "shop_web", "-shop", `x"; DROP`} {
		if _, err := nativePGName(bad); err == nil {
			t.Fatalf("nativePGName(%q) accepted", bad)
		}
	}
}

// RFC 7677 section 3: user "user", password "pencil". The verifier's keys
// must reproduce the example's client proof and server signature.
func TestNativeSCRAMVerifierRFC7677(t *testing.T) {
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	verifier, err := nativeSCRAMVerifier("pencil", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`^SCRAM-SHA-256\$4096:W22ZaJ0SNY7soEsUEjb6gQ==\$([A-Za-z0-9+/=]+):([A-Za-z0-9+/=]+)$`).FindStringSubmatch(verifier)
	if match == nil {
		t.Fatalf("verifier format: %s", verifier)
	}
	stored, _ := base64.StdEncoding.DecodeString(match[1])
	server, _ := base64.StdEncoding.DecodeString(match[2])
	nonce := "rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	auth := "n=user,r=rOprNGfwEbeRWgbNEkqO,r=" + nonce + ",s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096,c=biws,r=" + nonce
	if got := base64.StdEncoding.EncodeToString(nativeHMAC(server, auth)); got != "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=" {
		t.Fatalf("server signature %s", got)
	}
	proof, _ := base64.StdEncoding.DecodeString("dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=")
	signature := nativeHMAC(stored, auth)
	clientKey := make([]byte, len(proof))
	for i := range proof {
		clientKey[i] = proof[i] ^ signature[i]
	}
	if sum := sha256.Sum256(clientKey); !hmac.Equal(sum[:], stored) {
		t.Fatal("stored key does not verify the RFC client proof")
	}
	if _, err := nativeSCRAMVerifier("pencil", salt[:8], 4096); err == nil {
		t.Fatal("short salt accepted")
	}
	if secret := nativeRandomSecret(); !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(secret) {
		t.Fatalf("secret %q", secret)
	}
}

func TestNativePGProvisionSQLGolden(t *testing.T) {
	got, err := renderNativePGProvisionSQL("app_shop_web", "SCRAM-SHA-256$4096:c2FsdHNhbHRzYWx0c2FsdA==$c3RvcmVk:c2VydmVy")
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT 'CREATE ROLE "app_shop_web"' WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'app_shop_web')\gexec
ALTER ROLE "app_shop_web" WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT PASSWORD 'SCRAM-SHA-256$4096:c2FsdHNhbHRzYWx0c2FsdA==$c3RvcmVk:c2VydmVy';
SELECT 'CREATE DATABASE "app_shop_web" OWNER "app_shop_web" TEMPLATE template0 ENCODING ''UTF8''' WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = 'app_shop_web')\gexec
ALTER DATABASE "app_shop_web" OWNER TO "app_shop_web";
REVOKE CONNECT, TEMPORARY ON DATABASE "app_shop_web" FROM PUBLIC;
GRANT CONNECT, TEMPORARY ON DATABASE "app_shop_web" TO "app_shop_web";
\connect "app_shop_web"
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO "app_shop_web";
`
	if got != want {
		t.Fatalf("provision SQL:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "SUPERUSER ") && !strings.Contains(got, "NOSUPERUSER") {
		t.Fatal("role may become superuser")
	}
	if _, err := renderNativePGProvisionSQL("app_shop_web", "plaintext"); err == nil {
		t.Fatal("plaintext password accepted")
	}
}

func TestNativePGReleaseSQLGolden(t *testing.T) {
	got, err := renderNativePGReleaseSQL("app_shop_web")
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT 'ALTER ROLE "app_shop_web" NOLOGIN' WHERE EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'app_shop_web')\gexec
SELECT count(pg_catalog.pg_terminate_backend(pid)) FROM pg_catalog.pg_stat_activity WHERE datname = 'app_shop_web' OR usename = 'app_shop_web';
DROP DATABASE IF EXISTS "app_shop_web" WITH (FORCE);
DROP ROLE IF EXISTS "app_shop_web";
`
	if got != want {
		t.Fatalf("release SQL:\n%s\nwant:\n%s", got, want)
	}
}

func TestNativePGURL(t *testing.T) {
	if got := nativePGURL("app_shop", strings.Repeat("ab", 32), 5433); got != "postgres://app_shop:"+strings.Repeat("ab", 32)+"@127.0.0.1:5433/app_shop?sslmode=disable" {
		t.Fatal(got)
	}
	// The URL passes the native backup helper's connection validation.
	policy := NativeDatabasePolicy{Host: "127.0.0.1", Port: "5433"}
	if err := validateNativeDatabaseURL(nativePGURL("app_shop", "x", 5433), policy); err != nil {
		t.Fatal(err)
	}
}

func TestNativePGClusters(t *testing.T) {
	out := `16  main    5432 online postgres /var/lib/postgresql/16/main /var/log/postgresql/postgresql-16-main.log
17  main    5433 online postgres /var/lib/postgresql/17/main /var/log/postgresql/postgresql-17-main.log
17  replica 5434 online,recovery postgres /var/lib/postgresql/17/replica /var/log/postgresql/postgresql-17-replica.log
15  old     5435 down   postgres /var/lib/postgresql/15/old /var/log/postgresql/postgresql-15-old.log
9.6 legacy  5436 online postgres /var/lib/postgresql/9.6/legacy /var/log/x.log
`
	clusters := parsePGClusters(out)
	if len(clusters) != 4 || !clusters[2].Recovery || clusters[3].Online {
		t.Fatalf("clusters %+v", clusters)
	}
	cases := []struct {
		version, port int
		want          string
		fails         bool
	}{
		{0, 0, "16/main", false},
		{17, 0, "17/main", false},
		{0, 5433, "17/main", false},
		{0, 5434, "", true}, // standby
		{15, 0, "", true},   // stopped
		{18, 0, "", true},   // absent
	}
	for _, c := range cases {
		got, err := chooseNativePGCluster(clusters, c.version, c.port)
		if c.fails != (err != nil) || !c.fails && got.ID() != c.want {
			t.Fatalf("choose(%d,%d) = %s, %v", c.version, c.port, got.ID(), err)
		}
	}
	if _, err := chooseNativePGCluster(parsePGClusters("14 main 5432 online postgres /d /l\n"), 0, 0); err == nil || strings.Contains(err.Error(), "no PostgreSQL cluster") {
		t.Fatalf("unsupported version: %v", err)
	}
	if major := nativeDistributionPGMajor("Package: postgresql\nVersion: 16+257\nDepends: postgresql-16\n"); major != 16 {
		t.Fatal(major)
	}
	if major := nativeDistributionPGMajor("Package: postgresql\n"); major != 0 {
		t.Fatal(major)
	}
}

const nativeHBAFixture = `# PostgreSQL Client Authentication Configuration File
# TYPE  DATABASE        USER            ADDRESS                 METHOD

local   all             postgres                                peer
local   all             all                                     peer
host    all             all             127.0.0.1/32            scram-sha-256
host    all             all             ::1/128                 scram-sha-256
`

func TestNativeHBABlock(t *testing.T) {
	block := renderNativeHBABlock([]nativeHBAEntry{{Role: "app_b", Database: "app_b"}, {Role: "app_a", Database: "app_a"}})
	want := [][]string{
		{"#", "BEGIN"},
		{"local", "app_a", "app_a", "scram-sha-256"},
		{"host", "app_a", "app_a", "127.0.0.1/32", "scram-sha-256"},
		{"host", "app_a", "app_a", "::1/128", "scram-sha-256"},
		{"local", "all", "app_a", "reject"},
		{"host", "all", "app_a", "all", "reject"},
		{"local", "app_b", "app_b", "scram-sha-256"},
		{"host", "app_b", "app_b", "127.0.0.1/32", "scram-sha-256"},
		{"host", "app_b", "app_b", "::1/128", "scram-sha-256"},
		{"local", "all", "app_b", "reject"},
		{"host", "all", "app_b", "all", "reject"},
		{"#", "END"},
	}
	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("block:\n%s", block)
	}
	for i, line := range lines {
		fields := strings.Fields(line)
		if fields[0] == "#" {
			fields = fields[:2]
		}
		if strings.Join(fields, " ") != strings.Join(want[i], " ") {
			t.Fatalf("line %d = %q, want %q", i, line, want[i])
		}
	}
	if strings.Contains(block, "0.0.0.0") || strings.Contains(block, "trust") || strings.Contains(block, "md5") {
		t.Fatal("managed block admits a network or weak method")
	}
	if renderNativeHBABlock(nil) != "" {
		t.Fatal("empty block rendered")
	}

	spliced, err := spliceNativeHBA(nativeHBAFixture, block)
	if err != nil {
		t.Fatal(err)
	}
	header := "# PostgreSQL Client Authentication Configuration File\n# TYPE  DATABASE        USER            ADDRESS                 METHOD\n\n"
	if !strings.HasPrefix(spliced, header+block+"local   all             postgres") {
		t.Fatalf("block is not ahead of every rule:\n%s", spliced)
	}
	again, err := spliceNativeHBA(spliced, block)
	if err != nil || again != spliced {
		t.Fatalf("splice is not idempotent: %v\n%s", err, again)
	}
	smaller := renderNativeHBABlock([]nativeHBAEntry{{Role: "app_a", Database: "app_a"}})
	replaced, err := spliceNativeHBA(spliced, smaller)
	if err != nil || strings.Contains(replaced, "app_b") || strings.Count(replaced, nativeHBABegin) != 1 {
		t.Fatalf("replace: %v\n%s", err, replaced)
	}
	removed, err := spliceNativeHBA(replaced, "")
	if err != nil || removed != nativeHBAFixture {
		t.Fatalf("removal must restore the file byte for byte: %v\n%s", err, removed)
	}
	if out, err := spliceNativeHBA("# only comments", block); err != nil || out != "# only comments\n"+block {
		t.Fatalf("comment-only file: %v %q", err, out)
	}
	for _, broken := range []string{nativeHBABegin + "\nhost x\n", nativeHBAEnd + "\n", nativeHBABegin + "\n" + nativeHBABegin + "\n" + nativeHBAEnd + "\n"} {
		if _, err := spliceNativeHBA(broken, block); err == nil {
			t.Fatalf("damaged block accepted: %q", broken)
		}
	}
}

func TestNativeRedisRenderers(t *testing.T) {
	dir := nativeRedisDir("/srv/apps/shop")
	config := renderNativeRedisConfig("shop", dir, "secret", 128)
	want := `# Managed by Līdza Deploy for application shop. Changes are overwritten.
port 0
unixsocket /srv/apps/shop/shared/redis/redis.sock
unixsocketperm 700
protected-mode yes
daemonize no
loglevel notice
logfile ""
databases 16
dir /srv/apps/shop/shared/redis
dbfilename dump.rdb
appendonly yes
appendfilename "appendonly.aof"
appendfsync everysec
maxmemory 128mb
maxmemory-policy noeviction
requirepass secret
`
	if config != want {
		t.Fatalf("config:\n%s", config)
	}
	dropIn := renderNativeRedisDropIn(NativeAppAccount{User: "app-shop", UID: 1001, GID: 1002, Root: "/srv/apps/shop"}, dir, 128)
	if dropIn != "# Managed by Līdza Deploy.\n[Service]\nUser=app-shop\nGroup=1002\nReadWritePaths=/srv/apps/shop/shared/redis\nMemoryMax=320M\nMemorySwapMax=0\n" {
		t.Fatalf("drop-in:\n%s", dropIn)
	}
	unit := renderNativeRedisTemplate()
	for _, line := range []string{"ExecStart=/usr/bin/redis-server /etc/lidza/redis/%i.conf --supervised systemd --daemonize no", "Type=notify", "ProtectSystem=strict", "PrivateTmp=yes", "NoNewPrivileges=yes", "RestrictAddressFamilies=AF_UNIX", "IPAddressDeny=any", "CapabilityBoundingSet="} {
		if !strings.Contains(unit, "\n"+line+"\n") {
			t.Fatalf("template lacks %q", line)
		}
	}
	if strings.Contains(unit, "User=") {
		t.Fatal("template must take its account from the instance drop-in")
	}
	if got := nativeRedisURL(nativeRedisSocket("/srv/apps/shop"), "pw"); got != "unix://:pw@/srv/apps/shop/shared/redis/redis.sock?db=0" {
		t.Fatal(got)
	}
	if err := validNativeRedisPath("/srv/apps/" + strings.Repeat("x", 90)); err == nil {
		t.Fatal("socket path beyond the Unix limit accepted")
	}
	if err := validNativeRedisPath("/srv/my apps/shop"); err == nil {
		t.Fatal("path with whitespace accepted")
	}
	if got := nativeRESP("AUTH", "pw"); got != "*2\r\n$4\r\nAUTH\r\n$2\r\npw\r\n" {
		t.Fatalf("%q", got)
	}
	memory := parseNativeRedisMemory("# Memory\r\nused_memory:1048576\r\nused_memory_human:1.00M\r\nmaxmemory:134217728\r\n")
	if memory.Used != 1048576 || memory.Max != 134217728 {
		t.Fatalf("%+v", memory)
	}
}

func TestNativeHostDataBackupPolicy(t *testing.T) {
	policy, err := nativeHostDataBackupPolicy(NativeDataRequest{})
	if err != nil || policy != (BackupPolicy{Hours: 24, Keep: 1}) {
		t.Fatalf("%+v %v", policy, err)
	}
	policy, err = nativeHostDataBackupPolicy(NativeDataRequest{BackupHours: 1, Offsite: true})
	if err != nil || policy != (BackupPolicy{Hours: 1, Keep: 1, Offsite: true}) {
		t.Fatalf("%+v %v", policy, err)
	}
	if _, err = nativeHostDataBackupPolicy(NativeDataRequest{BackupHours: 6}); err == nil {
		t.Fatal("unsupported interval accepted")
	}
	if id := nativeHostResourceID(strings.Repeat("a", 48)); !idPattern.MatchString(id) {
		t.Fatalf("resource id %q", id)
	}
	if err := (NativeHostDataConfig{PostgresVersion: 14}).validate(); err == nil {
		t.Fatal("unsupported version accepted")
	}
}

// hostDataFixture runs provisioning against a fixture tree with recorded
// host commands: one PostgreSQL 16 cluster on port 5432.
type hostDataFixture struct {
	mu       sync.Mutex
	root     string
	commands []string
	scripts  []string
}

func (f *hostDataFixture) run(_ context.Context, stdin, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, name+" "+strings.Join(args, " "))
	switch name {
	case "/usr/bin/pg_lsclusters":
		return "16  main    5432 online postgres /var/lib/postgresql/16/main /var/log/postgresql/postgresql-16-main.log\n", nil
	case "/usr/sbin/runuser":
		f.scripts = append(f.scripts, stdin)
		switch {
		case strings.HasPrefix(stdin, "SHOW hba_file"):
			return "/etc/postgresql/16/main/pg_hba.conf\n", nil
		case strings.Contains(stdin, "pg_hba_file_rules"):
			return "0\n", nil
		case strings.Contains(stdin, "pg_reload_conf"):
			return "t\n", nil
		}
	}
	return "", nil
}

func newHostDataFixture(t *testing.T) (*Manager, *hostDataFixture) {
	t.Helper()
	f := &hostDataFixture{root: t.TempDir()}
	for name, content := range map[string]string{"usr/bin/pg_lsclusters": "#!/bin/sh\n", "etc/postgresql/16/main/pg_hba.conf": nativeHBAFixture} {
		path := filepath.Join(f.root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0640); err != nil {
			t.Fatal(err)
		}
	}
	previous := nativeHostDataHost
	nativeHostDataHost = func(*Manager) nativeHostData {
		return nativeHostData{root: f.root, run: f.run, digest: func(string) (string, error) { return strings.Repeat("ab", 32), nil }}
	}
	t.Cleanup(func() { nativeHostDataHost = previous })
	m := &Manager{cfg: Config{DataDir: t.TempDir()}, key: bytes.Repeat([]byte{7}, 32), ctx: context.Background(), databaseSlots: make(chan struct{}, 1), data: diskState{Apps: map[string]App{}}}
	return m, f
}

func TestNativeHostDataPostgresLifecycle(t *testing.T) {
	m, f := newHostDataFixture(t)
	account := NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	grant, err := provisionNativeData(ctx, m, "shop", account, NativeDataRequest{Postgres: true})
	if err != nil {
		t.Fatal(err)
	}
	raw := grant.Env["DATABASE_URL"]
	match := regexp.MustCompile(`^postgres://app_shop:([0-9a-f]{64})@127\.0\.0\.1:5432/app_shop\?sslmode=disable$`).FindStringSubmatch(raw)
	if match == nil || len(grant.Databases) != 1 || !strings.HasPrefix(grant.Databases[0], "shop-db") {
		t.Fatalf("grant %v %v", raw, grant.Databases)
	}
	password, resource := match[1], grant.Databases[0]

	m.mu.Lock()
	record := m.data.NativeHostData["shop"]
	binding := m.data.NativeDatabases[resource]
	database := m.data.Databases[resource]
	inUse := m.nativeDatabaseInUseLocked(resource)
	m.mu.Unlock()
	if record.Postgres == nil || !record.Postgres.Ready || strings.Contains(record.Postgres.Password, password) {
		t.Fatalf("record %+v", record.Postgres)
	}
	if opened, err := m.unsealHostSecret(record.Postgres.Password); err != nil || opened != password {
		t.Fatal("sealed password does not open")
	}
	if !inUse || binding.Policy.AppID != "shop" || binding.Policy.Roles[0] != "primary" || database.URL != raw || !database.Ready || database.Mode != "external" || database.Backup.Hours != 24 {
		t.Fatalf("protected resource: %+v %+v in use %v", binding.Policy, database, inUse)
	}
	if source, err := m.nativeDatabaseSource(binding.Policy); err != nil || source != raw {
		t.Fatalf("backup source %v", err)
	}
	for _, script := range f.scripts {
		if strings.Contains(script, password) {
			t.Fatal("plaintext password sent to psql")
		}
	}
	if !hostDataScriptStartsWith(f.scripts, `SELECT 'CREATE ROLE "app_shop"'`) {
		t.Fatalf("provisioning script missing: %q", f.scripts)
	}
	hba, _ := os.ReadFile(filepath.Join(f.root, "etc/postgresql/16/main/pg_hba.conf"))
	if !strings.Contains(string(hba), nativeHBABegin) || !strings.Contains(string(hba), "app_shop") || !strings.HasSuffix(string(hba), nativeHBAFixture[strings.Index(nativeHBAFixture, "local"):]) {
		t.Fatalf("pg_hba.conf:\n%s", hba)
	}

	// Re-provisioning keeps the password, cluster and resource.
	again, err := provisionNativeData(ctx, m, "shop", account, NativeDataRequest{Postgres: true})
	if err != nil || again.Env["DATABASE_URL"] != raw || again.Databases[0] != resource || len(m.data.Databases) != 1 {
		t.Fatalf("re-provision changed the database: %v %v", err, again)
	}

	// Release without a ready resource skips the final dump (no pg_dump here).
	m.mu.Lock()
	database = m.data.Databases[resource]
	database.Ready = false
	m.data.Databases[resource] = database
	m.mu.Unlock()
	if err := releaseNativeData(ctx, m, "shop"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	_, bound := m.data.NativeDatabases[resource]
	retained := m.data.Databases[resource]
	_, recorded := m.data.NativeHostData["shop"]
	m.mu.Unlock()
	if bound || recorded || strings.Contains(retained.URL, password) || retained.URL == "" {
		t.Fatalf("release left state: bound %v recorded %v url %q", bound, recorded, retained.URL)
	}
	if !hostDataScriptStartsWith(f.scripts, `SELECT 'ALTER ROLE "app_shop" NOLOGIN'`) {
		t.Fatal("release did not drop the database")
	}
	hba, _ = os.ReadFile(filepath.Join(f.root, "etc/postgresql/16/main/pg_hba.conf"))
	if string(hba) != nativeHBAFixture {
		t.Fatalf("pg_hba.conf not restored:\n%s", hba)
	}
	if err := releaseNativeData(ctx, m, "shop"); err != nil {
		t.Fatal("second release must be a no-op")
	}
}

func TestNativeHostDataRequestValidation(t *testing.T) {
	m, _ := newHostDataFixture(t)
	account := NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}
	if _, err := provisionNativeData(context.Background(), m, "shop", account, NativeDataRequest{Postgres: true, ExternalDatabaseURL: "postgres://u:p@db.example.com/x?sslmode=require"}); err == nil {
		t.Fatal("host and external database accepted together")
	}
	grant, err := provisionNativeData(context.Background(), m, "shop", account, NativeDataRequest{ExternalDatabaseURL: "postgres://u:p@db.example.com/x?sslmode=require"})
	if err != nil || grant.Env["DATABASE_URL"] == "" || len(grant.Databases) != 0 {
		t.Fatalf("external database: %v %v", grant, err)
	}
	if _, err := provisionNativeData(context.Background(), m, "shop", NativeAppAccount{User: "root", UID: 0, Root: "/srv/apps/shop"}, NativeDataRequest{Redis: true}); err == nil {
		t.Fatal("root account accepted")
	}
	if _, err := provisionNativeData(context.Background(), m, "shop", account, NativeDataRequest{Postgres: true, Offsite: true}); err == nil {
		t.Fatal("off-site backups accepted without storage")
	}
}

func hostDataScriptStartsWith(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
