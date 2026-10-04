package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"time"

	"github.com/agim/lidza/packs/storage"
)

var postgresOptions = map[string]string{"sslmode": "PGSSLMODE", "application_name": "PGAPPNAME", "options": "PGOPTIONS", "channel_binding": "PGCHANNELBINDING", "connect_timeout": "PGCONNECT_TIMEOUT"}

type BackupPolicy struct {
	Hours   int  `json:"hours"` // 0: manual only; 1, 24, 168: scheduled.
	Keep    int  `json:"keep"`
	Offsite bool `json:"offsite"`
}
type DatabaseRequest struct {
	Mode   string       `json:"mode"` // local, external, none
	URL    string       `json:"url,omitempty"`
	Backup BackupPolicy `json:"backup"`
}
type BackupRecord struct {
	ID        string    `json:"id"`
	Created   time.Time `json:"created"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	Offsite   bool      `json:"offsite"`
	ObjectKey string    `json:"object_key,omitempty"`
}
type Database struct {
	AppID         string         `json:"app_id"`
	Mode          string         `json:"mode"`
	URL           string         `json:"url"`
	AdminPassword string         `json:"admin_password,omitempty"`
	Network       string         `json:"network,omitempty"`
	Ready         bool           `json:"ready"`
	Operation     string         `json:"operation,omitempty"`
	Error         string         `json:"error,omitempty"`
	Event         string         `json:"event,omitempty"`
	Backup        BackupPolicy   `json:"backup"`
	NextBackup    time.Time      `json:"next_backup"`
	Backups       []BackupRecord `json:"backups"`
}
type DatabaseView struct {
	AppID      string         `json:"app_id"`
	Mode       string         `json:"mode"`
	Ready      bool           `json:"ready"`
	Operation  string         `json:"operation"`
	Error      string         `json:"error"`
	Event      string         `json:"event"`
	Backup     BackupPolicy   `json:"backup"`
	NextBackup time.Time      `json:"next_backup"`
	Backups    []BackupRecord `json:"backups"`
	Retained   bool           `json:"retained"`
}

func (p BackupPolicy) Validate() error {
	if p.Hours != 0 && p.Hours != 1 && p.Hours != 24 && p.Hours != 168 {
		return errors.New("choose manual, hourly, daily or weekly backups")
	}
	if p.Keep < 1 || p.Keep > 100 {
		return errors.New("retain between 1 and 100 backups")
	}
	return nil
}
func (r DatabaseRequest) Validate() error {
	if r.Mode == "" || r.Mode == "none" {
		return nil
	}
	if r.Mode != "local" && r.Mode != "external" {
		return errors.New("choose a local, external or no database")
	}
	if err := r.Backup.Validate(); err != nil {
		return err
	}
	if r.Mode == "external" {
		u, err := url.Parse(r.URL)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.User == nil || u.Path == "" || u.Path == "/" || u.Fragment != "" || len(r.URL) > 8192 {
			return errors.New("enter a PostgreSQL URL with a host, user and database")
		}
		for key, values := range u.Query() {
			if postgresOptions[key] == "" || len(values) != 1 {
				return errors.New("unsupported or duplicate PostgreSQL URL parameter; use sslmode, application_name, options, channel_binding or connect_timeout")
			}
		}
		switch u.Query().Get("sslmode") {
		case "require", "verify-ca", "verify-full":
		default:
			return errors.New("external PostgreSQL must use TLS: add sslmode=require or sslmode=verify-full")
		}
	}
	return nil
}
func (m *Manager) databaseViews() []DatabaseView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []DatabaseView{}
	for id, d := range m.data.Databases {
		_, live := m.data.Apps[id]
		out = append(out, DatabaseView{AppID: id, Mode: d.Mode, Ready: d.Ready, Operation: d.Operation, Error: d.Error, Event: d.Event, Backup: d.Backup, NextBackup: d.NextBackup, Backups: slices.Clone(d.Backups), Retained: !live})
	}
	return out
}
func (m *Manager) configureDatabase(id string, input DatabaseRequest) error {
	if err := input.Validate(); err != nil {
		return err
	}
	if input.Mode == "none" || input.Mode == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok || a.Retiring {
		return errors.New("application unavailable")
	}
	if m.busy(id) {
		return errors.New("wait for the active deployment")
	}
	previous, exists := m.data.Databases[id]
	if exists && (previous.Mode != "external" || input.Mode != "external" || previous.Operation != "") {
		return errors.New("database already configured or busy; use retry or backup settings")
	}
	if _, ok = a.Env["DATABASE_URL"]; ok && !exists {
		return errors.New("remove manual DATABASE_URL before attaching a managed database")
	}
	d := Database{AppID: id, Mode: input.Mode, URL: input.URL, Backup: input.Backup}
	if exists {
		d.Backups = previous.Backups
	}
	if d.Mode == "local" {
		d.Network = "lidza-db-" + id
		d.AdminPassword = newID() + newID()
		u := url.URL{Scheme: "postgres", User: url.UserPassword("app", newID()+newID()), Host: d.Network + ":5432", Path: "/app", RawQuery: "sslmode=disable"}
		d.URL = u.String()
	}
	if input.Backup.Offsite && m.data.BackupStorage == nil {
		return errors.New("configure S3 storage before enabling off-site backups")
	}
	m.data.Databases[id] = d
	if err := m.save(); err != nil {
		if exists {
			m.data.Databases[id] = previous
		} else {
			delete(m.data.Databases, id)
		}
		return err
	}
	return m.startDatabaseLocked(id, "provision")
}
func (m *Manager) databaseAction(id, action string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startDatabaseLocked(id, action)
}
func (m *Manager) startDatabaseLocked(id, action string) error {
	d, ok := m.data.Databases[id]
	if !ok {
		return errors.New("no database configured")
	}
	a, live := m.data.Apps[id]
	if !live || a.Retiring {
		return errors.New("application unavailable; retained database is preserved")
	}
	if d.Operation != "" {
		return errors.New("database operation already running")
	}
	if action != "provision" && action != "backup" {
		return errors.New("unknown database operation")
	}
	if action == "backup" && !d.Ready {
		return errors.New("database not ready")
	}
	if action == "provision" && d.Ready {
		return errors.New("database already ready")
	}
	if action == "provision" && m.busy(id) {
		return errors.New("wait for the active deployment")
	}
	old := d
	d.Operation = action
	d.Error = ""
	m.data.Databases[id] = d
	if err := m.save(); err != nil {
		m.data.Databases[id] = old
		return err
	}
	var target *storage.Config
	if m.data.BackupStorage != nil {
		copy := *m.data.BackupStorage
		target = &copy
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 2*time.Hour)
		defer cancel()
		var err error
		var record BackupRecord
		select {
		case m.databaseSlots <- struct{}{}:
			defer func() { <-m.databaseSlots }()
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err == nil {
			if action == "provision" {
				err = m.provisionDatabase(ctx, d)
			} else {
				record, err = m.dumpDatabase(ctx, d, target)
			}
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		latest := m.data.Databases[id]
		latest.Operation = ""
		latest.Event = newID()
		if record.ID != "" {
			latest.Backups = append(latest.Backups, record)
		}
		if err != nil {
			latest.Error = "Database operation failed: " + err.Error()
			if action == "backup" {
				latest.NextBackup = time.Now().UTC().Add(15 * time.Minute)
			}
		} else {
			latest.Error = ""
			if action == "provision" {
				latest.Ready = true
				latest.NextBackup = time.Now().UTC()
				a := m.data.Apps[id]
				a.Env = maps.Clone(a.Env)
				if a.Env == nil {
					a.Env = map[string]string{}
				}
				a.Env["DATABASE_URL"] = latest.URL
				m.data.Apps[id] = a
			} else {
				latest.NextBackup = time.Now().UTC().Add(time.Duration(latest.Backup.Hours) * time.Hour)
			}
		}
		m.data.Databases[id] = latest
		// Keep the database reserved if persistence fails; a restart retries safely.
		if e := m.save(); e != nil {
			latest.Error = "could not persist database result; retry after checking disk space"
			m.data.Databases[id] = latest
			return
		}
		if record.ID != "" {
			m.pruneBackupsLocked(id)
		}
	}()
	return nil
}
func (m *Manager) setBackupPolicy(id string, p BackupPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data.Databases[id]
	if !ok {
		return errors.New("no database configured")
	}
	if d.Operation != "" {
		return errors.New("database operation running")
	}
	if p.Offsite && m.data.BackupStorage == nil {
		return errors.New("configure S3 storage first")
	}
	old := d
	d.Backup = p
	d.NextBackup = time.Now().UTC().Add(time.Duration(p.Hours) * time.Hour)
	m.data.Databases[id] = d
	if err := m.save(); err != nil {
		m.data.Databases[id] = old
		return err
	}
	return nil
}
func (m *Manager) backupDir(id string) string { return filepath.Join(m.cfg.DataDir, "backups", id) }
func (m *Manager) setBackupStorage(cfg *storage.Config) error {
	if cfg != nil {
		if err := ValidateBackupStorage(*cfg); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.data.BackupStorage
	m.data.BackupStorage = cfg
	if err := m.save(); err != nil {
		m.data.BackupStorage = old
		return err
	}
	return nil
}
func ValidateBackupStorage(cfg storage.Config) error {
	if cfg.Provider != "s3" {
		return errors.New("backup storage must be S3-compatible")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("S3 endpoint requires HTTPS")
	}
	if _, err = storage.New(cfg); err != nil {
		return fmt.Errorf("invalid S3 configuration")
	}
	return nil
}
