package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/agim/lidza/packs/storage"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const maxBackupSize int64 = 5 << 30 // S3 single PUT limit; never truncate a successful backup.
type backupWriter struct {
	w io.Writer
	n int64
}

func (w *backupWriter) Write(p []byte) (int, error) {
	if w.n+int64(len(p)) > maxBackupSize {
		return 0, errors.New("backup exceeds 5 GiB single-object limit")
	}
	n, e := w.w.Write(p)
	w.n += int64(n)
	return n, e
}
func (m *Manager) dumpDatabase(ctx context.Context, d Database, target *storage.Config) (BackupRecord, error) {
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
	args = append(args, databaseImage, "pg_dump", "--format=custom", "--no-owner", "--no-acl")
	if err = dockerStream(ctx, nil, writer, environment, args...); err != nil {
		return record, err
	}
	if writer.n == 0 {
		return record, errors.New("empty backup rejected")
	}
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
	record = BackupRecord{ID: id, Created: time.Now().UTC(), Size: writer.n, SHA256: hex.EncodeToString(hash.Sum(nil))}
	if d.Backup.Offsite {
		if target == nil {
			return record, errors.New("local backup saved; S3 destination is not configured")
		}
		key := "databases/" + d.AppID + "/" + id + ".dump"
		if err = uploadBackup(ctx, *target, path, key, writer.n); err != nil {
			return record, errors.New("local backup saved; off-site upload failed")
		}
		record.Offsite = true
		record.ObjectKey = key
	}
	return record, nil
}

// Līdza supplies S3 signing; the existing presigned PUT API lets us stream a
// file without buffering database contents in the control panel or agent RAM.
func uploadBackup(ctx context.Context, cfg storage.Config, path, key string, size int64) error {
	store, err := storage.New(cfg)
	if err != nil {
		return err
	}
	signed, err := store.PresignPut(ctx, key, 2*time.Hour, "application/octet-stream")
	if err != nil {
		return errors.New("could not authorize upload")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	req, err := http.NewRequestWithContext(ctx, "PUT", signed, file)
	if err != nil {
		return errors.New("invalid upload destination")
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	client := &http.Client{Timeout: 2 * time.Hour, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("upload failed")
	}
	res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return errors.New("storage rejected upload")
	}
	object, err := store.Stat(ctx, key)
	if err != nil || object.Size != size {
		return errors.New("uploaded object size verification failed")
	}
	return nil
}
func (m *Manager) pruneBackupsLocked(id string) {
	d := m.data.Databases[id]
	if len(d.Backups) <= d.Backup.Keep {
		return
	}
	old := d
	remove := append([]BackupRecord{}, d.Backups[:len(d.Backups)-d.Backup.Keep]...)
	keep := append([]BackupRecord{}, d.Backups[len(d.Backups)-d.Backup.Keep:]...)
	var failed []BackupRecord
	for _, b := range remove {
		if err := os.Remove(filepath.Join(m.backupDir(id), b.ID+".dump")); err != nil && !os.IsNotExist(err) {
			failed = append(failed, b)
		}
	}
	d.Backups = append(failed, keep...)
	if len(failed) > 0 {
		d.Error = "backup saved; could not prune old local files; check permissions and disk space"
	}
	m.data.Databases[id] = d
	if err := m.save(); err != nil {
		m.data.Databases[id] = old
	}
	// Off-site retention is managed by the bucket's lifecycle policy, independently.
}
