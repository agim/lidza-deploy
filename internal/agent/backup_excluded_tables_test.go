package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// excludedTablesFixture is a container database with three tables whose
// statistics the fake psql reports; public.events was never analyzed.
func excludedTablesFixture(t *testing.T) (*Manager, *[]string) {
	t.Helper()
	m := testManager(t, &fakeRuntime{})
	m.data.Databases["shop"] = Database{AppID: "shop", Mode: "local", Network: "lidza-db-shop", Ready: true, URL: "postgres://app:secret@lidza-db-shop:5432/app"}
	statements := []string{}
	previous := movePSQL
	t.Cleanup(func() { movePSQL = previous })
	tables := map[string]bool{"public.orders": true, "public.events": true, "audit.log_entries": true}
	movePSQL = func(_ context.Context, _ *Manager, id, sql string) (string, error) {
		statements = append(statements, sql)
		if id != "shop" {
			t.Fatalf("unexpected database %s", id)
		}
		switch {
		case strings.HasPrefix(sql, "SELECT count(*)"):
			return "3\n", nil
		case strings.Contains(sql, "reltuples") && strings.Contains(sql, "c.relname) IN ("):
			out := ""
			for _, line := range []string{"public.events\tr\t52000\t81920000", "public.orders\tr\t1200\t409600", "audit.log_entries\tr\t-1\t8192"} {
				if strings.Contains(sql, "'"+strings.Split(strings.Split(line, "\t")[0], ".")[1]+"'") {
					out += line + "\n"
				}
			}
			return out, nil
		case strings.Contains(sql, "reltuples"):
			return "public.events\tr\t52000\t81920000\npublic.orders\tr\t1200\t409600\naudit.log_entries\tr\t-1\t8192\nMixed.Case\tr\t3\t8192\npublic.parted\tp\t0\t0\n", nil
		case strings.HasPrefix(sql, "WITH t AS"): // no incremental key
			return "\t\t0\t0\t0\t0\n", nil
		case strings.Contains(sql, "c.relkind = 'r'"):
			out := ""
			for name := range tables {
				schema, table, _ := strings.Cut(name, ".")
				if strings.Contains(sql, "('"+schema+"'::name, '"+table+"'::name)") {
					out += name + "\n"
				}
			}
			return out, nil
		}
		t.Fatalf("unexpected statement %q", sql)
		return "", nil
	}
	return m, &statements
}

func tablesCall(t *testing.T, m *Manager, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, reader)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	w := httptest.NewRecorder()
	Handler(m).ServeHTTP(w, r)
	return w
}

// The panel lists a database's tables with estimates, saves the tables
// whose data backups skip, and the saved list survives a restart.
func TestBackupExcludedTablesRoundTrip(t *testing.T) {
	m, statements := excludedTablesFixture(t)
	w := tablesCall(t, m, "GET", "/v1/databases/shop/tables", "")
	var listing DatabaseTables
	if json.Unmarshal(w.Body.Bytes(), &listing); w.Code != 200 || len(listing.Tables) != 5 || listing.Total != 5 || listing.Truncated || len(listing.Excluded) != 0 {
		t.Fatalf("listing: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains((*statements)[0], "n_live_tup") || !strings.Contains((*statements)[0], "pg_total_relation_size") {
		t.Fatal("the listing does not estimate unanalyzed tables or measure sizes:", (*statements)[0])
	}
	byName := map[string]DatabaseTable{}
	for _, table := range listing.Tables {
		byName[table.Name] = table
	}
	if e := byName["public.events"]; e.Rows != 52000 || e.Bytes != 81920000 || !e.Excludable || e.Excluded {
		t.Fatalf("events: %+v", e)
	}
	if byName["audit.log_entries"].Rows != -1 || byName["Mixed.Case"].Excludable || byName["public.parted"].Excludable {
		t.Fatalf("listing flags: %+v", listing.Tables)
	}

	if w := tablesCall(t, m, "PATCH", "/v1/databases/shop/backups/excluded-tables", `{"tables":["public.events","audit.log_entries"]}`); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if got := m.backupExcludedTables("shop"); !slices.Equal(got, []string{"audit.log_entries", "public.events"}) {
		t.Fatal("saved", got)
	}
	view := m.databaseViews()[0]
	if !slices.Equal(view.ExcludedTables, []string{"audit.log_entries", "public.events"}) || !view.ExcludedTablesSupported {
		t.Fatalf("view: %+v", view)
	}
	w = tablesCall(t, m, "GET", "/v1/databases/shop/tables", "")
	json.Unmarshal(w.Body.Bytes(), &listing)
	for _, table := range listing.Tables {
		if table.Excluded != (table.Name == "public.events" || table.Name == "audit.log_entries") {
			t.Fatalf("excluded flag: %+v", table)
		}
	}
	// A move measures the same estimates in one query.
	measured := m.moveExcludedTables(context.Background(), "shop")
	if len(measured) != 2 || measured[0].Name != "audit.log_entries" || measured[0].Bytes != 8192 || measured[1].Rows != 52000 {
		t.Fatalf("move measure: %+v", measured)
	}

	reopened, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.backupExcludedTables("shop"); !slices.Equal(got, []string{"audit.log_entries", "public.events"}) {
		t.Fatal("not persisted", got)
	}
}

func TestBackupExcludedTablesValidation(t *testing.T) {
	m, _ := excludedTablesFixture(t)
	for body, code := range map[string]int{
		`{"tables":["public.missing"]}`:                                    409,
		`{"tables":["public.events","public.orders","audit.log_entries"]}`: 409,
		`{"tables":["public.*"]}`:                                          400,
		`{"tables":["events"]}`:                                            400,
		`{"tables":["public.events","public.events"]}`:                     400,
		`{"tables":["public.events"],"extra":true}`:                        400,
	} {
		w := tablesCall(t, m, "PATCH", "/v1/databases/shop/backups/excluded-tables", body)
		if w.Code != code {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
		if strings.Contains(body, "missing") && !strings.Contains(w.Body.String(), "public.missing") {
			t.Fatal("the missing table is not named:", w.Body.String())
		}
		if code == 409 && strings.Contains(body, "orders") && !strings.Contains(w.Body.String(), "at least one table") {
			t.Fatal("excluding every table was not explained:", w.Body.String())
		}
	}
	if m.data.Databases["shop"].ExcludedTables != nil {
		t.Fatal("a refused list was saved")
	}
	if w := tablesCall(t, m, "PATCH", "/v1/databases/unknown/backups/excluded-tables", `{"tables":[]}`); w.Code != 409 {
		t.Fatalf("unknown database: %d", w.Code)
	}
	// Native resources always back up every table.
	m.mu.Lock()
	m.data.Databases["native-db"] = Database{AppID: "native-db", Mode: "external", Ready: true, URL: "postgres://app:secret@db/app"}
	if m.data.NativeDatabases == nil {
		m.data.NativeDatabases = map[string]NativeDatabaseBinding{}
	}
	m.data.NativeDatabases["native-db"] = NativeDatabaseBinding{}
	m.mu.Unlock()
	if w := tablesCall(t, m, "PATCH", "/v1/databases/native-db/backups/excluded-tables", `{"tables":["public.events"]}`); w.Code != 409 || !strings.Contains(w.Body.String(), "every table") {
		t.Fatalf("native: %d %s", w.Code, w.Body.String())
	}
	if w := tablesCall(t, m, "GET", "/v1/databases/native-db/tables", ""); w.Code != 409 {
		t.Fatalf("native listing: %d", w.Code)
	}
	for _, view := range m.databaseViews() {
		if view.AppID == "native-db" && view.ExcludedTablesSupported {
			t.Fatal("native database offered exclusions")
		}
	}
}

// The configuration key is the legacy default: it applies until the panel
// saves a list, and an empty saved list makes backups complete again.
func TestBackupExcludedTablesLegacyConfigFallback(t *testing.T) {
	m, _ := excludedTablesFixture(t)
	m.cfg.BackupExcludeTableData = map[string][]string{"shop": {"public.events"}}
	if got := m.backupExcludedTables("shop"); !slices.Equal(got, []string{"public.events"}) {
		t.Fatal("legacy default ignored", got)
	}
	if got := m.databaseViews()[0].ExcludedTables; !slices.Equal(got, []string{"public.events"}) {
		t.Fatal("view lacks the legacy default", got)
	}
	if w := tablesCall(t, m, "PATCH", "/v1/databases/shop/backups/excluded-tables", `{"tables":[]}`); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	if got := m.backupExcludedTables("shop"); len(got) != 0 {
		t.Fatal("an empty saved list fell back to the configuration", got)
	}
	if w := tablesCall(t, m, "PATCH", "/v1/databases/shop/backups/excluded-tables", `{"tables":["public.orders"]}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if got := m.backupExcludedTables("shop"); !slices.Equal(got, []string{"public.orders"}) {
		t.Fatal("saved list does not replace the configuration", got)
	}
}

// A container backup passes the saved list to pg_dump and records it.
func TestBackupDumpSkipsTheSavedTables(t *testing.T) {
	bin := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\ncase \"$1\" in\nrun) printf '%s\\n' \"$@\" > " + argsFile + "; printf 'fixture-dump';;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	m := testManager(t, &fakeRuntime{})
	m.cfg.BackupExcludeTableData = map[string][]string{"shop": {"public.orders"}}
	saved := []string{"public.events"}
	d := Database{AppID: "shop", Mode: "local", Network: "lidza-db-shop", Ready: true, URL: "postgres://app:secret@lidza-db-shop:5432/app", ExcludedTables: &saved}
	m.mu.Lock()
	m.data.Databases["shop"] = d
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	record, err := m.dumpDatabase(ctx, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if !slices.Contains(args, "--exclude-table-data=public.events") || slices.Contains(args, "--exclude-table-data=public.orders") {
		t.Fatal("pg_dump arguments", args)
	}
	if !slices.Equal(record.ExcludedTableData, []string{"public.events"}) {
		t.Fatal("record", record.ExcludedTableData)
	}
}
