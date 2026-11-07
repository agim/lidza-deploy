package agent

// Native applications keep their current files, environment and user services.
// Their registry is separate from container apps: Docker reconciliation, cleanup,
// deployments and routing must never acquire ownership merely through discovery.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// NativeHostConfig is decoded with encoding/json, which ignores unknown keys.
// Existing host configurations may still contain keys of the retired native
// Rails layer (rails_serving, rails_runtimes, rails_workers,
// rails_preparations, rails_cron_baselines); they are accepted and ignored.
type NativeHostConfig struct {
	Databases         map[string]NativeDatabasePolicy `json:"databases,omitempty"`
	StaticDeployments map[string]NativeStaticPolicy   `json:"static_deployments,omitempty"`
	Releases          map[string]NativeReleasePolicy  `json:"releases,omitempty"`
	Root              string                          `json:"root"`
	User              string                          `json:"user"`
	ServiceControl    bool                            `json:"service_control"`

	SQLiteSidecarBackups map[string]bool `json:"sqlite_sidecar_backups,omitempty"`
	// HostData tunes the PostgreSQL/Redis provisioned for created applications.
	HostData *NativeHostDataConfig `json:"host_data,omitempty"`

	// AppsRoot holds the roots of applications this agent creates, each
	// owned by its own account; default /srv/lidza/apps.
	AppsRoot string `json:"apps_root,omitempty"`
	// AppPorts is the inclusive loopback port range assigned to the web
	// processes of created applications; default 4100-4999.
	AppPorts [2]int `json:"app_ports,omitzero"`
	// Caddy names an admin-API Caddy that owns this host's web ports; created
	// applications are then published there (see native_caddy.go).
	Caddy *NativeCaddyConfig `json:"caddy,omitempty"`
}

type NativeApplication struct {
	Runtime           string    `json:"runtime,omitempty"`
	DocumentRoot      string    `json:"document_root,omitempty"`
	AdditionalDomains []string  `json:"additional_domains,omitempty"`
	Port              string    `json:"port,omitempty"`
	HealthPath        string    `json:"health_path"`
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Domain            string    `json:"domain"`
	Repository        string    `json:"repository"`
	Branch            string    `json:"branch"`
	Services          []string  `json:"services"`
	Registered        time.Time `json:"registered"`
}

type NativeService struct {
	Unit          string `json:"unit"`
	Active        string `json:"active"`
	State         string `json:"state"`
	UnitFileState string `json:"unit_file_state,omitempty"`
}

type NativeStatus struct {
	Readiness             NativeReadiness   `json:"readiness"`
	Application           NativeApplication `json:"application"`
	Services              []NativeService   `json:"services"`
	ServiceControl        bool              `json:"service_control"`
	DeploymentSupported   bool              `json:"deployment_supported"`
	ReleaseManaged        bool              `json:"release_managed,omitempty"`
	ConfigurationEditable bool              `json:"configuration_editable"`
	Checked               time.Time         `json:"checked"`
}

var nativeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,100}$`)
var nativeUnitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{0,149}\.service$`)

func (c NativeHostConfig) validate() error {
	if !filepath.IsAbs(c.Root) || filepath.Clean(c.Root) != c.Root || c.Root == "/" || !regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`).MatchString(c.User) || c.User == "root" {
		return errors.New("native management requires a clean absolute application root and a non-root user")
	}
	if c.Caddy != nil {
		return c.Caddy.validate()
	}
	return nil
}

func (m *Manager) nativeRoot() (string, error) {
	if m.cfg.Native == nil {
		return "", errors.New("native host access is not configured")
	}
	if err := m.cfg.Native.validate(); err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(m.cfg.Native.Root)
	if err != nil || root != m.cfg.Native.Root {
		return "", errors.New("native application root must exist without a symlink")
	}
	return root, nil
}

// nativeBinding returns the deployment user's service units bound to
// <root>/<name>/current, or none for a built static Astro site. It is the
// binding of applications that run on that user's own service manager;
// applications with system units are described by their release policy
// instead (nativeRuntimeServices).
func (m *Manager) nativeBinding(name string) ([]string, error) {
	root, err := m.nativeRoot()
	if err != nil {
		return nil, err
	}
	if !nativeNamePattern.MatchString(name) {
		return nil, errors.New("invalid native application name")
	}
	app := filepath.Join(root, name)
	info, err := os.Lstat(app)
	if err != nil || !info.IsDir() {
		return nil, errors.New("native application directory is unavailable")
	}
	current, err := filepath.EvalSymlinks(filepath.Join(app, "current"))
	if err != nil {
		return nil, errors.New("native current release is unavailable")
	}
	releases := filepath.Join(app, "releases")
	rel, err := filepath.Rel(releases, current)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("native current release is outside its releases directory")
	}
	// Check every traversed release parent, not just a lexical prefix.
	realReleases, err := filepath.EvalSymlinks(releases)
	if err != nil || realReleases != releases {
		return nil, errors.New("native releases must not redirect outside the application")
	}
	static := false
	if err := regularFile(filepath.Join(current, "Gemfile"), false); err != nil {
		if regularFile(filepath.Join(current, "package.json"), false) != nil || regularFile(filepath.Join(current, "public", "index.html"), false) != nil {
			return nil, errors.New("native discovery requires Rails or a built Astro website")
		}
		data, err := os.ReadFile(filepath.Join(current, "package.json"))
		if err != nil || len(data) > 1<<20 {
			return nil, errors.New("Astro package metadata unavailable")
		}
		var pkg struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		if json.Unmarshal(data, &pkg) != nil || pkg.Dependencies["astro"] == "" && pkg.DevDependencies["astro"] == "" {
			return nil, errors.New("native package does not identify Astro")
		}
		public, err := filepath.EvalSymlinks(filepath.Join(current, "public"))
		if err != nil || public != filepath.Join(current, "public") {
			return nil, errors.New("Astro document root must stay inside its release")
		}
		static = true
	}
	unitDir := filepath.Join(root, ".config", "systemd", "user")
	realUnits, err := filepath.EvalSymlinks(unitDir)
	if err != nil || realUnits != unitDir {
		return nil, errors.New("native user service directory must not be a symlink")
	}
	files, err := os.ReadDir(unitDir)
	if err != nil {
		return nil, errors.New("native user services unavailable")
	}
	units := []string{}
	for _, file := range files {
		if !nativeUnitPattern.MatchString(file.Name()) {
			continue
		}
		path := filepath.Join(unitDir, file.Name())
		stat, e := os.Lstat(path)
		if e != nil || !stat.Mode().IsRegular() || stat.Size() > 64<<10 {
			continue
		}
		data, e := os.ReadFile(path)
		if e != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && key == "WorkingDirectory" && value == filepath.Join(app, "current") {
				units = append(units, file.Name())
				break
			}
		}
	}
	slices.Sort(units)
	if len(units) == 0 && !static || len(units) > 32 || static && len(units) > 0 {
		return nil, errors.New("native application needs between 1 and 32 directly bound user services")
	}
	return units, nil
}

// nativeSystemPolicy returns the release policy whose system units an
// application runs: reviewed configuration first, then a policy the agent
// created. Applications on the deployment user's service manager have none.
func (m *Manager) nativeSystemPolicy(id string) (NativeReleasePolicy, bool) {
	if !m.nativeCreatedSystemUnits(id) {
		return NativeReleasePolicy{}, false
	}
	if m.cfg.Native != nil {
		if policy, ok := m.cfg.Native.Releases[id]; ok {
			return policy, true
		}
	}
	return m.nativeCreatedPolicy(id)
}

// nativeSystemUnitFiles returns the units of a policy that exist as unit
// files in the system unit directory, in policy order: the units a deploy
// restarts, then the ones the operator preserves. A unit without a file
// (removed, or masked) is left out, so the panel can name it.
func nativeSystemUnitFiles(policy NativeReleasePolicy) []string {
	units := []string{}
	for _, unit := range slices.Concat(policy.Units, policy.Preserve) {
		if !nativeUnitPattern.MatchString(unit) {
			continue
		}
		info, err := os.Lstat(filepath.Join(nativeSystemUnitDir, unit))
		if err == nil && info.Mode().IsRegular() {
			units = append(units, unit)
		}
	}
	return units
}

// nativeRuntimeServices reports the services an application runs today. A
// release policy with system units is the source of truth: for applications
// the agent created, and for configured applications that moved to their own
// account, the user units that move left on the deployment user's manager
// are not the application's services any more. Every other application is
// bound through the user units of <root>/<name>/current.
func (m *Manager) nativeRuntimeServices(a NativeApplication) ([]string, error) {
	if policy, ok := m.nativeSystemPolicy(a.ID); ok {
		return nativeSystemUnitFiles(policy), nil
	}
	return m.nativeBinding(a.Name)
}

// nativeDiscovery is the host's native inventory, read without touching a
// service: every registered application with the services it runs today,
// configured system-unit policies nobody has registered yet, and finally the
// directory scan of the native root, which exists to adopt existing
// applications and only proposes names that are not registered. The panel
// compares the first group with the agent's registrations and adopts the rest.
func (m *Manager) nativeDiscovery(ctx context.Context) ([]NativeApplication, error) {
	root, err := m.nativeRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, errors.New("native root unavailable")
	}
	m.mu.Lock()
	apps := maps.Clone(m.data.NativeApps)
	// A configured policy of a deleted application is not offered again:
	// it is restored from its archive, never re-adopted from the file.
	archived := map[string]bool{}
	for _, a := range m.data.AppArchives {
		archived[a.AppID] = true
	}
	m.mu.Unlock()
	out := []NativeApplication{}
	reported := map[string]bool{}
	for _, id := range slices.Sorted(maps.Keys(apps)) {
		a := apps[id]
		reported[a.Name] = true
		units, err := m.nativeRuntimeServices(a)
		if err != nil {
			// A registration whose binding cannot be verified stays out of the
			// snapshot; the panel names it and keeps its record.
			continue
		}
		a.Services = units
		a.AdditionalDomains = slices.Clone(a.AdditionalDomains)
		out = append(out, a)
	}
	for _, id := range slices.Sorted(maps.Keys(m.cfg.Native.Releases)) {
		policy := m.cfg.Native.Releases[id]
		name := filepath.Base(policy.Root)
		if _, registered := apps[id]; registered || archived[id] || reported[name] || !policy.Enabled || !policy.SystemUnits() || !nativeNamePattern.MatchString(name) || len(out) >= 100 {
			continue
		}
		units := nativeSystemUnitFiles(policy)
		if len(units) == 0 {
			continue
		}
		reported[name] = true
		// Without an ID the panel observes the application instead of
		// registering it: metadata registration binds user units, which a
		// system-unit policy does not have.
		out = append(out, NativeApplication{Name: name, Runtime: policy.Runtime, Domain: policy.Domain, Repository: policy.Repository, Branch: policy.Branch, Port: policy.Port, HealthPath: policy.HealthPath, Services: units})
	}
	for _, e := range entries {
		if len(out) >= 100 {
			break
		}
		if !e.IsDir() || !nativeNamePattern.MatchString(e.Name()) || reported[e.Name()] {
			continue
		}
		if units, err := m.nativeBinding(e.Name()); err == nil {
			out = append(out, m.nativeDiscoveredApplication(e.Name(), units))
		}
	}
	return out, nil
}

func (m *Manager) nativeExec(ctx context.Context, args ...string) (string, error) {
	if m.nativeCommand != nil {
		return m.nativeCommand(ctx, args)
	}
	account, err := user.Lookup(m.cfg.Native.User)
	if err != nil {
		return "", errors.New("native deployment user is unavailable")
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid <= 0 {
		return "", errors.New("invalid native deployment user")
	}
	// No PAM session is opened for observation/control. A session opener can
	// start a previously stopped user manager and its enabled applications.
	argv := []string{"--reuid=" + account.Uid, "--regid=" + account.Gid, "--init-groups", "--no-new-privs", "--", "/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "HOME=" + account.HomeDir, "XDG_RUNTIME_DIR=/run/user/" + account.Uid}
	argv = append(argv, args...)
	if os.Geteuid() == uid {
		return command(ctx, "", nil, "/usr/bin/env", append([]string{"-i", "PATH=/usr/bin:/bin", "HOME=" + account.HomeDir, "XDG_RUNTIME_DIR=/run/user/" + account.Uid}, args...)...)
	}
	return command(ctx, "", nil, "/usr/bin/setpriv", argv...)
}

func (m *Manager) nativeRegistration(in NativeApplication) (NativeApplication, error) {
	if !idPattern.MatchString(in.ID) {
		return in, errors.New("invalid application ID")
	}
	units, err := m.nativeBinding(in.Name)
	if err != nil {
		return in, err
	}
	if len(units) == 0 {
		if in.Runtime != "astro" || in.DocumentRoot != "public" {
			return in, errors.New("static Astro registration requires its verified public document root")
		}
	} else if in.Runtime != "" && in.Runtime != "rails" {
		return in, errors.New("service bindings do not match the requested native runtime")
	}
	if in.Repository == "" {
		in.Repository = "native/" + in.ID
	}
	if in.Branch == "" {
		in.Branch = "main"
	}
	if in.HealthPath == "" {
		in.HealthPath = "/up"
	}
	if err := validateNativeHealth(in); err != nil {
		return in, err
	}
	in.Port = m.nativeConfiguredPort(in.Name, units)
	revisions, err := m.nativeRevisions(units)
	if err != nil {
		return in, err
	}
	in.Services = units
	in.Registered = time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.data.Apps[in.ID]; exists {
		return in, errors.New("application ID belongs to a container application")
	}
	for _, archive := range m.data.AppArchives {
		if archive.AppID == in.ID && archive.Kind == archiveKindAdopted {
			return in, errors.New("a deleted application with this id can still be restored; restore it, delete it permanently, or choose another id")
		}
	}
	for _, app := range m.data.Apps {
		if app.DomainsOverlap(App{Domain: in.Domain, AdditionalDomains: in.AdditionalDomains}) {
			return in, errors.New("domain belongs to a container application")
		}
	}
	if err := m.checkRedirectHostsLocked((App{Domain: in.Domain, AdditionalDomains: in.AdditionalDomains}).Hostnames(), ""); err != nil {
		return in, err
	}
	for id, app := range m.data.NativeApps {
		if id != in.ID && (App{Domain: app.Domain, AdditionalDomains: app.AdditionalDomains}).DomainsOverlap(App{Domain: in.Domain, AdditionalDomains: in.AdditionalDomains}) {
			return in, errors.New("domain belongs to another native application")
		}
		if id == in.ID && app.Name == in.Name && app.Domain == in.Domain && app.Repository == in.Repository && app.Branch == in.Branch && slices.Equal(app.Services, in.Services) && app.HealthPath == in.HealthPath && app.Port == in.Port && app.Runtime == in.Runtime && app.DocumentRoot == in.DocumentRoot && slices.Equal(app.AdditionalDomains, in.AdditionalDomains) && maps.Equal(m.data.NativeBindings[id], revisions) {
			return app, nil
		}
		if id == in.ID || app.Name == in.Name {
			return in, errors.New("native application is already registered")
		}
	}
	if len(m.data.NativeApps) >= 100 {
		return in, errors.New("native application limit reached")
	}
	if m.data.NativeApps == nil {
		m.data.NativeApps = map[string]NativeApplication{}
	}
	if m.data.NativeBindings == nil {
		m.data.NativeBindings = map[string]map[string]string{}
	}
	m.data.NativeApps[in.ID] = in
	m.data.NativeBindings[in.ID] = revisions
	if err := m.save(); err != nil {
		delete(m.data.NativeApps, in.ID)
		delete(m.data.NativeBindings, in.ID)
		return in, err
	}
	return in, nil
}

// Caller holds the inventory mutex. Native registrations reserve their primary
// name even when they were added after the reviewed host reservation snapshot.
func (m *Manager) checkNativeDomainsLocked(a App) error {
	for _, native := range m.data.NativeApps {
		if a.DomainsOverlap(App{Domain: native.Domain, AdditionalDomains: native.AdditionalDomains}) {
			return errors.New("domain belongs to a native application")
		}
	}
	return nil
}

func (m *Manager) nativeApp(id string) (NativeApplication, error) {
	m.mu.Lock()
	a, found := m.data.NativeApps[id]
	a.Services = slices.Clone(a.Services)
	a.AdditionalDomains = slices.Clone(a.AdditionalDomains)
	expected := maps.Clone(m.data.NativeBindings[id])
	m.mu.Unlock()
	if !found {
		return a, errors.New("native application is not registered")
	}
	if created, ok := m.nativeCreatedApp(id); ok {
		return created, nil
	}
	units, err := m.nativeBinding(a.Name)
	if err != nil {
		return a, err
	}
	actual, err := m.nativeRevisions(units)
	if err != nil {
		return a, err
	}
	if !slices.Equal(units, a.Services) || len(expected) == 0 && a.Runtime != "astro" || !maps.Equal(expected, actual) {
		return a, errors.New("native service bindings changed; review them before management")
	}
	return a, nil
}

func (m *Manager) nativeViews() []App {
	m.mu.Lock()
	apps := maps.Clone(m.data.NativeApps)
	histories := maps.Clone(m.data.NativeReleases)
	stopped := maps.Clone(m.data.NativeStopped)
	redirects := map[string][]RedirectDomain{}
	for id := range apps {
		if list := m.data.Redirects[id]; len(list) > 0 {
			redirects[id] = slices.Clone(list)
		}
	}
	m.mu.Unlock()
	views := []App{}
	for _, binding := range apps {
		a := binding
		a.Services = slices.Clone(a.Services)
		a.AdditionalDomains = slices.Clone(a.AdditionalDomains)
		runtime := a.Runtime
		if runtime == "" {
			runtime = "rails"
		}
		view := App{Management: m.nativeManagementCapabilities(a), ID: a.ID, Runtime: runtime, Repository: a.Repository, Branch: a.Branch, Domain: a.Domain, AdditionalDomains: slices.Clone(a.AdditionalDomains), RedirectDomains: redirects[a.ID], Native: &a}
		_, view.Stopped = stopped[a.ID]
		if history := histories[a.ID]; history.Current.Path != "" {
			view.Current = &Release{ID: history.Current.ID, Commit: history.Current.Commit}
		}
		views = append(views, view)
	}
	slices.SortFunc(views, func(a, b App) int { return strings.Compare(a.ID, b.ID) })
	return views
}

func (m *Manager) nativeStatus(ctx context.Context, id string) (NativeStatus, error) {
	a, err := m.nativeApp(id)
	if err != nil {
		return NativeStatus{}, err
	}
	_, created := m.nativeCreatedPolicy(id)
	status := NativeStatus{Application: a, Services: []NativeService{}, ServiceControl: m.cfg.Native.ServiceControl || created, Checked: time.Now().UTC()}
	if _, err := m.nativeStaticPolicy(a); err == nil {
		status.DeploymentSupported = true
	}
	if m.nativeReleaseManaged(id) {
		status.DeploymentSupported, status.ReleaseManaged = true, true
	}
	if len(a.Services) > 0 {
		out, err := m.nativeServiceMetadataFor(ctx, id, a.Services)
		if err != nil {
			return NativeStatus{}, errors.New("native service status unavailable")
		}
		for _, block := range strings.Split(out, "\n\n") {
			s := NativeService{}
			for _, line := range strings.Split(block, "\n") {
				key, val, _ := strings.Cut(line, "=")
				switch key {
				case "Id":
					s.Unit = val
				case "ActiveState":
					s.Active = val
				case "SubState":
					s.State = val
				case "UnitFileState":
					s.UnitFileState = val
				}
			}
			if slices.Contains(a.Services, s.Unit) {
				status.Services = append(status.Services, s)
			}
		}
	}
	if len(status.Services) != len(a.Services) {
		return NativeStatus{}, errors.New("native status response is incomplete")
	}
	// An HTTP connection can activate a stopped systemd service through its
	// listening socket. Even an active-state check would race with a stop before
	// the request. Status reads therefore never connect to the application's port.
	status.Readiness = NativeReadiness{Path: a.HealthPath, Message: "HTTP readiness is not probed during status checks; stopped and socket-activated services are preserved."}
	return status, nil
}

func (m *Manager) nativeSecrets(a NativeApplication) []string {
	values := m.nativeEnvironment(a.Name)
	return nativeEnvironmentLogSecrets(values)
}

func nativeEnvironmentLogSecrets(values map[string]string) []string {
	// Small, non-secret values (ports, thread counts, booleans) are not
	// masked: redacting them would corrupt unrelated log text. All other
	// variables retain conservative value/password redaction.
	if nativePort(values) != "" {
		delete(values, "PORT")
	}
	for _, key := range []string{"RAILS_MAX_THREADS", "RAILS_MIN_THREADS", "WEB_CONCURRENCY"} {
		if regexp.MustCompile(`^[0-9]{1,4}$`).MatchString(values[key]) {
			delete(values, key)
		}
	}
	for _, key := range []string{"RAILS_LOG_TO_STDOUT", "RAILS_SERVE_STATIC_FILES"} {
		if values[key] == "true" || values[key] == "false" {
			delete(values, key)
		}
	}
	for _, key := range []string{"RAILS_ENV", "RACK_ENV", "NODE_ENV"} {
		if slices.Contains([]string{"production", "development", "test", "staging"}, values[key]) {
			delete(values, key)
		}
	}
	return outputSecrets(values)
}

func (m *Manager) nativeLogs(ctx context.Context, id, unit string) (string, error) {
	a, err := m.nativeApp(id)
	if err != nil {
		return "", err
	}
	units := []string{unit}
	if unit == "" {
		// Without a selection the log interleaves every process of the
		// application, as one container's output does.
		units = a.Services
		if len(units) == 0 {
			return "This application has no service process. Its files are served by the web server.", nil
		}
	} else if !slices.Contains(a.Services, unit) {
		return "", errors.New("select a registered native service")
	}
	out, err := m.nativeServiceJournalFor(ctx, id, units...)
	if err != nil {
		return "", errors.New("native service logs unavailable")
	}
	secrets := m.nativeSecretsFor(a)
	return scrubOutput(out, secrets), nil
}

func (m *Manager) nativeServiceAction(ctx context.Context, id, unit, action, confirm string) error {
	a, err := m.nativeApp(id)
	if err != nil {
		return err
	}
	if _, created := m.nativeCreatedPolicy(id); !m.cfg.Native.ServiceControl && !created {
		return errors.New("native service control is disabled; review and approve the agent configuration first")
	}
	if confirm != a.Name || !slices.Contains(a.Services, unit) || !slices.Contains([]string{"start", "stop", "restart"}, action) {
		return errors.New("explicit application-name confirmation and a registered service action are required")
	}
	if _, stopped := m.nativeStopped(id); stopped && action != "stop" {
		return errors.New("the application is stopped; start the application first")
	}
	m.mu.Lock()
	if m.nativeOperations[id] {
		m.mu.Unlock()
		return errors.New("native service operation is already active")
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[id] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.nativeOperations, id); m.mu.Unlock() }()
	if err := m.nativeServiceActionFor(ctx, id, unit, action); err != nil {
		return fmt.Errorf("native service %s failed; inspect its status before retrying", action)
	}
	return nil
}

// nativeServiceMetadata reads the systemd state of the owner's user units.
func (m *Manager) nativeServiceMetadata(ctx context.Context, units []string) (string, error) {
	args := []string{"systemctl", "--user", "show", "--property=Id,ActiveState,SubState,UnitFileState", "--no-pager"}
	for _, unit := range units {
		if !nativeUnitPattern.MatchString(unit) {
			return "", errors.New("native metadata services contain an invalid identity")
		}
		args = append(args, unit)
	}
	return m.nativeExec(ctx, args...)
}

func (m *Manager) nativeServiceJournal(ctx context.Context, units ...string) (string, error) {
	args := []string{"journalctl"}
	for _, unit := range units {
		args = append(args, "--user-unit="+unit)
	}
	return m.nativeExec(ctx, append(args, nativeJournalFormat(units)...)...)
}

// nativeJournalFormat is the journal output of one or several units: the
// plain messages of one unit, timestamped lines naming their process when
// several units are interleaved.
func nativeJournalFormat(units []string) []string {
	if len(units) > 1 {
		return []string{"--no-pager", "--lines=400", "--output=short-iso"}
	}
	return []string{"--no-pager", "--lines=200", "--output=cat"}
}

func (m *Manager) nativeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/native/databases/{resource}/adopt", m.nativeDatabaseRoute)
	m.nativeHostDataRoutes(mux)
	m.nativeCreateRoutes(mux)
	m.nativeUsageRoutes(mux)
	mux.HandleFunc("GET /v1/native/capabilities", func(w http.ResponseWriter, r *http.Request) {
		if _, err := m.nativeRoot(); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]bool{"status_metadata_only": true, "http_readiness_probes": false, "automatic_registration_metadata_only": true, "astro_static_inventory": true})
	})
	mux.HandleFunc("GET /v1/native/discovery", func(w http.ResponseWriter, r *http.Request) {
		out, err := m.nativeDiscovery(r.Context())
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, out)
	})
	mux.HandleFunc("POST /v1/native/apps", func(w http.ResponseWriter, r *http.Request) {
		if m.nativeCreateDispatch(w, r) {
			return
		}
		var in NativeApplication
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		app, err := m.nativeRegistration(in)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 201, app)
	})
	mux.HandleFunc("GET /v1/native/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		status, err := m.nativeStatus(ctx, r.PathValue("id"))
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, status)
	})
	mux.HandleFunc("GET /v1/native/apps/{id}/settings", m.nativeSettingsRoute)
	mux.HandleFunc("PATCH /v1/native/apps/{id}/settings", m.patchNativeSettingsRoute)
	mux.HandleFunc("GET /v1/native/apps/{id}/environment/{key}", m.revealNativeCredential)
	mux.HandleFunc("POST /v1/native/apps/{id}/release", m.nativeReleaseRoute(false))
	mux.HandleFunc("GET /v1/native/apps/{id}/releases", m.nativeReleasesRoute)
	mux.HandleFunc("POST /v1/native/apps/{id}/deploy", func(w http.ResponseWriter, r *http.Request) {
		if m.nativeReleaseManaged(r.PathValue("id")) {
			m.nativeReleaseRoute(false)(w, r)
			return
		}
		m.nativeStaticDeployRoute(w, r)
	})
	mux.HandleFunc("POST /v1/native/apps/{id}/rollback", func(w http.ResponseWriter, r *http.Request) {
		if m.nativeReleaseManaged(r.PathValue("id")) {
			m.nativeReleaseRoute(true)(w, r)
			return
		}
		if err := m.rollbackNativeStatic(r.PathValue("id")); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "rolled_back"})
	})
	mux.HandleFunc("GET /v1/native/apps/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		out, err := m.nativeLogs(ctx, r.PathValue("id"), r.URL.Query().Get("unit"))
		if err != nil {
			Fail(w, 409, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, map[string]string{"log": out})
	})
	mux.HandleFunc("POST /v1/native/apps/{id}/services/{unit}/{action}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Confirm string `json:"confirm"`
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		if err := m.nativeServiceAction(ctx, r.PathValue("id"), r.PathValue("unit"), r.PathValue("action"), in.Confirm); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "requested"})
	})
}
