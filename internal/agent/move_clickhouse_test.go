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
