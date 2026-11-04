package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/agim/lidza/packs/storage"
)

func (m *Manager) backupDiskLimit(dir, database string) (int64, error) {
	limit := m.cfg.BackupMaxBytes[database]
	if limit <= maxBackupSize {
		return maxBackupSize, nil
	}
	disk, err := diskHealth(dir)
	// Database operations share a bounded pool. Reserve space for every slot
	// at this limit plus host headroom before starting an extended archive.
	slots := max(cap(m.databaseSlots), 1)
	if err != nil || disk.Available < uint64(limit)*uint64(slots)+(1<<30) {
		return 0, errors.New("available disk space is too low for the configured extended backup limit")
	}
	return limit, nil
}

func uploadMultipartBackup(ctx context.Context, cfg storage.Config, path, key string, size int64) error {
	cfg.MaxSize = size
	cfg.Timeout = 2 * time.Hour
	store, err := storage.New(cfg)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	counted := &backupUploadReader{ReadCloser: file}
	stopUpload := backupProgress(ctx, fmt.Sprintf("Uploading required multipart S3 copy (%.1f MiB total)", float64(size)/(1<<20)), counted.n.Load)
	_, err = store.PutMultipart(ctx, key, counted, size, storage.PutOptions{ContentType: "application/octet-stream"})
	stopUpload()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("multipart S3 upload interrupted: %w", ctx.Err())
		}
		return errors.New("multipart S3 upload failed")
	}
	return verifyBackupObjectSize(ctx, store, key, size)
}
