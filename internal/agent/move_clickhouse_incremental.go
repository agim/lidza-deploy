package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Incremental final sync of a moved ClickHouse database. The bulk copy's
// directory BACKUP is the base: the source keeps its backup directory and
// the destination keeps the extracted copy, both under the same name on
// their backups disk (Disk('backups', 'agent/<name>/') on a host server,
// File('<name>/') inside a managed container). The final sync then runs
//
//	BACKUP DATABASE db TO <name>-final SETTINGS base_backup = <name>
//
// on the frozen source, which writes only what the base does not have, and
// the destination restores it with
//
//	RESTORE DATABASE db FROM <name>-final SETTINGS base_backup = <name>
//
// ClickHouse deduplicates against the base by file content (size and
// checksum, BackupImpl::writeFile), not by name or time: parts that are
// unchanged since the base are referenced from it, parts merged or added
// since are written anew. The RESTORE setting overrides the base location
// recorded inside the incremental backup's metadata (the source's path,
// which differs on the destination) and ClickHouse checks the base's UUID,
// so the extracted copy must be the base's byte-for-byte copy, which the
// tar transfer guarantees. The incremental backup describes the database
// as it is at final-sync time: tables created since the base are in it and
// tables dropped since are not, and the destination's database is replaced
// as a whole, as for a full copy. Verified on the deployed release series
// (26.x); the feature exists since ClickHouse 22.6.
//
// Both agents advertise the capability as "incremental" among their
// ClickHouse formats; the control panel only requests kept bases and
// incremental copies from agents that do. The base backups are removed
// when the move finishes or aborts (DELETE .../clickhouse/exports?base=1
// on both servers), and when a full export replaces them.

// clickhouseFormatIncremental is the capability name in MoveHost.ClickHouseFormats.
const clickhouseFormatIncremental = "incremental"

// clickhouseExportOptions shape a directory export (exportClickHouseArchiveWith).
type clickhouseExportOptions struct {
	// Keep leaves the backup directory on the backups disk after the
	// export, as the base of a later incremental backup.
	Keep bool
	// Base makes an incremental backup against the kept directory Base.
	Base string
}

// clickhouseRestoreOptions shape a directory restore (restoreClickHouseArchiveWith).
type clickhouseRestoreOptions struct {
	// Keep leaves the extracted backup directory on the backups disk under
	// the archive's own name (ClickHouseManifest.Backup).
	Keep bool
	// Base restores an incremental archive against the kept directory Base.
	Base string
}

// clickhouseIncrementalBackupName names the incremental backup made against
// base, so a retried attempt replaces the previous one.
func clickhouseIncrementalBackupName(base string) string {
	name := base + "-final"
	if !clickhouseBackupDirName.MatchString(name) {
		return newClickHouseBackupDirName()
	}
	return name
}

// MoveClickHouseDelta is what the source added since a point in time: the
// active parts newer than Since, measured from system.parts; an incremental
// backup against a base taken at Since is about Bytes large.
type MoveClickHouseDelta struct {
	Since time.Time `json:"since"`
	Bytes int64     `json:"bytes"`
	Parts int64     `json:"parts"`
}

// delta measures the active parts of the database modified after since.
// Parts merged since are counted as new, as an incremental backup stores
// them anew.
func (s clickhouseService) delta(ctx context.Context, since time.Time) (MoveClickHouseDelta, error) {
	out := MoveClickHouseDelta{Since: since.UTC()}
	literal, err := chString(s.database)
	if err != nil {
		return out, err
	}
	type row struct {
		Bytes uint64 `json:"bytes"`
		Parts uint64 `json:"parts"`
	}
	rows, err := clickhouseRows[row](ctx, s.client, "SELECT toUInt64(sum(bytes_on_disk)) AS bytes, toUInt64(count()) AS parts FROM system.parts WHERE database = "+literal+" AND active AND modification_time > toDateTime('"+since.UTC().Format("2006-01-02 15:04:05")+"', 'UTC')")
	if err != nil {
		return out, err
	}
	if len(rows) == 1 {
		out.Bytes = int64(min(rows[0].Bytes, 1<<62))
		out.Parts = int64(min(rows[0].Parts, 1<<62))
	}
	return out, nil
}

// ---- base backup records -------------------------------------------------------------

// moveClickHouseBaseRecord is the base backup an agent keeps for one
// application's move: on the source the BACKUP directory the bulk copy
// streamed, on the destination its extracted copy.
type moveClickHouseBaseRecord struct {
	Name     string    `json:"name"`
	Database string    `json:"database"`
	Created  time.Time `json:"created"`
}

func (m *Manager) moveClickHouseBasePath(id string) string {
	return filepath.Join(m.moveClickHouseDir(id, "base"), "base.json")
}

// moveClickHouseBase returns the application's kept base backup.
func (m *Manager) moveClickHouseBase(id string) (moveClickHouseBaseRecord, bool) {
	var record moveClickHouseBaseRecord
	raw, err := os.ReadFile(m.moveClickHouseBasePath(id))
	if err != nil || json.Unmarshal(raw, &record) != nil || !clickhouseBackupDirName.MatchString(record.Name) {
		return moveClickHouseBaseRecord{}, false
	}
	return record, true
}

func (m *Manager) writeMoveClickHouseBase(id string, record moveClickHouseBaseRecord) error {
	path := m.moveClickHouseBasePath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errors.New("the base backup record could not be stored")
	}
	raw, _ := json.Marshal(record)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return errors.New("the base backup record could not be stored")
	}
	return nil
}

// removeMoveClickHouseBase removes the kept base backup directory and its
// record; a missing directory counts as removed. The record stays when the
// directory could not be removed, so a later cleanup tries again.
func (m *Manager) removeMoveClickHouseBase(ctx context.Context, service clickhouseService, id string) error {
	record, ok := m.moveClickHouseBase(id)
	if !ok {
		return os.RemoveAll(m.moveClickHouseDir(id, "base"))
	}
	remove, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := service.files.removeDir(remove, record.Name); err != nil {
		return fmt.Errorf("the base backup %s could not be removed: %w", record.Name, err)
	}
	return os.RemoveAll(m.moveClickHouseDir(id, "base"))
}

// checkMoveClickHouseBase confirms the kept base backup name is still on
// the backups disk; it returns its size on disk.
func (m *Manager) checkMoveClickHouseBase(ctx context.Context, service clickhouseService, id, name string) (moveClickHouseBaseRecord, int64, error) {
	record, ok := m.moveClickHouseBase(id)
	if !ok || record.Name != name {
		return record, 0, fmt.Errorf("this server no longer keeps the base backup %s", name)
	}
	size, err := service.files.dirSize(ctx, name)
	if err != nil {
		return record, 0, fmt.Errorf("the base backup %s is missing from the backups disk", name)
	}
	return record, size, nil
}

// clickhouseIncrementalFits checks the disk before an incremental export:
// the incremental backup directory and its archive copy.
func clickhouseIncrementalFits(dir string, delta MoveClickHouseDelta) error {
	return diskFits(dir, 2*uint64(max(delta.Bytes, 0)), "the incremental ClickHouse export")
}

// ---- directory sizes -------------------------------------------------------------------

// dirSize is the size on disk of a backup directory on the host server.
func (f hostBackupFiles) dirSize(_ context.Context, name string) (int64, error) {
	dir, err := f.dirPath(name)
	if err != nil {
		return 0, err
	}
	return directorySize(dir)
}

func directorySize(dir string) (int64, error) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return 0, errors.New("the ClickHouse backup directory is missing")
	}
	var total int64
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

// dirSize is the size of a backup directory inside the managed container.
func (f containerBackupFiles) dirSize(ctx context.Context, name string) (int64, error) {
	dir, err := f.dirPath(name)
	if err != nil {
		return 0, err
	}
	out, err := clickhouseDockerExec(ctx, "exec", f.container, "du", "-sb", dir).Output()
	if err != nil {
		return 0, errors.New("the ClickHouse backup directory is missing in the container")
	}
	size, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	n, err := strconv.ParseInt(size, 10, 64)
	if err != nil {
		return 0, errors.New("the ClickHouse backup directory could not be measured")
	}
	return n, nil
}
