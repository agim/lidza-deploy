package agent

// Managed host firewall. The panel edits a model; the agent renders it to ufw,
// shows the exact commands for review and applies them behind a lockout
// guard: an applied change must be confirmed from the panel within 120 seconds
// or the agent restores the previous ruleset.
//
// Scope: ufw filters traffic addressed to the host itself (SSH, Caddy, the
// agent, native databases). Ports that Docker publishes for containers are
// forwarded before ufw sees them; the agent governs those through its own
// DOCKER-USER chain (public_ports.go). They appear here as read-only "app"
// rules and are never written to ufw, and Docker's chains are never touched.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// HostFirewallRule allows inbound traffic to one port or port range.
type HostFirewallRule struct {
	Port    string   `json:"port"`           // "5432" or "60000:61000"
	Proto   string   `json:"proto"`          // tcp, udp or any
	From    []string `json:"from,omitempty"` // addresses or CIDR networks; empty = anywhere
	Comment string   `json:"comment,omitempty"`
	// Source is "system" (always present), "app:<id>" (a container's public
	// port), "move:<id>" (a replication relay while a move runs,
	// move_replication_firewall.go) or "user" (edited in the panel or
	// imported from the host).
	Source string `json:"source"`
	// Enforced is "ufw", or "docker" for container ports governed by the
	// Docker firewall chain.
	Enforced string `json:"enforced,omitempty"`
}

// HostFirewallModel is the operator-edited firewall definition.
type HostFirewallModel struct {
	Enabled        bool   `json:"enabled"`
	DefaultInbound string `json:"default_inbound"` // deny
	// SSHFrom restricts the system SSH rule; empty means reachable from
	// anywhere. SSH always stays reachable from at least one address.
	SSHFrom []string           `json:"ssh_from,omitempty"`
	Rules   []HostFirewallRule `json:"rules"` // user rules only
}

type hostFirewallSnapshot struct {
	Active          bool              `json:"active"`
	DefaultIncoming string            `json:"default_incoming,omitempty"`
	Owned           []ufwAtom         `json:"owned,omitempty"`
	Model           HostFirewallModel `json:"model"`
}

// HostFirewallPending is an applied change waiting for confirmation.
type HostFirewallPending struct {
	Deadline time.Time            `json:"deadline"`
	Previous hostFirewallSnapshot `json:"previous"`
}

// HostFirewallState is persisted in agent state.
type HostFirewallState struct {
	Model       HostFirewallModel    `json:"model"`
	Imported    bool                 `json:"imported"`
	ImportedAt  time.Time            `json:"imported_at,omitzero"`
	Owned       []ufwAtom            `json:"owned,omitempty"` // ufw rules the agent manages
	LastApplied time.Time            `json:"last_applied,omitzero"`
	LastError   string               `json:"last_error,omitempty"`
	Pending     *HostFirewallPending `json:"pending,omitempty"`
}

// HostFirewallView is GET /v1/host/firewall.
type HostFirewallView struct {
	Supported       bool               `json:"supported"`
	Reason          string             `json:"reason,omitempty"`
	Backend         string             `json:"backend"`
	Installed       bool               `json:"installed"`
	Active          bool               `json:"active"`
	DefaultIncoming string             `json:"default_incoming,omitempty"`
	Model           HostFirewallModel  `json:"model"`
	Rules           []HostFirewallRule `json:"rules"` // effective: system, user and app rules
	Kept            []string           `json:"kept,omitempty"`
	PendingDeadline time.Time          `json:"pending_deadline,omitzero"`
	PendingSeconds  int                `json:"pending_seconds,omitempty"`
	Applying        bool               `json:"applying,omitempty"`
	LastApplied     time.Time          `json:"last_applied,omitzero"`
	LastError       string             `json:"last_error,omitempty"`
	Notes           []string           `json:"notes,omitempty"`
}

// HostFirewallChange is one reviewed command.
type HostFirewallChange struct {
	Action  string `json:"action"` // install, add, delete, default, enable, disable
	Command string `json:"command"`
}

// HostFirewallPlan is the dry-run preview of an apply.
type HostFirewallPlan struct {
	Changes        []HostFirewallChange `json:"changes"`
	Unmanaged      []string             `json:"unmanaged,omitempty"` // host rules the agent does not own; kept
	Kept           []string             `json:"kept,omitempty"`      // rules ufw holds that the model cannot express; kept
	Warnings       []string             `json:"warnings,omitempty"`
	Fingerprint    string               `json:"fingerprint"`
	ConfirmSeconds int                  `json:"confirm_seconds"`
}

// HostFirewallApply must name the fingerprint of the reviewed plan.
type HostFirewallApply struct {
	Fingerprint string `json:"fingerprint"`
}

const (
	firewallConfirmWindow = 120 * time.Second
	firewallMaxRules      = 200
	firewallMaxSources    = 64
)

var errFirewallPending = errors.New("a firewall change is waiting for confirmation; confirm it or wait for the automatic rollback")

func (m *Manager) firewallSupported() error {
	if err := m.hostOperationsAllowed(); err != nil {
		return err
	}
	if ok, reason := aptHost(); !ok {
		return errors.New(strings.Replace(reason, "package management", "firewall management", 1))
	}
	if os.Geteuid() != 0 {
		return errors.New("host firewall management requires the agent to run as root")
	}
	return nil
}

func readUfwHost(ctx context.Context) (ufwHostStatus, error) {
	host := ufwHostStatus{Installed: hostToolAvailable("ufw")}
	if !host.Installed {
		return host, nil
	}
	status, err := hostCommand(ctx, "ufw", "status", "verbose")
	if err != nil {
		return host, errors.New("ufw status could not be read")
	}
	host.Active, host.DefaultIncoming = parseUfwStatus(status)
	if host.DefaultIncoming == "" {
		raw, _ := os.ReadFile(hostPath("/etc/default/ufw"))
		host.DefaultIncoming = parseUfwDefaults(string(raw))
	}
	added, err := hostCommand(ctx, "ufw", "show", "added")
	if err != nil {
		return host, errors.New("ufw rules could not be read")
	}
	host.Atoms, host.Kept = parseUfwAdded(added)
	return host, nil
}

// sshPorts reads the ports sshd listens on; 22 when it cannot be determined.
func sshPorts(ctx context.Context) []string {
	out, err := hostCommand(ctx, "sshd", "-T")
	var ports []string
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "port" && validFirewallPort(fields[1]) == nil && !slices.Contains(ports, fields[1]) {
				ports = append(ports, fields[1])
			}
		}
	}
	if len(ports) == 0 {
		ports = []string{"22"}
	}
	return ports
}

// systemFirewallRules are always present: SSH, HTTP and HTTPS (including
// HTTP/3) for Caddy, and the agent's own listeners when they are public.
func (m *Manager) systemFirewallRules(ssh []string, sshFrom []string) []HostFirewallRule {
	var rules []HostFirewallRule
	for _, port := range ssh {
		rules = append(rules, HostFirewallRule{Port: port, Proto: "tcp", From: slices.Clone(sshFrom), Comment: "SSH", Source: "system"})
	}
	rules = append(rules,
		HostFirewallRule{Port: "80", Proto: "tcp", Comment: "HTTP", Source: "system"},
		HostFirewallRule{Port: "443", Proto: "tcp", Comment: "HTTPS", Source: "system"},
		HostFirewallRule{Port: "443", Proto: "udp", Comment: "HTTP/3", Source: "system"})
	listeners := []string{m.cfg.Listen, m.cfg.TLSListen}
	if m.cfg.Gateway != nil {
		listeners = append(listeners, m.cfg.Gateway.Listen)
	}
	for _, listener := range listeners {
		host, port, err := net.SplitHostPort(listener)
		if err != nil || port == "80" || port == "443" || validFirewallPort(port) != nil {
			continue
		}
		if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
			continue
		}
		if !slices.ContainsFunc(rules, func(r HostFirewallRule) bool { return r.Port == port && r.Proto == "tcp" }) {
			rules = append(rules, HostFirewallRule{Port: port, Proto: "tcp", Comment: "Līdza agent", Source: "system"})
		}
	}
	for i := range rules {
		rules[i].Enforced = "ufw"
	}
	return rules
}

// appFirewallRules lists container public ports (read-only, Docker-enforced).
func (m *Manager) appFirewallRules() []HostFirewallRule {
	m.mu.Lock()
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range m.data.Apps {
			if !yield(id) {
				return
			}
		}
	})
	var rules []HostFirewallRule
	for _, id := range ids {
		for _, reg := range releasePortRegistrations(m.data.Apps[id].Current) {
			for _, p := range reg.Ports {
				proto := p.Protocol
				if proto == "" {
					proto = "tcp"
				}
				rules = append(rules, HostFirewallRule{Port: strconv.Itoa(p.Port), Proto: proto, Comment: "Container public port (" + reg.Service + ")", Source: "app:" + reg.AppID, Enforced: "docker"})
			}
		}
	}
	m.mu.Unlock()
	return rules
}

// importUfwModel turns existing ufw rules into a model, preserving every
// allowlist verbatim: rules matching a system rule become that system rule,
// restricted SSH rules become the SSH source list, and the rest become user
// rules grouped by port, protocol and comment.
func importUfwModel(host ufwHostStatus, ssh []string) HostFirewallModel {
	model := HostFirewallModel{Enabled: host.Active, DefaultInbound: "deny", Rules: []HostFirewallRule{}}
	sshAnywhere := false
	var sshSources []string
	consumed := map[int]bool{}
	for i, a := range host.Atoms {
		if a.Proto == "tcp" && slices.Contains(ssh, a.Port) {
			consumed[i] = true
			if a.From == "" {
				sshAnywhere = true
			} else if !slices.Contains(sshSources, a.From) {
				sshSources = append(sshSources, a.From)
			}
			continue
		}
		if a.From == "" && (a.Port == "80" && a.Proto == "tcp" || a.Port == "443" && (a.Proto == "tcp" || a.Proto == "udp")) {
			consumed[i] = true
		}
	}
	if !sshAnywhere {
		model.SSHFrom = sshSources
	}
	index := map[string]int{}
	for i, a := range host.Atoms {
		if consumed[i] {
			continue
		}
		key := a.Proto + "|" + a.Port + "|" + a.Comment + "|" + strconv.FormatBool(a.From == "")
		if at, ok := index[key]; ok && a.From != "" {
			if !slices.Contains(model.Rules[at].From, a.From) {
				model.Rules[at].From = append(model.Rules[at].From, a.From)
			}
			continue
		}
		rule := HostFirewallRule{Port: a.Port, Proto: a.Proto, Comment: a.Comment, Source: "user", Enforced: "ufw"}
		if a.From != "" {
			rule.From = []string{a.From}
		}
		index[key] = len(model.Rules)
		model.Rules = append(model.Rules, rule)
	}
	return model
}

func validFirewallComment(c string) error {
	if len(c) > 80 {
		return errors.New("rule comments are limited to 80 characters")
	}
	for _, r := range c {
		if !unicode.IsPrint(r) || r == '\'' || r == '"' || r == '\\' {
			return errors.New("rule comments may contain printable characters except quotes and backslashes")
		}
	}
	return nil
}

func normalizeSources(sources []string) ([]string, error) {
	if len(sources) > firewallMaxSources {
		return nil, fmt.Errorf("at most %d sources per rule", firewallMaxSources)
	}
	var out []string
	for _, s := range sources {
		if strings.TrimSpace(s) == "" {
			continue
		}
		n, err := normalizeSource(s)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// normalizeFirewallModel validates a submitted model. Only user rules are
// accepted; system and app rules are derived by the agent.
func normalizeFirewallModel(in HostFirewallModel) (HostFirewallModel, error) {
	out := HostFirewallModel{Enabled: in.Enabled, DefaultInbound: in.DefaultInbound, Rules: []HostFirewallRule{}}
	if out.DefaultInbound == "" {
		out.DefaultInbound = "deny"
	}
	if out.DefaultInbound != "deny" {
		return out, errors.New("the default inbound policy must be deny")
	}
	sshFrom, err := normalizeSources(in.SSHFrom)
	if err != nil {
		return out, fmt.Errorf("SSH sources: %w", err)
	}
	if len(sshFrom) > 0 && !slices.ContainsFunc(sshFrom, func(s string) bool {
		ip := net.ParseIP(strings.Split(s, "/")[0])
		return ip != nil && !ip.IsLoopback() && !ip.IsUnspecified()
	}) {
		return out, errors.New("SSH must stay reachable from at least one non-loopback address")
	}
	out.SSHFrom = sshFrom
	seen := map[string]bool{}
	for _, r := range in.Rules {
		if r.Source != "" && r.Source != "user" {
			continue
		}
		if len(out.Rules) == firewallMaxRules {
			return out, fmt.Errorf("at most %d firewall rules", firewallMaxRules)
		}
		r.Source, r.Enforced = "user", "ufw"
		r.Port = strings.TrimSpace(r.Port)
		if r.Proto == "" {
			r.Proto = "tcp"
		}
		if r.Proto != "tcp" && r.Proto != "udp" && r.Proto != "any" {
			return out, errors.New("protocol must be tcp, udp or any")
		}
		if err := validFirewallPort(r.Port); err != nil {
			return out, err
		}
		if strings.Contains(r.Port, ":") && r.Proto == "any" {
			return out, errors.New("port ranges need a protocol (tcp or udp)")
		}
		r.Comment = strings.TrimSpace(r.Comment)
		if err := validFirewallComment(r.Comment); err != nil {
			return out, err
		}
		if r.From, err = normalizeSources(r.From); err != nil {
			return out, fmt.Errorf("port %s: %w", r.Port, err)
		}
		for _, atom := range expandRules([]HostFirewallRule{r}) {
			if seen[atom.key()] {
				return out, fmt.Errorf("port %s/%s is listed twice for the same source", r.Port, r.Proto)
			}
			seen[atom.key()] = true
		}
		out.Rules = append(out.Rules, r)
	}
	return out, nil
}

// ensureFirewallState imports the host's ufw rules the first time the
// firewall is viewed, so an existing allowlist is preserved verbatim.
func (m *Manager) ensureFirewallState(host ufwHostStatus, ssh []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.HostFirewall != nil && m.data.HostFirewall.Imported {
		return nil
	}
	m.data.HostFirewall = &HostFirewallState{Model: importUfwModel(host, ssh), Imported: true, ImportedAt: time.Now().UTC(), Owned: slices.Clone(host.Atoms)}
	if err := m.save(); err != nil {
		m.data.HostFirewall = nil
		return err
	}
	return nil
}

func (m *Manager) firewallStateCopy() HostFirewallState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.HostFirewall == nil {
		return HostFirewallState{}
	}
	out := *m.data.HostFirewall
	out.Owned = slices.Clone(out.Owned)
	out.Model.Rules = slices.Clone(out.Model.Rules)
	out.Model.SSHFrom = slices.Clone(out.Model.SSHFrom)
	if out.Pending != nil {
		pending := *out.Pending
		out.Pending = &pending
	}
	return out
}

type firewallContext struct {
	host    ufwHostStatus
	ssh     []string
	state   HostFirewallState
	desired []ufwAtom
	rules   []HostFirewallRule
}

// loadFirewall reads the host and the model. Caller holds hc.fwMu.
func (m *Manager) loadFirewall(ctx context.Context) (firewallContext, error) {
	var fc firewallContext
	var err error
	if fc.host, err = readUfwHost(ctx); err != nil {
		return fc, err
	}
	fc.ssh = sshPorts(ctx)
	if err = m.ensureFirewallState(fc.host, fc.ssh); err != nil {
		return fc, err
	}
	fc.state = m.firewallStateCopy()
	system := m.systemFirewallRules(fc.ssh, fc.state.Model.SSHFrom)
	fc.rules = append(append(append(system, fc.state.Model.Rules...), m.appFirewallRules()...), m.moveFirewallRules()...)
	fc.desired = expandRules(fc.rules)
	return fc, nil
}

func (m *Manager) firewallView(ctx context.Context) (HostFirewallView, error) {
	view := HostFirewallView{Backend: "ufw", Rules: []HostFirewallRule{}, Notes: []string{
		"Container public ports are forwarded by Docker before ufw sees them; the agent governs them through its Docker firewall chain and lists them here read-only.",
		"Rules the host already had are imported unchanged. Host rules the agent cannot represent are kept exactly as they are.",
	}}
	if err := m.firewallSupported(); err != nil {
		view.Reason = err.Error()
		return view, nil
	}
	hc := m.host()
	// Never block a read behind a running apply; wait briefly for a plan or
	// another read to finish.
	locked := false
	for range 50 {
		hc.mu.Lock()
		applying := hc.fwApplying
		hc.mu.Unlock()
		if applying {
			break
		}
		if locked = hc.fwMu.TryLock(); locked {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !locked {
		return m.firewallSnapshotView(), nil
	}
	defer hc.fwMu.Unlock()
	fc, err := m.loadFirewall(ctx)
	if err != nil {
		return view, err
	}
	view.Supported = true
	view.Installed, view.Active, view.DefaultIncoming = fc.host.Installed, fc.host.Active, fc.host.DefaultIncoming
	view.Model = fc.state.Model
	view.Rules = fc.rules
	view.Kept = fc.host.Kept
	view.LastApplied, view.LastError = fc.state.LastApplied, fc.state.LastError
	if p := fc.state.Pending; p != nil {
		view.PendingDeadline = p.Deadline
		view.PendingSeconds = max(0, int(time.Until(p.Deadline).Seconds()))
	}
	return view, nil
}

func (m *Manager) saveFirewallModel(ctx context.Context, in HostFirewallModel) (HostFirewallView, error) {
	if err := m.firewallSupported(); err != nil {
		return HostFirewallView{}, err
	}
	model, err := normalizeFirewallModel(in)
	if err != nil {
		return HostFirewallView{}, err
	}
	if m.firewallBusy() {
		return HostFirewallView{}, errFirewallPending
	}
	hc := m.host()
	hc.fwMu.Lock()
	if _, err := m.loadFirewall(ctx); err != nil {
		hc.fwMu.Unlock()
		return HostFirewallView{}, err
	}
	m.mu.Lock()
	st := m.data.HostFirewall
	if st.Pending != nil {
		m.mu.Unlock()
		hc.fwMu.Unlock()
		return HostFirewallView{}, errFirewallPending
	}
	previous := st.Model
	st.Model = model
	if err := m.save(); err != nil {
		st.Model = previous
		m.mu.Unlock()
		hc.fwMu.Unlock()
		return HostFirewallView{}, err
	}
	m.mu.Unlock()
	hc.fwMu.Unlock()
	return m.firewallView(ctx)
}

// firewallBusy reports an apply in progress or awaiting confirmation, without
// waiting for the firewall lock.
func (m *Manager) firewallBusy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data.HostFirewall != nil && m.data.HostFirewall.Pending != nil
}

// listeningPorts returns public (non-loopback) listening sockets as
// "port/proto" with the owning process, for lockout warnings.
func listeningPorts(ctx context.Context) map[string]string {
	out, err := hostCommand(ctx, "ss", "-H", "-l", "-n", "-t", "-u", "-p")
	ports := map[string]string{}
	if err != nil {
		return ports
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		local := fields[4]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		address, port := strings.Trim(strings.Split(local[:i], "%")[0], "[]"), local[i+1:]
		if ip := net.ParseIP(address); ip != nil && (ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
			continue
		}
		process := ""
		if len(fields) >= 7 {
			if start := strings.Index(fields[6], `(("`); start >= 0 {
				rest := fields[6][start+3:]
				process = rest[:max(0, strings.Index(rest, `"`))]
			}
		}
		ports[port+"/"+fields[0]] = process
	}
	return ports
}

func portCovered(port, proto string, rules []HostFirewallRule) bool {
	n, _ := strconv.Atoi(port)
	for _, r := range rules {
		if r.Proto != proto && r.Proto != "any" {
			continue
		}
		low, high, isRange := strings.Cut(r.Port, ":")
		if !isRange {
			high = low
		}
		l, _ := strconv.Atoi(low)
		h, _ := strconv.Atoi(high)
		if n >= l && n <= h {
			return true
		}
	}
	return false
}

// firewallPlan computes the reviewed command list. Caller holds hc.fwMu.
func (m *Manager) firewallPlanLocked(ctx context.Context) (HostFirewallPlan, []firewallStep, firewallContext, error) {
	plan := HostFirewallPlan{Changes: []HostFirewallChange{}, ConfirmSeconds: int(firewallConfirmWindow.Seconds())}
	fc, err := m.loadFirewall(ctx)
	if err != nil {
		return plan, nil, fc, err
	}
	model := fc.state.Model
	steps, unmanaged := planUfw(fc.host, fc.state.Owned, fc.desired, model.Enabled, model.DefaultInbound)
	if model.Enabled && !fc.host.Installed {
		steps = append([]firewallStep{{Action: "install", Args: []string{"apt-get", "install", "-y", "ufw"}}}, steps...)
	}
	for _, s := range steps {
		command := s.command()
		if s.Action == "install" {
			command = strings.Join(s.Args, " ")
		}
		plan.Changes = append(plan.Changes, HostFirewallChange{Action: s.Action, Command: command})
	}
	for _, a := range unmanaged {
		plan.Unmanaged = append(plan.Unmanaged, a.command(true))
	}
	plan.Kept = fc.host.Kept
	plan.Fingerprint = planFingerprint(steps)
	if model.Enabled {
		for key, process := range listeningPorts(ctx) {
			port, proto, _ := strings.Cut(key, "/")
			if !portCovered(port, proto, fc.rules) && !strings.HasPrefix(process, "docker-proxy") {
				label := key
				if process != "" {
					label += " (" + process + ")"
				}
				plan.Warnings = append(plan.Warnings, "Port "+label+" listens on a public address and will be blocked.")
			}
		}
		slices.Sort(plan.Warnings)
		if len(model.SSHFrom) > 0 {
			plan.Warnings = append(plan.Warnings, "SSH will be reachable only from "+strings.Join(model.SSHFrom, ", ")+".")
		}
	}
	if len(steps) > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("After applying, confirm within %d seconds or the previous firewall is restored automatically.", plan.ConfirmSeconds))
	}
	return plan, steps, fc, nil
}

func (m *Manager) firewallPlan(ctx context.Context) (HostFirewallPlan, error) {
	if err := m.firewallSupported(); err != nil {
		return HostFirewallPlan{}, err
	}
	if m.firewallBusy() {
		return HostFirewallPlan{}, errFirewallPending
	}
	hc := m.host()
	hc.fwMu.Lock()
	defer hc.fwMu.Unlock()
	plan, _, _, err := m.firewallPlanLocked(ctx)
	return plan, err
}

// runFirewallSteps stops at the first failure, unless keepGoing (a rollback
// restores as much of the previous ruleset as it can).
func runFirewallSteps(ctx context.Context, steps []firewallStep, keepGoing bool) (string, error) {
	var transcript strings.Builder
	var failures []error
	for _, s := range steps {
		name, args := "ufw", s.Args
		if s.Atom != nil {
			args = s.Atom.args(s.Action != "delete")
			if s.Action == "delete" {
				args = append([]string{"--force", "delete"}, args...)
			}
		}
		if s.Action == "install" {
			name, args = s.Args[0], append(slices.Clone(s.Args[1:]), "-q", "-o", "DPkg::Lock::Timeout=300")
		}
		out, err := hostCommand(ctx, name, args...)
		transcript.WriteString("$ " + name + " " + strings.Join(args, " ") + "\n" + strings.TrimSpace(out) + "\n")
		if err != nil {
			failures = append(failures, fmt.Errorf("%s failed: %w", s.command(), err))
			if !keepGoing {
				break
			}
		}
	}
	return transcript.String(), errors.Join(failures...)
}

// applyFirewall starts the reviewed plan in the background and returns at
// once; the panel polls the firewall until the change is applied and then
// confirms it. The pending rollback snapshot is saved before any command runs,
// so a crash or restart mid-apply still restores the previous firewall.
func (m *Manager) applyFirewall(ctx context.Context, in HostFirewallApply) (HostFirewallView, error) {
	if err := m.firewallSupported(); err != nil {
		return HostFirewallView{}, err
	}
	if m.firewallBusy() {
		return HostFirewallView{}, errFirewallPending
	}
	hc := m.host()
	hc.fwMu.Lock()
	plan, steps, fc, err := m.firewallPlanLocked(ctx)
	if err != nil {
		hc.fwMu.Unlock()
		return HostFirewallView{}, err
	}
	if fc.state.Pending != nil {
		hc.fwMu.Unlock()
		return HostFirewallView{}, errFirewallPending
	}
	if in.Fingerprint == "" || in.Fingerprint != plan.Fingerprint {
		hc.fwMu.Unlock()
		return HostFirewallView{}, errors.New("the firewall plan changed since it was reviewed; review the changes again")
	}
	if len(steps) == 0 {
		hc.fwMu.Unlock()
		return m.firewallView(ctx)
	}
	now := time.Now().UTC()
	m.mu.Lock()
	st := m.data.HostFirewall
	st.Pending = &HostFirewallPending{Deadline: now.Add(firewallConfirmWindow + 10*time.Minute), Previous: hostFirewallSnapshot{Active: fc.host.Active, DefaultIncoming: fc.host.DefaultIncoming, Owned: slices.Clone(st.Owned), Model: st.Model}}
	st.LastError = ""
	job := newHostJob("firewall", "manual", now)
	job.Summary = "Applying firewall change."
	m.recordHostJobLocked(job)
	if err := m.save(); err != nil {
		st.Pending = nil
		m.mu.Unlock()
		hc.fwMu.Unlock()
		return HostFirewallView{}, err
	}
	m.mu.Unlock()
	hc.mu.Lock()
	hc.fwApplying = true
	hc.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer hc.fwMu.Unlock()
		applyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		output, runErr := runFirewallSteps(applyCtx, steps, false)
		cancel()
		finished := time.Now().UTC()
		job.Finished, job.Output = &finished, output
		m.mu.Lock()
		if runErr == nil {
			st.Owned = slices.Clone(fc.desired)
			job.Status, job.Summary = "succeeded", "Firewall change applied; waiting for confirmation."
			// The confirmation window starts when the change is in place.
			st.Pending.Deadline = finished.Add(firewallConfirmWindow)
		} else {
			job.Status, job.Summary = "failed", runErr.Error()
		}
		m.recordHostJobLocked(job)
		_ = m.save()
		deadline := st.Pending.Deadline
		m.mu.Unlock()
		hc.mu.Lock()
		hc.fwApplying = false
		hc.mu.Unlock()
		if runErr != nil {
			m.revertFirewallLocked("applying it failed: " + runErr.Error())
			return
		}
		m.scheduleFirewallRevert(deadline)
		if _, ok := m.runtime.(*Docker); ok {
			// Enabling ufw must not reorder forwarding: re-check the agent's Docker
			// public-port chain, which reports any problem in port inventory.
			m.reconcilePublicPorts()
		}
	}()
	return m.firewallSnapshotView(), nil
}

// firewallSnapshotView describes the firewall from agent state alone, while
// an apply holds the host.
func (m *Manager) firewallSnapshotView() HostFirewallView {
	st := m.firewallStateCopy()
	hc := m.host()
	hc.mu.Lock()
	applying := hc.fwApplying
	hc.mu.Unlock()
	view := HostFirewallView{Supported: true, Backend: "ufw", Applying: applying, Model: st.Model, Rules: slices.Clone(st.Model.Rules), LastApplied: st.LastApplied, LastError: st.LastError}
	if view.Rules == nil {
		view.Rules = []HostFirewallRule{}
	}
	if st.Pending != nil && !applying {
		view.PendingDeadline = st.Pending.Deadline
		view.PendingSeconds = max(0, int(time.Until(st.Pending.Deadline).Seconds()))
	}
	return view
}

func (m *Manager) scheduleFirewallRevert(deadline time.Time) {
	hc := m.host()
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if hc.fwTimer != nil {
		hc.fwTimer.Stop()
	}
	hc.fwTimer = time.AfterFunc(max(0, time.Until(deadline)), func() {
		if m.ctx.Err() != nil {
			return // the persisted pending change is reverted on the next start
		}
		m.revertFirewall(fmt.Sprintf("it was not confirmed within %d seconds", int(firewallConfirmWindow.Seconds())))
	})
}

// confirmFirewall keeps the applied change. The panel calls it after it has
// reached the agent again through its normal connection.
func (m *Manager) confirmFirewall(ctx context.Context) (HostFirewallView, error) {
	if err := m.firewallSupported(); err != nil {
		return HostFirewallView{}, err
	}
	hc := m.host()
	hc.mu.Lock()
	applying := hc.fwApplying
	hc.mu.Unlock()
	if applying {
		return HostFirewallView{}, errors.New("the firewall change is still being applied; confirm it once it is in place")
	}
	hc.fwMu.Lock()
	m.mu.Lock()
	st := m.data.HostFirewall
	if st == nil || st.Pending == nil {
		message := "no firewall change is waiting for confirmation"
		if st != nil && st.LastError != "" {
			message += "; " + st.LastError
		}
		m.mu.Unlock()
		hc.fwMu.Unlock()
		return HostFirewallView{}, errors.New(message)
	}
	pending := st.Pending
	st.Pending = nil
	st.LastApplied = time.Now().UTC()
	if err := m.save(); err != nil {
		st.Pending = pending
		m.mu.Unlock()
		hc.fwMu.Unlock()
		return HostFirewallView{}, err
	}
	m.mu.Unlock()
	hc.mu.Lock()
	if hc.fwTimer != nil {
		hc.fwTimer.Stop()
		hc.fwTimer = nil
	}
	hc.mu.Unlock()
	hc.fwMu.Unlock()
	return m.firewallView(ctx)
}

// revertFirewall restores the snapshot taken before the pending change.
func (m *Manager) revertFirewall(reason string) {
	hc := m.host()
	hc.fwMu.Lock()
	defer hc.fwMu.Unlock()
	m.revertFirewallLocked(reason)
}

// revertFirewallLocked is revertFirewall for a caller holding hc.fwMu.
func (m *Manager) revertFirewallLocked(reason string) {
	hc := m.host()
	hc.mu.Lock()
	if hc.fwTimer != nil {
		hc.fwTimer.Stop()
		hc.fwTimer = nil
	}
	hc.mu.Unlock()
	m.mu.Lock()
	st := m.data.HostFirewall
	if st == nil || st.Pending == nil {
		m.mu.Unlock()
		return
	}
	previous := st.Pending.Previous
	owned := slices.Clone(st.Owned)
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var output string
	host, err := readUfwHost(ctx)
	if err == nil {
		steps, _ := planUfw(host, append(owned, previous.Owned...), previous.Owned, previous.Active, previous.DefaultIncoming)
		output, err = runFirewallSteps(ctx, steps, true)
	}
	now := time.Now().UTC()
	job := newHostJob("firewall", "rollback", now)
	job.Finished, job.Output = &now, output
	job.Status, job.Summary = "succeeded", "Previous firewall restored because "+reason+"."
	m.mu.Lock()
	defer m.mu.Unlock()
	st.LastError = "The firewall change was rolled back because " + reason + "."
	if err != nil {
		job.Status = "failed"
		job.Summary = "Restoring the previous firewall failed: " + err.Error()
		st.LastError += " Restoring the previous rules failed (" + err.Error() + "); inspect ufw on the host."
	}
	st.Model, st.Owned, st.Pending = previous.Model, previous.Owned, nil
	m.recordHostJobLocked(job)
	if err := m.save(); err != nil {
		log.Printf("firewall rollback status: %v", err)
	}
	log.Printf("host firewall: %s", job.Summary)
}

// firewallRecover runs from the maintenance tick: it closes interrupted job
// records once after start and re-arms (or performs) a pending rollback.
func (m *Manager) firewallRecover(now time.Time) {
	hc := m.host()
	// An apply in progress owns the pending record and arms its own timer.
	if !hc.fwMu.TryLock() {
		return
	}
	defer hc.fwMu.Unlock()
	hc.mu.Lock()
	first := !hc.recovered
	hc.recovered = true
	armed := hc.fwTimer != nil
	hc.mu.Unlock()
	m.mu.Lock()
	if first && m.recoverHostJobsLocked(now) {
		_ = m.save()
	}
	var deadline time.Time
	pending := m.data.HostFirewall != nil && m.data.HostFirewall.Pending != nil
	if pending {
		deadline = m.data.HostFirewall.Pending.Deadline
	}
	m.mu.Unlock()
	if pending && !armed {
		m.scheduleFirewallRevert(deadline)
	}
}
