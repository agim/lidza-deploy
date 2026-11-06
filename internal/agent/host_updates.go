package agent

// Host updates: the agent reports pending operating-system updates, keeps
// unattended-upgrades enabled for security pockets when asked, installs
// updates on request, applies disruptive upgrades inside the maintenance
// window and reboots the host inside that window when a reboot is required.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// HostRebootWindow is a weekly maintenance window in UTC.
type HostRebootWindow struct {
	Weekdays        []int `json:"weekdays,omitempty"` // 0 = Sunday … 6 = Saturday; empty = every day
	Hour            int   `json:"hour"`               // start hour, UTC
	DurationMinutes int   `json:"duration_minutes"`
}

// HostUpdatesPolicy is the operator's update policy for one server.
type HostUpdatesPolicy struct {
	// AutoSecurity installs unattended-upgrades and keeps it enabled for the
	// distribution's security pockets.
	AutoSecurity bool `json:"auto_security"`
	// AutoReboot reboots inside RebootWindow when the host reports that a
	// reboot is required (for example after a kernel update).
	AutoReboot   bool             `json:"auto_reboot"`
	RebootWindow HostRebootWindow `json:"reboot_window"`
	// ManagePackageSources lets unattended-upgrades also install updates from
	// the official Caddy, PostgreSQL and Docker repositories when the host has
	// them configured. No repository is added and no binary is replaced.
	ManagePackageSources bool `json:"manage_package_sources"`
	// DisruptiveInWindow installs disruptive upgrades (container runtime,
	// databases, web server, kernel, systemd) only inside RebootWindow, and
	// keeps unattended-upgrades away from them. Unset means on; manual runs
	// can still include them explicitly.
	DisruptiveInWindow *bool `json:"disruptive_in_window,omitempty"`
}

// deferDisruptive reports whether disruptive upgrades wait for the window.
func (p HostUpdatesPolicy) deferDisruptive() bool {
	return p.DisruptiveInWindow == nil || *p.DisruptiveInWindow
}

// HostUpdatesState is the persisted part of host update management.
type HostUpdatesState struct {
	Policy     HostUpdatesPolicy `json:"policy"`
	Configured bool              `json:"configured,omitempty"`
	// ConfigurePending is set until unattended-upgrades matches the policy;
	// the maintenance tick retries it after a busy period or a restart.
	ConfigurePending bool      `json:"configure_pending,omitempty"`
	LastReboot       time.Time `json:"last_reboot,omitzero"`
	RebootDeferred   string    `json:"reboot_deferred,omitempty"`
	// LastDisruptiveRun is when the maintenance window last installed, or
	// found nothing to install; one window installs at most once.
	LastDisruptiveRun time.Time        `json:"last_disruptive_run,omitzero"`
	Docker            *HostDockerState `json:"docker,omitempty"`
	Jobs              []HostJob        `json:"jobs,omitempty"`
}

// HostDockerState records what the agent did to the container runtime.
type HostDockerState struct {
	// InstalledByAgent is set when the agent installed Docker Engine, which
	// makes the daemon configuration the agent's to reconcile.
	InstalledByAgent   bool      `json:"installed_by_agent,omitempty"`
	LiveRestoreApplied time.Time `json:"live_restore_applied,omitzero"`
	// RestartPending is set when daemon.json enables live-restore but the
	// running daemon did not pick it up on reload; only a restart applies it,
	// and that happens in the maintenance window.
	RestartPending bool   `json:"restart_pending,omitempty"`
	ConfigBackup   string `json:"config_backup,omitempty"`
}

// HostComponent is the update state of one piece of host infrastructure.
type HostComponent struct {
	Name             string `json:"name"` // caddy, postgresql, redis, docker
	Package          string `json:"package,omitempty"`
	Installed        string `json:"installed,omitempty"`
	Candidate        string `json:"candidate,omitempty"`
	UpgradeAvailable bool   `json:"upgrade_available"`
	// Source is "package" (installed from an apt repository), "binary" (a file
	// no package owns, so apt never updates it) or "absent".
	Source     string `json:"source"`
	Repository string `json:"repository,omitempty"`
	// Automatic says what unattended-upgrades installs for it: "security",
	// "all" or "none".
	Automatic string `json:"automatic"`
}

// HostDockerStatus is the container runtime as the host reports it.
type HostDockerStatus struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Version   string `json:"version,omitempty"`
	// LiveRestore is what the running daemon reports; Configured is what
	// /etc/docker/daemon.json asks for. They differ until a reload.
	LiveRestore bool `json:"live_restore"`
	Configured  bool `json:"live_restore_configured"`
	// Managed is set when the agent installed Docker or runs containers on
	// it, so it reconciles the daemon configuration.
	Managed        bool   `json:"managed"`
	RestartPending bool   `json:"restart_pending,omitempty"`
	Warning        string `json:"warning,omitempty"`
	// Buildx reports the BuildKit plugin image builds use
	// (docker_buildkit.go); the agent installs it on hosts it manages.
	Buildx bool `json:"buildx"`
}

// HostUpdatesStatus is the host observation merged with policy and history.
type HostUpdatesStatus struct {
	Supported bool      `json:"supported"`
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitzero"`
	Pending   int       `json:"pending"`
	Security  int       `json:"security"`
	// Disruptive counts pending packages that restart shared services or
	// need a reboot; DisruptivePackages lists them with their effect.
	Disruptive          int                  `json:"disruptive"`
	Packages            []HostPackageUpgrade `json:"packages,omitempty"`
	DisruptivePackages  []HostPackageUpgrade `json:"disruptive_packages,omitempty"`
	RebootRequired      bool                 `json:"reboot_required"`
	RebootPackages      []string             `json:"reboot_packages,omitempty"`
	RebootReason        string               `json:"reboot_reason,omitempty"`
	UnattendedInstalled bool                 `json:"unattended_installed"`
	UnattendedEnabled   bool                 `json:"unattended_enabled"`
	UnattendedLastRun   time.Time            `json:"unattended_last_run,omitzero"`
	ManagedOrigins      []string             `json:"managed_origins,omitempty"`
	Components          []HostComponent      `json:"components,omitempty"`
	Docker              *HostDockerStatus    `json:"docker,omitempty"`
	Policy              HostUpdatesPolicy    `json:"policy"`
	Configured          bool                 `json:"configured"`
	NextRebootWindow    time.Time            `json:"next_reboot_window,omitzero"`
	// NextDisruptiveWindow is when deferred disruptive upgrades install next.
	NextDisruptiveWindow time.Time `json:"next_disruptive_window,omitzero"`
	LastReboot           time.Time `json:"last_reboot,omitzero"`
	RebootDeferred       string    `json:"reboot_deferred,omitempty"`
	Running              string    `json:"running,omitempty"`
	Jobs                 []HostJob `json:"jobs,omitempty"`
	Error                string    `json:"error,omitempty"`
}

// HostUpdatesRun selects which pending updates to install now.
type HostUpdatesRun struct {
	Scope string `json:"scope,omitempty"` // security (default) or all
	// IncludeDisruptive installs disruptive packages in this run instead of
	// leaving them for the maintenance window. The panel asks for it after
	// listing what each of them restarts.
	IncludeDisruptive bool `json:"include_disruptive,omitempty"`
}

// HostRebootRequest must carry confirm=true.
type HostRebootRequest struct {
	Confirm bool `json:"confirm"`
}

const (
	unattendedDropIn     = "/etc/apt/apt.conf.d/52lidza-unattended-upgrades"
	aptPreferencesParts  = "/etc/apt/preferences.d"
	hostStatusMaxAge     = 30 * time.Minute
	hostRebootMinSpacing = 6 * time.Hour
	hostPackageListLimit = 200
)

func defaultHostUpdatesPolicy() HostUpdatesPolicy {
	return HostUpdatesPolicy{RebootWindow: HostRebootWindow{Weekdays: []int{0}, Hour: 3, DurationMinutes: 60}}
}

func (p HostUpdatesPolicy) Validate() error {
	w := p.RebootWindow
	if w.Hour < 0 || w.Hour > 23 {
		return errors.New("reboot window hour must be between 0 and 23 (UTC)")
	}
	if w.DurationMinutes < 30 || w.DurationMinutes > 360 {
		return errors.New("reboot window must last between 30 minutes and 6 hours")
	}
	seen := map[int]bool{}
	for _, d := range w.Weekdays {
		if d < 0 || d > 6 || seen[d] {
			return errors.New("reboot window weekdays must be distinct values from 0 (Sunday) to 6 (Saturday)")
		}
		seen[d] = true
	}
	if p.ManagePackageSources && !p.AutoSecurity {
		return errors.New("package source updates need automatic security updates turned on")
	}
	return nil
}

// presented returns the policy as the API shows it: every option explicit.
func (p HostUpdatesPolicy) presented() HostUpdatesPolicy {
	deferred := p.deferDisruptive()
	p.DisruptiveInWindow = &deferred
	p.RebootWindow.Weekdays = slices.Clone(p.RebootWindow.Weekdays)
	return p
}

func (w HostRebootWindow) allows(day time.Weekday) bool {
	return len(w.Weekdays) == 0 || slices.Contains(w.Weekdays, int(day))
}

// active returns the start of the window containing now, if any. A window may
// cross midnight, so the previous day's window is checked too.
func (w HostRebootWindow) active(now time.Time) (time.Time, bool) {
	now = now.UTC()
	for _, offset := range []int{0, -1} {
		day := now.AddDate(0, 0, offset)
		start := time.Date(day.Year(), day.Month(), day.Day(), w.Hour, 0, 0, 0, time.UTC)
		if w.allows(start.Weekday()) && !now.Before(start) && now.Before(start.Add(time.Duration(w.DurationMinutes)*time.Minute)) {
			return start, true
		}
	}
	return time.Time{}, false
}

// next returns the start of the current or next window.
func (w HostRebootWindow) next(now time.Time) time.Time {
	if start, ok := w.active(now); ok {
		return start
	}
	now = now.UTC()
	for offset := 0; offset <= 7; offset++ {
		day := now.AddDate(0, 0, offset)
		start := time.Date(day.Year(), day.Month(), day.Day(), w.Hour, 0, 0, 0, time.UTC)
		if start.After(now) && w.allows(start.Weekday()) {
			return start
		}
	}
	return time.Time{}
}

// hostUpdatesStateLocked returns the persisted state, creating defaults.
func (m *Manager) hostUpdatesStateLocked() *HostUpdatesState {
	if m.data.HostUpdates == nil {
		m.data.HostUpdates = &HostUpdatesState{Policy: defaultHostUpdatesPolicy()}
	}
	return m.data.HostUpdates
}

// hostPolicyLocked reads the policy without creating state. Caller holds m.mu.
func (m *Manager) hostPolicyLocked() HostUpdatesPolicy {
	if m.data.HostUpdates == nil {
		return defaultHostUpdatesPolicy()
	}
	return m.data.HostUpdates.Policy
}

// rebootRequirement reads the markers Debian-family hosts leave after an
// update that needs a restart. Debian does not always write
// /var/run/reboot-required, so a kernel newer than the running one counts too.
func rebootRequirement() (bool, []string, string) {
	if hostFileExists("/var/run/reboot-required") {
		raw, _ := os.ReadFile(hostPath("/var/run/reboot-required.pkgs"))
		var pkgs []string
		for _, line := range strings.Fields(string(raw)) {
			if !slices.Contains(pkgs, line) {
				pkgs = append(pkgs, line)
			}
		}
		return true, pkgs, "the system reports that a restart is required"
	}
	running, err := os.ReadFile(hostPath("/proc/sys/kernel/osrelease"))
	if err != nil {
		return false, nil, ""
	}
	kernels, _ := filepath.Glob(hostPath("/boot/vmlinuz-*"))
	var newest string
	var newestTime time.Time
	for _, k := range kernels {
		if info, err := os.Stat(k); err == nil && info.ModTime().After(newestTime) {
			newest, newestTime = strings.TrimPrefix(filepath.Base(k), "vmlinuz-"), info.ModTime()
		}
	}
	if newest != "" && newest != strings.TrimSpace(string(running)) {
		return true, []string{"linux-image-" + newest}, "kernel " + newest + " is installed but " + strings.TrimSpace(string(running)) + " is running"
	}
	return false, nil, ""
}

// pendingHostUpgrades simulates a dist-upgrade and confirms each package's
// security classification against the repositories that offer its candidate.
// It never refreshes package lists. The returned note names a degraded
// classification; the error means nothing could be simulated.
func pendingHostUpgrades(ctx context.Context) ([]HostPackageUpgrade, []aptRelease, string, error) {
	out, err := hostCommand(ctx, "apt-get", "-s", "-q", "-o", "Debug::NoLocking=true", "dist-upgrade")
	if err != nil {
		return nil, nil, "", errors.New("pending updates could not be simulated")
	}
	pkgs := parseAptSimulation(out)
	releasesOut, _ := hostCommand(ctx, "apt-cache", "policy")
	releases := parseAptReleases(releasesOut)
	note := ""
	if len(pkgs) > 0 {
		policyOut, err := hostCommand(ctx, "apt-cache", append([]string{"policy"}, packageNames(pkgs)...)...)
		if err != nil {
			note = "package origins could not be confirmed with apt-cache policy; the security classification comes from the simulation alone"
		} else {
			classifySecurity(pkgs, parseAptPolicy(policyOut), releases)
		}
	}
	return pkgs, releases, note, nil
}

// observeHostUpdates inspects the host. It never changes packages or
// configuration and never refreshes package lists (apt's daily timer does).
func observeHostUpdates(ctx context.Context) HostUpdatesStatus {
	status := HostUpdatesStatus{CheckedAt: time.Now().UTC()}
	if ok, reason := aptHost(); !ok {
		status.Reason = reason
		return status
	}
	status.Supported = true
	var problems []string
	status.RebootRequired, status.RebootPackages, status.RebootReason = rebootRequirement()
	pkgs, releases, note, err := pendingHostUpgrades(ctx)
	if err != nil {
		problems = append(problems, err.Error())
		releasesOut, _ := hostCommand(ctx, "apt-cache", "policy")
		releases = parseAptReleases(releasesOut)
	} else {
		if note != "" {
			problems = append(problems, note)
		}
		status.Pending = len(pkgs)
		for _, p := range pkgs {
			if p.Security {
				status.Security++
			}
			if p.Disruptive {
				status.Disruptive++
				status.DisruptivePackages = append(status.DisruptivePackages, p)
			}
		}
		slices.SortStableFunc(pkgs, func(a, b HostPackageUpgrade) int {
			if a.Security != b.Security {
				if a.Security {
					return -1
				}
				return 1
			}
			return strings.Compare(a.Name, b.Name)
		})
		if len(pkgs) > hostPackageListLimit {
			pkgs = pkgs[:hostPackageListLimit]
		}
		status.Packages = pkgs
	}
	queried := []string{"unattended-upgrades", "caddy", "redis-server", "docker-ce", "docker.io", "postgresql-[0-9]*"}
	dpkgOut, _ := hostCommand(ctx, "dpkg-query", append([]string{"-W", "-f=${Package}\t${db:Status-Abbrev}\t${Version}\n"}, queried...)...)
	installed := dpkgInstalled(dpkgOut)
	_, status.UnattendedInstalled = installed["unattended-upgrades"]
	if dump, err := hostCommand(ctx, "apt-config", "dump"); err == nil {
		status.UnattendedEnabled = status.UnattendedInstalled && unattendedConfigured(dump)
	}
	for _, stamp := range []string{"/var/lib/apt/periodic/unattended-upgrades-stamp", "/var/log/unattended-upgrades/unattended-upgrades.log"} {
		if info, err := os.Stat(hostPath(stamp)); err == nil {
			status.UnattendedLastRun = info.ModTime().UTC()
			break
		}
	}
	status.ManagedOrigins = managedOriginPatterns(releases)
	status.Components = observeComponents(ctx, installed)
	status.Docker = observeDocker(ctx)
	if len(problems) > 0 {
		status.Error = strings.Join(problems, "; ")
	}
	return status
}

func observeComponents(ctx context.Context, installed map[string]string) []HostComponent {
	postgres := ""
	for name := range installed {
		if strings.HasPrefix(name, "postgresql-") && (postgres == "" || name > postgres) {
			postgres = name
		}
	}
	docker := "docker-ce"
	if _, ok := installed[docker]; !ok {
		if _, ok := installed["docker.io"]; ok {
			docker = "docker.io"
		}
	}
	components := []HostComponent{{Name: "caddy", Package: "caddy"}, {Name: "postgresql", Package: postgres}, {Name: "redis", Package: "redis-server"}, {Name: "docker", Package: docker}}
	var names []string
	for _, c := range components {
		if _, ok := installed[c.Package]; ok {
			names = append(names, c.Package)
		}
	}
	policies := map[string]aptPackagePolicy{}
	if len(names) > 0 {
		out, _ := hostCommand(ctx, "apt-cache", append([]string{"policy"}, names...)...)
		policies = parseAptPolicy(out)
	}
	for i := range components {
		c := &components[i]
		version, ok := installed[c.Package]
		if !ok || c.Package == "" {
			c.Source = "absent"
			if c.Name == "caddy" && hostToolAvailable("caddy") {
				c.Source = "binary"
				if out, err := hostCommand(ctx, "caddy", "version"); err == nil {
					if fields := strings.Fields(out); len(fields) > 0 {
						c.Installed = fields[0]
					}
				}
			}
			continue
		}
		c.Source = "package"
		c.Installed = version
		if policy, ok := policies[c.Package]; ok {
			c.Candidate = policy.Candidate
			c.UpgradeAvailable = policy.Candidate != "" && policy.Installed != "" && policy.Candidate != policy.Installed
			if len(policy.Sources) > 0 {
				c.Repository = policy.Sources[0]
			}
		}
	}
	return components
}

// annotateComponents fills what unattended-upgrades covers for each component.
func annotateComponents(status *HostUpdatesStatus) {
	for i := range status.Components {
		c := &status.Components[i]
		switch {
		case c.Source != "package" || !status.UnattendedEnabled:
			c.Automatic = "none"
		case managedRepository(c.Repository):
			c.Automatic = "none"
			if status.Policy.ManagePackageSources && len(status.ManagedOrigins) > 0 {
				c.Automatic = "all"
			}
		default:
			c.Automatic = "security"
		}
	}
}

// hostUpdatesStatus returns the cached observation (refreshing it when asked
// or missing) merged with the persisted policy and job history.
func (m *Manager) hostUpdatesStatus(ctx context.Context, refresh bool) HostUpdatesStatus {
	hc := m.host()
	hc.mu.Lock()
	cached := hc.status
	running := hc.job
	hc.mu.Unlock()
	if cached == nil || refresh && running == "" {
		bounded, cancel := context.WithTimeout(ctx, 25*time.Second)
		observed := observeHostUpdates(bounded)
		cancel()
		hc.mu.Lock()
		hc.status = &observed
		hc.mu.Unlock()
		cached = &observed
	}
	var out HostUpdatesStatus
	if cached != nil {
		out = *cached
		out.Packages = slices.Clone(cached.Packages)
		out.DisruptivePackages = slices.Clone(cached.DisruptivePackages)
		out.Components = slices.Clone(cached.Components)
		if cached.Docker != nil {
			docker := *cached.Docker
			out.Docker = &docker
		}
	}
	managed := m.dockerManaged()
	m.mu.Lock()
	state := HostUpdatesState{Policy: defaultHostUpdatesPolicy()}
	if m.data.HostUpdates != nil {
		state = *m.data.HostUpdates
	}
	out.Policy = state.Policy.presented()
	out.Configured = state.Configured
	out.LastReboot = state.LastReboot
	out.RebootDeferred = state.RebootDeferred
	out.Jobs = slices.Clone(state.Jobs)
	restartPending := state.Docker != nil && state.Docker.RestartPending
	m.mu.Unlock()
	if err := m.hostOperationsAllowed(); err != nil {
		out.Supported = false
		out.Reason = err.Error()
	}
	out.Running = running
	now := time.Now()
	if out.Policy.AutoReboot {
		out.NextRebootWindow = out.Policy.RebootWindow.next(now)
	}
	if out.Policy.deferDisruptive() {
		out.NextDisruptiveWindow = out.Policy.RebootWindow.next(now)
	}
	if out.Docker != nil {
		out.Docker.Managed = managed
		out.Docker.RestartPending = restartPending
		out.Docker.Warning = dockerWarning(*out.Docker, out.Policy, out.NextDisruptiveWindow)
		if out.Docker.Running && !out.Docker.Buildx {
			note := "The Docker buildx plugin is missing, so image builds cannot use BuildKit. "
			if out.Docker.Managed {
				note += "The agent installs docker-buildx-plugin on its next hourly check."
			} else {
				note += "Install docker-buildx-plugin from Docker's repository."
			}
			out.Docker.Warning = strings.TrimSpace(out.Docker.Warning + " " + note)
		}
	}
	annotateComponents(&out)
	return out
}

func (m *Manager) refreshHostUpdatesIfStale(now time.Time) { m.refreshHostUpdates(now, false) }

// refreshHostUpdates re-observes a stale status in the background. Without
// force it refreshes only a status someone has looked at (the first read
// observes synchronously); the maintenance window forces one so it can act.
func (m *Manager) refreshHostUpdates(now time.Time, force bool) {
	hc := m.host()
	hc.mu.Lock()
	stale := hc.status == nil || now.Sub(hc.status.CheckedAt) >= hostStatusMaxAge
	if hc.refreshing || hc.job != "" || !stale || hc.status == nil && !force {
		hc.mu.Unlock()
		return
	}
	hc.refreshing = true
	hc.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
		observed := observeHostUpdates(ctx)
		cancel()
		hc.mu.Lock()
		hc.status = &observed
		hc.refreshing = false
		hc.mu.Unlock()
	}()
}

// setHostUpdatesPolicy saves the policy and, when needed, reconfigures
// unattended-upgrades in the background.
func (m *Manager) setHostUpdatesPolicy(p HostUpdatesPolicy) error {
	if err := m.hostOperationsAllowed(); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if ok, reason := aptHost(); !ok {
		return errors.New(reason)
	}
	p.RebootWindow.Weekdays = slices.Sorted(slices.Values(p.RebootWindow.Weekdays))
	m.mu.Lock()
	state := m.hostUpdatesStateLocked()
	previous := *state
	reconfigure := !state.Configured || previous.Policy.AutoSecurity != p.AutoSecurity || previous.Policy.ManagePackageSources != p.ManagePackageSources || previous.Policy.deferDisruptive() != p.deferDisruptive()
	state.Policy = p
	state.Configured = true
	state.ConfigurePending = state.ConfigurePending || reconfigure
	if err := m.save(); err != nil {
		*state = previous
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	if reconfigure {
		// When another host operation is running, the tick applies it later.
		_ = m.startHostJob("configure", "manual", m.configureUnattended)
	}
	return nil
}

// configureTick retries a pending unattended-upgrades configuration.
func (m *Manager) configureTick() {
	m.mu.Lock()
	pending := m.data.HostUpdates != nil && m.data.HostUpdates.ConfigurePending
	if pending {
		// After a failure, retry hourly rather than every minute.
		for _, job := range m.data.HostUpdates.Jobs {
			if job.Kind == "configure" {
				pending = job.Status != "running" && (job.Status != "failed" || job.Finished == nil || time.Since(*job.Finished) > time.Hour)
				break
			}
		}
	}
	m.mu.Unlock()
	if pending {
		_ = m.startHostJob("configure", "schedule", m.configureUnattended)
	}
}

// startHostJob runs one host operation at a time in the background, recording
// it in the job history.
func (m *Manager) startHostJob(kind, trigger string, run func(context.Context) (string, error)) error {
	hc := m.host()
	now := time.Now().UTC()
	m.mu.Lock()
	if busy := m.hostBusyLocked(); busy != "" && kind != "configure" {
		m.mu.Unlock()
		return errors.New("host updates are refused while " + busy)
	}
	hc.mu.Lock()
	if hc.job != "" {
		hc.mu.Unlock()
		m.mu.Unlock()
		return fmt.Errorf("host %s is already running", hc.job)
	}
	hc.job = kind
	hc.mu.Unlock()
	job := newHostJob(kind, trigger, now)
	m.recordHostJobLocked(job)
	if err := m.save(); err != nil {
		hc.mu.Lock()
		hc.job = ""
		hc.mu.Unlock()
		m.mu.Unlock()
		return err
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 45*time.Minute)
		output, err := run(ctx)
		cancel()
		finished := time.Now().UTC()
		job.Finished = &finished
		job.Output = strings.TrimSpace(output)
		job.Status = "succeeded"
		job.Summary = hostJobSummary(kind, output)
		if err != nil {
			job.Status = "failed"
			job.Summary = err.Error()
		}
		observeCtx, stop := context.WithTimeout(m.ctx, time.Minute)
		observed := observeHostUpdates(observeCtx)
		stop()
		hc.mu.Lock()
		hc.job = ""
		hc.status = &observed
		hc.mu.Unlock()
		m.mu.Lock()
		m.recordHostJobLocked(job)
		if kind == "configure" && err == nil {
			m.hostUpdatesStateLocked().ConfigurePending = false
		}
		if saveErr := m.save(); saveErr != nil {
			log.Printf("host %s status: %v", kind, saveErr)
		}
		m.mu.Unlock()
		if trigger == "schedule" {
			log.Printf("host %s (%s): %s", kind, job.Status, job.Summary)
		}
	}()
	return nil
}

// hostJobSummary is the one-line result of a job: the last "Summary:" line a
// run wrote, else a fixed sentence per kind.
func hostJobSummary(kind, output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if summary, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "Summary: "); ok {
			return summary
		}
	}
	switch kind {
	case "security-updates":
		return "Security updates installed."
	case "all-updates":
		return "Pending updates installed."
	case "disruptive-updates":
		return "Disruptive updates installed in the maintenance window."
	case "configure":
		return "Automatic update configuration applied."
	case "docker-live-restore":
		return "Docker live-restore enabled."
	case "docker-restart":
		return "Docker restarted in the maintenance window."
	}
	return "Completed."
}

// configureUnattended makes the host match AutoSecurity, ManagePackageSources
// and DisruptiveInWindow. Off removes only the agent's own drop-in, leaving
// the distribution's configuration as it was.
func (m *Manager) configureUnattended(ctx context.Context) (string, error) {
	m.mu.Lock()
	policy := m.hostPolicyLocked()
	m.mu.Unlock()
	var transcript strings.Builder
	run := func(name string, args ...string) error {
		out, err := hostCommand(ctx, name, args...)
		transcript.WriteString("$ " + name + " " + strings.Join(args, " ") + "\n" + out + "\n")
		if err != nil {
			return fmt.Errorf("%s failed: %w", name, err)
		}
		return nil
	}
	path := hostPath(unattendedDropIn)
	if !policy.AutoSecurity {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return transcript.String(), err
		}
		transcript.WriteString("Removed the agent's unattended-upgrades configuration; the distribution defaults apply.\n")
		return transcript.String(), nil
	}
	dpkgOut, _ := hostCommand(ctx, "dpkg-query", "-W", "-f=${Package}\t${db:Status-Abbrev}\t${Version}\n", "unattended-upgrades")
	if _, ok := dpkgInstalled(dpkgOut)["unattended-upgrades"]; !ok {
		if err := run("apt-get", "update", "-q", "-o", "DPkg::Lock::Timeout=300"); err != nil {
			return transcript.String(), err
		}
		if err := run("apt-get", "install", "-y", "-q", "-o", "DPkg::Lock::Timeout=300", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "unattended-upgrades"); err != nil {
			return transcript.String(), err
		}
	}
	var origins []string
	if policy.ManagePackageSources {
		releases, _ := hostCommand(ctx, "apt-cache", "policy")
		origins = managedOriginPatterns(parseAptReleases(releases))
	}
	previous, readErr := os.ReadFile(path)
	if err := writeHostFile(path, []byte(renderUnattendedConfig(origins, policy.deferDisruptive())), 0644); err != nil {
		return transcript.String(), err
	}
	// A syntax error in apt configuration breaks every apt command on the
	// host, so validate and restore the previous file on failure.
	if err := run("apt-config", "dump"); err != nil {
		if readErr == nil {
			_ = writeHostFile(path, previous, 0644)
		} else {
			_ = os.Remove(path)
		}
		return transcript.String(), errors.New("apt rejected the generated configuration; the previous configuration was restored")
	}
	transcript.WriteString("Wrote " + unattendedDropIn + " (security pockets")
	if len(origins) > 0 {
		transcript.WriteString(" and " + strings.Join(origins, "; "))
	}
	if policy.deferDisruptive() {
		transcript.WriteString("; disruptive packages left to the maintenance window")
	}
	transcript.WriteString(").\n")
	if err := run("systemctl", "enable", "--now", "apt-daily.timer", "apt-daily-upgrade.timer"); err != nil {
		return transcript.String(), err
	}
	return transcript.String(), nil
}

// aptPinDirectory builds a temporary preferences.d for one apt run: the
// host's own fragments, unchanged, plus the run's pins. apt reads it through
// Dir::Etc::PreferencesParts, so nothing under /etc/apt is modified.
func aptPinDirectory(pins string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "lidza-apt-pins-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	entries, _ := os.ReadDir(hostPath(aptPreferencesParts))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(hostPath(aptPreferencesParts), entry.Name()))
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), data, 0644); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "99-lidza-run"), []byte(pins), 0644); err != nil {
		cleanup()
		return "", nil, err
	}
	// apt runs as root and reads the directory itself.
	_ = os.Chmod(dir, 0755)
	return dir, cleanup, nil
}

func scopeLabel(scope string) string {
	switch scope {
	case "security":
		return "Security updates"
	case "disruptive":
		return "Disruptive updates"
	}
	return "Updates"
}

// installHostUpdates refreshes package lists, classifies what is pending and
// installs one selection: confirmed security updates, every pending update
// or the disruptive ones. Everything outside the selection is pinned away
// for the run, so apt keeps back (never pulls in) a package that would need
// an excluded one. Services are never restarted by the agent; the packages'
// own maintainer scripts decide, which is what makes some of them disruptive.
func installHostUpdates(scope string, includeDisruptive bool) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		var transcript strings.Builder
		run := func(name string, args ...string) (string, error) {
			out, err := hostCommand(ctx, name, args...)
			transcript.WriteString("$ " + name + " " + strings.Join(args, " ") + "\n" + out + "\n")
			if err != nil {
				return out, fmt.Errorf("%s failed: %w", name, err)
			}
			return out, nil
		}
		lock := []string{"-o", "DPkg::Lock::Timeout=300", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}
		if _, err := run("apt-get", "update", "-q", "-o", "DPkg::Lock::Timeout=300"); err != nil {
			return transcript.String(), err
		}
		pending, _, note, err := pendingHostUpgrades(ctx)
		if err != nil {
			return transcript.String(), err
		}
		if note != "" {
			transcript.WriteString(note + "\n")
		}
		selected, deferred := selectUpgrades(pending, scope, includeDisruptive)
		if len(selected) == 0 {
			summary := "Nothing to install: no " + strings.ToLower(scopeLabel(scope)) + " are pending."
			if len(deferred) > 0 {
				summary = "Nothing installed: the pending " + strings.ToLower(scopeLabel(scope)) + " are all disruptive (" + strings.Join(packageNames(deferred), ", ") + "); they wait for the maintenance window or an explicit request."
			}
			transcript.WriteString("Summary: " + summary + "\n")
			return transcript.String(), nil
		}
		args := append([]string{"-y", "-q"}, lock...)
		if len(selected) < len(pending) {
			dir, cleanup, err := aptPinDirectory(renderAptPins(pending, selected))
			if err != nil {
				return transcript.String(), err
			}
			defer cleanup()
			var held []string
			keep := map[string]bool{}
			for _, p := range selected {
				keep[p.Name] = true
			}
			for _, p := range pending {
				if !keep[p.Name] {
					held = append(held, p.Name)
				}
			}
			transcript.WriteString("Held back for this run: " + strings.Join(held, ", ") + "\n")
			args = append(args, "-o", "Dir::Etc::PreferencesParts="+dir)
		}
		transcript.WriteString("Installing: " + strings.Join(packageNames(selected), ", ") + "\n")
		out, err := run("apt-get", append(args, "--with-new-pkgs", "upgrade")...)
		if err != nil {
			return transcript.String(), err
		}
		kept := aptKeptBack(out)
		installed := 0
		for _, p := range selected {
			if !slices.Contains(kept, p.Name) {
				installed++
			}
		}
		summary := fmt.Sprintf("%s installed: %d.", scopeLabel(scope), installed)
		if len(deferred) > 0 {
			summary += " Disruptive, not installed: " + strings.Join(packageNames(deferred), ", ") + "."
		}
		if len(kept) > 0 {
			summary += " Kept back because they need a held package: " + strings.Join(kept, ", ") + "."
		}
		transcript.WriteString("Summary: " + summary + "\n")
		return transcript.String(), nil
	}
}

func (m *Manager) runHostUpdates(in HostUpdatesRun) error {
	if err := m.hostOperationsAllowed(); err != nil {
		return err
	}
	if ok, reason := aptHost(); !ok {
		return errors.New(reason)
	}
	kind := "security-updates"
	switch in.Scope {
	case "", "security":
		in.Scope = "security"
	case "all":
		kind = "all-updates"
	default:
		return errors.New("scope must be security or all")
	}
	return m.startHostJob(kind, "manual", installHostUpdates(in.Scope, in.IncludeDisruptive))
}

// disruptiveTick installs the pending disruptive upgrades once per
// maintenance window when the policy defers them there. It works from a
// fresh observation, so a window first refreshes the status and acts on the
// next tick; a reboot the upgrades require follows through rebootTick.
func (m *Manager) disruptiveTick(now time.Time) {
	m.mu.Lock()
	policy := m.hostPolicyLocked()
	var last time.Time
	if m.data.HostUpdates != nil {
		last = m.data.HostUpdates.LastDisruptiveRun
	}
	m.mu.Unlock()
	if !policy.deferDisruptive() {
		return
	}
	start, ok := policy.RebootWindow.active(now)
	if !ok || !last.Before(start) {
		return
	}
	if ok, _ := aptHost(); !ok {
		return
	}
	hc := m.host()
	hc.mu.Lock()
	status := hc.status
	hc.mu.Unlock()
	if status == nil || now.Sub(status.CheckedAt) >= hostStatusMaxAge {
		m.refreshHostUpdates(now, true)
		return
	}
	if status.Disruptive == 0 {
		m.mu.Lock()
		m.hostUpdatesStateLocked().LastDisruptiveRun = now.UTC()
		_ = m.save()
		m.mu.Unlock()
		return
	}
	if err := m.startHostJob("disruptive-updates", "schedule", installHostUpdates("disruptive", true)); err != nil {
		// Busy hosts retry every minute until the window closes.
		return
	}
	log.Printf("maintenance window: installing %d disruptive update(s): %s", status.Disruptive, strings.Join(packageNames(status.DisruptivePackages), ", "))
	m.mu.Lock()
	m.hostUpdatesStateLocked().LastDisruptiveRun = now.UTC()
	_ = m.save()
	m.mu.Unlock()
}

// hostRebootCommand is replaced by tests.
var hostRebootCommand = func(ctx context.Context) error {
	_, err := hostCommand(ctx, "systemctl", "reboot")
	return err
}

// beginRebootLocked records the reboot before it happens: the job history
// and the six-hour spacing guard must survive the restart. Caller holds m.mu.
func (m *Manager) beginRebootLocked(trigger, summary string, now time.Time) error {
	state := m.hostUpdatesStateLocked()
	previous := *state
	previous.Jobs = slices.Clone(state.Jobs)
	job := newHostJob("reboot", trigger, now)
	job.Summary = summary
	job.BootID = hostBootID()
	state.LastReboot = now.UTC()
	state.RebootDeferred = ""
	m.recordHostJobLocked(job)
	if err := m.save(); err != nil {
		*state = previous
		return err
	}
	log.Printf("host reboot (%s): %s", trigger, summary)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		// Let the API response and state write reach the panel first.
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := hostRebootCommand(ctx); err != nil {
			m.mu.Lock()
			job.Status = "failed"
			job.Summary = "Reboot command failed: " + err.Error()
			finished := time.Now().UTC()
			job.Finished = &finished
			m.recordHostJobLocked(job)
			_ = m.save()
			m.mu.Unlock()
		}
	}()
	return nil
}

// rebootHost is the manual, immediate reboot. It still refuses while
// deployments, backups or package operations are running.
func (m *Manager) rebootHost(in HostRebootRequest) error {
	if err := m.hostOperationsAllowed(); err != nil {
		return err
	}
	if !in.Confirm {
		return errors.New("confirm the reboot")
	}
	if ok, reason := aptHost(); !ok {
		return errors.New(reason)
	}
	hc := m.host()
	m.mu.Lock()
	defer m.mu.Unlock()
	if busy := m.hostBusyLocked(); busy != "" {
		return errors.New("reboot refused: " + busy)
	}
	hc.mu.Lock()
	job := hc.job
	hc.mu.Unlock()
	if job != "" {
		return errors.New("reboot refused: host " + job + " is running")
	}
	return m.beginRebootLocked("manual", "Reboot requested from the control panel.", time.Now())
}

// rebootTick reboots inside the window when required. It never reboots twice
// within six hours and never while work is running; if work is still running
// when the window closes, the reboot waits for the next window.
func (m *Manager) rebootTick(now time.Time) {
	m.mu.Lock()
	policy := m.hostPolicyLocked()
	m.mu.Unlock()
	if !policy.AutoReboot {
		return
	}
	if _, ok := policy.RebootWindow.active(now); !ok {
		return
	}
	required, pkgs, reason := rebootRequirement()
	if !required {
		return
	}
	if ok, _ := aptHost(); !ok {
		return
	}
	hc := m.host()
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.hostUpdatesStateLocked()
	if !state.LastReboot.IsZero() && now.Sub(state.LastReboot) < hostRebootMinSpacing {
		return
	}
	busy := m.hostBusyLocked()
	hc.mu.Lock()
	if busy == "" && hc.job != "" {
		busy = "host " + hc.job + " is running"
	}
	hc.mu.Unlock()
	if busy != "" {
		deferred := "Reboot waiting: " + busy + "."
		if state.RebootDeferred != deferred {
			state.RebootDeferred = deferred
			_ = m.save()
		}
		return
	}
	summary := "Scheduled reboot: " + reason
	if len(pkgs) > 0 {
		summary += " (" + strings.Join(pkgs, ", ") + ")"
	}
	if err := m.beginRebootLocked("schedule", summary+".", now); err != nil {
		log.Printf("scheduled reboot deferred: %v", err)
	}
}

// recoverHostJobsLocked closes job records interrupted by a restart: a reboot
// that was running succeeded when the agent comes back; any other job was cut
// short. Caller holds m.mu.
func (m *Manager) recoverHostJobsLocked(now time.Time) bool {
	if m.data.HostUpdates == nil {
		return false
	}
	changed := false
	for i := range m.data.HostUpdates.Jobs {
		job := &m.data.HostUpdates.Jobs[i]
		if job.Status != "running" {
			continue
		}
		finished := now.UTC()
		job.Finished = &finished
		switch {
		case job.Kind == "reboot" && job.BootID != "" && job.BootID == hostBootID():
			job.Status = "failed"
			job.Summary = strings.TrimSpace(job.Summary + " The agent restarted but the host did not reboot.")
		case job.Kind == "reboot":
			job.Status = "succeeded"
			job.Summary = strings.TrimSpace(job.Summary + " The host restarted and the agent is back.")
		default:
			job.Status = "failed"
			job.Summary = "The agent restarted during this operation; inspect the host before retrying."
		}
		changed = true
	}
	return changed
}

// hostBootID identifies the current boot, so a reboot record can tell a host
// restart from an agent restart.
func hostBootID() string {
	raw, err := os.ReadFile(hostPath("/proc/sys/kernel/random/boot_id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// writeHostFile atomically replaces a host configuration file.
func writeHostFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
