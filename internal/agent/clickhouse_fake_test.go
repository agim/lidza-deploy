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
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeClickHouse answers the statements the agent sends over ClickHouse's
// HTTP interface, with databases held in memory. BACKUP writes the database
// as JSON to the backups disk (Disk) or the File() directory; RESTORE reads
// it back. Table data travels as "rows:N" in place of the Native format.
type fakeClickHouse struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	version string
	disk    string // Disk('backups', ...) root
	files   string // File(...) root
	users   map[string]string
	hashed  map[string][2]string // user → sha256(password+salt), salt
	dbs     map[string]map[string]*fakeTable
	ops     map[string]string
	log     []string
	// loseRows drops rows of restored tables, to test verification.
	loseRows bool
	// failRestore fails every RESTORE; holdOps keeps operations running
	// until they are killed; killed lists the killed query ids.
	failRestore bool
	holdOps     bool
	killed      []string
	// noRename refuses RENAME DATABASE (an Ordinary database).
	noRename bool
}

type fakeTable struct {
	Engine string `json:"engine"`
	Rows   uint64 `json:"rows"`
	Create string `json:"create"`
}

func newFakeClickHouse(t *testing.T, adminPassword string) *fakeClickHouse {
	f := &fakeClickHouse{t: t, version: "26.8.15.10", disk: t.TempDir(), files: t.TempDir(), users: map[string]string{"default": adminPassword}, hashed: map[string][2]string{}, dbs: map[string]map[string]*fakeTable{}, ops: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeClickHouse) statements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakeClickHouse) table(db, name string) *fakeTable {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dbs[db][name]
}

var (
	fakeIdent        = "`((?:[^`\\\\]|\\\\.)+)`"
	fakeBackupRe     = regexp.MustCompile("^BACKUP DATABASE " + fakeIdent + " TO (Disk\\('backups', '([^']+)'\\)|File\\('([^']+)'\\)) ASYNC$")
	fakeRestoreRe    = regexp.MustCompile("^RESTORE DATABASE " + fakeIdent + "(?: AS " + fakeIdent + ")? FROM (Disk\\('backups', '([^']+)'\\)|File\\('([^']+)'\\)) ASYNC$")
	fakeCountRe      = regexp.MustCompile("^SELECT count\\(\\) FROM " + fakeIdent + "\\." + fakeIdent + "$")
	fakeSelectRe     = regexp.MustCompile("^SELECT \\* FROM " + fakeIdent + "\\." + fakeIdent + " FORMAT Native$")
	fakeInsertRe     = regexp.MustCompile("^INSERT INTO " + fakeIdent + "\\." + fakeIdent + " SETTINGS optimize_on_insert = 0 FORMAT Native$")
	fakeCreateRe     = regexp.MustCompile("^CREATE (?:TABLE|VIEW|MATERIALIZED VIEW|DICTIONARY) (?:`([^`]+)`|([A-Za-z0-9_]+))\\.([A-Za-z0-9_]+)(.*)$")
	fakeDatabaseRe   = regexp.MustCompile("database = '([^']+)'")
	fakeBackupIDRe   = regexp.MustCompile("WHERE id = '([^']+)'")
	fakeIdentifiedRe = regexp.MustCompile("^(?:CREATE USER IF NOT EXISTS|ALTER USER) " + fakeIdent + " IDENTIFIED WITH sha256_hash BY '([0-9a-f]{64})' SALT '([0-9a-f]+)' HOST IP '127.0.0.1', IP '::1' ")
	fakeDropUserRe   = regexp.MustCompile("^DROP USER IF EXISTS " + fakeIdent)
	fakeDropDBRe     = regexp.MustCompile("^DROP DATABASE IF EXISTS " + fakeIdent + " SYNC$")
	fakeCreateDBRe   = regexp.MustCompile("^CREATE DATABASE IF NOT EXISTS " + fakeIdent)
	fakeEngineRe     = regexp.MustCompile(`ENGINE = ([A-Za-z]+)`)
	fakeRenameDBRe   = regexp.MustCompile("^RENAME DATABASE " + fakeIdent + " TO " + fakeIdent + "$")
	fakeKillRe       = regexp.MustCompile("^KILL QUERY WHERE query_id = '([^']+)' ASYNC$")
	fakeNamesRe      = regexp.MustCompile("'([^']+)'")
	fakeRowsPrefix   = "rows:"
	errFakeNotFound  = errors.New("not found")
)

func (f *fakeClickHouse) serve(w http.ResponseWriter, r *http.Request) {
	user, key := r.Header.Get("X-ClickHouse-User"), r.Header.Get("X-ClickHouse-Key")
	f.mu.Lock()
	expected, known := f.users[user]
	if hashed, ok := f.hashed[user]; ok {
		sum := sha256.Sum256([]byte(key + hashed[1]))
		known, expected = true, key
		if hex.EncodeToString(sum[:]) != hashed[0] {
			expected = ""
		}
	}
	f.mu.Unlock()
	if !known || expected != key || key == "" {
		http.Error(w, "Code: 516. DB::Exception: "+user+": Authentication failed", 516)
		return
	}
	sql := r.URL.Query().Get("query")
	var body []byte
	if sql == "" {
		raw, _ := io.ReadAll(r.Body)
		sql = string(raw)
	} else {
		body, _ = io.ReadAll(r.Body)
	}
	sql = strings.TrimSpace(sql)
	out, status, err := f.answer(user, sql, body)
	if err != nil {
		http.Error(w, "Code: "+strconv.Itoa(status)+". DB::Exception: "+err.Error(), 500)
		return
	}
	w.WriteHeader(200)
	io.WriteString(w, out)
}

func fakeUnquote(name string) string {
	return strings.NewReplacer("\\`", "`", `\\`, `\`).Replace(name)
}

func (f *fakeClickHouse) backupPath(match []string, diskIndex, fileIndex int) string {
	// A trailing slash names a directory backup.
	if match[diskIndex] != "" {
		return filepath.Join(f.disk, match[diskIndex]) + map[bool]string{true: "/"}[strings.HasSuffix(match[diskIndex], "/")]
	}
	return filepath.Join(f.files, match[fileIndex]) + map[bool]string{true: "/"}[strings.HasSuffix(match[fileIndex], "/")]
}

func (f *fakeClickHouse) answer(user, sql string, body []byte) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, sql)
	jsonRows := func(rows any) string {
		var b strings.Builder
		values, _ := json.Marshal(rows)
		var list []json.RawMessage
		_ = json.Unmarshal(values, &list)
		for _, row := range list {
			b.Write(row)
			b.WriteByte('\n')
		}
		return b.String()
	}
	db := ""
	if match := fakeDatabaseRe.FindStringSubmatch(sql); match != nil {
		db = match[1]
	}
	switch {
	case sql == "SELECT version()":
		return f.version + "\n", 0, nil
	case sql == "SELECT currentUser(), currentDatabase()":
		return user + "\t" + user + "\n", 0, nil
	case strings.HasPrefix(sql, "SELECT name, engine, has_own_data, create_table_query FROM system.tables"):
		type row struct {
			Name    string `json:"name"`
			Engine  string `json:"engine"`
			OwnData int    `json:"has_own_data"`
			Create  string `json:"create_table_query"`
		}
		rows := []row{}
		for name, table := range f.dbs[db] {
			own := 1
			if strings.Contains(table.Engine, "View") {
				own = 0
			}
			rows = append(rows, row{Name: name, Engine: table.Engine, OwnData: own, Create: table.Create})
		}
		return jsonRows(rows), 0, nil
	case strings.HasPrefix(sql, "SELECT count() AS tables"):
		var rows uint64
		for _, table := range f.dbs[db] {
			rows += table.Rows
		}
		return fmt.Sprintf(`{"tables":%d,"rows":%d}`+"\n", len(f.dbs[db]), rows), 0, nil
	case strings.HasPrefix(sql, "SELECT toUInt64(sum(bytes_on_disk)) AS bytes"):
		var rows uint64
		for _, table := range f.dbs[db] {
			rows += table.Rows
		}
		return fmt.Sprintf(`{"bytes":%d,"uncompressed":%d}`+"\n", rows*16, rows*64), 0, nil
	case strings.HasPrefix(sql, "SELECT status, error, query_id, num_files, files_read, total_size, bytes_read FROM system.backups"):
		id := fakeBackupIDRe.FindStringSubmatch(sql)[1]
		status, ok := f.ops[id]
		if !ok {
			return "", 0, nil
		}
		message := ""
		if strings.HasSuffix(status, "FAILED") || strings.HasSuffix(status, "CANCELLED") {
			message = "Code: 36. backup failed"
		}
		return fmt.Sprintf(`{"status":%q,"error":%q,"query_id":"q-%s","num_files":3,"files_read":3,"total_size":300,"bytes_read":300}`+"\n", status, message, id), 0, nil
	case strings.HasPrefix(sql, "SELECT name, engine FROM system.databases WHERE name IN"):
		rows := []map[string]string{}
		for _, m := range fakeNamesRe.FindAllStringSubmatch(sql, -1) {
			if f.dbs[m[1]] != nil {
				rows = append(rows, map[string]string{"name": m[1], "engine": "Atomic"})
			}
		}
		return jsonRows(rows), 0, nil
	}
	if match := fakeKillRe.FindStringSubmatch(sql); match != nil {
		f.killed = append(f.killed, match[1])
		id := strings.TrimPrefix(match[1], "q-")
		switch f.ops[id] {
		case "CREATING_BACKUP":
			f.ops[id] = "BACKUP_CANCELLED"
		case "RESTORING":
			f.ops[id] = "RESTORE_CANCELLED"
		}
		return "", 0, nil
	}
	if match := fakeRenameDBRe.FindStringSubmatch(sql); match != nil {
		from, to := fakeUnquote(match[1]), fakeUnquote(match[2])
		if f.noRename {
			return "", 48, errors.New("RENAME DATABASE is not supported for Ordinary databases")
		}
		if f.dbs[from] == nil || f.dbs[to] != nil {
			return "", 81, errFakeNotFound
		}
		f.dbs[to] = f.dbs[from]
		delete(f.dbs, from)
		return "", 0, nil
	}
	if match := fakeCountRe.FindStringSubmatch(sql); match != nil {
		table := f.dbs[fakeUnquote(match[1])][fakeUnquote(match[2])]
		if table == nil {
			return "", 60, errFakeNotFound
		}
		return strconv.FormatUint(table.Rows, 10) + "\n", 0, nil
	}
	if match := fakeBackupRe.FindStringSubmatch(sql); match != nil {
		source := f.dbs[fakeUnquote(match[1])]
		if source == nil {
			return "", 81, errFakeNotFound
		}
		path := f.backupPath(match, 3, 4)
		raw, _ := json.Marshal(source)
		if strings.HasSuffix(path, "/") {
			// A directory backup: metadata plus a file per table.
			if err := os.MkdirAll(filepath.Join(path, "data"), 0o755); err != nil {
				return "", 1, err
			}
			for name := range source {
				if err := os.WriteFile(filepath.Join(path, "data", name+".bin"), []byte("part of "+name), 0o644); err != nil {
					return "", 1, err
				}
			}
			path = filepath.Join(path, ".backup")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", 1, err
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return "", 1, err
		}
		id := fmt.Sprintf("op-%d", len(f.ops)+1)
		f.ops[id] = "BACKUP_CREATED"
		if f.holdOps {
			f.ops[id] = "CREATING_BACKUP"
		}
		return id + "\tCREATING_BACKUP\n", 0, nil
	}
	if match := fakeRestoreRe.FindStringSubmatch(sql); match != nil {
		path := f.backupPath(match, 4, 5)
		if strings.HasSuffix(path, "/") {
			path = filepath.Join(path, ".backup")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", 1, err
		}
		id := fmt.Sprintf("op-%d", len(f.ops)+1)
		if f.holdOps {
			f.ops[id] = "RESTORING"
			return id + "\tRESTORING\n", 0, nil
		}
		if f.failRestore {
			// A failed restore leaves a partial database behind.
			f.dbs[fakeUnquote(cmpString(match[2], match[1]))] = map[string]*fakeTable{}
			f.ops[id] = "RESTORE_FAILED"
			return id + "\tRESTORING\n", 0, nil
		}
		tables := map[string]*fakeTable{}
		if err := json.Unmarshal(raw, &tables); err != nil {
			return "", 1, err
		}
		target := fakeUnquote(match[1])
		if match[2] != "" {
			target = fakeUnquote(match[2])
		}
		for name, table := range tables {
			if f.loseRows && table.Rows > 0 {
				table.Rows--
			}
			table.Create = strings.ReplaceAll(table.Create, fakeUnquote(match[1])+".", target+".")
			tables[name] = table
		}
		f.dbs[target] = tables
		f.ops[id] = "RESTORED"
		return id + "\tRESTORING\n", 0, nil
	}
	if match := fakeSelectRe.FindStringSubmatch(sql); match != nil {
		table := f.dbs[fakeUnquote(match[1])][fakeUnquote(match[2])]
		if table == nil {
			return "", 60, errFakeNotFound
		}
		return fakeRowsPrefix + strconv.FormatUint(table.Rows, 10), 0, nil
	}
	if match := fakeInsertRe.FindStringSubmatch(sql); match != nil {
		table := f.dbs[fakeUnquote(match[1])][fakeUnquote(match[2])]
		n, err := strconv.ParseUint(strings.TrimPrefix(string(body), fakeRowsPrefix), 10, 64)
		if table == nil || err != nil {
			return "", 60, errFakeNotFound
		}
		if f.loseRows && n > 0 {
			n--
		}
		table.Rows += n
		return "", 0, nil
	}
	if match := fakeDropDBRe.FindStringSubmatch(sql); match != nil {
		delete(f.dbs, fakeUnquote(match[1]))
		return "", 0, nil
	}
	if match := fakeCreateDBRe.FindStringSubmatch(sql); match != nil {
		if f.dbs[fakeUnquote(match[1])] == nil {
			f.dbs[fakeUnquote(match[1])] = map[string]*fakeTable{}
		}
		return "", 0, nil
	}
	if match := fakeCreateRe.FindStringSubmatch(sql); match != nil {
		db := match[1] + match[2]
		if f.dbs[db] == nil {
			return "", 81, errFakeNotFound
		}
		engine := "MergeTree"
		switch {
		case strings.HasPrefix(sql, "CREATE MATERIALIZED VIEW"):
			engine = "MaterializedView"
		case strings.HasPrefix(sql, "CREATE VIEW"):
			engine = "View"
		default:
			if e := fakeEngineRe.FindStringSubmatch(sql); e != nil {
				engine = e[1]
			}
		}
		f.dbs[db][match[3]] = &fakeTable{Engine: engine, Create: sql}
		return "", 0, nil
	}
	if match := fakeIdentifiedRe.FindStringSubmatch(sql); match != nil {
		f.hashed[fakeUnquote(match[1])] = [2]string{match[2], match[3]}
		return "", 0, nil
	}
	if match := fakeDropUserRe.FindStringSubmatch(sql); match != nil {
		delete(f.users, fakeUnquote(match[1]))
		delete(f.hashed, fakeUnquote(match[1]))
		return "", 0, nil
	}
	for _, prefix := range []string{"CREATE DATABASE IF NOT EXISTS", "CREATE SETTINGS PROFILE", "ALTER SETTINGS PROFILE", "REVOKE ALL", "GRANT ", "KILL QUERY", "DROP SETTINGS PROFILE"} {
		if strings.HasPrefix(sql, prefix) {
			return "", 0, nil
		}
	}
	return "", 62, fmt.Errorf("fake ClickHouse does not understand %q", sql)
}

// seed creates a database with tables of rows each.
func (f *fakeClickHouse) seed(db string, tables map[string]uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dbs[db] = map[string]*fakeTable{}
	for name, rows := range tables {
		f.dbs[db][name] = &fakeTable{Engine: "MergeTree", Rows: rows, Create: "CREATE TABLE " + db + "." + name + " (id UInt64) ENGINE = MergeTree ORDER BY id"}
	}
}

// fileBackups are File() backups in a local directory (a container's
// backups directory in production).
type fileBackups struct{ dir string }

func (b fileBackups) ref(name string) string { return "File('" + name + "')" }
func (b fileBackups) open(_ context.Context, name string) (io.ReadCloser, int64, error) {
	file, err := os.Open(filepath.Join(b.dir, name))
	if err != nil {
		return nil, 0, err
	}
	info, _ := file.Stat()
	return file, info.Size(), nil
}
func (b fileBackups) create(_ context.Context, name string, data io.Reader, size int64) error {
	raw, err := io.ReadAll(io.LimitReader(data, size))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.dir, name), raw, 0o644)
}
func (b fileBackups) remove(_ context.Context, name string) error {
	return os.Remove(filepath.Join(b.dir, name))
}

func cmpString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (b fileBackups) dirRef(name string) string { return "File('" + name + "/')" }
func (b fileBackups) readDir(ctx context.Context, name string) (io.ReadCloser, error) {
	dir := filepath.Join(b.dir, name)
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	return tarDirectory(ctx, dir), nil
}
func (b fileBackups) writeDir(ctx context.Context, name string, stream io.Reader) error {
	return hostBackupFiles{dir: b.dir}.writeDirAt(ctx, filepath.Join(b.dir, name), stream)
}
func (b fileBackups) removeDir(_ context.Context, name string) error {
	return os.RemoveAll(filepath.Join(b.dir, name))
}
