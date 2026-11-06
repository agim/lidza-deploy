package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Incremental copies of append-only tables. A table whose data the backups
// leave out is often a large append-only log, and copying it in full again
// at the final sync, with the source frozen, is most of the lock. When the
// table has an incremental key the final sync copies only the rows added
// since the bulk copy:
//
//   - the key is a single-column integer primary key, or an indexed
//     (btree, not null) timestamp column named like a creation time
//     (created_at, received_at, inserted_at, occurred_at, ts, timestamp;
//     the agent's move_key_columns setting changes the list);
//   - the table showed no updates or deletes (pg_stat_user_tables) while
//     the bulk dump ran, and none since by the time of the final sync;
//     otherwise the table is copied in full again.
//
// The bulk dump records the key and its maximum (the watermark). The final
// sync asks the source for the rows at or after the watermark as a binary
// COPY, and the destination deletes its rows at or after the watermark and
// appends the copy in one transaction, so rows sharing the watermark
// instant are never duplicated and a retry is exact. Both ends then
// report count(*) and max(key); the source is frozen, so they match.
//
//   - POST /v1/move/databases/{id}/table-delta            export the rows since a watermark
//   - GET  /v1/move/databases/{id}/table-delta/{export}   download it (ranged)
//   - POST /v1/move/databases/{id}/table-delta/restore    replace rows since the watermark with the export

// defaultMoveKeyColumns are the timestamp columns that mark a row's
// insertion, in order of preference.
var defaultMoveKeyColumns = []string{"created_at", "received_at", "inserted_at", "occurred_at", "ts", "timestamp"}

const (
	moveKeyInteger     = "integer"
	moveKeyTimestamp   = "timestamp"
	moveKeyTimestampTZ = "timestamptz"
)

// MoveTableKey is a table's incremental key and the state of the table
// when it was recorded.
type MoveTableKey struct {
	Column string `json:"column"`
	Kind   string `json:"kind"` // integer, timestamp or timestamptz
	// Watermark is the key's maximum at the dump, as PostgreSQL prints it;
	// empty for an empty table.
	Watermark string `json:"watermark,omitempty"`
	// Updates and Deletes are the table's n_tup_upd and n_tup_del when the
	// key was recorded; a later copy compares them to see that rows were
	// only added.
	Updates int64 `json:"updates"`
	Deletes int64 `json:"deletes"`
	// RowsPerHour is the table's average insert rate from the source's
	// statistics, with RateBasis saying over what period; 0 when unknown.
	RowsPerHour float64 `json:"rows_per_hour,omitempty"`
	RateBasis   string  `json:"rate_basis,omitempty"`
}

// moveTableStats is what the key query returns for one table.
type moveTableStats struct {
	Column, Kind                      string
	Updates, Deletes, Inserts, AgeSec int64
}

func moveKeyColumns(cfg Config) []string {
	if len(cfg.MoveKeyColumns) > 0 {
		return cfg.MoveKeyColumns
	}
	return defaultMoveKeyColumns
}

var moveColumnName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// moveTableKeyQuery finds a table's incremental key: an integer primary key,
// else the first configured timestamp column that is not null and has a
// btree index leading with it; with the table's write counters.
func moveTableKeyQuery(table string, columns []string) string {
	names := []string{}
	for _, column := range columns {
		if moveColumnName.MatchString(column) {
			names = append(names, "'"+column+"'")
		}
	}
	list := "ARRAY[" + strings.Join(names, ", ") + "]::text[]"
	if len(names) == 0 {
		list = "ARRAY[]::text[]"
	}
	return "WITH t AS (SELECT c.oid FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE (n.nspname, c.relname) = (" + moveTableLiteral(table) + ")), " +
		"pk AS (SELECT a.attname::text AS col FROM pg_catalog.pg_index i JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0] " +
		"WHERE i.indrelid = (SELECT oid FROM t) AND i.indisprimary AND i.indnatts = 1 AND a.atttypid IN ('int2'::regtype, 'int4'::regtype, 'int8'::regtype)), " +
		"ts AS (SELECT a.attname::text AS col, CASE WHEN a.atttypid = 'timestamptz'::regtype THEN 'timestamptz' ELSE 'timestamp' END AS kind FROM pg_catalog.pg_attribute a " +
		"WHERE a.attrelid = (SELECT oid FROM t) AND a.attnum > 0 AND NOT a.attisdropped AND a.attnotnull AND a.atttypid IN ('timestamp'::regtype, 'timestamptz'::regtype) " +
		"AND a.attname::text = ANY (" + list + ") AND EXISTS (SELECT 1 FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid JOIN pg_catalog.pg_am am ON am.oid = ic.relam " +
		"WHERE i.indrelid = a.attrelid AND i.indkey[0] = a.attnum AND i.indpred IS NULL AND i.indisvalid AND am.amname = 'btree') " +
		"ORDER BY array_position(" + list + ", a.attname::text) LIMIT 1) " +
		"SELECT coalesce((SELECT col FROM pk), (SELECT col FROM ts), ''), CASE WHEN EXISTS (SELECT 1 FROM pk) THEN 'integer' ELSE coalesce((SELECT kind FROM ts), '') END, " +
		// pg_stat_user_tables has no stats_reset column; the database's
		// counters reset together, so its reset time dates the table's.
		"coalesce(s.n_tup_upd, 0), coalesce(s.n_tup_del, 0), coalesce(s.n_tup_ins, 0), extract(epoch FROM now() - coalesce((SELECT d.stats_reset FROM pg_catalog.pg_stat_database d WHERE d.datname = current_database()), pg_postmaster_start_time()))::bigint " +
		"FROM t LEFT JOIN pg_catalog.pg_stat_user_tables s ON s.relid = t.oid"
}

// moveTableStatsOf runs the key query for one table.
func (m *Manager) moveTableStatsOf(ctx context.Context, id, table string) (moveTableStats, error) {
	text, err := movePSQL(ctx, m, id, moveTableKeyQuery(table, moveKeyColumns(m.cfg)))
	if err != nil {
		return moveTableStats{}, err
	}
	fields := strings.Split(strings.TrimRight(text, "\n"), "\t")
	if len(fields) != 6 {
		return moveTableStats{}, errors.New("the database returned unexpected table statistics")
	}
	out := moveTableStats{Column: fields[0], Kind: fields[1]}
	for i, target := range []*int64{&out.Updates, &out.Deletes, &out.Inserts, &out.AgeSec} {
		n, err := strconv.ParseInt(strings.TrimSpace(fields[2+i]), 10, 64)
		if err != nil {
			return moveTableStats{}, errors.New("the database returned unexpected table statistics")
		}
		*target = n
	}
	return out, nil
}

// moveTableKeyOf detects a table's incremental key with its current
// watermark and insert rate; nil when the table has none.
func (m *Manager) moveTableKeyOf(ctx context.Context, id, table string) (*MoveTableKey, error) {
	stats, err := m.moveTableStatsOf(ctx, id, table)
	if err != nil {
		return nil, err
	}
	if stats.Column == "" || !moveColumnName.MatchString(stats.Column) {
		return nil, nil
	}
	key := &MoveTableKey{Column: stats.Column, Kind: stats.Kind, Updates: stats.Updates, Deletes: stats.Deletes}
	text, err := movePSQL(ctx, m, id, moveSessionPins+"SELECT "+moveMaxExpr(*key)+" FROM "+moveTableIdent(table))
	if err != nil {
		return nil, err
	}
	key.Watermark = strings.TrimSpace(text)
	if key.Watermark != "" && !moveWatermarkValid(key.Kind, key.Watermark) {
		return nil, nil
	}
	if stats.AgeSec >= 600 && stats.Inserts > 0 {
		key.RowsPerHour = float64(stats.Inserts) / (float64(stats.AgeSec) / 3600)
		key.RateBasis = "the source's insert statistics over " + moveStatsAge(stats.AgeSec)
	}
	return key, nil
}

func moveStatsAge(seconds int64) string {
	switch d := time.Duration(seconds) * time.Second; {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d minutes", int(time.Duration(seconds)*time.Second/time.Minute))
}

func moveColumnIdent(column string) string { return `"` + column + `"` }

// moveSessionPins fixes the session's time zone and date style, so a
// watermark prints identically on every cluster and quotes back exactly.
const moveSessionPins = "SET TIME ZONE 'UTC'; SET DateStyle = 'ISO, YMD'; "

// moveMaxExpr renders max(key) as text: a timestamptz in UTC without an
// offset, a timestamp as it is, both to the microsecond; an integer plain.
func moveMaxExpr(key MoveTableKey) string {
	column := moveColumnIdent(key.Column)
	switch key.Kind {
	case moveKeyTimestampTZ:
		return "to_char(max(" + column + ") AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US')"
	case moveKeyTimestamp:
		return "to_char(max(" + column + "), 'YYYY-MM-DD\"T\"HH24:MI:SS.US')"
	}
	return "max(" + column + ")::text"
}

var (
	moveIntegerLiteral   = regexp.MustCompile(`^-?[0-9]{1,19}$`)
	moveTimestampLiteral = regexp.MustCompile(`^[0-9]{4,}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}$`)
)

// moveWatermarkValid accepts a watermark as PostgreSQL printed it, so it
// can be quoted back into a statement.
func moveWatermarkValid(kind, watermark string) bool {
	switch kind {
	case moveKeyInteger:
		return moveIntegerLiteral.MatchString(watermark)
	case moveKeyTimestamp, moveKeyTimestampTZ:
		return moveTimestampLiteral.MatchString(watermark)
	}
	return false
}

// moveKeyLiteral renders a validated watermark as a typed literal, the
// inverse of moveMaxExpr.
func moveKeyLiteral(key MoveTableKey) string {
	switch key.Kind {
	case moveKeyInteger:
		return key.Watermark + "::bigint"
	case moveKeyTimestampTZ:
		return "('" + key.Watermark + "'::timestamp AT TIME ZONE 'UTC')"
	}
	return "'" + key.Watermark + "'::timestamp"
}

// moveDeltaCondition selects the rows a delta copies: strictly after an
// integer watermark (keys are unique), at or after a timestamp one (several
// rows may share the instant; the destination drops its rows at the
// watermark before appending).
func moveDeltaCondition(key MoveTableKey) string {
	if key.Watermark == "" {
		return "true"
	}
	op := ">="
	if key.Kind == moveKeyInteger {
		op = ">"
	}
	return moveColumnIdent(key.Column) + " " + op + " " + moveKeyLiteral(key)
}

func validMoveKey(key MoveTableKey) error {
	if !moveColumnName.MatchString(key.Column) {
		return errors.New("the key column must be a lowercase identifier")
	}
	if key.Watermark != "" && !moveWatermarkValid(key.Kind, key.Watermark) {
		return errors.New("the watermark does not match the key's type")
	}
	if key.Watermark == "" && key.Kind != moveKeyInteger && key.Kind != moveKeyTimestamp && key.Kind != moveKeyTimestampTZ {
		return errors.New("the key kind is unknown")
	}
	return nil
}

// MoveTableDeltaRequest asks for one table's rows since its watermark.
type MoveTableDeltaRequest struct {
	Table string       `json:"table"`
	Key   MoveTableKey `json:"key"`
}

// MoveTableDeltaExport is a finished delta: Rows is the number of rows in
// it, Total and Max the table's count(*) and max(key) on the source when it
// was taken. Fallback names why no delta was taken (the table changed in
// other ways than appends); the table is then copied in full.
type MoveTableDeltaExport struct {
	ID       string `json:"id,omitempty"`
	Size     int64  `json:"size,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Table    string `json:"table"`
	Rows     int64  `json:"rows"`
	Total    int64  `json:"total"`
	Max      string `json:"max,omitempty"`
	Fallback string `json:"fallback,omitempty"`
}

// MoveTableDeltaResult is the destination's table after a delta load.
type MoveTableDeltaResult struct {
	Table string `json:"table"`
	Total int64  `json:"total"`
	Max   string `json:"max,omitempty"`
}

// moveOnlyAppended reports why a table recorded with key cannot be copied
// incrementally any more; "" when rows were only added since.
func moveOnlyAppended(key MoveTableKey, now moveTableStats) string {
	switch {
	case now.Column != key.Column || now.Kind != key.Kind:
		return "its incremental key changed"
	case now.Updates < key.Updates || now.Deletes < key.Deletes:
		return "the source's table statistics were reset"
	case now.Updates > key.Updates && now.Deletes > key.Deletes:
		return fmt.Sprintf("%d rows were updated and %d deleted since the bulk copy", now.Updates-key.Updates, now.Deletes-key.Deletes)
	case now.Updates > key.Updates:
		return fmt.Sprintf("%d rows were updated since the bulk copy", now.Updates-key.Updates)
	case now.Deletes > key.Deletes:
		return fmt.Sprintf("%d rows were deleted since the bulk copy", now.Deletes-key.Deletes)
	}
	return ""
}

// moveTableTotals returns a table's count(*) and max(key).
func (m *Manager) moveTableTotals(ctx context.Context, id, table string, key MoveTableKey) (int64, string, error) {
	text, err := movePSQL(ctx, m, id, moveSessionPins+"SELECT count(*), coalesce("+moveMaxExpr(key)+", '') FROM "+moveTableIdent(table))
	if err != nil {
		return 0, "", errors.New("the table could not be counted")
	}
	total, maximum, _ := strings.Cut(strings.TrimRight(text, "\n"), "\t")
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil {
		return 0, "", errors.New("the database returned an unexpected row count")
	}
	return n, strings.TrimSpace(maximum), nil
}

// moveCopyTableDelta streams the rows of table selected by condition as a
// binary COPY; tests replace it.
var moveCopyTableDelta = func(ctx context.Context, m *Manager, id, table, condition, order string, output io.Writer) error {
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	sql := "COPY (SELECT * FROM " + moveTableIdent(table) + " WHERE " + condition + " ORDER BY " + moveColumnIdent(order) + ") TO STDOUT (FORMAT binary)"
	options := []string{"-X", "-q", "-v", "ON_ERROR_STOP=1", "-c", "SET TIME ZONE 'UTC'", "-c", "SET DateStyle = 'ISO, YMD'", "-c", sql}
	if target.Host != nil {
		return m.moveHostPGTool(ctx, *target.Host, "psql", options, nil, output)
	}
	environment, flags := postgresEnvironment(target.Database.URL)
	args := []string{"run", "--rm", "--network", target.Database.Network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "32"}
	args = append(args, flags...)
	args = append(args, databaseImage, "psql")
	args = append(args, options...)
	if err := dockerStream(ctx, nil, output, environment, args...); err != nil {
		return errors.New("the incremental copy of the table failed")
	}
	return nil
}

// moveLoadTableDelta deletes the destination's rows matching condition and
// appends the binary COPY in one transaction; tests replace it.
var moveLoadTableDelta = func(ctx context.Context, m *Manager, target moveDatabaseTarget, table, condition string, data *os.File) error {
	options := []string{"-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction",
		"-c", "SET TIME ZONE 'UTC'", "-c", "SET DateStyle = 'ISO, YMD'",
		"-c", "DELETE FROM " + moveTableIdent(table) + " WHERE " + condition,
		"-c", "COPY " + moveTableIdent(table) + " FROM STDIN (FORMAT binary)"}
	if target.Host != nil {
		if err := m.moveHostPGTool(ctx, *target.Host, "psql", options, data, io.Discard); err != nil {
			return errors.New("appending the table's new rows failed; the destination table is unchanged")
		}
		return nil
	}
	environment, flags := postgresEnvironment(target.Database.URL)
	args := []string{"run", "--rm", "-i", "--network", target.Database.Network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "32"}
	args = append(args, flags...)
	args = append(args, databaseImage, "psql")
	args = append(args, options...)
	if err := dockerStream(ctx, data, io.Discard, environment, args...); err != nil {
		return errors.New("appending the table's new rows failed; the destination table is unchanged")
	}
	return nil
}

func (m *Manager) moveTableDeltaDir(id string) string {
	return filepath.Join(m.cfg.DataDir, "move-table-delta", id)
}

func (m *Manager) moveTableDeltaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/move/databases/{id}/table-delta", m.moveTransfer(m.moveTableDeltaExportRoute))
	mux.HandleFunc("GET /v1/move/databases/{id}/table-delta/{export}", m.moveTransfer(m.moveTableDeltaDownloadRoute))
	mux.HandleFunc("POST /v1/move/databases/{id}/table-delta/restore", m.moveTransfer(m.moveTableDeltaRestoreRoute))
}

func (m *Manager) moveTableDeltaExportRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in MoveTableDeltaRequest
	if err := Decode(w, r, &in); err != nil || !moveTableName.MatchString(in.Table) || validMoveKey(in.Key) != nil || !idPattern.MatchString(id) {
		Fail(w, 400, errors.New("name one table as an exact schema.table name with its recorded key"))
		return
	}
	if m.upgradePending() {
		Fail(w, 409, errors.New("agent upgrade in progress"))
		return
	}
	m.mu.Lock()
	_, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		Fail(w, 404, err)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(6 * time.Hour))
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	ctx, endProgress := m.trackDatabase(ctx, id, "Copying the rows added to "+in.Table)
	defer endProgress()
	stats, err := m.moveTableStatsOf(ctx, id, in.Table)
	if err != nil {
		Fail(w, 409, errors.New("the table's statistics could not be read"))
		return
	}
	if why := moveOnlyAppended(in.Key, stats); why != "" {
		JSON(w, 200, MoveTableDeltaExport{Table: in.Table, Fallback: why})
		return
	}
	dir := m.moveTableDeltaDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		Fail(w, 500, errors.New("cannot prepare the export directory"))
		return
	}
	file, err := os.CreateTemp(dir, ".delta-*")
	if err != nil {
		Fail(w, 500, errors.New("cannot store the export"))
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	condition := moveDeltaCondition(in.Key)
	hash := sha256.New()
	counter := &progressWriter{}
	progressFrom(ctx).Stage("Copying the rows added to "+in.Table, 0, counter.n.Load)
	if err := moveCopyTableDelta(ctx, m, id, in.Table, condition, in.Key.Column, io.MultiWriter(&reserveWriter{dir: dir}, file, hash, counter)); err != nil {
		code := 409
		if errors.Is(err, errDiskReserve) {
			code = http.StatusInsufficientStorage
		}
		Fail(w, code, err)
		return
	}
	if err := file.Sync(); err != nil || counter.n.Load() == 0 {
		Fail(w, 500, errors.New("the export could not be stored"))
		return
	}
	text, err := movePSQL(ctx, m, id, "SELECT count(*) FROM "+moveTableIdent(in.Table)+" WHERE "+condition)
	if err != nil {
		Fail(w, 409, errors.New("the table could not be counted"))
		return
	}
	rows, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		Fail(w, 409, errors.New("the database returned an unexpected row count"))
		return
	}
	total, maximum, err := m.moveTableTotals(ctx, id, in.Table, in.Key)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	export := MoveTableDeltaExport{ID: nativeRandomSecret(), Size: counter.n.Load(), SHA256: hex.EncodeToString(hash.Sum(nil)), Table: in.Table, Rows: rows, Total: total, Max: maximum}
	if err := os.Rename(file.Name(), filepath.Join(dir, export.ID+".copy")); err != nil {
		Fail(w, 500, errors.New("the export could not be finalized"))
		return
	}
	raw, _ := json.Marshal(export)
	if err := os.WriteFile(filepath.Join(dir, export.ID+".json"), raw, 0o600); err != nil {
		Fail(w, 500, errors.New("the export record could not be stored"))
		return
	}
	JSON(w, 200, export)
}

func (m *Manager) moveTableDeltaDownloadRoute(w http.ResponseWriter, r *http.Request) {
	id, key := r.PathValue("id"), r.PathValue("export")
	if !idPattern.MatchString(id) || !moveSHA256Pattern.MatchString(key) {
		Fail(w, 404, errors.New("unknown export"))
		return
	}
	dir := m.moveTableDeltaDir(id)
	raw, err := os.ReadFile(filepath.Join(dir, key+".json"))
	var export MoveTableDeltaExport
	if err != nil || json.Unmarshal(raw, &export) != nil || export.ID != key {
		Fail(w, 404, errors.New("unknown export"))
		return
	}
	f, err := os.Open(filepath.Join(dir, key+".copy"))
	if err != nil {
		Fail(w, 404, errors.New("export file unavailable"))
		return
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() != export.Size {
		Fail(w, 409, errors.New("the export file does not match its record"))
		return
	}
	serveMoveArtifact(w, r, f, export.Size, export.SHA256, "application/octet-stream", 6*time.Hour)
}

// moveTableDeltaRestoreRoute replaces the destination's rows since the
// watermark (?table=&column=&kind=&after=) with the uploaded binary COPY.
func (m *Manager) moveTableDeltaRestoreRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	query := r.URL.Query()
	table := query.Get("table")
	key := MoveTableKey{Column: query.Get("column"), Kind: query.Get("kind"), Watermark: query.Get("after")}
	if !moveTableName.MatchString(table) || validMoveKey(key) != nil {
		Fail(w, 400, errors.New("name one table as an exact schema.table name with its recorded key"))
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(6 * time.Hour))
	_ = controller.SetWriteDeadline(time.Now().Add(6*time.Hour + time.Minute))
	target, err := m.beginMoveRestore(id)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	var result error
	defer func() { m.endMoveRestore(id, result) }()
	progress, endProgress := m.progress.track(databaseProgressKey(id), "Receiving the rows added to "+table)
	defer endProgress()
	staged, err := m.receiveMoveUpload(r, m.backupDir(id), moveMaxDumpSize, "table delta", progress)
	if err != nil {
		if !errors.Is(err, errMoveUploadIncomplete) {
			result = err
		}
		failMoveUpload(w, err)
		return
	}
	restored := false
	defer func() { staged.Done(restored) }()
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	progress.Stage("Appending the rows added to "+table, 0, nil)
	if err := moveLoadTableDelta(ctx, m, target, table, moveDeltaCondition(key), staged.File); err != nil {
		result = err
		Fail(w, 409, err)
		return
	}
	restored = true
	total, maximum, err := m.moveTableTotals(ctx, id, table, key)
	if err != nil {
		result = err
		Fail(w, 409, err)
		return
	}
	w.Header().Set(MoveVerifiedHeader, staged.SHA256)
	JSON(w, 200, MoveTableDeltaResult{Table: table, Total: total, Max: maximum})
}

// MoveTableDeltaRestorePath is the delta restore route with its query for one
// table, shared with the control panel's relay.
func MoveTableDeltaRestorePath(database, table string, key MoveTableKey) string {
	return "/v1/move/databases/" + url.PathEscape(database) + "/table-delta/restore?" + url.Values{"table": {table}, "column": {key.Column}, "kind": {key.Kind}, "after": {key.Watermark}}.Encode()
}
