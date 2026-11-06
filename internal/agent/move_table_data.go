package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Table data a backup leaves out. An agent's backup_exclude_table_data
// policy keeps disposable tables' rows out of backups (their schema stays),
// and a move copies PostgreSQL through a fresh backup, so those tables would
// arrive empty. The move plan lists them with their sizes; for each table
// the operator chooses to copy its data or leave it behind. Copied tables
// move as a separate data-only dump of just those tables, so the backup
// policy stays as it is:
//
//   - POST   /v1/move/databases/{id}/table-data            dump the named tables' rows
//   - GET    /v1/move/databases/{id}/table-data/{export}   download it (ranged)
//   - DELETE /v1/move/databases/{id}/table-data            remove this database's dumps
//   - POST   /v1/move/databases/{id}/table-data/restore    empty the tables, then load the rows
//   - POST   /v1/move/databases/{id}/row-counts            exact row counts of named tables

// moveTableName is an exact lowercase schema.table, as backup exclusions are.
var moveTableName = regexp.MustCompile(`^[a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*$`)

const moveMaxDataTables = 100

// MoveExcludedTable is one table whose data the source's backups leave out.
// Rows is PostgreSQL's estimate (pg_class.reltuples); -1 when unknown.
type MoveExcludedTable struct {
	Name  string `json:"name"`
	Rows  int64  `json:"rows"`
	Bytes int64  `json:"bytes"`
	// Key is the table's incremental key when it has one
	// (move_table_delta.go): the final sync then copies only the rows added
	// since the bulk copy.
	Key *MoveTableKey `json:"key,omitempty"`
}

// MoveTableDataRequest names the tables of a data dump, restore or count.
type MoveTableDataRequest struct {
	Tables []string `json:"tables"`
}

// MoveTableDataExport is a finished data-only dump with the exact row count
// of each table at the time of the dump.
type MoveTableDataExport struct {
	ID     string           `json:"id"`
	Size   int64            `json:"size"`
	SHA256 string           `json:"sha256"`
	Tables []string         `json:"tables"`
	Rows   map[string]int64 `json:"rows"`
	// RowsAfter are the counts taken after the dump finished. A live
	// table keeps growing while the dump streams, so the dump's snapshot
	// holds between Rows and RowsAfter rows.
	RowsAfter map[string]int64 `json:"rows_after,omitempty"`
	// Keys are the incremental keys of the tables that have one, with the
	// watermark taken before the dump: the dump holds every row up to it.
	Keys map[string]MoveTableKey `json:"keys,omitempty"`
}

// MoveTableRows are exact row counts by table.
type MoveTableRows struct {
	Rows map[string]int64 `json:"rows"`
}

func validMoveTables(tables []string) error {
	if len(tables) == 0 || len(tables) > moveMaxDataTables {
		return fmt.Errorf("name between 1 and %d tables", moveMaxDataTables)
	}
	for _, table := range tables {
		if !moveTableName.MatchString(table) {
			return errors.New("tables must be exact lowercase schema.table names")
		}
	}
	return nil
}

func moveTableLiteral(table string) string {
	schema, name, _ := strings.Cut(table, ".")
	return "'" + schema + "'::name, '" + name + "'::name"
}

func moveTableIdent(table string) string {
	schema, name, _ := strings.Cut(table, ".")
	return `"` + schema + `"."` + name + `"`
}

// movePSQL runs one read or maintenance statement in a database this agent
// can measure or restore into, and returns its unaligned output.
var movePSQL = func(ctx context.Context, m *Manager, id, sql string) (string, error) {
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	if target.Host != nil {
		return nativeHostRun(ctx, "", "/usr/sbin/runuser", "-u", "postgres", "--", "/usr/bin/psql", "-X", "-q", "-A", "-t", "-F", "\t", "-v", "ON_ERROR_STOP=1",
			"-h", nativePGSocketDir, "-p", strconv.Itoa(target.Host.Port), "-d", target.Host.Name, "-c", sql)
	}
	var buffer limitedBuffer
	environment, flags := postgresEnvironment(target.Database.URL)
	args := []string{"run", "--rm", "--network", target.Database.Network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "32"}
	args = append(args, flags...)
	args = append(args, databaseImage, "psql", "-X", "-q", "-A", "-t", "-F", "\t", "-v", "ON_ERROR_STOP=1", "-c", sql)
	err = dockerStream(ctx, nil, &buffer, environment, args...)
	return buffer.String(), err
}

// moveExcludedTables measures the tables whose data the database's backups
// leave out.
func (m *Manager) moveExcludedTables(ctx context.Context, id string) []MoveExcludedTable {
	tables := m.measureMoveTables(ctx, id, m.backupExcludedTables(id))
	for i := range tables {
		if tables[i].Bytes < 0 || !moveTableName.MatchString(tables[i].Name) {
			continue
		}
		detect, cancel := context.WithTimeout(ctx, 20*time.Second)
		if key, err := m.moveTableKeyOf(detect, id, tables[i].Name); err == nil {
			tables[i].Key = key
		}
		cancel()
	}
	return tables
}

// measureMoveTables estimates the rows and measures the size of tables in
// one query, as the database's table listing does: a table PostgreSQL never
// analyzed (reltuples -1) is estimated from its live tuple statistics.
func (m *Manager) measureMoveTables(ctx context.Context, id string, tables []string) []MoveExcludedTable {
	if len(tables) == 0 {
		return nil
	}
	literals := []string{}
	for _, table := range tables {
		if moveTableName.MatchString(table) {
			literals = append(literals, "("+moveTableLiteral(table)+")")
		}
	}
	measured := map[string]DatabaseTable{}
	if len(literals) > 0 {
		measure, cancel := context.WithTimeout(ctx, 20*time.Second)
		text, err := movePSQL(measure, m, id, tableEstimateQuery("(n.nspname, c.relname) IN ("+strings.Join(literals, ", ")+")", ""))
		cancel()
		if err == nil {
			for _, table := range parseDatabaseTables(text) {
				measured[table.Name] = table
			}
		}
	}
	out := []MoveExcludedTable{}
	for _, table := range tables {
		entry := MoveExcludedTable{Name: table, Rows: -1, Bytes: -1}
		if found, ok := measured[table]; ok {
			entry.Rows, entry.Bytes = found.Rows, found.Bytes
		}
		out = append(out, entry)
	}
	return out
}

// moveTableRows counts the rows of each table exactly; a table that does
// not exist counts -1.
func (m *Manager) moveTableRows(ctx context.Context, id string, tables []string) (map[string]int64, error) {
	if err := validMoveTables(tables); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, table := range tables {
		text, err := movePSQL(ctx, m, id, "SELECT to_regclass('"+moveTableIdent(table)+"') IS NOT NULL")
		if err != nil {
			return nil, errors.New("the tables could not be counted")
		}
		if strings.TrimSpace(text) != "t" {
			out[table] = -1
			continue
		}
		text, err = movePSQL(ctx, m, id, "SELECT count(*) FROM "+moveTableIdent(table))
		if err != nil {
			return nil, errors.New("the tables could not be counted")
		}
		n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return nil, errors.New("the database returned an unexpected row count")
		}
		out[table] = n
	}
	return out, nil
}

// moveDumpTableData writes a data-only custom-format dump of tables; tests
// replace it.
var moveDumpTableData = func(ctx context.Context, m *Manager, id string, tables []string, output io.Writer) error {
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	options := []string{"--format=custom", "--compress=zstd:1", "--no-owner", "--no-acl", "--data-only", "--lock-wait-timeout=30s"}
	for _, table := range tables {
		options = append(options, "--table="+moveTableIdent(table))
	}
	if target.Host != nil {
		return m.moveHostPGTool(ctx, *target.Host, "pg_dump", options, nil, output)
	}
	environment, flags := postgresEnvironment(target.Database.URL)
	args := []string{"run", "--rm", "--network", target.Database.Network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128"}
	args = append(args, flags...)
	args = append(args, databaseImage, "pg_dump")
	args = append(args, options...)
	if err := dockerStream(ctx, nil, output, environment, args...); err != nil {
		return errors.New("the data-only dump of the excluded tables failed")
	}
	return nil
}

// moveRestoreTableData empties tables and loads a data-only dump into them
// in one transaction; tests replace it.
var moveRestoreTableData = func(ctx context.Context, m *Manager, target moveDatabaseTarget, tables []string, dump *os.File) error {
	idents := []string{}
	for _, table := range tables {
		idents = append(idents, moveTableIdent(table))
	}
	if _, err := movePSQL(ctx, m, target.ID, "TRUNCATE "+strings.Join(idents, ", ")); err != nil {
		return errors.New("the destination tables could not be emptied before loading their data")
	}
	options := []string{"--no-owner", "--no-acl", "--exit-on-error", "--single-transaction", "--data-only"}
	// The rows are a verified copy of a consistent source and are counted
	// afterwards, so the load skips foreign-key and user triggers: a
	// foreign-key check of every loaded row at the end of the COPY takes
	// as long as the load itself on a large table.
	environment := []string{"PGOPTIONS=" + moveTableLoadOptions}
	if target.Host != nil {
		m.moveGrantLoadOptions(ctx, *target.Host)
		if err := m.moveHostPGToolEnv(ctx, *target.Host, "pg_restore", options, environment, dump, io.Discard); err != nil {
			return errors.New("loading the excluded tables' data failed; their tables are empty on the destination")
		}
		return nil
	}
	connection, flags := postgresEnvironment(target.Database.URL)
	environment = append(environment, connection...)
	container := "lidza-move-table-data-" + newID()
	args := []string{"run", "--rm", "-i", "--name", container, "--network", target.Database.Network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128", "--env", "PGOPTIONS"}
	args = append(args, flags...)
	u, err := url.Parse(target.Database.URL)
	if err != nil || u.Path == "" {
		return errors.New("the destination database connection is invalid")
	}
	args = append(args, databaseImage, "pg_restore", "--dbname="+strings.TrimPrefix(u.Path, "/"))
	args = append(args, options...)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = command(cleanup, "", nil, "docker", "rm", "-f", container)
	}()
	if err := dockerStream(ctx, dump, io.Discard, environment, args...); err != nil {
		return errors.New("loading the excluded tables' data failed; their tables are empty on the destination")
	}
	return nil
}

// moveTableLoadOptions are the session options of a verified data load:
// no foreign-key or user triggers (session_replication_role=replica).
const moveTableLoadOptions = "-c session_replication_role=replica"

// moveGrantLoadOptions lets the application's role set the load options
// for its own sessions on a host cluster (PostgreSQL 15 and newer; the
// provisioning script grants it to new roles, this covers roles created
// before). A failure only costs the optimisation: the load then fires the
// triggers, as before.
func (m *Manager) moveGrantLoadOptions(ctx context.Context, pg NativeHostPostgres) {
	if pg.Major < 15 {
		return
	}
	ident, err := pgQuoteIdent(pg.Name)
	if err != nil {
		return
	}
	h := nativeHostDataHost(m)
	if _, err := h.psql(ctx, pg.Port, "GRANT SET ON PARAMETER session_replication_role TO "+ident+";\n"); err != nil {
		log.Printf("move: %s cannot be granted the data-load session options: %v", pg.Name, err)
	}
}

// moveHostPGTool runs pg_dump or pg_restore as the application's own role
// over loopback; the password travels in the tool's environment.
func (m *Manager) moveHostPGTool(ctx context.Context, pg NativeHostPostgres, tool string, options []string, input io.Reader, output io.Writer) error {
	return m.moveHostPGToolEnv(ctx, pg, tool, options, nil, input, output)
}

// moveHostPGToolEnv is moveHostPGTool with extra environment entries (for
// example PGOPTIONS).
func (m *Manager) moveHostPGToolEnv(ctx context.Context, pg NativeHostPostgres, tool string, options, extra []string, input io.Reader, output io.Writer) error {
	password, err := m.unsealHostSecret(pg.Password)
	if err != nil {
		return err
	}
	restore, err := movePGRestoreTool(pg.Major)
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(restore), tool)
	args := append([]string{"--no-password", "--dbname=" + pg.Name}, options...)
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8", "PGHOST=127.0.0.1", "PGPORT=" + strconv.Itoa(pg.Port), "PGUSER=" + pg.Name,
		"PGPASSWORD=" + password, "PGDATABASE=" + pg.Name, "PGAPPNAME=lidza-move", "PGCONNECT_TIMEOUT=10", "PGPASSFILE=/nonexistent"}
	cmd.Env = append(cmd.Env, extra...)
	cmd.Stdin = input
	var stderr limitedBuffer
	cmd.Stdout, cmd.Stderr = output, &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s failed", tool)
	}
	return nil
}

func (m *Manager) moveTableDataDir(id string) string {
	return filepath.Join(m.cfg.DataDir, "move-table-data", id)
}

func (m *Manager) moveTableDataRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/move/databases/{id}/row-counts", func(w http.ResponseWriter, r *http.Request) {
		var in MoveTableDataRequest
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		rows, err := m.moveTableRows(ctx, r.PathValue("id"), in.Tables)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, MoveTableRows{Rows: rows})
	})
	mux.HandleFunc("POST /v1/move/databases/{id}/table-data", m.moveTransfer(m.moveTableDataExportRoute))
	mux.HandleFunc("GET /v1/move/databases/{id}/table-data/{export}", m.moveTransfer(m.moveTableDataDownloadRoute))
	mux.HandleFunc("POST /v1/move/databases/{id}/table-data/restore", m.moveTransfer(m.moveTableDataRestoreRoute))
	mux.HandleFunc("DELETE /v1/move/databases/{id}/table-data", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !idPattern.MatchString(id) {
			Fail(w, 400, errors.New("invalid database"))
			return
		}
		if err := errors.Join(os.RemoveAll(m.moveTableDataDir(id)), os.RemoveAll(m.moveTableDeltaDir(id))); err != nil {
			Fail(w, 500, errors.New("the table data dumps could not be removed"))
			return
		}
		JSON(w, 200, map[string]string{"status": "removed"})
	})
}

func (m *Manager) moveTableDataExportRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in MoveTableDataRequest
	if err := Decode(w, r, &in); err != nil || validMoveTables(in.Tables) != nil || !idPattern.MatchString(id) {
		Fail(w, 400, errors.New("name the tables as exact schema.table names"))
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
	ctx, endProgress := m.trackDatabase(ctx, id, "Dumping the excluded tables' data")
	defer endProgress()
	dir := m.moveTableDataDir(id)
	// One dump per database: a new one replaces earlier attempts.
	if err := os.RemoveAll(dir); err != nil || os.MkdirAll(dir, 0o700) != nil {
		Fail(w, 500, errors.New("cannot prepare the dump directory"))
		return
	}
	var need int64
	for _, table := range m.measureMoveTables(ctx, id, in.Tables) {
		need += max(table.Bytes, 0)
	}
	if err := diskFits(dir, uint64(need), "the excluded tables' data dump"); err != nil {
		Fail(w, http.StatusInsufficientStorage, err)
		return
	}
	rows, err := m.moveTableRows(ctx, id, in.Tables)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	for table, n := range rows {
		if n < 0 {
			Fail(w, 409, fmt.Errorf("table %s does not exist in the source database", table))
			return
		}
	}
	keys := map[string]MoveTableKey{}
	for _, table := range in.Tables {
		if key, err := m.moveTableKeyOf(ctx, id, table); err == nil && key != nil {
			keys[table] = *key
		}
	}
	file, err := os.CreateTemp(dir, ".dump-*")
	if err != nil {
		Fail(w, 500, errors.New("cannot store the dump"))
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	counter := &progressWriter{}
	progressFrom(ctx).Stage("Dumping the excluded tables' data", need, counter.n.Load)
	if err := moveDumpTableData(ctx, m, id, in.Tables, io.MultiWriter(&reserveWriter{dir: dir}, file, hash, counter)); err != nil {
		code := 409
		if errors.Is(err, errDiskReserve) {
			code = http.StatusInsufficientStorage
		}
		Fail(w, code, err)
		return
	}
	if err := file.Sync(); err != nil || counter.n.Load() == 0 {
		Fail(w, 500, errors.New("the dump could not be stored"))
		return
	}
	export := MoveTableDataExport{ID: nativeRandomSecret(), Size: counter.n.Load(), SHA256: hex.EncodeToString(hash.Sum(nil)), Tables: slices.Clone(in.Tables), Rows: rows}
	if after, err := m.moveTableRows(ctx, id, in.Tables); err == nil {
		export.RowsAfter = after
	}
	export.Keys = m.moveKeysAfterDump(ctx, id, keys)
	if err := os.Rename(file.Name(), filepath.Join(dir, export.ID+".dump")); err != nil {
		Fail(w, 500, errors.New("the dump could not be finalized"))
		return
	}
	raw, _ := json.Marshal(export)
	if err := os.WriteFile(filepath.Join(dir, export.ID+".json"), raw, 0o600); err != nil {
		Fail(w, 500, errors.New("the dump record could not be stored"))
		return
	}
	JSON(w, 200, export)
}

func (m *Manager) moveTableDataDownloadRoute(w http.ResponseWriter, r *http.Request) {
	id, key := r.PathValue("id"), r.PathValue("export")
	if !idPattern.MatchString(id) || !moveSHA256Pattern.MatchString(key) {
		Fail(w, 404, errors.New("unknown dump"))
		return
	}
	dir := m.moveTableDataDir(id)
	raw, err := os.ReadFile(filepath.Join(dir, key+".json"))
	var export MoveTableDataExport
	if err != nil || json.Unmarshal(raw, &export) != nil || export.ID != key {
		Fail(w, 404, errors.New("unknown dump"))
		return
	}
	f, err := os.Open(filepath.Join(dir, key+".dump"))
	if err != nil {
		Fail(w, 404, errors.New("dump file unavailable"))
		return
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() != export.Size {
		Fail(w, 409, errors.New("the dump file does not match its record"))
		return
	}
	serveMoveArtifact(w, r, f, export.Size, export.SHA256, "application/octet-stream", 6*time.Hour)
}

func (m *Manager) moveTableDataRestoreRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tables := strings.Split(r.URL.Query().Get("tables"), ",")
	if validMoveTables(tables) != nil {
		Fail(w, 400, errors.New("name the tables as exact schema.table names"))
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
	progress, endProgress := m.progress.track(databaseProgressKey(id), "Receiving the excluded tables' data")
	defer endProgress()
	staged, err := m.receiveMoveUpload(r, m.backupDir(id), moveMaxDumpSize, "table data dump", progress)
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
	progress.Stage("Loading the excluded tables' data", 0, nil)
	if err := moveRestoreTableData(ctx, m, target, tables, staged.File); err != nil {
		result = err
		Fail(w, 409, err)
		return
	}
	restored = true
	rows, err := m.moveTableRows(ctx, id, tables)
	if err != nil {
		result = err
		Fail(w, 409, err)
		return
	}
	w.Header().Set(MoveVerifiedHeader, staged.SHA256)
	JSON(w, 200, MoveTableRows{Rows: rows})
}

// moveKeysAfterDump keeps the keys of the tables that were only appended
// to while the dump ran: a table updated or deleted from meanwhile has no
// watermark, and the final sync copies it in full. The counters recorded
// are the ones after the dump, so changes after it are seen later.
func (m *Manager) moveKeysAfterDump(ctx context.Context, id string, before map[string]MoveTableKey) map[string]MoveTableKey {
	out := map[string]MoveTableKey{}
	for table, key := range before {
		now, err := m.moveTableStatsOf(ctx, id, table)
		if err != nil || moveOnlyAppended(key, now) != "" {
			continue
		}
		key.Updates, key.Deletes = now.Updates, now.Deletes
		out[table] = key
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
