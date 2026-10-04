// Package control is the single-operator Līdza application control plane.
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
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/packs/db"
	"github.com/agim/lidza/packs/jobs"
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
	PublicURL  string
	User       string
	Password   string
	Key        []byte
	DataDir    string
	Servers    []Server
	GitHub     *gh.Client
	Connectors []auth.Connector
}
type Application struct {
	Generation string `json:"generation,omitempty"`
	Retiring   bool   `json:"retiring,omitempty"`
	ID         string `json:"id"`
	ServerID   string `json:"server_id"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Domain     string `json:"domain"`
	AutoDeploy bool   `json:"auto_deploy"`
	HookID     int64  `json:"hook_id,omitempty"`
	Secret     string `json:"secret,omitempty"`
}
type saved struct {
	Apps    map[string]Application `json:"apps"`
	Servers []Server               `json:"servers"`
}
type Control struct {
	cfg        Config
	mu         sync.Mutex
	registryMu sync.RWMutex
	registry   []Server
	data       saved
	client     *http.Client
	operatorID string
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

// Protect uses Līdza sessions and then limits fleet access to the configured operator.
func (c *Control) Protect(next http.Handler) http.Handler {
	return auth.Require()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if auth.CurrentUser(r.Context()).ID != c.operatorID {
			http.Error(w, "operator access required", 403)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Origin") != strings.TrimSuffix(c.cfg.PublicURL, "/") {
			http.Error(w, "origin rejected", 403)
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
	if _, err = tx.Exec(ctx, auth.SessionTable+auth.TokenTable+auth.AccountTable+auth.UserTable+auth.IdentityTable+auth.ConnectionTable+jobs.JobTable+jobs.ScheduleTable); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
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
	jobs.FromServices(s).Handle("deploy.push", c.dispatchPush, jobs.Concurrency(2))
	return nil
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
	private.HandleFunc("GET /api/control/status", c.status)
	private.HandleFunc("GET /api/control/servers", func(w http.ResponseWriter, r *http.Request) {
		out := c.servers()
		for i := range out {
			out[i].Token = ""
		}
		agent.JSON(w, 200, out)
	})
	private.HandleFunc("POST /api/control/servers", c.addServer)
	private.HandleFunc("PUT /api/control/servers/{id}", c.editServer)
	private.HandleFunc("DELETE /api/control/servers/{id}", c.removeServer)
	private.HandleFunc("GET /api/control/apps/{id}/settings", c.settings)
	private.HandleFunc("PATCH /api/control/apps/{id}/settings", c.updateSettings)
	private.HandleFunc("DELETE /api/control/apps/{id}/webhook", c.disableHook)
	private.HandleFunc("GET /api/control/apps", c.apps)
	private.HandleFunc("POST /api/control/apps", c.create)
	private.HandleFunc("DELETE /api/control/apps/{id}", c.retire)
	private.HandleFunc("POST /api/control/apps/{id}/deploy", c.deploy)
	private.HandleFunc("POST /api/control/apps/{id}/rollback", c.rollback)
	private.HandleFunc("POST /api/control/apps/{id}/webhook", c.hook)
	private.HandleFunc("GET /api/control/apps/{id}/logs", c.logs)
	private.HandleFunc("GET /api/control/deployments", c.deployments)
	private.HandleFunc("GET /api/control/deliveries", c.deliveries)
	private.HandleFunc("GET /api/control/github/repos", c.repos)
	private.HandleFunc("POST /api/control/github/config", c.configureGitHub)

	mux.HandleFunc("POST /hooks/github/{id}", c.webhook)
	mux.Handle("/api/control/", c.Protect(private))
	mux.Handle("/", frontend)
	return mux
}
func (c *Control) status(w http.ResponseWriter, r *http.Request) {
	token, err := c.token(r.Context())
	agent.JSON(w, 200, map[string]any{"github_connected": err == nil && token != "", "github_configured": githubConfigured(), "server_count": len(c.servers())})
}
func (c *Control) apps(w http.ResponseWriter, r *http.Request) {
	type view struct {
		Application
		Current    *agent.Release `json:"current,omitempty"`
		AgentError string         `json:"agent_error,omitempty"`
	}
	c.mu.Lock()
	out := make([]view, 0, len(c.data.Apps))
	for _, a := range c.data.Apps {
		a.Secret = ""
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
		Env map[string]string `json:"env"`
	}
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	a := input.Application
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
	agent.JSON(w, 201, a)
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
	token, err := c.token(r.Context())
	if err != nil {
		agent.Fail(w, 409, err)
		return
	}
	var d agent.Deployment
	if err := c.agentCall(r, a.ServerID, "POST", "/v1/apps/"+a.ID+"/deploy", agent.DeployRequest{Token: token}, &d); err != nil {
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
	cfg := Config{PublicURL: values["PUBLIC_URL"], User: values["CONTROL_USER"], Password: values["CONTROL_PASSWORD"], Key: key, DataDir: values["CONTROL_DATA_DIR"]}
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
