package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const approvedQueueYML = `# MSP-style multi-database production configuration.
default: &default
  adapter: postgresql
  encoding: unicode
  pool: <%= ENV.fetch("RAILS_MAX_THREADS") { 5 } %>

development:
  <<: *default
  database: app_development

production:
  primary: &primary_production
    <<: *default
    url: <%= ENV["DATABASE_URL"] %>
  queue:
    <<: *primary_production
    migrations_paths: db/queue_migrate
`

// The deployed change of the incident: pool and checkout_timeout only.
const tunedQueueYML = `# MSP-style multi-database production configuration.
default: &default
  adapter: postgresql
  encoding: unicode
  pool: <%= ENV.fetch("RAILS_MAX_THREADS") { 5 } %>

development:
  <<: *default
  database: app_development

production:
  primary: &primary_production
    <<: *default
    url: <%= ENV["DATABASE_URL"] %>
  queue:
    <<: *primary_production
    migrations_paths: db/queue_migrate
    pool: 10
    checkout_timeout: 30
`

func ymlDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// databaseConfigFixture adopts a database whose reviewed database.yml is
// approved, in release one, with only its hash in the configuration.
func databaseConfigFixture(t *testing.T, approved string) (*Manager, NativeDatabasePolicy, string) {
	t.Helper()
	m, _, p, app := nativeDatabaseFixture(t, "postgresql://fixture:fixture-password@127.0.0.1:15435/native_fixture")
	if err := os.WriteFile(filepath.Join(app, "releases/one/config/database.yml"), []byte(approved), 0644); err != nil {
		t.Fatal(err)
	}
	p.ConfigSHA256 = ymlDigest(approved)
	m.cfg.Native.Databases = map[string]NativeDatabasePolicy{"native-fixture": p}
	raw := "postgresql://fixture:fixture-password@127.0.0.1:15435/native_fixture"
	m.data.NativeDatabases = map[string]NativeDatabaseBinding{"native-fixture": {Policy: p, Adopted: time.Now()}}
	m.data.Databases = map[string]Database{"native-fixture": {AppID: "native-fixture", Mode: "external", URL: raw, Ready: true, Backup: p.Backup}}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	return m, p, app
}

// publishRelease makes a release with the given database.yml current.
func publishRelease(t *testing.T, app, id, yml string) {
	t.Helper()
	dir := filepath.Join(app, "releases", id)
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config/database.yml"), []byte(yml), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("source 'https://rubygems.org'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(app, "current.next")
	os.Remove(link)
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(link, filepath.Join(app, "current")); err != nil {
		t.Fatal(err)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &logs
}

func TestDatabaseConfigPoolChangeIsReapprovedAndPersists(t *testing.T) {
	m, p, app := databaseConfigFixture(t, approvedQueueYML)
	logs := captureLog(t)
	// The failed backup of the incident, recorded before this check existed.
	d := m.data.Databases["native-fixture"]
	d.Error = "pre-deployment backup failed: native database source changed; existing backup retained"
	m.data.Databases["native-fixture"] = d
	publishRelease(t, app, "two", tunedQueueYML)

	// The proactive check after the deployment approves the file.
	if lines := m.checkNativeDatabaseConfigs("my-rails"); len(lines) != 0 {
		t.Fatalf("pool/timeout change refused: %q", lines)
	}
	approval, ok := m.data.DatabaseConfigApprovals["my-rails"]
	if !ok || approval.SHA256 != ymlDigest(tunedQueueYML) || approval.PreviousSHA256 != p.ConfigSHA256 || approval.Release != "two" ||
		!slices.Equal(approval.Changed, []string{"queue.checkout_timeout", "queue.pool"}) || approval.Config != tunedQueueYML ||
		!slices.Contains(approval.Supersedes, p.ConfigSHA256) || approval.Approved.IsZero() {
		t.Fatalf("approval not recorded: %+v", approval)
	}
	if !strings.Contains(logs.String(), "database.yml for MyRails changed only queue.checkout_timeout, queue.pool: re-approved") {
		t.Fatalf("re-approval not logged: %s", logs.String())
	}
	if m.data.Databases["native-fixture"].Error != "" {
		t.Fatal("stale source error was kept", m.data.Databases["native-fixture"].Error)
	}
	if len(m.databaseConfigHealth()) != 0 || len(m.databaseViews()[0].Warnings) != 0 {
		t.Fatal("approved file still warned")
	}

	// A restart keeps the approval; the old release is no longer needed.
	if err := os.RemoveAll(filepath.Join(app, "releases/one")); err != nil {
		t.Fatal(err)
	}
	reloaded := &Manager{cfg: m.cfg, key: m.key}
	if err := reloaded.load(); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	if raw, err := reloaded.nativeDatabaseSource(p); err != nil || raw != d.URL {
		t.Fatal("approval lost across restart", err)
	}
	if strings.Contains(logs.String(), "re-approved") {
		t.Fatal("approved file re-approved again")
	}

	// A further tuning change compares with the recorded copy.
	publishRelease(t, app, "three", strings.Replace(tunedQueueYML, "pool: 10", "pool: 12", 1))
	if _, err := reloaded.nativeDatabaseSource(p); err != nil {
		t.Fatal("second tuning change refused", err)
	}
	next := reloaded.data.DatabaseConfigApprovals["my-rails"]
	if next.PreviousSHA256 != approval.SHA256 || !slices.Equal(next.Changed, []string{"queue.pool"}) ||
		!slices.Contains(next.Supersedes, p.ConfigSHA256) || !slices.Contains(next.Supersedes, approval.SHA256) {
		t.Fatalf("successive approval: %+v", next)
	}
}

func TestDatabaseConfigSnapshotRecordedWhenFileMatches(t *testing.T) {
	m, p, app := databaseConfigFixture(t, approvedQueueYML)
	if _, err := m.nativeDatabaseSource(p); err != nil {
		t.Fatal(err)
	}
	snapshot := m.data.DatabaseConfigApprovals["my-rails"]
	if snapshot.SHA256 != p.ConfigSHA256 || snapshot.Config != approvedQueueYML || snapshot.PreviousSHA256 != "" {
		t.Fatalf("approved copy not kept: %+v", snapshot)
	}
	// With the copy kept, the reference survives release cleanup.
	publishRelease(t, app, "two", tunedQueueYML)
	os.RemoveAll(filepath.Join(app, "releases/one"))
	if _, err := m.nativeDatabaseSource(p); err != nil {
		t.Fatal("tuning change refused without the old release", err)
	}
}

func TestDatabaseConfigIdentityChangeIsRefusedAndWarned(t *testing.T) {
	m, p, app := databaseConfigFixture(t, approvedQueueYML)
	changed := strings.Replace(tunedQueueYML, "    migrations_paths: db/queue_migrate\n", "    url: <%= ENV[\"QUEUE_DATABASE_URL\"] %>\n    migrations_paths: db/queue_migrate\n", 1)
	publishRelease(t, app, "two", changed)
	want := "MyRails: database.yml now reads the queue database from QUEUE_DATABASE_URL instead of DATABASE_URL; review needed"

	lines := m.checkNativeDatabaseConfigs("")
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("refusal = %q", lines)
	}
	if _, ok := m.data.DatabaseConfigApprovals["my-rails"]; ok && m.data.DatabaseConfigApprovals["my-rails"].SHA256 == ymlDigest(changed) {
		t.Fatal("identity change approved")
	}
	views := m.databaseViews()
	if len(views) != 1 || !slices.Contains(views[0].Warnings, want) {
		t.Fatalf("database warning missing: %+v", views)
	}
	health := m.databaseConfigHealth()
	if len(health) != 1 || !strings.Contains(health[0], want) || !strings.Contains(health[0], "Backups of native-fixture fail") {
		t.Fatalf("server warning = %q", health)
	}

	// The backup reports the real reason, not "source changed".
	_, err := m.nativeDatabaseDumpPolicy(context.Background(), m.data.Databases["native-fixture"])
	if err == nil || !strings.Contains(err.Error(), want) || !strings.HasSuffix(err.Error(), "; existing backup retained") || strings.Contains(err.Error(), "source changed") {
		t.Fatalf("dump error = %v", err)
	}

	// Rolling back to the approved file clears the warning.
	publishRelease(t, app, "three", approvedQueueYML)
	if lines := m.checkNativeDatabaseConfigs(""); len(lines) != 0 || len(m.databaseConfigHealth()) != 0 || len(m.databaseViews()[0].Warnings) != 0 {
		t.Fatalf("warning kept after the approved file returned: %q", lines)
	}
	_ = p
}

func TestDatabaseConfigConnectionDriftIsNamed(t *testing.T) {
	m, _, _ := databaseConfigFixture(t, approvedQueueYML)
	d := m.data.Databases["native-fixture"]
	d.URL = "postgresql://fixture:old-password@127.0.0.1:15435/native_fixture"
	_, err := m.nativeDatabaseDumpPolicy(context.Background(), d)
	if err == nil || err.Error() != "DATABASE_URL now holds a different connection than the adopted database; existing backup retained" {
		t.Fatalf("drift error = %v", err)
	}
}

func TestDatabaseConfigRefusesWithoutApprovedCopy(t *testing.T) {
	m, p, app := databaseConfigFixture(t, approvedQueueYML)
	publishRelease(t, app, "two", tunedQueueYML)
	os.RemoveAll(filepath.Join(app, "releases/one"))
	_, err := m.nativeDatabaseSource(p)
	if err == nil || !strings.Contains(err.Error(), "no copy of the approved file remains") {
		t.Fatal(err)
	}
}

func TestDatabaseConfigSQLitePathChangeRefused(t *testing.T) {
	approved := "default: &default\n  adapter: sqlite3\n  pool: 5\n  timeout: 5000\n\nproduction:\n  <<: *default\n  database: storage/production.sqlite3\n"
	m, p, app := databaseConfigFixture(t, approved)
	p.SQLitePath, p.Host, p.Port, p.PostgresMajor, p.EnvironmentKey = "storage/production.sqlite3", "", "", 0, ""
	publishRelease(t, app, "two", strings.Replace(approved, "storage/production.sqlite3", "storage/other.sqlite3", 1))
	_, err := m.nativeDatabaseSource(p)
	if err == nil || err.Error() != "MyRails: database.yml changed the production database's SQLite path from storage/production.sqlite3 to storage/other.sqlite3; review needed" {
		t.Fatal(err)
	}
}

func TestCompareRailsDatabaseConfig(t *testing.T) {
	single := "production:\n  adapter: postgresql\n  host: 127.0.0.1\n  port: 5432\n  database: app_production\n  username: app\n  password: <%= ENV['DB_PASSWORD'] %>\n  pool: 5\n"
	cases := []struct {
		name, approved, current string
		changed                 []string
		refusal                 string
	}{
		{name: "pool and variables", approved: single, current: strings.Replace(single, "pool: 5\n", "pool: 10\n  prepared_statements: false\n  variables:\n    statement_timeout: 5000\n", 1),
			changed: []string{"pool", "prepared_statements", "variables.statement_timeout"}},
		{name: "comment only", approved: single, current: "# tuned\n" + single, changed: []string{"comments, formatting or other environments"}},
		{name: "quoting is not a change", approved: single, current: strings.Replace(single, "host: 127.0.0.1", `host: "127.0.0.1"`, 1), changed: []string{"comments, formatting or other environments"}},
		{name: "other environment", approved: single, current: single + "test:\n  database: app_test\n", changed: []string{"comments, formatting or other environments"}},
		{name: "host", approved: single, current: strings.Replace(single, "127.0.0.1", "10.0.0.9", 1),
			refusal: "database.yml changed the production database's host from 127.0.0.1 to 10.0.0.9"},
		{name: "database name", approved: single, current: strings.Replace(single, "app_production", "app_other", 1),
			refusal: "database.yml changed the production database's database from app_production to app_other"},
		{name: "username", approved: single, current: strings.Replace(single, "username: app", "username: <%= ENV['DB_USER'] %>", 1),
			refusal: `database.yml changed the production database's username from app to ENV["DB_USER"]`},
		{name: "Ruby in identity", approved: single, current: strings.Replace(single, "host: 127.0.0.1", "host: <%= Rails.application.credentials.db_host %>", 1),
			refusal: "database.yml computes connection settings with Ruby (the production database's host)"},
		{name: "control code", approved: single, current: "<% if ENV['X'] %>\n" + single + "<% end %>\n",
			refusal: "database.yml computes connection settings with Ruby (ERB control code or a computed key)"},
		{name: "Ruby outside identity", approved: single, current: strings.Replace(single, "pool: 5", "pool: <%= Integer(ENV.fetch('THREADS', 5)) * 2 %>", 1), changed: []string{"pool"}},
		{name: "fetch default kept", approved: "production:\n  url: <%= ENV.fetch(\"DATABASE_URL\") { \"postgres://localhost/app\" } %>\n",
			current: "production:\n  url: <%= ENV.fetch(\"DATABASE_URL\") { \"postgres://localhost/app\" } %>\n  pool: 3\n", changed: []string{"pool"}},
		{name: "fetch default changed", approved: "production:\n  url: <%= ENV.fetch(\"DATABASE_URL\", \"postgres://localhost/app\") %>\n",
			current: "production:\n  url: <%= ENV.fetch(\"DATABASE_URL\", \"postgres://localhost/other\") %>\n", refusal: `database.yml changed the production database's url from ENV.fetch("DATABASE_URL", "postgres://localhost/app") to ENV.fetch("DATABASE_URL", "postgres://localhost/other")`},
		{name: "fetch Ruby default", approved: "production:\n  url: <%= ENV['DATABASE_URL'] %>\n",
			current: "production:\n  url: <%= ENV.fetch('DATABASE_URL') { Rails.root.join('x').to_s } %>\n", refusal: "database.yml computes connection settings with Ruby"},
		{name: "literal url password hidden", approved: "production:\n  url: postgres://app:secret@db/app\n", current: "production:\n  url: postgres://app:secret@db2/app\n",
			refusal: "database.yml changed the production database's url from postgres://app:xxxxx@db/app to postgres://app:xxxxx@db2/app"},
		{name: "database added", approved: "production:\n  primary:\n    url: <%= ENV['DATABASE_URL'] %>\n",
			current: "production:\n  primary:\n    url: <%= ENV['DATABASE_URL'] %>\n  cache:\n    url: <%= ENV['CACHE_DATABASE_URL'] %>\n", refusal: "database.yml now defines the cache database"},
		{name: "block scalar", approved: single, current: single + "  host_note: |\n    a\n", refusal: "database.yml could not be read"},
		{name: "merge after own key", approved: "base: &base\n  host: a\nproduction:\n  host: b\n  <<: *base\n", current: "base: &base\n  host: a\nproduction:\n  <<: *base\n  host: b\n",
			refusal: "database.yml changed the production database's host from a to b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			changed, err := compareRailsDatabaseConfig([]byte(c.approved), []byte(c.current))
			if c.refusal != "" {
				if err == nil || !strings.HasPrefix(err.Error(), c.refusal) {
					t.Fatalf("refusal = %v, want %q", err, c.refusal)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatal("password in message")
				}
				return
			}
			if err != nil || !slices.Equal(changed, c.changed) {
				t.Fatalf("changed = %q, %v; want %q", changed, err, c.changed)
			}
		})
	}
}

func TestDatabaseConfigTickChecksHourly(t *testing.T) {
	m, _, app := databaseConfigFixture(t, approvedQueueYML)
	logs := captureLog(t)
	now := time.Now().UTC()
	publishRelease(t, app, "two", tunedQueueYML)
	m.databaseConfigTick(now)
	if m.data.DatabaseConfigApprovals["my-rails"].SHA256 != ymlDigest(tunedQueueYML) || !strings.Contains(logs.String(), "re-approved") {
		t.Fatal("periodic check did not re-approve", logs.String())
	}
	publishRelease(t, app, "three", strings.Replace(approvedQueueYML, "127.0.0.1", "x", 1)+"  cache:\n    url: <%= ENV['CACHE_URL'] %>\n")
	m.databaseConfigTick(now.Add(time.Minute))
	if len(m.databaseConfigHealth()) != 0 {
		t.Fatal("checked again within the hour")
	}
	m.databaseConfigTick(now.Add(time.Hour))
	if health := m.databaseConfigHealth(); len(health) != 1 || !strings.Contains(health[0], "database.yml now defines the cache database") {
		t.Fatalf("hourly check: %q", health)
	}
}
