package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza/packs/storage"
)

// The 2026-11-08 incident: an operator re-pinned dump_sha256 of the SQLite
// databases by hand to the verified new /usr/bin/sqlite3, while the stored
// bindings kept the old pin. The databases must stay in use (so the release
// gate covers their roles), and the binding must converge and persist.
func TestNativeDatabaseBindingFollowsAHandRepinnedToolHash(t *testing.T) {
	host := newBackupToolHost(t)
	old := backupToolDigest("sqlite3 3.45.1-1ubuntu2.8")
	current := host.install("/usr/bin/sqlite3", "sqlite3", "3.45.1-1ubuntu2.9", "sqlite3 3.45.1-1ubuntu2.9")
	app := "rails-bookshop"
	primary := NativeDatabasePolicy{Enabled: true, AppID: app, Roles: []string{"primary"}, Host: "127.0.0.1", Port: "5432", EnvironmentKey: "DATABASE_URL",
		ConfigSHA256: strings.Repeat("a", 64), PostgresMajor: 16, DumpSHA256: strings.Repeat("d", 64), Backup: BackupPolicy{Keep: 1, Offsite: true}}
	m := backupToolManager(t, "native-bookshop-pg-primary", primary)
	m.mu.Lock()
	ids := []string{"native-bookshop-pg-primary"}
	for _, role := range []string{"cable", "cache", "queue"} {
		id := "native-bookshop-sqlite-" + role
		ids = append(ids, id)
		bound := NativeDatabasePolicy{Enabled: true, AppID: app, Roles: []string{role}, SQLitePath: "storage/production_" + role + ".sqlite3",
			ConfigSHA256: strings.Repeat("a", 64), DumpSHA256: old, Backup: BackupPolicy{Keep: 1, Offsite: true}}
		configured := bound
		configured.Roles = []string{role}
		configured.DumpSHA256 = current
		m.cfg.Native.Databases[id] = configured
		m.data.NativeDatabases[id] = NativeDatabaseBinding{Policy: bound, Adopted: time.Now()}
		m.data.Databases[id] = Database{AppID: id, Mode: "external", Ready: true, Backup: bound.Backup}
	}
	m.data.BackupStorage = &storage.Config{}
	// Busy databases stop the gate at the first backup, after its role check.
	for _, id := range ids {
		d := m.data.Databases[id]
		d.Operation = "restore"
		m.data.Databases[id] = d
	}
	m.nativeOperations = map[string]bool{app: true}
	targets := m.nativeReleaseBackupTargetsLocked(app)
	m.mu.Unlock()
	if len(targets) != 4 {
		t.Fatalf("release backup targets = %q, want all four databases", targets)
	}
	_, err := m.backupBeforeNativeRailsRelease(context.Background(), app, []string{"cable", "cache", "primary", "queue"})
	if err == nil || strings.Contains(err.Error(), "unprotected database roles") || !strings.Contains(err.Error(), "unavailable or busy") {
		t.Fatalf("release gate = %v; want it to cover every role and reach the backups", err)
	}
	m.mu.Lock()
	binding := m.data.NativeDatabases["native-bookshop-sqlite-queue"]
	m.mu.Unlock()
	if binding.Policy.DumpSHA256 != current || binding.Policy.SQLitePath != "storage/production_queue.sqlite3" || binding.Adopted.IsZero() {
		t.Fatalf("binding = %+v; want the configured pin with its identity kept", binding)
	}
	// The backup verifies the binary against the converged pin as it is:
	// no re-approval and no package manager query is needed.
	calls := host.commandCount()
	if pin, note, err := m.nativeBackupToolPin(context.Background(), binding.Policy); err != nil || pin != current || note != "" || host.commandCount() != calls {
		t.Fatalf("tool pin = %q %q %v", pin, note, err)
	}

	// The converged binding was saved.
	cfg := m.cfg
	m.Close()
	cfg.Native = nil
	reopened, err := NewManager(context.Background(), cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.mu.Lock()
	defer reopened.mu.Unlock()
	for _, role := range []string{"cable", "cache", "queue"} {
		if got := reopened.data.NativeDatabases["native-bookshop-sqlite-"+role].Policy.DumpSHA256; got != current {
			t.Fatalf("%s: saved pin = %q, want %q", role, got, current)
		}
	}
}

// Identity is not a pin: a changed file, connection, role or variable keeps
// the database out of use until it is reviewed, and the binding is untouched.
func TestNativeDatabaseIdentityChangeIsNotAdoptedWithItsPins(t *testing.T) {
	sqlite := sqliteToolPolicy(strings.Repeat("b", 64))
	pg := pgToolPolicy(strings.Repeat("b", 64))
	cases := []struct {
		name   string
		policy NativeDatabasePolicy
		change func(*NativeDatabasePolicy)
	}{
		{"sqlite_path", sqlite, func(p *NativeDatabasePolicy) { p.SQLitePath = "storage/other.sqlite3" }},
		{"roles", sqlite, func(p *NativeDatabasePolicy) { p.Roles = []string{"cache"} }},
		{"host", pg, func(p *NativeDatabasePolicy) { p.Host = "127.0.0.2" }},
		{"port", pg, func(p *NativeDatabasePolicy) { p.Port = "6543" }},
		{"environment key", pg, func(p *NativeDatabasePolicy) { p.EnvironmentKey = "OTHER_DATABASE_URL" }},
		{"postgres major", pg, func(p *NativeDatabasePolicy) { p.PostgresMajor = 17 }},
		{"backup", sqlite, func(p *NativeDatabasePolicy) { p.Backup.Offsite = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := backupToolManager(t, "native-shop", tc.policy)
			changed := tc.policy
			changed.Roles = append([]string(nil), tc.policy.Roles...)
			tc.change(&changed)
			changed.DumpSHA256 = strings.Repeat("c", 64)
			changed.ConfigSHA256 = strings.Repeat("e", 64)
			if err := validateNativeDatabasePolicy("native-shop", changed); err != nil {
				t.Fatal("fixture change is invalid:", err)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			m.cfg.Native.Databases["native-shop"] = changed
			if m.nativeDatabaseInUseLocked("native-shop") {
				t.Fatal("an identity change kept the database in use")
			}
			if got := m.data.NativeDatabases["native-shop"].Policy; got.DumpSHA256 != tc.policy.DumpSHA256 || got.ConfigSHA256 != tc.policy.ConfigSHA256 {
				t.Fatalf("pins of a changed identity were adopted: %+v", got)
			}
		})
	}
}

// A hand pin is adopted, not trusted: a binary that matches neither it nor
// a package-verified update is still refused at backup time.
func TestNativeDatabaseRepinToUnverifiableBinaryFailsAtBackup(t *testing.T) {
	host := newBackupToolHost(t)
	host.install("/usr/bin/sqlite3", "sqlite3", "3.45.1-1ubuntu2.9", "sqlite3 patched")
	host.verify = "??5??????   /usr/bin/sqlite3\n"
	policy := sqliteToolPolicy(backupToolDigest("sqlite3 old"))
	m := backupToolManager(t, "native-shop-queue", policy)
	configured := policy
	configured.DumpSHA256 = backupToolDigest("sqlite3 something else")
	m.mu.Lock()
	m.cfg.Native.Databases["native-shop-queue"] = configured
	inUse := m.nativeDatabaseInUseLocked("native-shop-queue")
	binding := m.data.NativeDatabases["native-shop-queue"]
	m.mu.Unlock()
	if !inUse || binding.Policy.DumpSHA256 != configured.DumpSHA256 {
		t.Fatalf("in use %v, binding %+v", inUse, binding.Policy)
	}
	if _, _, err := m.nativeBackupToolPin(context.Background(), binding.Policy); err == nil || !strings.Contains(err.Error(), "changed and was not re-approved") {
		t.Fatalf("tool check = %v; want the tool refusal", err)
	}
}

// config_sha256 is handled the same way: the configured database.yml pin is
// adopted, and the file is still checked against it before every backup.
func TestNativeDatabaseBindingFollowsAHandRepinnedConfigHash(t *testing.T) {
	m, _, p, _ := nativeDatabaseFixture(t, "postgresql://fixture:password@127.0.0.1:15435/native_fixture")
	bound := p
	bound.ConfigSHA256 = strings.Repeat("f", 64) // an earlier database.yml no release keeps
	m.data.NativeDatabases = map[string]NativeDatabaseBinding{"native-fixture": {Policy: bound, Adopted: time.Now()}}
	if _, err := m.nativeDatabaseSource(bound); err == nil {
		t.Fatal("fixture: the stale pin verified")
	}
	m.mu.Lock()
	inUse := m.nativeDatabaseInUseLocked("native-fixture")
	converged := m.data.NativeDatabases["native-fixture"].Policy
	m.mu.Unlock()
	if !inUse || converged.ConfigSHA256 != p.ConfigSHA256 || converged.DumpSHA256 != p.DumpSHA256 {
		t.Fatalf("in use %v, binding %+v", inUse, converged)
	}
	if _, err := m.nativeDatabaseSource(converged); err != nil {
		t.Fatal("the converged pin does not verify the current database.yml:", err)
	}
	reloaded := &Manager{cfg: m.cfg, key: m.key}
	if err := reloaded.load(); err != nil || reloaded.data.NativeDatabases["native-fixture"].Policy.ConfigSHA256 != p.ConfigSHA256 {
		t.Fatal("the converged binding was not saved", err)
	}

	// A configured pin the file does not match is adopted and then refused.
	other := sha256.Sum256([]byte("production:\n  primary:\n    url: <%= ENV['OTHER_URL'] %>\n"))
	repinned := p
	repinned.ConfigSHA256 = hex.EncodeToString(other[:])
	m.mu.Lock()
	m.cfg.Native.Databases["native-fixture"] = repinned
	inUse = m.nativeDatabaseInUseLocked("native-fixture")
	converged = m.data.NativeDatabases["native-fixture"].Policy
	m.mu.Unlock()
	if !inUse || converged.ConfigSHA256 != repinned.ConfigSHA256 {
		t.Fatalf("in use %v, binding %+v", inUse, converged)
	}
	if _, err := m.nativeDatabaseSource(converged); err == nil {
		t.Fatal("a database.yml that matches no approved pin was accepted")
	}
}
