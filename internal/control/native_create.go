package control

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Creating native applications from the wizard. The control panel validates
// the request, forwards it to the chosen server's agent, which creates the
// account, data services, units and route, and registers the application
// like other native applications. Containerized applications keep using
// POST /api/control/apps.

type nativeCreateInput struct {
	agent.NativeCreateRequest
	ServerID           string `json:"server_id"`
	GitHubInstallation int64  `json:"github_installation,omitempty"`
	// Deploy starts the first release once the application is registered.
	Deploy bool `json:"deploy,omitempty"`
}

var (
	nativeProcessName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	nativeRuntimes    = map[string]bool{"go": true, "rails": true, "node": true, "static": true}
)

func validateNativeCreate(in agent.NativeCreateRequest) error {
	if !nativeRuntimes[in.Runtime] {
		return errors.New("choose the go, rails, node or static runtime")
	}
	// Identity, repository, branch, hostnames, source directory and
	// environment follow the same rules as every application.
	spec := agent.App{ID: in.ID, Repository: in.Repository, Branch: in.Branch, Domain: in.Domain, AdditionalDomains: in.AdditionalDomains, Env: in.Env, EnvKinds: in.EnvKinds, Build: agent.BuildConfig{SourceDirectory: in.SourceDirectory}}
	if err := spec.Validate(); err != nil {
		return err
	}
	if in.HealthPath != "" {
		u, err := url.ParseRequestURI(in.HealthPath)
		if err != nil || len(in.HealthPath) > 200 || !strings.HasPrefix(in.HealthPath, "/") || strings.HasPrefix(in.HealthPath, "//") || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(in.HealthPath, "\\\r\n\x00") || strings.Contains(in.HealthPath, "..") {
			return errors.New("the readiness path must be an absolute local path without a query")
		}
	}
	if in.Runtime == "static" {
		if len(in.Processes) > 0 {
			return errors.New("static sites have no processes")
		}
		if in.StaticOutput != "" && in.StaticOutput != "." && (path.Clean(in.StaticOutput) != in.StaticOutput || strings.HasPrefix(in.StaticOutput, "/") || strings.HasPrefix(in.StaticOutput, "../") || in.StaticOutput == ".." || strings.ContainsAny(in.StaticOutput, "\\\r\n\x00")) {
			return errors.New("the build output directory must stay inside the repository")
		}
	} else {
		if len(in.Processes) == 0 || len(in.Processes) > 10 {
			return errors.New("define a web process and at most 10 processes")
		}
		seen, web := map[string]bool{}, 0
		for _, p := range in.Processes {
			if !nativeProcessName.MatchString(p.Name) || seen[p.Name] {
				return fmt.Errorf("process names must be unique lowercase words, not %q", p.Name)
			}
			seen[p.Name] = true
			if strings.TrimSpace(p.Command) == "" || len(p.Command) > 500 || strings.ContainsAny(p.Command, "\r\n\x00") {
				return fmt.Errorf("enter a single-line command for the %s process", p.Name)
			}
			if p.Web {
				web++
			}
		}
		if web != 1 {
			return errors.New("exactly one process must be the web process")
		}
	}
	d := in.Data
	if d.Postgres && d.ExternalDatabaseURL != "" {
		return errors.New("choose either a new PostgreSQL database or an existing database URL")
	}
	if d.ExternalDatabaseURL != "" {
		u, err := url.Parse(d.ExternalDatabaseURL)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" || strings.ContainsAny(d.ExternalDatabaseURL, "\r\n\x00") {
			return errors.New("the database URL must be a postgres:// connection URL")
		}
	}
	if _, set := in.Env["DATABASE_URL"]; set && (d.Postgres || d.ExternalDatabaseURL != "") {
		return errors.New("database settings supply DATABASE_URL automatically")
	}
	if _, set := in.Env["REDIS_URL"]; set && d.Redis {
		return errors.New("Redis settings supply REDIS_URL automatically")
	}
	if d.BackupHours < 0 || d.BackupHours > 8760 {
		return errors.New("choose a backup frequency")
	}
	if in.MemoryMiB < 0 || in.MemoryMiB > 1<<20 || in.BuildMemoryMiB < 0 || in.BuildMemoryMiB > 1<<20 || in.CPUPercent < 0 || in.CPUPercent > 6400 {
		return errors.New("invalid resource limits")
	}
	return nil
}

func (c *Control) createNative(w http.ResponseWriter, r *http.Request) {
	var input nativeCreateInput
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	in := input.NativeCreateRequest
	in.SourceDirectory = strings.Trim(strings.TrimSpace(in.SourceDirectory), "/")
	if in.SourceDirectory == "." {
		in.SourceDirectory = ""
	}
	if err := validateNativeCreate(in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	installation := input.GitHubInstallation
	if installation == 0 {
		if cfg := c.githubApp(); cfg != nil && c.cfg.GitHub != nil {
			if id, err := c.cfg.GitHub.RepositoryInstallation(r.Context(), cfg.App, in.Repository); err == nil && cfg.Installations[id] != "" {
				installation = id
			}
		}
	}
	if installation > 0 {
		if _, err := c.tokenFor(r.Context(), Application{Repository: in.Repository, GitHubInstallation: installation}); err != nil {
			agent.Fail(w, 400, errors.New("select a repository from a verified GitHub installation"))
			return
		}
	}
	if in.AutoDeploy && (installation <= 0 || !strings.HasPrefix(c.cfg.PublicURL, "https://")) {
		agent.Fail(w, 400, errors.New("auto-deploy requires a public HTTPS control panel and a repository authorized through the connected GitHub App"))
		return
	}
	a, warning, status, err := c.registerCreatedNative(r, input.ServerID, installation, in)
	if err != nil {
		agent.Fail(w, status, err)
		return
	}
	var deployment *agent.Deployment
	if input.Deploy {
		d, err := c.firstNativeDeploy(r, a)
		if err != nil {
			warning = strings.TrimSpace(warning + " Application created; the first deployment could not start: " + err.Error())
		} else {
			deployment = &d
		}
	}
	a.Secret = ""
	agent.JSON(w, 201, struct {
		Application
		Warning    string            `json:"warning,omitempty"`
		Deployment *agent.Deployment `json:"deployment,omitempty"`
	}{a, warning, deployment})
}

// registerCreatedNative creates the application on the agent and records it.
// The lock is held across the agent call so two requests cannot claim the
// same ID or hostname.
func (c *Control) registerCreatedNative(r *http.Request, serverID string, installation int64, in agent.NativeCreateRequest) (Application, string, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if in.AutoDeploy && (c.data.GitHubApp == nil || c.data.GitHubApp.Installations[installation] == "") {
		return Application{}, "", 409, errors.New("GitHub installation changed; reconnect the repository before enabling auto-deploy")
	}
	if len(c.data.Apps) >= 500 {
		return Application{}, "", 409, errors.New("application limit reached")
	}
	if _, exists := c.data.Apps[in.ID]; exists {
		return Application{}, "", 409, errors.New("application ID already exists")
	}
	if _, exists := c.data.ObservedApps[in.ID]; exists {
		return Application{}, "", 409, errors.New("application ID already exists")
	}
	spec := agent.App{Domain: in.Domain, AdditionalDomains: in.AdditionalDomains}
	for _, other := range c.data.Apps {
		if spec.DomainsOverlap(agent.App{Domain: other.Domain, AdditionalDomains: other.AdditionalDomains}) {
			return Application{}, "", 409, errors.New("domain already registered")
		}
	}
	warning := ""
	if in.Data.Offsite {
		switch {
		case c.data.BackupStorage == nil:
			in.Data.Offsite = false
			warning = "Configure S3 storage to copy backups offsite; local backups are kept."
		case c.agentCall(r, serverID, "PUT", "/v1/backup-storage", backupStorageConfig(*c.data.BackupStorage), nil) != nil:
			in.Data.Offsite = false
			warning = "Storage could not be sent to the agent; backups are kept locally until offsite copies are enabled."
		}
	}
	var created agent.NativeApplication
	if err := c.agentCall(r, serverID, "POST", "/v1/native/apps", in, &created); err != nil {
		return Application{}, "", 502, err
	}
	// Record what the agent reports for the application, so later native
	// actions compare against the same binding.
	native := created
	var status agent.NativeStatus
	if err := c.agentCall(r, serverID, "GET", "/v1/native/apps/"+url.PathEscape(in.ID), nil, &status); err == nil && status.Application.ID == in.ID {
		native = status.Application
	}
	if native.ID != in.ID {
		native = agent.NativeApplication{ID: in.ID, Name: in.ID, Runtime: in.Runtime, Repository: in.Repository, Branch: in.Branch, Domain: in.Domain, AdditionalDomains: in.AdditionalDomains, HealthPath: in.HealthPath}
	}
	runtime := native.Runtime
	if runtime == "" {
		runtime = in.Runtime
	}
	a := Application{ID: in.ID, Native: &native, Runtime: runtime, Repository: in.Repository, Branch: in.Branch, Domain: in.Domain, AdditionalDomains: in.AdditionalDomains, ServerID: serverID, GitHubInstallation: installation, AutoDeploy: in.AutoDeploy, Generation: random()}
	c.data.Apps[a.ID] = a
	if err := c.save(); err != nil {
		delete(c.data.Apps, a.ID)
		return Application{}, "", 500, fmt.Errorf("the server created %s, but the control panel could not record it: %w", a.ID, err)
	}
	return a, warning, 201, nil
}

// firstNativeDeploy starts the first release of a created application.
func (c *Control) firstNativeDeploy(r *http.Request, a Application) (agent.Deployment, error) {
	var d agent.Deployment
	var status agent.NativeStatus
	path := "/v1/native/apps/" + url.PathEscape(a.ID)
	if err := c.agentCall(r, a.ServerID, "GET", path, nil, &status); err != nil {
		return d, err
	}
	if !status.DeploymentSupported && !status.ReleaseManaged {
		return d, errors.New("the server has not enabled deployments for this application")
	}
	issue := c.deploymentCredentials
	if c.credentialsFixture != nil {
		issue = c.credentialsFixture
	}
	credentials, err := issue(r.Context(), a, "")
	if err != nil {
		return d, err
	}
	return d, c.agentCall(r, a.ServerID, "POST", path+"/deploy", credentials, &d)
}
