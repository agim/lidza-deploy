package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostDatabaseSizeMeasuresSQLiteAndSkipsContainers(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "production.sqlite3")
	os.WriteFile(db, make([]byte, 4096), 0o600)
	os.WriteFile(db+"-wal", make([]byte, 1024), 0o600)
	if n, ok, err := hostDatabaseSize(context.Background(), Database{URL: db}); !ok || err != nil || n != 5120 {
		t.Fatal("sqlite size", n, ok, err)
	}
	if _, ok, err := hostDatabaseSize(context.Background(), Database{URL: filepath.Join(dir, "missing.sqlite3")}); !ok || err == nil {
		t.Fatal("missing sqlite file must be reported")
	}
	if _, ok, _ := hostDatabaseSize(context.Background(), Database{Mode: "managed", URL: "postgres://u:p@db:5432/app"}); ok {
		t.Fatal("container databases are measured inside their network")
	}
}

func TestServerHealthMeasuresArchivesAndRetainedDatabasesWithoutErrors(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	fakeDisk(t, 100<<30, 50<<30)
	savedHost, savedArchive := measureHostDatabase, nativeArchiveQuery
	t.Cleanup(func() { measureHostDatabase, nativeArchiveQuery = savedHost, savedArchive })
	var archives []string
	nativeArchiveQuery = func(ctx context.Context, p NativeDatabasePolicy, sql string) (string, error) {
		archives = append(archives, p.ArchiveDatabase+": "+sql)
		return "1048576\n", nil
	}
	measureHostDatabase = func(ctx context.Context, d Database) (int64, bool, error) {
		switch d.AppID {
		case "gone", "live-broken":
			return 0, true, errors.New(`psql: error: connection to server at "127.0.0.1", port 5432 failed: FATAL:  database "gone" does not exist`)
		case "moved":
			return 4096, true, nil
		}
		return 0, true, nil
	}
	external := func(id string) Database {
		return Database{AppID: id, Mode: "external", URL: "postgres://" + id + ":pw@127.0.0.1:5432/" + id + "?sslmode=require", Ready: true}
	}
	// gone: a removed application's database; its data is gone and only backups remain.
	gone := external("gone")
	gone.Backups = []BackupRecord{{ID: "b1", Created: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Size: 100}, {ID: "b2", Created: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Size: 1288490189}}
	// moved: retained after a move, the host still answers.
	moved := external("moved")
	// live-broken: bound to a running application; its probe genuinely fails.
	broken := external("live-broken")
	// native-archive-x: a standalone archive measured by its archive_database name.
	archive := Database{AppID: "native-archive-x", Mode: "external", URL: nativeArchiveMetadataURL("x_archive"), Ready: true}
	m.mu.Lock()
	m.data.Databases = map[string]Database{"gone": gone, "moved": moved, "live-broken": broken, "native-archive-x": archive}
	m.data.NativeDatabases = map[string]NativeDatabaseBinding{"native-archive-x": {Policy: NativeDatabasePolicy{ArchiveDatabase: "x_archive", PostgresMajor: 16}}}
	m.data.Apps = map[string]App{"portal": {ID: "portal", Bindings: map[string]string{"DATABASE_URL": "live-broken"}}}
	m.mu.Unlock()

	health := m.serverHealth(context.Background())
	if got := health.DatabaseBytes["native-archive-x"]; got != 1048576 {
		t.Fatalf("archive size %d; probes %q", got, archives)
	}
	if len(archives) != 1 || archives[0] != "x_archive: SELECT pg_database_size(current_database())" {
		t.Fatalf("archive probes %q", archives)
	}
	if _, measured := health.DatabaseBytes["gone"]; measured {
		t.Fatal("a database that does not answer was given a size")
	}
	if got := health.RetainedDatabases["gone"]; got.BackupBytes != 1288490189 || !got.BackupCreated.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("retained gone %+v", got)
	}
	if got := health.DatabaseBytes["moved"]; got != 4096 {
		t.Fatalf("retained moved measured %d", got)
	}
	if got, ok := health.RetainedDatabases["moved"]; !ok || got.BackupBytes != 0 {
		t.Fatalf("retained moved %+v %v", got, ok)
	}
	if _, ok := health.RetainedDatabases["live-broken"]; ok {
		t.Fatal("a bound database was reported retained")
	}
	var sizeErrors []string
	for _, e := range health.Errors {
		if strings.Contains(e, "database size unavailable") {
			sizeErrors = append(sizeErrors, e)
		}
	}
	if len(sizeErrors) != 1 || sizeErrors[0] != `database size unavailable: live-broken (psql: error: connection to server at "127.0.0.1", port 5432 failed: FATAL: database "gone" does not exist)` {
		t.Fatalf("size errors %q", sizeErrors)
	}
}

func TestProbeReasonUsesStderrFirstLine(t *testing.T) {
	err := &exec.ExitError{ProcessState: &os.ProcessState{}, Stderr: []byte("\npsql: error: FATAL:  role \"x\" does not exist\nmore\n")}
	if got := probeReason(err); got != `psql: error: FATAL: role "x" does not exist` {
		t.Fatalf("reason %q", got)
	}
	if got := probeReason(errors.New("no size returned")); got != "no size returned" {
		t.Fatalf("reason %q", got)
	}
}
