package agent

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestNativeArchiveDatabaseRemainsStandaloneAndKeepsExactSource(t *testing.T) {
	id := "native-archive-fixture"
	p := NativeDatabasePolicy{Enabled: true, Host: "127.0.0.1", Port: "5432", PostgresMajor: 17,
		ConfigSHA256: nativeArchivePeerDigest("fixture_archive"), DumpSHA256: strings.Repeat("b", 64), ArchiveDatabase: "fixture_archive",
		Backup: BackupPolicy{Offsite: true, Keep: 1}}
	if err := validateNativeDatabasePolicy(id, p); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	const limit = 32 << 20
	config := Config{
		DataDir: dir, APIKey: strings.Repeat("a", 32),
		Native:         &NativeHostConfig{Root: dir, User: "deploy", Databases: map[string]NativeDatabasePolicy{id: p}},
		BackupMaxBytes: map[string]int64{id: limit},
	}
	rawConfig, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rawConfig, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := Manager{cfg: loaded}
	if got, err := manager.backupDiskLimit(dir, id); err != nil || got != limit {
		t.Fatalf("standalone archive ceiling = %d, error = %v", got, err)
	}
	for name, change := range map[string]func(*NativeDatabasePolicy){
		"attached":   func(p *NativeDatabasePolicy) { p.AppID = "rails-fixture" },
		"app role":   func(p *NativeDatabasePolicy) { p.Roles = []string{"primary"} },
		"env source": func(p *NativeDatabasePolicy) { p.EnvironmentKey = "DATABASE_URL" },
		"foreign":    func(p *NativeDatabasePolicy) { p.Host = "10.0.0.10" },
		"peer drift": func(p *NativeDatabasePolicy) { p.ConfigSHA256 = strings.Repeat("c", 64) },
		"no offsite": func(p *NativeDatabasePolicy) { p.Backup.Offsite = false },
	} {
		t.Run(name, func(t *testing.T) {
			changed := p
			change(&changed)
			if err := validateNativeDatabasePolicy(id, changed); err == nil {
				t.Fatal("unreviewed archive configuration accepted")
			}
		})
	}
	raw := "postgresql://backup:fixture-password@127.0.0.1:5432/fixture_archive"
	if err := validateNativeDatabaseURL(raw, p); err != nil {
		t.Fatal(err)
	}
	if err := validateNativeDatabaseURL(strings.Replace(raw, "fixture_archive", "different_db", 1), p); err == nil {
		t.Fatal("different database accepted")
	}
	if !nativeArchiveMatchesURL(p, "postgresql://different-user@localhost:6432/fixture_archive") ||
		nativeArchiveMatchesURL(p, "postgresql://different-user@10.0.0.1:5432/fixture_archive") {
		t.Fatal("incorrect duplicate detection for the local archive")
	}
	m := &Manager{cfg: Config{Native: &NativeHostConfig{Databases: map[string]NativeDatabasePolicy{id: p}}},
		data: diskState{NativeDatabases: map[string]NativeDatabaseBinding{id: {Policy: p}},
			Databases: map[string]Database{id: {AppID: id, Ready: true, URL: raw}}}}
	if !m.nativeDatabaseInUseLocked(id) || len(m.attachmentsLocked(id)) != 0 {
		t.Fatal("archive must be usable without registering or attaching an app")
	}
	if err := m.bindDatabaseLocked("container-fixture", "DATABASE_URL", id); err == nil {
		t.Fatal("archive credential was attachable to an app")
	}
	if err := m.setBackupPolicy(id, BackupPolicy{Keep: 1}); err == nil {
		t.Fatal("archive schedule accepted disabling off-site protection")
	}
	changed := p
	changed.ArchiveDatabase = "other_database"
	m.cfg.Native.Databases[id] = changed
	if m.nativeDatabaseInUseLocked(id) {
		t.Fatal("changed standalone database retained backup authority")
	}
}

func TestNativeArchiveDatabasePeerDumpLocal(t *testing.T) {
	if os.Getenv("LIDZA_NATIVE_ARCHIVE_PEER_FIXTURE") != "docker-local" {
		t.Skip("disposable PostgreSQL peer fixture required")
	}
	if _, err := os.Stat("/.dockerenv"); err != nil || os.Geteuid() != 0 {
		t.Fatal("disposable container root required")
	}
	digest, err := nativeStaticFileDigest("/usr/lib/postgresql/17/bin/pg_dump")
	if err != nil {
		t.Fatal(err)
	}
	id, name := "native-archive-local-fixture", "lidza_archive_fixture"
	p := NativeDatabasePolicy{Enabled: true, Host: "127.0.0.1", Port: "5432", PostgresMajor: 17, ArchiveDatabase: name,
		ConfigSHA256: nativeArchivePeerDigest(name), DumpSHA256: hex.EncodeToString(digest[:]), Backup: BackupPolicy{Offsite: true, Keep: 1}}
	if err = validateNativeDatabasePolicy(id, p); err != nil {
		t.Fatal(err)
	}
	if err = checkNativeBackupSource(context.Background(), nativeArchiveMetadataURL(name), p); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), ".lidza-native-pg-locks")
	if err = os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	spec := nativePGDumpInput{PeerDatabase: name, Major: p.PostgresMajor, ToolSHA256: p.DumpSHA256, Lock: filepath.Join(directory, id+".lock")}
	raw, _ := json.Marshal(spec)
	var output bytes.Buffer
	if err = RunNativePGDump(context.Background(), bytes.NewReader(raw), &output); err != nil {
		t.Fatal(err)
	}
	restore := exec.Command("/usr/lib/postgresql/17/bin/pg_restore", "--list")
	restore.Stdin = bytes.NewReader(output.Bytes())
	listing, err := restore.Output()
	if err != nil || !strings.Contains(string(listing), "TABLE DATA public archive_probe postgres") {
		t.Fatal("isolated pg_restore could not read the peer dump", err)
	}
	credential, err := nativeArchivePeerCredential()
	if err != nil {
		t.Fatal(err)
	}
	target := "lidza_archive_restored"
	restore = exec.Command("/usr/lib/postgresql/17/bin/pg_restore", "--host="+nativeArchiveSocket, "--username=postgres",
		"--dbname="+target, "--exit-on-error", "--single-transaction")
	restore.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	restore.Stdin = bytes.NewReader(output.Bytes())
	if diagnostic, err := restore.CombinedOutput(); err != nil {
		t.Fatal("isolated restore failed", err, string(diagnostic))
	}
	query := exec.Command("/usr/lib/postgresql/17/bin/psql", "-XAtw", "-h", nativeArchiveSocket, "-U", "postgres",
		"-d", target, "-c", "SELECT count(*) FROM archive_probe")
	query.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	if raw, err := query.Output(); err != nil || strings.TrimSpace(string(raw)) != "2" {
		t.Fatal("isolated restore row count differed", err)
	}
}
