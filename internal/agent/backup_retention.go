package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agim/lidza-deploy/internal/retention"
	"github.com/agim/lidza/packs/storage"
)

// Off-site retention. Local disk keeps only the newest dump of a database
// (pruneBackupsLocked); the bucket keeps a grandfather-father-son history
// chosen by internal/retention. After each verified upload the agent lists
// that database's folder and deletes what the policy no longer keeps, once
// the control plane has sent the workspace's retention with Automatic set.
// The same plan is available for review (GET /v1/backups/retention/plan) and
// is applied only as reviewed (POST /v1/backups/retention/apply).
//
// The bucket is shared by every server of the workspace, so an agent never
// decides that a folder without a local database is orphaned: another server
// may own it. The control plane, which sees every server, plans orphans.

// offsiteRetentionMu serializes listing and deleting, so an automatic run
// and a reviewed apply never interleave.
var offsiteRetentionMu sync.Mutex

// offsiteRetentionListLimit bounds the objects listed per database.
const offsiteRetentionListLimit = 20000

// offsiteRetentionTarget is one database captured for planning.
type offsiteRetentionTarget struct {
	id       string
	policy   retention.Policy
	override bool
	busy     string
	created  map[string]time.Time
}

// offsitePolicyLocked is the policy of one database: its own override, the
// workspace's, or the default. The caller holds m.mu.
func (m *Manager) offsitePolicyLocked(d Database) (retention.Policy, bool) {
	if d.Backup.Retention != nil {
		return *d.Backup.Retention, true
	}
	if m.data.BackupRetention != nil {
		return m.data.BackupRetention.Policy, false
	}
	return retention.Default(), false
}

// offsiteRetentionTargets captures the databases to plan; nil ids means every
// database in the inventory.
func (m *Manager) offsiteRetentionTargets(ids []string) (storage.Config, []offsiteRetentionTarget, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.BackupStorage == nil {
		return storage.Config{}, nil, errors.New("configure off-site storage first")
	}
	if ids == nil {
		for id := range m.data.Databases {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var out []offsiteRetentionTarget
	for _, id := range ids {
		d, ok := m.data.Databases[id]
		if !ok || !idPattern.MatchString(id) {
			continue
		}
		policy, override := m.offsitePolicyLocked(d)
		target := offsiteRetentionTarget{id: id, policy: policy, override: override, busy: d.Operation, created: map[string]time.Time{}}
		for _, b := range d.Backups {
			if b.Offsite && b.ObjectKey != "" && !b.Created.IsZero() {
				target.created[b.ObjectKey] = b.Created
			}
		}
		out = append(out, target)
	}
	return *m.data.BackupStorage, out, nil
}

// offsiteRetentionPlan lists each database's folder and plans it.
func (m *Manager) offsiteRetentionPlan(ctx context.Context, ids []string) (retention.Plan, storage.Config, error) {
	cfg, targets, err := m.offsiteRetentionTargets(ids)
	if err != nil {
		return retention.Plan{}, cfg, err
	}
	store, err := storage.New(cfg)
	if err != nil {
		return retention.Plan{}, cfg, errors.New("off-site storage configuration is invalid")
	}
	now := time.Now().UTC()
	var plans []retention.DatabasePlan
	for _, target := range targets {
		listed, err := store.List(ctx, retention.Prefix(target.id), offsiteRetentionListLimit)
		if err != nil {
			return retention.Plan{}, cfg, fmt.Errorf("could not list off-site backups of %s; check ListBucket permission", target.id)
		}
		if len(listed) >= offsiteRetentionListLimit {
			plans = append(plans, retention.KeepAll(target.id, retention.StatusProtected, "too many objects to plan safely", nil))
			continue
		}
		objects := make([]retention.Object, 0, len(listed))
		for _, o := range listed {
			created := o.ModifiedAt
			if t, ok := target.created[o.Key]; ok {
				created = t
			}
			objects = append(objects, retention.Object{Key: o.Key, Size: o.Size, Created: created})
		}
		if len(objects) == 0 {
			continue
		}
		var plan retention.DatabasePlan
		if target.busy != "" {
			plan = retention.KeepAll(target.id, retention.StatusProtected, "a "+target.busy+" operation is running", objects)
		} else {
			plan = retention.Select(target.id, objects, target.policy, now, nil)
			plan.Override = target.override
		}
		plans = append(plans, plan)
	}
	return retention.Summarize(plans, now), cfg, nil
}

// OffsiteRetentionResult reports an applied cleanup.
type OffsiteRetentionResult struct {
	Deleted      int               `json:"deleted"`
	DeletedBytes int64             `json:"deleted_bytes"`
	Failed       []retention.Entry `json:"failed,omitempty"`
}

// applyOffsiteRetentionPlan deletes the plan's objects, then clears the
// off-site flag of inventory records whose object is gone.
func (m *Manager) applyOffsiteRetentionPlan(ctx context.Context, plan retention.Plan, cfg storage.Config, trigger string) OffsiteRetentionResult {
	var result OffsiteRetentionResult
	if plan.DeleteObjects == 0 {
		return result
	}
	store, err := storage.New(cfg)
	if err != nil {
		for _, d := range plan.Databases {
			result.Failed = append(result.Failed, d.Delete...)
		}
		return result
	}
	deleted := map[string]bool{}
	for _, d := range plan.Databases {
		var lines []string
		var bytes int64
		for _, e := range d.Delete {
			if db, _, ok := retention.ParseKey(e.Key); !ok || db != d.Database || e.Location != "" {
				result.Failed = append(result.Failed, e)
				continue
			}
			if err := store.Delete(ctx, e.Key); err != nil {
				result.Failed = append(result.Failed, e)
				continue
			}
			deleted[e.Key] = true
			result.Deleted++
			result.DeletedBytes += e.Size
			bytes += e.Size
			lines = append(lines, fmt.Sprintf("%s (%s)", strings.TrimPrefix(e.Key, retention.Prefix(d.Database)), retentionSize(e.Size)))
		}
		if len(lines) > 0 {
			log.Printf("off-site retention (%s): %s: deleted %d objects, %s: %s", trigger, d.Database, len(lines), retentionSize(bytes), strings.Join(lines, ", "))
		}
	}
	if len(result.Failed) > 0 {
		log.Printf("off-site retention (%s): %d objects could not be deleted; check DeleteObject permission", trigger, len(result.Failed))
	}
	if len(deleted) == 0 {
		return result
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := false
	for _, d := range plan.Databases {
		current, ok := m.data.Databases[d.Database]
		if !ok {
			continue
		}
		var backups []BackupRecord
		for i, b := range current.Backups {
			if b.Offsite && deleted[b.ObjectKey] {
				if backups == nil {
					backups = slices.Clone(current.Backups)
				}
				backups[i].Offsite, backups[i].ObjectKey = false, ""
			}
		}
		if backups == nil {
			continue
		}
		current.Backups = backups
		m.data.Databases[d.Database] = current
		changed = true
	}
	if changed {
		if err := m.save(); err != nil {
			log.Printf("off-site retention (%s): inventory update not saved: %v", trigger, err)
		}
	}
	return result
}

func retentionSize(n int64) string {
	switch {
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
}

// offsiteRetentionAfterUploadLocked starts the automatic cleanup of one
// database after a verified upload. The caller holds m.mu.
func (m *Manager) offsiteRetentionAfterUploadLocked(id string) {
	if s := m.data.BackupRetention; s == nil || !s.Automatic || m.data.BackupStorage == nil {
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.enforceOffsiteRetention(m.ctx, id)
	}()
}

// enforceOffsiteRetention plans and applies one database's retention.
func (m *Manager) enforceOffsiteRetention(ctx context.Context, id string) OffsiteRetentionResult {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	offsiteRetentionMu.Lock()
	defer offsiteRetentionMu.Unlock()
	plan, cfg, err := m.offsiteRetentionPlan(ctx, []string{id})
	if err != nil {
		log.Printf("off-site retention (after upload): %s: %v", id, err)
		return OffsiteRetentionResult{}
	}
	return m.applyOffsiteRetentionPlan(ctx, plan, cfg, "after upload")
}

func (m *Manager) setBackupRetention(s *retention.Settings) error {
	if s != nil {
		if err := s.Validate(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.data.BackupRetention
	m.data.BackupRetention = s
	if err := m.save(); err != nil {
		m.data.BackupRetention = old
		return err
	}
	return nil
}

// setDatabaseRetention sets or clears (nil) one database's override.
func (m *Manager) setDatabaseRetention(id string, p *retention.Policy) error {
	if p != nil {
		if err := p.Validate(); err != nil {
			return err
		}
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
	old := d
	d.Backup.Retention = p
	m.data.Databases[id] = d
	if err := m.save(); err != nil {
		m.data.Databases[id] = old
		return err
	}
	return nil
}

func (m *Manager) backupRetentionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/backup-retention", func(w http.ResponseWriter, r *http.Request) {
		var s *retention.Settings
		if err := Decode(w, r, &s); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setBackupRetention(s); err != nil {
			Fail(w, 400, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "saved"})
	})
	mux.HandleFunc("PATCH /v1/databases/{id}/backups/retention", func(w http.ResponseWriter, r *http.Request) {
		var p *retention.Policy
		if err := Decode(w, r, &p); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setDatabaseRetention(r.PathValue("id"), p); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "saved"})
	})
	mux.HandleFunc("GET /v1/backups/retention/plan", func(w http.ResponseWriter, r *http.Request) {
		offsiteRetentionMu.Lock()
		plan, _, err := m.offsiteRetentionPlan(r.Context(), nil)
		offsiteRetentionMu.Unlock()
		if err != nil {
			Fail(w, 409, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, plan)
	})
	mux.HandleFunc("POST /v1/backups/retention/apply", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Fingerprint string `json:"fingerprint"`
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		offsiteRetentionMu.Lock()
		defer offsiteRetentionMu.Unlock()
		plan, cfg, err := m.offsiteRetentionPlan(ctx, nil)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		if err := plan.Check(in.Fingerprint); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, m.applyOffsiteRetentionPlan(ctx, plan, cfg, "reviewed"))
	})
}
