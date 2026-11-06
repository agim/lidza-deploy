package agent

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeZstd installs a stand-in for the zstd tool: it writes the Zstandard
// magic followed by a gzip stream, which exercises detection, the pipes
// and the negotiation without the real tool.
func fakeZstd(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	tool := filepath.Join(dir, "zstd")
	script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "-d" ]; then tail -c +5 | gzip -dc; exit $?; fi
done
printf '\050\265\057\375'; gzip -1 -c
`
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := zstdPath
	zstdPath = func() (string, error) { return tool, nil }
	t.Cleanup(func() { zstdPath = previous })
}

func noZstd(t *testing.T) {
	t.Helper()
	previous := zstdPath
	zstdPath = func() (string, error) { return "", errors.New("not installed") }
	t.Cleanup(func() { zstdPath = previous })
}

func TestMoveClickHouseDirectoryBackupWithZstd(t *testing.T) {
	fakeZstd(t)
	source, sourceCH := containerClickHouseSource(t)
	destination, targetCH := nativeClickHouseDestination(t)
	export, archive := exportClickHouse(t, moveMux(source), "?format=directory&compress=zstd")
	if export.Format != clickhouseFormatDirectory || export.Compression != compressionZstd || !bytes.HasPrefix(archive, zstdMagic) || export.Files == 0 {
		t.Fatalf("export %+v", export)
	}
	if !slices.ContainsFunc(sourceCH.statements(), func(s string) bool {
		return strings.HasPrefix(s, "BACKUP DATABASE `telemetry` TO File('lidza-") && strings.HasSuffix(s, "/') ASYNC")
	}) {
		t.Fatalf("source statements %v", sourceCH.statements())
	}
	if leftovers, _ := os.ReadDir(sourceCH.files); len(leftovers) != 0 {
		t.Fatalf("the source kept its backup directory: %v", leftovers)
	}
	w := restoreClickHouse(moveMux(destination), archive, export.SHA256)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"format":"directory"`) {
		t.Fatalf("restore %d %s", w.Code, w.Body)
	}
	statements := targetCH.statements()
	if !slices.ContainsFunc(statements, func(s string) bool {
		return strings.HasPrefix(s, "RESTORE DATABASE `telemetry` AS `app_shop` FROM Disk('backups', 'agent/lidza-") && strings.HasSuffix(s, "/') ASYNC")
	}) || slices.ContainsFunc(statements, func(s string) bool { return strings.Contains(s, ".zip") }) {
		t.Fatalf("destination statements %v", statements)
	}
	if table := targetCH.table("app_shop", "events"); table == nil || table.Rows != 1200 {
		t.Fatalf("destination %+v", table)
	}
	backups := filepath.Join(targetCH.disk, nativeCHBackupSubdir)
	if leftovers, _ := os.ReadDir(backups); len(leftovers) != 0 {
		t.Fatalf("the destination kept the extracted backup: %v", leftovers)
	}
	// A destination without the tool cannot restore Zstandard and says so,
	// leaving its database untouched.
	noZstd(t)
	w = restoreClickHouse(moveMux(destination), archive, export.SHA256)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "zstd") {
		t.Fatalf("restore without zstd %d %s", w.Code, w.Body)
	}
	if table := targetCH.table("app_shop", "events"); table == nil || table.Rows != 1200 {
		t.Fatalf("destination changed by a refused restore: %+v", table)
	}
	// Without the tool a source exports plain tar.
	export, archive = exportClickHouse(t, moveMux(source), "?format=directory&compress=zstd")
	if export.Compression != compressionNone || bytes.HasPrefix(archive, zstdMagic) {
		t.Fatalf("export without zstd %+v", export)
	}
}

func TestClickHouseRestoreFailureLeavesTheDatabaseUnchanged(t *testing.T) {
	noZstd(t)
	source, _ := containerClickHouseSource(t)
	destination, targetCH := nativeClickHouseDestination(t)
	export, archive := exportClickHouse(t, moveMux(source), "?format=directory")
	targetCH.seed("app_shop", map[string]uint64{"existing": 5})
	targetCH.mu.Lock()
	targetCH.failRestore = true
	targetCH.mu.Unlock()
	if w := restoreClickHouse(moveMux(destination), archive, export.SHA256); w.Code != 409 {
		t.Fatalf("failing restore %d %s", w.Code, w.Body)
	}
	if table := targetCH.table("app_shop", "existing"); table == nil || table.Rows != 5 || targetCH.table("app_shop", "events") != nil {
		t.Fatal("a failed restore changed the destination database")
	}
	// A restore that loses rows is rolled back the same way.
	targetCH.mu.Lock()
	targetCH.failRestore, targetCH.loseRows = false, true
	targetCH.mu.Unlock()
	if w := restoreClickHouse(moveMux(destination), archive, export.SHA256); w.Code != 409 || !strings.Contains(w.Body.String(), "row-count verification failed") {
		t.Fatalf("lossy restore %d %s", w.Code, w.Body)
	}
	if table := targetCH.table("app_shop", "existing"); table == nil || table.Rows != 5 {
		t.Fatal("a restore that failed verification changed the destination database")
	}
	targetCH.mu.Lock()
	targetCH.loseRows = false
	targetCH.mu.Unlock()
	if w := restoreClickHouse(moveMux(destination), archive, export.SHA256); w.Code != 200 {
		t.Fatalf("restore %d %s", w.Code, w.Body)
	}
	targetCH.mu.Lock()
	_, previous := targetCH.dbs["app_shop"+clickhousePreviousSuffix]
	_, existing := targetCH.dbs["app_shop"]["existing"]
	targetCH.mu.Unlock()
	if previous || existing || targetCH.table("app_shop", "events") == nil {
		t.Fatal("a successful restore did not replace the database")
	}
	// A database that cannot be renamed is replaced as before.
	targetCH.mu.Lock()
	targetCH.noRename = true
	targetCH.mu.Unlock()
	if w := restoreClickHouse(moveMux(destination), archive, export.SHA256); w.Code != 200 {
		t.Fatalf("restore without rename %d %s", w.Code, w.Body)
	}
}

func TestClickHouseRestoreRecoversAnInterruptedSwap(t *testing.T) {
	noZstd(t)
	source, _ := containerClickHouseSource(t)
	destination, targetCH := nativeClickHouseDestination(t)
	export, archive := exportClickHouse(t, moveMux(source), "?format=directory")
	// An earlier restore moved the database aside and stopped: the aside
	// copy is restored when this one fails.
	targetCH.seed("app_shop"+clickhousePreviousSuffix, map[string]uint64{"existing": 5})
	targetCH.seed("app_shop", map[string]uint64{"partial": 1})
	targetCH.mu.Lock()
	targetCH.failRestore = true
	targetCH.mu.Unlock()
	if w := restoreClickHouse(moveMux(destination), archive, export.SHA256); w.Code != 409 {
		t.Fatalf("failing restore %d %s", w.Code, w.Body)
	}
	if table := targetCH.table("app_shop", "existing"); table == nil || table.Rows != 5 {
		t.Fatal("the aside copy was not put back")
	}
}

func TestClickHouseOperationIsKilledWhenCancelled(t *testing.T) {
	noZstd(t)
	source, sourceCH := containerClickHouseSource(t)
	service, err := source.clickhouseServiceFor(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	sourceCH.mu.Lock()
	sourceCH.holdOps = true
	sourceCH.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	var out bytes.Buffer
	_, err = exportClickHouseArchive(ctx, service, clickhouseFormatDirectory, t.TempDir(), &out)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 5*time.Second {
		t.Fatalf("cancelled backup: %v after %s", err, time.Since(started))
	}
	sourceCH.mu.Lock()
	killed := slices.Clone(sourceCH.killed)
	status := sourceCH.ops["op-1"]
	sourceCH.mu.Unlock()
	if len(killed) != 1 || killed[0] != "q-op-1" || status != "BACKUP_CANCELLED" {
		t.Fatalf("killed %v, status %s", killed, status)
	}
	// A restore is killed the same way.
	destination, targetCH := nativeClickHouseDestination(t)
	sourceCH.mu.Lock()
	sourceCH.holdOps = false
	sourceCH.mu.Unlock()
	export, archive := exportClickHouse(t, moveMux(source), "?format=directory")
	_ = export
	target, _, err := destination.hostClickHouseService("shop")
	if err != nil {
		t.Fatal(err)
	}
	targetCH.mu.Lock()
	targetCH.holdOps = true
	targetCH.mu.Unlock()
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := restoreClickHouseArchive(ctx, target, bytes.NewReader(archive), true); err == nil {
		t.Fatal("a cancelled restore succeeded")
	}
	targetCH.mu.Lock()
	defer targetCH.mu.Unlock()
	if len(targetCH.killed) != 1 || !slices.ContainsFunc(targetCH.log, func(s string) bool { return strings.HasPrefix(s, "KILL QUERY") }) {
		t.Fatalf("restore not killed: %v", targetCH.killed)
	}
}

func TestClickHouseDirectoryEntriesStayInside(t *testing.T) {
	for _, bad := range []string{"../x", "a/../../x", "/etc/passwd", "a//b", `a\b`} {
		if _, err := clickhouseDirectoryEntry(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	for name, want := range map[string]string{"./": "", ".backup": ".backup", "./data/x.bin": "data/x.bin", "data/": "data"} {
		if got, err := clickhouseDirectoryEntry(name); err != nil || got != want {
			t.Fatalf("%q: %q %v", name, got, err)
		}
	}
}

func TestMoveHostAdvertisesClickHouseFormats(t *testing.T) {
	fakeZstd(t)
	m, _ := containerClickHouseSource(t)
	w := httpGet(moveMux(m), "/v1/move/host")
	if !strings.Contains(w, `"clickhouse_formats":["directory","backup","native","incremental"]`) || !strings.Contains(w, `"zstd":true`) {
		t.Fatalf("host %s", w)
	}
}

func httpGet(mux http.Handler, path string) string {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w.Body.String()
}
