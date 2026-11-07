package control

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/buildmem"
	"github.com/agim/lidza-deploy/internal/detect"
	"github.com/agim/lidza/packs/audit"
	"github.com/agim/lidza/packs/auth"
)

// Moving an application to another server.
//
// A move is a job in control state with one record per step. The source
// application stays the live record (c.data.Apps) and keeps serving until
// the operator finishes the move; the job holds the destination side under
// the same application id. Databases move as dumps and persistent files as
// verified archives (move_files.go). DNS is switched by hand: the move pauses
// at cutover with the records to set and continues once every hostname
// resolves to the destination. Every step is idempotent, so a failed or
// interrupted step (including a control panel restart) is retried from its
// start.
//
//   POST /api/control/apps/{id}/move          start {target_server, runtime_override?}
//   GET  /api/control/apps/{id}/move-plan     review before starting
//   GET  /api/control/apps/{id}/move          the application's latest move
//   GET  /api/control/moves/{id}              one move
//   POST /api/control/moves/{id}/{action}     continue, recheck-dns, final-sync, finish, abort, remove-retained

const (
	moveStatePlanned   = "planned"
	moveStateRunning   = "running"
	moveStateFailed    = "failed"
	moveStateCutover   = "cutover"
	moveStateReady     = "ready"
	moveStateSyncing   = "syncing"
	moveStateFinishing = "finishing"
	moveStateFinished  = "finished"
	moveStateAborting  = "aborting"
	moveStateAborted   = "aborted"

	moveStepPlan        = "plan"
	moveStepPrepare     = "prepare"
	moveStepEnvironment = "environment"
	moveStepData        = "data"
	moveStepFiles       = "files"
	moveStepDeploy      = "deploy"
	moveStepCutover     = "cutover"
	moveStepFinalSync   = "final_sync"
	moveStepFinish      = "finish"

	moveRuntimeNative    = "native"
	moveRuntimeContainer = "container"

	moveMaxLogLines = 120
	moveKeepJobs    = 50
)

var moveStepTitles = []struct{ key, title string }{
	{moveStepPlan, "Plan"},
	{moveStepPrepare, "Prepare destination"},
	{moveStepEnvironment, "Copy environment"},
	{moveStepData, "Copy data"},
	{moveStepFiles, "Copy files"},
	{moveStepDeploy, "Deploy on destination"},
	{moveStepCutover, "Switch DNS"},
	{moveStepFinalSync, "Final sync (application locked)"},
	{moveStepFinish, "Finish"},
}

// MoveJob is a move's durable record. It never holds an environment value,
// credential or database content.
type MoveJob struct {
	ID            string                     `json:"id"`
	AppID         string                     `json:"app_id"`
	SourceServer  string                     `json:"source_server"`
	TargetServer  string                     `json:"target_server"`
	SourceRuntime string                     `json:"source_runtime"` // native or container
	TargetRuntime string                     `json:"target_runtime"` // native or container
	Runtime       string                     `json:"runtime"`        // go, rails, node, static, astro
	State         string                     `json:"state"`
	Error         string                     `json:"error,omitempty"`
	Steps         []MoveStep                 `json:"steps"`
	Plan          MovePlan                   `json:"plan"`
	Target        Application                `json:"target"`
	NativeSpec    *agent.NativeCreateRequest `json:"native_spec,omitempty"`
	ContainerSpec *agent.App                 `json:"container_spec,omitempty"`
	Databases     []MoveDatabase             `json:"databases"`
	Files         []MoveFileCopy             `json:"files,omitempty"`
	ClickHouse    *MoveClickHouse            `json:"clickhouse,omitempty"`
	Redis         bool                       `json:"redis"`
	CacheLocal    bool                       `json:"cache_local,omitempty"`
	Commit        string                     `json:"commit,omitempty"`
	Hostnames     []string                   `json:"hostnames"`
	// RedirectHostnames are the application's redirect domains: recreated
	// on the destination, and their DNS moves with the cutover.
	RedirectHostnames []string        `json:"redirect_hostnames,omitempty"`
	Addresses         []string        `json:"addresses"`
	DNS               []MoveDNSStatus `json:"dns"`
	DNSChecked        *time.Time      `json:"dns_checked,omitempty"`
	// PTR is the reverse DNS of the destination's addresses, checked at the
	// cutover of a mail-sending application (move_mail.go); it warns only.
	PTR []MovePTRStatus `json:"ptr,omitempty"`
	// DNSAhead records that a hostname resolved to the destination before
	// the application was deployed there.
	DNSAhead      bool   `json:"dns_ahead,omitempty"`
	TLSReady      bool   `json:"tls_ready"`
	TLSDetail     string `json:"tls_detail,omitempty"`
	TargetCreated bool   `json:"target_created"`
	Deployment    string `json:"deployment,omitempty"`
	DeployAttempt int    `json:"deploy_attempt,omitempty"`
	SourceFrozen  bool   `json:"source_frozen,omitempty"`
	// TargetFrozen is set while the final sync holds the destination in
	// maintenance (or stopped), so it writes nothing while its data is
	// replaced.
	TargetFrozen bool `json:"target_frozen,omitempty"`
	FinalSynced  bool `json:"final_synced,omitempty"`
	Switched     bool `json:"switched"`
	// AutoCutover starts the final sync and the finish itself once DNS
	// points to the destination and HTTPS works there (move_auto.go).
	AutoCutover bool      `json:"auto_cutover"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`

	// The fields below were added with retries, checkpoints and the
	// final-sync lock; jobs recorded before them load with their zero values
	// and continue under the same rules.

	// Failure is the current step's last failure: while a transient failure
	// waits for its automatic retry, and once the move stopped
	// (move_retry.go).
	Failure *MoveFailure `json:"failure,omitempty"`
	// MaxAttempts bounds the automatic attempts of one step; 0 is the
	// default (moveDefaultAttempts).
	MaxAttempts int `json:"max_attempts,omitempty"`
	// Checkpoints record what each engine finished, by phase, engine and
	// object (move_engine.go), so a retried step continues instead of
	// starting over.
	Checkpoints map[string]MoveCheckpoint `json:"checkpoints,omitempty"`
	// SourceChanges are changes made to the live source while the move
	// copied in the background (move_reconcile.go); the final sync picks
	// them up.
	SourceChanges []MoveSourceChange `json:"source_changes,omitempty"`
	// DeployedCommit is the commit the destination last deployed. Stale
	// marks a destination release the source has since replaced: the
	// destination redeploys the source's commit before the switch.
	DeployedCommit string `json:"deployed_commit,omitempty"`
	Stale          bool   `json:"destination_stale,omitempty"`
	// EnvDigests fingerprint the copied environment (keyed HMAC, never the
	// values), so the final sync can show what changed on the source.
	EnvDigests map[string]string `json:"env_digests,omitempty"`
	// Stall is set while the running step's progress stands still
	// (move_progress.go).
	Stall *MoveStall `json:"stall,omitempty"`
}

// MoveStep is one step's status and log.
type MoveStep struct {
	Key      string        `json:"key"`
	Title    string        `json:"title"`
	Status   string        `json:"status"` // pending, running, waiting, done, skipped, failed
	Started  *time.Time    `json:"started,omitempty"`
	Finished *time.Time    `json:"finished,omitempty"`
	Log      []MoveLogLine `json:"log"`
	// Progress is the step's live progress while it runs (from memory, see
	// move_progress.go) and its last progress once it ended. Absent in
	// steps recorded before progress existed.
	Progress *agent.Progress `json:"progress,omitempty"`
}

type MoveLogLine struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
	// Heartbeat marks a "still working" line, which the next heartbeat
	// replaces; it is not output of the step itself.
	Heartbeat bool `json:"heartbeat,omitempty"`
}

// MoveDatabase is one database being copied.
type MoveDatabase struct {
	SourceID string             `json:"source_id"`
	TargetID string             `json:"target_id,omitempty"`
	EnvKeys  []string           `json:"env_keys"`
	Mode     string             `json:"mode"`
	Copyable bool               `json:"copyable"`
	Backup   agent.BackupPolicy `json:"backup"`
	// BeforeDeploy is the source database's pre-deployment backup policy,
	// set again on the destination database once it exists.
	BeforeDeploy agent.BeforeDeployPolicy `json:"before_deploy,omitzero"`
	Copied       string                   `json:"copied_sha256,omitempty"`
	Bytes        int64                    `json:"bytes,omitempty"`
	Skipped      string                   `json:"skipped,omitempty"`
	// Excluded are the tables whose data the source's backups leave out,
	// each with the operator's choice to copy its data or leave it behind
	// (move_tables.go).
	Excluded []MoveExcludedTable `json:"excluded_tables,omitempty"`
	// Replication is how the database is copied: continuous (logical
	// replication) or dump (move_replication.go); ReplicationReason says why
	// the dump path was chosen or fallen back to; ReplicationRestart marks a
	// source that Prepare restarts once for wal_level=logical;
	// ReplicationExcluded are tables without a replica identity, copied with
	// a data dump instead; Link is the replication path once set up.
	Replication         string               `json:"replication,omitempty"`
	ReplicationReason   string               `json:"replication_reason,omitempty"`
	ReplicationRestart  bool                 `json:"replication_restart,omitempty"`
	ReplicationExcluded []string             `json:"replication_excluded,omitempty"`
	Link                *MoveReplicationLink `json:"replication_link,omitempty"`
	// ReplicationVia is the planned network path in words ("through the
	// control panel").
	ReplicationVia string `json:"replication_via,omitempty"`
}

// MoveFileCopy is one set of persistent files being copied.
type MoveFileCopy struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Source string `json:"source"`
	Files  int64  `json:"files"`
	Bytes  int64  `json:"bytes"`
	// Transfer is false for sets the plan could not move; the operator
	// acknowledged a warning for them.
	Transfer bool `json:"transfer"`
	// Method is the planned transfer (s3 or direct); Used is how the last
	// copy went.
	Method       string `json:"method,omitempty"`
	Used         string `json:"used,omitempty"`
	Copied       string `json:"copied_sha256,omitempty"`
	ArchiveBytes int64  `json:"archive_bytes,omitempty"`
	Skipped      string `json:"skipped,omitempty"`
}

// MoveClickHouse is the application's managed ClickHouse database: copied
// as a verified archive (move_clickhouse.go).
type MoveClickHouse struct {
	SourceMode     string `json:"source_mode"` // container or host
	SourceDatabase string `json:"source_database"`
	SourceVersion  string `json:"source_version,omitempty"`
	TargetDatabase string `json:"target_database,omitempty"`
	TargetVersion  string `json:"target_version,omitempty"`
	Format         string `json:"format,omitempty"` // backup or native, of the last copy
	Copied         string `json:"copied_sha256,omitempty"`
	Bytes          int64  `json:"bytes,omitempty"`
	Tables         int    `json:"tables,omitempty"`
	Rows           uint64 `json:"rows,omitempty"`
	Skipped        string `json:"skipped,omitempty"`
}

// MoveDNSStatus is the last observed resolution of one hostname.
type MoveDNSStatus struct {
	Hostname  string   `json:"hostname"`
	Addresses []string `json:"addresses"`
	Ready     bool     `json:"ready"`
	Error     string   `json:"error,omitempty"`
	// Redirect marks a redirect domain of the application.
	Redirect bool `json:"redirect,omitempty"`
	// Source names the nameserver that answered: the zone's own
	// nameserver (authoritative, so caches do not delay the cutover) or a
	// fallback resolver.
	Source string `json:"source,omitempty"`
}

// MoveDNSRecord is one record the operator sets at their DNS provider (A,
// AAAA, TXT) or, for a PTR, at the server provider; Note says which and why.
type MoveDNSRecord struct {
	Hostname string `json:"hostname"`
	Type     string `json:"type"`
	Value    string `json:"value"`
	Note     string `json:"note,omitempty"`
}

// MovePlan is reviewed before a move starts.
type MovePlan struct {
	AppID          string         `json:"app_id"`
	SourceServer   string         `json:"source_server"`
	TargetServer   string         `json:"target_server"`
	SourceRuntime  string         `json:"source_runtime"`
	Runtime        string         `json:"runtime"`
	TargetRuntime  string         `json:"target_runtime"`
	RuntimeChoices []string       `json:"runtime_choices"`
	Detection      *MoveDetection `json:"detection,omitempty"`
	Repository     string         `json:"repository"`
	Branch         string         `json:"branch"`
	Commit         string         `json:"commit,omitempty"`
	Hostnames      []string       `json:"hostnames"`
	// RedirectDomains are recreated on the destination; their hostnames'
	// DNS records are listed with the cutover's (redirects.go).
	RedirectDomains []agent.RedirectDomain `json:"redirect_domains,omitempty"`
	RedirectNote    string                 `json:"redirect_note,omitempty"`
	EnvironmentKeys []string               `json:"environment_keys"`
	CopiedKeys      []string               `json:"copied_keys"`
	CopiedSources   map[string]int         `json:"copied_sources,omitempty"` // copied variables by origin: panel, deploy defaults, image
	DestinationKeys []string               `json:"destination_keys"`
	Databases       []MovePlanDatabase     `json:"databases"`
	Files           []MovePlanFileSet      `json:"files"`
	Redis           bool                   `json:"redis"`
	Volumes         []string               `json:"volumes"`
	Addresses       []string               `json:"addresses"`
	Capacity        *MoveCapacity          `json:"capacity,omitempty"`
	// BuildMemory compares the memory the application's build needs with
	// the destination's memory.
	BuildMemory *buildmem.Assessment `json:"build_memory,omitempty"`
	// Estimate is each engine's size and estimated bulk-copy and
	// final-sync duration (move_estimate.go).
	Estimate *MoveEstimate `json:"estimate,omitempty"`
	// PublicPorts are the host ports the destination publishes again
	// (move_ports.go); Mail the checks and records of a mail-sending
	// application (move_mail.go).
	PublicPorts []agent.StackPort `json:"public_ports,omitempty"`
	Mail        *MovePlanMail     `json:"mail,omitempty"`
	// HostMail is the destination's host mail server that Prepare binds to
	// loopback to free a published port (move_ports.go).
	HostMail *MovePlanHostMail `json:"host_mail,omitempty"`
	// MaxAttempts is the attempt limit of each step; Replication the
	// requested database copy mode (auto or dump).
	MaxAttempts int    `json:"max_attempts"`
	Replication string `json:"replication,omitempty"`
	// ReplicationNetwork is the requested replication path: auto (a shared
	// private network the destination reaches, else through the control
	// panel), panel, or public (the TLS relay on the source's public
	// address, only on request).
	ReplicationNetwork string `json:"replication_network,omitempty"`
	// AutoCutover: the final sync and the finish start by themselves once
	// DNS points to the destination (default when every database
	// replicates continuously, move_auto.go).
	AutoCutover bool      `json:"auto_cutover"`
	Warnings    []string  `json:"warnings"`
	Notes       []string  `json:"notes"`
	Blockers    []string  `json:"blockers"`
	Checked     time.Time `json:"checked"`
}

// moveServerLabel names a server in the plan's sentences: its name, else
// its id. The installer names the panel's own host "This server", which
// reads as the wrong server in a sentence about a move between two
// servers; such a name becomes "the source server (This server)".
func moveServerLabel(s Server, role string) string {
	name := cmp.Or(strings.TrimSpace(s.Name), s.ID)
	if strings.EqualFold(name, "this server") || strings.EqualFold(name, "local") {
		return "the " + role + " server (" + name + ")"
	}
	return name
}

type MoveDetection struct {
	Mode      string   `json:"mode,omitempty"`
	Runtime   string   `json:"runtime,omitempty"`
	Framework string   `json:"framework,omitempty"`
	Reasons   []string `json:"reasons,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type MovePlanDatabase struct {
	ID        string `json:"id"`
	Engine    string `json:"engine"`
	Mode      string `json:"mode"`
	Tables    int    `json:"tables"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Action    string `json:"action"` // copy, skip_empty, external, manual
	Note      string `json:"note,omitempty"`
	// BeforeDeploy names the database's pre-deployment backup policy
	// ("always", "reuse within 24h", "never"), which the move carries over.
	BeforeDeploy string `json:"before_deploy,omitempty"`
	// ExcludedTables are the tables whose data the source's backups leave
	// out, with their estimated rows and sizes; each is copied unless the
	// operator leaves it behind (move_tables.go).
	ExcludedTables []MoveExcludedTable `json:"excluded_tables,omitempty"`
	// Replication is the copy mode the move will use: continuous or dump
	// (move_replication.go), with the reason for a dump and whether Prepare
	// restarts the source database once.
	Replication        string `json:"replication,omitempty"`
	ReplicationReason  string `json:"replication_reason,omitempty"`
	ReplicationRestart bool   `json:"replication_restart,omitempty"`
	// ReplicationPath is the first network path the move tries (panel,
	// private or public), ReplicationVia the same in words.
	ReplicationPath string `json:"replication_path,omitempty"`
	ReplicationVia  string `json:"replication_via,omitempty"`
}

// MovePlanFileSet is one set of persistent files in a plan.
type MovePlanFileSet struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Source string `json:"source"`
	Files  int64  `json:"files"`            // -1 when not measured
	Bytes  int64  `json:"bytes"`            // -1 when not measured
	Action string `json:"action"`           // copy, skip_empty, manual, blocked
	Method string `json:"method,omitempty"` // s3 or direct
	Note   string `json:"note,omitempty"`
	// Skipped counts runtime files (sockets, pipes, devices) the copy
	// leaves behind; the service recreates them on the destination.
	Skipped int64 `json:"skipped,omitempty"`
}

type MoveCapacity struct {
	DiskAvailable   uint64 `json:"disk_available"`
	MemoryAvailable uint64 `json:"memory_available"`
	Needed          uint64 `json:"needed"`
}

type moveStartInput struct {
	TargetServer        string   `json:"target_server"`
	RuntimeOverride     string   `json:"runtime_override,omitempty"`
	AcknowledgeWarnings bool     `json:"acknowledge_warnings,omitempty"`
	Addresses           []string `json:"addresses,omitempty"`
	// MaxAttempts bounds the automatic attempts of each step (1–10; 0 is
	// the default of six).
	MaxAttempts int `json:"max_attempts,omitempty"`
	// ExcludedTables chooses, by database and table, "copy" (the default)
	// or "leave" for tables whose data the source's backups leave out.
	ExcludedTables map[string]map[string]string `json:"excluded_tables,omitempty"`
	// Replication is "auto" (continuous replication when the databases
	// allow it, the default) or "dump" (always dump and restore).
	Replication string `json:"replication,omitempty"`
	// ReplicationNetwork is "auto" (the default), "panel" or "public"; see
	// MovePlan.ReplicationNetwork.
	ReplicationNetwork string `json:"replication_network,omitempty"`
	// AutoCutover runs the final sync and the finish automatically once DNS
	// points to the destination; absent, the plan's default applies.
	AutoCutover *bool `json:"auto_cutover,omitempty"`
}

// moveResolver resolves hostnames, reverse DNS and the TXT and MX records
// of mail-sending applications (move_mail.go); tests replace it.
type moveResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupAddr(ctx context.Context, address string) ([]string, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

var defaultMoveResolver moveResolver = newAuthoritativeResolver()

func terminalMoveState(state string) bool {
	return state == moveStateFinished || state == moveStateAborted
}

// ---- state helpers ----------------------------------------------------------

func (c *Control) moveJob(id string) (MoveJob, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	job, ok := c.data.Moves[id]
	return cloneMoveJob(job), ok
}

func cloneMoveJob(job MoveJob) MoveJob {
	if job.Failure != nil {
		failure := *job.Failure
		failure.Diagnostics = slices.Clone(failure.Diagnostics)
		job.Failure = &failure
	}
	if job.Stall != nil {
		stall := *job.Stall
		job.Stall = &stall
	}
	if job.ClickHouse != nil {
		ch := *job.ClickHouse
		job.ClickHouse = &ch
	}
	job.Checkpoints = maps.Clone(job.Checkpoints)
	job.SourceChanges = slices.Clone(job.SourceChanges)
	job.EnvDigests = maps.Clone(job.EnvDigests)
	job.Steps = slices.Clone(job.Steps)
	for i := range job.Steps {
		job.Steps[i].Log = slices.Clone(job.Steps[i].Log)
	}
	job.Databases = slices.Clone(job.Databases)
	for i := range job.Databases {
		if link := job.Databases[i].Link; link != nil {
			copy := *link
			copy.Peers, copy.Exclude = slices.Clone(link.Peers), slices.Clone(link.Exclude)
			job.Databases[i].Link = &copy
		}
	}
	job.Files = slices.Clone(job.Files)
	job.DNS = slices.Clone(job.DNS)
	job.PTR = slices.Clone(job.PTR)
	if job.Plan.Mail != nil {
		mail := *job.Plan.Mail
		mail.PTR, mail.Records = slices.Clone(mail.PTR), slices.Clone(mail.Records)
		job.Plan.Mail = &mail
	}
	job.Hostnames = slices.Clone(job.Hostnames)
	job.RedirectHostnames = slices.Clone(job.RedirectHostnames)
	job.Target.RedirectDomains = slices.Clone(job.Target.RedirectDomains)
	job.Addresses = slices.Clone(job.Addresses)
	return job
}

// activeMoveLocked returns the application's move that is not finished or
// aborted. Caller holds c.mu.
func (c *Control) activeMoveLocked(appID string) (MoveJob, bool) {
	for _, job := range c.data.Moves {
		if job.AppID == appID && !terminalMoveState(job.State) {
			return job, true
		}
	}
	return MoveJob{}, false
}

// updateMove applies change to a job and saves state; the change is undone
// when saving fails.
func (c *Control) updateMove(id string, change func(*MoveJob) error) (MoveJob, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, ok := c.data.Moves[id]
	if !ok {
		return MoveJob{}, errors.New("move not found")
	}
	job := cloneMoveJob(previous)
	if err := change(&job); err != nil {
		return previous, err
	}
	job.Updated = time.Now().UTC()
	c.data.Moves[id] = job
	if err := c.save(); err != nil {
		c.data.Moves[id] = previous
		return previous, err
	}
	return cloneMoveJob(job), nil
}

func (job *MoveJob) step(key string) *MoveStep {
	for i := range job.Steps {
		if job.Steps[i].Key == key {
			return &job.Steps[i]
		}
	}
	return nil
}

func (job *MoveJob) logf(key, format string, args ...any) {
	s := job.step(key)
	if s == nil {
		return
	}
	s.Log = append(s.Log, MoveLogLine{Time: time.Now().UTC(), Message: fmt.Sprintf(format, args...)})
	if len(s.Log) > moveMaxLogLines {
		s.Log = s.Log[len(s.Log)-moveMaxLogLines:]
	}
}

func (job *MoveJob) setStep(key, status string) {
	s := job.step(key)
	if s == nil {
		return
	}
	now := time.Now().UTC()
	switch status {
	case "running":
		s.Started, s.Finished, s.Progress = &now, nil, nil
	case "done", "skipped", "failed":
		s.Finished = &now
	}
	s.Status = status
}

// setProgress records a step's last progress at a transition.
func (job *MoveJob) setProgress(key string, p *agent.Progress) {
	if s := job.step(key); s != nil {
		s.Progress = p
	}
}

func (c *Control) moveLog(id, step, format string, args ...any) {
	_, _ = c.updateMove(id, func(job *MoveJob) error { job.logf(step, format, args...); return nil })
}

// moveAudit records a move transition when the audit pack is available.
func moveAudit(ctx context.Context, job MoveJob, action string, failed bool) {
	moveAuditMeta(ctx, job, action, failed, nil)
}

// moveAuditMeta records a move transition with extra metadata.
func moveAuditMeta(ctx context.Context, job MoveJob, action string, failed bool, extra map[string]string) {
	log, ok := lidza.Optional[*audit.Audit](ctx)
	if !ok || log == nil {
		return
	}
	meta := map[string]string{"move": job.ID, "source": job.SourceServer, "target": job.TargetServer}
	for key, value := range extra {
		meta[key] = value
	}
	e := audit.Event{Action: "move." + action, Scope: fleetScope, Resource: "app/" + job.AppID, Meta: meta}
	if failed {
		e.Outcome = audit.Failed
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if user := auth.CurrentUser(writeCtx); user == nil || user.ID == "" {
		writeCtx = audit.System(writeCtx, "move")
	}
	_ = log.Record(writeCtx, e)
}

// moveShadowLocked reports whether serverID hosts the non-live side of a
// move of appID: the prepared destination before the switch, or the source
// after it until it is retired. Inventory synchronization ignores it.
// Caller holds c.mu.
func (c *Control) moveShadowLocked(serverID, appID string) bool {
	for _, job := range c.data.Moves {
		if job.AppID != appID || terminalMoveState(job.State) {
			continue
		}
		if !job.Switched && job.TargetServer == serverID || job.Switched && job.SourceServer == serverID {
			return true
		}
	}
	return false
}

// moveCheckoutTarget returns the destination record a checkout ticket was
// issued for while the move has not switched yet.
func (c *Control) moveCheckoutTarget(appID, serverID, generation string) (Application, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, job := range c.data.Moves {
		if job.AppID == appID && !terminalMoveState(job.State) && !job.Switched && job.Target.ServerID == serverID && job.Target.Generation == generation && generation != "" {
			return job.Target, true
		}
	}
	return Application{}, false
}

// moveView is a job as the browser sees it.
type moveView struct {
	MoveJob
	Records []MoveDNSRecord `json:"records"`
	Actions []string        `json:"actions"`
	// Locked reports the final-sync lock (move_lock.go); Phase is live,
	// locked or done.
	Locked bool   `json:"locked"`
	Phase  string `json:"phase"`
	// LockEstimate is the expected duration of the final-sync lock.
	LockEstimate *MoveLockEstimate `json:"lock_estimate,omitempty"`
	// TargetServerName is the destination's display name, for pages
	// that show the move without the server list.
	TargetServerName string `json:"target_server_name,omitempty"`
}

func moveJobView(job MoveJob) moveView {
	job.Target.Secret = ""
	job.Target.Previews.Env = nil
	for i := range job.Steps {
		job.Steps[i].Title = moveStepTitle(job.Steps[i].Key, job.Steps[i].Title)
	}
	view := moveView{MoveJob: job, Records: moveRecords(job), Actions: moveActions(job), Locked: moveLocks(job), Phase: movePhase(job)}
	if !terminalMoveState(job.State) && !job.Switched {
		view.LockEstimate = moveLockEstimate(job)
	}
	return view
}

func moveRecords(job MoveJob) []MoveDNSRecord {
	out := []MoveDNSRecord{}
	for _, host := range job.Hostnames {
		for _, address := range job.Addresses {
			out = append(out, MoveDNSRecord{Hostname: host, Type: moveRecordType(address), Value: address})
		}
	}
	out = append(out, redirectRecords(job.RedirectHostnames, job.Addresses, job.Target.RedirectDomains, job.Target.Domain)...)
	return append(out, moveMailRecords(job)...)
}

// moveRecordType is the DNS record type of an address.
func moveRecordType(address string) string {
	if ip, err := netip.ParseAddr(address); err == nil && ip.Is6() {
		return "AAAA"
	}
	return "A"
}

func moveActions(job MoveJob) []string {
	actions := []string{}
	switch job.State {
	case moveStateFailed:
		actions = append(actions, "continue")
	case moveStateRunning, moveStateSyncing, moveStateFinishing, moveStateAborting:
		actions = append(actions, "continue")
	case moveStateCutover, moveStateReady:
		actions = append(actions, "recheck-dns", "finish", "auto-cutover")
	}
	if (job.State == moveStateCutover || job.State == moveStateReady) && moveCanFinalSync(job) {
		actions = append(actions, "final-sync")
	}
	if !job.Switched && !terminalMoveState(job.State) && job.State != moveStateAborting && job.State != moveStateFinishing {
		actions = append(actions, "abort")
	}
	if job.Failure != nil && job.Failure.NextRetry != nil {
		actions = append(actions, "retry-now")
	}
	if job.State == moveStateFailed && job.Failure != nil && job.Failure.Retained != nil {
		actions = append(actions, "remove-retained")
	}
	if moveHasExcludedTables(job) && !terminalMoveState(job.State) && job.State != moveStateSyncing && job.State != moveStateFinishing && !job.Switched {
		actions = append(actions, "excluded-tables")
	}
	return actions
}

func moveHasCopyableData(job MoveJob) bool {
	for _, d := range job.Databases {
		if d.Copyable {
			return true
		}
	}
	return false
}

func moveHasExcludedTables(job MoveJob) bool {
	for _, d := range job.Databases {
		if len(d.Excluded) > 0 {
			return true
		}
	}
	return false
}

func moveHasFiles(job MoveJob) bool {
	for _, f := range job.Files {
		if f.Transfer {
			return true
		}
	}
	return false
}

// moveCanFinalSync reports whether a final sync has anything to copy.
func moveCanFinalSync(job MoveJob) bool {
	return moveHasCopyableData(job) || moveHasFiles(job) || job.ClickHouse != nil
}

// ---- routes -------------------------------------------------------------------

func (c *Control) moveRoutes(handle func(string, http.HandlerFunc)) {
	handle("GET /api/control/apps/{id}/move-plan", c.movePlanRoute)
	handle("GET /api/control/apps/{id}/move", c.appMoveRoute)
	handle("POST /api/control/apps/{id}/move", c.startMoveRoute)
	handle("GET /api/control/moves/{id}", c.moveRoute)
	handle("POST /api/control/moves/{id}/{action}", c.moveActionRoute)
}

func (c *Control) movePlanRoute(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addresses := []string{}
	if raw := q.Get("addresses"); raw != "" {
		addresses = strings.Split(raw, ",")
	}
	plan, status, err := c.computeMovePlan(r, r.PathValue("id"), moveStartInput{TargetServer: q.Get("target_server"), RuntimeOverride: q.Get("runtime_override"), Addresses: addresses, ReplicationNetwork: q.Get("replication_network")})
	if err != nil {
		agent.Fail(w, status, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, plan)
}

func (c *Control) appMoveRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c.mu.Lock()
	var latest MoveJob
	found := false
	for _, job := range c.data.Moves {
		if job.AppID != id {
			continue
		}
		if !found || !terminalMoveState(job.State) && terminalMoveState(latest.State) || terminalMoveState(job.State) == terminalMoveState(latest.State) && job.Created.After(latest.Created) {
			latest, found = cloneMoveJob(job), true
		}
	}
	c.mu.Unlock()
	if !found {
		// No move yet is a normal answer, not an error: browsers log every 4xx
		// response as a console error, and the panel asks on each card expand.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, c.moveLiveView(latest))
}

func (c *Control) moveRoute(w http.ResponseWriter, r *http.Request) {
	job, ok := c.moveJob(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, c.moveLiveView(job))
}

func (c *Control) startMoveRoute(w http.ResponseWriter, r *http.Request) {
	var in moveStartInput
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	plan, status, err := c.computeMovePlan(r, r.PathValue("id"), in)
	if err != nil {
		agent.Fail(w, status, err)
		return
	}
	if len(plan.Blockers) > 0 {
		agent.JSON(w, 409, map[string]any{"error": "resolve the blockers before moving: " + strings.Join(plan.Blockers, " "), "plan": plan})
		return
	}
	if len(plan.Warnings) > 0 && !in.AcknowledgeWarnings {
		agent.JSON(w, 409, map[string]any{"error": "acknowledge the warnings to start the move", "plan": plan})
		return
	}
	job, err := c.createMove(r, plan)
	if err != nil {
		agent.Fail(w, 409, err)
		return
	}
	moveAudit(r.Context(), job, "start", false)
	c.startMoveRunner(context.WithoutCancel(r.Context()), job.ID)
	agent.JSON(w, 202, withoutActiveResume(moveJobView(job), true))
}

func (c *Control) moveActionRoute(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	var in struct {
		OverrideDNS bool   `json:"override_dns,omitempty"`
		Confirm     string `json:"confirm,omitempty"`
		// ConfirmLock accepts a final-sync lock expected to last longer
		// than 15 minutes.
		ConfirmLock bool `json:"confirm_lock,omitempty"`
		// Database and Tables choose copy or leave for excluded tables.
		Database string            `json:"database,omitempty"`
		Tables   map[string]string `json:"tables,omitempty"`
		// FinalBackup asks remove-retained to back the databases up first.
		FinalBackup bool `json:"final_backup,omitempty"`
		// Enabled switches the automatic cutover on or off.
		Enabled *bool `json:"enabled,omitempty"`
	}
	if r.ContentLength != 0 {
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
	}
	job, ok := c.moveJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	var err error
	switch action {
	case "continue":
		job, err = c.updateMove(id, func(job *MoveJob) error {
			switch job.State {
			case moveStateFailed:
				job.Error = ""
				// The operator's retry starts a new series of attempts.
				job.Failure = nil
				for i := range job.Steps {
					if job.Steps[i].Status == "failed" && job.Steps[i].Key != moveStepFinalSync {
						job.Steps[i].Status = "pending"
					}
				}
				job.State = moveStateRunning
				if job.Switched {
					job.State = moveStateFinishing
				}
			case moveStateRunning, moveStateSyncing, moveStateFinishing, moveStateAborting:
			default:
				return errors.New("this move has nothing to continue")
			}
			return nil
		})
		if err == nil {
			c.startMoveRunner(ctx, id)
		}
	case "remove-retained":
		job, err = c.moveRemoveRetainedAction(r, job, in.Confirm, in.FinalBackup)
		if err == nil {
			c.startMoveRunner(ctx, id)
		}
	case "retry-now":
		job, err = c.updateMove(id, func(job *MoveJob) error {
			if job.Failure == nil || job.Failure.NextRetry == nil {
				return errors.New("no retry is scheduled")
			}
			now := time.Now().UTC()
			job.Failure.NextRetry = &now
			return nil
		})
	case "excluded-tables":
		job, err = c.updateMove(id, func(job *MoveJob) error {
			if terminalMoveState(job.State) || job.Switched || job.State == moveStateSyncing || job.State == moveStateFinishing {
				return errors.New("choose before the final sync starts")
			}
			return moveSetExcludedChoices(job, in.Database, in.Tables)
		})
	case "auto-cutover":
		if in.Enabled == nil {
			err = errors.New("say whether the automatic cutover is enabled")
			break
		}
		job, err = c.moveSetAutoCutover(ctx, id, *in.Enabled)
	case "recheck-dns":
		if job.State != moveStateCutover && job.State != moveStateReady {
			err = errors.New("DNS is checked during the cutover")
			break
		}
		c.checkMoveDNS(r.Context(), id, true)
		job, _ = c.moveJob(id)
	case "final-sync":
		job, err = c.updateMove(id, func(job *MoveJob) error {
			if job.State != moveStateCutover && job.State != moveStateReady {
				return errors.New("the final sync runs during the cutover, before finishing")
			}
			if !moveCanFinalSync(*job) {
				return errors.New("this application has no databases or files to synchronize")
			}
			if estimate := moveLockEstimate(*job); estimate.Confirm && !in.ConfirmLock {
				return fmt.Errorf("the final sync is expected to lock the application for about %s (%s); confirm the lock to start it", moveSeconds(estimate.Seconds), estimate.Basis)
			}
			for _, d := range job.Databases {
				for _, table := range d.Excluded {
					if table.Choice == "" {
						return fmt.Errorf("choose whether to copy or leave behind the data of %s in %s first", table.Name, d.SourceID)
					}
				}
			}
			job.State = moveStateSyncing
			job.Error, job.Failure = "", nil
			// The final sync copies the frozen source anew.
			job.clearMoveCheckpoints(movePhaseFinal)
			job.setStep(moveStepFinalSync, "pending")
			return nil
		})
		if err == nil {
			c.startMoveRunner(ctx, id)
		}
	case "finish":
		job, err = c.updateMove(id, func(job *MoveJob) error {
			switch {
			case job.State == moveStateReady:
			case job.State == moveStateCutover && in.OverrideDNS && in.Confirm == job.AppID && job.TLSReady:
				job.logf(moveStepCutover, "Finishing on the operator's confirmation: HTTPS on the destination works, but DNS does not resolve to its addresses (for example behind a CDN or proxy).")
				job.setStep(moveStepCutover, "done")
			case job.State == moveStateCutover && in.OverrideDNS:
				return errors.New("confirm the application id, and wait until HTTPS works on the destination")
			default:
				return errors.New("finish after DNS for every hostname points to the destination and HTTPS works there")
			}
			job.State = moveStateFinishing
			job.Error = ""
			return nil
		})
		if err == nil {
			c.startMoveRunner(ctx, id)
		}
	case "abort":
		job, err = c.updateMove(id, func(job *MoveJob) error {
			if job.Switched || terminalMoveState(job.State) || job.State == moveStateFinishing {
				return errors.New("the move has switched to the destination and can no longer be aborted")
			}
			job.State = moveStateAborting
			job.Error = ""
			return nil
		})
		if err == nil {
			c.startMoveRunner(ctx, id)
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		agent.Fail(w, 409, err)
		return
	}
	if action == "excluded-tables" {
		moveAuditMeta(r.Context(), job, "excluded_tables", false, map[string]string{"database": in.Database})
	} else {
		moveAudit(r.Context(), job, strings.ReplaceAll(action, "-", "_"), false)
	}
	agent.JSON(w, 202, c.moveLiveView(job))
}

// ---- plan -----------------------------------------------------------------------

func moveRequestFor(ctx context.Context) *http.Request {
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1", nil)
	return r
}

// computeMovePlan inventories the source, checks the destination and
// chooses the destination runtime. It changes nothing.
func (c *Control) computeMovePlan(r *http.Request, appID string, in moveStartInput) (MovePlan, int, error) {
	plan := MovePlan{AppID: appID, TargetServer: in.TargetServer, Checked: time.Now().UTC(), RuntimeChoices: []string{}, Hostnames: []string{}, EnvironmentKeys: []string{}, CopiedKeys: []string{}, DestinationKeys: []string{},
		Databases: []MovePlanDatabase{}, Files: []MovePlanFileSet{}, Volumes: []string{}, Addresses: []string{}, Warnings: []string{}, Notes: []string{}, Blockers: []string{}}
	src, ok := c.app(appID)
	if !ok || src.Retiring {
		return plan, 404, errors.New("application not found")
	}
	if in.RuntimeOverride != "" && in.RuntimeOverride != moveRuntimeNative && in.RuntimeOverride != moveRuntimeContainer {
		return plan, 400, errors.New("runtime_override must be native or container")
	}
	if len(in.Addresses) > 8 {
		return plan, 400, errors.New("at most eight destination addresses")
	}
	if in.MaxAttempts < 0 || in.MaxAttempts > moveMaxAttempts {
		return plan, 400, fmt.Errorf("max_attempts must be between 1 and %d", moveMaxAttempts)
	}
	plan.MaxAttempts = cmp.Or(in.MaxAttempts, moveDefaultAttempts)
	if in.Replication != "" && in.Replication != moveReplicationAuto && in.Replication != moveReplicationDump {
		return plan, 400, errors.New("replication must be auto or dump")
	}
	plan.Replication = cmp.Or(in.Replication, moveReplicationAuto)
	if !slices.Contains([]string{"", moveReplicationNetworkAuto, moveReplicationPathPanel, moveReplicationPathPublic}, in.ReplicationNetwork) {
		return plan, 400, errors.New("replication_network must be auto, panel or public")
	}
	plan.ReplicationNetwork = cmp.Or(in.ReplicationNetwork, moveReplicationNetworkAuto)
	plan.SourceServer = src.ServerID
	plan.Repository, plan.Branch = src.Repository, src.Branch
	var target *Server
	var sourceServer Server
	sourceName := moveServerLabel(Server{ID: src.ServerID}, "source")
	for _, s := range c.servers() {
		if s.ID == in.TargetServer {
			copy := s
			target = &copy
		}
		if s.ID == src.ServerID {
			sourceServer = s
			sourceName = moveServerLabel(s, "source")
		}
	}
	if target == nil {
		return plan, 400, errors.New("choose a connected destination server")
	}
	targetName := moveServerLabel(*target, "destination")
	if target.ID == src.ServerID {
		return plan, 400, errors.New("the application already runs on that server")
	}
	c.mu.Lock()
	if job, active := c.activeMoveLocked(appID); active {
		plan.Blockers = append(plan.Blockers, "A move of this application is already in progress ("+job.State+").")
	}
	for _, other := range c.data.Apps {
		if other.PreviewParent == appID {
			plan.Blockers = append(plan.Blockers, "Close the application's preview environments before moving it.")
			break
		}
	}
	c.mu.Unlock()
	if src.PreviewParent != "" {
		plan.Blockers = append(plan.Blockers, "Previews are not moved; move the parent application instead.")
	}
	req := r
	var source agent.MoveSource
	if err := c.agentCall(req, src.ServerID, http.MethodGet, "/v1/move/apps/"+url.PathEscape(appID), nil, &source); err != nil {
		plan.Blockers = append(plan.Blockers, "The source server cannot describe the application for a move ("+moveAgentError(err)+"); upgrade its agent.")
		return plan, 200, nil
	}
	plan.Runtime, plan.Commit = source.Runtime, source.Commit
	plan.Hostnames = append([]string{source.Domain}, source.AdditionalDomains...)
	plan.RedirectDomains = slices.Clone(source.RedirectDomains)
	if len(plan.RedirectDomains) > 0 {
		plan.RedirectNote = fmt.Sprintf("%d redirect %s (%s) %s recreated on the destination; the cutover lists their DNS records with the application's, and they keep redirecting from the source until DNS points to the destination.",
			len(plan.RedirectDomains), pluralWord(len(plan.RedirectDomains), "domain", "domains"), strings.Join(agent.RedirectHosts(plan.RedirectDomains), ", "), pluralWord(len(plan.RedirectDomains), "is", "are"))
	}
	plan.EnvironmentKeys = source.EnvironmentKeys
	plan.Redis = source.Redis
	plan.Volumes = source.Volumes
	plan.Blockers = append(plan.Blockers, source.Blockers...)
	if source.Busy {
		plan.Blockers = append(plan.Blockers, "Wait for the active operation on the source application.")
	}
	plan.SourceRuntime = moveRuntimeContainer
	if source.Native {
		plan.SourceRuntime = moveRuntimeNative
	}

	// Destination: roles, addresses, capacity and conflicts.
	var host agent.MoveHost
	if err := c.agentCall(req, target.ID, http.MethodGet, "/v1/move/host", nil, &host); err != nil {
		plan.Blockers = append(plan.Blockers, "The destination server cannot receive moved applications ("+moveAgentError(err)+"); upgrade its agent.")
	}
	// The source's addresses: the replication path and the mail records.
	var sourceHost agent.MoveHost
	_ = c.agentCall(req, src.ServerID, http.MethodGet, "/v1/move/host", nil, &sourceHost)
	plan.Addresses = moveTargetAddresses(host, *target, in.Addresses)
	if len(in.Addresses) > 0 && len(plan.Addresses) != len(in.Addresses) {
		return plan, 400, errors.New("destination addresses must be public IPv4 or IPv6 addresses")
	}
	if len(plan.Addresses) == 0 {
		plan.Blockers = append(plan.Blockers, "The destination's public address is unknown; enter the addresses DNS should point to.")
	}
	var remote []agent.App
	if err := c.agentCall(req, target.ID, http.MethodGet, "/v1/apps", nil, &remote); err == nil {
		hosts := agent.App{Domain: source.Domain, AdditionalDomains: source.AdditionalDomains}
		redirects := agent.RedirectHosts(source.RedirectDomains)
		for _, other := range remote {
			if other.ID == appID {
				plan.Blockers = append(plan.Blockers, "The destination already has an application with this id.")
			} else if hosts.DomainsOverlap(other) {
				plan.Blockers = append(plan.Blockers, "The destination already serves a hostname of this application ("+other.ID+").")
			} else if slices.ContainsFunc(redirects, other.HasDomain) || slices.ContainsFunc(other.RedirectDomains, func(d agent.RedirectDomain) bool {
				return slices.Contains(redirects, d.Host) || hosts.HasDomain(d.Host)
			}) {
				plan.Blockers = append(plan.Blockers, "The destination already serves or redirects a redirect domain of this application ("+other.ID+").")
			}
		}
	} else {
		plan.Blockers = append(plan.Blockers, "The destination's applications could not be listed.")
	}

	// Runtime: native when the repository is native, else the source runtime.
	var detection *RepositoryDetection
	if plan.SourceRuntime == moveRuntimeContainer {
		plan.Detection = &MoveDetection{}
		installation := src.GitHubInstallation
		if det, err := c.repositoryDetection(r.Context(), src.Repository, src.Branch, "", installation); err == nil {
			detection = &det
			plan.Detection.Mode, plan.Detection.Runtime, plan.Detection.Framework, plan.Detection.Reasons = det.Mode, det.Runtime, det.Framework, det.Reasons
		} else {
			plan.Detection.Error = err.Error()
		}
	}
	nativeDetected := detection != nil && detection.Detected && detection.Mode == detect.ModeNative && nativeRuntimes[detection.Runtime] && detection.SourceDirectory == "" && src.Build.SourceDirectory == ""
	switch {
	case plan.SourceRuntime == moveRuntimeNative:
		plan.RuntimeChoices = []string{moveRuntimeNative}
		plan.TargetRuntime = moveRuntimeNative
		if in.RuntimeOverride == moveRuntimeContainer {
			plan.Blockers = append(plan.Blockers, "Native applications move to the native runtime.")
		}
	case nativeDetected:
		plan.RuntimeChoices = []string{moveRuntimeNative, moveRuntimeContainer}
		plan.TargetRuntime = moveRuntimeNative
		if in.RuntimeOverride == moveRuntimeContainer {
			plan.TargetRuntime = moveRuntimeContainer
		}
	default:
		plan.RuntimeChoices = []string{moveRuntimeContainer}
		plan.TargetRuntime = moveRuntimeContainer
		if in.RuntimeOverride == moveRuntimeNative {
			plan.Blockers = append(plan.Blockers, "The repository was not detected as a native application; it keeps the container runtime.")
		}
		if detection == nil && plan.Detection != nil {
			plan.Notes = append(plan.Notes, "Repository detection is unavailable ("+plan.Detection.Error+"); the destination keeps the container runtime.")
		}
	}
	if plan.TargetRuntime == moveRuntimeNative && !host.Native {
		plan.Blockers = append(plan.Blockers, "Native management is not configured on the destination agent.")
	}
	planMoveGateway(&plan, appID, source, host, targetName)

	// Databases.
	copyable := 0
	var sizes map[string]int64
	var sourceHealth agent.ServerHealth
	if c.agentCall(req, src.ServerID, http.MethodGet, "/v1/server-health", nil, &sourceHealth) == nil {
		sizes = sourceHealth.DatabaseBytes
	}
	needed := uint64(1 << 30)
	for _, d := range source.Databases {
		entry := MovePlanDatabase{ID: d.ID, Engine: d.Engine, Mode: d.Mode, Tables: d.Tables, SizeBytes: sizes[d.ID], Note: d.Reason, BeforeDeploy: d.BeforeDeploy.String()}
		if d.Copyable {
			entry.ExcludedTables = moveExcludedChoices(d.ExcludedTables, in.ExcludedTables[d.ID])
			if moveExcludedUnmeasured(entry.ExcludedTables) {
				var listing agent.DatabaseTables
				if c.agentCall(req, src.ServerID, http.MethodGet, "/v1/databases/"+url.PathEscape(d.ID)+"/tables", nil, &listing) == nil {
					moveFillExcludedEstimates(entry.ExcludedTables, listing)
				}
			}
			planMoveExcludedTables(&plan, entry, host)
		}
		switch {
		case d.Copyable && d.Tables == 0:
			entry.Action, entry.Note = "skip_empty", "no tables; nothing to copy"
			copyable++
		case d.Copyable:
			entry.Action = "copy"
			copyable++
			needed += 2 * uint64(max(entry.SizeBytes, 0))
			if d.Tables < 0 {
				entry.Note = "table count unknown; the database is copied"
			}
			planMoveReplication(&plan, &entry, d, sourceHost, host, plan.Replication, plan.ReplicationNetwork, sourceName, targetName)
		case d.Mode == "external":
			entry.Action = "external"
		default:
			entry.Action = "manual"
		}
		plan.Databases = append(plan.Databases, entry)
	}
	if plan.TargetRuntime == moveRuntimeNative && copyable > 1 {
		plan.Blockers = append(plan.Blockers, "The native runtime provisions one PostgreSQL database per application; move with the container runtime.")
	}
	if plan.TargetRuntime == moveRuntimeNative && (copyable > 0 || source.Redis) && !host.HostData {
		plan.Blockers = append(plan.Blockers, "The destination agent cannot provision PostgreSQL or Redis.")
	}
	needed += planMoveClickHouse(&plan, source, host)
	needed += c.planMoveFiles(&plan, source, host)
	var targetHealth agent.ServerHealth
	if c.agentCall(req, target.ID, http.MethodGet, "/v1/server-health", nil, &targetHealth) == nil {
		capacity := &MoveCapacity{MemoryAvailable: targetHealth.MemoryAvailable, Needed: needed}
		for _, disk := range targetHealth.Disks {
			capacity.DiskAvailable = max(capacity.DiskAvailable, disk.Available)
		}
		plan.Capacity = capacity
		if len(targetHealth.Disks) > 0 && capacity.DiskAvailable < needed {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("The destination has %.1f GiB of free disk space; the move needs about %.1f GiB.", float64(capacity.DiskAvailable)/(1<<30), float64(needed)/(1<<30)))
		}
		if targetHealth.MemoryTotal > 0 && targetHealth.MemoryAvailable < 256<<20 {
			plan.Warnings = append(plan.Warnings, "The destination has less than 256 MiB of memory available.")
		}
		// The destination builds the application: compare what its build
		// needs (measured on the source when known) with its memory.
		estimate := moveBuildEstimate(source, detection).Refine(source.BuildPeakMiB, sourceName)
		assessment := buildmem.Assess(estimate, buildmem.HostFromBytes(targetHealth.MemoryTotal, targetHealth.MemoryAvailable, targetHealth.SwapTotal, targetHealth.SwapFree), targetName)
		plan.BuildMemory = &assessment
		switch assessment.Level {
		case buildmem.LevelBlocker:
			plan.Blockers = append(plan.Blockers, assessment.Message)
		case buildmem.LevelWarning:
			plan.Warnings = append(plan.Warnings, assessment.Message)
		}
	} else {
		plan.Notes = append(plan.Notes, "The destination's capacity could not be measured.")
	}
	plan.Warnings = append(plan.Warnings, source.Notes...)
	if plan.TargetRuntime == moveRuntimeContainer {
		plan.Notes = append(plan.Notes, "Container deployments build the branch head; the destination may build a newer commit than the source runs.")
	}
	if source.Redis {
		plan.Notes = append(plan.Notes, "Redis is provisioned on the destination; its contents (cache and queues) are not copied.")
	}
	planMovePorts(&plan, source, host)
	if len(moveMailPortsOf(plan.PublicPorts)) > 0 && plan.TargetRuntime == moveRuntimeContainer {
		// The source's addresses are what the SPF records name today.
		c.planMoveMail(r.Context(), &plan, moveTargetAddresses(sourceHost, sourceServer, nil), source.OutgoingMail)
	}
	if copyable > 0 || source.ClickHouse != nil {
		plan.Notes = append(plan.Notes, moveFinalSyncNote(plan))
	}
	planMoveAutoCutover(&plan, in.AutoCutover)
	keys := MoveJob{TargetRuntime: plan.TargetRuntime, Redis: source.Redis, CacheLocal: source.Container != nil && source.Redis, ClickHouse: moveClickHouseFrom(source)}
	for _, d := range source.Databases {
		if d.Copyable {
			keys.Databases = append(keys.Databases, MoveDatabase{SourceID: d.ID, EnvKeys: d.EnvironmentKeys, Copyable: true})
		}
	}
	values := map[string]string{}
	for _, key := range source.EnvironmentKeys {
		values[key] = ""
	}
	_, _, report := moveEnvironmentChanges(keys, agent.MoveEnvironment{Values: values, Sources: source.EnvironmentSources}, nil)
	plan.CopiedKeys = report.copied
	if moveSourceSummary(report.sources) != "" {
		plan.CopiedSources = report.sources
	}
	plan.DestinationKeys = append(append(slices.Clone(report.owned), report.mapped...), report.rebuilt...)
	slices.Sort(plan.DestinationKeys)
	slices.Sort(plan.Blockers)
	plan.Blockers = slices.Compact(plan.Blockers)
	estimate := c.movePlanEstimate(r.Context(), plan, source, host)
	plan.Estimate = &estimate
	return plan, 200, nil
}

func moveAgentError(err error) string {
	var remote *agentHTTPError
	if errors.As(err, &remote) {
		if remote.status == http.StatusNotFound && remote.message == "" {
			return "the agent does not support moves"
		}
		if remote.message != "" {
			return remote.message
		}
		return fmt.Sprintf("HTTP %d", remote.status)
	}
	return err.Error()
}

// moveTargetAddresses picks the addresses DNS must point to: the operator's,
// else what the destination agent detects, else its URL's IP literal.
func moveTargetAddresses(host agent.MoveHost, server Server, explicit []string) []string {
	out := []string{}
	add := func(raw string) {
		ip, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
			return
		}
		value := ip.Unmap().String()
		if !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	if len(explicit) > 0 {
		for _, raw := range explicit {
			add(raw)
		}
		return out
	}
	add(host.IPv4)
	add(host.IPv6)
	if len(out) == 0 {
		if u, err := url.Parse(server.URL); err == nil {
			add(u.Hostname())
		}
	}
	return out
}

// createMove records a new job from a reviewed plan.
func (c *Control) createMove(r *http.Request, plan MovePlan) (MoveJob, error) {
	src, ok := c.app(plan.AppID)
	if !ok {
		return MoveJob{}, errors.New("application not found")
	}
	// Destination deploys mint checkout credentials for the destination
	// record; resolve the repository's installation once, here.
	if resolved, err := c.resolveApplicationInstallation(r.Context(), src); err == nil {
		src = resolved
	}
	var source agent.MoveSource
	if err := c.agentCall(r, src.ServerID, http.MethodGet, "/v1/move/apps/"+url.PathEscape(src.ID), nil, &source); err != nil {
		return MoveJob{}, err
	}
	now := time.Now().UTC()
	job := MoveJob{ID: random()[:24], AppID: src.ID, SourceServer: src.ServerID, TargetServer: plan.TargetServer, SourceRuntime: plan.SourceRuntime, TargetRuntime: plan.TargetRuntime,
		Runtime: plan.Runtime, State: moveStateRunning, Plan: plan, Commit: plan.Commit, Hostnames: slices.Clone(plan.Hostnames), RedirectHostnames: agent.RedirectHosts(plan.RedirectDomains), Addresses: slices.Clone(plan.Addresses),
		Redis: source.Redis, CacheLocal: source.Container != nil && source.Redis, DNS: []MoveDNSStatus{}, Databases: []MoveDatabase{}, Created: now, Updated: now}
	job.MaxAttempts = plan.MaxAttempts
	job.AutoCutover = plan.AutoCutover
	for _, d := range source.Databases {
		entry := MoveDatabase{SourceID: d.ID, EnvKeys: slices.Clone(d.EnvironmentKeys), Mode: d.Mode, Copyable: d.Copyable, Backup: d.Backup, BeforeDeploy: d.BeforeDeploy}
		for _, p := range plan.Databases {
			if p.ID == d.ID && p.Engine != moveEngineClickHouse && d.Copyable {
				entry.Excluded = slices.Clone(p.ExcludedTables)
				entry.Replication, entry.ReplicationReason, entry.ReplicationRestart, entry.ReplicationVia = p.Replication, p.ReplicationReason, p.ReplicationRestart, p.ReplicationVia
				if d.Replication != nil {
					entry.ReplicationExcluded = slices.Clone(d.Replication.NoIdentity)
				}
			}
		}
		job.Databases = append(job.Databases, entry)
	}
	job.ClickHouse = moveClickHouseFrom(source)
	for _, f := range plan.Files {
		job.Files = append(job.Files, MoveFileCopy{ID: f.ID, Path: f.Path, Source: f.Source, Files: f.Files, Bytes: f.Bytes, Method: f.Method, Transfer: f.Action == "copy" || f.Action == "skip_empty"})
	}
	job.Target = Application{ID: src.ID, ServerID: plan.TargetServer, Repository: src.Repository, Branch: src.Branch, Domain: source.Domain, AdditionalDomains: slices.Clone(source.AdditionalDomains),
		RedirectDomains: slices.Clone(plan.RedirectDomains), CustomerHosts: src.CustomerHosts, GitHubInstallation: src.GitHubInstallation, Generation: random()}
	if plan.TargetRuntime == moveRuntimeNative {
		spec, err := c.moveNativeSpec(r.Context(), src, source, plan)
		if err != nil {
			return MoveJob{}, err
		}
		job.NativeSpec = &spec
		job.Target.Runtime = spec.Runtime
	} else {
		if source.Container == nil {
			return MoveJob{}, errors.New("the source application cannot be recreated as a container")
		}
		spec := *source.Container
		spec.Env, spec.EnvKinds = nil, nil
		job.ContainerSpec = &spec
		job.Target.Runtime, job.Target.Build, job.Target.Astro, job.Target.Rails, job.Target.RuntimeProfile = spec.Runtime, spec.Build, spec.Astro, spec.Rails, spec.RuntimeProfile
	}
	for _, s := range moveStepTitles {
		status := "pending"
		if s.key == moveStepPlan {
			status = "done"
		}
		if s.key == moveStepFinalSync {
			status = "skipped"
		}
		job.Steps = append(job.Steps, MoveStep{Key: s.key, Title: s.title, Status: status, Log: []MoveLogLine{}})
	}
	job.setStep(moveStepPlan, "done")
	job.logf(moveStepPlan, "Moving %s from %s to %s as a %s application (%s).", src.ID, src.ServerID, plan.TargetServer, plan.TargetRuntime, plan.Runtime)
	for _, warning := range plan.Warnings {
		job.logf(moveStepPlan, "Acknowledged: %s", warning)
	}
	for _, d := range job.Databases {
		if copied, left := moveExcludedNames(d, moveTableCopy), moveExcludedNames(d, moveTableLeave); len(copied)+len(left) > 0 {
			job.logf(moveStepPlan, "Tables of %s whose data the backups leave out: copied separately %s; left behind (schema only) %s.", d.SourceID, moveKeyList(copied), moveKeyList(left))
		}
		if d.Copyable && d.Replication != "" {
			job.logf(moveStepPlan, "Database %s: %s.", d.SourceID, moveReplicationSummary(d.Replication, d.ReplicationReason, d.ReplicationVia))
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, active := c.activeMoveLocked(src.ID); active {
		return MoveJob{}, errors.New("a move of this application is already in progress")
	}
	current, ok := c.data.Apps[src.ID]
	if !ok || current.Retiring || current.ServerID != src.ServerID {
		return MoveJob{}, errors.New("the application changed while the move was planned; review the plan again")
	}
	if c.data.Moves == nil {
		c.data.Moves = map[string]MoveJob{}
	}
	c.pruneMovesLocked()
	c.data.Moves[job.ID] = job
	if err := c.save(); err != nil {
		delete(c.data.Moves, job.ID)
		return MoveJob{}, err
	}
	return cloneMoveJob(job), nil
}

// pruneMovesLocked keeps the most recent finished and aborted moves.
func (c *Control) pruneMovesLocked() {
	done := []MoveJob{}
	for _, job := range c.data.Moves {
		if terminalMoveState(job.State) {
			done = append(done, job)
		}
	}
	if len(done) < moveKeepJobs {
		return
	}
	slices.SortFunc(done, func(a, b MoveJob) int { return a.Updated.Compare(b.Updated) })
	for _, job := range done[:len(done)-moveKeepJobs+1] {
		delete(c.data.Moves, job.ID)
	}
}

// moveNativeSpec is the destination's creation request: the source's
// recorded request for native applications, detection otherwise.
func (c *Control) moveNativeSpec(ctx context.Context, src Application, source agent.MoveSource, plan MovePlan) (agent.NativeCreateRequest, error) {
	hasDatabase := false
	backupHours, offsite := 0, false
	for _, d := range source.Databases {
		if d.Copyable {
			hasDatabase = true
			backupHours, offsite = d.Backup.Hours, d.Backup.Offsite
		}
	}
	if backupHours != 1 && backupHours != 24 && backupHours != 168 {
		backupHours = 24
	}
	var spec agent.NativeCreateRequest
	if source.CreateSpec != nil {
		spec = *source.CreateSpec
	} else {
		det, err := c.repositoryDetection(ctx, src.Repository, src.Branch, "", src.GitHubInstallation)
		if err != nil {
			return spec, err
		}
		spec = agent.NativeCreateRequest{Runtime: det.Runtime, HealthPath: det.HealthPath}
		if det.Runtime == detect.RuntimeStatic {
			spec.StaticOutput = det.StaticOutput
		}
		for _, p := range det.Processes {
			spec.Processes = append(spec.Processes, agent.NativeProcessSpec{Name: p.Name, Command: p.Command, Web: p.Web})
		}
	}
	if source.CreateSpec != nil && !source.SpecRecorded && len(spec.Processes) == 0 && spec.Runtime != "static" {
		if det, err := c.repositoryDetection(ctx, src.Repository, src.Branch, "", src.GitHubInstallation); err == nil && det.Runtime == spec.Runtime {
			for _, p := range det.Processes {
				spec.Processes = append(spec.Processes, agent.NativeProcessSpec{Name: p.Name, Command: p.Command, Web: p.Web})
			}
		}
	}
	spec.ID, spec.Repository, spec.Branch, spec.Domain, spec.AdditionalDomains = src.ID, src.Repository, src.Branch, source.Domain, slices.Clone(source.AdditionalDomains)
	spec.Env, spec.EnvKinds, spec.SourceDirectory, spec.AutoDeploy = nil, nil, "", false
	spec.Data = agent.NativeDataRequest{Postgres: hasDatabase, Redis: source.Redis, BackupHours: backupHours, Offsite: offsite}
	if ch := moveClickHouseFrom(source); ch != nil {
		// The destination installs the source's release series when it has
		// no ClickHouse yet, so the database moves as a native BACKUP.
		spec.Data.ClickHouse, spec.Data.ClickHouseVersion = true, moveClickHouseSeries(ch.SourceVersion)
	}
	if !nativeRuntimes[spec.Runtime] {
		return spec, fmt.Errorf("the %s runtime cannot be created natively", spec.Runtime)
	}
	return spec, nil
}

// moveBuildEstimate estimates the build memory of a moving application:
// from repository detection when it ran, otherwise from its runtime.
func moveBuildEstimate(source agent.MoveSource, detection *RepositoryDetection) buildmem.Estimate {
	if detection != nil && detection.BuildMemory != nil {
		return *detection.BuildMemory
	}
	stack := buildmem.Stack{Mode: detect.ModeNative, Runtime: source.Runtime, Framework: source.Runtime}
	switch source.Runtime {
	case "astro":
		stack.Runtime, stack.Frontend = detect.RuntimeStatic, true
	case "node":
		stack.Frontend = true
	case "rails":
		stack.Assets = true
	case "go", "static":
	default:
		stack = buildmem.Stack{Mode: detect.ModeContainer}
	}
	return buildmem.EstimateStack(stack)
}

// planMoveGateway checks that the destination can serve a protected release
// (gateway_transport): through its host gateway, or one its agent sets up
// at the first deployment.
func planMoveGateway(plan *MovePlan, appID string, source agent.MoveSource, host agent.MoveHost, targetName string) {
	if !source.Protected || plan.TargetRuntime != moveRuntimeContainer {
		return
	}
	switch host.Gateway {
	case "configured":
	case "auto":
		plan.Notes = append(plan.Notes, "The destination sets up the protected gateway for "+appID+" at the first deploy.")
	case "unavailable":
		plan.Blockers = append(plan.Blockers, appID+" uses a protected gateway (gateway_transport), and "+targetName+" has neither a host gateway nor a Caddy admin API its agent can publish routes to; run Caddy with its admin API on 127.0.0.1:2019 there, or configure a host gateway in its agent configuration.")
	default:
		plan.Blockers = append(plan.Blockers, appID+" uses a protected gateway (gateway_transport), which the agent on "+targetName+" cannot set up; upgrade its agent.")
	}
}
