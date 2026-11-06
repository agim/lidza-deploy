package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/retention"
	"github.com/agim/lidza/packs/audit"
	"github.com/agim/lidza/packs/storage"
)

// Off-site backup retention for the whole workspace bucket. Agents clean up
// one database after each of its uploads (agent/backup_retention.go); the
// control plane sees every server, so it alone plans folders whose database
// no server has any more (orphans), and it can review and apply one plan for
// the entire bucket with the same selection rules.

// retentionMu serializes workspace planning and deleting.
var retentionMu sync.Mutex

// retentionListLimit bounds the objects listed per bucket location.
const retentionListLimit = 100000

const legacyLocation = "legacy"

var retentionDatabaseID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

// retentionSettings returns the saved settings, or the defaults with
// reviewed false.
func (c *Control) retentionSettings() (retention.Settings, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.BackupRetention == nil {
		return retention.DefaultSettings(), false
	}
	return *c.data.BackupRetention, true
}

// pushBackupRetention sends the saved settings to one agent. Nothing is sent
// before an operator saves them, so agents do not clean up on their own.
func (c *Control) pushBackupRetention(r *http.Request, server string) error {
	c.mu.Lock()
	var settings *retention.Settings
	if c.data.BackupRetention != nil {
		copy := *c.data.BackupRetention
		settings = &copy
	}
	c.mu.Unlock()
	if settings == nil {
		return nil
	}
	return c.agentCall(r, server, http.MethodPut, "/v1/backup-retention", settings, nil)
}

// RetentionPlan is the workspace-wide cleanup plan.
type RetentionPlan struct {
	retention.Plan
	Bucket       string             `json:"bucket"`
	Prefix       string             `json:"prefix"`
	LegacyPrefix string             `json:"legacy_prefix,omitempty"`
	Settings     retention.Settings `json:"settings"`
	Reviewed     bool               `json:"reviewed"`
	// Unavailable servers did not answer; folders no answering server owns
	// are then kept as "unknown" instead of being planned as orphans.
	Unavailable []string `json:"unavailable_servers"`
	// Ignored counts objects that are not database backups; they are never
	// touched.
	Ignored int `json:"ignored_objects"`
}

// RetentionResult reports an applied workspace cleanup.
type RetentionResult struct {
	Deleted      int               `json:"deleted"`
	DeletedBytes int64             `json:"deleted_bytes"`
	Failed       []retention.Entry `json:"failed,omitempty"`
}

type retentionOwner struct {
	server string
	view   agent.DatabaseView
}

// planWorkspaceRetention lists the bucket, asks every server for its
// databases and plans each database folder.
func (c *Control) planWorkspaceRetention(r *http.Request) (RetentionPlan, map[string]*storage.Storage, error) {
	cfg, legacyPrefix, ok := c.backupDestination()
	if !ok {
		return RetentionPlan{}, nil, errors.New("configure off-site storage first")
	}
	settings, reviewed := c.retentionSettings()
	out := RetentionPlan{Bucket: cfg.Bucket, Prefix: strings.Trim(cfg.Prefix, "/"), Settings: settings, Reviewed: reviewed, Unavailable: []string{}}
	stores := map[string]*storage.Storage{}
	locations := []string{""}
	if legacyPrefix != "" {
		out.LegacyPrefix = legacyPrefix
		locations = append(locations, legacyLocation)
	}
	objects := map[string][]retention.Object{}
	for _, location := range locations {
		target := cfg
		if location == legacyLocation {
			target.Prefix = legacyPrefix
		}
		store, err := storage.New(target)
		if err != nil {
			return RetentionPlan{}, nil, errors.New("storage configuration unavailable")
		}
		stores[location] = store
		listed, err := store.List(r.Context(), "databases/", retentionListLimit)
		if err != nil {
			if location == legacyLocation {
				// The legacy location is optional; plan the current one.
				delete(stores, location)
				out.LegacyPrefix = ""
				continue
			}
			return RetentionPlan{}, nil, errors.New("cannot list remote backups; check bucket ListBucket permission, endpoint and credentials")
		}
		if len(listed) >= retentionListLimit {
			return RetentionPlan{}, nil, fmt.Errorf("the bucket holds more than %d backup objects; plan cleanup per server instead", retentionListLimit)
		}
		for _, o := range listed {
			database, _, ok := retention.ParseKey(o.Key)
			if !ok {
				out.Ignored++
				continue
			}
			objects[database] = append(objects[database], retention.Object{Key: o.Key, Size: o.Size, Created: o.ModifiedAt, Location: location})
		}
	}
	owners := map[string][]retentionOwner{}
	for _, result := range c.readFleet(r, "/v1/databases") {
		var views []agent.DatabaseView
		if result.Err != nil || json.Unmarshal(result.Body, &views) != nil {
			name := result.Server.Name
			if name == "" {
				name = result.Server.ID
			}
			out.Unavailable = append(out.Unavailable, name)
			continue
		}
		for _, v := range views {
			owners[v.AppID] = append(owners[v.AppID], retentionOwner{server: result.Server.ID, view: v})
		}
	}
	slices.Sort(out.Unavailable)
	now := time.Now().UTC()
	c.mu.Lock()
	moving := map[string]bool{}
	for _, job := range c.data.Moves {
		if terminalMoveState(job.State) {
			continue
		}
		for _, d := range job.Databases {
			moving[d.SourceID], moving[d.TargetID] = true, true
		}
	}
	changed := false
	if c.data.BackupOrphans == nil {
		c.data.BackupOrphans = map[string]time.Time{}
	}
	var plans []retention.DatabasePlan
	for database, list := range objects {
		found := owners[database]
		switch {
		case len(found) > 0:
			if _, ok := c.data.BackupOrphans[database]; ok {
				delete(c.data.BackupOrphans, database)
				changed = true
			}
			plans = append(plans, ownedRetentionPlan(database, list, found, settings.Policy, moving[database], now))
		case len(out.Unavailable) > 0:
			plans = append(plans, retention.KeepAll(database, retention.StatusUnknown, "no answering server has this database; "+strings.Join(out.Unavailable, ", ")+" did not answer", list))
		default:
			removed, ok := c.data.BackupOrphans[database]
			if !ok {
				removed = now
				c.data.BackupOrphans[database] = now
				changed = true
			}
			plans = append(plans, retention.Orphan(database, list, removed, settings.OrphanGrace(), now))
		}
	}
	if len(out.Unavailable) == 0 {
		for database := range c.data.BackupOrphans {
			if len(objects[database]) == 0 {
				delete(c.data.BackupOrphans, database)
				changed = true
			}
		}
	}
	var saveErr error
	if changed {
		saveErr = c.save()
	}
	c.mu.Unlock()
	if saveErr != nil {
		return RetentionPlan{}, nil, errors.New("could not save when orphaned backups were first noticed")
	}
	out.Plan = retention.Summarize(plans, now)
	return out, stores, nil
}

// ownedRetentionPlan plans a database at least one server has. Several
// owners (the same id on two servers) keep the most of each period.
func ownedRetentionPlan(database string, objects []retention.Object, owners []retentionOwner, workspace retention.Policy, moving bool, now time.Time) retention.DatabasePlan {
	if moving {
		return retention.KeepAll(database, retention.StatusProtected, "an application move is in progress", objects)
	}
	var policy retention.Policy
	override := false
	for i, o := range owners {
		if o.view.Operation != "" {
			return retention.KeepAll(database, retention.StatusProtected, "a "+o.view.Operation+" operation is running on "+o.server, objects)
		}
		p := workspace
		if o.view.Backup.Retention != nil {
			p, override = *o.view.Backup.Retention, true
		}
		if i == 0 {
			policy = p
		} else {
			policy = retention.Policy{Daily: max(policy.Daily, p.Daily), Weekly: max(policy.Weekly, p.Weekly), Monthly: max(policy.Monthly, p.Monthly)}
		}
	}
	plan := retention.Select(database, objects, policy, now, nil)
	plan.Override = override
	if len(owners) > 1 {
		plan.Note = fmt.Sprintf("%d servers have this database; the longest retention of each period applies", len(owners))
	}
	return plan
}

// deleteRetentionPlan deletes exactly the plan's objects.
func deleteRetentionPlan(ctx context.Context, plan retention.Plan, stores map[string]*storage.Storage, trigger string) RetentionResult {
	var result RetentionResult
	for _, d := range plan.Databases {
		var lines []string
		var bytes int64
		for _, e := range d.Delete {
			store := stores[e.Location]
			if database, _, ok := retention.ParseKey(e.Key); !ok || database != d.Database || store == nil {
				result.Failed = append(result.Failed, e)
				continue
			}
			if err := store.Delete(ctx, e.Key); err != nil {
				result.Failed = append(result.Failed, e)
				continue
			}
			result.Deleted++
			result.DeletedBytes += e.Size
			bytes += e.Size
			name := strings.TrimPrefix(e.Key, retention.Prefix(d.Database))
			if e.Location != "" {
				name += " [" + e.Location + "]"
			}
			lines = append(lines, fmt.Sprintf("%s (%s)", name, moveSize(e.Size)))
		}
		if len(lines) > 0 {
			log.Printf("off-site retention (%s): %s (%s): deleted %d objects, %s: %s", trigger, d.Database, d.Status, len(lines), moveSize(bytes), strings.Join(lines, ", "))
		}
	}
	if len(result.Failed) > 0 {
		log.Printf("off-site retention (%s): %d objects could not be deleted; check DeleteObject permission", trigger, len(result.Failed))
	}
	return result
}

// retentionTick applies the workspace plan on its own once an operator has
// saved retention with automatic cleanup, and only when every server
// answered, so no folder is mistaken for an orphan.
func (c *Control) retentionTick(ctx context.Context) error {
	settings, reviewed := c.retentionSettings()
	if !reviewed || !settings.Automatic {
		return nil
	}
	if _, _, ok := c.backupDestination(); !ok {
		return nil
	}
	ctx = audit.System(ctx, "backup-retention")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.PublicURL, nil)
	if err != nil {
		return err
	}
	result, err := c.automaticRetention(request)
	if err != nil || result.Deleted+len(result.Failed) == 0 {
		return err
	}
	event := audit.Event{Action: "backups.retention.apply", Resource: "storage/backups", Scope: fleetScope, Meta: map[string]string{"deleted": fmt.Sprint(result.Deleted), "deleted_bytes": fmt.Sprint(result.DeletedBytes), "failed": fmt.Sprint(len(result.Failed))}}
	if len(result.Failed) > 0 {
		event.Outcome = audit.Failed
	}
	return audit.From(ctx).Record(ctx, event)
}

func (c *Control) automaticRetention(r *http.Request) (RetentionResult, error) {
	retentionMu.Lock()
	defer retentionMu.Unlock()
	plan, stores, err := c.planWorkspaceRetention(r)
	if err != nil {
		return RetentionResult{}, err
	}
	if len(plan.Unavailable) > 0 {
		log.Printf("off-site retention (automatic): skipped; %s did not answer", strings.Join(plan.Unavailable, ", "))
		return RetentionResult{}, nil
	}
	return deleteRetentionPlan(r.Context(), plan.Plan, stores, "automatic"), nil
}

func (c *Control) backupRetentionRoutes(handle func(string, http.HandlerFunc)) {
	handle("GET /api/control/storage/retention", c.getRetentionSettings)
	handle("PUT /api/control/storage/retention", c.putRetentionSettings)
	handle("GET /api/control/storage/retention/plan", c.workspaceRetentionPlan)
	handle("POST /api/control/storage/retention/apply", c.applyWorkspaceRetention)
	handle("GET /api/control/servers/{server}/backups/retention/plan", c.serverRetentionPlan)
	handle("POST /api/control/servers/{server}/backups/retention/apply", c.serverRetentionApply)
	handle("PATCH /api/control/servers/{server}/databases/{id}/backups/retention", c.databaseRetention)
}

func (c *Control) getRetentionSettings(w http.ResponseWriter, r *http.Request) {
	settings, reviewed := c.retentionSettings()
	agent.JSON(w, 200, map[string]any{"settings": settings, "reviewed": reviewed, "defaults": retention.DefaultSettings(), "limits": map[string]int{"daily": retention.MaxDaily, "weekly": retention.MaxWeekly, "monthly": retention.MaxMonthly, "orphan_grace_days": retention.MaxOrphanGrace}})
}

func (c *Control) putRetentionSettings(w http.ResponseWriter, r *http.Request) {
	var in retention.Settings
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if err := in.Validate(); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	c.mu.Lock()
	old := c.data.BackupRetention
	c.data.BackupRetention = &in
	if err := c.save(); err != nil {
		c.data.BackupRetention = old
		c.mu.Unlock()
		agent.Fail(w, 500, errors.New("could not save backup retention"))
		return
	}
	c.mu.Unlock()
	unsynced := []string{}
	for _, s := range c.servers() {
		if err := c.pushBackupRetention(r, s.ID); err != nil {
			unsynced = append(unsynced, serverLabel(s))
		}
	}
	agent.JSON(w, 200, map[string]any{"status": "saved", "unsynced_servers": unsynced})
}

func serverLabel(s Server) string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

func (c *Control) workspaceRetentionPlan(w http.ResponseWriter, r *http.Request) {
	retentionMu.Lock()
	plan, _, err := c.planWorkspaceRetention(r)
	retentionMu.Unlock()
	if err != nil {
		agent.Fail(w, 502, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, plan)
}

func (c *Control) applyWorkspaceRetention(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	retentionMu.Lock()
	defer retentionMu.Unlock()
	plan, stores, err := c.planWorkspaceRetention(r)
	if err != nil {
		agent.Fail(w, 502, err)
		return
	}
	if err := plan.Check(in.Fingerprint); err != nil {
		agent.Fail(w, 409, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	agent.JSON(w, 200, deleteRetentionPlan(ctx, plan.Plan, stores, "reviewed"))
}

func (c *Control) serverRetentionPlan(w http.ResponseWriter, r *http.Request) {
	server := r.PathValue("server")
	if err := c.syncBackupStorage(r, server); err != nil {
		failAgent(w, err)
		return
	}
	var plan json.RawMessage
	if err := c.agentCall(r, server, http.MethodGet, "/v1/backups/retention/plan", nil, &plan); err != nil {
		failAgent(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, plan)
}

func (c *Control) serverRetentionApply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	var result json.RawMessage
	if err := c.agentCall(r, r.PathValue("server"), http.MethodPost, "/v1/backups/retention/apply", in, &result); err != nil {
		failAgent(w, err)
		return
	}
	agent.JSON(w, 200, result)
}

func (c *Control) databaseRetention(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !retentionDatabaseID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	var in *retention.Policy
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if in != nil {
		if err := in.Validate(); err != nil {
			agent.Fail(w, 400, err)
			return
		}
	}
	if err := c.agentCall(r, r.PathValue("server"), http.MethodPatch, "/v1/databases/"+id+"/backups/retention", in, nil); err != nil {
		failAgent(w, err)
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "saved"})
}
