package release

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var unitPattern = regexp.MustCompile(`^[A-Za-z0-9:_.@-]+\.service$`)

// ServiceState is the service manager's view of one unit.
type ServiceState struct {
	Active   string // active, inactive, failed, activating, ...
	Sub      string // running, dead, exited, ...
	UnitFile string // enabled, disabled, static, linked, ...
}

// Running reports whether the unit currently has a live process.
func (s ServiceState) Running() bool { return s.Active == "active" && s.Sub == "running" }

// Services controls the application's existing service units.
type Services interface {
	State(ctx context.Context, unit string) (ServiceState, error)
	Restart(ctx context.Context, unit string) error
}

// UserSystemd controls units in the application owner's systemd user
// manager. It never starts a user manager that is not already running.
type UserSystemd struct {
	Exec Executor
	Env  []string // must include XDG_RUNTIME_DIR for the owner
}

func (u UserSystemd) systemctl(ctx context.Context, args ...string) (string, error) {
	if _, ok := envValue(u.Env, "XDG_RUNTIME_DIR"); !ok {
		return "", errors.New("release: user systemd needs XDG_RUNTIME_DIR")
	}
	return output(ctx, u.Exec, Command{Name: "systemctl " + args[0], Dir: "/", Env: u.Env,
		Args: append([]string{"systemctl", "--user", "--no-pager"}, args...)})
}

func (u UserSystemd) State(ctx context.Context, unit string) (ServiceState, error) {
	if !unitPattern.MatchString(unit) {
		return ServiceState{}, fmt.Errorf("release: invalid unit %q", unit)
	}
	out, err := u.systemctl(ctx, "show", unit, "--property=ActiveState,SubState,UnitFileState")
	if err != nil {
		return ServiceState{}, err
	}
	var state ServiceState
	for _, line := range strings.Split(out, "\n") {
		key, value, _ := strings.Cut(line, "=")
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

func (u UserSystemd) Restart(ctx context.Context, unit string) error {
	if !unitPattern.MatchString(unit) {
		return fmt.Errorf("release: invalid unit %q", unit)
	}
	_, err := u.systemctl(ctx, "restart", unit)
	return err
}

// RestartResult reports which units were restarted and which were left alone.
type RestartResult struct {
	Restarted []string
	Skipped   map[string]ServiceState
}

// RestartRunning restarts, in order, each unit that is running now and
// waits for it to be running again. Stopped, failed, and idle
// socket-activated units are skipped: a restart would start them, and their
// desired state belongs to the operator. Socket-activated servers keep
// accepting connections on their socket while the service restarts, and the
// next connection to an idle one starts the new release.
func RestartRunning(ctx context.Context, s Services, units []string, settle time.Duration) (RestartResult, error) {
	result := RestartResult{Skipped: map[string]ServiceState{}}
	for _, unit := range units {
		state, err := s.State(ctx, unit)
		if err != nil {
			return result, err
		}
		if !state.Running() {
			result.Skipped[unit] = state
			continue
		}
		if err := s.Restart(ctx, unit); err != nil {
			return result, fmt.Errorf("release: restart %s: %w", unit, err)
		}
		if err := waitRunning(ctx, s, unit, settle); err != nil {
			return result, err
		}
		result.Restarted = append(result.Restarted, unit)
	}
	return result, nil
}

func waitRunning(ctx context.Context, s Services, unit string, settle time.Duration) error {
	if settle <= 0 {
		settle = 30 * time.Second
	}
	deadline := time.Now().Add(settle)
	var last ServiceState
	for {
		state, err := s.State(ctx, unit)
		if err == nil {
			last = state
			if state.Running() {
				return nil
			}
			if state.Active == "failed" {
				return fmt.Errorf("release: %s failed after restart", unit)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("release: %s is %s/%s after restart", unit, last.Active, last.Sub)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
