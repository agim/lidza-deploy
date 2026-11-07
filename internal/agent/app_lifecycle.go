package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// Application lifecycle (POST /v1/apps/{id}/stop|start|restart), the same
// three actions for every runtime:
//
//   - stop: every process stops and is disabled, so it stays down after a
//     reboot; schedules pause; the web hostnames answer with the stopped
//     page; data, cache and files are kept. The stopped state is persisted
//     before anything changes, so health checks, deploys and webhooks leave
//     the application alone until it is started.
//   - start: the reverse; a native application enables and starts exactly
//     the processes that were enabled or running when it stopped, so units
//     the operator keeps disabled stay disabled.
//   - restart: every running or enabled process restarts (a container
//     application reloads its current release).
//
// Container applications keep their existing implementation
// (lifecycle.go); native applications are handled here.

// NativeStoppedState is a native application an operator stopped.
type NativeStoppedState struct {
	Since time.Time `json:"since"`
	By    string    `json:"by,omitempty"`
	// Units were enabled or running when the application stopped: start
	// enables and starts these again, and no other.
	Units []string `json:"units"`
	// Schedule is the crontab block that was paused; ScheduleUser and
	// ScheduleID name the crontab and its block.
	Schedule       string `json:"schedule,omitempty"`
	SchedulePaused bool   `json:"schedule_paused,omitempty"`
	ScheduleUser   string `json:"schedule_user,omitempty"`
	ScheduleID     string `json:"schedule_id,omitempty"`
	// Route reports the stopped page published in Caddy.
	Route bool `json:"route,omitempty"`
}

// AppLifecycleRequest is the body of the lifecycle routes.
type AppLifecycleRequest struct {
	By string `json:"by,omitempty"`
}

// AppLifecycleResult says what a lifecycle action did.
type AppLifecycleResult struct {
	AppID string `json:"app_id"`
	// Status is stopped, starting, running, restarting or restarted.
	Status     string      `json:"status"`
	Units      []string    `json:"units,omitempty"`
	Deployment *Deployment `json:"deployment,omitempty"`
	Message    string      `json:"message,omitempty"`
}

var errLifecycleNotFound = errors.New("no such application on this server")

var appLifecycleActions = []string{"stop", "start", "restart"}

// stoppedPage is what the hostnames of a stopped application answer.
var stoppedPage = renderErrorPage("Application stopped", "This application is stopped", "This application is stopped. Its data is kept; it serves again when it is started.")

func serveStoppedPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "300")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(stoppedPage)
}

func (m *Manager) appLifecycleRoutes(mux *http.ServeMux) {
	for _, action := range appLifecycleActions {
		mux.HandleFunc("POST /v1/apps/{id}/"+action, func(w http.ResponseWriter, r *http.Request) {
			var req AppLifecycleRequest
			if r.ContentLength != 0 {
				if err := Decode(w, r, &req); err != nil {
					Fail(w, 400, err)
					return
				}
			}
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Minute))
			ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
			defer cancel()
			result, err := m.AppLifecycle(ctx, r.PathValue("id"), action, req)
			if err != nil {
				status := 409
				if errors.Is(err, errLifecycleNotFound) {
					status = 404
				}
				Fail(w, status, err)
				return
			}
			JSON(w, 200, result)
		})
	}
}

// AppLifecycle stops, starts or restarts an application of any runtime.
func (m *Manager) AppLifecycle(ctx context.Context, id, action string, req AppLifecycleRequest) (AppLifecycleResult, error) {
	if !slices.Contains(appLifecycleActions, action) {
		return AppLifecycleResult{}, errors.New("the action must be stop, start or restart")
	}
	if len(req.By) > 200 {
		return AppLifecycleResult{}, errors.New("by is limited to 200 characters")
	}
	m.mu.Lock()
	a, container := m.data.Apps[id]
	_, native := m.data.NativeApps[id]
	m.mu.Unlock()
	switch {
	case container:
		return m.containerLifecycle(id, a, action)
	case native:
		return m.nativeLifecycle(ctx, id, action, req.By)
	}
	return AppLifecycleResult{}, errLifecycleNotFound
}

func (m *Manager) containerLifecycle(id string, a App, action string) (AppLifecycleResult, error) {
	result := AppLifecycleResult{AppID: id}
	if a.Retiring {
		return result, errors.New("the application is being removed")
	}
	switch action {
	case "stop":
		stopped := true
		if err := m.setApplicationState(id, ApplicationState{Stopped: &stopped}); err != nil {
			return result, err
		}
		result.Status = "stopped"
	case "start":
		if !a.Stopped {
			result.Status, result.Message = "running", "The application is running."
			return result, nil
		}
		stopped := false
		if err := m.setApplicationState(id, ApplicationState{Stopped: &stopped}); err != nil {
			return result, err
		}
		result.Status = "starting"
	case "restart":
		if a.Stopped {
			return result, errors.New("the application is stopped; start it instead")
		}
		d, err := m.Reload(id)
		if err != nil {
			return result, err
		}
		result.Status, result.Deployment = "restarting", &d
	}
	return result, nil
}

// ---- native applications -----------------------------------------------------

// nativeStopped returns the stopped state of a native application.
func (m *Manager) nativeStopped(id string) (NativeStoppedState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.data.NativeStopped[id]
	return s, ok
}

// nativeStoppedHostLocked reports a hostname of a stopped native application.
func (m *Manager) nativeStoppedHostLocked(host string) bool {
	for id := range m.data.NativeStopped {
		a, ok := m.data.NativeApps[id]
		if ok && (App{Domain: a.Domain, AdditionalDomains: a.AdditionalDomains}).HasDomain(host) {
			return true
		}
	}
	return false
}

func (m *Manager) setNativeStopped(id string, change func(*NativeStoppedState, bool) bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous, existed := m.data.NativeStopped[id]
	next := previous
	if keep := change(&next, existed); !keep {
		delete(m.data.NativeStopped, id)
	} else {
		if m.data.NativeStopped == nil {
			m.data.NativeStopped = map[string]NativeStoppedState{}
		}
		m.data.NativeStopped[id] = next
	}
	if err := m.save(); err != nil {
		if existed {
			m.data.NativeStopped[id] = previous
		} else {
			delete(m.data.NativeStopped, id)
		}
		return err
	}
	return nil
}

// nativeLifecycleAllowed reports why the panel cannot stop, start or
// restart a native application; "" when it can.
func (m *Manager) nativeLifecycleAllowed(id string) string {
	if m.cfg.Native == nil {
		return "Native management is not configured on this server."
	}
	if _, created := m.nativeCreatedPolicy(id); created || m.cfg.Native.ServiceControl {
		return ""
	}
	return "Service control of applications adopted from the existing host setup is not enabled on this server (native.service_control in the agent configuration)."
}

// nativeLifecycleUnits returns the registration and the units an
// application-level action controls: every service except those its
// release policy keeps preserved (stopped or disabled by the operator).
func (m *Manager) nativeLifecycleUnits(id string) (NativeApplication, []string, error) {
	a, units, err := m.nativeServiceUnits(id)
	if err != nil {
		return a, nil, err
	}
	var preserve []string
	if _, policy, err := m.nativeReleasePolicy(id); err == nil {
		preserve = policy.Preserve
	}
	return a, slices.DeleteFunc(units, func(unit string) bool { return slices.Contains(preserve, unit) }), nil
}

// nativeUnitStates reads the active and unit-file state of units.
func (m *Manager) nativeUnitStates(ctx context.Context, id string, units []string) (map[string]NativeService, error) {
	out := map[string]NativeService{}
	if len(units) == 0 {
		return out, nil
	}
	raw, err := m.nativeServiceMetadataFor(ctx, id, units)
	if err != nil {
		return nil, errors.New("the services' state could not be read")
	}
	for _, block := range strings.Split(raw, "\n\n") {
		s := NativeService{}
		for _, line := range strings.Split(block, "\n") {
			key, value, _ := strings.Cut(line, "=")
			switch key {
			case "Id":
				s.Unit = value
			case "ActiveState":
				s.Active = value
			case "SubState":
				s.State = value
			case "UnitFileState":
				s.UnitFileState = value
			}
		}
		if slices.Contains(units, s.Unit) {
			out[s.Unit] = s
		}
	}
	return out, nil
}

func (s NativeService) enabled() bool {
	return s.UnitFileState == "enabled" || s.UnitFileState == "enabled-runtime" || s.UnitFileState == "static" || s.UnitFileState == "alias"
}

func (s NativeService) running() bool {
	return s.Active == "active" || s.Active == "activating" || s.Active == "reloading"
}

// nativeUnitCtl runs systemctl for an application's units: as root for
// system units, through the owner's manager for user units.
func (m *Manager) nativeUnitCtl(ctx context.Context, id string, args ...string) error {
	if m.nativeCreatedSystemUnits(id) {
		_, err := m.nativeRootExec(ctx, append([]string{"systemctl"}, args...)...)
		return err
	}
	_, err := m.nativeExec(ctx, append([]string{"systemctl", "--user"}, args...)...)
	return err
}

// nativeScheduleTab is the crontab and block of a release-managed
// application's schedule; false when it has none.
func (m *Manager) nativeScheduleTab(id string) (nativeReleaseCrontab, bool) {
	_, policy, err := m.nativeReleasePolicy(id)
	if err != nil {
		return nativeReleaseCrontab{}, false
	}
	user := policy.User
	if user == "" && m.cfg.Native != nil {
		user = m.cfg.Native.User
	}
	if user == "" {
		return nativeReleaseCrontab{}, false
	}
	return nativeReleaseCrontab{m: m, user: user, identifier: policy.ScheduleIdentifier()}, true
}

// pauseNativeSchedule removes the application's crontab block and returns
// its entries; nothing else in the crontab changes.
func pauseNativeSchedule(ctx context.Context, tab nativeReleaseCrontab) (string, error) {
	existing, err := tab.Read(ctx)
	if err != nil {
		return "", err
	}
	entries, found := release.ScheduleBlock(existing, tab.identifier)
	if !found {
		return "", nil
	}
	next, err := release.ReplaceScheduleBlock(existing, tab.identifier, "")
	if err != nil {
		return "", err
	}
	if err := tab.Write(ctx, next); err != nil {
		return "", err
	}
	return entries, nil
}

// resumeNativeSchedule installs entries as the application's block again.
func resumeNativeSchedule(ctx context.Context, tab nativeReleaseCrontab, entries string) error {
	if strings.TrimSpace(entries) == "" {
		return nil
	}
	_, err := release.SyncSchedule(ctx, tab, tab.identifier, entries)
	return err
}

func (m *Manager) nativeLifecycle(ctx context.Context, id, action, by string) (AppLifecycleResult, error) {
	result := AppLifecycleResult{AppID: id}
	if reason := m.nativeLifecycleAllowed(id); reason != "" {
		return result, errors.New(reason)
	}
	if m.upgradePending() {
		return result, errors.New("agent upgrade in progress; try again after it finishes")
	}
	a, units, err := m.nativeLifecycleUnits(id)
	if err != nil {
		return result, err
	}
	m.mu.Lock()
	switch {
	case m.nativeOperations[id]:
		m.mu.Unlock()
		return result, errors.New("an operation on this application is running; try again after it finishes")
	case m.busy(id):
		m.mu.Unlock()
		return result, errors.New("the application has an active deployment; try again after it finishes")
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[id] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.nativeOperations, id); m.mu.Unlock() }()

	switch action {
	case "stop":
		return m.stopNative(ctx, a, units, by)
	case "start":
		return m.startNative(ctx, a)
	}
	return m.restartNative(ctx, a, units)
}

func (m *Manager) stopNative(ctx context.Context, a NativeApplication, units []string, by string) (AppLifecycleResult, error) {
	id := a.ID
	result := AppLifecycleResult{AppID: id, Status: "stopped"}
	states, err := m.nativeUnitStates(ctx, id, units)
	if err != nil {
		return result, err
	}
	// The desired state is saved first: a crash part way leaves it stopped,
	// and a retry finishes the stop without losing which units ran.
	if err := m.setNativeStopped(id, func(s *NativeStoppedState, existed bool) bool {
		if !existed {
			s.Since, s.By, s.Units = time.Now().UTC(), by, []string{}
			for _, unit := range units {
				if state := states[unit]; state.enabled() || state.running() {
					s.Units = append(s.Units, unit)
				}
			}
		}
		return true
	}); err != nil {
		return result, err
	}
	var failed []string
	for _, unit := range units {
		if err := m.nativeUnitCtl(ctx, id, "disable", "--now", "--quiet", unit); err != nil {
			failed = append(failed, unit)
		}
	}
	state, _ := m.nativeStopped(id)
	if tab, ok := m.nativeScheduleTab(id); ok && !state.SchedulePaused {
		entries, err := pauseNativeSchedule(ctx, tab)
		if err != nil {
			return result, fmt.Errorf("the processes stopped but the schedule could not be paused; stop again to finish: %w", err)
		}
		if err := m.setNativeStopped(id, func(s *NativeStoppedState, _ bool) bool {
			s.Schedule, s.SchedulePaused, s.ScheduleUser, s.ScheduleID = entries, true, tab.user, tab.identifier
			return true
		}); err != nil {
			return result, err
		}
	}
	if caddy, ok := m.nativeCaddy(ctx); ok {
		if err := caddy.publishStoppedRoute(ctx, id, append([]string{a.Domain}, a.AdditionalDomains...)); err != nil {
			return result, fmt.Errorf("the processes stopped but the stopped page could not be published: %w", err)
		}
		if err := m.setNativeStopped(id, func(s *NativeStoppedState, _ bool) bool { s.Route = true; return true }); err != nil {
			return result, err
		}
	}
	if len(failed) > 0 {
		return result, fmt.Errorf("%s could not be stopped; stop the application again to retry", strings.Join(failed, ", "))
	}
	state, _ = m.nativeStopped(id)
	result.Units = state.Units
	log.Printf("native %s: stopped by %s; %d units disabled", id, cmpOr(by, "the panel"), len(units))
	return result, nil
}

func (m *Manager) startNative(ctx context.Context, a NativeApplication) (AppLifecycleResult, error) {
	id := a.ID
	result := AppLifecycleResult{AppID: id, Status: "running"}
	state, stopped := m.nativeStopped(id)
	if !stopped {
		result.Message = "The application is running."
		return result, nil
	}
	var failed []string
	for _, unit := range state.Units {
		if err := m.nativeUnitCtl(ctx, id, "enable", "--now", "--no-block", "--quiet", unit); err != nil {
			failed = append(failed, unit)
		}
	}
	if len(failed) > 0 {
		return result, fmt.Errorf("%s could not be started; the application stays stopped, start it again to retry", strings.Join(failed, ", "))
	}
	if state.SchedulePaused && state.Schedule != "" {
		tab := nativeReleaseCrontab{m: m, user: state.ScheduleUser, identifier: state.ScheduleID}
		if err := resumeNativeSchedule(ctx, tab, state.Schedule); err != nil {
			return result, fmt.Errorf("the processes started but the schedule could not be restored; start again to retry: %w", err)
		}
	}
	if state.Route {
		if caddy, ok := m.nativeCaddy(ctx); ok {
			if err := caddy.withdrawStoppedRoute(ctx, id); err != nil {
				return result, fmt.Errorf("the processes started but the stopped page is still published; start again to retry: %w", err)
			}
		}
	}
	if err := m.setNativeStopped(id, func(*NativeStoppedState, bool) bool { return false }); err != nil {
		return result, err
	}
	result.Status, result.Units = "starting", state.Units
	return result, nil
}

func (m *Manager) restartNative(ctx context.Context, a NativeApplication, units []string) (AppLifecycleResult, error) {
	id := a.ID
	result := AppLifecycleResult{AppID: id, Status: "restarting"}
	if _, stopped := m.nativeStopped(id); stopped {
		return result, errors.New("the application is stopped; start it instead")
	}
	if len(units) == 0 {
		return result, errors.New("this application has no process to restart; its files are served by the web server")
	}
	states, err := m.nativeUnitStates(ctx, id, units)
	if err != nil {
		return result, err
	}
	var failed []string
	for _, unit := range units {
		// A unit the operator keeps disabled and stopped is left alone.
		if state := states[unit]; !state.enabled() && !state.running() {
			continue
		}
		if err := m.nativeUnitCtl(ctx, id, "restart", "--no-block", unit); err != nil {
			failed = append(failed, unit)
			continue
		}
		result.Units = append(result.Units, unit)
	}
	if len(failed) > 0 {
		return result, fmt.Errorf("%s could not be restarted", strings.Join(failed, ", "))
	}
	if len(result.Units) == 0 {
		return result, errors.New("no process of this application is enabled or running; start it instead")
	}
	return result, nil
}

// ---- the stopped page in Caddy ----------------------------------------------

const nativeCaddyStoppedPrefix = "lidza-stopped-"

func nativeCaddyStoppedRouteID(id string) string { return nativeCaddyStoppedPrefix + id }

// renderStoppedCaddyRoute answers the hostnames of a stopped application
// with the stopped page, ahead of whatever route serves them otherwise.
func renderStoppedCaddyRoute(id string, hosts []string) ([]byte, error) {
	if !idPattern.MatchString(id) || len(hosts) == 0 {
		return nil, errors.New("a stopped route needs an application and hostnames")
	}
	return json.Marshal(map[string]any{
		"@id":   nativeCaddyStoppedRouteID(id),
		"match": []any{map[string]any{"host": hosts}},
		"handle": []any{map[string]any{
			"handler":     "static_response",
			"status_code": http.StatusServiceUnavailable,
			"headers":     map[string][]string{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}, "Retry-After": {"300"}},
			"body":        string(stoppedPage),
		}},
		"terminal": true,
	})
}

func (c nativeCaddy) publishStoppedRoute(ctx context.Context, id string, hosts []string) error {
	route, err := renderStoppedCaddyRoute(id, hosts)
	if err != nil {
		return err
	}
	var current json.RawMessage
	if found, err := c.get(ctx, "/id/"+nativeCaddyStoppedRouteID(id), &current); err != nil {
		return err
	} else if found && jsonEqual(current, route) {
		return nil
	}
	status, err := c.call(ctx, http.MethodPatch, "/id/"+nativeCaddyStoppedRouteID(id), route)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound {
		return nil
	}
	_, err = c.call(ctx, http.MethodPut, "/config/apps/http/servers/"+c.cfg.Server+"/routes/0", route)
	return err
}

func (c nativeCaddy) withdrawStoppedRoute(ctx context.Context, id string) error {
	_, err := c.call(ctx, http.MethodDelete, "/id/"+nativeCaddyStoppedRouteID(id), nil)
	return err
}
