package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Moving an application's ClickHouse database. Both runtimes are handled:
// the managed container service of a container application (database
// "telemetry") and the host server's app_<id> database of a native
// application. The control panel drives the copy:
//
//   - GET    /v1/move/apps/{id}/clickhouse          the destination's database and version
//     (?counts=1 its row counts, ?since=<RFC 3339> the parts added since then)
//   - POST   /v1/move/apps/{id}/clickhouse/prepare  provision a container destination's service
//   - POST   /v1/move/apps/{id}/clickhouse/exports  export an archive on the source
//     (?format=directory|backup|native, &compress=zstd, &keep=1 keeps the
//     directory backup as a base, &base=<name> exports an incremental
//     backup against it; move_clickhouse_incremental.go)
//   - GET    /v1/move/apps/{id}/clickhouse/exports/{export} download it with its SHA-256
//   - DELETE /v1/move/apps/{id}/clickhouse/exports  remove the exports (&base=1 the kept base too)
//   - POST   /v1/move/apps/{id}/clickhouse/restore  verify and restore an uploaded archive
//     (&keep=1 keeps the extracted backup as a base, &base=<name> restores
//     an incremental archive against it)
//
// Archives are described in clickhouse_archive.go. The destination stores
// the upload, verifies its length and SHA-256 before touching the database,
// restores it under its own database name and verifies the row counts.

// MoveSourceClickHouse is a moving application's ClickHouse database.
type MoveSourceClickHouse struct {
	Mode            string   `json:"mode"` // container or host
	Database        string   `json:"database"`
	Version         string   `json:"version,omitempty"`
	Tables          int      `json:"tables"` // -1 when unknown
	Rows            uint64   `json:"rows"`
	SizeBytes       int64    `json:"size_bytes"`
	EnvironmentKeys []string `json:"environment_keys"`
	Copyable        bool     `json:"copyable"`
	Reason          string   `json:"reason,omitempty"`
}

// MoveClickHouseTarget is the destination's database for the application.
type MoveClickHouseTarget struct {
	Mode     string `json:"mode"`
	Database string `json:"database"`
	Version  string `json:"version"`
	Ready    bool   `json:"ready"`
	// Counts are the database's current row counts by data table, reported
	// on request (?counts=1, with the export's count_cutoff), so the control
	// panel can confirm that a restore it recorded as finished still holds
	// before it decides not to restore again.
	Counts map[string]uint64 `json:"counts,omitempty"`
	// Delta is what the database gained since a point in time (?since=),
	// the expected size of an incremental backup against a base taken then.
	Delta *MoveClickHouseDelta `json:"delta,omitempty"`
}

// MoveClickHouseExport is one finished archive on the source.
type MoveClickHouseExport struct {
	ID       string                    `json:"id"`
	Format   string                    `json:"format"`
	Database string                    `json:"database"`
	Version  string                    `json:"version"`
	Size     int64                     `json:"size"`
	SHA256   string                    `json:"sha256"`
	Tables   []ClickHouseManifestTable `json:"tables"`
	Created  time.Time                 `json:"created"`
	// Compression is the archive's stream compression (zstd or none).
	Compression string `json:"compression,omitempty"`
	// Files is the number of files of a directory backup.
	Files int64 `json:"files,omitempty"`
	// Seconds is how long the export took.
	Seconds float64 `json:"seconds,omitempty"`
	// Fallback explains why a requested BACKUP export used Native instead.
	Fallback string `json:"fallback,omitempty"`
	// CountCutoff is the manifest's row-count cutoff (ClickHouseManifest):
	// counts compared with Tables must use it.
	CountCutoff time.Time `json:"count_cutoff,omitzero"`
	// Backup is the directory backup the source kept as a base; BaseBackup
	// the base this incremental export was made against.
	Backup     string `json:"backup,omitempty"`
	BaseBackup string `json:"base_backup,omitempty"`
}

// ClickHouseEnvironmentKeys are the connection variables both runtimes give
// an application with a managed ClickHouse database.
var ClickHouseEnvironmentKeys = []string{"CLICKHOUSE_DATABASE", "CLICKHOUSE_PASSWORD", "CLICKHOUSE_URL", "CLICKHOUSE_USER"}

const (
	containerClickHouseDatabase  = "telemetry"
	containerClickHouseBackupDir = "/var/lib/clickhouse/backups/" // the image's default backups.allowed_path
)

// moveClickHouseLocks serializes exports and restores per application.
var moveClickHouseLocks sync.Map

func moveClickHouseLock(id string) *sync.Mutex {
	lock, _ := moveClickHouseLocks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (m *Manager) moveClickHouseDir(id, kind string) string {
	return filepath.Join(m.cfg.DataDir, "move-clickhouse", kind, id)
}

// ---- services ------------------------------------------------------------------------

// clickhouseServiceFor is the application's managed ClickHouse database:
// its container service, or its database on the host server.
func (m *Manager) clickhouseServiceFor(ctx context.Context, id string) (clickhouseService, error) {
	if !idPattern.MatchString(id) {
		return clickhouseService{}, errors.New("invalid application id")
	}
	m.mu.Lock()
	_, container := m.data.Apps[id]
	c, managed := m.data.ClickHouses[id]
	m.mu.Unlock()
	if container {
		if !managed || !c.Ready || c.Network == "" {
			return clickhouseService{}, errors.New("the application has no ready managed ClickHouse service")
		}
		return containerClickHouseService(ctx, c)
	}
	service, _, err := m.hostClickHouseService(id)
	return service, err
}

// containerClickHouseService reaches a managed container on its private
// network address from the host; tests replace it.
var containerClickHouseService = func(ctx context.Context, c ClickHouseResource) (clickhouseService, error) {
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	address, err := clickhouseDockerCommand(probe, c, "inspect ClickHouse address", nil, nil, "inspect", "--format", `{{with index .NetworkSettings.Networks "`+c.Network+`"}}{{.IPAddress}}{{end}}`, c.Network)
	if err != nil {
		return clickhouseService{}, err
	}
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil {
		return clickhouseService{}, errors.New("the managed ClickHouse container has no private address; is it running?")
	}
	return clickhouseService{mode: "container", database: containerClickHouseDatabase,
		client: clickhouseClient{base: "http://" + net.JoinHostPort(ip.String(), "8123"), user: "lidza", password: c.Password},
		files:  containerBackupFiles{container: c.Network}}, nil
}

// containerBackupFiles are BACKUP files inside a managed container, reached
// with docker exec; paths are arguments, never shell text.
type containerBackupFiles struct{ container string }

func (f containerBackupFiles) ref(name string) string { return "File('" + name + "')" }

func (f containerBackupFiles) path(name string) (string, error) {
	if !clickhouseBackupFileName.MatchString(name) || !strings.HasPrefix(f.container, "lidza-clickhouse-") {
		return "", errors.New("invalid ClickHouse backup file")
	}
	return containerClickHouseBackupDir + name, nil
}

func clickhouseDockerExec(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	for _, name := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT"} {
		if value := os.Getenv(name); value != "" {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

type commandReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (r commandReader) Close() error {
	_ = r.ReadCloser.Close()
	return r.cmd.Wait()
}

func (f containerBackupFiles) open(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	path, err := f.path(name)
	if err != nil {
		return nil, 0, err
	}
	out, err := clickhouseDockerExec(ctx, "exec", f.container, "stat", "-c", "%s", path).Output()
	size, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || perr != nil || size <= 0 {
		return nil, 0, errors.New("the ClickHouse backup file is missing in the container")
	}
	cmd := clickhouseDockerExec(ctx, "exec", f.container, "cat", path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, err
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, errors.New("the ClickHouse backup file could not be read from the container")
	}
	return commandReader{ReadCloser: stdout, cmd: cmd}, size, nil
}

func (f containerBackupFiles) create(ctx context.Context, name string, data io.Reader, size int64) error {
	path, err := f.path(name)
	if err != nil {
		return err
	}
	cmd := clickhouseDockerExec(ctx, "exec", "-i", f.container, "sh", "-c", `umask 022; mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path)
	cmd.Stdin = io.LimitReader(data, size)
	var output limitedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return errors.New("the ClickHouse backup could not be copied into the container")
	}
	out, err := clickhouseDockerExec(ctx, "exec", f.container, "stat", "-c", "%s", path).Output()
	if got, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err != nil || perr != nil || got != size {
		return errors.New("the ClickHouse backup copied into the container is incomplete")
	}
	return nil
}

func (f containerBackupFiles) remove(ctx context.Context, name string) error {
	path, err := f.path(name)
	if err != nil {
		return err
	}
	return clickhouseDockerExec(ctx, "exec", f.container, "rm", "-f", path).Run()
}

// ---- inventory -----------------------------------------------------------------------

// measureMoveClickHouse fills in the source database's version and size.
func (m *Manager) measureMoveClickHouse(ctx context.Context, id string, entry *MoveSourceClickHouse) error {
	measure, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	service, err := m.clickhouseServiceFor(measure, id)
	if err != nil {
		return err
	}
	summary, err := service.summary(measure)
	if err != nil {
		return err
	}
	entry.Version, entry.Tables, entry.Rows, entry.SizeBytes = summary.Version, summary.Tables, summary.Rows, summary.SizeBytes
	return nil
}

// ---- routes ------------------------------------------------------------------------

func (m *Manager) moveClickHouseRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/move/apps/{id}/clickhouse", func(w http.ResponseWriter, r *http.Request) {
		target, err := m.moveClickHouseTarget(r.Context(), r.PathValue("id"))
		if err != nil {
			Fail(w, 409, err)
			return
		}
		if r.URL.Query().Get("counts") == "1" {
			var cutoff time.Time
			if raw := r.URL.Query().Get("cutoff"); raw != "" {
				if cutoff, err = time.Parse(time.RFC3339, raw); err != nil {
					Fail(w, 400, errors.New("cutoff must be an RFC 3339 time"))
					return
				}
			}
			if target.Counts, err = m.moveClickHouseCounts(r.Context(), r.PathValue("id"), cutoff); err != nil {
				Fail(w, 409, err)
				return
			}
		}
		if raw := r.URL.Query().Get("since"); raw != "" {
			since, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				Fail(w, 400, errors.New("since must be an RFC 3339 time"))
				return
			}
			delta, err := m.moveClickHouseDelta(r.Context(), r.PathValue("id"), since)
			if err != nil {
				Fail(w, 409, err)
				return
			}
			target.Delta = &delta
		}
		JSON(w, 200, target)
	})
	mux.HandleFunc("POST /v1/move/apps/{id}/clickhouse/prepare", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		m.mu.Lock()
		_, container := m.data.Apps[id]
		m.mu.Unlock()
		if container {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
			defer cancel()
			if err := m.provisionManagedClickHouse(ctx, id); err != nil {
				Fail(w, 409, err)
				return
			}
		}
		target, err := m.moveClickHouseTarget(r.Context(), id)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, target)
	})
	mux.HandleFunc("POST /v1/move/apps/{id}/clickhouse/exports", m.moveTransfer(m.moveClickHouseExportRoute))
	mux.HandleFunc("GET /v1/move/apps/{id}/clickhouse/exports/{export}", m.moveTransfer(m.moveClickHouseDownloadRoute))
	mux.HandleFunc("DELETE /v1/move/apps/{id}/clickhouse/exports", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !idPattern.MatchString(id) {
			Fail(w, 400, errors.New("invalid application id"))
			return
		}
		lock := moveClickHouseLock(id)
		lock.Lock()
		defer lock.Unlock()
		if err := os.RemoveAll(m.moveClickHouseDir(id, "exports")); err != nil {
			Fail(w, 500, errors.New("the exports could not be removed"))
			return
		}
		if r.URL.Query().Get("base") == "1" {
			if _, kept := m.moveClickHouseBase(id); kept {
				service, err := m.clickhouseServiceFor(r.Context(), id)
				if err == nil {
					err = m.removeMoveClickHouseBase(r.Context(), service, id)
				}
				if err != nil {
					Fail(w, 409, fmt.Errorf("the base backup could not be removed: %w", err))
					return
				}
			}
		}
		JSON(w, 200, map[string]string{"status": "removed"})
	})
	mux.HandleFunc("POST /v1/move/apps/{id}/clickhouse/restore", m.moveTransfer(m.moveClickHouseRestoreRoute))
}

func (m *Manager) moveClickHouseTarget(ctx context.Context, id string) (MoveClickHouseTarget, error) {
	service, err := m.clickhouseServiceFor(ctx, id)
	if err != nil {
		return MoveClickHouseTarget{}, err
	}
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	version, err := service.client.version(probe)
	if err != nil {
		return MoveClickHouseTarget{}, fmt.Errorf("ClickHouse is not answering: %w", err)
	}
	return MoveClickHouseTarget{Mode: service.mode, Database: service.database, Version: version, Ready: true}, nil
}

// moveClickHouseCounts counts the rows of the application's database by
// data table, with the export's cutoff for tables with a row TTL.
func (m *Manager) moveClickHouseCounts(ctx context.Context, id string, cutoff time.Time) (map[string]uint64, error) {
	service, err := m.clickhouseServiceFor(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	objects, err := service.objects(ctx, service.database)
	if err != nil {
		return nil, err
	}
	return service.counts(ctx, service.database, objects, cutoff)
}

// moveClickHouseDelta measures the parts the application's database gained
// since a point in time (move_clickhouse_incremental.go).
func (m *Manager) moveClickHouseDelta(ctx context.Context, id string, since time.Time) (MoveClickHouseDelta, error) {
	service, err := m.clickhouseServiceFor(ctx, id)
	if err != nil {
		return MoveClickHouseDelta{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return service.delta(ctx, since)
}

var moveClickHouseExportID = moveSHA256Pattern // exports are named by a random 64-hex id

func (m *Manager) moveClickHouseExportRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	format := r.URL.Query().Get("format")
	if format == "" {
		format = clickhouseFormatBackup
	}
	if format != clickhouseFormatBackup && format != clickhouseFormatDirectory && format != clickhouseFormatNative {
		Fail(w, 400, errors.New("format must be directory, backup or native"))
		return
	}
	compression := r.URL.Query().Get("compress")
	if compression != compressionNone && compression != compressionZstd {
		Fail(w, 400, errors.New("compress must be zstd or empty"))
		return
	}
	if compression == compressionZstd && !zstdAvailable() {
		compression = compressionNone
	}
	opts := clickhouseExportOptions{Keep: r.URL.Query().Get("keep") == "1" && format == clickhouseFormatDirectory, Base: r.URL.Query().Get("base")}
	if opts.Base != "" && (format != clickhouseFormatDirectory || !clickhouseBackupDirName.MatchString(opts.Base)) {
		Fail(w, 400, errors.New("base names a kept directory backup and needs format=directory"))
		return
	}
	if m.upgradePending() {
		Fail(w, 409, errors.New("agent upgrade in progress"))
		return
	}
	service, err := m.clickhouseServiceFor(r.Context(), id)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	lock := moveClickHouseLock(id)
	lock.Lock()
	defer lock.Unlock()
	var base moveClickHouseBaseRecord
	if opts.Base != "" {
		// The control panel falls back to a full copy on this refusal.
		if base, _, err = m.checkMoveClickHouseBase(r.Context(), service, id, opts.Base); err != nil {
			Fail(w, 409, fmt.Errorf("no incremental export: %w", err))
			return
		}
	} else if err := m.removeMoveClickHouseBase(r.Context(), service, id); err != nil {
		// A full export replaces any base kept earlier.
		Fail(w, 409, err)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(6 * time.Hour))
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	if move := r.URL.Query().Get("move"); moveIDPattern.MatchString(move) {
		// The control panel follows the export like a file set's archive.
		var endProgress func()
		ctx, endProgress = m.trackMoveFiles(ctx, move, "clickhouse", "Creating the ClickHouse export")
		defer endProgress()
	}
	dir := m.moveClickHouseDir(id, "exports")
	// One export per application: a new one replaces any earlier attempt.
	if err := os.RemoveAll(dir); err != nil {
		Fail(w, 500, errors.New("earlier exports could not be removed"))
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		Fail(w, 500, errors.New("cannot prepare the export directory"))
		return
	}
	measure, stop := context.WithTimeout(ctx, 15*time.Second)
	summary, err := service.summary(measure)
	stop()
	if err != nil {
		Fail(w, 409, err)
		return
	}
	if opts.Base != "" {
		measure, stop := context.WithTimeout(ctx, 30*time.Second)
		delta, err := service.delta(measure, base.Created)
		stop()
		if err == nil {
			err = clickhouseIncrementalFits(dir, delta)
		}
		if err != nil {
			Fail(w, http.StatusInsufficientStorage, err)
			return
		}
	} else if err := clickhouseExportFits(dir, summary, format); err != nil {
		Fail(w, http.StatusInsufficientStorage, err)
		return
	}
	export, err := m.writeClickHouseExport(ctx, service, format, compression, dir, opts)
	if err != nil && format != clickhouseFormatNative && opts.Base == "" && r.URL.Query().Get("strict") != "1" && ctx.Err() == nil && !errors.Is(err, errClickHouseDiskReserve) {
		// A source whose BACKUP is unavailable still moves table by table,
		// but only when the much larger per-table export fits.
		fallback := err.Error()
		if fit := clickhouseExportFits(dir, summary, clickhouseFormatNative); fit != nil {
			err = fmt.Errorf("%s; the per-table fallback was not tried: %w", fallback, fit)
		} else if export, err = m.writeClickHouseExport(ctx, service, clickhouseFormatNative, compression, dir, clickhouseExportOptions{}); err == nil {
			export.Fallback = boundedErrorText(fallback, 400)
		}
	}
	if err != nil {
		Fail(w, 409, fmt.Errorf("the ClickHouse export failed: %w", err))
		return
	}
	raw, _ := json.Marshal(export)
	if err := os.WriteFile(filepath.Join(dir, export.ID+".json"), raw, 0o600); err != nil {
		Fail(w, 500, errors.New("the export record could not be stored"))
		return
	}
	if export.Backup != "" {
		if err := m.writeMoveClickHouseBase(id, moveClickHouseBaseRecord{Name: export.Backup, Database: export.Database, Created: export.Created}); err != nil {
			Fail(w, 500, err)
			return
		}
	}
	JSON(w, 200, export)
}

// errClickHouseDiskReserve stops an export before it fills the disk
// (disk_reserve.go).
var errClickHouseDiskReserve = errDiskReserve

// clickhouseExportFits checks before an export starts. A BACKUP archive is
// compressed (the BACKUP file plus its archive copy); a per-table export is
// written uncompressed, and Native output can exceed the parts' uncompressed
// size, so it reserves twice that.
func clickhouseExportFits(dir string, summary ClickHouseSummary, format string) error {
	need := uint64(2 * max(summary.SizeBytes, 0))
	if format == clickhouseFormatNative {
		need = uint64(2 * max(summary.UncompressedBytes, summary.SizeBytes, 0))
	}
	return diskFits(dir, need, "the ClickHouse export")
}

func moveHumanBytes(n uint64) string {
	const unit = 1 << 30
	if n >= unit {
		return fmt.Sprintf("%.1f GiB", float64(n)/unit)
	}
	return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
}

// writeClickHouseExport writes one archive into dir as <id>.tar, compressed
// with compression; the size and digest are those of the stored file.
func (m *Manager) writeClickHouseExport(ctx context.Context, service clickhouseService, format, compression, dir string, opts clickhouseExportOptions) (MoveClickHouseExport, error) {
	started := time.Now()
	file, err := os.CreateTemp(dir, ".export-*")
	if err != nil {
		return MoveClickHouseExport{}, errors.New("cannot store the export")
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	counter := &progressWriter{}
	progressFrom(ctx).Stage("Exporting the ClickHouse database", 0, counter.n.Load)
	compressing, cancel := context.WithCancel(ctx)
	defer cancel()
	output, err := compressedWriter(compressing, io.MultiWriter(&reserveWriter{dir: dir}, file, hash, counter), compression)
	if err != nil {
		return MoveClickHouseExport{}, err
	}
	manifest, err := exportClickHouseArchiveWith(ctx, service, format, dir, output, opts)
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return MoveClickHouseExport{}, err
	}
	if err := file.Sync(); err != nil {
		return MoveClickHouseExport{}, errors.New("the export could not be stored")
	}
	export := MoveClickHouseExport{ID: nativeRandomSecret(), Format: manifest.Format, Database: manifest.Database, Version: manifest.Version, Size: counter.n.Load(), SHA256: hex.EncodeToString(hash.Sum(nil)), Created: manifest.Created,
		Compression: compression, Files: manifest.Files, Seconds: time.Since(started).Seconds(), CountCutoff: manifest.CountCutoff, Backup: manifest.Backup, BaseBackup: manifest.BaseBackup}
	for _, table := range manifest.Tables {
		table.Create, table.File = "", ""
		export.Tables = append(export.Tables, table)
	}
	if export.Tables == nil {
		export.Tables = []ClickHouseManifestTable{}
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, export.ID+".tar")); err != nil {
		return MoveClickHouseExport{}, errors.New("the export could not be finalized")
	}
	return export, nil
}

func (m *Manager) moveClickHouseDownloadRoute(w http.ResponseWriter, r *http.Request) {
	id, key := r.PathValue("id"), r.PathValue("export")
	if !idPattern.MatchString(id) || !moveClickHouseExportID.MatchString(key) {
		Fail(w, 404, errors.New("unknown export"))
		return
	}
	dir := m.moveClickHouseDir(id, "exports")
	raw, err := os.ReadFile(filepath.Join(dir, key+".json"))
	var export MoveClickHouseExport
	if err != nil || json.Unmarshal(raw, &export) != nil || export.ID != key {
		Fail(w, 404, errors.New("unknown export"))
		return
	}
	f, err := os.Open(filepath.Join(dir, key+".tar"))
	if err != nil {
		Fail(w, 404, errors.New("export file unavailable"))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != export.Size {
		Fail(w, 409, errors.New("the export file does not match its record"))
		return
	}
	serveMoveArtifact(w, r, f, export.Size, export.SHA256, "application/x-tar", 6*time.Hour)
}

// moveClickHouseRestoreRoute stores the upload, verifies it and restores it
// into the application's own database on this agent.
func (m *Manager) moveClickHouseRestoreRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if m.upgradePending() {
		Fail(w, 409, errors.New("agent upgrade in progress"))
		return
	}
	m.mu.Lock()
	busy := m.busy(id) || m.nativeOperations[id]
	m.mu.Unlock()
	if busy {
		Fail(w, 409, errors.New("wait for the application's active deployment"))
		return
	}
	service, err := m.clickhouseServiceFor(r.Context(), id)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	opts := clickhouseRestoreOptions{Keep: r.URL.Query().Get("keep") == "1", Base: r.URL.Query().Get("base")}
	if opts.Base != "" && !clickhouseBackupDirName.MatchString(opts.Base) {
		Fail(w, 400, errors.New("base names a kept directory backup"))
		return
	}
	lock := moveClickHouseLock(id)
	lock.Lock()
	defer lock.Unlock()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(6 * time.Hour))
	_ = controller.SetWriteDeadline(time.Now().Add(6*time.Hour + time.Minute))
	dir := m.moveClickHouseDir(id, "restore")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		Fail(w, 500, errors.New("cannot prepare the upload directory"))
		return
	}
	// An incremental archive needs its base here, checked before the upload
	// is received: the control panel falls back to a full copy on this
	// refusal. The restored data is about the base's size plus the delta.
	var baseBytes int64
	if opts.Base != "" {
		var err error
		if _, baseBytes, err = m.checkMoveClickHouseBase(r.Context(), service, id, opts.Base); err != nil {
			Fail(w, 409, fmt.Errorf("no incremental restore: %w", err))
			return
		}
	}
	// Restoring needs room for the extracted BACKUP and the restored data
	// beside the received (possibly compressed) archive.
	if size, _ := strconv.ParseInt(r.Header.Get(MoveSizeHeader), 10, 64); size > 0 || r.ContentLength > 0 {
		if err := diskFits(dir, 3*uint64(max(size, r.ContentLength))+uint64(max(baseBytes, 0)), "restoring the ClickHouse archive"); err != nil {
			Fail(w, http.StatusInsufficientStorage, err)
			return
		}
	}
	staged, err := m.receiveMoveUpload(r, dir, moveMaxDumpSize, "ClickHouse archive", nil)
	if err != nil {
		failMoveUpload(w, err)
		return
	}
	restored := false
	defer func() { staged.Done(restored) }()
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	if opts.Keep && opts.Base == "" {
		// A new base replaces any kept earlier.
		if err := m.removeMoveClickHouseBase(ctx, service, id); err != nil {
			Fail(w, 409, err)
			return
		}
	}
	result, err := restoreClickHouseArchiveWith(ctx, service, staged.File, r.URL.Query().Get("replace") == "1", opts)
	if err != nil {
		Fail(w, 409, fmt.Errorf("the ClickHouse restore failed: %w", err))
		return
	}
	restored = true
	if result.Backup != "" {
		if err := m.writeMoveClickHouseBase(id, moveClickHouseBaseRecord{Name: result.Backup, Database: service.database, Created: time.Now().UTC()}); err != nil {
			Fail(w, 500, err)
			return
		}
	}
	result.Bytes, result.SHA256 = staged.Size, staged.SHA256
	w.Header().Set(MoveVerifiedHeader, staged.SHA256)
	JSON(w, 200, result)
}

// ---- Data & Storage: sizes and in-place restore -------------------------------------

// measuredDatabaseViews adds the size of each live ClickHouse database.
func (m *Manager) measuredDatabaseViews(ctx context.Context) []DatabaseView {
	views := m.databaseViews()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for i := range views {
		if v := &views[i]; v.Engine == "clickhouse" && v.Ready && !v.Retained && v.Operation == "" {
			if n, err := m.hostClickHouseSize(ctx, v.AppID); err == nil {
				v.SizeBytes = n
			}
		}
	}
	return views
}

// clickhouseRestoreBackupRoute restores one local backup of an
// agent-provisioned ClickHouse database into that database, replacing its
// contents (POST /v1/databases/{id}/backups/{backup}/restore).
func (m *Manager) clickhouseRestoreBackupRoute(w http.ResponseWriter, r *http.Request) {
	resource, key := r.PathValue("id"), r.PathValue("backup")
	if m.upgradePending() {
		Fail(w, 409, errors.New("agent upgrade in progress"))
		return
	}
	app, ok := m.clickhouseResourceApp(resource)
	if !ok {
		Fail(w, 409, errors.New("in-place restores are available for ClickHouse databases; restore PostgreSQL backups into a new database"))
		return
	}
	m.mu.Lock()
	d, exists := m.data.Databases[resource]
	var record BackupRecord
	for _, b := range d.Backups {
		if b.ID == key {
			record = b
		}
	}
	switch {
	case !exists || record.ID == "" || filepath.Base(key) != key || !moveSHA256Pattern.MatchString(record.SHA256):
		m.mu.Unlock()
		Fail(w, 404, errors.New("unknown backup"))
		return
	case !d.Ready || d.Operation != "" || m.nativeOperations[app]:
		m.mu.Unlock()
		Fail(w, 409, errors.New("the database or its application is busy"))
		return
	}
	old := d
	d.Operation, d.Error = "restore", ""
	m.data.Databases[resource] = d
	if err := m.save(); err != nil {
		m.data.Databases[resource] = old
		m.mu.Unlock()
		Fail(w, 500, err)
		return
	}
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 6*time.Hour)
		defer cancel()
		ctx, endProgress := m.trackDatabase(ctx, resource, "Waiting for an available database operation slot")
		defer endProgress()
		var err error
		select {
		case m.databaseSlots <- struct{}{}:
			err = m.restoreClickHouseBackup(ctx, app, resource, record)
			<-m.databaseSlots
		case <-ctx.Done():
			err = ctx.Err()
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		latest := m.data.Databases[resource]
		latest.Operation, latest.Event = "", newID()
		latest.Error = ""
		if err != nil {
			latest.Error = "ClickHouse restore failed: " + err.Error()
		}
		m.data.Databases[resource] = latest
		_ = m.save()
	}()
	JSON(w, 202, map[string]string{"status": "restoring"})
}

func (m *Manager) restoreClickHouseBackup(ctx context.Context, app, resource string, record BackupRecord) error {
	progress := progressFrom(ctx)
	file, err := os.Open(filepath.Join(m.backupDir(resource), record.ID+".dump"))
	if err != nil {
		return errors.New("local backup unavailable")
	}
	defer file.Close()
	hash := sha256.New()
	verified := &progressReader{r: file}
	progress.Stage("Verifying the backup's SHA-256", record.Size, verified.n.Load)
	if n, err := io.Copy(hash, verified); err != nil || n != record.Size || hex.EncodeToString(hash.Sum(nil)) != record.SHA256 {
		return errors.New("backup integrity check failed; the database was left unchanged")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	service, _, err := m.hostClickHouseService(app)
	if err != nil {
		return err
	}
	progress.Stage("Restoring the ClickHouse database", 0, nil)
	_, err = restoreClickHouseArchive(ctx, service, file, true)
	return err
}
