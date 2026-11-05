package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeSQLiteSnapshotValidatesAndStreamsStandaloneFile(t *testing.T) {
	tool, err := os.ReadFile(nativeSQLiteTool)
	if os.IsNotExist(err) {
		t.Skip("sqlite3 CLI is unavailable")
	}
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(tool)
	root := t.TempDir()
	source := filepath.Join(root, "source.db")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	execute := func(path, sql string) string {
		t.Helper()
		command := exec.CommandContext(ctx, nativeSQLiteTool, "-init", "/dev/null", "-batch", "-bail", path, sql)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("sqlite fixture failed: %s", output)
		}
		return string(output)
	}
	execute(source, "CREATE TABLE parent(id INTEGER PRIMARY KEY); CREATE TABLE item(parent_id REFERENCES parent(id)); INSERT INTO parent VALUES(42); INSERT INTO item VALUES(42);")
	identity, err := nativeSQLiteIdentity(source)
	if err != nil {
		t.Fatal(err)
	}
	spec := nativeSQLiteSnapshotInput{Name: "source.db", Identity: identity, ToolSHA256: hex.EncodeToString(digest[:])}
	var data bytes.Buffer
	if err := streamNativeSQLiteSnapshot(ctx, source, spec, &data); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(root, "restored.db")
	if err := os.WriteFile(restored, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if rows := execute(restored, "SELECT parent_id FROM item;"); rows != "42\n" {
		t.Fatalf("unexpected restored rows %q", rows)
	}
	execute(source, "INSERT INTO item VALUES(7);")
	data.Reset()
	if err := streamNativeSQLiteSnapshot(ctx, source, spec, &data); err == nil || data.Len() != 0 {
		t.Fatal("snapshot with a broken foreign key escaped verification")
	}
	link := filepath.Join(root, "source-link.db")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if err := streamNativeSQLiteSnapshot(ctx, link, spec, &data); err == nil || data.Len() != 0 {
		t.Fatal("snapshot followed an unreviewed source link")
	}
}

func TestNativeSQLiteResourcePolicyCannotMixAdapters(t *testing.T) {
	policy := NativeDatabasePolicy{
		Enabled: true, AppID: "rails-fixture", Roles: []string{"queue"},
		SQLitePath: "storage/production_queue.sqlite3", ConfigSHA256: string(bytes.Repeat([]byte("a"), 64)),
		DumpSHA256: string(bytes.Repeat([]byte("b"), 64)), Backup: BackupPolicy{Keep: 1, Offsite: true},
	}
	if err := validateNativeDatabasePolicy("native-fixture", policy); err != nil {
		t.Fatal(err)
	}
	invalid := policy
	invalid.PostgresMajor = 17
	if validateNativeDatabasePolicy("native-fixture", invalid) == nil || sameNativeDatabasePolicy(policy, invalid) {
		t.Fatal("SQLite policy accepted PostgreSQL tool settings")
	}
	invalid = policy
	invalid.SQLitePath = "storage/../other.sqlite3"
	if validateNativeDatabasePolicy("native-fixture", invalid) == nil || sameNativeDatabasePolicy(policy, invalid) {
		t.Fatal("SQLite policy accepted a path alias")
	}
}

func TestNativeSQLiteSnapshotsRunAsTheApplicationsAccount(t *testing.T) {
	m := &Manager{cfg: Config{Native: &NativeHostConfig{User: "deploy", Releases: map[string]NativeReleasePolicy{
		"shared-owner": {Root: "/home/deploy/shared-owner"},
		"own-account":  {Root: "/home/deploy/own-account", User: "app-own-account"},
	}}}}
	m.data.NativeCreated = map[string]NativeReleasePolicy{"created": {Root: "/srv/lidza/apps/created", User: "app-created"}}
	for id, want := range map[string]string{"shared-owner": "deploy", "own-account": "app-own-account", "created": "app-created", "unknown": "deploy"} {
		if got := m.nativeDatabaseAccount(id); got != want {
			t.Errorf("%s: snapshot account %q, want %q", id, got, want)
		}
	}
}
