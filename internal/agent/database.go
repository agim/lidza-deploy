package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/retention"
	"github.com/agim/lidza/packs/jobs"
	"github.com/agim/lidza/packs/storage"
)

var postgresOptions = map[string]string{"sslmode": "PGSSLMODE", "application_name": "PGAPPNAME", "options": "PGOPTIONS", "channel_binding": "PGCHANNELBINDING", "connect_timeout": "PGCONNECT_TIMEOUT"}

type BackupPolicy struct {
	Hours    int    `json:"hours"` // Legacy interval in hours.
	Cron     string `json:"cron,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	Keep     int    `json:"keep"`
	Offsite  bool   `json:"offsite"`
	// Retention overrides the workspace's off-site retention for this
	// database; nil follows the workspace default.
	Retention *retention.Policy `json:"retention,omitempty"`
}
type DatabaseRequest struct {
	ID     string       `json:"id,omitempty"`
	EnvKey string       `json:"env_key,omitempty"`
	Mode   string       `json:"mode"` // local, external, none
	URL    string       `json:"url,omitempty"`
	Backup BackupPolicy `json:"backup"`
}
type BackupRecord struct {
	ExcludedTableData []string  `json:"excluded_table_data,omitempty"`
	Kind              string    `json:"kind,omitempty"`
	ID                string    `json:"id"`
	Created           time.Time `json:"created"`
	Size              int64     `json:"size"`
	SHA256            string    `json:"sha256"`
	Offsite           bool      `json:"offsite"`
	ObjectKey         string    `json:"object_key,omitempty"`
	OffsiteRequested  bool      `json:"offsite_requested,omitempty"`
	UploadError       string    `json:"upload_error,omitempty"`
	// Seconds is how long a successful backup took, from the start of the
	// dump through its required off-site upload. It sizes later
	// pre-deployment backup deadlines.
	Seconds float64 `json:"seconds,omitempty"`
}
type Database struct {
	ResourceProfile string         `json:"resource_profile,omitempty"`
	MemoryMiB       int            `json:"memory_mib,omitempty"`
	CPUs            string         `json:"cpus,omitempty"`
	Ephemeral       bool           `json:"ephemeral,omitempty"`
	PreviewID       string         `json:"preview_id,omitempty"`
	AppID           string         `json:"app_id"`
	Mode            string         `json:"mode"`
	URL             string         `json:"url"`
	AdminPassword   string         `json:"admin_password,omitempty"`
	Network         string         `json:"network,omitempty"`
	Ready           bool           `json:"ready"`
	Operation       string         `json:"operation,omitempty"`
	Error           string         `json:"error,omitempty"`
	Event           string         `json:"event,omitempty"`
	Backup          BackupPolicy   `json:"backup"`
	NextBackup      time.Time      `json:"next_backup"`
	Backups         []BackupRecord `json:"backups"`
	// BeforeDeploy is the pre-deployment backup choice the panel saved for
	// this database (before_deploy.go); zero until an operator sets one, when
	// the reviewed configuration's value, or always, applies.
	BeforeDeploy BeforeDeployPolicy `json:"before_deploy,omitzero"`
	// ExcludedTables are the tables whose data backups skip, as the panel
	// saved them (backup_excluded_tables.go); nil until saved, when the
	// legacy backup_exclude_table_data configuration applies.
	ExcludedTables *[]string `json:"backup_excluded_tables,omitempty"`
	// Removed is when the database's data was removed from this host
	// (retained_remove.go); the record keeps its backup history.
	Removed time.Time `json:"removed,omitzero"`
}
type DatabaseView struct {
	Attachments []DatabaseAttachment `json:"attachments"`
	Engine      string               `json:"engine,omitempty"`
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
	// BeforeDeploy is the database's effective pre-deployment backup policy;
	// Primary marks a database that cannot choose never (a native primary,
	// analytics or standalone database, or a container application's
	// DATABASE_URL).
	BeforeDeploy BeforeDeployPolicy `json:"before_deploy"`
	Primary      bool               `json:"primary"`
	// ExcludedTables are the tables whose data this database's backups
	// skip (schema retained); ExcludedTablesSupported is false where
	// backups always contain every table (native, SQLite, ClickHouse).
	ExcludedTables          []string `json:"excluded_tables,omitempty"`
	ExcludedTablesSupported bool     `json:"excluded_tables_supported,omitempty"`
	// SizeBytes is measured for ClickHouse databases (system.parts).
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Progress is the running operation's live progress; agents without it
	// omit the field.
	Progress *Progress `json:"progress,omitempty"`
	// Removed is when the database's data was removed; only its backups
	// remain.
	Removed time.Time `json:"removed,omitzero"`
}

func (p BackupPolicy) Validate() error {
	if p.Hours != 0 && p.Hours != 1 && p.Hours != 24 && p.Hours != 168 {
		return errors.New("choose manual, hourly, daily or weekly backups")
	}
	if p.Cron != "" {
		if p.Hours != 0 || len(p.Cron) > 120 || len(p.Timezone) > 80 {
			return errors.New("choose one backup interval or one calendar schedule")
		}
		if fields := strings.Fields(p.Cron); len(fields) == 5 {
			if _, err := strconv.Atoi(fields[0]); err != nil {
				return errors.New("calendar backups require one fixed minute per hour")
			}
		}
		if _, err := jobs.Cron(p.Cron, p.Timezone); err != nil {
			return errors.New("enter a valid five-field backup schedule and timezone")
		}
	} else if p.Timezone != "" {
		return errors.New("backup timezone requires a calendar schedule")
	}
	if p.Keep < 1 || p.Keep > 100 {
		return errors.New("retain between 1 and 100 backups")
	}
	if p.Retention != nil {
		return p.Retention.Validate()
	}
	return nil
}

func (p BackupPolicy) Scheduled() bool { return p.Hours > 0 || p.Cron != "" }

func (p BackupPolicy) NextBackupAfter(after time.Time) time.Time {
	if p.Cron != "" {
		schedule, err := jobs.Cron(p.Cron, p.Timezone)
		if err != nil {
			return time.Time{}
		}
		return schedule.Next(after).UTC()
	}
	return after.UTC().Add(time.Duration(p.Hours) * time.Hour)
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
		live := m.databaseInUseLocked(id)
		policy, next := d.Backup, d.NextBackup
		if !live {
			policy.Hours, policy.Cron, policy.Timezone = 0, "", ""
			next = time.Time{}
		}
		engine := ""
		if m.data.NativeDatabases[id].Policy.SQLitePath != "" {
			engine = "sqlite"
		} else if m.data.NativeDatabases[id].Policy.ClickHouseDatabase != "" {
			engine = "clickhouse"
		}
		view := DatabaseView{Attachments: attachments, Engine: engine, AppID: id, Mode: d.Mode, Ready: d.Ready, Operation: d.Operation, Error: d.Error, Event: d.Event, Backup: policy, NextBackup: next, Backups: slices.Clone(d.Backups), Retained: !live,
			BeforeDeploy: m.beforeDeployPolicyLocked(id), Primary: m.databasePrimaryLocked(id), Removed: d.Removed, ExcludedTables: m.backupExcludedTablesLocked(id)}
		if _, err := m.excludedTablesSupportedLocked(id); err == nil {
			view.ExcludedTablesSupported = true
		}
		if d.Operation != "" {
			view.Progress = m.progress.snapshot(databaseProgressKey(id))
		}
		out = append(out, view)
	}
	return out
}

// A retained database's owner ID may be reused by a fresh application. Only
// actual bindings or a live release establish that it still needs backups.
func (m *Manager) databaseInUseLocked(id string) bool {
	if m.nativeDatabaseInUseLocked(id) {
		return true
	}
	for _, a := range m.data.Apps {
		if a.Retiring {
			continue
		}
		for _, dbID := range a.Bindings {
			if dbID == id {
				return true
			}
		}
		if a.Current != nil && slices.Contains(a.Current.DatabaseIDs, id) {
			return true
		}
	}
	return false
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
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[appID]
	if !ok || a.Retiring {
		return errors.New("application unavailable")
	}
	if input.ID == "" {
		input.ID = a.resourceID()
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
	if m.cfg.Native != nil {
		if _, reserved := m.cfg.Native.Databases[id]; reserved {
			return errors.New("native database identity is reserved for its reviewed adoption")
		}
	}
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
	input.Backup.Keep = 1 // Local retention is fixed independently of S3 history.
	d := Database{AppID: id, Mode: input.Mode, URL: input.URL, Backup: input.Backup}
	if exists {
		d.Backups = previous.Backups
	}
	if d.Mode == "local" {
		if profile, ok := profileByID(a.RuntimeProfile); ok {
			d.ResourceProfile = profile.ID
			d.MemoryMiB = profile.Database.MemoryMiB
			d.CPUs = profile.Database.CPUs
		}
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
	if action == "backup" && !m.databaseInUseLocked(id) {
		return errors.New("backups are disabled for databases without an active application")
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
	if !d.Removed.IsZero() {
		return errors.New("this database's data was removed; restore one of its backups into a new database instead")
	}
	if _, native := m.data.NativeDatabases[id]; native && action != "backup" {
		return errors.New("native databases are adopted in place; provisioning is unavailable")
	}
	for _, ref := range m.attachmentsLocked(id) {
		if m.busy(ref.AppID) || m.nativeOperations[ref.AppID] {
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
		ctx, endProgress := m.trackDatabase(ctx, id, "Waiting for an available database operation slot")
		defer endProgress()
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
				progressFrom(ctx).Stage("Provisioning the database", 0, nil)
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
				if latest.Backup.Cron != "" {
					latest.NextBackup = latest.Backup.NextBackupAfter(latest.NextBackup)
				}
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
				latest.NextBackup = latest.Backup.NextBackupAfter(time.Now())
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
		if err == nil && record.Offsite {
			m.offsiteRetentionAfterUploadLocked(id)
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
	if m.data.NativeDatabases[id].Policy.ArchiveDatabase != "" && !p.Offsite {
		return errors.New("standalone native backups require off-site storage")
	}
	if p.Offsite && m.data.BackupStorage == nil {
		return errors.New("configure S3 storage first")
	}
	old := d
	p.Keep = 1
	if p.Retention == nil {
		// Schedule changes keep the retention override; clear it through
		// the retention endpoint.
		p.Retention = d.Backup.Retention
	}
	d.Backup = p
	d.NextBackup = p.NextBackupAfter(time.Now())
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
