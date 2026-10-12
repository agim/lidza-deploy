package agent

import (
	"context"
	"errors"
	"path/filepath"
	"time"
)

func (m *Manager) retryBackupUpload(id, backup string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data.Databases[id]
	if !ok {
		return errors.New("database not found")
	}
	if d.Operation != "" {
		return errors.New("wait for the active database operation")
	}
	if m.data.BackupStorage == nil {
		return errors.New("configure off-site storage first")
	}
	var record BackupRecord
	for _, b := range d.Backups {
		if b.ID == backup {
			record = b
			break
		}
	}
	if record.ID == "" {
		return errors.New("retained local backup not found")
	}
	if record.Offsite {
		return errors.New("backup already has an off-site copy")
	}
	cfg := *m.data.BackupStorage
	old := d
	d.Operation = "upload"
	d.Error = ""
	m.data.Databases[id] = d
	if err := m.save(); err != nil {
		m.data.Databases[id] = old
		return err
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 2*time.Hour)
		defer cancel()
		var err error
		select {
		case m.databaseSlots <- struct{}{}:
			defer func() { <-m.databaseSlots }()
		case <-ctx.Done():
			err = ctx.Err()
		}
		key := backupObjectKey(id, record)
		if err == nil {
			err = uploadBackup(ctx, cfg, filepath.Join(m.backupDir(id), record.ID+".dump"), key, record.Size)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		latest := m.data.Databases[id]
		latest.Operation = ""
		latest.Event = newID()
		for i, b := range latest.Backups {
			if b.ID == record.ID {
				latest.Backups[i].OffsiteRequested = true
				if err != nil {
					latest.Backups[i].UploadError = "Off-site upload failed: " + err.Error()
				} else {
					latest.Backups[i].Offsite = true
					latest.Backups[i].ObjectKey = key
					latest.Backups[i].UploadError = ""
				}
				break
			}
		}
		if err != nil {
			latest.Error = "Local backup retained; off-site upload failed: " + err.Error()
		} else {
			latest.Error = ""
		}
		m.data.Databases[id] = latest
		_ = m.save()
	}()
	return nil
}
