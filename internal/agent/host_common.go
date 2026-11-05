package agent

// Host operating-system management shared by host updates, the managed host
// firewall and on-demand Docker installation. Everything here acts on the
// whole machine, so it is unavailable to agents that share a host (coexist),
// observe native services only, or serve ingress only.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// HostJob is one host-level operation the agent ran: package updates, a
// reboot, unattended-upgrades configuration or a firewall change. A bounded
// history stays in agent state so the panel can show what happened even after
// the agent restarts (for example after a reboot).
type HostJob struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"`    // security-updates, all-updates, configure, reboot, firewall
	Trigger  string     `json:"trigger"` // manual or schedule
	Status   string     `json:"status"`  // running, succeeded, failed
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
	Summary  string     `json:"summary,omitempty"`
	Output   string     `json:"output,omitempty"`
	BootID   string     `json:"boot_id,omitempty"` // reboot jobs: the boot they were issued from
}

const (
	hostJobHistory   = 20
	hostJobOutputCap = 16 << 10
)

// hostController holds runtime-only coordination for one manager; durable
// policy and history live in diskState.
type hostController struct {
	mu         sync.Mutex
	status     *HostUpdatesStatus
	refreshing bool
	job        string
	fwMu       sync.Mutex // serialises firewall reads and changes
	fwTimer    *time.Timer
	fwApplying bool
	recovered  bool
}

var hostControllers sync.Map // *Manager -> *hostController

func (m *Manager) host() *hostController {
	value, _ := hostControllers.LoadOrStore(m, &hostController{})
	return value.(*hostController)
}

// hostCommand runs a host tool non-interactively with bounded output. Package
// operations never prompt, keep existing configuration files and only list
// (never restart) services that need a restart, so hosted applications are not
// interrupted by a library update.
var hostCommand = func(ctx context.Context, name string, args ...string) (string, error) {
	out := &cappedConsoleBuffer{}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C", "NEEDRESTART_MODE=l", "APT_LISTCHANGES_FRONTEND=none")
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	return out.String(), err
}

// hostPath resolves a host file; tests point it at a fixture directory.
var hostPath = func(path string) string { return path }

func hostFileExists(path string) bool {
	_, err := os.Stat(hostPath(path))
	return err == nil
}

func hostToolAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// hostOperationsAllowed reports why host-wide operations are refused.
func (m *Manager) hostOperationsAllowed() error {
	switch {
	case m.cfg.IngressOnly:
		return errors.New("host management is unavailable on an ingress-only agent")
	case m.cfg.NativeOnly:
		return errors.New("host management is unavailable on a native-only observer agent")
	case m.cfg.HostMode == "coexist":
		return errors.New("host management is disabled in coexist mode; the host is shared with another manager")
	}
	return nil
}

// aptHost reports whether this machine is a Debian-family host with apt and
// systemd, the only supported platform for package and reboot management.
func aptHost() (bool, string) {
	raw, err := os.ReadFile(hostPath("/etc/os-release"))
	if err != nil {
		return false, "the operating system could not be identified"
	}
	release := parseOSRelease(string(raw))
	family := " " + release["ID"] + " " + release["ID_LIKE"] + " "
	if !strings.Contains(family, " debian ") && !strings.Contains(family, " ubuntu ") {
		return false, "host package management supports Ubuntu and Debian only"
	}
	if !hostFileExists("/run/systemd/system") {
		return false, "host package management requires systemd"
	}
	return true, ""
}

func parseOSRelease(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		out[key] = strings.Trim(value, `"'`)
	}
	return out
}

// hostBusyLocked names the work that a reboot or package run must not
// interrupt. Caller holds m.mu.
func (m *Manager) hostBusyLocked() string {
	if m.cleanupBusyLocked() {
		return "deployments, database backups, restores or agent upgrades are running"
	}
	if m.data.Maintenance != nil && m.data.Maintenance.Running {
		return "server cleanup is running"
	}
	if len(m.data.NativeTransitions) > 0 || len(m.nativeOperations) > 0 {
		return "a native application release or backup is running"
	}
	for _, t := range m.data.Tasks {
		if t.Running || t.Launching {
			return "an application task is running"
		}
	}
	for _, c := range m.data.Caches {
		if c.Operation {
			return "a cache operation is running"
		}
	}
	return ""
}

// recordHostJobLocked inserts or replaces a job record, newest first, and keeps
// the history bounded. Caller holds m.mu and saves afterwards.
func (m *Manager) recordHostJobLocked(job HostJob) {
	state := m.hostUpdatesStateLocked()
	if len(job.Output) > hostJobOutputCap {
		job.Output = job.Output[len(job.Output)-hostJobOutputCap:]
	}
	for i := range state.Jobs {
		if state.Jobs[i].ID == job.ID {
			state.Jobs[i] = job
			return
		}
	}
	state.Jobs = append([]HostJob{job}, state.Jobs...)
	if len(state.Jobs) > hostJobHistory {
		state.Jobs = state.Jobs[:hostJobHistory]
	}
}

func newHostJob(kind, trigger string, now time.Time) HostJob {
	return HostJob{ID: kind + "-" + trigger + "-" + now.UTC().Format("20060102T150405.000Z"), Kind: kind, Trigger: trigger, Status: "running", Started: now.UTC()}
}

// hostTick runs from the agent's maintenance loop once a minute: it refreshes
// the cached update status, enforces the reboot window and recovers a firewall
// change whose confirmation window expired while the agent was down.
func (m *Manager) hostTick(now time.Time) {
	if m.hostOperationsAllowed() != nil {
		return
	}
	m.firewallRecover(now)
	m.configureTick()
	m.refreshHostUpdatesIfStale(now)
	m.rebootTick(now)
}
