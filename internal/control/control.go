// Package control is the team-enabled Līdza application control plane.
// Agents are independently deployed; the browser never receives their keys.
package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/platform/state"
	gh "github.com/agim/lidza-deploy/internal/providers/github"
	"github.com/agim/lidza/packs/analytics"
	"github.com/agim/lidza/packs/audit"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/packs/db"
	"github.com/agim/lidza/packs/jobs"
	"github.com/agim/lidza/packs/mail"
	"github.com/agim/lidza/packs/storage"
	"github.com/agim/lidza/pkg/credentials"
	"github.com/agim/lidza/pkg/env"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}
type Config struct {
	PublicURL        string
	User             string
	Password         string
	Key              []byte
	DataDir          string
	Servers          []Server
	GitHub           *gh.Client
	SelfUpdateSecret string
	SelfUpdateServer string
	Connectors       []auth.Connector
}
type Application struct {
	GitHubInstallation int64         `json:"github_installation,omitempty"`
	Previews           PreviewConfig `json:"previews,omitempty"`
	PreviewParent      string        `json:"preview_parent,omitempty"`
	Generation         string        `json:"generation,omitempty"`
	Retiring           bool          `json:"retiring,omitempty"`
	ID                 string        `json:"id"`
	ServerID           string        `json:"server_id"`
	Repository         string        `json:"repository"`
	Branch             string        `json:"branch"`
	Domain             string        `json:"domain"`
	AutoDeploy         bool          `json:"auto_deploy"`
	HookID             int64         `json:"hook_id,omitempty"`
	Secret             string        `json:"secret,omitempty"`
}
type saved struct {
	GitHubApp     *githubAppConfig       `json:"github_app,omitempty"`
	BackupStorage *storage.Config        `json:"backup_storage,omitempty"`
	Incidents     map[string]Incident    `json:"incidents,omitempty"`
	Apps          map[string]Application `json:"apps"`
	Servers       []Server               `json:"servers"`
}
type Control struct {
	githubMu   sync.Mutex
	cfg        Config
	mu         sync.Mutex
	registryMu sync.RWMutex
	registry   []Server
	data       saved
	client     *http.Client
	operatorID string
	teamMu     sync.Mutex
}

func New(cfg Config) (*Control, error) {
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("PUBLIC_URL must be an origin")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
		return nil, errors.New("PUBLIC_URL requires HTTPS except on loopback")
	}
	if (len(cfg.Password) > 0 && len(cfg.Password) < 16) || len(cfg.Key) != 32 || !strings.Contains(cfg.User, "@") {
		return nil, errors.New("set operator user, password (16+ characters), and a 32-byte encryption key")
	}
	if err := validateServers(cfg.Servers); err != nil {
		return nil, err
	}
	c := &Control{cfg: cfg, client: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, data: saved{Apps: map[string]Application{}}}
	var encrypted string
	if err := state.Load(c.path(), &encrypted); err != nil {
		return nil, err
	}
	if encrypted != "" {
		plain, err := credentials.Decrypt(cfg.Key, encrypted)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(plain, &c.data); err != nil {
			return nil, err
		}
	}
	if c.data.Apps == nil {
		c.data.Apps = map[string]Application{}
	}
	if c.data.Servers == nil {
		c.data.Servers = append([]Server{}, cfg.Servers...)
	}
	if err := validateServers(c.data.Servers); err != nil {
		return nil, err
	}
	c.registry = append([]Server{}, c.data.Servers...)
	return c, nil
}
func (c *Control) path() string { return filepath.Join(c.cfg.DataDir, "control.enc.json") }
func (c *Control) save() error {
	plain, err := json.Marshal(c.data)
	if err != nil {
		return err
	}
	sealed, err := credentials.Encrypt(c.cfg.Key, plain)
	if err != nil {
		return err
	}
	return state.Save(c.path(), sealed)
}
func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Protect authenticates the fleet workspace and enforces mutation origin checks.
func (c *Control) Protect(next http.Handler) http.Handler {
	return auth.Require()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if err := fleetRoles.Check(r.Context(), fleetScope, "fleet.read"); err != nil {
			permissionError(w, err)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Origin") != strings.TrimSuffix(c.cfg.PublicURL, "/") {
			http.Error(w, "origin rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
}
func (c *Control) Start(ctx context.Context, s *lidza.Services) error {
	ctx = lidza.WithServices(ctx, s)
	pool := db.From(ctx)
	// Versioned application bootstrap; framework-owned DDL comes from the pinned pack.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(71924161)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, auth.SessionTable+auth.TokenTable+auth.AccountTable+auth.UserTable+auth.IdentityTable+auth.ConnectionTable+auth.MemberTable+audit.Table+jobs.JobTable+jobs.ScheduleTable+mail.OutboxTable+analytics.Tables+consoleTable); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	// On an installation's first audit-enabled boot, the pack starts before
	// application bootstrap creates its table. Apply startup retention now too.
	if _, err = audit.From(ctx).Prune(ctx); err != nil {
		return err
	}
	a := auth.From(ctx)
	profile, err := a.ProfileByEmail(ctx, auth.NormalizeEmail(c.cfg.User))
	if errors.Is(err, pgx.ErrNoRows) {
		if len(c.cfg.Password) < 16 {
			return errors.New("CONTROL_PASSWORD (16+ characters) required for first operator bootstrap")
		}
		profile, err = a.CreateUser(ctx, c.cfg.User, "Operator", c.cfg.Password)
	}
	if err != nil {
		return err
	}
	c.operatorID = profile.Subject
	if err := fleetRoles.Grant(ctx, c.operatorID, fleetScope, "admin"); err != nil {
		return err
	}
	jobs.FromServices(s).Handle("deploy.push", c.auditedJob("deploy.push", c.dispatchPush), jobs.Concurrency(2))
	q := jobs.FromServices(s)
	q.Handle("deploy.preview", c.auditedJob("deploy.preview", c.dispatchPreview), jobs.Concurrency(2))
	q.Handle("deploy.task", c.auditedJob("deploy.task", c.dispatchTask), jobs.Concurrency(4))
	q.Handle("ops.tick", c.operationsTick, jobs.Concurrency(1))
	q.Handle("errors.sync", c.configureCollectors, jobs.Concurrency(1))
	if err := q.Schedule("errors.sync", jobs.Every(time.Minute), nil); err != nil {
		return err
	}
	if _, err := q.Enqueue(ctx, "errors.sync", nil); err != nil {
		return err
	}
	return q.Schedule("ops.tick", jobs.Every(time.Minute), nil)
}
func (c *Control) token(ctx context.Context) (string, error) {
	conn, err := auth.From(ctx).Connection(ctx, c.operatorID, "github")
	if errors.Is(err, auth.ErrNotConnected) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return conn.Token(ctx)
}
func (c *Control) agentCall(r *http.Request, serverID, method, path string, body, out any) error {
	var target *Server
	servers := c.servers()
	for i := range servers {
		if servers[i].ID == serverID {
			target = &servers[i]
			break
		}
	}
	if target == nil {
		return errors.New("unknown server")
	}
	return c.agentRequest(r, *target, method, path, body, out)
}
func (c *Control) agentRequest(r *http.Request, target Server, method, path string, body, out any) error {
	var b []byte
	if body != nil {
		var err error
		b, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), method, target.URL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+target.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return errors.New("agent unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
		return fmt.Errorf("agent returned %d: %s", res.StatusCode, e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
}
func (c *Control) Handler(frontend http.Handler) http.Handler {
	mux := http.NewServeMux()
	private := http.NewServeMux()
	handle := func(pattern string, fn http.HandlerFunc) { private.HandleFunc(pattern, c.protectedRoute(pattern, fn)) }
	for _, pattern := range []string{"GET /api/control/github/app/status", "POST /api/control/github/app/register", "GET /api/control/github/app/manifest-callback", "POST /api/control/github/app/install", "GET /api/control/github/app/install-callback", "DELETE /api/control/github/app"} {
		handle(pattern, c.githubAppRoute)
	}
	handle("GET /api/control/team", c.team)
	handle("POST /api/control/team", c.team)
	handle("DELETE /api/control/team/{subject}", c.team)
	handle("GET /api/control/audit", c.auditLog)
	handle("GET /api/control/servers/{server}/upgrade", c.upgradeServer)
	handle("POST /api/control/servers/{server}/upgrade", c.upgradeServer)
	handle("PUT /api/control/apps/{id}/previews", c.previewConfig)
	handle("GET /api/control/tasks", c.tasks)
	handle("PUT /api/control/apps/{id}/tasks/{task}", c.taskAction)
	handle("POST /api/control/apps/{id}/tasks/{task}/{action}", c.taskAction)
	handle("GET /api/control/apps/{id}/tasks/{task}/{action}", c.taskAction)
	handle("GET /api/control/server-health", c.serverHealth)
	handle("PUT /api/control/apps/{id}/{feature}", c.appFeature)
	handle("POST /api/control/apps/{id}/restore", func(w http.ResponseWriter, r *http.Request) { r.SetPathValue("feature", "restore"); c.appFeature(w, r) })
	handle("GET /api/control/status", c.status)
	handle("GET /api/control/servers", func(w http.ResponseWriter, r *http.Request) {
		out := c.servers()
		for i := range out {
			out[i].Token = ""
		}
		agent.JSON(w, 200, out)
	})
	handle("POST /api/control/servers", c.addServer)
	handle("PUT /api/control/servers/{id}", c.editServer)
	handle("DELETE /api/control/servers/{id}", c.removeServer)
	handle("GET /api/control/databases", c.databases)
	handle("POST /api/control/apps/{id}/database", c.configureDatabase)
	handle("POST /api/control/apps/{id}/database/{action}", c.databaseAction)
	handle("PATCH /api/control/apps/{id}/backups", c.backupPolicy)
	handle("GET /api/control/servers/{server}/databases/{id}/backups/{backup}", c.downloadBackup)
	handle("GET /api/control/infrastructure", c.infrastructure)
	handle("PUT /api/control/storage", c.configureStorage)
	handle("PUT /api/control/mail", c.configureMail)
	handle("POST /api/control/mail/test", c.testMail)
	handle("GET /api/control/apps/{id}/settings", c.settings)
	handle("PATCH /api/control/apps/{id}/settings", c.updateSettings)
	handle("DELETE /api/control/apps/{id}/webhook", c.disableHook)
	handle("GET /api/control/apps", c.apps)
	handle("POST /api/control/apps", c.create)
	handle("DELETE /api/control/apps/{id}", c.retire)
	handle("POST /api/control/apps/{id}/reload", c.reloadApp)
	handle("POST /api/control/servers/{server}/databases/{id}/{action}", c.databaseResourceAction)
	handle("PATCH /api/control/servers/{server}/databases/{id}/backups", c.databaseResourcePolicy)
	handle("POST /api/control/apps/{id}/deploy", c.deploy)
	handle("POST /api/control/apps/{id}/rollback", c.rollback)
	handle("POST /api/control/apps/{id}/webhook", c.hook)
	handle("POST /api/control/apps/{id}/github-app", c.migrateGitHubApp)
	handle("GET /api/control/apps/{id}/logs", c.logs)
	handle("POST /api/control/apps/{id}/owner-claim", c.ownerClaim)
	handle("GET /api/control/apps/{id}/errors", c.appErrors)
	handle("GET /api/control/apps/{id}/security", c.appErrors)
	handle("GET /api/control/apps/{id}/analytics-errors", c.analyticsErrors)
	handle("GET /api/control/deployments", c.deployments)
	handle("GET /api/control/deliveries", c.deliveries)
	handle("GET /api/control/github/repos", c.repos)
	handle("GET /api/control/github/branches", c.branches)
	handle("POST /api/control/github/config", c.configureGitHub)

	mux.HandleFunc("POST /hooks/github/{id}", c.webhook)
	mux.HandleFunc("POST /hooks/github-app", c.githubAppWebhook)
	mux.HandleFunc("POST /hooks/self-update", c.selfUpdate)
	mux.HandleFunc("POST /api/agent/checkout-token", c.checkoutToken)
	mux.HandleFunc("POST /api/agent/errors", c.ingestErrors)
	mux.Handle("/api/control/", c.Protect(private))
	mux.Handle("/", frontend)
	return mux
}
func (c *Control) status(w http.ResponseWriter, r *http.Request) {
	token, err := c.token(r.Context())
	app := c.githubApp()
	appConfigured := app != nil
	appConnected := appConfigured && len(app.Installations) > 0
	agent.JSON(w, 200, map[string]any{"github_public_https": strings.HasPrefix(c.cfg.PublicURL, "https://"), "github_app_configured": appConfigured, "github_app_connected": appConnected, "github_app_slug": func() string {
		if app != nil {
			return app.App.Slug
		}
		return ""
	}(), "github_connected": appConnected || (err == nil && token != ""), "github_configured": appConfigured || githubConfigured(), "server_count": len(c.servers()), "roles": heldRoles(r.Context()), "github_owner": auth.CurrentUser(r.Context()).ID == c.operatorID})
}
func (c *Control) apps(w http.ResponseWriter, r *http.Request) {
	type view struct {
		Application
		Maintenance  agent.Maintenance   `json:"maintenance"`
		Restoring    bool                `json:"restoring,omitempty"`
		DomainStatus *agent.DomainStatus `json:"domain_status,omitempty"`
		Current      *agent.Release      `json:"current,omitempty"`
		AgentError   string              `json:"agent_error,omitempty"`
	}
	c.mu.Lock()
	out := make([]view, 0, len(c.data.Apps))
	for _, a := range c.data.Apps {
		a.Secret = ""
		a.Previews.Env = nil
		out = append(out, view{Application: a})
	}
	c.mu.Unlock()
	for _, result := range c.readFleet(r, "/v1/apps") {
		server := result.Server
		var remote []agent.App
		err := result.Err
		if err == nil {
			err = json.Unmarshal(result.Body, &remote)
		}
		for i := range out {
			if out[i].ServerID != server.ID {
				continue
			}
			if err != nil {
				out[i].AgentError = "agent unavailable"
				continue
			}
			for _, a := range remote {
				if a.ID == out[i].ID {
					out[i].Current = a.Current
					out[i].DomainStatus = a.DomainStatus
					out[i].Maintenance = a.Maintenance
					out[i].Restoring = a.Restoring
					break
				}
			}
		}
	}
	agent.JSON(w, 200, out)
}
func (c *Control) AuthorizeConnect(ctx context.Context, u *auth.User, provider string) error {
	if u.ID != c.operatorID {
		return errors.New("operator access required")
	}
	return nil
}
func (c *Control) create(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Application
		Env      map[string]string      `json:"env"`
		Database *agent.DatabaseRequest `json:"database,omitempty"`
	}
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if input.Database != nil {
		if err := input.Database.Validate(); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		if input.Database.Mode != "none" && input.Database.Mode != "" {
			if _, conflict := input.Env["DATABASE_URL"]; conflict {
				agent.Fail(w, 400, errors.New("database settings supply DATABASE_URL automatically"))
				return
			}
		}
	}
	a := input.Application
	if a.GitHubInstallation == 0 {
		if cfg := c.githubApp(); cfg != nil {
			if id, err := c.cfg.GitHub.RepositoryInstallation(r.Context(), cfg.App, a.Repository); err == nil && cfg.Installations[id] != "" {
				a.GitHubInstallation = id
			}
		}
	}
	if a.GitHubInstallation > 0 {
		if _, err := c.tokenFor(r.Context(), a); err != nil {
			agent.Fail(w, 400, errors.New("select a repository from a verified GitHub installation"))
			return
		}
	}
	a.HookID = 0
	a.Secret = ""
	a.AutoDeploy = false
	a.Retiring = false
	a.Generation = random()
	spec := agent.App{ID: a.ID, Repository: a.Repository, Branch: a.Branch, Domain: a.Domain, Env: input.Env}
	if err := spec.Validate(); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.data.Apps) >= 500 {
		agent.Fail(w, 409, errors.New("application limit reached"))
		return
	}
	if _, exists := c.data.Apps[a.ID]; exists {
		agent.Fail(w, 409, errors.New("application ID already exists"))
		return
	}
	for _, other := range c.data.Apps {
		if other.Domain == a.Domain {
			agent.Fail(w, 409, errors.New("domain already registered"))
			return
		}
	}
	if err := c.agentCall(r, a.ServerID, "PUT", "/v1/apps/"+a.ID, spec, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	c.data.Apps[a.ID] = a
	if err := c.save(); err != nil {
		delete(c.data.Apps, a.ID)
		agent.Fail(w, 500, err)
		return
	}
	warning := ""
	if input.Database != nil && input.Database.Mode != "none" && input.Database.Mode != "" {
		if input.Database.Backup.Offsite {
			if c.data.BackupStorage == nil {
				warning = "Configure S3 storage, then attach the database using Database & backups."
			} else if err := c.agentCall(r, a.ServerID, "PUT", "/v1/backup-storage", c.data.BackupStorage, nil); err != nil {
				warning = "Storage could not be sent to the agent. Attach the database using Database & backups."
			}
		}
		if warning == "" {
			if err := c.agentCall(r, a.ServerID, "POST", "/v1/apps/"+a.ID+"/database", input.Database, nil); err != nil {
				warning = "App created; database setup could not start. Retry in Database & backups."
			}
		}
	}
	agent.JSON(w, 201, struct {
		Application
		Warning string `json:"warning,omitempty"`
	}{a, warning})
}
func (c *Control) app(id string) (Application, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.data.Apps[id]
	return a, ok
}
func (c *Control) deploy(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	credentialRequest, err := c.deploymentCredentials(r.Context(), a, "")
	if err != nil {
		agent.Fail(w, 409, err)
		return
	}
	if err := c.syncStorageIfConfigured(r, a.ServerID); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	var d agent.Deployment
	if err := c.agentCall(r, a.ServerID, "POST", "/v1/apps/"+a.ID+"/deploy", credentialRequest, &d); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 202, d)
}
func (c *Control) rollback(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	if err := c.agentCall(r, a.ServerID, "POST", "/v1/apps/"+a.ID+"/rollback", nil, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "rolled_back"})
}
func (c *Control) logs(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var out any
	if err := c.agentCall(r, a.ServerID, "GET", "/v1/apps/"+a.ID+"/logs", nil, &out); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 200, out)
}
func (c *Control) deployments(w http.ResponseWriter, r *http.Request) {
	type row struct {
		agent.Deployment
		ServerID string `json:"server_id"`
	}
	out := []row{}
	unavailable := []string{}
	for _, result := range c.readFleet(r, "/v1/deployments") {
		s := result.Server
		var rows []agent.Deployment
		err := result.Err
		if err == nil {
			err = json.Unmarshal(result.Body, &rows)
		}
		if err != nil {
			unavailable = append(unavailable, s.ID)
			continue
		}
		for _, d := range rows {
			out = append(out, row{Deployment: d, ServerID: s.ID})
		}
	}
	agent.JSON(w, 200, map[string]any{"deployments": out, "unavailable_servers": unavailable})
}
func (c *Control) repos(w http.ResponseWriter, r *http.Request) {
	if cfg := c.githubApp(); cfg != nil && len(cfg.Installations) > 0 {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		if page > 1000 {
			agent.Fail(w, 400, errors.New("invalid page"))
			return
		}
		all := []gh.Repository{}
		for id := range cfg.Installations {
			rows, err := c.cfg.GitHub.InstallationRepositories(r.Context(), cfg.App, id, page)
			if err != nil {
				agent.Fail(w, 409, errors.New("repository access unavailable; check GitHub installation approval"))
				return
			}
			all = append(all, rows...)
		}
		agent.JSON(w, 200, all)
		return
	}
	token, tokenErr := c.token(r.Context())
	if tokenErr != nil {
		agent.Fail(w, 409, tokenErr)
		return
	}
	if token == "" || c.cfg.GitHub == nil {
		agent.Fail(w, 409, errors.New("connect GitHub first"))
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 1000 {
		http.Error(w, "invalid page", 400)
		return
	}
	repos, err := c.cfg.GitHub.Repositories(r.Context(), token, page)
	if err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 200, repos)
}
func ConfigFromEnv() (Config, error) {
	key, err := credentials.Key(".")
	if err != nil {
		return Config{}, err
	}
	values, err := env.Values(".")
	if err != nil {
		return Config{}, err
	}
	cfg := Config{SelfUpdateSecret: values["LIDZA_SELF_UPDATE_SECRET"], SelfUpdateServer: values["LIDZA_SELF_UPDATE_SERVER"], PublicURL: values["PUBLIC_URL"], User: values["CONTROL_USER"], Password: values["CONTROL_PASSWORD"], Key: key, DataDir: values["CONTROL_DATA_DIR"]}
	if cfg.DataDir == "" {
		cfg.DataDir = "/var/lib/lidza-control"
	}
	if file := values["CONTROL_SERVERS_FILE"]; file != "" {
		if err := state.Load(file, &cfg.Servers); err != nil {
			return cfg, err
		}
	}
	var warnings []string
	cfg.Connectors, warnings = auth.ConnectorsFromEnv(values)
	if len(warnings) > 0 {
		return cfg, fmt.Errorf("GitHub connector configuration: %s", strings.Join(warnings, "; "))
	}
	cfg.GitHub = &gh.Client{}
	return cfg, nil
}

func (c *Control) ownerClaim(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var out agent.OwnerClaim
	if err := c.agentCall(r, a.ServerID, "POST", "/v1/apps/"+a.ID+"/owner-claim", map[string]string{}, &out); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 200, out)
}
