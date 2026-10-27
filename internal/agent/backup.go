package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/agim/lidza/packs/storage"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const maxBackupSize int64 = 5 << 30 // S3 single PUT limit; never truncate a successful backup.
type backupWriter struct {
	w io.Writer
	n atomic.Int64
}

func (w *backupWriter) Write(p []byte) (int, error) {
	if w.n.Load()+int64(len(p)) > maxBackupSize {
		return 0, errors.New("backup exceeds 5 GiB single-object limit")
	}
	n, e := w.w.Write(p)
	w.n.Add(int64(n))
	return n, e
}
func (m *Manager) dumpDatabase(ctx context.Context, d Database, target *storage.Config, kind ...string) (BackupRecord, error) {
	var record BackupRecord
	dir := m.backupDir(d.AppID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return record, errors.New("cannot create backup directory")
	}
	file, err := os.CreateTemp(dir, ".backup-*")
	if err != nil {
		return record, errors.New("cannot create backup file")
	}
	defer os.Remove(file.Name())
	defer file.Close()
	id := newID()
	container := "lidza-backup-" + id
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = command(cleanup, "", nil, "docker", "rm", "-f", container)
	}()
	hash := sha256.New()
	writer := &backupWriter{w: io.MultiWriter(file, hash)}
	args := []string{"run", "--rm", "--name", container, "--label", "io.lidza.backup=" + d.AppID, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "512m", "--pids-limit", "128"}
	if d.Network != "" {
		args = append(args, "--network", d.Network)
	}
	environment, flags := postgresEnvironment(d.URL)
	args = append(args, flags...)
	// Full telemetry dumps spend substantial CPU compressing repeated JSON.
	// Keep the complete custom archive, but avoid pg_dump's moderate gzip default.
	// PostgreSQL 17's matching pg_restore reads Zstandard without an external tool.
	args = append(args, databaseImage, "pg_dump", "--format=custom", "--compress=zstd:1", "--no-owner", "--no-acl", "--lock-wait-timeout=30s")
	for _, table := range m.cfg.BackupExcludeTableData[d.AppID] {
		args = append(args, "--exclude-table-data="+table)
		commandDiagnostic(ctx, "backup", "Omitting disposable table data by agent policy: "+table+" (schema retained).")
	}
	stopDump := backupProgress(ctx, "Dumping PostgreSQL to local disk (database lock wait limited to 30 seconds)", writer.n.Load)
	err = dockerStream(ctx, nil, writer, environment, args...)
	stopDump()
	if err != nil {
		if ctx.Err() != nil {
			return record, fmt.Errorf("PostgreSQL dump interrupted: %w", ctx.Err())
		}
		return record, err
	}
	if writer.n.Load() == 0 {
		return record, errors.New("empty backup rejected")
	}
	stopSync := backupProgress(ctx, "Finalizing local backup on disk", writer.n.Load)
	defer stopSync()
	if err = file.Sync(); err != nil {
		return record, errors.New("backup could not be synced")
	}
	if err = file.Close(); err != nil {
		return record, errors.New("backup could not be closed")
	}
	path := filepath.Join(dir, id+".dump")
	if err = os.Rename(file.Name(), path); err != nil {
		return record, errors.New("backup could not be finalized")
	}
	parent, err := os.Open(dir)
	if err != nil {
		return record, err
	}
	err = parent.Sync()
	parent.Close()
	if err != nil {
		return record, errors.New("backup directory sync failed")
	}
	record = BackupRecord{ID: id, Created: time.Now().UTC(), Size: writer.n.Load(), SHA256: hex.EncodeToString(hash.Sum(nil)), ExcludedTableData: append([]string(nil), m.cfg.BackupExcludeTableData[d.AppID]...)}
	// Do not carry the disk-stage reporter into the upload stage.
	stopSync()
	commandDiagnostic(ctx, "backup", fmt.Sprintf("Local backup finalized: %.1f MiB; SHA-256 recorded.", float64(record.Size)/(1<<20)))
	if err := ctx.Err(); err != nil {
		return record, fmt.Errorf("local backup finalization interrupted: %w", err)
	}
	if len(kind) > 0 {
		record.Kind = kind[0]
	}
	if d.Backup.Offsite {
		record.OffsiteRequested = true
		if target == nil {
			record.UploadError = "S3 destination is not configured"
			return record, errors.New("local backup saved; S3 destination is not configured")
		}
		key := backupObjectKey(d.AppID, record)
		if err = uploadBackup(ctx, *target, path, key, record.Size); err != nil {
			record.UploadError = err.Error()
			return record, fmt.Errorf("local backup saved; off-site upload failed: %w", err)
		}
		record.Offsite = true
		record.ObjectKey = key
	}
	return record, nil
}

// Līdza supplies S3 signing; the existing presigned PUT API lets us stream a
// file without buffering database contents in the control panel or agent RAM.
func uploadBackup(ctx context.Context, cfg storage.Config, path, key string, size int64) error {
	stopAuthorization := backupProgress(ctx, "Authorizing required S3 upload", nil)
	store, err := storage.New(cfg)
	if err != nil {
		stopAuthorization()
		return err
	}
	signed, err := store.PresignPut(ctx, key, 2*time.Hour, "application/octet-stream")
	stopAuthorization()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("S3 upload authorization interrupted: %w", ctx.Err())
		}
		return errors.New("could not authorize upload")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	counted := &backupUploadReader{ReadCloser: file}
	req, err := http.NewRequestWithContext(ctx, "PUT", signed, counted)
	if err != nil {
		return errors.New("invalid upload destination")
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	client := &http.Client{Timeout: 2 * time.Hour, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	stopUpload := backupProgress(ctx, fmt.Sprintf("Uploading required S3 copy (%.1f MiB total)", float64(size)/(1<<20)), counted.n.Load)
	res, err := client.Do(req)
	stopUpload()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("S3 upload interrupted: %w", ctx.Err())
		}
		return errors.New("upload failed")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var response struct {
			Code string `xml:"Code"`
		}
		_ = xml.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&response)
		res.Body.Close()
		if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`).MatchString(response.Code) {
			response.Code = ""
		}
		if response.Code != "" {
			return fmt.Errorf("storage rejected upload (HTTP %d, %s)", res.StatusCode, response.Code)
		}
		return fmt.Errorf("storage rejected upload (HTTP %d)", res.StatusCode)
	}
	res.Body.Close()
	stopVerify := backupProgress(ctx, "Verifying uploaded S3 object size", nil)
	object, err := store.Stat(ctx, key)
	stopVerify()
	if err != nil || object.Size != size {
		if ctx.Err() != nil {
			return fmt.Errorf("S3 object verification interrupted: %w", ctx.Err())
		}
		return errors.New("uploaded object size verification failed")
	}
	return nil
}

type backupUploadReader struct {
	io.ReadCloser
	n atomic.Int64
}

func (r *backupUploadReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.n.Add(int64(n))
	return n, err
}

const predeploymentBackup = "predeployment"

func backupObjectKey(id string, record BackupRecord) string {
	name := record.ID
	if record.Kind == predeploymentBackup {
		name = "predeployment"
	}
	return "databases/" + id + "/" + name + ".dump"
}

func (m *Manager) pruneBackupsLocked(id string) error {
	d := m.data.Databases[id]
	old := d
	// A dump is eligible only after it was finalized and is still a regular,
	// nonempty local file. Never delete a usable older copy just because the
	// newest inventory entry refers to a missing or incomplete file.
	if d.Operation != "" {
		return nil
	}
	keepIDs := make(map[string]bool)
	newest := -1
	for i, b := range d.Backups {
		if b.ID == "" || filepath.Base(b.ID) != b.ID || strings.Contains(b.ID, "..") {
			return errors.New("invalid backup identity; local cleanup refused")
		}
		info, err := os.Lstat(filepath.Join(m.backupDir(id), b.ID+".dump"))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		if newest < 0 || !b.Created.Before(d.Backups[newest].Created) {
			newest = i
		}
	}
	if newest < 0 {
		return nil
	}
	keepIDs[d.Backups[newest].ID] = true
	var retained []BackupRecord
	var pruneErr error
	for _, b := range d.Backups {
		if !keepIDs[b.ID] {
			if err := os.Remove(filepath.Join(m.backupDir(id), b.ID+".dump")); err != nil && !os.IsNotExist(err) {
				pruneErr = errors.New("backup saved; could not prune old local files; check permissions and disk space")
			} else {
				continue
			}
		}
		retained = append(retained, b)
	}
	if len(retained) == len(d.Backups) && pruneErr == nil {
		return nil
	}
	d.Backups = retained
	if pruneErr != nil {
		d.Error = pruneErr.Error()
	}
	m.data.Databases[id] = d
	if err := m.save(); err != nil {
		m.data.Databases[id] = old
		return err
	}
	// Normal off-site history uses bucket lifecycle; predeployment overwrites one key.
	return pruneErr
}
