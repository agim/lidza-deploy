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
	"strconv"
	"strings"
	"testing"
	"time"
)

func moveRequest(t *testing.T, h http.Handler, method, path string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if b, ok := body.(*bytes.Reader); ok {
		req.ContentLength = int64(b.Len())
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestMoveEndpointsRequireTheAgentKey(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	h := Handler(m)
	for _, route := range []struct{ method, path string }{
		{"GET", "/v1/move/host"},
		{"GET", "/v1/move/apps/source"},
		{"GET", "/v1/move/apps/source/environment"},
		{"POST", "/v1/move/apps/source/health"},
		{"GET", "/v1/move/databases/source/tables"},
		{"GET", "/v1/move/databases/source/backups/b1"},
		{"POST", "/v1/move/databases/source/restore"},
	} {
		for _, authorization := range []string{"", "Bearer wrong-key-wrong-key-wrong-key-wrong"} {
			req := httptest.NewRequest(route.method, route.path, nil)
			if authorization != "" {
				req.Header.Set("Authorization", authorization)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s with %q: status %d", route.method, route.path, authorization, w.Code)
			}
		}
	}
}

func TestMoveEnvironmentExportsValuesOnlyToTheKeyHolder(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("source")
	a.Env["PUBLIC_NAME"] = "Example"
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	stored := m.data.Apps["source"]
	stored.Env[gatewayEnvPrefix+"KEY"] = "agent-owned"
	m.data.Apps["source"] = stored
	m.mu.Unlock()
	w := moveRequest(t, Handler(m), "GET", "/v1/move/apps/source/environment", nil, nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, cache %q: %s", w.Code, w.Header().Get("Cache-Control"), w.Body)
	}
	var env MoveEnvironment
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Values["APP_SECRET"] != "runtime-secret" || env.Values["PUBLIC_NAME"] != "Example" || env.Native {
		t.Fatalf("unexpected export %+v", env)
	}
	if _, leaked := env.Values[gatewayEnvPrefix+"KEY"]; leaked {
		t.Fatal("gateway transport keys must stay on the host")
	}
	if env.Kinds["APP_SECRET"] != EnvironmentCredential {
		t.Fatalf("kinds not exported: %+v", env.Kinds)
	}
	if w := moveRequest(t, Handler(m), "GET", "/v1/move/apps/missing/environment", nil, nil); w.Code != 409 {
		t.Fatalf("missing application: %d", w.Code)
	}
}

func TestMoveSourceReportsDatabasesWithoutSecrets(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	previous := moveTableCount
	moveTableCount = func(context.Context, *Manager, string) (int, error) { return 7, nil }
	t.Cleanup(func() { moveTableCount = previous })
	a := testApp("source")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	stored := m.data.Apps["source"]
	stored.Current = &Release{Commit: "0123456789abcdef0123456789abcdef01234567"}
	stored.Bindings = map[string]string{"DATABASE_URL": "source"}
	stored.Env["DATABASE_URL"] = "postgres://app:database-password@lidza-db-source:5432/app"
	m.data.Apps["source"] = stored
	m.data.Databases["source"] = Database{AppID: "source", Mode: "local", Network: "lidza-db-source", Ready: true, URL: "postgres://app:database-password@lidza-db-source:5432/app", AdminPassword: "admin-secret", Backup: BackupPolicy{Hours: 24, Keep: 1}}
	m.data.StorageVolumes["source"] = "lidza-storage-source"
	m.mu.Unlock()
	w := moveRequest(t, Handler(m), "GET", "/v1/move/apps/source", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	for _, secret := range []string{"runtime-secret", "database-password", "admin-secret"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("source inventory leaked %s", secret)
		}
	}
	var source MoveSource
	if err := json.Unmarshal(w.Body.Bytes(), &source); err != nil {
		t.Fatal(err)
	}
	if len(source.Databases) != 1 || !source.Databases[0].Copyable || source.Databases[0].Tables != 7 || source.Databases[0].Mode != "local" {
		t.Fatalf("unexpected databases %+v", source.Databases)
	}
	if source.Commit == "" || source.Container == nil || source.Container.Env != nil || len(source.Volumes) != 1 || len(source.Blockers) != 0 {
		t.Fatalf("unexpected source %+v", source)
	}
}

func writeMoveBackup(t *testing.T, m *Manager, database string, content []byte) BackupRecord {
	t.Helper()
	if err := os.MkdirAll(m.backupDir(database), 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	record := BackupRecord{ID: "b1", Created: time.Now().UTC(), Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
	if err := os.WriteFile(filepath.Join(m.backupDir(database), record.ID+".dump"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	d := m.data.Databases[database]
	d.Backups = append(d.Backups, record)
	m.data.Databases[database] = d
	m.mu.Unlock()
	return record
}

func TestMoveBackupDownloadCarriesTheRecordedDigest(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.mu.Lock()
	m.data.Databases["source"] = Database{AppID: "source", Mode: "local", Network: "lidza-db-source", Ready: true}
	m.mu.Unlock()
	content := []byte("PGDMP custom archive bytes")
	record := writeMoveBackup(t, m, "source", content)
	w := moveRequest(t, Handler(m), "GET", "/v1/move/databases/source/backups/b1", nil, nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), content) {
		t.Fatalf("status %d body %q", w.Code, w.Body)
	}
	if w.Header().Get(MoveSHA256Header) != record.SHA256 || w.Header().Get("Content-Length") != strconv.Itoa(len(content)) {
		t.Fatalf("headers %v", w.Header())
	}
	if w := moveRequest(t, Handler(m), "GET", "/v1/move/databases/source/backups/unknown", nil, nil); w.Code != 404 {
		t.Fatalf("unknown backup: %d", w.Code)
	}
	// A file that no longer matches its record is never served.
	if err := os.WriteFile(filepath.Join(m.backupDir("source"), "b1.dump"), content[:5], 0o600); err != nil {
		t.Fatal(err)
	}
	if w := moveRequest(t, Handler(m), "GET", "/v1/move/databases/source/backups/b1", nil, nil); w.Code != 409 {
		t.Fatalf("truncated backup: %d", w.Code)
	}
}

func TestMoveRestoreVerifiesTheUploadBeforeRestoring(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.mu.Lock()
	m.data.Databases["target"] = Database{AppID: "target", Mode: "local", Network: "lidza-db-target", Ready: true, URL: "postgres://app:pw@lidza-db-target:5432/app"}
	m.mu.Unlock()
	tables := 0
	var restored [][]byte
	var replaced []bool
	previousCount, previousRestore, previousLimit := moveTableCount, moveRestoreDump, moveMaxDumpSize
	moveTableCount = func(context.Context, *Manager, string) (int, error) { return tables, nil }
	moveRestoreDump = func(_ context.Context, m *Manager, target moveDatabaseTarget, dump *os.File, replace bool) error {
		if target.Database.Operation != "restore" {
			t.Error("restore runs while the database is reserved")
		}
		data, _ := io.ReadAll(dump)
		restored = append(restored, data)
		replaced = append(replaced, replace)
		return nil
	}
	moveMaxDumpSize = 1 << 20
	t.Cleanup(func() {
		moveTableCount, moveRestoreDump, moveMaxDumpSize = previousCount, previousRestore, previousLimit
	})
	content := []byte("PGDMP verified archive")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	h := Handler(m)

	if w := moveRequest(t, h, "POST", "/v1/move/databases/target/restore", bytes.NewReader(content), nil); w.Code != 400 {
		t.Fatalf("missing digest: %d", w.Code)
	}
	wrong := sha256.Sum256([]byte("something else"))
	if w := moveRequest(t, h, "POST", "/v1/move/databases/target/restore", bytes.NewReader(content), map[string]string{MoveSHA256Header: hex.EncodeToString(wrong[:])}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("checksum mismatch: %d %s", w.Code, w.Body)
	}
	if len(restored) != 0 {
		t.Fatal("a mismatching dump was restored")
	}
	oversized := bytes.NewReader(make([]byte, 2<<20))
	if w := moveRequest(t, h, "POST", "/v1/move/databases/target/restore", oversized, map[string]string{MoveSHA256Header: digest}); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d", w.Code)
	}
	req := httptest.NewRequest("POST", "/v1/move/databases/target/restore", strings.NewReader(string(content)))
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	req.Header.Set(MoveSHA256Header, digest)
	req.ContentLength = -1
	unknownLength := httptest.NewRecorder()
	h.ServeHTTP(unknownLength, req)
	if unknownLength.Code != http.StatusLengthRequired {
		t.Fatalf("unknown length: %d", unknownLength.Code)
	}
	tables = 3
	if w := moveRequest(t, h, "POST", "/v1/move/databases/target/restore", bytes.NewReader(content), map[string]string{MoveSHA256Header: digest}); w.Code != 409 {
		t.Fatalf("non-empty destination without replace: %d", w.Code)
	}
	if w := moveRequest(t, h, "POST", "/v1/move/databases/target/restore?replace=1", bytes.NewReader(content), map[string]string{MoveSHA256Header: digest}); w.Code != 200 {
		t.Fatalf("verified restore: %d %s", w.Code, w.Body)
	}
	if len(restored) != 1 || !bytes.Equal(restored[0], content) || !replaced[0] {
		t.Fatalf("restore calls %q %v", restored, replaced)
	}
	m.mu.Lock()
	after := m.data.Databases["target"]
	m.mu.Unlock()
	if after.Operation != "" {
		t.Fatal("the database stays reserved after the restore")
	}
	entries, _ := os.ReadDir(m.backupDir("target"))
	if len(entries) != 0 {
		t.Fatalf("uploads must not remain on disk: %v", entries)
	}
	if w := moveRequest(t, h, "POST", "/v1/move/databases/unknown/restore", bytes.NewReader(content), map[string]string{MoveSHA256Header: digest}); w.Code != 409 {
		t.Fatalf("unknown database: %d", w.Code)
	}
}

func TestMoveHostReportsAddresses(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	previous := moveDetectAddresses
	moveDetectAddresses = func() NetworkInfo { return NetworkInfo{IPv4: "203.0.113.10", IPv6: "2001:db8::10"} }
	t.Cleanup(func() { moveDetectAddresses = previous })
	w := moveRequest(t, Handler(m), "GET", "/v1/move/host", nil, nil)
	var host MoveHost
	if err := json.Unmarshal(w.Body.Bytes(), &host); err != nil || host.IPv4 != "203.0.113.10" || host.IPv6 != "2001:db8::10" || !host.HostData {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestMoveHealthChecksTheRunningRelease(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if err := m.Upsert(testApp("target")); err != nil {
		t.Fatal(err)
	}
	var health MoveHealth
	w := moveRequest(t, Handler(m), "POST", "/v1/move/apps/target/health", nil, nil)
	if json.Unmarshal(w.Body.Bytes(), &health) != nil || health.Healthy {
		t.Fatalf("an undeployed application is not healthy: %s", w.Body)
	}
	m.mu.Lock()
	a := m.data.Apps["target"]
	a.Current = &Release{ID: "r1", Port: "12345"}
	m.data.Apps["target"] = a
	m.mu.Unlock()
	w = moveRequest(t, Handler(m), "POST", "/v1/move/apps/target/health", nil, nil)
	if json.Unmarshal(w.Body.Bytes(), &health) != nil || !health.Healthy {
		t.Fatalf("running release: %s", w.Body)
	}
}

func TestNativeCreatedSpecDropsSecrets(t *testing.T) {
	spec := nativeCreatedSpec(NativeCreateRequest{ID: "app", Env: map[string]string{"SECRET": "x"}, EnvKinds: map[string]string{"SECRET": "credential"},
		Data: NativeDataRequest{Postgres: false, ExternalDatabaseURL: "postgres://user:password@db/app"}, Processes: []NativeProcessSpec{{Name: "web", Command: "bin/web", Web: true}}})
	encoded, _ := json.Marshal(spec)
	if strings.Contains(string(encoded), "password") || strings.Contains(string(encoded), `"x"`) || len(spec.Processes) != 1 {
		t.Fatalf("recorded spec %s", encoded)
	}
}
