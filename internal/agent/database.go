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
	ID     string       `json:"id,omitempty"`
	EnvKey string       `json:"env_key,omitempty"`
	Mode   string       `json:"mode"` // local, external, none
	URL    string       `json:"url,omitempty"`
	Backup BackupPolicy `json:"backup"`
}
type BackupRecord struct {
	Kind      string    `json:"kind,omitempty"`
	ID        string    `json:"id"`
	Created   time.Time `json:"created"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	Offsite   bool      `json:"offsite"`
	ObjectKey string    `json:"object_key,omitempty"`
}
type Database struct {
	Ephemeral     bool           `json:"ephemeral,omitempty"`
	PreviewID     string         `json:"preview_id,omitempty"`
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
	Attachments []DatabaseAttachment `json:"attachments"`
	AppID       string               `json:"app_id"`
	Mode        string               `json:"mode"`
	Ready       bool                 `json:"ready"`
	Operation   string               `json:"operation"`
	Error       string               `json:"error"`
	Event       string               `json:"event"`
	Backup      BackupPolicy         `json:"backup"`
	NextBackup  time.Time            `json:"next_backup"`
	Backups     []BackupRecord       `json:"backups"`
	Retained    bool                 `json:"retained"`
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
	if r.ID != "" && !idPattern.MatchString(r.ID) {
		return errors.New("invalid database ID")
	}
	if r.EnvKey != "" && !validDatabaseKey(r.EnvKey) {
		return errors.New("use DATABASE_URL or a name ending in _DATABASE_URL")
	}
	if r.Mode == "existing" {
		if r.ID == "" {
			return errors.New("choose a database")
		}
		return nil
	}
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
		attachments := m.attachmentsLocked(id)
		live := len(attachments) > 0
		for _, a := range m.data.Apps {
			if a.Current != nil && slices.Contains(a.Current.DatabaseIDs, id) {
				live = true
			}
		}
		out = append(out, DatabaseView{Attachments: attachments, AppID: id, Mode: d.Mode, Ready: d.Ready, Operation: d.Operation, Error: d.Error, Event: d.Event, Backup: d.Backup, NextBackup: d.NextBackup, Backups: slices.Clone(d.Backups), Retained: !live})
	}
	return out
}
func (m *Manager) configureDatabase(appID string, input DatabaseRequest) error {
	if m.upgradePending() {
		return errors.New("agent upgrade in progress")
	}
	if err := input.Validate(); err != nil {
		return err
	}
	if input.Mode == "" || input.Mode == "none" {
		return nil
	}
	if input.EnvKey == "" {
		input.EnvKey = "DATABASE_URL"
	}
	if input.ID == "" {
		input.ID = appID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[appID]
	if !ok || a.Retiring {
		return errors.New("application unavailable")
	}
	if m.busy(appID) {
		return errors.New("wait for deployment or reload")
	}
	if input.Mode == "existing" {
		return m.bindDatabaseLocked(appID, input.EnvKey, input.ID)
	}
	if _, exists := a.Env[input.EnvKey]; !exists && len(a.Env) >= 100 {
		return errors.New("at most 100 environment variables")
	}
	id := input.ID
	previous, exists := m.data.Databases[id]
	if exists {
		if previous.Ready {
			return errors.New("create another connection and attach it to change the URL; the old database and backups are preserved")
		}
		if previous.Mode != "external" || input.Mode != "external" || previous.Operation != "" || a.Bindings[input.EnvKey] != id {
			return errors.New("database ID exists; choose attach existing or a new ID")
		}
		for _, ref := range m.attachmentsLocked(id) {
			if m.busy(ref.AppID) {
				return errors.New("an attached app is busy")
			}
		}
	} else if len(m.data.Databases) >= 200 {
		return errors.New("agent database limit reached")
	}
	if _, manual := a.Env[input.EnvKey]; manual && a.Bindings[input.EnvKey] == "" {
		return errors.New("remove the manually configured variable before attaching a database")
	}
	if input.Backup.Offsite && m.data.BackupStorage == nil {
		return errors.New("configure S3 storage first")
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
	if a.Preview {
		d.Ephemeral = true
		d.PreviewID = a.ID
	}
	oldApp := a
	a.Bindings = maps.Clone(a.Bindings)
	if a.Bindings == nil {
		a.Bindings = map[string]string{}
	}
	a.Bindings[input.EnvKey] = id
	m.data.Apps[appID] = a
	m.data.Databases[id] = d
	if err := m.save(); err != nil {
		m.data.Apps[appID] = oldApp
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
	if m.upgradePending() {
		return errors.New("agent upgrade in progress")
	}
	d, ok := m.data.Databases[id]
	if !ok {
		return errors.New("no database configured")
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
	for _, ref := range m.attachmentsLocked(id) {
		if m.busy(ref.AppID) {
			return errors.New("wait for the active deployment or reload")
		}
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
		reloadApps := map[string]bool{}
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
				for _, ref := range m.attachmentsLocked(id) {
					a := m.data.Apps[ref.AppID]
					a.Env = maps.Clone(a.Env)
					if a.Env == nil {
						a.Env = map[string]string{}
					}
					a.Env[ref.EnvKey] = latest.URL
					m.data.Apps[a.ID] = a
					reloadApps[a.ID] = true
				}
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

		for appID := range reloadApps {
			a := m.data.Apps[appID]
			if a.Current != nil {
				if _, e := m.queueLocked(a, DeployRequest{}, true); e != nil {
					latest.Error = "database ready; automatic reload could not start; use Reload after other operations finish"
					m.data.Databases[id] = latest
					_ = m.save()
				}
			}
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
