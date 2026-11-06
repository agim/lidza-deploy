package agent

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Stale move transfer files. Every transfer removes its temporary files when
// it ends, but an agent killed in the middle of one (a restart, a crash, the
// out-of-memory killer) leaves them behind: a partial export spool can hold
// as much as the database it was exporting. The agent sweeps them on startup
// and with the hourly cleanup:
//
//   - temporary files and directories (.export-*, .clickhouse-export-*,
//     .upload-*, .archive-*, .incoming-*, .move-*, .backup-*) that no
//     running transfer can own: they were last written before this agent
//     process started, and the application's transfer lock is free;
//   - finished ClickHouse exports nothing downloaded for moveExportKeep;
//   - file-set spools of moves that ended long ago (moveFileSpoolKeep).
//
// Only the agent's own move and backup directories are read; application
// files are never touched.

// moveExportKeep is how long a finished ClickHouse export waits for its
// download: longer than any move keeps its source agent waiting.
const moveExportKeep = 48 * time.Hour

// moveTempPrefixes are the temporary names transfers create in the agent's
// move and backup directories.
var moveTempPrefixes = []string{".export-", ".clickhouse-export-", ".upload-", ".archive-", ".incoming-", ".move-", ".backup-"}

func moveTempName(name string) bool {
	for _, prefix := range moveTempPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// moveSweepResult is what one sweep removed.
type moveSweepResult struct {
	Removed []string // paths relative to the data directory
	Sizes   []int64  // bytes of each removed path
	Bytes   int64
}

func (r *moveSweepResult) message() string {
	if len(r.Removed) == 0 {
		return ""
	}
	shown := r.Removed
	if len(shown) > 10 {
		shown = append(append([]string{}, shown[:10]...), fmt.Sprintf("and %d more", len(r.Removed)-10))
	}
	return fmt.Sprintf("Removed %d stale move transfer file(s), freeing %s: %s", len(r.Removed), bytesText(r.Bytes), strings.Join(shown, ", "))
}

// treeFacts are the size and the newest modification time of a path and,
// for a directory, everything below it.
func treeFacts(path string) (int64, time.Time) {
	var size int64
	var newest time.Time
	_ = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return size, newest
}

// sweepMoveTemps removes stale transfer files under the data directory; a
// dry run only lists them.
func (m *Manager) sweepMoveTemps(now time.Time, dryRun bool) moveSweepResult {
	var result moveSweepResult
	remove := func(path string) {
		size, _ := treeFacts(path)
		if !dryRun {
			if err := os.RemoveAll(path); err != nil {
				log.Printf("stale move file %s could not be removed: %v", path, err)
				return
			}
		}
		rel, err := filepath.Rel(m.cfg.DataDir, path)
		if err != nil {
			rel = path
		}
		result.Removed = append(result.Removed, rel)
		result.Sizes = append(result.Sizes, size)
		result.Bytes += size
	}
	stale := func(path string) bool {
		_, newest := treeFacts(path)
		return !newest.IsZero() && newest.Before(agentStarted)
	}
	sweepTemps := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if path := filepath.Join(dir, entry.Name()); moveTempName(entry.Name()) && stale(path) {
				remove(path)
			}
		}
	}
	transfers := m.moveTransfers.Load() > 0

	// ClickHouse exports and uploads, per application, under its lock.
	for _, kind := range []string{"exports", "restore"} {
		base := filepath.Join(m.cfg.DataDir, "move-clickhouse", kind)
		apps, _ := os.ReadDir(base)
		for _, app := range apps {
			if !app.IsDir() || !idPattern.MatchString(app.Name()) {
				continue
			}
			lock := moveClickHouseLock(app.Name())
			if !lock.TryLock() {
				continue // an export or restore of this application is running
			}
			dir := filepath.Join(base, app.Name())
			sweepTemps(dir)
			if kind == "exports" && !transfers {
				m.sweepFinishedExports(dir, now, remove)
			}
			if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 && !dryRun {
				_ = os.Remove(dir)
			}
			lock.Unlock()
		}
	}

	// File-set spools are per move; any running file transfer may own one.
	m.mu.Lock()
	fileTransfers := len(m.moveFileOps) > 0
	databases := map[string]bool{}
	for id, d := range m.data.Databases {
		databases[id] = d.Operation != ""
	}
	m.mu.Unlock()
	if !transfers && !fileTransfers {
		base := filepath.Join(m.cfg.DataDir, moveFileSpoolDir)
		moves, _ := os.ReadDir(base)
		for _, move := range moves {
			if !move.IsDir() {
				continue
			}
			path := filepath.Join(base, move.Name())
			if info, err := move.Info(); err == nil && now.Sub(info.ModTime()) > moveFileSpoolKeep {
				remove(path)
				continue
			}
			sweepTemps(path)
		}
	}

	// Received dumps and interrupted backups in each database's directory.
	if !transfers {
		base := filepath.Join(m.cfg.DataDir, "backups")
		dirs, _ := os.ReadDir(base)
		for _, dir := range dirs {
			if !dir.IsDir() || databases[dir.Name()] {
				continue
			}
			sweepTemps(filepath.Join(base, dir.Name()))
		}
	}
	return result
}

// sweepFinishedExports removes ClickHouse exports older than moveExportKeep,
// and archives whose record was never written.
func (m *Manager) sweepFinishedExports(dir string, now time.Time, remove func(string)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		id, ok := strings.CutSuffix(name, ".tar")
		if !ok || !moveClickHouseExportID.MatchString(id) {
			continue
		}
		archive := filepath.Join(dir, name)
		record := filepath.Join(dir, id+".json")
		info, err := os.Stat(record)
		switch {
		case err == nil && now.Sub(info.ModTime()) > moveExportKeep:
			remove(archive)
			remove(record)
		case os.IsNotExist(err) && stalePath(archive):
			remove(archive)
		}
	}
}

func stalePath(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.ModTime().Before(agentStarted)
}

// sweepMoveTempsNow runs a sweep and logs what it freed.
func (m *Manager) sweepMoveTempsNow(now time.Time) string {
	result := m.sweepMoveTemps(now, false)
	message := result.message()
	if message != "" {
		log.Print(message)
	}
	return message
}
