package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A base restore that keeps tables. The final sync of a move restores the
// source's fresh dump over the destination database, which would empty the
// tables the final sync then only appends to (move_table_delta.go). With
// ?keep=schema.table,… the restore replaces every object in the dump except
// those tables and what is expensive or impossible to recreate around them:
// the table itself, its indexes and constraints, the sequences it owns and
// its schema. The dump's other entries, including foreign keys and triggers
// on the kept table (dropped and recreated with the objects they depend
// on) and the sequence values, apply as usual. The restore runs in place
// in one transaction, through pg_restore's list file (-L), so a failure
// still leaves the database unchanged.

// moveKeep names what the restore leaves alone, as schema.name sets.
type moveKeep struct {
	tables, indexes, sequences, schemas map[string]bool
}

// moveKeptObjects looks up, on the destination, the indexes and owned
// sequences of the kept tables.
func (m *Manager) moveKeptObjects(ctx context.Context, id string, tables []string) (moveKeep, error) {
	keep := moveKeep{tables: map[string]bool{}, indexes: map[string]bool{}, sequences: map[string]bool{}, schemas: map[string]bool{}}
	for _, table := range tables {
		keep.tables[table] = true
		schema, _, _ := strings.Cut(table, ".")
		keep.schemas[schema] = true
		text, err := movePSQL(ctx, m, id, "SELECT 'index', n.nspname || '.' || ic.relname FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid JOIN pg_catalog.pg_namespace n ON n.oid = ic.relnamespace WHERE i.indrelid = to_regclass('"+moveTableIdent(table)+"') "+
			"UNION ALL SELECT 'sequence', n.nspname || '.' || s.relname FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_class s ON s.oid = d.objid AND s.relkind = 'S' JOIN pg_catalog.pg_namespace n ON n.oid = s.relnamespace WHERE d.classid = 'pg_class'::regclass AND d.refobjid = to_regclass('"+moveTableIdent(table)+"') AND d.deptype = 'a'")
		if err != nil {
			return keep, errors.New("the kept tables' indexes could not be listed on the destination")
		}
		for _, line := range strings.Split(text, "\n") {
			kind, name, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			if kind == "index" {
				keep.indexes[name] = true
			} else {
				keep.sequences[name] = true
			}
		}
	}
	return keep, nil
}

// moveFilterRestoreList comments out, in a pg_restore -l listing, the
// entries that would drop or recreate a kept table or what it owns.
func moveFilterRestoreList(listing []byte, keep moveKeep) []byte {
	var out bytes.Buffer
	for _, line := range strings.SplitAfter(string(listing), "\n") {
		if moveRestoreEntryKept(strings.TrimRight(line, "\n"), keep) {
			out.WriteString(";")
		}
		out.WriteString(line)
	}
	return out.Bytes()
}

// moveRestoreEntryKept parses one listing line ("id; catalog oid DESC
// schema tag owner") and reports whether it belongs to a kept table.
func moveRestoreEntryKept(line string, keep moveKeep) bool {
	if line == "" || strings.HasPrefix(line, ";") {
		return false
	}
	_, rest, ok := strings.Cut(line, "; ")
	if !ok {
		return false
	}
	fields := strings.Fields(rest)
	if len(fields) < 5 {
		return false
	}
	rest = strings.Join(fields[2:], " ")
	for _, desc := range []string{"TABLE DATA", "SEQUENCE OWNED BY", "SEQUENCE SET", "SEQUENCE", "TABLE", "INDEX", "CONSTRAINT", "SCHEMA"} {
		if !strings.HasPrefix(rest, desc+" ") {
			continue
		}
		fields = strings.Fields(strings.TrimPrefix(rest, desc+" "))
		if len(fields) < 3 { // schema tag owner
			return false
		}
		schema, tag := fields[0], fields[1:len(fields)-1]
		name := schema + "." + tag[0]
		switch desc {
		case "TABLE", "TABLE DATA":
			return keep.tables[name]
		case "INDEX":
			return keep.indexes[name]
		case "SEQUENCE", "SEQUENCE OWNED BY":
			return keep.sequences[name]
		case "SEQUENCE SET":
			return false
		case "CONSTRAINT":
			return keep.tables[name] // tag is "table constraint"
		case "SCHEMA":
			return keep.schemas[tag[0]]
		}
	}
	return false
}

// moveRestoreKeeping restores dump in place over the destination, replacing
// everything but the kept tables.
func (m *Manager) moveRestoreKeeping(ctx context.Context, target moveDatabaseTarget, dump *os.File, keepTables []string) error {
	keep, err := m.moveKeptObjects(ctx, target.ID, keepTables)
	if err != nil {
		return err
	}
	options := []string{"--no-owner", "--no-acl", "--exit-on-error", "--single-transaction", "--clean", "--if-exists"}
	var listing bytes.Buffer
	if target.Host != nil {
		env, tool, err := m.moveHostRestoreCommand(*target.Host)
		if err != nil {
			return err
		}
		if err := pgRestoreRun(ctx, tool, []string{"-l"}, env, dump, func(line string) { listing.WriteString(line + "\n") }); err != nil {
			return errors.New("the dump's table of contents could not be read")
		}
		if _, err := dump.Seek(0, io.SeekStart); err != nil {
			return err
		}
		list, err := moveWriteRestoreList(m.cfg.DataDir, moveFilterRestoreList(listing.Bytes(), keep))
		if err != nil {
			return err
		}
		defer os.Remove(list)
		return m.moveRestoreHostDatabase(ctx, *target.Host, dump, append(options, "-L", list))
	}
	u, err := url.Parse(target.Database.URL)
	if err != nil || u.Path == "" {
		return errors.New("the destination database connection is invalid")
	}
	environment, flags := postgresEnvironment(target.Database.URL)
	base := []string{"run", "--rm", "-i", "--network", target.Database.Network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128"}
	base = append(base, flags...)
	if err := dockerStream(ctx, dump, &listing, environment, append(base, databaseImage, "pg_restore", "-l")...); err != nil {
		return errors.New("the dump's table of contents could not be read")
	}
	if _, err := dump.Seek(0, io.SeekStart); err != nil {
		return err
	}
	list, err := moveWriteRestoreList(m.cfg.DataDir, moveFilterRestoreList(listing.Bytes(), keep))
	if err != nil {
		return err
	}
	defer os.Remove(list)
	environment = append(environment, "PGOPTIONS="+pgRestoreOptions)
	args := append(base, "--env", "PGOPTIONS", "-v", list+":/lidza-restore.list:ro", databaseImage, "pg_restore", "--dbname="+strings.TrimPrefix(u.Path, "/"))
	args = append(args, options...)
	args = append(args, "-L", "/lidza-restore.list")
	if err := dockerStream(ctx, dump, io.Discard, environment, args...); err != nil {
		return errors.New("PostgreSQL restore failed; the destination database was left unchanged")
	}
	return nil
}

func moveWriteRestoreList(dir string, listing []byte) (string, error) {
	file, err := os.CreateTemp(dir, ".restore-list-*")
	if err != nil {
		return "", errors.New("the restore list could not be stored")
	}
	defer file.Close()
	if _, err := file.Write(listing); err != nil {
		os.Remove(file.Name())
		return "", errors.New("the restore list could not be stored")
	}
	return filepath.Clean(file.Name()), nil
}

// moveHostRestoreCommand is the environment and pg_restore path for a host
// database, as moveRestoreHostDatabase uses them.
func (m *Manager) moveHostRestoreCommand(pg NativeHostPostgres) ([]string, string, error) {
	password, err := m.unsealHostSecret(pg.Password)
	if err != nil {
		return nil, "", err
	}
	tool, err := movePGRestoreTool(pg.Major)
	if err != nil {
		return nil, "", err
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8", "PGHOST=127.0.0.1", "PGPORT=" + strconv.Itoa(pg.Port), "PGUSER=" + pg.Name,
		"PGPASSWORD=" + password, "PGDATABASE=" + pg.Name, "PGAPPNAME=lidza-move-restore", "PGCONNECT_TIMEOUT=10", "PGPASSFILE=/nonexistent"}
	return env, tool, nil
}

// moveKeepTables parses a restore's ?keep= list.
func moveKeepTables(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	tables := strings.Split(raw, ",")
	if err := validMoveTables(tables); err != nil {
		return nil, errors.New("keep must name exact schema.table names")
	}
	return tables, nil
}
