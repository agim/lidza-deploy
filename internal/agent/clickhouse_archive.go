package agent

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ClickHouse database archives: the unit of every ClickHouse backup and of
// moving a database between servers. An archive is a tar stream, plain or
// compressed (stream_compress.go: readers detect it), whose first entry is
// manifest.json; it names the source database, the server version and
// every table with its row counts. The payload is one of
//
//   - backup/..., a ClickHouse BACKUP DATABASE written to a directory
//     (clickhouse_directory.go), restored from a directory again with
//     RESTORE DATABASE ... AS <target>, which renames the database and
//     every reference inside views. The default: it restores tens of
//     thousands of parts in seconds;
//   - backup.zip, a ClickHouse BACKUP DATABASE .zip archive, which older
//     agents wrote and every agent still restores (a .zip restore reads
//     each file with a scan of the archive and is slow for many parts), or
//   - one tables/NNNN.native file per table with data, in ClickHouse's Native
//     format, plus each object's CREATE statement in the manifest. Any
//     release reads it, so it also moves a database to an older server.
//
// Row counts are taken just before and just after the snapshot. With the
// source frozen (a move's final sync) they are equal and verification is
// exact; on a live source the restored count must lie between them.

const (
	clickhouseFormatBackup    = "backup"
	clickhouseFormatDirectory = "directory"
	clickhouseFormatNative    = "native"
	clickhouseManifestEntry   = "manifest.json"
	clickhouseBackupEntry     = "backup.zip"
	clickhouseArchiveFormat   = "lidza-clickhouse/1"
	clickhouseMaxManifest     = 8 << 20
	clickhouseMaxTables       = 4096
)

// clickhousePollInterval paces BACKUP/RESTORE status polls; tests shorten it.
var clickhousePollInterval = time.Second

// ClickHouseManifest describes one archive.
type ClickHouseManifest struct {
	Archive  string                    `json:"archive"`
	Format   string                    `json:"format"` // directory, backup (zip) or native
	Database string                    `json:"database"`
	Version  string                    `json:"version"`
	Created  time.Time                 `json:"created"`
	Tables   []ClickHouseManifestTable `json:"tables"`
	// Files is the number of files of a directory backup.
	Files int64 `json:"files,omitempty"`
	// CountCutoff makes row counts comparable across servers and time for
	// tables with a row TTL: only rows that expire after it are counted, on
	// the source and on the destination. Expired rows disappear whenever a
	// server merges parts, so raw counts of such tables never agree.
	CountCutoff time.Time `json:"count_cutoff,omitzero"`
}

// ClickHouseManifestTable is one table, view or dictionary.
type ClickHouseManifestTable struct {
	Name   string `json:"name"`
	Engine string `json:"engine"`
	Data   bool   `json:"data"` // the table stores rows
	// RowsBefore and RowsAfter bracket the snapshot.
	RowsBefore uint64 `json:"rows_before"`
	RowsAfter  uint64 `json:"rows_after"`
	Create     string `json:"create,omitempty"` // native archives
	File       string `json:"file,omitempty"`   // native archives, data tables
	// TTL is the table's row TTL expression; TTLComplex marks a TTL the
	// counts cannot filter by (several clauses, moves between volumes,
	// conditional deletes or aggregation), so only an upper bound is checked.
	TTL        string `json:"ttl,omitempty"`
	TTLComplex bool   `json:"ttl_complex,omitempty"`
}

// Rows is the table's row count when the snapshot was taken.
func (t ClickHouseManifestTable) Rows() uint64 { return max(t.RowsBefore, t.RowsAfter) }

// ClickHouseRestoreResult reports a verified restore.
type ClickHouseRestoreResult struct {
	Status   string            `json:"status"`
	Format   string            `json:"format"`
	Source   string            `json:"source_database"`
	Database string            `json:"database"`
	Tables   int               `json:"tables"`
	Rows     uint64            `json:"rows"`
	Counts   map[string]uint64 `json:"counts"`
	Bytes    int64             `json:"bytes,omitempty"`
	SHA256   string            `json:"sha256,omitempty"`
}

// clickhouseService is one managed ClickHouse server with the database an
// application uses on it.
type clickhouseService struct {
	mode     string // host or container
	database string
	client   clickhouseClient
	files    clickhouseBackupFiles
}

// clickhouseBackupFiles is where the server reads and writes BACKUP files.
type clickhouseBackupFiles interface {
	ref(name string) string // the BACKUP/RESTORE destination expression
	open(ctx context.Context, name string) (io.ReadCloser, int64, error)
	create(ctx context.Context, name string, data io.Reader, size int64) error
	remove(ctx context.Context, name string) error
	// Directory backups (clickhouse_directory.go).
	dirRef(name string) string
	readDir(ctx context.Context, name string) (io.ReadCloser, error)
	writeDir(ctx context.Context, name string, stream io.Reader) error
	removeDir(ctx context.Context, name string) error
}

var clickhouseBackupFileName = regexp.MustCompile(`^lidza-[a-z0-9-]{1,64}\.zip$`)

func newClickHouseBackupFileName() string { return "lidza-" + newID() + ".zip" }

// ---- inventory ----------------------------------------------------------------------

type clickhouseObject struct {
	Name    string `json:"name"`
	Engine  string `json:"engine"`
	OwnData uint8  `json:"has_own_data"`
	Create  string `json:"create_table_query"`
}

// rowTTL is the table-level TTL expression of a MergeTree table, taken from
// its normalized CREATE statement, and whether it is more than one plain
// expression. Column TTLs (before ENGINE) are not row TTLs.
func (o clickhouseObject) rowTTL() (expr string, complex bool) {
	create := o.Create
	engine := strings.Index(create, "ENGINE")
	if engine < 0 {
		return "", false
	}
	rest := create[engine:]
	at := strings.Index(rest, " TTL ")
	if at < 0 {
		at = strings.Index(rest, "\nTTL ")
	}
	if at < 0 {
		return "", false
	}
	expr = rest[at+5:]
	for _, stop := range []string{"\nSETTINGS ", " SETTINGS ", "\nSETTINGS\n"} {
		if i := strings.Index(expr, stop); i >= 0 {
			expr = expr[:i]
		}
	}
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", false
	}
	for _, clause := range []string{" TO VOLUME ", " TO DISK ", " DELETE", " WHERE ", " GROUP BY ", " RECOMPRESS "} {
		if strings.Contains(expr, clause) {
			return expr, true
		}
	}
	depth := 0
	for _, r := range expr {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				return expr, true
			}
		}
	}
	return expr, false
}

// countFilter is the WHERE clause that counts only rows expiring after
// cutoff, for a table with a plain row TTL; empty otherwise.
func (o clickhouseObject) countFilter(cutoff time.Time) string {
	expr, complex := o.rowTTL()
	if expr == "" || complex || cutoff.IsZero() {
		return ""
	}
	return " WHERE (" + expr + ") > toDateTime('" + cutoff.UTC().Format("2006-01-02 15:04:05") + "', 'UTC')"
}

func (o clickhouseObject) data() bool {
	switch o.Engine {
	case "View", "MaterializedView", "Dictionary", "LiveView", "WindowView":
		return false
	}
	return o.OwnData == 1
}

// clickhouseObjectRank orders creation for a per-table restore: tables with
// data first, then other tables, dictionaries, views and materialized views
// (which must not see the copied rows as new inserts).
func clickhouseObjectRank(o ClickHouseManifestTable) int {
	switch {
	case o.Data:
		return 0
	case o.Engine == "Dictionary":
		return 2
	case o.Engine == "View":
		return 3
	case o.Engine == "MaterializedView" || o.Engine == "LiveView" || o.Engine == "WindowView":
		return 4
	}
	return 1
}

func (s clickhouseService) objects(ctx context.Context, database string) ([]clickhouseObject, error) {
	literal, err := chString(database)
	if err != nil {
		return nil, err
	}
	objects, err := clickhouseRows[clickhouseObject](ctx, s.client, "SELECT name, engine, has_own_data, create_table_query FROM system.tables WHERE database = "+literal+" AND NOT is_temporary ORDER BY name")
	if err != nil {
		return nil, err
	}
	if len(objects) > clickhouseMaxTables {
		return nil, fmt.Errorf("the database has more than %d tables", clickhouseMaxTables)
	}
	return objects, nil
}

func (s clickhouseService) count(ctx context.Context, database, table string) (uint64, error) {
	return s.countWhere(ctx, database, table, "")
}

func (s clickhouseService) countWhere(ctx context.Context, database, table, where string) (uint64, error) {
	out, err := s.client.query(ctx, "SELECT count() FROM "+mustCHIdent(database)+"."+mustCHIdent(table)+where)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(out, 10, 64)
	if err != nil {
		return 0, errors.New("ClickHouse returned an unexpected row count")
	}
	return n, nil
}

// counts the rows of every data table; tables with a plain row TTL count
// only rows that expire after cutoff (see ClickHouseManifest.CountCutoff).
func (s clickhouseService) counts(ctx context.Context, database string, objects []clickhouseObject, cutoff time.Time) (map[string]uint64, error) {
	counts := map[string]uint64{}
	for _, o := range objects {
		if !o.data() {
			continue
		}
		n, err := s.countWhere(ctx, database, o.Name, o.countFilter(cutoff))
		if err != nil {
			return nil, fmt.Errorf("counting %s: %w", o.Name, err)
		}
		counts[o.Name] = n
	}
	return counts, nil
}

// ClickHouseSummary is what the panel shows about one database.
type ClickHouseSummary struct {
	Version   string `json:"version,omitempty"`
	Tables    int    `json:"tables"`
	Rows      uint64 `json:"rows"`
	SizeBytes int64  `json:"size_bytes"`
	// UncompressedBytes is the data size before compression; a per-table
	// (Native) export writes about this much.
	UncompressedBytes int64 `json:"uncompressed_bytes,omitempty"`
}

// summary measures a database from system.tables and system.parts; it never
// scans table data.
func (s clickhouseService) summary(ctx context.Context) (ClickHouseSummary, error) {
	var out ClickHouseSummary
	version, err := s.client.version(ctx)
	if err != nil {
		return out, err
	}
	out.Version = version
	literal, err := chString(s.database)
	if err != nil {
		return out, err
	}
	type row struct {
		Tables uint64 `json:"tables"`
		Rows   uint64 `json:"rows"`
	}
	tables, err := clickhouseRows[row](ctx, s.client, "SELECT count() AS tables, toUInt64(sum(ifNull(total_rows, 0))) AS rows FROM system.tables WHERE database = "+literal+" AND NOT is_temporary")
	if err != nil {
		return out, err
	}
	type parts struct {
		Bytes        uint64 `json:"bytes"`
		Uncompressed uint64 `json:"uncompressed"`
	}
	size, err := clickhouseRows[parts](ctx, s.client, "SELECT toUInt64(sum(bytes_on_disk)) AS bytes, toUInt64(sum(data_uncompressed_bytes)) AS uncompressed FROM system.parts WHERE database = "+literal+" AND active")
	if err != nil {
		return out, err
	}
	if len(tables) == 1 {
		out.Tables, out.Rows = int(tables[0].Tables), tables[0].Rows
	}
	if len(size) == 1 {
		out.SizeBytes = int64(min(size[0].Bytes, 1<<62))
		out.UncompressedBytes = int64(min(size[0].Uncompressed, 1<<62))
	}
	return out, nil
}

// ---- BACKUP and RESTORE -----------------------------------------------------------

var clickhouseOperationID = regexp.MustCompile(`^[0-9A-Za-z-]{1,64}$`)

// runClickHouseOperation starts BACKUP or RESTORE with ASYNC and waits for
// its final status, so a long operation never depends on one HTTP request.
// A cancelled context kills the operation instead of waiting for it.
func runClickHouseOperation(ctx context.Context, c clickhouseClient, sql string) error {
	_, err := runClickHouseOperationPhase(ctx, c, sql, "")
	return err
}

// runClickHouseOperationPhase reports the operation's progress from
// system.backups under phase (files and bytes read by a RESTORE, files
// written by a BACKUP).
func runClickHouseOperationPhase(ctx context.Context, c clickhouseClient, sql, phase string) (clickhouseOperationStatus, error) {
	var last clickhouseOperationStatus
	out, err := c.query(ctx, sql+" ASYNC")
	if err != nil {
		return last, err
	}
	id, _, _ := strings.Cut(strings.SplitN(out, "\n", 2)[0], "\t")
	if !clickhouseOperationID.MatchString(id) {
		return last, errors.New("ClickHouse did not report the operation id")
	}
	restore := strings.HasPrefix(sql, "RESTORE")
	tracker := progressFrom(ctx)
	var read atomic.Int64
	if phase != "" {
		tracker.Stage(phase, 0, read.Load)
	}
	queryID := ""
	finished, sized := false, false
	defer func() {
		if !finished {
			cancelClickHouseOperation(ctx, c, id, queryID)
		}
	}()
	missing := time.Time{}
	for {
		rows, err := clickhouseRows[clickhouseOperationStatus](ctx, c, "SELECT status, error, query_id, num_files, files_read, total_size, bytes_read FROM system.backups WHERE id = "+mustCHString(id))
		if err != nil {
			return last, err
		}
		if len(rows) == 1 {
			last = rows[0]
			queryID = rows[0].QueryID
			if phase != "" && restore && !sized && rows[0].TotalSize > 0 {
				// The size is known once the restore read the backup's metadata.
				tracker.Stage(phase, int64(min(rows[0].TotalSize, 1<<62)), read.Load)
				sized = true
			}
			rows[0].progress(tracker, phase, restore, &read)
		}
		if len(rows) == 0 {
			if missing.IsZero() {
				missing = time.Now()
			} else if time.Since(missing) > 30*time.Second {
				return last, errors.New("ClickHouse lost track of the operation; was the server restarted?")
			}
		} else {
			switch rows[0].Status {
			case "BACKUP_CREATED", "RESTORED":
				finished = true
				return last, nil
			case "BACKUP_FAILED", "RESTORE_FAILED", "BACKUP_CANCELLED", "RESTORE_CANCELLED":
				finished = true
				detail := boundedErrorText(c.scrub(strings.TrimSpace(rows[0].Error)), 600)
				if detail == "" {
					detail = rows[0].Status
				}
				return last, errors.New("ClickHouse " + strings.ToLower(strings.ReplaceAll(rows[0].Status, "_", " ")) + ": " + detail)
			}
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(clickhousePollInterval):
		}
	}
}

// ---- export ---------------------------------------------------------------------------

// exportClickHouseArchive writes an archive of the service's database to w.
// spool holds per-table files of a native export while it is assembled.
func exportClickHouseArchive(ctx context.Context, s clickhouseService, format, spool string, w io.Writer) (ClickHouseManifest, error) {
	manifest := ClickHouseManifest{Archive: clickhouseArchiveFormat, Format: format, Database: s.database, Created: time.Now().UTC(), Tables: []ClickHouseManifestTable{}}
	if format != clickhouseFormatBackup && format != clickhouseFormatDirectory && format != clickhouseFormatNative {
		return manifest, errors.New("unknown ClickHouse archive format")
	}
	version, err := s.client.version(ctx)
	if err != nil {
		return manifest, err
	}
	manifest.Version = version
	objects, err := s.objects(ctx, s.database)
	if err != nil {
		return manifest, err
	}
	// Rows expiring within the next hours are left out of every count: the
	// destination may merge them away before it is verified.
	manifest.CountCutoff = manifest.Created.Add(6 * time.Hour)
	before, err := s.counts(ctx, s.database, objects, manifest.CountCutoff)
	if err != nil {
		return manifest, err
	}
	for _, o := range objects {
		entry := ClickHouseManifestTable{Name: o.Name, Engine: o.Engine, Data: o.data(), RowsBefore: before[o.Name]}
		entry.TTL, entry.TTLComplex = o.rowTTL()
		if format == clickhouseFormatNative {
			if strings.HasPrefix(o.Name, ".inner") {
				return manifest, fmt.Errorf("%s stores a materialized view's rows in an inner table; a per-table copy cannot preserve it (copy between servers of the same release instead)", o.Name)
			}
			entry.Create = o.Create
		}
		manifest.Tables = append(manifest.Tables, entry)
	}
	slices.SortStableFunc(manifest.Tables, func(a, b ClickHouseManifestTable) int { return clickhouseObjectRank(a) - clickhouseObjectRank(b) })
	after := func() error {
		counts, err := s.counts(ctx, s.database, objects, manifest.CountCutoff)
		if err != nil {
			return err
		}
		for i := range manifest.Tables {
			manifest.Tables[i].RowsAfter = counts[manifest.Tables[i].Name]
		}
		return nil
	}
	tw := tar.NewWriter(w)
	writeManifest := func() error {
		raw, err := json.Marshal(manifest)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: clickhouseManifestEntry, Mode: 0o600, Size: int64(len(raw)), ModTime: manifest.Created, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err = tw.Write(raw)
		return err
	}
	if format == clickhouseFormatDirectory {
		name := newClickHouseBackupDirName()
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			_ = s.files.removeDir(cleanup, name)
		}()
		started := time.Now()
		status, err := runClickHouseOperationPhase(ctx, s.client, "BACKUP DATABASE "+mustCHIdent(s.database)+" TO "+s.files.dirRef(name), "Creating the ClickHouse backup")
		if err != nil {
			return manifest, err
		}
		manifest.Files = int64(min(status.NumFiles, 1<<40))
		backupSeconds := time.Since(started)
		if err := after(); err != nil {
			return manifest, err
		}
		stream, err := s.files.readDir(ctx, name)
		if err != nil {
			return manifest, err
		}
		defer stream.Close()
		if err := writeManifest(); err != nil {
			return manifest, err
		}
		started = time.Now()
		files, err := copyClickHouseDirectory(ctx, tw, stream, manifest.Created)
		if err == nil {
			err = stream.Close()
		}
		if err != nil {
			return manifest, err
		}
		commandDiagnostic(ctx, "backup", fmt.Sprintf("ClickHouse BACKUP to a directory took %s; streaming its %s took %s.", backupSeconds.Round(time.Millisecond), clickhouseFiles(files), time.Since(started).Round(time.Millisecond)))
		return manifest, tw.Close()
	}
	if format == clickhouseFormatBackup {
		name := newClickHouseBackupFileName()
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = s.files.remove(cleanup, name)
		}()
		if err := runClickHouseOperation(ctx, s.client, "BACKUP DATABASE "+mustCHIdent(s.database)+" TO "+s.files.ref(name)); err != nil {
			return manifest, err
		}
		if err := after(); err != nil {
			return manifest, err
		}
		file, size, err := s.files.open(ctx, name)
		if err != nil {
			return manifest, err
		}
		defer file.Close()
		if err := writeManifest(); err != nil {
			return manifest, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: clickhouseBackupEntry, Mode: 0o600, Size: size, ModTime: manifest.Created, Format: tar.FormatPAX}); err != nil {
			return manifest, err
		}
		if n, err := io.Copy(tw, io.LimitReader(file, size)); err != nil || n != size {
			return manifest, errors.New("the ClickHouse backup file could not be read completely")
		}
		return manifest, tw.Close()
	}
	dir, err := os.MkdirTemp(spool, ".clickhouse-export-*")
	if err != nil {
		return manifest, errors.New("cannot prepare the ClickHouse export directory")
	}
	defer os.RemoveAll(dir)
	sizes := map[string]int64{}
	for i := range manifest.Tables {
		table := &manifest.Tables[i]
		if !table.Data {
			continue
		}
		table.File = fmt.Sprintf("tables/%04d.native", i+1)
		path := filepath.Join(dir, filepath.Base(table.File))
		if err := keepDiskReserve(dir); err != nil {
			return manifest, err
		}
		if err := s.exportTable(ctx, table.Name, path); err != nil {
			return manifest, fmt.Errorf("exporting %s: %w", table.Name, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return manifest, err
		}
		sizes[table.File] = info.Size()
	}
	if err := after(); err != nil {
		return manifest, err
	}
	if err := writeManifest(); err != nil {
		return manifest, err
	}
	for _, table := range manifest.Tables {
		if table.File == "" {
			continue
		}
		file, err := os.Open(filepath.Join(dir, filepath.Base(table.File)))
		if err != nil {
			return manifest, err
		}
		err = tw.WriteHeader(&tar.Header{Name: table.File, Mode: 0o600, Size: sizes[table.File], ModTime: manifest.Created, Format: tar.FormatPAX})
		if err == nil {
			_, err = io.Copy(tw, file)
		}
		file.Close()
		// The spool copy is not needed once it is in the archive, so the
		// export never holds every table twice.
		os.Remove(filepath.Join(dir, filepath.Base(table.File)))
		if err != nil {
			return manifest, err
		}
	}
	return manifest, tw.Close()
}

func (s clickhouseService) exportTable(ctx context.Context, table, path string) error {
	stream, err := s.client.stream(ctx, "SELECT * FROM "+mustCHIdent(s.database)+"."+mustCHIdent(table)+" FORMAT Native")
	if err != nil {
		return err
	}
	defer stream.Close()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(file, stream); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// ---- restore --------------------------------------------------------------------------

// readClickHouseManifest reads and validates an archive's first entry.
func readClickHouseManifest(tr *tar.Reader) (ClickHouseManifest, error) {
	var manifest ClickHouseManifest
	header, err := tr.Next()
	if err != nil || header.Name != clickhouseManifestEntry || header.Size <= 0 || header.Size > clickhouseMaxManifest {
		return manifest, errors.New("the archive does not start with a ClickHouse manifest")
	}
	decoder := json.NewDecoder(io.LimitReader(tr, header.Size))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, errors.New("the ClickHouse manifest is invalid")
	}
	if manifest.Archive != clickhouseArchiveFormat || manifest.Format != clickhouseFormatBackup && manifest.Format != clickhouseFormatDirectory && manifest.Format != clickhouseFormatNative {
		return manifest, errors.New("the archive format is not supported by this agent")
	}
	if _, err := chIdent(manifest.Database); err != nil || len(manifest.Tables) > clickhouseMaxTables {
		return manifest, errors.New("the ClickHouse manifest is invalid")
	}
	files := map[string]bool{}
	for _, table := range manifest.Tables {
		if _, err := chIdent(table.Name); err != nil {
			return manifest, errors.New("the ClickHouse manifest names an invalid table")
		}
		if manifest.Format == clickhouseFormatNative {
			if table.Create == "" || table.Data != (table.File != "") || table.File != "" && (!regexp.MustCompile(`^tables/\d{4}\.native$`).MatchString(table.File) || files[table.File]) {
				return manifest, errors.New("the ClickHouse manifest is incomplete")
			}
			files[table.File] = true
		}
	}
	return manifest, nil
}

// restoreClickHouseArchive restores an archive into the service's database.
// With replace the database is dropped first; otherwise it must not have
// any table. The restored row counts are verified against the manifest.
func restoreClickHouseArchive(ctx context.Context, s clickhouseService, archive io.Reader, replace bool) (ClickHouseRestoreResult, error) {
	result := ClickHouseRestoreResult{Database: s.database, Counts: map[string]uint64{}}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	plain, _, err := decompressedReader(ctx, archive)
	if err != nil {
		return result, err
	}
	defer plain.Close()
	tr := tar.NewReader(plain)
	manifest, err := readClickHouseManifest(tr)
	if err != nil {
		return result, err
	}
	result.Format, result.Source = manifest.Format, manifest.Database
	existing, err := s.objects(ctx, s.database)
	if err != nil {
		return result, err
	}
	if len(existing) > 0 && !replace {
		return result, errors.New("the destination ClickHouse database already has tables; restore with replace")
	}
	err = s.replaceDatabase(ctx, func(ctx context.Context) error {
		if err := s.restorePayload(ctx, tr, manifest); err != nil {
			return err
		}
		var verifyErr error
		result, verifyErr = s.verifyRestore(ctx, manifest, result)
		return verifyErr
	})
	if err != nil {
		return result, err
	}
	// The whole stream was read: a damaged compressed stream fails here.
	if _, err := io.Copy(io.Discard, plain); err != nil {
		return result, err
	}
	return result, plain.Close()
}

// restorePayload restores the archive's payload into the service's
// database, which does not exist yet.
func (s clickhouseService) restorePayload(ctx context.Context, tr *tar.Reader, manifest ClickHouseManifest) error {
	target := mustCHIdent(s.database)
	statement := "RESTORE DATABASE " + mustCHIdent(manifest.Database)
	if manifest.Database != s.database {
		statement += " AS " + target
	}
	switch manifest.Format {
	case clickhouseFormatDirectory:
		header, err := tr.Next()
		if err != nil || !strings.HasPrefix(header.Name, clickhouseDirectoryPrefix) {
			return errors.New("the archive has no ClickHouse backup")
		}
		name := newClickHouseBackupDirName()
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			_ = s.files.removeDir(cleanup, name)
		}()
		var files atomic.Int64
		progress := progressFrom(ctx)
		progress.Stage("Extracting the ClickHouse backup", 0, nil)
		stream := directoryStream(ctx, tr, header, &files)
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Second):
					progress.Objects("files", files.Load(), int64(manifest.Files))
				}
			}
		}()
		started := time.Now()
		err = s.files.writeDir(ctx, name, stream)
		close(stop)
		stream.Close()
		if err != nil {
			return err
		}
		extracted := time.Since(started)
		started = time.Now()
		if _, err := runClickHouseOperationPhase(ctx, s.client, statement+" FROM "+s.files.dirRef(name), "Restoring the ClickHouse backup"); err != nil {
			return err
		}
		commandDiagnostic(ctx, "restore", fmt.Sprintf("Extracting %s of the ClickHouse backup took %s; RESTORE took %s.", clickhouseFiles(files.Load()), extracted.Round(time.Millisecond), time.Since(started).Round(time.Millisecond)))
		return nil
	case clickhouseFormatBackup:
		header, err := tr.Next()
		if err != nil || header.Name != clickhouseBackupEntry || header.Size <= 0 {
			return errors.New("the archive has no ClickHouse backup")
		}
		name := newClickHouseBackupFileName()
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = s.files.remove(cleanup, name)
		}()
		if err := s.files.create(ctx, name, io.LimitReader(tr, header.Size), header.Size); err != nil {
			return err
		}
		_, err = runClickHouseOperationPhase(ctx, s.client, statement+" FROM "+s.files.ref(name), "Restoring the ClickHouse backup")
		return err
	}
	return s.restoreNative(ctx, tr, manifest)
}

func (s clickhouseService) restoreNative(ctx context.Context, tr *tar.Reader, manifest ClickHouseManifest) error {
	target := mustCHIdent(s.database)
	if err := s.client.exec(ctx, "CREATE DATABASE IF NOT EXISTS "+target); err != nil {
		return err
	}
	pending := []ClickHouseManifestTable{}
	for _, table := range manifest.Tables {
		if !table.Data {
			pending = append(pending, table)
			continue
		}
		if err := s.client.exec(ctx, rewriteClickHouseDatabase(table.Create, manifest.Database, s.database)); err != nil {
			return fmt.Errorf("creating %s: %w", table.Name, err)
		}
		header, err := tr.Next()
		if err != nil || header.Name != table.File {
			return errors.New("the archive's table data is out of order")
		}
		statement := "INSERT INTO " + target + "." + mustCHIdent(table.Name) + " SETTINGS optimize_on_insert = 0 FORMAT Native"
		if err := s.client.insert(ctx, statement, io.LimitReader(tr, header.Size)); err != nil {
			return fmt.Errorf("loading %s: %w", table.Name, err)
		}
	}
	// Views may reference each other: create in passes until none is left.
	for len(pending) > 0 {
		var failed []ClickHouseManifestTable
		var last error
		for _, object := range pending {
			if err := s.client.exec(ctx, rewriteClickHouseDatabase(object.Create, manifest.Database, s.database)); err != nil {
				failed, last = append(failed, object), fmt.Errorf("creating %s: %w", object.Name, err)
			}
		}
		if len(failed) == len(pending) {
			return last
		}
		pending = failed
	}
	return nil
}

// clickhouseCollapsing engines may legitimately merge rows away after the
// snapshot, so only an upper bound applies to their restored counts.
func clickhouseCollapsing(engine string) bool {
	for _, family := range []string{"Replacing", "Collapsing", "Summing", "Aggregating", "Graphite", "Coalescing"} {
		if strings.Contains(engine, family) {
			return true
		}
	}
	return false
}

// VerifyClickHouseRestore checks a destination's reported row counts
// against the tables of the source's export (the control panel's second
// check; the destination also verifies every object exists).
func VerifyClickHouseRestore(tables []ClickHouseManifestTable, counts map[string]uint64) error {
	present := map[string]bool{}
	for name := range counts {
		present[name] = true
	}
	for _, table := range tables {
		if !table.Data {
			present[table.Name] = true
		}
	}
	return verifyClickHouseCounts(ClickHouseManifest{Tables: tables}, counts, present)
}

// verifyClickHouseCounts compares restored counts with the manifest.
func verifyClickHouseCounts(manifest ClickHouseManifest, restored map[string]uint64, present map[string]bool) error {
	// Inner tables of materialized views are named after the view's UUID,
	// which a restore assigns anew: compare their number and total rows.
	var innerWant, innerGot int
	var innerLow, innerHigh, innerRows uint64
	for name := range present {
		if strings.HasPrefix(name, ".inner") {
			innerGot++
			innerRows += restored[name]
		}
	}
	for _, table := range manifest.Tables {
		if strings.HasPrefix(table.Name, ".inner") {
			innerWant++
			innerLow += min(table.RowsBefore, table.RowsAfter)
			innerHigh += max(table.RowsBefore, table.RowsAfter)
			continue
		}
		if !present[table.Name] {
			return fmt.Errorf("%s is missing after the restore", table.Name)
		}
		if !table.Data {
			continue
		}
		low, high := min(table.RowsBefore, table.RowsAfter), max(table.RowsBefore, table.RowsAfter)
		got := restored[table.Name]
		if clickhouseCollapsing(table.Engine) || table.TTLComplex {
			if got > high || high > 0 && got == 0 {
				return fmt.Errorf("%s has %d rows after the restore; the source had at most %d", table.Name, got, high)
			}
			continue
		}
		if got < low || got > high {
			if low == high {
				return fmt.Errorf("%s has %d rows after the restore; the source had %d", table.Name, got, high)
			}
			return fmt.Errorf("%s has %d rows after the restore; the source had %d to %d", table.Name, got, low, high)
		}
	}
	// Inner tables usually aggregate: an upper bound applies, as above.
	if innerGot != innerWant || innerRows > innerHigh || innerHigh > 0 && innerRows == 0 {
		return fmt.Errorf("materialized views have %d inner tables with %d rows after the restore; the source had %d with %d to %d", innerGot, innerRows, innerWant, innerLow, innerHigh)
	}
	return nil
}

func (s clickhouseService) verifyRestore(ctx context.Context, manifest ClickHouseManifest, result ClickHouseRestoreResult) (ClickHouseRestoreResult, error) {
	objects, err := s.objects(ctx, s.database)
	if err != nil {
		return result, err
	}
	counts, err := s.counts(ctx, s.database, objects, manifest.CountCutoff)
	if err != nil {
		return result, err
	}
	present := map[string]bool{}
	for _, o := range objects {
		present[o.Name] = true
	}
	if err := verifyClickHouseCounts(manifest, counts, present); err != nil {
		return result, fmt.Errorf("row-count verification failed: %w", err)
	}
	result.Status, result.Tables, result.Counts = "restored", len(objects), counts
	for _, n := range counts {
		result.Rows += n
	}
	return result, nil
}

// rewriteClickHouseDatabase points a CREATE statement of a per-table archive
// at the destination database: qualified references to the source database
// (quoted or not) are renamed; everything else is left unchanged.
func rewriteClickHouseDatabase(statement, from, to string) string {
	if from == to {
		return statement
	}
	replacement := mustCHIdent(to)
	statement = strings.ReplaceAll(statement, mustCHIdent(from)+".", replacement+".")
	if !clickhouseNamePattern.MatchString(from) {
		return statement
	}
	pattern := regexp.MustCompile("(^|[^A-Za-z0-9_.`'\"])" + regexp.QuoteMeta(from) + `\.`)
	return pattern.ReplaceAllString(statement, "${1}"+strings.ReplaceAll(replacement, "$", "$$")+".")
}
