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
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const containerClickHousePassword = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// containerClickHouseSource is a container application whose managed
// ClickHouse service is the fake server.
func containerClickHouseSource(t *testing.T) (*Manager, *fakeClickHouse) {
	t.Helper()
	m := testManager(t, &fakeRuntime{})
	ch := newFakeClickHouse(t, "")
	ch.users["lidza"] = containerClickHousePassword
	ch.seed("telemetry", map[string]uint64{"events": 1200, "sessions": 30})
	m.mu.Lock()
	app := testApp("shop")
	app.Current = &Release{Commit: strings.Repeat("a", 40)}
	m.data.Apps["shop"] = app
	m.data.ClickHouses["shop"] = ClickHouseResource{Network: "lidza-clickhouse-shop1", Password: containerClickHousePassword, Ready: true}
	m.mu.Unlock()
	previous := containerClickHouseService
	containerClickHouseService = func(_ context.Context, c ClickHouseResource) (clickhouseService, error) {
		return clickhouseService{mode: "container", database: containerClickHouseDatabase, client: clickhouseClient{base: ch.srv.URL, user: "lidza", password: c.Password}, files: fileBackups{dir: ch.files}}, nil
	}
	t.Cleanup(func() { containerClickHouseService = previous })
	previousPoll := clickhousePollInterval
	clickhousePollInterval = time.Millisecond
	t.Cleanup(func() { clickhousePollInterval = previousPoll })
	return m, ch
}

// nativeClickHouseDestination is a native agent with app_shop provisioned.
func nativeClickHouseDestination(t *testing.T) (*Manager, *fakeClickHouse) {
	t.Helper()
	m, f := newClickHouseHostFixture(t, nativeCHKeyFingerprint)
	account := NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}
	short, stop := context.WithTimeout(context.Background(), time.Second)
	_, _ = provisionNativeData(short, m, "shop", account, NativeDataRequest{ClickHouse: true})
	stop()
	f.learnAdmin(t, m)
	if _, err := provisionNativeData(context.Background(), m, "shop", account, NativeDataRequest{ClickHouse: true}); err != nil {
		t.Fatal(err)
	}
	return m, f.ch
}

func moveMux(m *Manager) *http.ServeMux {
	mux := http.NewServeMux()
	m.moveRoutes(mux)
	return mux
}

func TestMoveSourceCopiesManagedClickHouse(t *testing.T) {
	m, _ := containerClickHouseSource(t)
	source, err := m.moveSource(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	for _, blocker := range source.Blockers {
		if strings.Contains(blocker, "ClickHouse") {
			t.Fatalf("ClickHouse still blocks the move: %v", source.Blockers)
		}
	}
	ch := source.ClickHouse
	if ch == nil || !ch.Copyable || ch.Mode != "container" || ch.Database != "telemetry" || ch.Version != "26.8.15.10" || ch.Tables != 2 || ch.Rows != 1230 || ch.SizeBytes != 1230*16 || !slices.Equal(ch.EnvironmentKeys, ClickHouseEnvironmentKeys) {
		t.Fatalf("source %+v", ch)
	}
	// A service that was never ready blocks with a reason.
	m.mu.Lock()
	c := m.data.ClickHouses["shop"]
	c.Ready = false
	m.data.ClickHouses["shop"] = c
	m.mu.Unlock()
	source, _ = m.moveSource(context.Background(), "shop")
	if source.ClickHouse == nil || source.ClickHouse.Copyable || !slices.ContainsFunc(source.Blockers, func(b string) bool { return strings.Contains(b, "Managed ClickHouse is not ready") }) {
		t.Fatalf("not-ready service %+v %v", source.ClickHouse, source.Blockers)
	}
	var host MoveHost
	w := httptest.NewRecorder()
	moveMux(m).ServeHTTP(w, httptest.NewRequest("GET", "/v1/move/host", nil))
	if json.Unmarshal(w.Body.Bytes(), &host) != nil || !host.ClickHouse {
		t.Fatalf("host %s", w.Body)
	}
}

func exportClickHouse(t *testing.T, mux *http.ServeMux, query string) (MoveClickHouseExport, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/move/apps/shop/clickhouse/exports"+query, nil))
	if w.Code != 200 {
		t.Fatalf("export %d %s", w.Code, w.Body)
	}
	var export MoveClickHouseExport
	if err := json.Unmarshal(w.Body.Bytes(), &export); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/move/apps/shop/clickhouse/exports/"+export.ID, nil))
	if w.Code != 200 || w.Header().Get(MoveSHA256Header) != export.SHA256 || w.Header().Get("Content-Length") != strconv.FormatInt(export.Size, 10) {
		t.Fatalf("download %d %v", w.Code, w.Header())
	}
	sum := sha256.Sum256(w.Body.Bytes())
	if hex.EncodeToString(sum[:]) != export.SHA256 {
		t.Fatal("download does not match its digest")
	}
	return export, w.Body.Bytes()
}

func restoreClickHouse(mux *http.ServeMux, archive []byte, digest string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/move/apps/shop/clickhouse/restore?replace=1", bytes.NewReader(archive))
	req.ContentLength = int64(len(archive))
	req.Header.Set(MoveSHA256Header, digest)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestMoveClickHouseFromContainerToNativeHost(t *testing.T) {
	source, sourceCH := containerClickHouseSource(t)
	destination, targetCH := nativeClickHouseDestination(t)
	sourceMux, targetMux := moveMux(source), moveMux(destination)

	export, archive := exportClickHouse(t, sourceMux, "?format=backup")
	if export.Format != clickhouseFormatBackup || export.Database != "telemetry" || len(export.Tables) != 2 || export.Tables[0].Create != "" {
		t.Fatalf("export %+v", export)
	}
	if !slices.ContainsFunc(sourceCH.statements(), func(s string) bool { return strings.HasPrefix(s, "BACKUP DATABASE `telemetry` TO File('lidza-") }) {
		t.Fatalf("source statements %v", sourceCH.statements())
	}

	// A corrupted upload is refused before the destination database is touched.
	before := len(targetCH.statements())
	corrupt := bytes.Clone(archive)
	corrupt[len(corrupt)/2] ^= 0xff
	if w := restoreClickHouse(targetMux, corrupt, export.SHA256); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "SHA-256") {
		t.Fatalf("corrupt upload %d %s", w.Code, w.Body)
	}
	if w := restoreClickHouse(targetMux, archive[:len(archive)-1], export.SHA256); w.Code != http.StatusUnprocessableEntity && w.Code != 400 {
		t.Fatalf("short upload %d %s", w.Code, w.Body)
	}
	if after := targetCH.statements(); len(after) != before {
		t.Fatalf("the destination ran %v for a rejected upload", after[before:])
	}

	// Restored under the destination's own name; counts verified.
	w := restoreClickHouse(targetMux, archive, export.SHA256)
	if w.Code != 200 {
		t.Fatalf("restore %d %s", w.Code, w.Body)
	}
	var result ClickHouseRestoreResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Database != "app_shop" || result.Source != "telemetry" || result.Counts["events"] != 1200 || result.Counts["sessions"] != 30 || result.SHA256 != export.SHA256 {
		t.Fatalf("result %+v", result)
	}
	if !slices.ContainsFunc(targetCH.statements(), func(s string) bool {
		return strings.HasPrefix(s, "RESTORE DATABASE `telemetry` AS `app_shop` FROM Disk('backups', 'agent/lidza-")
	}) {
		t.Fatalf("restore statement %v", targetCH.statements())
	}
	if table := targetCH.table("app_shop", "events"); table == nil || table.Rows != 1200 {
		t.Fatalf("destination table %+v", table)
	}

	// A restore that loses rows fails verification.
	targetCH.mu.Lock()
	targetCH.loseRows = true
	targetCH.mu.Unlock()
	if w := restoreClickHouse(targetMux, archive, export.SHA256); w.Code != 409 || !strings.Contains(w.Body.String(), "row-count verification failed") {
		t.Fatalf("row loss %d %s", w.Code, w.Body)
	}
	targetCH.mu.Lock()
	targetCH.loseRows = false
	targetCH.mu.Unlock()

	// The per-table format moves to any release, also renaming.
	export, archive = exportClickHouse(t, sourceMux, "?format=native")
	if export.Format != clickhouseFormatNative {
		t.Fatalf("native export %+v", export)
	}
	if w := restoreClickHouse(targetMux, archive, export.SHA256); w.Code != 200 || !strings.Contains(w.Body.String(), `"format":"native"`) {
		t.Fatalf("native restore %d %s", w.Code, w.Body)
	}
	if table := targetCH.table("app_shop", "sessions"); table == nil || table.Rows != 30 || !strings.Contains(table.Create, "`app_shop`.sessions") {
		t.Fatalf("native table %+v", table)
	}

	// Removing exports leaves nothing behind on the source.
	rm := httptest.NewRecorder()
	sourceMux.ServeHTTP(rm, httptest.NewRequest("DELETE", "/v1/move/apps/shop/clickhouse/exports", nil))
	get := httptest.NewRecorder()
	sourceMux.ServeHTTP(get, httptest.NewRequest("GET", "/v1/move/apps/shop/clickhouse/exports/"+export.ID, nil))
	if rm.Code != 200 || get.Code != 404 {
		t.Fatalf("cleanup %d, download after cleanup %d", rm.Code, get.Code)
	}
}

func TestMoveClickHouseBackupFallsBackToNativeExport(t *testing.T) {
	source, sourceCH := containerClickHouseSource(t)
	// The source cannot write BACKUP files: its File() directory is gone.
	sourceCH.files = "/nonexistent/lidza-test"
	export, archive := exportClickHouse(t, moveMux(source), "?format=backup")
	if export.Format != clickhouseFormatNative || export.Fallback == "" {
		t.Fatalf("fallback %+v", export)
	}
	destination, targetCH := nativeClickHouseDestination(t)
	if w := restoreClickHouse(moveMux(destination), archive, export.SHA256); w.Code != 200 {
		t.Fatalf("restore %d %s", w.Code, w.Body)
	}
	if table := targetCH.table("app_shop", "events"); table == nil || table.Rows != 1200 {
		t.Fatalf("destination %+v", table)
	}
	// Strict exports report the failure instead.
	w := httptest.NewRecorder()
	moveMux(source).ServeHTTP(w, httptest.NewRequest("POST", "/v1/move/apps/shop/clickhouse/exports?format=backup&strict=1", nil))
	if w.Code != 409 {
		t.Fatalf("strict export %d %s", w.Code, w.Body)
	}
}

func TestMoveClickHousePrepareReportsTheDestination(t *testing.T) {
	destination, _ := nativeClickHouseDestination(t)
	w := httptest.NewRecorder()
	moveMux(destination).ServeHTTP(w, httptest.NewRequest("POST", "/v1/move/apps/shop/clickhouse/prepare", nil))
	var target MoveClickHouseTarget
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &target) != nil || target.Mode != "host" || target.Database != "app_shop" || target.Version != "26.8.15.10" || !target.Ready {
		t.Fatalf("prepare %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	moveMux(destination).ServeHTTP(w, httptest.NewRequest("GET", "/v1/move/apps/other/clickhouse", nil))
	if w.Code != 409 {
		t.Fatalf("unknown application %d", w.Code)
	}
	_, _ = io.Copy(io.Discard, w.Body)
}

func TestMoveClickHouseIncrementalFinalSync(t *testing.T) {
	source, sourceCH := containerClickHouseSource(t)
	destination, targetCH := nativeClickHouseDestination(t)
	sourceMux, targetMux := moveMux(source), moveMux(destination)

	// The bulk copy keeps its directory backup as the base on both sides.
	base, archive := exportClickHouse(t, sourceMux, "?format=directory&keep=1")
	if base.Format != clickhouseFormatDirectory || !clickhouseBackupDirName.MatchString(base.Backup) || base.BaseBackup != "" {
		t.Fatalf("base export %+v", base)
	}
	if _, err := os.Stat(filepath.Join(sourceCH.files, base.Backup, ".backup")); err != nil {
		t.Fatalf("the source did not keep the base backup: %v", err)
	}
	if record, ok := source.moveClickHouseBase("shop"); !ok || record.Name != base.Backup {
		t.Fatalf("source base record %+v %v", record, ok)
	}
	w := restoreClickHouse(targetMux, archive, base.SHA256)
	var result ClickHouseRestoreResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Backup != "" {
		t.Fatalf("a restore without keep must not keep: %d %s", w.Code, w.Body)
	}
	req := httptest.NewRequest("POST", "/v1/move/apps/shop/clickhouse/restore?replace=1&keep=1", bytes.NewReader(archive))
	req.ContentLength = int64(len(archive))
	req.Header.Set(MoveSHA256Header, base.SHA256)
	w = httptest.NewRecorder()
	targetMux.ServeHTTP(w, req)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Backup != base.Backup || result.Counts["events"] != 1200 {
		t.Fatalf("keep restore %d %s", w.Code, w.Body)
	}
	kept := filepath.Join(targetCH.disk, nativeCHBackupSubdir, base.Backup)
	if _, err := os.Stat(filepath.Join(kept, ".backup")); err != nil {
		t.Fatalf("the destination did not keep the base under the source's name: %v", err)
	}

	// Rows arrive on the source; the final sync exports only what the base lacks.
	sourceCH.mu.Lock()
	sourceCH.dbs["telemetry"]["events"].Rows = 1500
	sourceCH.recent = 300
	sourceCH.mu.Unlock()
	w = httptest.NewRecorder()
	sourceMux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/move/apps/shop/clickhouse?since="+base.Created.UTC().Format(time.RFC3339), nil))
	var target MoveClickHouseTarget
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &target) != nil || target.Delta == nil || target.Delta.Bytes != 300*16 || target.Delta.Parts != 1 {
		t.Fatalf("delta %d %s", w.Code, w.Body)
	}
	final, archive := exportClickHouse(t, sourceMux, "?format=directory&base="+base.Backup)
	if final.BaseBackup != base.Backup || final.Backup != "" || final.Tables[0].RowsAfter+final.Tables[1].RowsAfter != 1530 {
		t.Fatalf("incremental export %+v", final)
	}
	want := "BACKUP DATABASE `telemetry` TO File('" + base.Backup + "-final/') SETTINGS base_backup = File('" + base.Backup + "/')"
	if !slices.ContainsFunc(sourceCH.statements(), func(s string) bool { return strings.HasPrefix(s, want) }) {
		t.Fatalf("source statements %v", sourceCH.statements())
	}
	if _, err := os.Stat(filepath.Join(sourceCH.files, base.Backup, ".backup")); err != nil {
		t.Fatal("the incremental export removed the base")
	}
	if _, err := os.Stat(filepath.Join(sourceCH.files, base.Backup+"-final")); err == nil {
		t.Fatal("the incremental backup directory stays on the source after the export")
	}
	// An incremental archive is refused without its base, and with a base the destination lacks.
	if w := restoreClickHouse(targetMux, archive, final.SHA256); w.Code != 409 || !strings.Contains(w.Body.String(), "its base must be named") {
		t.Fatalf("incremental without base %d %s", w.Code, w.Body)
	}
	restoreIncremental := func(baseName string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/move/apps/shop/clickhouse/restore?replace=1&base="+baseName, bytes.NewReader(archive))
		req.ContentLength = int64(len(archive))
		req.Header.Set(MoveSHA256Header, final.SHA256)
		w := httptest.NewRecorder()
		targetMux.ServeHTTP(w, req)
		return w
	}
	before := len(targetCH.statements())
	if w := restoreIncremental("lidza-unknown"); w.Code != 409 || !strings.Contains(w.Body.String(), "no incremental restore") {
		t.Fatalf("unknown base %d %s", w.Code, w.Body)
	}
	if after := targetCH.statements(); len(after) != before {
		t.Fatalf("a refused incremental restore touched the database: %v", after[before:])
	}
	w = restoreIncremental(base.Backup)
	var incremental ClickHouseRestoreResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &incremental) != nil || incremental.Counts["events"] != 1500 || incremental.BaseBackup != base.Backup || incremental.Backup != "" {
		t.Fatalf("incremental restore %d %s", w.Code, w.Body)
	}
	want = "RESTORE DATABASE `telemetry` AS `app_shop` FROM Disk('backups', 'agent/" + base.Backup + "-final/') SETTINGS base_backup = Disk('backups', 'agent/" + base.Backup + "/')"
	if !slices.ContainsFunc(targetCH.statements(), func(s string) bool { return strings.HasPrefix(s, want) }) {
		t.Fatalf("restore statements %v", targetCH.statements())
	}
	if table := targetCH.table("app_shop", "events"); table == nil || table.Rows != 1500 {
		t.Fatalf("destination table %+v", table)
	}
	if _, err := os.Stat(filepath.Join(kept, ".backup")); err != nil {
		t.Fatal("the incremental restore removed the base")
	}
	if _, err := os.Stat(kept + "-final"); err == nil {
		t.Fatal("the incremental backup directory stays on the destination after the restore")
	}

	// Cleanup removes the base on both sides; without base=1 it stays.
	for _, mux := range []*http.ServeMux{sourceMux, targetMux} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("DELETE", "/v1/move/apps/shop/clickhouse/exports", nil))
		if w.Code != 200 {
			t.Fatalf("cleanup %d %s", w.Code, w.Body)
		}
	}
	if _, err := os.Stat(filepath.Join(sourceCH.files, base.Backup)); err != nil {
		t.Fatal("removing the exports removed the base")
	}
	for _, mux := range []*http.ServeMux{sourceMux, targetMux} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("DELETE", "/v1/move/apps/shop/clickhouse/exports?base=1", nil))
		if w.Code != 200 {
			t.Fatalf("cleanup %d %s", w.Code, w.Body)
		}
	}
	if _, err := os.Stat(filepath.Join(sourceCH.files, base.Backup)); err == nil {
		t.Fatal("the source's base was not removed")
	}
	if _, err := os.Stat(kept); err == nil {
		t.Fatal("the destination's base was not removed")
	}
	if _, ok := source.moveClickHouseBase("shop"); ok {
		t.Fatal("the base record was not removed")
	}
	// Without the base, an incremental export is refused so the control panel copies in full.
	w = httptest.NewRecorder()
	sourceMux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/move/apps/shop/clickhouse/exports?format=directory&base="+base.Backup, nil))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "no incremental export") {
		t.Fatalf("export without base %d %s", w.Code, w.Body)
	}
}

func TestMoveClickHouseFullExportReplacesTheKeptBase(t *testing.T) {
	source, sourceCH := containerClickHouseSource(t)
	mux := moveMux(source)
	base, _ := exportClickHouse(t, mux, "?format=directory&keep=1")
	if _, err := os.Stat(filepath.Join(sourceCH.files, base.Backup)); err != nil {
		t.Fatal(err)
	}
	full, _ := exportClickHouse(t, mux, "?format=directory")
	if full.Backup != "" {
		t.Fatalf("a plain export kept a base: %+v", full)
	}
	if _, err := os.Stat(filepath.Join(sourceCH.files, base.Backup)); err == nil {
		t.Fatal("the full export left the earlier base behind")
	}
	if _, ok := source.moveClickHouseBase("shop"); ok {
		t.Fatal("the base record outlived the base")
	}
	// Other formats never keep a base, and base needs the directory format.
	if export, _ := exportClickHouse(t, mux, "?format=backup&keep=1"); export.Backup != "" {
		t.Fatalf("a .zip export kept a base: %+v", export)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/move/apps/shop/clickhouse/exports?format=backup&base=lidza-x", nil))
	if w.Code != 400 {
		t.Fatalf("base with .zip format %d %s", w.Code, w.Body)
	}
}
