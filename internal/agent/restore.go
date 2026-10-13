package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type RestoreRequest struct {
	Source string `json:"source"`
	Backup string `json:"backup"`
	Target string `json:"target"`
	EnvKey string `json:"env_key"`
}

func localDatabase(id string, policy BackupPolicy) Database {
	network := "lidza-db-" + id
	return Database{AppID: id, Mode: "local", Network: network, AdminPassword: newID() + newID(), URL: (&url.URL{Scheme: "postgres", User: url.UserPassword("app", newID()+newID()), Host: network + ":5432", Path: "/app", RawQuery: "sslmode=disable"}).String(), Backup: policy}
}
func (m *Manager) restoreDatabase(appID string, in RestoreRequest) error {
	if m.upgradePending() {
		return errors.New("agent upgrade in progress")
	}
	if !idPattern.MatchString(in.Target) || in.Target == in.Source {
		return errors.New("choose a new database ID")
	}
	if in.EnvKey == "" {
		in.EnvKey = "DATABASE_URL"
	}
	if !validDatabaseKey(in.EnvKey) {
		return errors.New("invalid database environment key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[appID]
	if !ok || a.Retiring || a.Restoring || m.busy(appID) {
		return errors.New("application unavailable or busy")
	}
	source, ok := m.data.Databases[in.Source]
	if !ok || source.Operation != "" {
		return errors.New("backup source unavailable or busy")
	}
	var record BackupRecord
	for _, b := range source.Backups {
		if b.ID == in.Backup {
			record = b
		}
	}
	if record.ID == "" {
		return errors.New("unknown local backup")
	}
	if _, ok := m.data.Databases[in.Target]; ok {
		return errors.New("target database already exists; restore never overwrites a database")
	}
	if _, ok := m.data.Apps[in.Target]; ok {
		return errors.New("target ID reserved by application")
	}
	target := localDatabase(in.Target, source.Backup)
	target.Operation = "restore"
	old := source
	source.Operation = "restore"
	a.Restoring = true
	m.data.Databases[in.Source] = source
	m.data.Databases[in.Target] = target
	m.data.Apps[appID] = a
	if err := m.save(); err != nil {
		m.data.Databases[in.Source] = old
		delete(m.data.Databases, in.Target)
		a.Restoring = false
		m.data.Apps[appID] = a
		return err
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 2*time.Hour)
		defer cancel()
		select {
		case m.databaseSlots <- struct{}{}:
			defer func() { <-m.databaseSlots }()
		case <-ctx.Done():
			m.finishRestore(appID, in, ctx.Err())
			return
		}
		err := m.restoreInto(ctx, in.Source, record, target)
		m.finishRestore(appID, in, err)
	}()
	return nil
}
func (m *Manager) restoreInto(ctx context.Context, source string, record BackupRecord, target Database) error {
	f, err := os.Open(filepath.Join(m.backupDir(source), record.ID+".dump"))
	if err != nil {
		return errors.New("local backup unavailable")
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if err != nil || n != record.Size || hex.EncodeToString(hash.Sum(nil)) != record.SHA256 {
		return errors.New("backup integrity check failed")
	}
	if _, err = f.Seek(0, 0); err != nil {
		return err
	}
	if err = m.provisionDatabase(ctx, target); err != nil {
		return err
	}
	env, flags := postgresEnvironment(target.URL)
	args := []string{"run", "--rm", "--name", "lidza-restore-" + target.AppID, "--network", target.Network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "512m", "--pids-limit", "128", "-i"}
	args = append(args, flags...)
	args = append(args, databaseImage, "pg_restore", "--dbname=app", "--no-owner", "--no-acl", "--exit-on-error", "--single-transaction")
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = command(cleanup, "", nil, "docker", "rm", "-f", "lidza-restore-"+target.AppID)
	}()
	if err = dockerStream(ctx, f, io.Discard, env, args...); err != nil {
		return errors.New("restore failed; original database remains attached")
	}
	return m.provisionDatabase(ctx, target)
}
func (m *Manager) finishRestore(appID string, in RestoreRequest, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	source := m.data.Databases[in.Source]
	source.Operation = ""
	m.data.Databases[in.Source] = source
	target := m.data.Databases[in.Target]
	target.Operation = ""
	target.Event = newID()
	target.Ready = err == nil
	if err != nil {
		target.Error = err.Error()
	} else {
		target.NextBackup = time.Now().UTC().Add(time.Duration(target.Backup.Hours) * time.Hour)
	}
	m.data.Databases[in.Target] = target
	a, exists := m.data.Apps[appID]
	a.Restoring = false
	if exists {
		m.data.Apps[appID] = a
	}
	if saveErr := m.save(); saveErr != nil {
		target.Error = "restore metadata could not be saved; original connection retained"
		m.data.Databases[in.Target] = target
		return
	}
	if err == nil && exists && !a.Retiring {
		if bindErr := m.bindDatabaseLocked(appID, in.EnvKey, in.Target); bindErr != nil {
			target.Error = "restore completed; automatic attachment failed; attach the new database after the app is idle"
			m.data.Databases[in.Target] = target
			_ = m.save()
		}
	}
}
func (m *Manager) restoreRoute(w http.ResponseWriter, r *http.Request) {
	var in RestoreRequest
	if err := Decode(w, r, &in); err != nil {
		Fail(w, 400, err)
		return
	}
	if err := m.restoreDatabase(r.PathValue("id"), in); err != nil {
		Fail(w, 409, err)
		return
	}
	JSON(w, 202, map[string]string{"status": "restoring", "database": in.Target})
}
