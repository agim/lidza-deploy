package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"
)

// System log access for the shared deployment account.
//
// On hosts taken over from an existing setup the shared deployment account
// (native.user, the same account service control names) is how operators
// sign in over SSH. Adopted applications now run as system units of their
// own accounts, so `journalctl -u <unit>` shows that account nothing unless
// it may read the system journal. This per-server setting adds the account
// to the systemd-journal group when on and removes that one membership when
// off; no other group is ever touched. The agent applies a change at once
// and reconciles the membership at agent start and every maintenance tick,
// so a membership changed by hand is put back to the setting. While the
// panel never set it, the agent leaves the membership as it is. A group
// change applies to new SSH sessions only.

const (
	logAccessGroup   = "systemd-journal"
	logAccessJobKind = "log-access"
)

// HostLogAccessState is the panel's setting and the last reconcile result.
type HostLogAccessState struct {
	Enabled bool   `json:"enabled"`
	Error   string `json:"error,omitempty"`
	// Applied is when the agent last changed the membership.
	Applied *time.Time `json:"applied,omitempty"`
}

// LogAccessStatus is the server's system log access setting.
type LogAccessStatus struct {
	// Supported is false unless native management with a shared deployment
	// account is configured.
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	Enabled   bool   `json:"enabled"`
	// Source is "setting" once the panel set it; until then the agent does
	// not change the membership.
	Source  string `json:"source"`
	Account string `json:"account,omitempty"`
	Group   string `json:"group"`
	// Member reports whether the account is in the group now.
	Member bool `json:"member"`
	// Error is the last reconcile failure.
	Error   string     `json:"error,omitempty"`
	Applied *time.Time `json:"applied,omitempty"`
}

// LogAccessSettings is the panel's change of the setting.
type LogAccessSettings struct {
	Enabled *bool `json:"enabled"`
}

// Validate checks a change before it is stored or forwarded.
func (s LogAccessSettings) Validate() error {
	if s.Enabled == nil {
		return errors.New("enabled is required")
	}
	return nil
}

// logAccessAccount is the shared deployment account, resolved as for
// service control, or the reason there is none.
func (m *Manager) logAccessAccountLocked() (string, string) {
	if m.cfg.IngressOnly {
		return "", "host management is unavailable on an ingress-only agent"
	}
	if m.cfg.Native == nil {
		return "", "native management is not configured on this server"
	}
	account := strings.TrimSpace(m.cfg.Native.User)
	if account == "" {
		return "", "this server has no shared deployment account"
	}
	if account == "root" {
		return "", "the shared deployment account must not be root"
	}
	return account, ""
}

// logAccessGroupMembers reads the systemd-journal group's supplementary
// members from /etc/group; found is false when the group does not exist.
func logAccessGroupMembers() (members []string, found bool, err error) {
	file, err := os.Open(hostPath("/etc/group"))
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) != 4 || fields[0] != logAccessGroup {
			continue
		}
		for _, member := range strings.Split(fields[3], ",") {
			if member = strings.TrimSpace(member); member != "" {
				members = append(members, member)
			}
		}
		return members, true, nil
	}
	return nil, false, scanner.Err()
}

func logAccessMember(account string) bool {
	members, _, err := logAccessGroupMembers()
	return err == nil && slices.Contains(members, account)
}

func (m *Manager) logAccessStatus() LogAccessStatus {
	m.mu.Lock()
	account, reason := m.logAccessAccountLocked()
	status := LogAccessStatus{Source: "config", Group: logAccessGroup, Account: account, Reason: reason, Supported: reason == ""}
	if state := m.data.HostLogAccess; state != nil && status.Supported {
		status.Enabled, status.Source, status.Error, status.Applied = state.Enabled, "setting", state.Error, state.Applied
	}
	m.mu.Unlock()
	if status.Supported {
		status.Member = logAccessMember(account)
	}
	return status
}

// setLogAccess stores the panel's setting and applies it at once.
func (m *Manager) setLogAccess(ctx context.Context, s LogAccessSettings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	if _, reason := m.logAccessAccountLocked(); reason != "" {
		m.mu.Unlock()
		return errors.New(reason)
	}
	previous := m.data.HostLogAccess
	next := HostLogAccessState{Enabled: *s.Enabled}
	if previous != nil {
		next.Applied = previous.Applied
	}
	m.data.HostLogAccess = &next
	if err := m.save(); err != nil {
		m.data.HostLogAccess = previous
		m.mu.Unlock()
		return errors.New("could not save the system log access setting")
	}
	m.mu.Unlock()
	m.reconcileLogAccess(ctx, "manual")
	return nil
}

// logAccessTick runs from hostTick: at agent start and once a minute. It
// only runs commands when the membership differs from the setting.
func (m *Manager) logAccessTick(now time.Time) {
	if m.nativeCommand == nil && (runtime.GOOS != "linux" || os.Geteuid() != 0) {
		return
	}
	trigger := "schedule"
	m.logAccessMu.Lock()
	if !m.logAccessBooted {
		m.logAccessBooted, trigger = true, "boot"
	}
	m.logAccessMu.Unlock()
	ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
	defer cancel()
	m.reconcileLogAccess(ctx, trigger)
}

// reconcileLogAccess adds the shared deployment account to, or removes it
// from, the systemd-journal group to match the stored setting. It changes
// only that one membership and records a host job when it changed it or
// when the failure is new.
func (m *Manager) reconcileLogAccess(ctx context.Context, trigger string) {
	m.logAccessMu.Lock()
	defer m.logAccessMu.Unlock()
	m.mu.Lock()
	account, reason := m.logAccessAccountLocked()
	state := m.data.HostLogAccess
	var want bool
	var lastError string
	if state != nil {
		want, lastError = state.Enabled, state.Error
	}
	m.mu.Unlock()
	if reason != "" || state == nil {
		return
	}
	started := time.Now().UTC()
	members, found, err := logAccessGroupMembers()
	changed := false
	var transcript strings.Builder
	switch {
	case err != nil:
		err = fmt.Errorf("could not read the %s group: %w", logAccessGroup, err)
	case !found && want:
		err = fmt.Errorf("the %s group does not exist on this server", logAccessGroup)
	case want && !slices.Contains(members, account):
		args := []string{"gpasswd", "--add", account, logAccessGroup}
		transcript.WriteString("$ " + strings.Join(args, " ") + "\n")
		out, runErr := m.nativeRootExec(ctx, args...)
		transcript.WriteString(out)
		if runErr != nil {
			err = fmt.Errorf("could not add %s to the %s group: %v", account, logAccessGroup, runErr)
		} else {
			changed = true
		}
	case !want && slices.Contains(members, account):
		args := []string{"gpasswd", "--delete", account, logAccessGroup}
		transcript.WriteString("$ " + strings.Join(args, " ") + "\n")
		out, runErr := m.nativeRootExec(ctx, args...)
		transcript.WriteString(out)
		if runErr != nil {
			err = fmt.Errorf("could not remove %s from the %s group: %v", account, logAccessGroup, runErr)
		} else {
			changed = true
		}
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	if !changed && message == lastError {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.data.HostLogAccess
	if current == nil || current.Enabled != want {
		// The setting changed meanwhile; its own reconcile records it.
		return
	}
	current.Error = message
	if changed {
		applied := time.Now().UTC()
		current.Applied = &applied
	}
	if changed || err != nil {
		job := newHostJob(logAccessJobKind, trigger, started)
		finished := time.Now().UTC()
		job.Finished, job.Output = &finished, strings.TrimSpace(transcript.String())
		switch {
		case err != nil:
			job.Status, job.Summary = "failed", "System log access: "+message
		case want:
			job.Status, job.Summary = "succeeded", account+" can read the system journal (new SSH sessions)."
		default:
			job.Status, job.Summary = "succeeded", account+" no longer reads the system journal."
		}
		m.recordHostJobLocked(job)
		log.Printf("log access (%s): %s", trigger, job.Summary)
	}
	if saveErr := m.save(); saveErr != nil {
		log.Printf("log access state: %v", saveErr)
	}
}

func (m *Manager) logAccessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/host/log-access", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, 200, m.logAccessStatus())
	})
	mux.HandleFunc("PUT /v1/host/log-access", func(w http.ResponseWriter, r *http.Request) {
		var s LogAccessSettings
		if err := Decode(w, r, &s); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := s.Validate(); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		if err := m.setLogAccess(ctx, s); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, m.logAccessStatus())
	})
}
