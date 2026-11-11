package agent

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const backupToolReleases = `Package files:
 100 /var/lib/dpkg/status
     release a=now
 500 http://mirror.example.net/ubuntu noble-updates/main amd64 Packages
     release v=24.04,o=Ubuntu,a=noble-updates,n=noble,l=Ubuntu,c=main,b=amd64
     origin mirror.example.net
 500 http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages
     release v=24.04,o=Ubuntu,a=noble-security,n=noble,l=Ubuntu,c=main,b=amd64
     origin security.ubuntu.com
 500 http://apt.postgresql.org/pub/repos/apt noble-pgdg/main amd64 Packages
     release o=apt.postgresql.org,a=noble-pgdg,n=noble-pgdg,l=PostgreSQL for Debian/Ubuntu repository,c=main,b=amd64
     origin apt.postgresql.org
 500 https://ppa.launchpadcontent.net/someone/tools/ubuntu noble/main amd64 Packages
     release v=24.04,o=LP-PPA-someone-tools,a=noble,n=noble,l=Tools,c=main,b=amd64
     origin ppa.launchpadcontent.net
Pinned packages:
`

// backupToolHost is a fake Debian host: the tool file, dpkg's records and
// the dpkg-query, dpkg --verify and apt-cache answers.
type backupToolHost struct {
	t       *testing.T
	root    string
	mu      sync.Mutex
	owner   string // dpkg-query -S answer
	verify  string // dpkg --verify output
	policy  string // apt-cache policy <pkg>
	calls   []string
	version string
}

func newBackupToolHost(t *testing.T) *backupToolHost {
	h := &backupToolHost{t: t, root: t.TempDir()}
	previousPath, previousCommand, previousUID := hostPath, hostCommand, nativeBackupToolRootUID
	hostPath = func(path string) string { return filepath.Join(h.root, path) }
	hostCommand = func(_ context.Context, name string, args ...string) (string, error) { return h.handle(name, args) }
	nativeBackupToolRootUID = uint32(os.Getuid())
	t.Cleanup(func() { hostPath, hostCommand, nativeBackupToolRootUID = previousPath, previousCommand, previousUID })
	return h
}

func (h *backupToolHost) handle(name string, args []string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, name+" "+strings.Join(args, " "))
	switch {
	case name == "dpkg-query" && len(args) == 2 && args[0] == "-S":
		if h.owner == "" {
			return "dpkg-query: no path found matching pattern " + args[1], os.ErrNotExist
		}
		return h.owner, nil
	case name == "dpkg" && len(args) == 2 && args[0] == "--verify":
		return h.verify, nil
	case name == "apt-cache" && len(args) == 2:
		return h.policy, nil
	case name == "apt-cache" && len(args) == 1:
		return backupToolReleases, nil
	}
	return "", nil
}

func (h *backupToolHost) commandCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

// install writes the tool and its package's md5sums record and returns the
// tool's SHA-256.
func (h *backupToolHost) install(path, pkg, version, content string) string {
	h.t.Helper()
	file := filepath.Join(h.root, path)
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Chmod(file, 0755); err != nil {
		h.t.Fatal(err)
	}
	sum := md5.Sum([]byte(content))
	info := filepath.Join(h.root, "var/lib/dpkg/info")
	if err := os.MkdirAll(info, 0755); err != nil {
		h.t.Fatal(err)
	}
	record := hex.EncodeToString(sum[:]) + "  usr/share/doc/" + pkg + "/copyright\n" + hex.EncodeToString(sum[:]) + "  " + strings.TrimPrefix(path, "/") + "\n"
	if err := os.WriteFile(filepath.Join(info, pkg+".md5sums"), []byte(record), 0644); err != nil {
		h.t.Fatal(err)
	}
	h.owner = pkg + ": " + path
	h.version = version
	h.policy = pkg + ":\n  Installed: " + version + "\n  Candidate: " + version + "\n  Version table:\n *** " + version + " 500\n" +
		"        500 http://mirror.example.net/ubuntu noble-updates/main amd64 Packages\n" +
		"        500 http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages\n" +
		"        100 /var/lib/dpkg/status\n     3.45.1-1ubuntu2 500\n        500 http://mirror.example.net/ubuntu noble/main amd64 Packages\n"
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func backupToolDigest(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

// backupToolManager is an agent with one approved native database in use.
func backupToolManager(t *testing.T, id string, p NativeDatabasePolicy) *Manager {
	t.Helper()
	m := testManager(t, &fakeRuntime{})
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Native = &NativeHostConfig{Databases: map[string]NativeDatabasePolicy{id: p}}
	if m.data.NativeApps == nil {
		m.data.NativeApps = map[string]NativeApplication{}
	}
	m.data.NativeApps[p.AppID] = NativeApplication{ID: p.AppID, Name: p.AppID, Runtime: "rails"}
	if m.data.NativeDatabases == nil {
		m.data.NativeDatabases = map[string]NativeDatabaseBinding{}
	}
	m.data.NativeDatabases[id] = NativeDatabaseBinding{Policy: p, Adopted: time.Now()}
	if m.data.Databases == nil {
		m.data.Databases = map[string]Database{}
	}
	m.data.Databases[id] = Database{AppID: id, Mode: "external", Ready: true, URL: "/srv/" + p.AppID + "/shared/storage/production.sqlite3", Backup: p.Backup}
	if !m.nativeDatabaseInUseLocked(id) {
		t.Fatal("fixture database is not in use")
	}
	return m
}

func sqliteToolPolicy(pin string) NativeDatabasePolicy {
	return NativeDatabasePolicy{Enabled: true, AppID: "rails-shop", Roles: []string{"queue"}, SQLitePath: "storage/production.sqlite3",
		ConfigSHA256: strings.Repeat("a", 64), DumpSHA256: pin, Backup: BackupPolicy{Keep: 1, Offsite: true}}
}

func pgToolPolicy(pin string) NativeDatabasePolicy {
	return NativeDatabasePolicy{Enabled: true, AppID: "rails-shop", Roles: []string{"primary"}, Host: "127.0.0.1", Port: "5432", EnvironmentKey: "DATABASE_URL",
		ConfigSHA256: strings.Repeat("a", 64), PostgresMajor: 16, DumpSHA256: pin, Backup: BackupPolicy{Keep: 1, Offsite: true}}
}

func TestBackupToolReapprovedAfterVerifiedPackageUpdate(t *testing.T) {
	host := newBackupToolHost(t)
	old := backupToolDigest("sqlite3 3.45.1-1ubuntu2.8")
	current := host.install("/usr/bin/sqlite3", "sqlite3", "3.45.1-1ubuntu2.9", "sqlite3 3.45.1-1ubuntu2.9")
	if err := os.MkdirAll(filepath.Join(host.root, "var/log"), 0755); err != nil {
		t.Fatal(err)
	}
	log := "2026-11-06 06:12:01 upgrade sqlite3:amd64 3.45.1-1ubuntu2.8 3.45.1-1ubuntu2.9\n"
	if err := os.WriteFile(filepath.Join(host.root, "var/log/dpkg.log"), []byte(log), 0644); err != nil {
		t.Fatal(err)
	}
	policy := sqliteToolPolicy(old)
	m := backupToolManager(t, "native-shop-queue", policy)

	pin, note, err := m.nativeBackupToolPin(context.Background(), policy)
	if err != nil || pin != current {
		t.Fatalf("pin = %q, %v; want the verified new binary %q", pin, err, current)
	}
	if want := "backup tool /usr/bin/sqlite3 updated by sqlite3 3.45.1-1ubuntu2.8→3.45.1-1ubuntu2.9: re-approved"; !strings.HasPrefix(note, want) {
		t.Fatalf("note = %q, want prefix %q", note, want)
	}
	m.mu.Lock()
	approval := m.data.BackupToolApprovals["/usr/bin/sqlite3"]
	m.mu.Unlock()
	if approval.SHA256 != current || approval.PreviousSHA256 != old || approval.Package != "sqlite3" || approval.Version != "3.45.1-1ubuntu2.9" ||
		approval.PreviousVersion != "3.45.1-1ubuntu2.8" || approval.Approved.IsZero() || len(approval.Supersedes) != 1 || approval.Supersedes[0] != old {
		t.Fatalf("approval = %+v", approval)
	}

	// The approval is preferred: the next check verifies the file against it
	// and asks the package manager nothing.
	calls := host.commandCount()
	if pin, note, err = m.nativeBackupToolPin(context.Background(), policy); err != nil || pin != current || note != "" || host.commandCount() != calls {
		t.Fatalf("second check = %q %q %v (commands %d→%d)", pin, note, err, calls, host.commandCount())
	}

	// It survives a restart of the agent.
	cfg := m.cfg
	m.Close()
	cfg.Native = nil
	reopened, err := NewManager(context.Background(), cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.mu.Lock()
	got := reopened.backupToolPinLocked("/usr/bin/sqlite3", old)
	reopened.mu.Unlock()
	if got != current {
		t.Fatalf("reopened pin = %q, want %q", got, current)
	}

	// A reviewed configuration re-pinned to some other binary is not
	// overridden by an approval it does not descend from.
	other := strings.Repeat("c", 64)
	reopened.mu.Lock()
	got = reopened.backupToolPinLocked("/usr/bin/sqlite3", other)
	reopened.mu.Unlock()
	if got != other {
		t.Fatalf("an unrelated configured pin was replaced: %q", got)
	}
}

func TestBackupToolReapprovalFollowsSuccessiveUpdates(t *testing.T) {
	host := newBackupToolHost(t)
	old := backupToolDigest("v8")
	second := host.install("/usr/bin/sqlite3", "sqlite3", "3.45.1-1ubuntu2.9", "v9")
	policy := sqliteToolPolicy(old)
	m := backupToolManager(t, "native-shop-queue", policy)
	if pin, _, err := m.nativeBackupToolPin(context.Background(), policy); err != nil || pin != second {
		t.Fatalf("first update: %q %v", pin, err)
	}
	third := host.install("/usr/bin/sqlite3", "sqlite3", "3.45.1-1ubuntu2.10", "v10")
	pin, note, err := m.nativeBackupToolPin(context.Background(), policy)
	if err != nil || pin != third || !strings.Contains(note, "re-approved") {
		t.Fatalf("second update: %q %q %v", pin, note, err)
	}
	m.mu.Lock()
	approval := m.data.BackupToolApprovals["/usr/bin/sqlite3"]
	m.mu.Unlock()
	if approval.PreviousSHA256 != second || len(approval.Supersedes) != 2 || approval.Supersedes[0] != old || approval.Supersedes[1] != second {
		t.Fatalf("approval = %+v", approval)
	}
}

func TestBackupToolReapprovesPGDumpFromPostgreSQLRepository(t *testing.T) {
	host := newBackupToolHost(t)
	old := backupToolDigest("pg_dump 16.10")
	path := "/usr/lib/postgresql/16/bin/pg_dump"
	current := host.install(path, "postgresql-client-16", "16.11-1.pgdg24.04+1", "pg_dump 16.11")
	host.policy = "postgresql-client-16:\n  Installed: 16.11-1.pgdg24.04+1\n  Candidate: 16.11-1.pgdg24.04+1\n  Version table:\n *** 16.11-1.pgdg24.04+1 500\n" +
		"        500 http://apt.postgresql.org/pub/repos/apt noble-pgdg/main amd64 Packages\n        100 /var/lib/dpkg/status\n"
	policy := pgToolPolicy(old)
	m := backupToolManager(t, "native-shop", policy)
	pin, note, err := m.nativeBackupToolPin(context.Background(), policy)
	if err != nil || pin != current || !strings.HasPrefix(note, "backup tool "+path+" updated by postgresql-client-16 ") {
		t.Fatalf("pg_dump: %q %q %v", pin, note, err)
	}

	// Another PostgreSQL major's client is not a provider of this pg_dump.
	host.install(path, "postgresql-client-17", "17.7-1.pgdg24.04+1", "pg_dump 16.12")
	if _, _, err = m.nativeBackupToolPin(context.Background(), policy); err == nil || !strings.Contains(err.Error(), "postgresql-client-17 is not an expected provider (postgresql-client-16 or postgresql-16)") {
		t.Fatalf("wrong major accepted: %v", err)
	}
}

func TestBackupToolRefusedUnlessThePackageManagerVouchesForIt(t *testing.T) {
	cases := []struct {
		name   string
		change func(h *backupToolHost)
		want   string
	}{
		{"md5 mismatch", func(h *backupToolHost) {
			if err := os.WriteFile(filepath.Join(h.root, "usr/bin/sqlite3"), []byte("patched by hand"), 0755); err != nil {
				h.t.Fatal(err)
			}
		}, "its MD5 differs from package sqlite3's md5sums record"},
		{"dpkg verify", func(h *backupToolHost) { h.verify = "??5??????   /usr/bin/sqlite3\n" }, "dpkg --verify sqlite3 reports it modified"},
		{"local package", func(h *backupToolHost) {
			h.policy = "sqlite3:\n  Installed: 3.45.1-1local1\n  Candidate: 3.45.1-1local1\n  Version table:\n *** 3.45.1-1local1 100\n        100 /var/lib/dpkg/status\n"
		}, "installed version 3.45.1-1local1 of sqlite3 is not offered by any configured apt repository"},
		{"third-party repository", func(h *backupToolHost) {
			h.policy = "sqlite3:\n  Installed: 3.46.0-1ppa1\n  Candidate: 3.46.0-1ppa1\n  Version table:\n *** 3.46.0-1ppa1 500\n        500 https://ppa.launchpadcontent.net/someone/tools/ubuntu noble/main amd64 Packages\n        100 /var/lib/dpkg/status\n"
		}, "comes from https://ppa.launchpadcontent.net/someone/tools/ubuntu noble/main (origin LP-PPA-someone-tools), not the distribution archive"},
		{"wrong package", func(h *backupToolHost) {
			h.owner = "sqlite3-tools: /usr/bin/sqlite3"
			if err := os.Rename(filepath.Join(h.root, "var/lib/dpkg/info/sqlite3.md5sums"), filepath.Join(h.root, "var/lib/dpkg/info/sqlite3-tools.md5sums")); err != nil {
				h.t.Fatal(err)
			}
		}, "its package sqlite3-tools is not an expected provider (sqlite3)"},
		{"two owners", func(h *backupToolHost) { h.owner = "sqlite3, other: /usr/bin/sqlite3" }, "dpkg lists 2 packages owning it"},
		{"diverted", func(h *backupToolHost) {
			h.owner = "diversion by other from: /usr/bin/sqlite3\ndiversion by other to: /usr/bin/sqlite3.real\nsqlite3: /usr/bin/sqlite3"
		}, "dpkg diverts it"},
		{"no owner", func(h *backupToolHost) { h.owner = "" }, "dpkg lists no package that owns it"},
		{"writable file", func(h *backupToolHost) {
			if err := os.Chmod(filepath.Join(h.root, "usr/bin/sqlite3"), 0775); err != nil {
				h.t.Fatal(err)
			}
		}, "backup tool /usr/bin/sqlite3 is not trusted: it is group or world writable"},
		{"not root", func(h *backupToolHost) { nativeBackupToolRootUID = uint32(os.Getuid()) + 1 }, "it is not owned by root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := newBackupToolHost(t)
			old := backupToolDigest("sqlite3 old")
			host.install("/usr/bin/sqlite3", "sqlite3", "3.45.1-1ubuntu2.9", "sqlite3 new")
			tc.change(host)
			policy := sqliteToolPolicy(old)
			m := backupToolManager(t, "native-shop-queue", policy)
			pin, _, err := m.nativeBackupToolPin(context.Background(), policy)
			if err == nil || pin != "" || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("pin = %q, err = %v; want refusal naming %q", pin, err, tc.want)
			}
			m.mu.Lock()
			approvals := len(m.data.BackupToolApprovals)
			m.mu.Unlock()
			if approvals != 0 {
				t.Fatal("a refused tool was approved")
			}
			// The refusal is a server warning and a warning on the database.
			health := m.backupToolHealth()
			if len(health) != 1 || !strings.Contains(health[0], tc.want) || !strings.Contains(health[0], "Backups of native-shop-queue fail") {
				t.Fatalf("server warnings = %q", health)
			}
			var view DatabaseView
			for _, v := range m.databaseViews() {
				if v.AppID == "native-shop-queue" {
					view = v
				}
			}
			if len(view.Warnings) != 1 || !strings.Contains(view.Warnings[0], tc.want) {
				t.Fatalf("database warnings = %q", view.Warnings)
			}
		})
	}
}

func TestBackupToolWarningClearsOnceTheBinaryMatches(t *testing.T) {
	host := newBackupToolHost(t)
	host.install("/usr/bin/sqlite3", "sqlite3", "3.45.1-1ubuntu2.9", "sqlite3 new")
	host.verify = "??5??????   /usr/bin/sqlite3\n"
	policy := sqliteToolPolicy(backupToolDigest("sqlite3 old"))
	m := backupToolManager(t, "native-shop-queue", policy)
	if _, _, err := m.nativeBackupToolPin(context.Background(), policy); err == nil || len(m.backupToolHealth()) != 1 {
		t.Fatal("modified binary was not refused")
	}
	host.install("/usr/bin/sqlite3", "sqlite3", "3.45.1-1ubuntu2.8", "sqlite3 old")
	if _, _, err := m.nativeBackupToolPin(context.Background(), policy); err != nil || len(m.backupToolHealth()) != 0 {
		t.Fatalf("restored binary: %v, warnings %q", err, m.backupToolHealth())
	}
}

func TestHostUpdatesJobReapprovesBackupTools(t *testing.T) {
	host := newBackupToolHost(t)
	old := backupToolDigest("sqlite3 3.45.1-1ubuntu2.8")
	host.install("/usr/bin/sqlite3", "sqlite3", "3.45.1-1ubuntu2.8", "sqlite3 3.45.1-1ubuntu2.8")
	policy := sqliteToolPolicy(old)
	m := backupToolManager(t, "native-shop-queue", policy)
	var current string
	install := func(context.Context) (string, error) {
		current = host.install("/usr/bin/sqlite3", "sqlite3", "3.45.1-1ubuntu2.9", "sqlite3 3.45.1-1ubuntu2.9")
		return "Installing: sqlite3\nSummary: Security updates installed: 1.\n", nil
	}
	if err := m.startHostJob("security-updates", "manual", install); err != nil {
		t.Fatal(err)
	}
	var job HostJob
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		job = m.hostUpdatesStateLocked().Jobs[0]
		m.mu.Unlock()
		if job.Status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.Status != "succeeded" || job.Summary != "Security updates installed: 1." {
		t.Fatalf("job = %+v", job)
	}
	if !strings.Contains(job.Output, "Backup tools:\nbackup tool /usr/bin/sqlite3 updated by sqlite3 ") || !strings.Contains(job.Output, "→3.45.1-1ubuntu2.9: re-approved") {
		t.Fatalf("job output lacks the re-approval:\n%s", job.Output)
	}
	m.mu.Lock()
	pin := m.backupToolPinLocked("/usr/bin/sqlite3", old)
	m.mu.Unlock()
	if pin != current {
		t.Fatalf("pin after the update = %q, want %q", pin, current)
	}
}

func TestBackupToolCheckClearsOnlyItsOwnStaleDatabaseErrors(t *testing.T) {
	host := newBackupToolHost(t)
	old := backupToolDigest("sqlite3 3.45.1-1ubuntu2.8")
	current := host.install("/usr/bin/sqlite3", "sqlite3", "3.45.1-1ubuntu2.9", "sqlite3 3.45.1-1ubuntu2.9")
	policy := sqliteToolPolicy(old)
	m := backupToolManager(t, "native-shop-queue", policy)
	setError := func(id, message string) {
		m.mu.Lock()
		d := m.data.Databases[id]
		d.Error = message
		m.data.Databases[id] = d
		m.mu.Unlock()
	}
	errorOf := func(id string) string {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.data.Databases[id].Error
	}
	// A second database on another tool keeps its tool error.
	m.mu.Lock()
	pg := pgToolPolicy(strings.Repeat("d", 64))
	m.data.NativeDatabases["native-shop-primary"] = NativeDatabaseBinding{Policy: pg, Adopted: time.Now()}
	m.data.Databases["native-shop-primary"] = Database{AppID: "native-shop-primary", Mode: "external", Ready: true}
	m.mu.Unlock()
	stale := "pre-deployment backup failed: native backup tool changed; review its binary before backup"
	setError("native-shop-queue", stale)
	setError("native-shop-primary", stale)

	// Re-approval clears the tool error of the databases it now verifies.
	if pin, _, err := m.nativeBackupToolPin(context.Background(), policy); err != nil || pin != current {
		t.Fatalf("re-approval: %q %v", pin, err)
	}
	if got := errorOf("native-shop-queue"); got != "" {
		t.Fatalf("stale tool error kept after re-approval: %q", got)
	}
	if got := errorOf("native-shop-primary"); got != stale {
		t.Fatalf("another tool's database error changed: %q", got)
	}

	// Errors of any other kind stay.
	other := "pre-deployment backup failed: database dump exited with status 1"
	setError("native-shop-queue", other)
	if _, _, err := m.nativeBackupToolPin(context.Background(), policy); err != nil || errorOf("native-shop-queue") != other {
		t.Fatalf("unrelated error changed: %v, %q", err, errorOf("native-shop-queue"))
	}

	// A match clears a refusal recorded earlier, and the cleared error is saved.
	setError("native-shop-queue", "Database operation failed: backup tool /usr/bin/sqlite3 changed and was not re-approved: dpkg diverts it to another file; review the binary before backups resume")
	if _, _, err := m.nativeBackupToolPin(context.Background(), policy); err != nil || errorOf("native-shop-queue") != "" {
		t.Fatalf("match: %v, error %q", err, errorOf("native-shop-queue"))
	}
	cfg := m.cfg
	m.Close()
	cfg.Native = nil
	reopened, err := NewManager(context.Background(), cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.mu.Lock()
	got, primary := reopened.data.Databases["native-shop-queue"].Error, reopened.data.Databases["native-shop-primary"].Error
	reopened.mu.Unlock()
	if got != "" || primary != stale {
		t.Fatalf("saved errors = %q / %q", got, primary)
	}
}
