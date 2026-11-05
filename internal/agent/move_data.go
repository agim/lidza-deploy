package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Database transfer for application moves. The source agent serves a
// finished backup with its recorded SHA-256; the destination agent stores the
// upload, verifies length and digest before touching any database, then
// restores it in one transaction into the database it provisioned for the
// application. Nothing is restored on a mismatch.

// moveMaxDumpSize bounds one transferred dump.
var moveMaxDumpSize = maxExtendedBackupSize

var moveSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const moveTableQuery = "SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'"

// moveDatabaseTarget is a database this agent can measure or restore into:
// a container PostgreSQL (local mode) or the host PostgreSQL database it
// provisioned for a native application.
type moveDatabaseTarget struct {
	ID       string
	Database Database
	Host     *NativeHostPostgres // nil for a container database
}

func (m *Manager) moveDatabaseTargetLocked(id string) (moveDatabaseTarget, error) {
	d, ok := m.data.Databases[id]
	if !ok {
		return moveDatabaseTarget{}, errors.New("unknown database")
	}
	if d.Mode == "local" && d.Network != "" {
		return moveDatabaseTarget{ID: id, Database: d}, nil
	}
	for _, record := range m.data.NativeHostData {
		if pg := record.Postgres; pg != nil && pg.Resource == id {
			copy := *pg
			return moveDatabaseTarget{ID: id, Database: d, Host: &copy}, nil
		}
	}
	return moveDatabaseTarget{}, errors.New("only container databases and host databases this agent provisioned can be measured or restored")
}

// moveTableCount counts user tables; tests replace it.
var moveTableCount = func(ctx context.Context, m *Manager, id string) (int, error) {
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		return 0, err
	}
	var out string
	if target.Host != nil {
		out, err = nativeHostRun(ctx, "", "/usr/sbin/runuser", "-u", "postgres", "--", "/usr/bin/psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1",
			"-h", nativePGSocketDir, "-p", strconv.Itoa(target.Host.Port), "-d", target.Host.Name, "-c", moveTableQuery)
	} else {
		var buffer limitedBuffer
		environment, flags := postgresEnvironment(target.Database.URL)
		args := []string{"run", "--rm", "--network", target.Database.Network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "128m", "--pids-limit", "32"}
		args = append(args, flags...)
		args = append(args, databaseImage, "psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", moveTableQuery)
		err = dockerStream(ctx, nil, &buffer, environment, args...)
		out = buffer.String()
	}
	if err != nil {
		return 0, errors.New("the database could not be measured")
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || count < 0 {
		return 0, errors.New("the database returned an unexpected table count")
	}
	return count, nil
}

// moveRestoreDump restores a verified dump; tests replace it. With replace,
// objects in the dump replace existing ones; either way the restore is one
// transaction, so a failure leaves the database unchanged.
var moveRestoreDump = func(ctx context.Context, m *Manager, target moveDatabaseTarget, dump *os.File, replace bool) error {
	options := []string{"--no-owner", "--no-acl", "--exit-on-error", "--single-transaction"}
	if replace {
		options = append(options, "--clean", "--if-exists")
	}
	if target.Host != nil {
		return m.moveRestoreHostDatabase(ctx, *target.Host, dump, options)
	}
	u, err := url.Parse(target.Database.URL)
	if err != nil || u.Path == "" {
		return errors.New("the destination database connection is invalid")
	}
	environment, flags := postgresEnvironment(target.Database.URL)
	container := "lidza-move-restore-" + newID()
	args := []string{"run", "--rm", "-i", "--name", container, "--network", target.Database.Network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "512m", "--pids-limit", "128"}
	args = append(args, flags...)
	args = append(args, databaseImage, "pg_restore", "--dbname="+strings.TrimPrefix(u.Path, "/"))
	args = append(args, options...)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = command(cleanup, "", nil, "docker", "rm", "-f", container)
	}()
	if err := dockerStream(ctx, dump, io.Discard, environment, args...); err != nil {
		return errors.New("PostgreSQL restore failed; the destination database was left unchanged")
	}
	return nil
}

// moveRestoreHostDatabase restores as the application's own role over
// loopback, so restored objects belong to it. The password travels in the
// tool's environment, never in its arguments.
func (m *Manager) moveRestoreHostDatabase(ctx context.Context, pg NativeHostPostgres, dump *os.File, options []string) error {
	password, err := m.unsealHostSecret(pg.Password)
	if err != nil {
		return err
	}
	tool, err := movePGRestoreTool(pg.Major)
	if err != nil {
		return err
	}
	args := append([]string{"--no-password", "--dbname=" + pg.Name}, options...)
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8", "PGHOST=127.0.0.1", "PGPORT=" + strconv.Itoa(pg.Port), "PGUSER=" + pg.Name,
		"PGPASSWORD=" + password, "PGDATABASE=" + pg.Name, "PGAPPNAME=lidza-move-restore", "PGCONNECT_TIMEOUT=10", "PGPASSFILE=/nonexistent"}
	cmd.Stdin = dump
	var output limitedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("PostgreSQL restore failed; the destination database was left unchanged")
	}
	return nil
}

// movePGRestoreTool picks the newest installed pg_restore that is at least
// the cluster's version: a newer client reads archives of older servers.
func movePGRestoreTool(major int) (string, error) {
	best := 0
	for version := nativePGMaxMajor + 2; version >= major; version-- {
		path := filepath.Join("/usr/lib/postgresql", strconv.Itoa(version), "bin/pg_restore")
		if _, err := nativePGDumpDigest(path); err == nil {
			best = version
			break
		}
	}
	if best == 0 {
		return "", fmt.Errorf("pg_restore for PostgreSQL %d or newer is not installed as a root-owned tool", major)
	}
	return filepath.Join("/usr/lib/postgresql", strconv.Itoa(best), "bin/pg_restore"), nil
}

// moveBackupDownload streams one finished backup with its recorded digest.
func (m *Manager) moveBackupDownload(w http.ResponseWriter, r *http.Request) {
	id, key := r.PathValue("id"), r.PathValue("backup")
	m.mu.Lock()
	d, ok := m.data.Databases[id]
	sqlite := m.data.NativeDatabases[id].Policy.SQLitePath != ""
	var record BackupRecord
	for _, b := range d.Backups {
		if b.ID == key {
			record = b
		}
	}
	m.mu.Unlock()
	if !ok || record.ID == "" || filepath.Base(key) != key || strings.Contains(key, "..") {
		Fail(w, 404, errors.New("unknown backup"))
		return
	}
	if sqlite || !moveSHA256Pattern.MatchString(record.SHA256) || record.Size <= 0 {
		Fail(w, 409, errors.New("this backup cannot be transferred"))
		return
	}
	f, err := os.Open(filepath.Join(m.backupDir(id), key+".dump"))
	if err != nil {
		Fail(w, 404, errors.New("backup file unavailable"))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != record.Size {
		Fail(w, 409, errors.New("backup file does not match its record"))
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Hour))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(record.Size, 10))
	w.Header().Set(MoveSHA256Header, record.SHA256)
	w.WriteHeader(200)
	_, _ = io.Copy(w, io.LimitReader(f, record.Size))
}

// moveRestoreRoute stores the upload, verifies it and restores it.
func (m *Manager) moveRestoreRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	expected := strings.ToLower(r.Header.Get(MoveSHA256Header))
	if !moveSHA256Pattern.MatchString(expected) {
		Fail(w, 400, errors.New("the upload needs its SHA-256 digest"))
		return
	}
	size := r.ContentLength
	if size <= 0 {
		Fail(w, http.StatusLengthRequired, errors.New("the upload needs its exact length"))
		return
	}
	if size > moveMaxDumpSize {
		Fail(w, http.StatusRequestEntityTooLarge, errors.New("the dump exceeds the transfer limit"))
		return
	}
	replace := r.URL.Query().Get("replace") == "1"
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(3 * time.Hour))
	_ = controller.SetWriteDeadline(time.Now().Add(3*time.Hour + time.Minute))
	target, err := m.beginMoveRestore(id)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	var result error
	defer func() { m.endMoveRestore(id, result) }()
	fail := func(code int, err error) {
		result = err
		Fail(w, code, err)
	}
	dir := m.backupDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail(500, errors.New("cannot prepare the upload directory"))
		return
	}
	if disk, err := diskHealth(dir); err != nil || disk.Available < uint64(size)+(256<<20) {
		fail(http.StatusInsufficientStorage, errors.New("not enough free disk space for the dump"))
		return
	}
	file, err := os.CreateTemp(dir, ".move-*")
	if err != nil {
		fail(500, errors.New("cannot store the upload"))
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(r.Body, size+1))
	if err != nil || n != size {
		fail(400, errors.New("the upload was incomplete; nothing was restored"))
		return
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		fail(http.StatusUnprocessableEntity, errors.New("the dump does not match its SHA-256 digest; nothing was restored"))
		return
	}
	if err := file.Sync(); err != nil {
		fail(500, errors.New("the upload could not be stored"))
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		fail(500, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Hour)
	defer cancel()
	if !replace {
		count, err := moveTableCount(ctx, m, id)
		if err != nil {
			fail(409, err)
			return
		}
		if count > 0 {
			fail(409, errors.New("the destination database already has tables; restore with replace"))
			return
		}
	}
	if err := moveRestoreDump(ctx, m, target, file, replace); err != nil {
		fail(409, err)
		return
	}
	JSON(w, 200, map[string]any{"status": "restored", "bytes": size, "sha256": expected})
}

// beginMoveRestore reserves the database like any database operation, so
// backups and other restores wait.
func (m *Manager) beginMoveRestore(id string) (moveDatabaseTarget, error) {
	if m.upgradePending() {
		return moveDatabaseTarget{}, errors.New("agent upgrade in progress")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	target, err := m.moveDatabaseTargetLocked(id)
	if err != nil {
		return target, err
	}
	d := target.Database
	if !d.Ready {
		return target, errors.New("the destination database is not ready")
	}
	if d.Operation != "" {
		return target, errors.New("a database operation is already running")
	}
	for _, ref := range m.attachmentsLocked(id) {
		if m.busy(ref.AppID) || m.nativeOperations[ref.AppID] {
			return target, errors.New("wait for the application's active deployment")
		}
	}
	if target.Host != nil {
		for appID, record := range m.data.NativeHostData {
			if record.Postgres != nil && record.Postgres.Resource == id && m.nativeOperations[appID] {
				return target, errors.New("wait for the application's active deployment")
			}
		}
	}
	old := d
	d.Operation = "restore"
	d.Error = ""
	m.data.Databases[id] = d
	if err := m.save(); err != nil {
		m.data.Databases[id] = old
		return target, err
	}
	target.Database = d
	return target, nil
}

func (m *Manager) endMoveRestore(id string, result error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data.Databases[id]
	if !ok {
		return
	}
	d.Operation = ""
	d.Event = newID()
	if result != nil {
		d.Error = "Move restore failed: " + result.Error()
	}
	m.data.Databases[id] = d
	_ = m.save()
}
