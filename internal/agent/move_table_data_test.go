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
	"strings"
	"testing"
)

// TestMoveTableDataRoundTrip dumps the rows of tables the backups leave out
// on one agent and loads them on another, with exact row counts.
func TestMoveTableDataRoundTrip(t *testing.T) {
	source, target := testManager(t, &fakeRuntime{}), testManager(t, &fakeRuntime{})
	for _, m := range []*Manager{source, target} {
		m.data.Databases["shop"] = Database{AppID: "shop", Mode: "local", Network: "lidza-db-shop", Ready: true, URL: "postgres://app:secret@lidza-db-shop:5432/app"}
	}
	source.cfg.BackupExcludeTableData = map[string][]string{"shop": {"public.events"}}
	var truncated []string
	previousPSQL, previousDump, previousRestore := movePSQL, moveDumpTableData, moveRestoreTableData
	t.Cleanup(func() {
		movePSQL, moveDumpTableData, moveRestoreTableData = previousPSQL, previousDump, previousRestore
	})
	movePSQL = func(_ context.Context, _ *Manager, _, sql string) (string, error) {
		switch {
		case strings.Contains(sql, "to_regclass"):
			return "t", nil
		case strings.HasPrefix(sql, "SELECT count(*)"):
			return "1200", nil
		case strings.Contains(sql, "reltuples"):
			return "public.events\tr\t1180\t4096000\n", nil
		case strings.HasPrefix(sql, "TRUNCATE"):
			truncated = append(truncated, sql)
			return "", nil
		}
		t.Fatalf("unexpected statement %q", sql)
		return "", nil
	}
	moveDumpTableData = func(_ context.Context, _ *Manager, _ string, tables []string, out io.Writer) error {
		_, err := out.Write([]byte("data-only dump of " + strings.Join(tables, ",")))
		return err
	}
	var loaded []byte
	moveRestoreTableData = func(_ context.Context, _ *Manager, _ moveDatabaseTarget, _ []string, dump *os.File) error {
		var err error
		loaded, err = io.ReadAll(dump)
		return err
	}
	if tables := source.moveExcludedTables(context.Background(), "shop"); len(tables) != 1 || tables[0].Rows != 1180 || tables[0].Bytes != 4096000 {
		t.Fatalf("measure: %+v", tables)
	}
	call := func(h http.Handler, method, path string, body io.Reader, header map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, body)
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := call(Handler(source), "POST", "/v1/move/databases/shop/table-data", strings.NewReader(`{"tables":["public.events"]}`), nil)
	var export MoveTableDataExport
	if json.Unmarshal(w.Body.Bytes(), &export); w.Code != 200 || export.Rows["public.events"] != 1200 || export.Size == 0 {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	w = call(Handler(source), "GET", "/v1/move/databases/shop/table-data/"+export.ID, nil, nil)
	sum := sha256.Sum256(w.Body.Bytes())
	if w.Code != 200 || hex.EncodeToString(sum[:]) != export.SHA256 {
		t.Fatalf("download: %d", w.Code)
	}
	dump := w.Body.Bytes()
	w = call(Handler(target), "POST", "/v1/move/databases/shop/table-data/restore?tables=public.events", bytes.NewReader(dump), map[string]string{MoveSHA256Header: export.SHA256})
	var restored MoveTableRows
	if json.Unmarshal(w.Body.Bytes(), &restored); w.Code != 200 || restored.Rows["public.events"] != 1200 || !bytes.Equal(loaded, dump) || w.Header().Get(MoveVerifiedHeader) != export.SHA256 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	for _, bad := range []string{`{"tables":["events; DROP TABLE x"]}`, `{"tables":[]}`} {
		if w := call(Handler(source), "POST", "/v1/move/databases/shop/table-data", strings.NewReader(bad), nil); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
}
