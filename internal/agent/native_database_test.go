package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agim/lidza/packs/storage"
	"github.com/jackc/pgx/v5"
)

// nativeRailsAppFixture registers a Rails application with two user units and
// a current release that has Rails metadata files.
func nativeRailsAppFixture(t *testing.T) (*Manager, NativeApplication, string) {
	t.Helper()
	m, app := nativeFixture(t)
	files := map[string]string{"Gemfile.lock": "GEM\n  specs:\n    rails (8.0.5)\n    puma (6.6.0)\n\nBUNDLED WITH\n   2.6.2\n", ".ruby-version": "3.3.5\n", "bin/rails": "#!/usr/bin/env ruby\n", "config/application.rb": "require 'rails/all'\n"}
	for name, value := range files {
		path := filepath.Join(app, "current", name)
		os.MkdirAll(filepath.Dir(path), 0755)
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("SECRET='production fixture credential'\nDATABASE_URL=postgres://fixture:password@127.0.0.1:5432/production\n"), 0600)
	a, err := m.nativeRegistration(NativeApplication{ID: "my-rails", Name: "MyRails", Runtime: "rails", Domain: "rails.example.com", Repository: "acme/rails", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	return m, a, app
}

func nativeDatabaseFixture(t *testing.T, raw string) (*Manager, NativeApplication, NativeDatabasePolicy, string) {
	t.Helper()
	m, a, app := nativeRailsAppFixture(t)
	data, _ := json.Marshal(m.data)
	cfg := m.cfg
	native := *cfg.Native
	cfg.Native = &native
	cfg.DataDir = t.TempDir()
	detached := &Manager{cfg: cfg, key: bytes.Clone(m.key), ctx: context.Background(), databaseSlots: make(chan struct{}, 1), nativeCommand: func(context.Context, []string) (string, error) {
		t.Fatal("database operation attempted a Rails service command")
		return "", nil
	}}
	if err := json.Unmarshal(data, &detached.data); err != nil {
		t.Fatal(err)
	}
	m = detached
	metadata := []byte("production:\n  primary:\n    url: <%= ENV['DATABASE_URL'] %>\n")
	if err := os.WriteFile(filepath.Join(app, "current/config/database.yml"), metadata, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("SECRET='fixture credential'\nDATABASE_URL='"+raw+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	sum := sha256.Sum256(metadata)
	p := NativeDatabasePolicy{Enabled: true, AppID: a.ID, EnvironmentKey: "DATABASE_URL", Roles: []string{"primary", "queue", "cache", "cable"}, Host: u.Hostname(), Port: port, ConfigSHA256: hex.EncodeToString(sum[:]), PostgresMajor: 18, DumpSHA256: strings.Repeat("a", 64), Backup: BackupPolicy{Hours: 24, Keep: 1}}
	m.cfg.Native.Databases = map[string]NativeDatabasePolicy{"native-fixture": p}
	return m, a, p, app
}

func TestNativeDatabaseLegacyRuntimeRetainsDatabaseSourceAndSchedulerWithoutChangingRegistration(t *testing.T) {
	for _, runtime := range []string{"", "rails", "astro", "other"} {
		t.Run("runtime="+runtime, func(t *testing.T) {
			m, app, policy, path := nativeDatabaseFixture(t, "postgresql://fixture:fixture-password@127.0.0.1:15435/native_fixture")
			app.Runtime = runtime
			m.data.NativeApps[app.ID] = app
			before, _ := json.Marshal(m.data.NativeApps)
			beforeEnvironment, _ := os.ReadFile(filepath.Join(path, ".asdf-vars"))
			beforeCurrent, _ := os.Readlink(filepath.Join(path, "current"))
			connection, err := m.nativeDatabaseSource(policy)
			allowed := runtime == "" || runtime == "rails"
			if allowed != (err == nil) {
				t.Fatal("database source runtime classification differs from legacy Rails inventory", err)
			}
			m.data.NativeDatabases = map[string]NativeDatabaseBinding{"native-fixture": {Policy: policy}}
			m.data.Databases = map[string]Database{"native-fixture": {AppID: "native-fixture", Mode: "external", URL: connection, Ready: true, Backup: policy.Backup}}
			if m.nativeDatabaseInUseLocked("native-fixture") != allowed {
				t.Fatal("scheduler runtime classification differs from database adoption")
			}
			if err = m.save(); err != nil {
				t.Fatal(err)
			}
			reloaded := &Manager{cfg: m.cfg, key: m.key}
			if err = reloaded.load(); err != nil || reloaded.nativeDatabaseInUseLocked("native-fixture") != allowed {
				t.Fatal("legacy database ownership changed after reload", err)
			}
			after, _ := json.Marshal(reloaded.data.NativeApps)
			afterEnvironment, _ := os.ReadFile(filepath.Join(path, ".asdf-vars"))
			afterCurrent, _ := os.Readlink(filepath.Join(path, "current"))
			if !bytes.Equal(before, after) || !bytes.Equal(beforeEnvironment, afterEnvironment) || beforeCurrent != afterCurrent {
				t.Fatal("database classification changed registration, environment or release")
			}
		})
	}
}

func TestNativeDatabasePolicySourceAndSchedulerPreserveInactiveApps(t *testing.T) {
	m, a, p, app := nativeDatabaseFixture(t, "postgresql://fixture:fixture-password@127.0.0.1:15435/native_fixture")
	if err := validateNativeDatabasePolicy("native-fixture", p); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(app, ".asdf-vars"))
	current, _ := os.Readlink(filepath.Join(app, "current"))
	raw, err := m.nativeDatabaseSource(p)
	if err != nil || !strings.Contains(raw, "fixture-password") {
		t.Fatal("literal source rejected", err)
	}
	m.data.NativeDatabases = map[string]NativeDatabaseBinding{"native-fixture": {Policy: p, Adopted: time.Now()}}
	m.data.Databases = map[string]Database{"native-fixture": {AppID: "native-fixture", Mode: "external", URL: raw, Ready: true, Backup: p.Backup, NextBackup: time.Now()}}
	views := m.databaseViews()
	if len(views) != 1 || views[0].Retained || views[0].Backup.Hours != 24 || len(views[0].Attachments) != 1 || views[0].Attachments[0].AppID != a.ID {
		t.Fatal("common scheduler cannot see adopted native resource", views)
	}
	if m.primaryDatabaseID(a.ID) != "native-fixture" {
		t.Fatal("native primary resource was not resolved")
	}
	settings, err := m.nativeSettings(a.ID)
	if err != nil || settings.Bindings["DATABASE_URL"] != "native-fixture" {
		t.Fatal("settings lost native binding", err)
	}
	if err = m.save(); err != nil {
		t.Fatal(err)
	}
	sealed, _ := os.ReadFile(m.path())
	if bytes.Contains(sealed, []byte("fixture-password")) || bytes.Contains(sealed, []byte("native_databases")) {
		t.Fatal("native connection was not sealed")
	}
	restarted := &Manager{cfg: m.cfg, key: m.key}
	if err = restarted.load(); err != nil || !restarted.nativeDatabaseInUseLocked("native-fixture") {
		t.Fatal("native resource lost on reload", err)
	}
	delete(restarted.data.NativeApps, a.ID)
	if !restarted.databaseViews()[0].Retained {
		t.Fatal("unregistered native app remains scheduled")
	}
	q := m.cfg.Native.Databases["native-fixture"]
	q.Enabled = false
	m.cfg.Native.Databases["native-fixture"] = q
	if !m.databaseViews()[0].Retained {
		t.Fatal("disabled policy remains scheduled")
	}
	if _, err = m.nativeDatabaseDumpPolicy(context.Background(), m.data.Databases["native-fixture"]); err == nil {
		t.Fatal("disabled binding fell back to a Docker dump")
	}
	after, _ := os.ReadFile(filepath.Join(app, ".asdf-vars"))
	link, _ := os.Readlink(filepath.Join(app, "current"))
	if !bytes.Equal(before, after) || current != link {
		t.Fatal("database metadata changed app environment/release")
	}
}

func TestNativeDatabaseRejectsUnreviewedSourcesRolesAndAdoption(t *testing.T) {
	for _, scenario := range []string{"no-policy", "wrong-host", "wrong-port", "config-drift", "shell-expression", "invalid-role", "duplicate-role", "bad-client", "ingress-role"} {
		t.Run(scenario, func(t *testing.T) {
			m, _, p, app := nativeDatabaseFixture(t, "postgresql://fixture:fixture-password@127.0.0.1:15435/native_fixture")
			switch scenario {
			case "no-policy":
				delete(m.cfg.Native.Databases, "native-fixture")
			case "wrong-host":
				p.Host = "other.example"
			case "wrong-port":
				p.Port = "15436"
			case "config-drift":
				os.WriteFile(filepath.Join(app, "current/config/database.yml"), []byte("changed"), 0644)
			case "shell-expression":
				os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("DATABASE_URL=$(touch forbidden)"), 0600)
			case "invalid-role":
				p.Roles = []string{"other"}
			case "duplicate-role":
				p.Roles = []string{"primary", "primary"}
			case "bad-client":
				p.PostgresMajor = 15
			case "ingress-role":
				m.cfg.IngressOnly = true
			}
			if scenario != "no-policy" {
				m.cfg.Native.Databases["native-fixture"] = p
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := m.adoptNativeDatabase(ctx, "native-fixture"); err == nil {
				t.Fatal("unreviewed native adoption accepted")
			}
			if len(m.data.Databases) != 0 || len(m.data.NativeDatabases) != 0 {
				t.Fatal("failed adoption left a database/binding")
			}
		})
	}
	for _, raw := range []string{"postgresql://u:p@127.0.0.1/db?options=-c%20role%3Dother", "postgresql://u:p@127.0.0.1/db?service=other", "postgresql://u:p@127.0.0.1/db?host=other", "postgresql://u:p@127.0.0.1/db?passfile=/tmp/secret"} {
		if _, err := urlForNativeDump(raw); err == nil {
			t.Fatal("libpq override accepted")
		}
	}
}

func TestNativeDatabaseOffsiteChecksRejectPublicAndCorruptObjects(t *testing.T) {
	for _, scenario := range []string{"private", "public", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			var mu sync.Mutex
			objects := map[string][]byte{}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				data, exists := objects[r.URL.Path]
				authorized := r.Header.Get("Authorization") != "" || r.URL.Query().Get("X-Amz-Signature") != ""
				if !authorized {
					if scenario == "public" && exists {
						w.Header().Set("Content-Length", strconv.Itoa(len(data)))
						w.WriteHeader(200)
					} else {
						w.WriteHeader(403)
					}
					return
				}
				switch r.Method {
				case "PUT":
					objects[r.URL.Path], _ = io.ReadAll(r.Body)
				case "DELETE":
					delete(objects, r.URL.Path)
				case "HEAD":
					if !exists {
						w.WriteHeader(404)
					} else {
						w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					}
				case "GET":
					if !exists {
						w.WriteHeader(404)
					} else {
						if scenario == "corrupt" {
							data = bytes.Repeat([]byte("x"), len(data))
						}
						w.Header().Set("Content-Length", strconv.Itoa(len(data)))
						w.Write(data)
					}
				}
			}))
			defer remote.Close()
			cfg := storage.Config{Provider: "s3", Endpoint: remote.URL, Bucket: "fixture", Prefix: "saved", AccessKey: "fixture", SecretKey: "fixture-secret", Region: "test", Timeout: time.Second}
			key := "databases/native-fixture/proof.dump"
			if err := nativeBackupPrivacy(context.Background(), cfg, key); scenario == "public" {
				if err == nil {
					t.Fatal("public object admitted")
				}
				return
			} else if err != nil {
				t.Fatal(err)
			}
			archive := []byte("fixture archive")
			hash := sha256.Sum256(archive)
			record := BackupRecord{Size: int64(len(archive)), SHA256: hex.EncodeToString(hash[:])}
			mu.Lock()
			objects["/fixture/saved/"+key] = archive
			mu.Unlock()
			err := verifyNativeOffsiteBackup(context.Background(), cfg, key, record)
			if scenario == "private" && err != nil || scenario == "corrupt" && err == nil {
				t.Fatal("offsite verification verdict", err)
			}
		})
	}
}

func TestNativeDatabaseRealAdoptionDumpAndCancellation(t *testing.T) {
	raw := os.Getenv("TEST_NATIVE_PG_URL")
	if raw == "" {
		t.Skip("set TEST_NATIVE_PG_URL for disposable local PostgreSQL18 acceptance")
	}
	u, _ := url.Parse(raw)
	if u == nil || u.Hostname() != "127.0.0.1" || u.Port() != "15435" || u.Path != "/native_fixture" || os.Geteuid() != 0 {
		t.Fatal("requires named local root/disposable fixture only")
	}
	binary := os.Getenv("TEST_NATIVE_PG_AGENT_BINARY")
	if !filepath.IsAbs(binary) {
		t.Fatal("pinned local agent executable required")
	}
	m, a, p, app := nativeDatabaseFixture(t, raw)
	// Exercise the actual host's pre-multi-runtime registration through the
	// real adoption/dump/off-site gate, without rewriting that registration.
	a.Runtime = ""
	m.data.NativeApps[a.ID] = a
	m.nativePGExecutable = binary
	digest, err := nativeStaticFileDigest("/usr/lib/postgresql/18/bin/pg_dump")
	if err != nil {
		t.Fatal(err)
	}
	p.DumpSHA256 = hex.EncodeToString(digest[:])
	m.cfg.Native.Databases["native-fixture"] = p
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	before, _ := os.ReadFile(filepath.Join(app, ".asdf-vars"))
	if err = m.adoptNativeDatabase(ctx, "native-fixture"); err != nil {
		t.Fatal("adoption", err)
	}
	if err = m.adoptNativeDatabase(ctx, "native-fixture"); err != nil {
		t.Fatal("idempotent adoption", err)
	}
	if m.data.Databases["native-fixture"].URL != raw || m.primaryDatabaseID(a.ID) != "native-fixture" {
		t.Fatal("native adoption replaced connection")
	}
	if m.data.NativeApps[a.ID].Runtime != "" || m.databaseViews()[0].Retained {
		t.Fatal("legacy adoption rewrote registration or lost backup scheduling")
	}
	record, err := m.dumpDatabase(ctx, m.data.Databases["native-fixture"], nil)
	if err != nil || record.Size == 0 || record.SHA256 == "" {
		t.Fatal("actual systemd-scoped native archive", err)
	}
	path := filepath.Join(m.backupDir("native-fixture"), record.ID+".dump")
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(data, []byte("PGDMP")) {
		t.Fatal("invalid native custom archive", err)
	}
	listing, err := exec.Command("/usr/lib/postgresql/18/bin/pg_restore", "--list", path).Output()
	if err != nil || !bytes.Contains(listing, []byte("fixture_rows")) {
		t.Fatal("actual archive omitted the fixture table", err)
	}
	restore := exec.Command("/usr/lib/postgresql/18/bin/pg_restore", "-h", "127.0.0.1", "-p", "15435", "-U", "proof", "-d", "native_fixture_restore", "--no-owner", "--no-acl", "--exit-on-error", "--single-transaction", path)
	restore.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "PGPASSFILE=/nonexistent"}
	if output, err := restore.CombinedOutput(); err != nil {
		t.Fatal("disposable full restore", err, string(output))
	}
	restored, err := pgx.Connect(ctx, "postgresql://proof@127.0.0.1:15435/native_fixture_restore")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(context.Background())
	var restoredCount int
	if err = restored.QueryRow(ctx, "SELECT count(*) FROM fixture_rows").Scan(&restoredCount); err != nil || restoredCount != 3 {
		t.Fatal("restored data differs", restoredCount, err)
	}
	failed, _, fp, _ := nativeDatabaseFixture(t, raw)
	fp.DumpSHA256 = p.DumpSHA256
	failed.cfg.Native.Databases["native-fixture"] = fp
	blocked := filepath.Join(t.TempDir(), "not-directory")
	os.WriteFile(blocked, []byte("blocked"), 0600)
	failed.cfg.DataDir = blocked
	if err = failed.adoptNativeDatabase(ctx, "native-fixture"); err == nil || len(failed.data.NativeDatabases) != 0 || len(failed.data.Databases) != 0 {
		t.Fatal("failed persistence left an adoption", err)
	}
	// Exercise the real pre-deployment gate, including the same shared resource
	// for all four Rails roles and complete private off-site byte verification.
	var bucketMu sync.Mutex
	objects := map[string][]byte{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucketMu.Lock()
		defer bucketMu.Unlock()
		data, exists := objects[r.URL.Path]
		if r.Header.Get("Authorization") == "" && r.URL.Query().Get("X-Amz-Signature") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.Method {
		case "PUT":
			objects[r.URL.Path], _ = io.ReadAll(r.Body)
		case "DELETE":
			delete(objects, r.URL.Path)
		case "HEAD":
			if !exists {
				w.WriteHeader(404)
			} else {
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			}
		case "GET":
			if !exists {
				w.WriteHeader(404)
			} else {
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.Write(data)
			}
		}
	}))
	defer remote.Close()
	m.data.BackupStorage = &storage.Config{Provider: "s3", Endpoint: remote.URL, Bucket: "fixture", Prefix: "saved", Region: "test", AccessKey: "fixture", SecretKey: "fixture-secret", Timeout: time.Second}
	d := m.data.Databases["native-fixture"]
	d.Backup.Offsite = true
	m.data.Databases[d.AppID] = d
	m.nativeOperations = map[string]bool{a.ID: true}
	proofs, err := m.backupBeforeNativeRailsRelease(ctx, a.ID, []string{"primary", "queue", "cache", "cable"})
	if err != nil || len(proofs) != 1 || proofs[0].Resource != "native-fixture" || proofs[0].Backup == "" || proofs[0].SHA256 == "" || proofs[0].Verified.Before(proofs[0].Created) {
		t.Fatal("native release database gate", proofs, err)
	}
	delete(m.nativeOperations, a.ID)
	// Retained lock models an agent crash while the trusted child still dumps.
	lockPath := filepath.Join(m.cfg.DataDir, ".lidza-native-pg-locks/native-fixture.lock")
	lock, err := os.OpenFile(lockPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	spec := nativePGDumpInput{URL: raw, Major: 18, ToolSHA256: p.DumpSHA256, Lock: lockPath}
	payload, _ := json.Marshal(spec)
	if err = RunNativePGDump(ctx, bytes.NewReader(payload), io.Discard); err == nil {
		t.Fatal("concurrent dump ignored retained lock")
	}
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	lock.Close()
	// A real exclusive relation lock must cause bounded cancellation without
	// changing its source table or leaving an active native dump process.
	conn, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "LOCK TABLE fixture_rows IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 600*time.Millisecond)
	err = m.dumpNativeDatabase(short, m.data.Databases["native-fixture"], p, io.Discard)
	stop()
	if err == nil || short.Err() != context.DeadlineExceeded {
		t.Fatal("native dump did not cancel under lock", err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for attempts := 0; attempts < 30; attempts++ {
		var active int
		if err = conn.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE application_name='lidza-native-backup'").Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active == 0 {
			break
		}
		if attempts == 29 {
			t.Fatal("cancelled native dump connection remains active")
		}
		time.Sleep(100 * time.Millisecond)
	}
	var count int
	if err = conn.QueryRow(ctx, "SELECT count(*) FROM fixture_rows").Scan(&count); err != nil || count != 3 {
		t.Fatal("native source changed", count, err)
	}
	after, _ := os.ReadFile(filepath.Join(app, ".asdf-vars"))
	if !bytes.Equal(before, after) {
		t.Fatal("native backup changed app environment")
	}
	if err = os.WriteFile(filepath.Join(app, "current/config/database.yml"), []byte("drift"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = m.dumpDatabase(ctx, m.data.Databases["native-fixture"], nil); err == nil {
		t.Fatal("source drift accepted before dump")
	}
}

func TestNativeDatabaseReleaseGateRefusesMissingRolesAndReservation(t *testing.T) {
	m, a, p, _ := nativeDatabaseFixture(t, "postgresql://fixture:password@127.0.0.1:15435/native_fixture")
	p.Roles = []string{"primary"}
	m.cfg.Native.Databases["native-fixture"] = p
	m.data.NativeDatabases = map[string]NativeDatabaseBinding{"native-fixture": {Policy: p}}
	m.data.Databases = map[string]Database{"native-fixture": {AppID: "native-fixture", Mode: "external", Ready: true, Backup: BackupPolicy{Keep: 1, Offsite: true}}}
	m.data.BackupStorage = &storage.Config{}
	if _, err := m.backupBeforeNativeRailsRelease(context.Background(), a.ID, []string{"primary"}); err == nil {
		t.Fatal("unreserved native release backed up")
	}
	m.nativeOperations = map[string]bool{a.ID: true}
	for _, roles := range [][]string{nil, {"queue"}, {"primary", "primary"}, {"primary", "queue"}} {
		if _, err := m.backupBeforeNativeRailsRelease(context.Background(), a.ID, roles); err == nil {
			t.Fatal("unprotected/invalid roles accepted", roles)
		}
	}
	d := m.data.Databases["native-fixture"]
	d.Backup.Offsite = false
	m.data.Databases[d.AppID] = d
	if _, err := m.backupBeforeNativeRailsRelease(context.Background(), a.ID, []string{"primary"}); err == nil {
		t.Fatal("native release bypassed mandatory offsite copy")
	}
	// A database the agent provisioned for an application it created follows
	// that application's choice: without off-site copies (and without
	// configured storage) the gate proceeds to take a verified local backup.
	m.data.BackupStorage = nil
	m.data.NativeHostData = map[string]NativeHostDataRecord{a.ID: {}}
	if _, err := m.backupBeforeNativeRailsRelease(context.Background(), a.ID, []string{"primary"}); err != nil && strings.Contains(err.Error(), "off-site") {
		t.Fatal("provisioned database without off-site copies still required off-site storage:", err)
	}
}
