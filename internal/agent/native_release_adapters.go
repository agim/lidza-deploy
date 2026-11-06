package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// nativeReleaseServices guards the owner's units: preserved units (kept
// stopped or disabled by the operator) are never restarted or started.
type nativeReleaseServices struct {
	inner    release.Services
	preserve map[string]bool
}

func (s nativeReleaseServices) State(ctx context.Context, unit string) (release.ServiceState, error) {
	return s.inner.State(ctx, unit)
}

func (s nativeReleaseServices) Restart(ctx context.Context, unit string) error {
	if s.preserve[unit] {
		return fmt.Errorf("%s is preserved by the operator and is never restarted by a release", unit)
	}
	return s.inner.Restart(ctx, unit)
}

// Inspect reads a unit's state and journal from the owner's user manager
// for the deploy log after a failed health check.
func (s nativeReleaseServices) Inspect(ctx context.Context, unit string, since time.Time) (release.UnitReport, error) {
	inspector, ok := s.inner.(release.Inspector)
	if !ok {
		return release.UnitReport{}, errors.New("the service manager cannot be inspected")
	}
	return inspector.Inspect(ctx, unit, since)
}

// nativeReleaseMask hides the application's environment values (its
// secrets) in diagnostic output that reaches the deploy log, with the same
// exemptions for small non-secret values as the application's log view.
func nativeReleaseMask(policy NativeReleasePolicy) func(string) string {
	values, err := nativeReleaseFileEnvironment(policy)
	if err != nil {
		return nil
	}
	secrets := nativeEnvironmentLogSecrets(values)
	if len(secrets) == 0 {
		return nil
	}
	return func(text string) string { return scrubOutput(text, secrets) }
}

// nativeHealthResponse records a refused health answer with the start of
// its body, so the deploy log can show what the application said.
func nativeHealthResponse(response *http.Response, where string) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, release.HealthBodyLimit))
	return fmt.Errorf("%w from %s", &release.HealthResponse{Status: response.StatusCode, Body: string(body)}, where)
}

func (m *Manager) nativeReleaseServices(account nativeReleaseAccount, policy NativeReleasePolicy) release.Services {
	preserve := map[string]bool{}
	for _, unit := range policy.Preserve {
		preserve[unit] = true
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + account.Home, "XDG_RUNTIME_DIR=/run/user/" + fmt.Sprint(account.UID)}
	return nativeReleaseServices{inner: release.UserSystemd{Exec: nativeReleaseUserExec{m: m, account: account}, Env: env}, preserve: preserve}
}

// nativeReleaseCrontab replaces the owner's crontab as root. A write must
// keep every line outside the application's own schedule block.
type nativeReleaseCrontab struct {
	m          *Manager
	user       string
	identifier string
}

const nativeReleaseCrontabLimit = 256 << 10

func (t nativeReleaseCrontab) exec(ctx context.Context, stdin string, args ...string) (string, string, error) {
	if t.m.nativeCrontabCommand != nil {
		return t.m.nativeCrontabCommand(ctx, stdin, args)
	}
	cmd := exec.CommandContext(ctx, "crontab", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	var stdout, stderr bytes.Buffer
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (t nativeReleaseCrontab) Read(ctx context.Context) (string, error) {
	stdout, stderr, err := t.exec(ctx, "", "-u", t.user, "-l")
	if err != nil {
		if strings.Contains(stderr, "no crontab for") {
			return "", nil
		}
		return "", fmt.Errorf("crontab -l for %s failed: %w", t.user, err)
	}
	if len(stdout) > nativeReleaseCrontabLimit {
		return "", errors.New("crontab exceeds the supported size")
	}
	return stdout, nil
}

func (t nativeReleaseCrontab) Write(ctx context.Context, body string) error {
	if len(body) > nativeReleaseCrontabLimit || strings.ContainsRune(body, 0) {
		return errors.New("crontab body exceeds the supported size")
	}
	existing, err := t.Read(ctx)
	if err != nil {
		return err
	}
	foreign, err := release.ReplaceScheduleBlock(existing, t.identifier, "")
	if err != nil {
		return err
	}
	lines := map[string]int{}
	for _, line := range strings.Split(body, "\n") {
		lines[line]++
	}
	for _, line := range strings.Split(foreign, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if lines[line] == 0 {
			return errors.New("crontab write would drop an entry outside the application's schedule block; refused")
		}
		lines[line]--
	}
	if _, stderr, err := t.exec(ctx, body, "-u", t.user, "-"); err != nil {
		detail := strings.TrimSpace(stderr)
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return fmt.Errorf("crontab install for %s failed: %w %s", t.user, err, detail)
	}
	return nil
}

// nativeReleaseBackup backs up every protected database the policy declares
// before any release step runs. Without declared databases the hook fails
// closed, so an application with migrations cannot skip backups by omission.
func (m *Manager) nativeReleaseBackup(appID string, policy NativeReleasePolicy) func(context.Context) error {
	return func(ctx context.Context) error {
		if len(policy.Databases) == 0 {
			return errors.New("release steps change databases; declare the application's protected databases in its release policy")
		}
		roles := []string{}
		m.mu.Lock()
		for _, id := range policy.Databases {
			binding, bound := m.data.NativeDatabases[id]
			if !bound || binding.Policy.AppID != appID {
				m.mu.Unlock()
				return fmt.Errorf("database %s is not a protected resource of this application", id)
			}
			for _, role := range binding.Policy.Roles {
				if !slices.Contains(roles, role) {
					roles = append(roles, role)
				}
			}
		}
		m.mu.Unlock()
		slices.Sort(roles)
		run := m.nativeBackupRun
		if run == nil {
			run = m.backupBeforeNativeRailsRelease
		}
		_, err := run(ctx, appID, roles)
		return err
	}
}

// nativeHealthDeadline is how long a health check keeps retrying; a
// restarted server gets that time to boot. nativeHealthRetry is the pause
// between attempts. Tests shorten both.
var (
	nativeHealthDeadline = 90 * time.Second
	nativeHealthRetry    = 2 * time.Second
)

// nativeReleaseHealth requests the application's health path through the
// local web server with the domain as SNI and Host, without DNS. Any 2xx
// within the deadline passes; a restarted server gets time to boot.
func (m *Manager) nativeReleaseHealth(domain, path string) func(context.Context) error {
	if domain == "" {
		return nil
	}
	if path == "" {
		path = "/up"
	}
	return func(ctx context.Context) error {
		address := m.nativeReleaseHealthAddress
		if address == "" {
			address = "127.0.0.1:443"
		}
		config := m.nativeReleaseHealthTLS
		if config == nil {
			config = &tls.Config{ServerName: domain, MinVersion: tls.VersionTLS12}
		}
		dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: config}
		client := &http.Client{Timeout: 15 * time.Second,
			Transport: &http.Transport{DisableKeepAlives: true, DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp", address)
			}},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		deadline := time.Now().Add(nativeHealthDeadline)
		var last error
		for {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+path, nil)
			if err != nil {
				return err
			}
			request.Host = domain
			response, err := client.Do(request)
			if err == nil {
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					response.Body.Close()
					return nil
				}
				last = nativeHealthResponse(response, domain+path)
				response.Body.Close()
			} else {
				last = err
			}
			if time.Now().After(deadline) {
				return last
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(nativeHealthRetry):
			}
		}
	}
}

// nativeSystemServices controls an application's system units as the root
// agent. Preserved units are never restarted or started.
type nativeSystemServices struct {
	m        *Manager
	preserve map[string]bool
}

func (s nativeSystemServices) State(ctx context.Context, unit string) (release.ServiceState, error) {
	if !nativeReleaseUnitPattern.MatchString(unit) {
		return release.ServiceState{}, fmt.Errorf("invalid unit %q", unit)
	}
	// A status probe, not deployment output: keep it out of the log.
	out, err := s.m.nativeRootExec(withoutDeploymentLog(ctx), "systemctl", "show", unit, "--no-pager", "--property=ActiveState,SubState,UnitFileState")
	if err != nil {
		return release.ServiceState{}, err
	}
	var state release.ServiceState
	for _, line := range strings.Split(out, "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "ActiveState":
			state.Active = value
		case "SubState":
			state.Sub = value
		case "UnitFileState":
			state.UnitFile = value
		}
	}
	return state, nil
}

// Inspect reads a unit's state and journal as the root agent for the
// deploy log after a failed health check. The probes themselves stay out
// of the log; the engine writes the masked report.
func (s nativeSystemServices) Inspect(ctx context.Context, unit string, since time.Time) (release.UnitReport, error) {
	if !nativeReleaseUnitPattern.MatchString(unit) {
		return release.UnitReport{}, fmt.Errorf("invalid unit %q", unit)
	}
	quiet := withoutDeploymentLog(ctx)
	var report release.UnitReport
	state, err := s.m.nativeRootExec(quiet, "systemctl", "show", unit, "--no-pager", "--property="+release.UnitReportProperties)
	if err != nil {
		return report, err
	}
	report.State = state
	report.Log, err = s.m.nativeRootExec(quiet, append([]string{"journalctl"}, release.JournalArgs(unit, since)...)...)
	return report, err
}

func (s nativeSystemServices) Restart(ctx context.Context, unit string) error {
	if s.preserve[unit] {
		return fmt.Errorf("%s is preserved by the operator and is never restarted by a release", unit)
	}
	if !nativeReleaseUnitPattern.MatchString(unit) {
		return fmt.Errorf("invalid unit %q", unit)
	}
	// systemctl prints nothing on success; its message explains a failure.
	var output strings.Builder
	quiet := context.WithValue(withoutDeploymentLog(ctx), diagnosticKey{}, func(line string) { output.WriteString(line) })
	_, err := s.m.nativeRootExec(quiet, "systemctl", "restart", unit)
	if err != nil {
		if detail := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(output.String()), "systemctl")); detail != "" {
			commandDiagnostic(ctx, "systemctl restart "+unit, detail)
		}
	}
	return err
}

// withoutDeploymentLog hides the deployment log of ctx from host commands
// whose output is only parsed (status probes).
func withoutDeploymentLog(ctx context.Context) context.Context {
	return context.WithValue(context.WithValue(ctx, diagnosticKey{}, nil), diagnosticStreamKey{}, nil)
}

func (m *Manager) nativeSystemServices(policy NativeReleasePolicy) nativeSystemServices {
	preserve := map[string]bool{}
	for _, unit := range policy.Preserve {
		preserve[unit] = true
	}
	return nativeSystemServices{m: m, preserve: preserve}
}

// nativeSystemRestart publishes a release to system units: every unit that
// is running or enabled is restarted (started, on the first release) and
// must be running again within settle. Disabled units that are not running
// were stopped by the operator and stay stopped.
func nativeSystemRestart(ctx context.Context, s release.Services, units, preserve []string, settle time.Duration, log io.Writer) ([]string, error) {
	var restarted []string
	for _, unit := range units {
		if slices.Contains(preserve, unit) {
			continue
		}
		state, err := s.State(ctx, unit)
		if err != nil {
			return restarted, err
		}
		if !state.Running() && state.UnitFile != "enabled" {
			if log != nil {
				fmt.Fprintf(log, "==> leaving %s %s (%s)\n", unit, state.Active, state.UnitFile)
			}
			continue
		}
		if log != nil {
			fmt.Fprintf(log, "==> restarting %s\n", unit)
		}
		if err := s.Restart(ctx, unit); err != nil {
			return restarted, fmt.Errorf("restart %s: %w", unit, err)
		}
		deadline := time.Now().Add(settle)
		for {
			state, err := s.State(ctx, unit)
			if err == nil && state.Running() {
				if log != nil {
					fmt.Fprintf(log, "%s: %s (%s)\n", unit, state.Active, state.Sub)
				}
				break
			}
			if err == nil && state.Active == "failed" {
				return restarted, fmt.Errorf("%s failed after restart; see its log", unit)
			}
			if time.Now().After(deadline) {
				return restarted, fmt.Errorf("%s is %s/%s after restart", unit, state.Active, state.Sub)
			}
			select {
			case <-ctx.Done():
				return restarted, ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		restarted = append(restarted, unit)
	}
	return restarted, nil
}

// nativeLoopbackHealth requests the health path from the web process on its
// loopback port, with the application's domain as Host. Any 2xx within the
// deadline passes; a restarted process gets time to boot.
func nativeLoopbackHealth(port, domain, path string) func(context.Context) error {
	if path == "" {
		path = "/"
	}
	return func(ctx context.Context) error {
		client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		deadline := time.Now().Add(nativeHealthDeadline)
		var last error
		for {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+path, nil)
			if err != nil {
				return err
			}
			if domain != "" {
				request.Host = domain
			}
			request.Header.Set("X-Forwarded-Proto", "https")
			response, err := client.Do(request)
			if err == nil {
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					response.Body.Close()
					return nil
				}
				last = nativeHealthResponse(response, path+" on port "+port)
				response.Body.Close()
			} else {
				last = err
			}
			if time.Now().After(deadline) {
				return last
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(nativeHealthRetry):
			}
		}
	}
}
