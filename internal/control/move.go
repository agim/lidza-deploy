package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/detect"
	"github.com/agim/lidza/packs/audit"
	"github.com/agim/lidza/packs/auth"
)

// Moving an application to another server.
//
// A move is a job in control state with one record per step. The source
// application stays the live record (c.data.Apps) and keeps serving until
// the operator finishes the move; the job holds the destination side under
// the same application id. DNS is switched by hand: the move pauses at
// cutover with the records to set and continues once every hostname resolves
// to the destination. Every step is idempotent, so a failed or interrupted
// step (including a control panel restart) is retried from its start.
//
//   POST /api/control/apps/{id}/move          start {target_server, runtime_override?}
//   GET  /api/control/apps/{id}/move-plan     review before starting
//   GET  /api/control/apps/{id}/move          the application's latest move
//   GET  /api/control/moves/{id}              one move
//   POST /api/control/moves/{id}/{action}     continue, recheck-dns, final-sync, finish, abort

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
	{moveStepDeploy, "Deploy on destination"},
	{moveStepCutover, "Switch DNS"},
	{moveStepFinalSync, "Final data sync (optional)"},
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
	Redis         bool                       `json:"redis"`
	CacheLocal    bool                       `json:"cache_local,omitempty"`
	Commit        string                     `json:"commit,omitempty"`
	Hostnames     []string                   `json:"hostnames"`
	Addresses     []string                   `json:"addresses"`
	DNS           []MoveDNSStatus            `json:"dns"`
	DNSChecked    *time.Time                 `json:"dns_checked,omitempty"`
	TLSReady      bool                       `json:"tls_ready"`
	TLSDetail     string                     `json:"tls_detail,omitempty"`
	TargetCreated bool                       `json:"target_created"`
	Deployment    string                     `json:"deployment,omitempty"`
	DeployAttempt int                        `json:"deploy_attempt,omitempty"`
	SourceFrozen  bool                       `json:"source_frozen,omitempty"`
	FinalSynced   bool                       `json:"final_synced,omitempty"`
	Switched      bool                       `json:"switched"`
	Created       time.Time                  `json:"created"`
	Updated       time.Time                  `json:"updated"`
}

// MoveStep is one step's status and log.
type MoveStep struct {
	Key      string        `json:"key"`
	Title    string        `json:"title"`
	Status   string        `json:"status"` // pending, running, waiting, done, skipped, failed
	Started  *time.Time    `json:"started,omitempty"`
	Finished *time.Time    `json:"finished,omitempty"`
	Log      []MoveLogLine `json:"log"`
}

type MoveLogLine struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

// MoveDatabase is one database being copied.
type MoveDatabase struct {
	SourceID string             `json:"source_id"`
	TargetID string             `json:"target_id,omitempty"`
	EnvKeys  []string           `json:"env_keys"`
	Mode     string             `json:"mode"`
	Copyable bool               `json:"copyable"`
	Backup   agent.BackupPolicy `json:"backup"`
	Copied   string             `json:"copied_sha256,omitempty"`
	Bytes    int64              `json:"bytes,omitempty"`
	Skipped  string             `json:"skipped,omitempty"`
}

// MoveDNSStatus is the last observed resolution of one hostname.
type MoveDNSStatus struct {
	Hostname  string   `json:"hostname"`
	Addresses []string `json:"addresses"`
	Ready     bool     `json:"ready"`
	Error     string   `json:"error,omitempty"`
}

// MoveDNSRecord is one record the operator sets at their DNS provider.
type MoveDNSRecord struct {
	Hostname string `json:"hostname"`
	Type     string `json:"type"`
	Value    string `json:"value"`
}

// MovePlan is reviewed before a move starts.
type MovePlan struct {
	AppID           string             `json:"app_id"`
	SourceServer    string             `json:"source_server"`
	TargetServer    string             `json:"target_server"`
	SourceRuntime   string             `json:"source_runtime"`
	Runtime         string             `json:"runtime"`
	TargetRuntime   string             `json:"target_runtime"`
	RuntimeChoices  []string           `json:"runtime_choices"`
	Detection       *MoveDetection     `json:"detection,omitempty"`
	Repository      string             `json:"repository"`
	Branch          string             `json:"branch"`
	Commit          string             `json:"commit,omitempty"`
	Hostnames       []string           `json:"hostnames"`
	EnvironmentKeys []string           `json:"environment_keys"`
	CopiedKeys      []string           `json:"copied_keys"`
	DestinationKeys []string           `json:"destination_keys"`
	Databases       []MovePlanDatabase `json:"databases"`
	Redis           bool               `json:"redis"`
	Volumes         []string           `json:"volumes"`
	Addresses       []string           `json:"addresses"`
	Capacity        *MoveCapacity      `json:"capacity,omitempty"`
	Warnings        []string           `json:"warnings"`
	Notes           []string           `json:"notes"`
	Blockers        []string           `json:"blockers"`
	Checked         time.Time          `json:"checked"`
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
}

// moveResolver resolves hostnames; tests replace it.
type moveResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

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
	job.Steps = slices.Clone(job.Steps)
	for i := range job.Steps {
		job.Steps[i].Log = slices.Clone(job.Steps[i].Log)
	}
	job.Databases = slices.Clone(job.Databases)
	job.DNS = slices.Clone(job.DNS)
	job.Hostnames = slices.Clone(job.Hostnames)
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
		s.Started, s.Finished = &now, nil
	case "done", "skipped", "failed":
		s.Finished = &now
	}
	s.Status = status
}

func (c *Control) moveLog(id, step, format string, args ...any) {
	_, _ = c.updateMove(id, func(job *MoveJob) error { job.logf(step, format, args...); return nil })
}

// moveAudit records a move transition when the audit pack is available.
func moveAudit(ctx context.Context, job MoveJob, action string, failed bool) {
	log, ok := lidza.Optional[*audit.Audit](ctx)
	if !ok || log == nil {
		return
	}
	e := audit.Event{Action: "move." + action, Scope: fleetScope, Resource: "app/" + job.AppID, Meta: map[string]string{"move": job.ID, "source": job.SourceServer, "target": job.TargetServer}}
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
}

func moveJobView(job MoveJob) moveView {
	job.Target.Secret = ""
	job.Target.Previews.Env = nil
	view := moveView{MoveJob: job, Records: moveRecords(job), Actions: moveActions(job)}
	return view
}

func moveRecords(job MoveJob) []MoveDNSRecord {
	out := []MoveDNSRecord{}
	for _, host := range job.Hostnames {
		for _, address := range job.Addresses {
			kind := "A"
			if ip, err := netip.ParseAddr(address); err == nil && ip.Is6() {
				kind = "AAAA"
			}
			out = append(out, MoveDNSRecord{Hostname: host, Type: kind, Value: address})
		}
	}
	return out
}

func moveActions(job MoveJob) []string {
	actions := []string{}
	switch job.State {
	case moveStateFailed:
		actions = append(actions, "continue")
	case moveStateRunning, moveStateSyncing, moveStateFinishing, moveStateAborting:
		actions = append(actions, "continue")
	case moveStateCutover:
		actions = append(actions, "recheck-dns", "finish")
	case moveStateReady:
		actions = append(actions, "recheck-dns", "finish")
	}
	if (job.State == moveStateCutover || job.State == moveStateReady) && moveHasCopyableData(job) {
		actions = append(actions, "final-sync")
	}
	if !job.Switched && !terminalMoveState(job.State) && job.State != moveStateAborting && job.State != moveStateFinishing {
		actions = append(actions, "abort")
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
	plan, status, err := c.computeMovePlan(r, r.PathValue("id"), moveStartInput{TargetServer: q.Get("target_server"), RuntimeOverride: q.Get("runtime_override"), Addresses: addresses})
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
		agent.Fail(w, 404, errors.New("no move recorded for this application"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, moveJobView(latest))
}

func (c *Control) moveRoute(w http.ResponseWriter, r *http.Request) {
	job, ok := c.moveJob(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, moveJobView(job))
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
	agent.JSON(w, 202, moveJobView(job))
}

func (c *Control) moveActionRoute(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	var in struct {
		OverrideDNS bool   `json:"override_dns,omitempty"`
		Confirm     string `json:"confirm,omitempty"`
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
			if !moveHasCopyableData(*job) {
				return errors.New("this application has no database to synchronize")
			}
			job.State = moveStateSyncing
			job.Error = ""
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
	moveAudit(r.Context(), job, strings.ReplaceAll(action, "-", "_"), false)
	agent.JSON(w, 202, moveJobView(job))
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
		Databases: []MovePlanDatabase{}, Volumes: []string{}, Addresses: []string{}, Warnings: []string{}, Notes: []string{}, Blockers: []string{}}
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
	plan.SourceServer = src.ServerID
	plan.Repository, plan.Branch = src.Repository, src.Branch
	var target *Server
	for _, s := range c.servers() {
		if s.ID == in.TargetServer {
			copy := s
			target = &copy
		}
	}
	if target == nil {
		return plan, 400, errors.New("choose a connected destination server")
	}
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
		for _, other := range remote {
			if other.ID == appID {
				plan.Blockers = append(plan.Blockers, "The destination already has an application with this id.")
			} else if hosts.DomainsOverlap(other) {
				plan.Blockers = append(plan.Blockers, "The destination already serves a hostname of this application ("+other.ID+").")
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
	if plan.TargetRuntime == moveRuntimeContainer && host.NativeOnly {
		plan.Blockers = append(plan.Blockers, "The destination agent manages native services only.")
	}

	// Databases.
	copyable := 0
	var sizes map[string]int64
	var sourceHealth agent.ServerHealth
	if c.agentCall(req, src.ServerID, http.MethodGet, "/v1/server-health", nil, &sourceHealth) == nil {
		sizes = sourceHealth.DatabaseBytes
	}
	needed := uint64(1 << 30)
	for _, d := range source.Databases {
		entry := MovePlanDatabase{ID: d.ID, Engine: d.Engine, Mode: d.Mode, Tables: d.Tables, SizeBytes: sizes[d.ID], Note: d.Reason}
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
	} else {
		plan.Notes = append(plan.Notes, "The destination's capacity could not be measured.")
	}
	for _, volume := range source.Volumes {
		plan.Warnings = append(plan.Warnings, "Files are not moved automatically: "+volume+". Copy them to the destination before finishing.")
	}
	plan.Warnings = append(plan.Warnings, source.Notes...)
	if plan.TargetRuntime == moveRuntimeContainer {
		plan.Notes = append(plan.Notes, "Container deployments build the branch head; the destination may build a newer commit than the source runs.")
	}
	if source.Redis {
		plan.Notes = append(plan.Notes, "Redis is provisioned on the destination; its contents (cache and queues) are not copied.")
	}
	if copyable > 0 {
		plan.Notes = append(plan.Notes, "Writes made on the source after the data copy are not on the destination. Use the final sync during the cutover to copy them with the source in maintenance.")
	}
	keys := MoveJob{TargetRuntime: plan.TargetRuntime, Redis: source.Redis, CacheLocal: source.Container != nil && source.Redis}
	for _, d := range source.Databases {
		if d.Copyable {
			keys.Databases = append(keys.Databases, MoveDatabase{SourceID: d.ID, EnvKeys: d.EnvironmentKeys, Copyable: true})
		}
	}
	values := map[string]string{}
	for _, key := range source.EnvironmentKeys {
		values[key] = ""
	}
	_, _, report := moveEnvironmentChanges(keys, agent.MoveEnvironment{Values: values}, nil)
	plan.CopiedKeys = report.copied
	plan.DestinationKeys = append(slices.Clone(report.owned), report.mapped...)
	slices.Sort(plan.DestinationKeys)
	slices.Sort(plan.Blockers)
	plan.Blockers = slices.Compact(plan.Blockers)
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
		Runtime: plan.Runtime, State: moveStateRunning, Plan: plan, Commit: plan.Commit, Hostnames: slices.Clone(plan.Hostnames), Addresses: slices.Clone(plan.Addresses),
		Redis: source.Redis, CacheLocal: source.Container != nil && source.Redis, DNS: []MoveDNSStatus{}, Databases: []MoveDatabase{}, Created: now, Updated: now}
	for _, d := range source.Databases {
		job.Databases = append(job.Databases, MoveDatabase{SourceID: d.ID, EnvKeys: slices.Clone(d.EnvironmentKeys), Mode: d.Mode, Copyable: d.Copyable, Backup: d.Backup})
	}
	job.Target = Application{ID: src.ID, ServerID: plan.TargetServer, Repository: src.Repository, Branch: src.Branch, Domain: source.Domain, AdditionalDomains: slices.Clone(source.AdditionalDomains),
		CustomerHosts: src.CustomerHosts, GitHubInstallation: src.GitHubInstallation, Generation: random()}
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
	if !nativeRuntimes[spec.Runtime] {
		return spec, fmt.Errorf("the %s runtime cannot be created natively", spec.Runtime)
	}
	return spec, nil
}
