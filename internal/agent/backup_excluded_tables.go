package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Tables whose data backups skip. An operator may keep the rows of
// disposable tables (logs, caches, event streams) out of a PostgreSQL
// database's backups; their schema stays in every dump, so a restore
// recreates them empty. The choice is per database, saved in agent state
// from the panel:
//
//   - GET   /v1/databases/{id}/tables                     user tables with estimated rows and sizes
//   - PATCH /v1/databases/{id}/backups/excluded-tables    {"tables": ["schema.table", …]}
//
// The configuration key backup_exclude_table_data is legacy: it still
// supplies a database's initial list until the panel saves one, after
// which the saved list (even an empty one) applies.

// maxListedTables bounds a table listing; the largest tables come first.
const maxListedTables = 500

// DatabaseTable is one user table of a PostgreSQL database. Rows is
// PostgreSQL's estimate (pg_class.reltuples, or pg_stat_user_tables'
// live tuples for a table never analyzed), -1 when neither is known; Bytes
// is pg_total_relation_size. Excludable tables are ordinary tables with an
// exact lowercase schema.table name.
type DatabaseTable struct {
	Name       string `json:"name"`
	Rows       int64  `json:"rows"`
	Bytes      int64  `json:"bytes"`
	Excludable bool   `json:"excludable"`
	Excluded   bool   `json:"excluded"`
}

// DatabaseTables is a database's table listing. Truncated reports that
// only the largest maxListedTables tables are listed; Total counts all.
type DatabaseTables struct {
	Tables    []DatabaseTable `json:"tables"`
	Total     int             `json:"total"`
	Truncated bool            `json:"truncated,omitempty"`
	Excluded  []string        `json:"excluded"`
}

// ExcludedTablesRequest names the tables whose data backups skip.
type ExcludedTablesRequest struct {
	Tables []string `json:"tables"`
}

const userTablesWhere = "c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'"

// tableEstimateQuery lists tables matching where as name, kind, estimated
// rows (reltuples, else the live tuple statistics of a table never
// analyzed, else -1) and total size.
func tableEstimateQuery(where, suffix string) string {
	return "SELECT n.nspname || '.' || c.relname, c.relkind, CASE WHEN c.reltuples >= 0 THEN c.reltuples::bigint ELSE coalesce(s.n_live_tup, -1) END, pg_total_relation_size(c.oid) " +
		"FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace LEFT JOIN pg_catalog.pg_stat_user_tables s ON s.relid = c.oid WHERE " + where + suffix
}

var databaseTablesQuery = tableEstimateQuery(userTablesWhere, " ORDER BY 4 DESC, 1 LIMIT "+strconv.Itoa(maxListedTables))

// databasePSQL runs one read-only statement in a database: through the
// move runner for container and host databases, and through a short-lived
// client container for an external database. Tests replace it.
var databasePSQL = func(ctx context.Context, m *Manager, id, sql string) (string, error) {
	m.mu.Lock()
	_, err := m.moveDatabaseTargetLocked(id)
	d := m.data.Databases[id]
	m.mu.Unlock()
	if err == nil {
		return movePSQL(ctx, m, id, sql)
	}
	if d.Mode != "external" || d.URL == "" {
		return "", errors.New("the tables of this database cannot be listed")
	}
	var buffer limitedBuffer
	environment, flags := postgresEnvironment(d.URL)
	args := []string{"run", "--rm", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "32"}
	if d.Network != "" {
		args = append(args, "--network", d.Network)
	}
	args = append(args, flags...)
	args = append(args, databaseImage, "psql", "-X", "-q", "-A", "-t", "-F", "\t", "-v", "ON_ERROR_STOP=1", "-c", sql)
	err = dockerStream(ctx, nil, &buffer, environment, args...)
	return buffer.String(), err
}

// backupExcludedTablesLocked is the list of tables whose data a database's
// backups skip: the panel's saved list, else the legacy configuration
// default. Caller holds m.mu.
func (m *Manager) backupExcludedTablesLocked(id string) []string {
	if saved := m.data.Databases[id].ExcludedTables; saved != nil {
		return slices.Clone(*saved)
	}
	return slices.Clone(m.cfg.BackupExcludeTableData[id])
}

func (m *Manager) backupExcludedTables(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.backupExcludedTablesLocked(id)
}

// excludedTablesSupportedLocked refuses databases whose backups always
// contain everything: native, SQLite and ClickHouse resources. Caller holds
// m.mu.
func (m *Manager) excludedTablesSupportedLocked(id string) (Database, error) {
	d, ok := m.data.Databases[id]
	if !ok || !d.Removed.IsZero() {
		return d, errors.New("no database configured")
	}
	if _, native := m.data.NativeDatabases[id]; native {
		return d, errors.New("this database's backups always contain every table")
	}
	return d, nil
}

// databaseTables lists a PostgreSQL database's user tables, largest first.
func (m *Manager) databaseTables(ctx context.Context, id string) (DatabaseTables, error) {
	m.mu.Lock()
	_, err := m.excludedTablesSupportedLocked(id)
	excluded := m.backupExcludedTablesLocked(id)
	m.mu.Unlock()
	if err != nil {
		return DatabaseTables{}, err
	}
	tables, err := m.listDatabaseTables(ctx, id)
	if err != nil {
		return DatabaseTables{}, err
	}
	out := DatabaseTables{Tables: tables, Total: len(tables), Excluded: excluded}
	if len(tables) >= maxListedTables {
		out.Truncated = true
		if text, err := databasePSQL(ctx, m, id, moveTableQuery); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(text)); err == nil {
				out.Total = n
			}
		}
	}
	for i := range out.Tables {
		out.Tables[i].Excluded = slices.Contains(excluded, out.Tables[i].Name)
	}
	return out, nil
}

// listDatabaseTables reads the table listing without policy checks.
func (m *Manager) listDatabaseTables(ctx context.Context, id string) ([]DatabaseTable, error) {
	measure, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	text, err := databasePSQL(measure, m, id, databaseTablesQuery)
	if err != nil {
		return nil, errors.New("the tables of the database could not be listed")
	}
	return parseDatabaseTables(text), nil
}

// parseDatabaseTables reads tableEstimateQuery's unaligned output.
func parseDatabaseTables(text string) []DatabaseTable {
	out := []DatabaseTable{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) != 4 {
			continue
		}
		table := DatabaseTable{Name: fields[0], Rows: -1, Bytes: -1}
		if n, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
			table.Rows = max(n, -1)
		}
		if n, err := strconv.ParseInt(fields[3], 10, 64); err == nil {
			table.Bytes = n
		}
		table.Excludable = fields[1] == "r" && moveTableName.MatchString(table.Name)
		out = append(out, table)
	}
	return out
}

// validExcludedTables checks a requested list's shape: at most 100 exact,
// distinct lowercase schema.table names (none at all clears the list).
func validExcludedTables(tables []string) error {
	if len(tables) > moveMaxDataTables {
		return fmt.Errorf("skip the data of at most %d tables", moveMaxDataTables)
	}
	seen := map[string]bool{}
	for _, table := range tables {
		if !moveTableName.MatchString(table) {
			return errors.New("name tables as exact lowercase schema.table names")
		}
		if seen[table] {
			return fmt.Errorf("%s is listed twice", table)
		}
		seen[table] = true
	}
	return nil
}

// setBackupExcludedTables saves the tables whose data a database's backups
// skip. Every table must exist as an ordinary table, and at least one user
// table must keep its data.
func (m *Manager) setBackupExcludedTables(ctx context.Context, id string, tables []string) error {
	if err := validExcludedTables(tables); err != nil {
		return err
	}
	tables = slices.Clone(tables)
	slices.Sort(tables)
	m.mu.Lock()
	_, err := m.excludedTablesSupportedLocked(id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if len(tables) > 0 {
		literals := []string{}
		for _, table := range tables {
			literals = append(literals, "("+moveTableLiteral(table)+")")
		}
		check, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		text, err := databasePSQL(check, m, id, "SELECT n.nspname || '.' || c.relname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind = 'r' AND (n.nspname, c.relname) IN ("+strings.Join(literals, ", ")+")")
		if err != nil {
			return errors.New("the tables of the database could not be checked")
		}
		found := strings.Fields(text)
		missing := []string{}
		for _, table := range tables {
			if !slices.Contains(found, table) {
				missing = append(missing, table)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("no such table in %s: %s", id, strings.Join(missing, ", "))
		}
		text, err = databasePSQL(check, m, id, moveTableQuery)
		total, parseErr := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || parseErr != nil {
			return errors.New("the tables of the database could not be counted")
		}
		if len(tables) >= total {
			return errors.New("backups must keep the data of at least one table; leave a table out of the list")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, err := m.excludedTablesSupportedLocked(id)
	if err != nil {
		return err
	}
	if d.Operation != "" {
		return errors.New("wait for the database's running operation to finish")
	}
	old := d
	d.ExcludedTables = &tables
	m.data.Databases[id] = d
	if err := m.save(); err != nil {
		m.data.Databases[id] = old
		return err
	}
	return nil
}

func (m *Manager) backupExcludedTableRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/databases/{id}/tables", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !idPattern.MatchString(id) {
			Fail(w, 404, errors.New("unknown database"))
			return
		}
		tables, err := m.databaseTables(r.Context(), id)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, tables)
	})
	mux.HandleFunc("PATCH /v1/databases/{id}/backups/excluded-tables", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var in ExcludedTablesRequest
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if in.Tables == nil {
			in.Tables = []string{}
		}
		if err := validExcludedTables(in.Tables); err != nil || !idPattern.MatchString(id) {
			if err == nil {
				err = errors.New("unknown database")
			}
			Fail(w, 400, err)
			return
		}
		if err := m.setBackupExcludedTables(r.Context(), id, in.Tables); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "saved"})
	})
}
