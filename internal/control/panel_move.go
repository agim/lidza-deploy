package control

// Moving the control panel to another server (Settings → Control panel).
//
// The panel that moves drives everything through the agents it already
// reaches (no new port, no SSH):
//
//  1. prepare: the destination's agent installs the panel binary from its
//     own verified bundle and starts it in standby (it serves a standby page
//     and runs nothing); the panel's data directory, rewritten for the
//     destination, and its database are copied; the hostname's certificate is
//     copied from this host's Caddy storage when its agent can read it; the
//     hostname is published on the destination's ingress.
//  2. cutover (the operator's button): this panel becomes read-only (every
//     mutating route answers 503), waits for running jobs, copies its data
//     and database once more and switches the destination to active.
//  3. DNS: this panel shows the records to set and checks them with the
//     authoritative resolver app moves use. Once the hostname points to the
//     destination and HTTPS answers there, it marks itself moved (it then
//     serves only a notice) and asks its own agent to replace its Caddy site
//     and stop its unit.
//
// Abort is possible until the cutover and leaves nothing on the destination.
// Steps are checkpointed in the encrypted state; a restarted panel resumes.
//
// Reaching the old host from the new panel: the destination becomes the
// panel's local server (its loopback agent address). The old host is kept
// only when it still holds applications and the operator gives an agent URL
// the destination reaches (probed from the destination); otherwise it leaves
// the registry and is listed for retirement. Other servers keep their URLs
// when the destination reaches them, else the operator gives one.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/buildinfo"
)

const (
	panelMoveRunning  = "running"  // a prepare or cutover step runs
	panelMoveStandby  = "standby"  // prepared; waiting for the operator's cutover
	panelMoveFailed   = "failed"   // a step failed after its retries
	panelMoveDNS      = "dns"      // the destination is active; waiting for DNS
	panelMoveDone     = "done"     // handed over
	panelMoveAborting = "aborting" // removing what the destination got
	panelMoveAborted  = "aborted"

	panelPhasePrepare = "prepare"
	panelPhaseCutover = "cutover"

	panelStepCheck    = "check"
	panelStepBundle   = "bundle"
	panelStepInstall  = "install"
	panelStepDatabase = "database"
	panelStepCert     = "certificate"
	panelStepRoute    = "route"
	panelStepFreeze   = "freeze"
	panelStepFinal    = "final-copy"
	panelStepActivate = "activate"
	panelStepDNS      = "dns"
	panelStepRetire   = "retire"

	panelMoveMaxLog = 200
)

var panelStepTitles = map[string]string{
	panelStepCheck: "Check the destination", panelStepBundle: "Panel binary", panelStepInstall: "Install in standby",
	panelStepDatabase: "Copy the database", panelStepCert: "Copy the certificate", panelStepRoute: "Publish the hostname",
	panelStepFreeze: "Read-only", panelStepFinal: "Final copy", panelStepActivate: "Activate the destination",
	panelStepDNS: "DNS and HTTPS", panelStepRetire: "Retire this panel",
}

var (
	panelPrepareSteps = []string{panelStepCheck, panelStepBundle, panelStepInstall, panelStepDatabase, panelStepCert, panelStepRoute}
	panelCutoverSteps = []string{panelStepFreeze, panelStepFinal, panelStepActivate}
)

// PanelMoveInput is what the operator chooses.
type PanelMoveInput struct {
	Target string `json:"target"`
	// SourceURL is an agent URL of this host the destination reaches; only
	// needed while this host still holds applications.
	SourceURL string `json:"source_url,omitempty"`
	// ServerURLs replace other servers' agent URLs for the new panel.
	ServerURLs map[string]string `json:"server_urls,omitempty"`
	Addresses  []string          `json:"addresses,omitempty"`
}

// PanelMoveServer is one registry entry as the new panel will see it.
type PanelMoveServer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Role      string `json:"role"` // destination, source, remote
	URL       string `json:"url"`
	NewURL    string `json:"new_url,omitempty"`
	Reachable bool   `json:"reachable"`
	Retire    bool   `json:"retire,omitempty"`
	Apps      int    `json:"apps"`
	Detail    string `json:"detail,omitempty"`
}

type PanelMoveCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// PanelMovePlan is what the move will do; it changes nothing.
type PanelMovePlan struct {
	Input         PanelMoveInput    `json:"input"`
	Hostname      string            `json:"hostname"`
	TargetName    string            `json:"target_name"`
	SourceServer  string            `json:"source_server,omitempty"`
	SourceName    string            `json:"source_name,omitempty"`
	Servers       []PanelMoveServer `json:"servers"`
	Addresses     []string          `json:"addresses"`
	DataFiles     int               `json:"data_files"`
	DataBytes     int64             `json:"data_bytes"`
	Database      string            `json:"database"` // managed or external
	DatabaseBytes int64             `json:"database_bytes"`
	Ingress       string            `json:"ingress"`
	IngressDetail string            `json:"ingress_detail"`
	Certificate   string            `json:"certificate"`
	CertificateOK bool              `json:"certificate_ok"`
	Version       string            `json:"version"`
	Checks        []PanelMoveCheck  `json:"checks"`
	Blockers      []string          `json:"blockers"`
	Warnings      []string          `json:"warnings"`
	Ready         bool              `json:"ready"`
	// agentURL and token stay on the server.
	agentURL string
}

type PanelMoveStep struct {
	Name     string     `json:"name"`
	Title    string     `json:"title"`
	Status   string     `json:"status"` // pending, running, done, failed, skipped, waiting
	Detail   string     `json:"detail,omitempty"`
	Attempts int        `json:"attempts,omitempty"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
}

type PanelMoveLog struct {
	At      time.Time `json:"at"`
	Step    string    `json:"step,omitempty"`
	Message string    `json:"message"`
}

// PanelMove is the move of this control panel.
type PanelMove struct {
	ID           string          `json:"id"`
	Target       string          `json:"target"`
	TargetName   string          `json:"target_name"`
	Source       string          `json:"source,omitempty"`
	Hostname     string          `json:"hostname"`
	State        string          `json:"state"`
	Phase        string          `json:"phase"`
	ReadOnly     bool            `json:"read_only"`
	Plan         PanelMovePlan   `json:"plan"`
	AgentURL     string          `json:"agent_url"`
	Addresses    []string        `json:"addresses"`
	Steps        []PanelMoveStep `json:"steps"`
	Log          []PanelMoveLog  `json:"log"`
	Error        string          `json:"error,omitempty"`
	Certificate  string          `json:"certificate,omitempty"`
	TLSSubject   bool            `json:"tls_subject,omitempty"`
	DNS          []MoveDNSStatus `json:"dns,omitempty"`
	DNSChecked   *time.Time      `json:"dns_checked,omitempty"`
	TLSReady     bool            `json:"tls_ready,omitempty"`
	TLSDetail    string          `json:"tls_detail,omitempty"`
	Created      time.Time       `json:"created"`
	Updated      time.Time       `json:"updated"`
	Finished     *time.Time      `json:"finished,omitempty"`
	FinalCopied  *time.Time      `json:"final_copied,omitempty"`
	RetireDetail []string        `json:"retire_detail,omitempty"`
}

// PanelArrival is what a moved-in panel remembers about its move.
type PanelArrival struct {
	From     string    `json:"from"`
	At       time.Time `json:"at"`
	Hostname string    `json:"hostname"`
	// Retired are servers that left the registry with the move; shut them
	// down once nothing else needs them.
	Retired []PanelMoveServer `json:"retired,omitempty"`
}

func (m *PanelMove) step(name string) *PanelMoveStep {
	for i := range m.Steps {
		if m.Steps[i].Name == name {
			return &m.Steps[i]
		}
	}
	return nil
}

func (m *PanelMove) setStep(name, status, detail string) {
	s := m.step(name)
	if s == nil {
		return
	}
	now := time.Now().UTC()
	switch status {
	case "running":
		if s.Started == nil {
			s.Started = &now
		}
		s.Finished = nil
	case "done", "failed", "skipped":
		s.Finished = &now
	}
	s.Status = status
	if detail != "" || status == "running" {
		s.Detail = detail
	}
}

func (m *PanelMove) logf(step, format string, args ...any) {
	m.Log = append(m.Log, PanelMoveLog{At: time.Now().UTC(), Step: step, Message: moveBounded(fmt.Sprintf(format, args...), 600)})
	if len(m.Log) > panelMoveMaxLog {
		m.Log = m.Log[len(m.Log)-panelMoveMaxLog:]
	}
}

func panelMoveActive(m *PanelMove) bool {
	return m != nil && m.State != panelMoveDone && m.State != panelMoveAborted
}

// ---- state access -------------------------------------------------------------------

func (c *Control) panelMove() (PanelMove, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.PanelMove == nil {
		return PanelMove{}, false
	}
	var out PanelMove
	raw, _ := json.Marshal(c.data.PanelMove)
	_ = json.Unmarshal(raw, &out)
	return out, true
}

func (c *Control) updatePanelMove(fn func(*PanelMove) error) (PanelMove, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.PanelMove == nil {
		return PanelMove{}, errors.New("no control panel move")
	}
	raw, _ := json.Marshal(c.data.PanelMove)
	var next PanelMove
	_ = json.Unmarshal(raw, &next)
	if err := fn(&next); err != nil {
		return PanelMove{}, err
	}
	next.Updated = time.Now().UTC()
	old := c.data.PanelMove
	c.data.PanelMove = &next
	if err := c.save(); err != nil {
		c.data.PanelMove = old
		return PanelMove{}, err
	}
	return next, nil
}

// panelReadOnly reports that this panel no longer accepts changes: its
// cutover started, or it handed over.
func (c *Control) panelReadOnly() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.PanelMove != nil && c.data.PanelMove.ReadOnly
}

// ticksAllowed gates the panel's scheduled work: a standby panel runs none
// and a read-only (moving or moved) panel must not change the fleet.
func (c *Control) ticksAllowed() bool {
	return !c.cfg.Standby && !c.panelReadOnly()
}

// gatedJob wraps a scheduled handler: skipped while ticks are not allowed,
// counted while it runs so the cutover can wait for it.
func (c *Control) gatedJob(scheduled bool, next func(context.Context, json.RawMessage) error) func(context.Context, json.RawMessage) error {
	return func(ctx context.Context, payload json.RawMessage) error {
		if !c.ticksAllowed() {
			if scheduled {
				return nil
			}
			return errors.New("the control panel is moving; the job runs on the new server")
		}
		c.panelJobs.Add(1)
		defer c.panelJobs.Add(-1)
		return next(ctx, payload)
	}
}

// panelMovingError is the read-only answer.
var panelMovingError = errors.New("the control panel is moving")

// panelReadOnlyGuard refuses every mutating route while the panel is read
// only, except the move's own.
func (c *Control) panelReadOnlyGuard(pattern string, next http.HandlerFunc) http.HandlerFunc {
	if panelMoveRoute(pattern) {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && c.panelReadOnly() {
			w.Header().Set("Retry-After", "300")
			agent.Fail(w, http.StatusServiceUnavailable, panelMovingError)
			return
		}
		next(w, r)
	}
}

func panelMoveRoute(pattern string) bool {
	_, path, _ := strings.Cut(pattern, " ")
	return path == "/api/control/panel-move" || strings.HasPrefix(path, "/api/control/panel-move/")
}

// ---- routes ------------------------------------------------------------------------

func (c *Control) panelMoveRoutes(handle func(string, http.HandlerFunc)) {
	handle("GET /api/control/panel-move", c.panelMoveView)
	handle("POST /api/control/panel-move", c.panelMoveStart)
	handle("POST /api/control/panel-move/{action}", c.panelMoveAction)
}

func (c *Control) panelMoveView(w http.ResponseWriter, r *http.Request) {
	move, ok := c.panelMove()
	c.mu.Lock()
	arrival := c.data.PanelArrival
	c.mu.Unlock()
	servers := []map[string]string{}
	for _, s := range c.servers() {
		servers = append(servers, map[string]string{"id": s.ID, "name": cmpOrString(s.Name, s.ID)})
	}
	out := map[string]any{"hostname": panelHostname(c.cfg.PublicURL), "public_url": c.cfg.PublicURL, "servers": servers, "read_only": c.panelReadOnly(), "arrival": arrival, "version": buildinfo.Version}
	if ok {
		out["move"] = move
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, out)
}

func (c *Control) panelMoveStart(w http.ResponseWriter, r *http.Request) {
	var in PanelMoveInput
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if move, ok := c.panelMove(); ok && panelMoveActive(&move) {
		agent.Fail(w, 409, errors.New("a control panel move is already in progress"))
		return
	}
	plan, err := c.panelMovePlan(r.Context(), in)
	if err != nil {
		agent.Fail(w, 409, err)
		return
	}
	if !plan.Ready {
		agent.Fail(w, 409, fmt.Errorf("the move cannot start: %s", strings.Join(plan.Blockers, " ")))
		return
	}
	now := time.Now().UTC()
	move := &PanelMove{ID: random()[:16], Target: in.Target, TargetName: plan.TargetName, Source: plan.SourceServer, Hostname: plan.Hostname, State: panelMoveRunning, Phase: panelPhasePrepare,
		Plan: plan, AgentURL: plan.agentURL, Addresses: slices.Clone(plan.Addresses), Created: now, Updated: now}
	for _, name := range append(append(append([]string{}, panelPrepareSteps...), panelCutoverSteps...), panelStepDNS, panelStepRetire) {
		move.Steps = append(move.Steps, PanelMoveStep{Name: name, Title: panelStepTitles[name], Status: "pending"})
	}
	move.logf("", "Moving the control panel %s to %s.", plan.Hostname, plan.TargetName)
	c.mu.Lock()
	old := c.data.PanelMove
	c.data.PanelMove = move
	if err := c.save(); err != nil {
		c.data.PanelMove = old
		c.mu.Unlock()
		agent.Fail(w, 500, err)
		return
	}
	c.mu.Unlock()
	c.panelMoveResume()
	view, _ := c.panelMove()
	agent.JSON(w, 202, view)
}

func (c *Control) panelMoveAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action == "plan" {
		var in PanelMoveInput
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		plan, err := c.panelMovePlan(r.Context(), in)
		if err != nil {
			agent.Fail(w, 409, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		agent.JSON(w, 200, plan)
		return
	}
	move, ok := c.panelMove()
	if !ok {
		http.NotFound(w, r)
		return
	}
	var err error
	switch action {
	case "retry":
		if move.State != panelMoveFailed {
			err = errors.New("only a failed step can be retried")
			break
		}
		_, err = c.updatePanelMove(func(m *PanelMove) error {
			m.State, m.Error = panelMoveRunning, ""
			if m.Phase == panelPhaseCutover && m.step(panelStepActivate).Status == "done" {
				m.State = panelMoveDNS
			}
			m.logf("", "Retrying.")
			return nil
		})
		if err == nil {
			c.panelMoveResume()
		}
	case "cutover":
		if move.State != panelMoveStandby {
			err = errors.New("the cutover starts once the destination stands by")
			break
		}
		_, err = c.updatePanelMove(func(m *PanelMove) error {
			m.State, m.Phase = panelMoveRunning, panelPhaseCutover
			m.logf("", "Cutover started by the operator.")
			return nil
		})
		if err == nil {
			c.panelMoveResume()
		}
	case "check-dns":
		if move.State != panelMoveDNS {
			err = errors.New("DNS is checked once the destination is active")
			break
		}
		c.panelCheckDNS(r.Context(), true)
	case "abort":
		err = c.panelMoveAbort(r.Context())
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		agent.Fail(w, 409, err)
		return
	}
	view, _ := c.panelMove()
	agent.JSON(w, 200, view)
}

// pick is a when ok, else b.
func pick(ok bool, a, b string) string {
	if ok {
		return a
	}
	return b
}

func cmpOrString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func panelHostname(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// ---- planning ------------------------------------------------------------------------

func (c *Control) serverEntry(id string) (Server, bool) {
	for _, s := range c.servers() {
		if s.ID == id {
			return s, true
		}
	}
	return Server{}, false
}

func loopbackServer(s Server) bool { return localUpdateServer(s) }

// panelSourceServer finds this panel's own host among the servers: the
// loopback agent on the same machine, else the only other loopback agent.
func (c *Control) panelSourceServer(ctx context.Context, target string) string {
	machine := c.localMachineID()
	candidates := []string{}
	for _, s := range c.servers() {
		if s.ID == target || !loopbackServer(s) {
			continue
		}
		candidates = append(candidates, s.ID)
		var status agent.ControlHostStatus
		if machine != "" && c.moveCall(ctx, 10*time.Second, s.ID, http.MethodGet, "/v1/host/control", nil, &status) == nil && status.MachineID == machine {
			return s.ID
		}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

func (c *Control) localMachineID() string {
	if c.panelMachineID != nil {
		return c.panelMachineID()
	}
	raw, _ := os.ReadFile("/etc/machine-id")
	return strings.TrimSpace(string(raw))
}

func (c *Control) panelMovePlan(ctx context.Context, in PanelMoveInput) (PanelMovePlan, error) {
	plan := PanelMovePlan{Input: in, Hostname: panelHostname(c.cfg.PublicURL), Servers: []PanelMoveServer{}, Addresses: []string{}, Checks: []PanelMoveCheck{}, Blockers: []string{}, Warnings: []string{}, Version: buildinfo.Version}
	check := func(name string, ok bool, passed, failed string) {
		detail := pick(ok, passed, failed)
		plan.Checks = append(plan.Checks, PanelMoveCheck{Name: name, OK: ok, Detail: detail})
		if !ok {
			plan.Blockers = append(plan.Blockers, detail)
		}
	}
	if !strings.HasPrefix(c.cfg.PublicURL, "https://") || plan.Hostname == "" {
		return plan, errors.New("only a panel with a public HTTPS hostname can move")
	}
	target, ok := c.serverEntry(in.Target)
	if !ok {
		return plan, errors.New("choose a destination server")
	}
	plan.TargetName = cmpOrString(target.Name, target.ID)
	var status agent.ControlHostStatus
	if err := c.moveCall(ctx, 20*time.Second, target.ID, http.MethodGet, "/v1/host/control?host="+url.QueryEscape(plan.Hostname), nil, &status); err != nil {
		var refused *agentHTTPError
		if errors.As(err, &refused) && refused.status == http.StatusNotFound {
			return plan, fmt.Errorf("%s runs an agent that cannot host the control panel yet; update it first", plan.TargetName)
		}
		return plan, fmt.Errorf("%s did not answer: %w", plan.TargetName, err)
	}
	if status.MachineID != "" && status.MachineID == c.localMachineID() {
		return plan, errors.New("the destination is the server this panel already runs on")
	}
	plan.agentURL = status.AgentURL
	check("helper", status.Supported, "The destination's upgrade helper can install the panel.", status.Detail)
	check("version", status.Version == buildinfo.Version, "The destination runs the same release "+buildinfo.Version+".", fmt.Sprintf("The destination's agent runs %s and this panel %s; update the destination to %s first.", status.Version, buildinfo.Version, buildinfo.Version))
	check("existing", !status.Installed || status.Owned, "No other control panel runs on the destination.", "A control panel that no move installed already runs on the destination.")
	check("port", status.ListenFree, status.Listen+" is free on the destination.", status.Listen+" is in use on the destination.")
	check("hostname", !status.Reserved, plan.Hostname+" is not reserved on the destination.", plan.Hostname+" is reserved on the destination.")
	bundle := c.updateSettingsSnapshot().Bundle
	switch {
	case status.BundleStaged:
		check("bundle", true, "The destination holds the verified bundle of "+status.Version+".", "")
	case bundle != nil && bundle.Version == status.Version:
		check("bundle", true, "The verified bundle of "+status.Version+" is sent to the destination.", "")
	default:
		check("bundle", false, "", "No verified bundle of "+status.Version+" is available for the destination; check for releases on the Updates page.")
	}
	plan.Ingress, plan.IngressDetail = status.Ingress, status.IngressDetail
	var host agent.MoveHost
	_ = c.moveCall(ctx, 15*time.Second, target.ID, http.MethodGet, "/v1/move/host", nil, &host)
	plan.Addresses = moveTargetAddresses(host, target, in.Addresses)
	check("addresses", len(plan.Addresses) > 0, "DNS will point to "+strings.Join(plan.Addresses, ", ")+".", "The destination's public address is unknown; enter it.")

	// The registry the new panel gets.
	plan.SourceServer = c.panelSourceServer(ctx, target.ID)
	apps := map[string]int{}
	c.mu.Lock()
	for _, a := range c.data.Apps {
		apps[a.ServerID]++
	}
	moving := 0
	for _, job := range c.data.Moves {
		if !terminalMoveState(job.State) {
			moving++
		}
	}
	c.mu.Unlock()
	check("moves", moving == 0, "No application move is in progress.", fmt.Sprintf("%d application move(s) are in progress; finish or abort them first.", moving))
	inProgress := updateInProgress(c.updateSettingsSnapshot())
	check("updates", !inProgress, "No update rollout is running.", "An update rollout is running; wait for it to finish.")
	for _, s := range c.servers() {
		entry := PanelMoveServer{ID: s.ID, Name: cmpOrString(s.Name, s.ID), Role: "remote", URL: s.URL, Apps: apps[s.ID]}
		switch {
		case s.ID == target.ID:
			entry.Role, entry.NewURL, entry.Reachable, entry.Detail = "destination", status.AgentURL, true, "Becomes this panel's own server."
		case s.ID == plan.SourceServer:
			entry.Role = "source"
			plan.SourceName = entry.Name
			if in.SourceURL != "" {
				entry.NewURL = in.SourceURL
				entry.Reachable, entry.Detail = c.panelProbe(ctx, target.ID, in.SourceURL, s.Token)
			}
			if !entry.Reachable {
				if entry.Apps > 0 {
					entry.Detail = strings.TrimSpace(entry.Detail + fmt.Sprintf(" %s still holds %d application(s): move them first, or give an agent URL of it that the destination reaches.", entry.Name, entry.Apps))
					plan.Blockers = append(plan.Blockers, entry.Detail)
				} else {
					entry.Retire, entry.NewURL = true, ""
					entry.Detail = "Holds no applications; it leaves the new panel's server list. Shut it down after the move."
				}
			}
		default:
			candidate := s.URL
			if v := in.ServerURLs[s.ID]; v != "" {
				candidate = v
			}
			entry.NewURL = candidate
			entry.Reachable, entry.Detail = c.panelProbe(ctx, target.ID, candidate, s.Token)
			if !entry.Reachable {
				entry.Detail = fmt.Sprintf("The destination cannot reach %s at %s (%s); enter an agent URL it can reach.", entry.Name, candidate, entry.Detail)
				plan.Blockers = append(plan.Blockers, entry.Detail)
			}
		}
		plan.Servers = append(plan.Servers, entry)
	}
	if plan.SourceServer == "" {
		plan.Warnings = append(plan.Warnings, "This panel's own server was not identified among the servers; it keeps its entry and URL.")
	}

	// Sizes.
	files, size, err := panelDataSize(c.cfg.DataDir)
	if err != nil {
		plan.Blockers = append(plan.Blockers, "The panel's data directory cannot be read: "+err.Error())
	}
	plan.DataFiles, plan.DataBytes = files, size
	managed, dbSize, dbErr := c.panelDatabaseInfo(ctx)
	switch {
	case dbErr != nil:
		plan.Blockers = append(plan.Blockers, "The panel's database cannot be measured: "+dbErr.Error())
	case managed:
		plan.Database, plan.DatabaseBytes = "managed", dbSize
		check("docker", status.Docker, "Docker runs the panel's database container on the destination.", "The destination has no Docker for the panel's database container.")
	default:
		plan.Database = "external"
		plan.Warnings = append(plan.Warnings, "The panel uses an external database; the destination connects to the same DATABASE_URL, which must be reachable from it.")
	}

	// Certificate.
	plan.Certificate = "Caddy on the destination requests a certificate when DNS points there; HTTPS answers within a minute of that."
	if plan.SourceServer != "" && status.CertificateStorage != "" {
		var cert agent.ControlCertificate
		if err := c.moveCall(ctx, 15*time.Second, plan.SourceServer, http.MethodGet, "/v1/host/control/certificate?check=1&host="+url.QueryEscape(plan.Hostname), nil, &cert); err == nil {
			plan.CertificateOK = true
			plan.Certificate = "The current certificate (" + cert.Issuer + ") is copied into the destination's Caddy storage, so HTTPS works as soon as DNS points there; Caddy renews it."
		}
	} else if status.CertificateStorage == "" {
		plan.Certificate = "The destination's Caddy storage was not found; " + plan.Certificate
	}
	plan.Ready = len(plan.Blockers) == 0
	return plan, nil
}

func (c *Control) panelProbe(ctx context.Context, target, raw, token string) (bool, string) {
	var out struct {
		Reachable bool   `json:"reachable"`
		Detail    string `json:"detail"`
	}
	if err := c.moveCall(ctx, 15*time.Second, target, http.MethodPost, "/v1/host/control/probe", map[string]string{"url": raw, "token": token}, &out); err != nil {
		return false, "the destination could not probe it"
	}
	if out.Reachable {
		return true, "The destination reaches it."
	}
	return false, out.Detail
}

// ---- running ----------------------------------------------------------------------

// panelMoveResume starts the runner for the move's current state; a
// restarted panel calls it from Start.
func (c *Control) panelMoveResume() {
	move, ok := c.panelMove()
	if !ok || (move.State != panelMoveRunning && move.State != panelMoveDNS) {
		return
	}
	c.mu.Lock()
	if c.panelRunning {
		c.mu.Unlock()
		return
	}
	c.panelRunning = true
	c.mu.Unlock()
	base := c.moveBase
	if base == nil {
		base = context.Background()
	}
	// Like move runners, the loop ends when the panel stops its background
	// work (move_work.go), so it never outlives the panel or a test.
	started := c.moveWork.goWith(base, func(ctx context.Context) {
		defer func() {
			c.mu.Lock()
			c.panelRunning = false
			c.mu.Unlock()
		}()
		c.panelMoveLoop(ctx)
	})
	if !started {
		c.mu.Lock()
		c.panelRunning = false
		c.mu.Unlock()
	}
}

func (c *Control) panelMoveLoop(ctx context.Context) {
	for ctx.Err() == nil {
		move, ok := c.panelMove()
		if !ok {
			return
		}
		switch move.State {
		case panelMoveRunning:
			if !c.panelRunPhase(ctx, move) {
				return
			}
		case panelMoveDNS:
			if c.panelCheckDNS(ctx, false) {
				continue
			}
			if sleepContext(ctx, c.panelPoll()) != nil {
				return
			}
		default:
			return
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Control) panelPoll() time.Duration {
	if c.panelPollInterval > 0 {
		return c.panelPollInterval
	}
	return 30 * time.Second
}

func (c *Control) panelRetries() []time.Duration {
	if c.panelRetryDelays != nil {
		return c.panelRetryDelays
	}
	return []time.Duration{10 * time.Second, 30 * time.Second, time.Minute}
}

// panelRunPhase runs the remaining steps of the move's phase. It returns
// false when the runner should stop (a failure, an abort, standby).
func (c *Control) panelRunPhase(ctx context.Context, move PanelMove) bool {
	steps := panelPrepareSteps
	if move.Phase == panelPhaseCutover {
		steps = panelCutoverSteps
	}
	for _, name := range steps {
		current, ok := c.panelMove()
		if !ok || current.State != panelMoveRunning {
			return false
		}
		if s := current.step(name); s == nil || s.Status == "done" || s.Status == "skipped" {
			continue
		}
		if !c.panelRunStep(ctx, name) {
			return false
		}
	}
	_, err := c.updatePanelMove(func(m *PanelMove) error {
		if m.State != panelMoveRunning {
			return errors.New("state changed")
		}
		if m.Phase == panelPhasePrepare {
			m.State = panelMoveStandby
			m.logf("", "The destination stands by with a copy of this panel. Start the cutover when ready; this panel then becomes read-only for a few seconds of final copy.")
			return nil
		}
		m.State = panelMoveDNS
		m.setStep(panelStepDNS, "waiting", "Set the DNS records shown below.")
		m.logf(panelStepDNS, "The destination is active. Point %s to %s.", m.Hostname, strings.Join(m.Addresses, ", "))
		return nil
	})
	return err == nil && move.Phase == panelPhaseCutover
}

func (c *Control) panelRunStep(ctx context.Context, name string) bool {
	delays := c.panelRetries()
	for attempt := 0; ; attempt++ {
		_, err := c.updatePanelMove(func(m *PanelMove) error {
			if m.State != panelMoveRunning {
				return errors.New("state changed")
			}
			m.setStep(name, "running", "")
			m.step(name).Attempts++
			return nil
		})
		if err != nil {
			return false
		}
		detail, skipped, err := c.panelStep(ctx, name)
		if err == nil {
			status := "done"
			if skipped {
				status = "skipped"
			}
			_, uerr := c.updatePanelMove(func(m *PanelMove) error {
				m.setStep(name, status, detail)
				if detail != "" {
					m.logf(name, "%s", detail)
				}
				return nil
			})
			return uerr == nil
		}
		last := attempt >= len(delays)
		_, _ = c.updatePanelMove(func(m *PanelMove) error {
			m.logf(name, "%s failed: %s", panelStepTitles[name], err.Error())
			if last {
				m.setStep(name, "failed", err.Error())
				m.State, m.Error = panelMoveFailed, err.Error()
			}
			return nil
		})
		if last || ctx.Err() != nil {
			return false
		}
		if sleepContext(ctx, delays[attempt]) != nil {
			return false
		}
		if move, ok := c.panelMove(); !ok || move.State != panelMoveRunning {
			return false
		}
	}
}

// panelStep runs one step; skipped marks an optional step that did not apply.
func (c *Control) panelStep(ctx context.Context, name string) (detail string, skipped bool, err error) {
	move, _ := c.panelMove()
	switch name {
	case panelStepCheck:
		plan, err := c.panelMovePlan(ctx, move.Plan.Input)
		if err != nil {
			return "", false, err
		}
		if !plan.Ready {
			return "", false, errors.New(strings.Join(plan.Blockers, " "))
		}
		_, err = c.updatePanelMove(func(m *PanelMove) error {
			m.Plan, m.AgentURL, m.Addresses = plan, plan.agentURL, plan.Addresses
			return nil
		})
		return "The destination is ready.", false, err
	case panelStepBundle:
		return c.panelEnsureBundle(ctx, move)
	case panelStepInstall:
		if err := c.panelCopyData(ctx, move); err != nil {
			return "", false, err
		}
		if err := c.panelApply(ctx, move, true); err != nil {
			return "", false, err
		}
		return "The panel is installed on " + move.TargetName + " in standby.", false, nil
	case panelStepDatabase:
		return c.panelCopyDatabase(ctx, move)
	case panelStepCert:
		return c.panelCopyCertificate(ctx, move)
	case panelStepRoute:
		return c.panelPublishRoute(ctx, move, move.Certificate != "")
	case panelStepFreeze:
		return c.panelFreeze(ctx)
	case panelStepFinal:
		if err := c.panelCopyData(ctx, move); err != nil {
			return "", false, err
		}
		if err := c.panelApply(ctx, move, true); err != nil {
			return "", false, err
		}
		if detail, _, err := c.panelCopyDatabase(ctx, move); err != nil {
			return "", false, err
		} else {
			now := time.Now().UTC()
			_, _ = c.updatePanelMove(func(m *PanelMove) error { m.FinalCopied = &now; return nil })
			return "Final copy: " + detail, false, nil
		}
	case panelStepActivate:
		if err := c.panelApply(ctx, move, false); err != nil {
			return "", false, err
		}
		return "The panel on " + move.TargetName + " is active; this panel stays read-only.", false, nil
	}
	return "", false, fmt.Errorf("unknown step %s", name)
}

// panelFreeze makes this panel read-only and waits for running jobs.
func (c *Control) panelFreeze(ctx context.Context) (string, bool, error) {
	if _, err := c.updatePanelMove(func(m *PanelMove) error { m.ReadOnly = true; return nil }); err != nil {
		return "", false, err
	}
	deadline := time.Now().Add(10 * time.Minute)
	for c.panelJobs.Load() > 0 {
		if time.Now().After(deadline) {
			return "", false, errors.New("jobs are still running after 10 minutes; retry once they finish")
		}
		if err := sleepContext(ctx, 500*time.Millisecond); err != nil {
			return "", false, err
		}
	}
	return "This panel is read-only; changes answer “the control panel is moving”.", false, nil
}

func (c *Control) panelEnsureBundle(ctx context.Context, move PanelMove) (string, bool, error) {
	var status agent.ControlHostStatus
	if err := c.moveCall(ctx, 20*time.Second, move.Target, http.MethodGet, "/v1/host/control", nil, &status); err != nil {
		return "", false, err
	}
	if status.BundleStaged {
		return "The destination holds the verified bundle of " + status.Version + ".", false, nil
	}
	bundle := c.updateSettingsSnapshot().Bundle
	if bundle == nil || bundle.Version != status.Version {
		return "", false, errors.New("no verified bundle of " + status.Version + " to send")
	}
	target, ok := c.serverEntry(move.Target)
	if !ok {
		return "", false, errors.New("the destination left the server list")
	}
	if c.panelBundleTransfer != nil {
		return "Sent the verified bundle of " + bundle.Version + ".", false, c.panelBundleTransfer(ctx, target, *bundle, status.Arch)
	}
	path, err := c.cachedArchive(ctx, *bundle, status.Arch)
	if err != nil {
		return "", false, err
	}
	if err := c.moveCall(ctx, 30*time.Second, move.Target, http.MethodPost, "/v1/host/control/bundle/manifest", map[string]any{"manifest": bundle.Manifest, "signature": bundle.Signature}, nil); err != nil {
		return "", false, err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	info, _ := file.Stat()
	if err := c.panelStream(ctx, target, http.MethodPut, "/v1/host/control/bundle/"+bundle.Version, "application/octet-stream", file, info.Size(), 20*time.Minute, nil); err != nil {
		return "", false, err
	}
	return "Sent the verified bundle of " + bundle.Version + ".", false, nil
}

// panelStream sends a body to an agent; size -1 streams it chunked.
func (c *Control) panelStream(ctx context.Context, target Server, method, path, contentType string, body io.Reader, size int64, timeout time.Duration, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target.URL+path, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+target.Token)
	req.Header.Set("Content-Type", contentType)
	client := &http.Client{Transport: c.client.Transport, CheckRedirect: c.client.CheckRedirect}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("agent unavailable: %s", moveBounded(err.Error(), 200))
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
		return &agentHTTPError{status: res.StatusCode, message: e.Error}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
	}
	return nil
}

func (c *Control) panelApply(ctx context.Context, move PanelMove, standby bool) error {
	return c.moveCall(ctx, 6*time.Minute, move.Target, http.MethodPost, "/v1/host/control/apply", map[string]any{"hostname": move.Hostname, "standby": standby, "env": panelEnvironment(os.Environ(), c.cfg.DataDir)}, nil)
}

func (c *Control) panelCopyData(ctx context.Context, move PanelMove) error {
	c.mu.Lock()
	raw, _ := json.Marshal(c.data)
	c.mu.Unlock()
	var snapshot saved
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return err
	}
	rewritten, err := panelRewriteState(snapshot, panelRewrite{Target: move.Target, TargetURL: move.AgentURL, Plan: move.Plan, Hostname: move.Hostname, At: time.Now().UTC()})
	if err != nil {
		return err
	}
	archive, files, err := panelSnapshot(c.cfg.DataDir, c.cfg.Key, rewritten)
	if err != nil {
		return err
	}
	target, ok := c.serverEntry(move.Target)
	if !ok {
		return errors.New("the destination left the server list")
	}
	var out struct {
		Files int `json:"files"`
	}
	if err := c.panelStream(ctx, target, http.MethodPut, "/v1/host/control/files", "application/x-tar", bytes.NewReader(archive), int64(len(archive)), 10*time.Minute, &out); err != nil {
		return err
	}
	if out.Files != files {
		return fmt.Errorf("the destination received %d of %d files", out.Files, files)
	}
	return nil
}

func (c *Control) panelCopyDatabase(ctx context.Context, move PanelMove) (string, bool, error) {
	managed, size, err := c.panelDatabaseInfo(ctx)
	if err != nil {
		return "", false, err
	}
	if !managed {
		return "The external database stays where it is; the destination uses the same DATABASE_URL.", true, nil
	}
	target, ok := c.serverEntry(move.Target)
	if !ok {
		return "", false, errors.New("the destination left the server list")
	}
	reader, writer := io.Pipe()
	dumpErr := make(chan error, 1)
	go func() {
		err := c.panelDump(ctx, writer)
		writer.CloseWithError(err)
		dumpErr <- err
	}()
	started := time.Now()
	err = c.panelStream(ctx, target, http.MethodPut, "/v1/host/control/database", "application/octet-stream", reader, -1, 60*time.Minute, nil)
	reader.CloseWithError(errors.New("upload ended"))
	if derr := <-dumpErr; derr != nil && err == nil {
		err = fmt.Errorf("pg_dump failed: %w", derr)
	}
	if err != nil {
		return "", false, err
	}
	return fmt.Sprintf("Database copied (%s) in %s.", formatBytes(size), time.Since(started).Round(time.Second)), false, nil
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func (c *Control) panelCopyCertificate(ctx context.Context, move PanelMove) (string, bool, error) {
	skip := "No certificate was copied; Caddy on the destination requests one when DNS points there, and HTTPS answers shortly after."
	if move.Source == "" {
		return skip, true, nil
	}
	var cert agent.ControlCertificate
	if err := c.moveCall(ctx, 20*time.Second, move.Source, http.MethodGet, "/v1/host/control/certificate?host="+url.QueryEscape(move.Hostname), nil, &cert); err != nil {
		return skip + " (" + moveBounded(err.Error(), 160) + ")", true, nil
	}
	var placed struct {
		Storage string `json:"storage"`
	}
	if err := c.moveCall(ctx, 6*time.Minute, move.Target, http.MethodPut, "/v1/host/control/certificate", cert, &placed); err != nil {
		var refused *agentHTTPError
		if errors.As(err, &refused) && refused.status == http.StatusConflict {
			return skip + " (" + moveBounded(refused.message, 160) + ")", true, nil
		}
		return "", false, err
	}
	_, err := c.updatePanelMove(func(m *PanelMove) error { m.Certificate = cert.Issuer; return nil })
	return "Copied the certificate (" + cert.Issuer + ") into " + placed.Storage + ".", false, err
}

func (c *Control) panelPublishRoute(ctx context.Context, move PanelMove, tls bool) (string, bool, error) {
	var out struct {
		Mode   string `json:"mode"`
		Detail string `json:"detail"`
	}
	if err := c.moveCall(ctx, 6*time.Minute, move.Target, http.MethodPut, "/v1/host/control/route", map[string]any{"hostname": move.Hostname, "tls": tls}, &out); err != nil {
		return "", false, err
	}
	if tls {
		_, _ = c.updatePanelMove(func(m *PanelMove) error { m.TLSSubject = true; return nil })
	}
	return move.Hostname + " is published on " + move.TargetName + " through " + out.Detail + ".", false, nil
}

// panelCheckDNS checks the hostname once; true when the move finished.
func (c *Control) panelCheckDNS(ctx context.Context, explicit bool) bool {
	c.panelDNSMu.Lock()
	defer c.panelDNSMu.Unlock()
	move, ok := c.panelMove()
	if !ok || move.State != panelMoveDNS {
		return false
	}
	statuses, all := c.resolveMoveHostnames(ctx, MoveJob{Hostnames: []string{move.Hostname}, Addresses: move.Addresses})
	tlsReady, tlsDetail := move.TLSReady, move.TLSDetail
	if all && !move.TLSSubject {
		// Certificates are requested only once DNS points to the destination.
		if _, _, err := c.panelPublishRoute(ctx, move, true); err != nil {
			tlsDetail = "The destination could not request the certificate: " + err.Error()
		}
		move, _ = c.panelMove()
	}
	if all || explicit {
		if err := c.moveHTTPSProbe(ctx, moveProbeAddress(move.Addresses), move.Hostname); err != nil {
			tlsReady, tlsDetail = false, fmt.Sprintf("HTTPS for %s on the destination: %s.", move.Hostname, err)
		} else {
			tlsReady, tlsDetail = true, "HTTPS answers on the destination."
		}
	}
	now := time.Now().UTC()
	updated, err := c.updatePanelMove(func(m *PanelMove) error {
		if m.State != panelMoveDNS {
			return errors.New("state changed")
		}
		m.DNS, m.DNSChecked, m.TLSReady, m.TLSDetail = statuses, &now, tlsReady, tlsDetail
		if all && tlsReady {
			m.setStep(panelStepDNS, "done", "DNS points to the destination and HTTPS answers there.")
			m.logf(panelStepDNS, "DNS for %s points to %s and HTTPS answers there.", m.Hostname, m.TargetName)
			m.setStep(panelStepRetire, "running", "")
		}
		return nil
	})
	if err != nil || !(all && tlsReady) {
		return false
	}
	c.panelRetire(ctx, updated)
	return true
}

// panelRetire hands over: the moved marker first (a restart then serves only
// the notice), then the move is done, then this host's agent replaces the
// Caddy site and stops this panel's unit, which ends this process.
func (c *Control) panelRetire(ctx context.Context, move PanelMove) {
	marker, _ := json.MarshalIndent(map[string]any{"moved_to": c.cfg.PublicURL, "server": move.TargetName, "at": time.Now().UTC()}, "", "  ")
	markErr := os.WriteFile(filepath.Join(c.cfg.DataDir, agent.ControlMovedFile), marker, 0o600)
	now := time.Now().UTC()
	notes := []string{}
	if markErr != nil {
		notes = append(notes, "The moved marker could not be written: "+markErr.Error())
	}
	_, _ = c.updatePanelMove(func(m *PanelMove) error {
		m.State, m.Finished = panelMoveDone, &now
		m.setStep(panelStepRetire, "done", "This panel hands over to "+m.TargetName+".")
		m.logf(panelStepRetire, "The move is complete. This panel now only points visitors to the new server.")
		m.RetireDetail = notes
		return nil
	})
	if move.Source == "" || markErr != nil {
		return
	}
	var out struct {
		Notes []string `json:"notes"`
	}
	err := c.moveCall(ctx, 6*time.Minute, move.Source, http.MethodPost, "/v1/host/control/retire", map[string]string{"hostname": move.Hostname, "moved_to": move.TargetName}, &out)
	_, _ = c.updatePanelMove(func(m *PanelMove) error {
		if err != nil {
			m.RetireDetail = append(m.RetireDetail, "This server's agent did not stop the panel ("+moveBounded(err.Error(), 200)+"); run systemctl disable --now lidza-control on it or shut it down.")
		} else {
			m.RetireDetail = append(m.RetireDetail, out.Notes...)
		}
		return nil
	})
}

// panelMoveAbort returns the destination to nothing and this panel to
// normal; only before the cutover.
func (c *Control) panelMoveAbort(ctx context.Context) error {
	move, ok := c.panelMove()
	if !ok || !panelMoveActive(&move) {
		return errors.New("no control panel move to abort")
	}
	if move.Phase == panelPhaseCutover {
		return errors.New("the cutover has started; the destination already holds the latest state, so the move can only finish")
	}
	if _, err := c.updatePanelMove(func(m *PanelMove) error {
		m.State = panelMoveAborting
		m.logf("", "Aborting: removing the standby panel from %s.", m.TargetName)
		return nil
	}); err != nil {
		return err
	}
	err := c.moveCall(ctx, 6*time.Minute, move.Target, http.MethodDelete, "/v1/host/control", nil, nil)
	now := time.Now().UTC()
	_, uerr := c.updatePanelMove(func(m *PanelMove) error {
		if err != nil {
			m.State, m.Error = panelMoveFailed, "abort: "+err.Error()
			m.logf("", "The destination could not be cleaned up: %s. Retry the abort.", err.Error())
			return nil
		}
		m.State, m.ReadOnly, m.Finished, m.Error = panelMoveAborted, false, &now, ""
		m.logf("", "Aborted. Nothing of the move remains on %s; this panel is unchanged.", m.TargetName)
		return nil
	})
	if err != nil {
		return err
	}
	return uerr
}

// ---- activity ----------------------------------------------------------------------

func (c *Control) panelMoveActivity(now time.Time) []ActivityItem {
	move, ok := c.panelMove()
	if !ok {
		return nil
	}
	state := map[string]string{panelMoveRunning: "running", panelMoveAborting: "running", panelMoveStandby: "waiting", panelMoveDNS: "waiting", panelMoveFailed: "failed", panelMoveDone: "succeeded", panelMoveAborted: "succeeded"}[move.State]
	if move.Finished != nil && now.Sub(*move.Finished) > activityFinishedFor {
		return nil
	}
	done := int64(0)
	for _, s := range move.Steps {
		if s.Status == "done" || s.Status == "skipped" {
			done++
		}
	}
	started := move.Created
	item := ActivityItem{ID: "panel-move:" + move.ID, Kind: "panel-move", Title: "Moving the control panel to " + move.TargetName, ServerID: move.Target, ServerName: move.TargetName,
		State: state, Started: &started, Updated: move.Updated, Progress: &ActivityProgress{Done: done, Total: int64(len(move.Steps)), Unit: "steps"}, Link: "/console/control-panel", Details: []string{}}
	switch move.State {
	case panelMoveStandby:
		item.WaitingOn = "The destination stands by; start the cutover."
	case panelMoveDNS:
		item.WaitingOn = "Point " + move.Hostname + " to " + strings.Join(move.Addresses, ", ") + "."
	case panelMoveFailed:
		item.Message = move.Error
	case panelMoveAborted:
		item.Message = "Aborted"
	}
	for i := len(move.Log) - 1; i >= 0 && len(item.Details) < activityMaxLogLines; i-- {
		item.Details = append(item.Details, move.Log[i].Message)
	}
	return []ActivityItem{item}
}
