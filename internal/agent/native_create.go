package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Creating native applications. POST /v1/native/apps with a
// NativeCreateRequest creates, on this host: the application's own account
// and root, its data services, its environment file, one hardened system
// unit per process (enabled, idle until the first release) and its web
// route; then it records the release policy in agent state. The first
// deploy is an ordinary release (POST /v1/native/apps/{id}/deploy) that
// builds the repository and starts the units. DELETE /v1/native/apps/{id}
// removes a created application again; ?purge=1 also deletes its files.

// nativeCreateMu serializes creation and removal, so port allocation and
// account creation never race.
var nativeCreateMu sync.Mutex

var nativeStaticOutputPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)

// nativeCreateRoutes registers the routes of created applications. The
// create request shares POST /v1/native/apps with registration of existing
// applications; nativeCreateDispatch tells them apart.
func (m *Manager) nativeCreateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("DELETE /v1/native/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Minute))
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
		defer cancel()
		purge := r.URL.Query().Get("purge") == "1"
		if err := m.deleteNativeApp(ctx, r.PathValue("id"), purge); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]any{"status": "removed", "purged": purge})
	})
}

// nativeCreateDispatch handles POST /v1/native/apps when the body is a
// NativeCreateRequest (it carries "data" and no "name", which every
// registration of an existing application has). It returns false, with the
// body restored, for registrations.
func (m *Manager) nativeCreateDispatch(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if err != nil {
		Fail(w, 400, err)
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var keys map[string]json.RawMessage
	if json.Unmarshal(body, &keys) != nil {
		return false
	}
	_, data := keys["data"]
	_, name := keys["name"]
	if !data || name {
		return false
	}
	var request NativeCreateRequest
	if err := Decode(w, r, &request); err != nil {
		Fail(w, 400, err)
		return true
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Minute))
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	view, err := m.createNativeApp(ctx, request)
	if err != nil {
		Fail(w, 409, err)
		return true
	}
	JSON(w, 201, view)
	return true
}

// nativeCreatedPolicy returns the stored policy of a created application.
func (m *Manager) nativeCreatedPolicy(id string) (NativeReleasePolicy, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	policy, found := m.data.NativeCreated[id]
	return policy, found
}

// nativeCreatedSystemUnits reports whether id's units are system units: a
// created application, or a configured policy moved to its own account.
func (m *Manager) nativeCreatedSystemUnits(id string) bool {
	if m.cfg.Native != nil {
		if policy, ok := m.cfg.Native.Releases[id]; ok {
			return policy.Enabled && policy.SystemUnits()
		}
	}
	policy, ok := m.nativeCreatedPolicy(id)
	return ok && policy.SystemUnits()
}

// nativeCreatedApp returns a created application's registration. Created
// applications have no discovered bindings to re-verify: the agent wrote
// their units itself.
func (m *Manager) nativeCreatedApp(id string) (NativeApplication, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, created := m.data.NativeCreated[id]; !created {
		return NativeApplication{}, false
	}
	a, found := m.data.NativeApps[id]
	a.Services = slices.Clone(a.Services)
	a.AdditionalDomains = slices.Clone(a.AdditionalDomains)
	return a, found
}

// nativeCreatedSecrets are the values scrubbed from a created
// application's logs.
func (m *Manager) nativeCreatedSecrets(id string) []string {
	policy, ok := m.nativeCreatedPolicy(id)
	if !ok {
		return nil
	}
	values, err := nativeReleaseEnvValues(policy)
	if err != nil {
		return nil
	}
	return nativeEnvironmentLogSecrets(values)
}

// nativeDefaultProcesses are the processes of a runtime when the request
// names none.
func nativeDefaultProcesses(runtime, id string) []NativeProcessSpec {
	switch runtime {
	case "go":
		return []NativeProcessSpec{{Name: "web", Command: "bin/" + id, Web: true}}
	case "rails":
		return []NativeProcessSpec{{Name: "web", Command: `bin/rails server -b 127.0.0.1 -p "$PORT"`, Web: true}}
	case "node":
		return []NativeProcessSpec{{Name: "web", Command: "npm start", Web: true}}
	}
	return nil
}

func nativeDefaultHealthPath(runtime string) string {
	switch runtime {
	case "rails":
		return "/up"
	case "go":
		return "/readyz"
	}
	return "/"
}

// validateNativeCreateRequest checks a request against the host without
// changing anything and fills defaults.
func (m *Manager) validateNativeCreateRequest(req *NativeCreateRequest) error {
	if m.cfg.Native == nil {
		return errors.New("native management is not configured on this agent")
	}
	if err := validNativeCreatedID(req.ID); err != nil {
		return err
	}
	if err := validNativeAppsRoot(m.nativeAppsRoot()); err != nil {
		return err
	}
	if !repoPattern.MatchString(req.Repository) || strings.Contains(req.Repository, "..") {
		return errors.New("repository must be a GitHub owner/name")
	}
	if req.Branch == "" {
		req.Branch = "main"
	}
	if !nativeReleaseBranchValue.MatchString(req.Branch) || strings.Contains(req.Branch, "..") {
		return errors.New("invalid branch")
	}
	switch req.Runtime {
	case "go", "rails", "node", "static":
	default:
		return errors.New("runtime must be go, rails, node or static")
	}
	if req.SourceDirectory != "" && req.SourceDirectory != "." {
		return errors.New("native applications build from the repository root; a source directory is not supported")
	}
	if req.Runtime == "static" {
		if req.StaticOutput == "" {
			req.StaticOutput = "dist"
		}
		if !nativeStaticOutputPattern.MatchString(req.StaticOutput) || slices.Contains(strings.Split(req.StaticOutput, "/"), "..") {
			return errors.New("the static output must be a relative directory inside the repository")
		}
		if len(req.Processes) > 0 {
			return errors.New("static sites run no processes")
		}
	} else if req.StaticOutput != "" {
		return errors.New("only static sites publish a build output directory")
	}
	if req.HealthPath == "" {
		req.HealthPath = nativeDefaultHealthPath(req.Runtime)
	}
	if !strings.HasPrefix(req.HealthPath, "/") || len(req.HealthPath) > 200 || strings.ContainsAny(req.HealthPath, " \t\r\n?#") {
		return errors.New("health path must be an absolute path")
	}
	hosts := App{Domain: req.Domain, AdditionalDomains: req.AdditionalDomains}
	if len(req.AdditionalDomains) > 20 {
		return errors.New("at most 20 additional domains per application")
	}
	seen := map[string]bool{}
	for _, domain := range hosts.Hostnames() {
		if err := validateHostname(domain); err != nil {
			return fmt.Errorf("%s: %w", domain, err)
		}
		if seen[domain] || m.cfg.reservedHostname(domain) {
			return fmt.Errorf("%s is reserved or listed twice", domain)
		}
		seen[domain] = true
	}
	if req.Runtime != "static" {
		if len(req.Processes) == 0 {
			req.Processes = nativeDefaultProcesses(req.Runtime, req.ID)
		}
		if len(req.Processes) > 8 {
			return errors.New("at most 8 processes per application")
		}
		web, names := 0, map[string]bool{}
		for _, process := range req.Processes {
			if !nativeProcessNamePattern.MatchString(process.Name) || names[process.Name] {
				return errors.New("process names must be unique lowercase slugs of at most 20 characters")
			}
			names[process.Name] = true
			if err := validNativeCommand(process.Command); err != nil {
				return fmt.Errorf("%s: %w", process.Name, err)
			}
			if process.Web {
				web++
			}
		}
		if web != 1 {
			return errors.New("exactly one process must be the web process")
		}
		// The web process restarts first.
		slices.SortStableFunc(req.Processes, func(a, b NativeProcessSpec) int {
			if a.Web == b.Web {
				return 0
			}
			if a.Web {
				return -1
			}
			return 1
		})
	}
	if req.MemoryMiB != 0 && (req.MemoryMiB < 128 || req.MemoryMiB > 65536) {
		return errors.New("process memory must be between 128 and 65536 MiB")
	}
	if req.CPUPercent < 0 || req.CPUPercent > 6400 {
		return errors.New("process CPU quota must be between 1 and 6400 percent")
	}
	if req.Data.ExternalDatabaseURL != "" && (req.Data.Postgres || !validEnvironmentURL(req.Data.ExternalDatabaseURL) || strings.ContainsAny(req.Data.ExternalDatabaseURL, "'\r\n\x00")) {
		return errors.New("an external database needs an absolute URL and excludes a provisioned PostgreSQL database")
	}
	if len(req.Env) > 100 {
		return errors.New("at most 100 environment variables")
	}
	for key, value := range req.Env {
		if !envPattern.MatchString(key) {
			return fmt.Errorf("invalid environment variable %q", key)
		}
		switch {
		case key == "LIDZA_MASTER_KEY":
			if decoded, err := hex.DecodeString(value); err != nil || len(decoded) != 32 {
				return errors.New("LIDZA_MASTER_KEY must contain exactly 64 hexadecimal characters")
			}
		case nativeReleaseHostKey(key):
			return fmt.Errorf("%s is managed by the host", key)
		}
		if _, err := nativeReleaseEnvLine(key, value); err != nil {
			return err
		}
	}
	for key, kind := range req.EnvKinds {
		if _, ok := req.Env[key]; !ok || kind != EnvironmentSetting && kind != EnvironmentCredential {
			return fmt.Errorf("invalid environment classification for %q", key)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.data.Apps[req.ID]; exists {
		return errors.New("application ID belongs to a container application")
	}
	if _, exists := m.cfg.Native.Releases[req.ID]; exists {
		return errors.New("application ID belongs to a configured native application")
	}
	if _, exists := m.data.NativeCreated[req.ID]; exists {
		return errors.New("a native application with this ID already exists")
	}
	for id, app := range m.data.NativeApps {
		if id == req.ID || app.Name == req.ID {
			return errors.New("a native application with this ID already exists")
		}
		if (App{Domain: app.Domain, AdditionalDomains: app.AdditionalDomains}).DomainsOverlap(hosts) {
			return errors.New("domain belongs to another native application")
		}
	}
	for _, app := range m.data.Apps {
		if app.DomainsOverlap(hosts) {
			return errors.New("domain belongs to a container application")
		}
	}
	if len(m.data.NativeApps) >= 100 {
		return errors.New("native application limit reached")
	}
	return nil
}

// nativeCreateEnvironment composes the first environment file: runtime
// defaults, the request, provisioned data and the values the host owns.
func nativeCreateEnvironment(req NativeCreateRequest, root, port string, grant NativeDataGrant) (map[string]string, error) {
	values := map[string]string{}
	switch req.Runtime {
	case "rails":
		values["RAILS_ENV"] = "production"
		values["RACK_ENV"] = "production"
		values["RAILS_LOG_TO_STDOUT"] = "true"
		values["RAILS_SERVE_STATIC_FILES"] = "true"
	case "node":
		values["NODE_ENV"] = "production"
		values["HOST"] = "127.0.0.1"
	case "go":
		values["STORAGE_DIR"] = filepath.Join(root, "shared", "storage")
	}
	maps.Copy(values, req.Env)
	if req.Data.ExternalDatabaseURL != "" {
		values["DATABASE_URL"] = req.Data.ExternalDatabaseURL
	}
	maps.Copy(values, grant.Env)
	if req.Runtime == "rails" && values["SECRET_KEY_BASE"] == "" {
		secret := make([]byte, 64)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		values["SECRET_KEY_BASE"] = hex.EncodeToString(secret)
	}
	if port != "" {
		values["PORT"] = port
		if req.Runtime == "go" {
			values["LIDZA_ADDR"] = "127.0.0.1:" + port
		}
	}
	return values, nil
}

// writeNativeEnvironmentFile replaces the environment file, owned by the
// account and readable only by it.
func writeNativeEnvironmentFile(path string, account NativeAppAccount, values map[string]string) error {
	var body strings.Builder
	body.WriteString("# Application environment; edit it from the dashboard. Applies on the next deploy.\n")
	for _, key := range slices.Sorted(maps.Keys(values)) {
		line, err := nativeReleaseEnvLine(key, values[key])
		if err != nil {
			return err
		}
		body.WriteString(line + "\n")
	}
	if body.Len() > 128<<10 {
		return errors.New("the application environment exceeds 128 KiB")
	}
	temporary := path + ".tmp"
	_ = os.Remove(temporary)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.WriteString(body.String()); err == nil {
		if err = file.Chown(account.UID, account.GID); err == nil {
			err = file.Sync()
		}
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(temporary)
		return err
	}
	return os.Rename(temporary, path)
}

// createNativeApp creates an application end to end, undoing every step
// already taken when a later one fails.
func (m *Manager) createNativeApp(ctx context.Context, req NativeCreateRequest) (App, error) {
	nativeCreateMu.Lock()
	defer nativeCreateMu.Unlock()
	if err := m.validateNativeCreateRequest(&req); err != nil {
		return App{}, err
	}
	m.mu.Lock()
	if m.nativeOperations[req.ID] {
		m.mu.Unlock()
		return App{}, errors.New("an operation on this application is already running")
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[req.ID] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.nativeOperations, req.ID); m.mu.Unlock() }()

	root := filepath.Join(m.nativeAppsRoot(), req.ID)
	_, rootErr := os.Lstat(root)
	newRoot := errors.Is(rootErr, os.ErrNotExist)
	var undo []func()
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}()

	account, err := ensureNativeAppAccount(ctx, m, req.ID)
	if err != nil {
		return App{}, err
	}
	undo = append(undo, func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = m.removeNativeAppAccount(cleanup, req.ID)
		if newRoot {
			_ = os.RemoveAll(root)
		}
	})

	port := ""
	if req.Runtime != "static" {
		m.mu.Lock()
		port, err = m.allocateNativePortLocked()
		m.mu.Unlock()
		if err != nil {
			return App{}, err
		}
	}

	var grant NativeDataGrant
	if req.Data.Postgres || req.Data.Redis {
		grant, err = provisionNativeData(ctx, m, req.ID, account, req.Data)
		if errors.Is(err, errNativeCreateUnavailable) {
			return App{}, errors.New("this agent cannot provision databases yet; create the application without them or with an external database URL")
		}
		if err != nil {
			return App{}, fmt.Errorf("data services: %w", err)
		}
		undo = append(undo, func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_ = releaseNativeData(cleanup, m, req.ID)
		})
	}
	for key, value := range grant.Env {
		if !envPattern.MatchString(key) || nativeReleaseHostKey(key) {
			return App{}, fmt.Errorf("data services returned an invalid variable %q", key)
		}
		if _, err := nativeReleaseEnvLine(key, value); err != nil {
			return App{}, err
		}
	}

	values, err := nativeCreateEnvironment(req, root, port, grant)
	if err != nil {
		return App{}, err
	}
	envFile := filepath.Join(root, "shared", ".env")
	if err := writeNativeEnvironmentFile(envFile, account, values); err != nil {
		return App{}, fmt.Errorf("environment file: %w", err)
	}

	specs := make([]nativeUnitSpec, 0, len(req.Processes))
	for _, process := range req.Processes {
		specs = append(specs, nativeUnitSpec{App: req.ID, Process: process.Name, User: account.User, Root: root,
			Command: process.Command, MemoryMiB: req.MemoryMiB, CPUPercent: req.CPUPercent})
	}
	units, err := m.writeNativeUnits(ctx, specs)
	if len(units) > 0 {
		written := slices.Clone(units)
		undo = append(undo, func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_ = m.removeNativeUnits(cleanup, written)
		})
	}
	if err != nil {
		return App{}, err
	}

	policy := NativeReleasePolicy{Enabled: true, Root: root, Repository: req.Repository, Branch: req.Branch, Runtime: req.Runtime,
		Output: req.StaticOutput, Domain: req.Domain, Port: port, HealthPath: req.HealthPath, Units: units,
		EnvFiles: []string{envFile}, BuildMemoryMiB: req.BuildMemoryMiB, Databases: slices.Clone(grant.Databases),
		User: account.User, UnitScope: "system"}
	if err := policy.Validate(); err != nil {
		return App{}, err
	}
	app := NativeApplication{Runtime: req.Runtime, AdditionalDomains: slices.Clone(req.AdditionalDomains), Port: port,
		HealthPath: req.HealthPath, ID: req.ID, Name: req.ID, Domain: req.Domain, Repository: req.Repository,
		Branch: req.Branch, Services: slices.Clone(units), Registered: time.Now().UTC()}
	if req.Runtime == "static" {
		app.DocumentRoot = req.StaticOutput
	}
	kinds := map[string]string{}
	for key, kind := range req.EnvKinds {
		kinds[key] = kind
	}
	for key := range grant.Env {
		kinds[key] = EnvironmentCredential
	}

	if err := m.publishNativeCaddyRoute(ctx, req.ID, req.Domain, req.AdditionalDomains); err != nil {
		return App{}, fmt.Errorf("could not publish the application's hostnames: %w", err)
	}
	undo = append(undo, func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer stop()
		_ = m.withdrawNativeCaddyRoute(cleanup, req.ID)
	})

	m.mu.Lock()
	if m.data.NativeCreated == nil {
		m.data.NativeCreated = map[string]NativeReleasePolicy{}
	}
	if m.data.NativeApps == nil {
		m.data.NativeApps = map[string]NativeApplication{}
	}
	if m.data.NativeSettings == nil {
		m.data.NativeSettings = map[string]NativeSettingsOverride{}
	}
	if m.data.NativeCreatedSpecs == nil {
		m.data.NativeCreatedSpecs = map[string]NativeCreateRequest{}
	}
	previousSettings, hadSettings := m.data.NativeSettings[req.ID]
	m.data.NativeCreated[req.ID] = policy
	m.data.NativeApps[req.ID] = app
	m.data.NativeSettings[req.ID] = NativeSettingsOverride{Kinds: kinds}
	m.data.NativeCreatedSpecs[req.ID] = nativeCreatedSpec(req)
	if err := m.save(); err != nil {
		delete(m.data.NativeCreated, req.ID)
		delete(m.data.NativeApps, req.ID)
		delete(m.data.NativeCreatedSpecs, req.ID)
		if hadSettings {
			m.data.NativeSettings[req.ID] = previousSettings
		} else {
			delete(m.data.NativeSettings, req.ID)
		}
		m.mu.Unlock()
		return App{}, err
	}
	m.mu.Unlock()
	succeeded = true
	m.wakeDomains()
	for _, view := range m.nativeViews() {
		if view.ID == req.ID {
			return view, nil
		}
	}
	return App{ID: req.ID, Native: &app, Runtime: req.Runtime, Repository: req.Repository, Branch: req.Branch, Domain: req.Domain}, nil
}

// nativeCreatedSpec is the request as recorded for recreating the
// application elsewhere: no environment values and no external database URL.
func nativeCreatedSpec(req NativeCreateRequest) NativeCreateRequest {
	spec := req
	spec.Env, spec.EnvKinds = nil, nil
	spec.Data.ExternalDatabaseURL = ""
	spec.Processes = slices.Clone(req.Processes)
	spec.AdditionalDomains = slices.Clone(req.AdditionalDomains)
	return spec
}

// deleteNativeApp removes a created application: its units, route, data
// services and account; with purge also its root (releases, shared files,
// environment). Applications registered from existing host configuration
// are never removed here.
func (m *Manager) deleteNativeApp(ctx context.Context, id string, purge bool) error {
	nativeCreateMu.Lock()
	defer nativeCreateMu.Unlock()
	policy, created := m.nativeCreatedPolicy(id)
	if !created {
		return errors.New("only applications created by this agent can be removed here")
	}
	if policy.Root != filepath.Join(m.nativeAppsRoot(), id) || policy.User != nativeAppUser(id) {
		return errors.New("the application's stored policy does not match its account; review it on the host")
	}
	m.mu.Lock()
	if m.nativeOperations[id] {
		m.mu.Unlock()
		return errors.New("an operation on this application is running; remove it after it finishes")
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[id] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.nativeOperations, id); m.mu.Unlock() }()

	if err := m.removeNativeUnits(ctx, policy.Units); err != nil {
		return fmt.Errorf("could not remove the application's units: %w", err)
	}
	// The agent's own route goes with the registration: the proxy and
	// certificate checks only serve hosts of registered applications. A
	// fronting Caddy route is withdrawn explicitly.
	if err := m.withdrawNativeCaddyRoute(ctx, id); err != nil {
		return fmt.Errorf("could not withdraw the application's hostnames: %w", err)
	}
	if err := releaseNativeData(ctx, m, id); err != nil && !errors.Is(err, errNativeCreateUnavailable) {
		return fmt.Errorf("data services: %w", err)
	}
	if err := m.removeNativeAppAccount(ctx, id); err != nil {
		return err
	}
	if purge {
		if err := os.RemoveAll(policy.Root); err != nil {
			return fmt.Errorf("could not delete the application files: %w", err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data.NativeCreated, id)
	delete(m.data.NativeCreatedSpecs, id)
	delete(m.data.NativeApps, id)
	delete(m.data.NativeSettings, id)
	delete(m.data.NativeReleases, id)
	delete(m.data.NativeBindings, id)
	return m.save()
}
