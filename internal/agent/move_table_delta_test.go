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
	"strconv"
	"strings"
	"testing"
)

// fakeKeyTable answers the key query and the max/count statements for one
// table of a fake database, so tests choose its shape and its writes.
type fakeKeyTable struct {
	column, kind   string
	updates, dels  int64
	inserts, age   int64
	maximum        string
	total, matched int64
}

func (f *fakeKeyTable) psql(sql string) (string, bool) {
	sql = strings.TrimPrefix(sql, moveSessionPins)
	switch {
	case strings.HasPrefix(sql, "WITH t AS"):
		return strings.Join([]string{f.column, f.kind, itoa(f.updates), itoa(f.dels), itoa(f.inserts), itoa(f.age)}, "\t") + "\n", true
	case strings.HasPrefix(sql, "SELECT ") && strings.Contains(sql, "max(") && !strings.HasPrefix(sql, "SELECT count"):
		return f.maximum + "\n", true
	case strings.HasPrefix(sql, "SELECT count(*), coalesce("):
		return itoa(f.total) + "\t" + f.maximum + "\n", true
	case strings.HasPrefix(sql, "SELECT count(*) FROM") && strings.Contains(sql, " WHERE "):
		return itoa(f.matched) + "\n", true
	}
	return "", false
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestMoveTableKeyDetection(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.data.Databases["shop"] = Database{AppID: "shop", Mode: "local", Network: "lidza-db-shop", Ready: true, URL: "postgres://app:secret@lidza-db-shop:5432/app"}
	previous := movePSQL
	t.Cleanup(func() { movePSQL = previous })
	var table *fakeKeyTable
	movePSQL = func(_ context.Context, _ *Manager, _, sql string) (string, error) {
		if out, ok := table.psql(sql); ok {
			return out, nil
		}
		t.Fatalf("unexpected statement %q", sql)
		return "", nil
	}
	cases := []struct {
		name  string
		table fakeKeyTable
		want  *MoveTableKey
	}{
		{"integer primary key", fakeKeyTable{column: "id", kind: "integer", maximum: "48213", inserts: 48213, age: 3 * 86400}, &MoveTableKey{Column: "id", Kind: "integer", Watermark: "48213", RowsPerHour: 48213.0 / 72, RateBasis: "the source's insert statistics over 3 days"}},
		{"indexed timestamp", fakeKeyTable{column: "occurred_at", kind: "timestamptz", maximum: "2026-11-06T09:59:58.250000", inserts: 120, age: 7200}, &MoveTableKey{Column: "occurred_at", Kind: "timestamptz", Watermark: "2026-11-06T09:59:58.250000", RowsPerHour: 60, RateBasis: "the source's insert statistics over 2 hours"}},
		{"timestamp without an index, or none configured", fakeKeyTable{column: "", kind: "", inserts: 10, age: 7200}, nil},
		{"empty table keeps its key without a watermark", fakeKeyTable{column: "created_at", kind: "timestamp", maximum: ""}, &MoveTableKey{Column: "created_at", Kind: "timestamp"}},
		{"a watermark the key cannot quote", fakeKeyTable{column: "created_at", kind: "timestamp", maximum: "2026-11-06 10:00:00+00"}, nil},
	}
	for _, tc := range cases {
		table = &tc.table
		got, err := m.moveTableKeyOf(context.Background(), "shop", "public.events")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
			t.Fatalf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
	// The key is dropped when the table was written to in other ways than
	// appends while the dump ran, and the counters after the dump are kept.
	table = &fakeKeyTable{column: "id", kind: "integer", maximum: "10", updates: 3, dels: 0}
	keys := m.moveKeysAfterDump(context.Background(), "shop", map[string]MoveTableKey{"public.events": {Column: "id", Kind: "integer", Watermark: "10", Updates: 3}})
	if key := keys["public.events"]; key.Column != "id" || key.Updates != 3 {
		t.Fatalf("unchanged counters keep the key: %+v", keys)
	}
	table.updates = 4
	if keys := m.moveKeysAfterDump(context.Background(), "shop", map[string]MoveTableKey{"public.events": {Column: "id", Kind: "integer", Watermark: "10", Updates: 3}}); keys != nil {
		t.Fatalf("an update during the dump drops the key: %+v", keys)
	}
}

func TestMoveOnlyAppended(t *testing.T) {
	key := MoveTableKey{Column: "occurred_at", Kind: "timestamptz", Updates: 2, Deletes: 5}
	for _, tc := range []struct {
		now  moveTableStats
		want string
	}{
		{moveTableStats{Column: "occurred_at", Kind: "timestamptz", Updates: 2, Deletes: 5}, ""},
		{moveTableStats{Column: "occurred_at", Kind: "timestamptz", Updates: 3, Deletes: 5}, "1 rows were updated since the bulk copy"},
		{moveTableStats{Column: "occurred_at", Kind: "timestamptz", Updates: 2, Deletes: 9}, "4 rows were deleted since the bulk copy"},
		{moveTableStats{Column: "occurred_at", Kind: "timestamptz", Updates: 0, Deletes: 0}, "the source's table statistics were reset"},
		{moveTableStats{Column: "id", Kind: "integer", Updates: 2, Deletes: 5}, "its incremental key changed"},
	} {
		if got := moveOnlyAppended(key, tc.now); got != tc.want {
			t.Fatalf("%+v: %q, want %q", tc.now, got, tc.want)
		}
	}
	for _, tc := range []struct {
		key  MoveTableKey
		want string
	}{
		{MoveTableKey{Column: "id", Kind: "integer", Watermark: "48213"}, `"id" > 48213::bigint`},
		{MoveTableKey{Column: "occurred_at", Kind: "timestamptz", Watermark: "2026-11-06T10:00:00.500000"}, `"occurred_at" >= ('2026-11-06T10:00:00.500000'::timestamp AT TIME ZONE 'UTC')`},
		{MoveTableKey{Column: "created_at", Kind: "timestamp", Watermark: "2026-11-06T10:00:00.000000"}, `"created_at" >= '2026-11-06T10:00:00.000000'::timestamp`},
		{MoveTableKey{Column: "id", Kind: "integer"}, "true"},
	} {
		if got := moveDeltaCondition(tc.key); got != tc.want {
			t.Fatalf("%+v: %s", tc.key, got)
		}
	}
	for _, bad := range []MoveTableKey{{Column: "id; DROP", Kind: "integer", Watermark: "1"}, {Column: "id", Kind: "integer", Watermark: "1 OR true"}, {Column: "ts", Kind: "timestamptz", Watermark: "now()"}, {Column: "ts", Kind: "timestamptz", Watermark: "2026-11-06 10:00:00+00"}, {Column: "ts", Kind: "money", Watermark: ""}} {
		if validMoveKey(bad) == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
}

// TestMoveTableDeltaRoundTrip exports the rows added since a watermark on
// one agent, loads them on another and verifies the totals; a table that
// was deleted from since the bulk copy asks for the full copy instead.
func TestMoveTableDeltaRoundTrip(t *testing.T) {
	source, target := testManager(t, &fakeRuntime{}), testManager(t, &fakeRuntime{})
	for _, m := range []*Manager{source, target} {
		m.data.Databases["shop"] = Database{AppID: "shop", Mode: "local", Network: "lidza-db-shop", Ready: true, URL: "postgres://app:secret@lidza-db-shop:5432/app"}
	}
	sourceTable := &fakeKeyTable{column: "occurred_at", kind: "timestamptz", maximum: "2026-11-06T12:00:00.000000", total: 1500, matched: 301}
	targetTable := &fakeKeyTable{column: "occurred_at", kind: "timestamptz", maximum: "2026-11-06T10:00:00.500000", total: 1200}
	previousPSQL, previousCopy, previousLoad := movePSQL, moveCopyTableDelta, moveLoadTableDelta
	t.Cleanup(func() { movePSQL, moveCopyTableDelta, moveLoadTableDelta = previousPSQL, previousCopy, previousLoad })
	movePSQL = func(_ context.Context, m *Manager, _, sql string) (string, error) {
		table := sourceTable
		if m == target {
			table = targetTable
		}
		if out, ok := table.psql(sql); ok {
			return out, nil
		}
		t.Fatalf("unexpected statement %q", sql)
		return "", nil
	}
	var copied []string
	moveCopyTableDelta = func(_ context.Context, _ *Manager, _, table, condition, order string, out io.Writer) error {
		copied = append(copied, table+" WHERE "+condition+" ORDER BY "+order)
		_, err := out.Write([]byte("PGCOPY binary rows"))
		return err
	}
	var loaded []byte
	var loadCondition string
	moveLoadTableDelta = func(_ context.Context, _ *Manager, _ moveDatabaseTarget, _, condition string, data *os.File) error {
		var err error
		loadCondition = condition
		loaded, err = io.ReadAll(data)
		// The load replaces the destination's rows from the watermark on.
		targetTable.total, targetTable.maximum = 1500, "2026-11-06T12:00:00.000000"
		return err
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
	key := MoveTableKey{Column: "occurred_at", Kind: "timestamptz", Watermark: "2026-11-06T10:00:00.500000"}
	request, _ := json.Marshal(MoveTableDeltaRequest{Table: "public.events", Key: key})
	w := call(Handler(source), "POST", "/v1/move/databases/shop/table-delta", bytes.NewReader(request), nil)
	var export MoveTableDeltaExport
	if json.Unmarshal(w.Body.Bytes(), &export); w.Code != 200 || export.Fallback != "" || export.Rows != 301 || export.Total != 1500 || export.Max != "2026-11-06T12:00:00.000000" || export.Size == 0 {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	if len(copied) != 1 || copied[0] != `public.events WHERE "occurred_at" >= ('2026-11-06T10:00:00.500000'::timestamp AT TIME ZONE 'UTC') ORDER BY occurred_at` {
		t.Fatalf("copied %v", copied)
	}
	w = call(Handler(source), "GET", "/v1/move/databases/shop/table-delta/"+export.ID, nil, nil)
	sum := sha256.Sum256(w.Body.Bytes())
	if w.Code != 200 || hex.EncodeToString(sum[:]) != export.SHA256 {
		t.Fatalf("download: %d", w.Code)
	}
	data := w.Body.Bytes()
	w = call(Handler(target), "POST", MoveTableDeltaRestorePath("shop", "public.events", key), bytes.NewReader(data), map[string]string{MoveSHA256Header: export.SHA256})
	var result MoveTableDeltaResult
	if json.Unmarshal(w.Body.Bytes(), &result); w.Code != 200 || result.Total != 1500 || result.Max != export.Max || !bytes.Equal(loaded, data) || w.Header().Get(MoveVerifiedHeader) != export.SHA256 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	if loadCondition != `"occurred_at" >= ('2026-11-06T10:00:00.500000'::timestamp AT TIME ZONE 'UTC')` {
		t.Fatalf("the load deletes the rows at the watermark first: %s", loadCondition)
	}
	// Rows deleted since the bulk copy: no delta, the full copy follows.
	sourceTable.dels = 7
	w = call(Handler(source), "POST", "/v1/move/databases/shop/table-delta", bytes.NewReader(request), nil)
	var fallback MoveTableDeltaExport
	if json.Unmarshal(w.Body.Bytes(), &fallback); w.Code != 200 || fallback.Fallback != "7 rows were deleted since the bulk copy" || fallback.ID != "" {
		t.Fatalf("fallback: %d %s", w.Code, w.Body.String())
	}
	for _, bad := range []string{`{"table":"events","key":{"column":"id","kind":"integer","watermark":"1"}}`, `{"table":"public.events","key":{"column":"id","kind":"integer","watermark":"1 OR 1=1"}}`} {
		if w := call(Handler(source), "POST", "/v1/move/databases/shop/table-delta", strings.NewReader(bad), nil); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	if w := call(Handler(target), "POST", "/v1/move/databases/shop/table-delta/restore?table=public.events&column=ts&kind=timestamptz&after=now()", bytes.NewReader(data), nil); w.Code != 400 {
		t.Fatalf("bad restore key: %d", w.Code)
	}
}

func TestMoveRestoreListKeepsTables(t *testing.T) {
	listing := strings.Join([]string{
		";",
		"; Archive created at 2026-11-06 10:00:00 UTC",
		"6; 2615 16385 SCHEMA - app owner",
		"215; 1259 16400 TABLE public events app",
		"216; 1259 16410 SEQUENCE public events_id_seq app",
		"4001; 0 0 SEQUENCE OWNED BY public events_id_seq app",
		"4002; 0 0 DEFAULT public events id app",
		"217; 1259 16420 TABLE public orders app",
		"3001; 0 16400 TABLE DATA public events app",
		"3002; 0 16420 TABLE DATA public orders app",
		"4003; 0 0 SEQUENCE SET public events_id_seq app",
		"4100; 2606 16430 CONSTRAINT public events events_pkey app",
		"4101; 2606 16431 CONSTRAINT public orders orders_pkey app",
		"4200; 1259 16440 INDEX public events_occurred_at_idx app",
		"4201; 1259 16441 INDEX public orders_created_idx app",
		"4300; 2606 16450 FK CONSTRAINT public orders orders_event_fk app",
		"4301; 2606 16451 FK CONSTRAINT public events events_user_fk app",
		"4400; 2620 16460 TRIGGER public events events_audit app",
	}, "\n") + "\n"
	keep := moveKeep{tables: map[string]bool{"public.events": true}, indexes: map[string]bool{"public.events_pkey": true, "public.events_occurred_at_idx": true}, sequences: map[string]bool{"public.events_id_seq": true}, schemas: map[string]bool{"public": true}}
	filtered := string(moveFilterRestoreList([]byte(listing), keep))
	kept, applied := []string{}, []string{}
	for _, line := range strings.Split(strings.TrimSpace(filtered), "\n") {
		switch {
		case len(strings.Fields(line)) < 4 || strings.HasPrefix(line, ";;") || strings.HasPrefix(line, "; "):
		case strings.HasPrefix(line, ";"):
			kept = append(kept, strings.Fields(line)[3])
		default:
			applied = append(applied, strings.Fields(line)[3])
		}
	}
	if strings.Join(kept, ",") != "TABLE,SEQUENCE,SEQUENCE,TABLE,CONSTRAINT,INDEX" || len(applied) != 10 {
		t.Fatalf("kept %v, applied %v:\n%s", kept, applied, filtered)
	}
	for _, line := range []string{"6; 2615 16385 SCHEMA - app owner", "217; 1259 16420 TABLE public orders app", "4003; 0 0 SEQUENCE SET public events_id_seq app", "4301; 2606 16451 FK CONSTRAINT public events events_user_fk app", "4400; 2620 16460 TRIGGER public events events_audit app", "4002; 0 0 DEFAULT public events id app"} {
		if !strings.Contains(filtered, "\n"+line+"\n") {
			t.Fatalf("%q must still apply", line)
		}
	}
	if !strings.Contains(filtered, "\n;215; 1259 16400 TABLE public events app\n") {
		t.Fatal("the kept table's entry is commented out")
	}
	if tables, err := moveKeepTables("public.events,audit.log"); err != nil || len(tables) != 2 {
		t.Fatal(tables, err)
	}
	if _, err := moveKeepTables("public.events;drop"); err == nil {
		t.Fatal("bad keep accepted")
	}
}

// The table statistics view has no stats_reset column (PostgreSQL 17
// rejected the query, so no incremental key was ever detected); the
// database's reset time dates the counters instead.
func TestMoveTableKeyQueryUsesTheDatabaseStatsReset(t *testing.T) {
	q := moveTableKeyQuery("public.events", defaultMoveKeyColumns)
	if strings.Contains(q, "s.stats_reset") || !strings.Contains(q, "pg_stat_database d WHERE d.datname = current_database()") {
		t.Fatalf("query: %s", q)
	}
}
