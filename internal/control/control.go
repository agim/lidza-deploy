// Package control is the team-enabled Līdza application control plane.
// Agents are independently deployed; the browser never receives their keys.
package control

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/buildinfo"
	"github.com/agim/lidza-deploy/internal/platform/state"
	gh "github.com/agim/lidza-deploy/internal/providers/github"
	"github.com/agim/lidza-deploy/internal/retention"
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
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// Standby is a panel installed by a control panel move that has not
	// taken over yet: it runs no ticks, jobs or updates (cmd/web serves it
	// a standby page and never boots it).
	Standby bool
}
type Application struct {
	Astro             *agent.AstroConfig       `json:"astro,omitempty"`
	Build             agent.BuildConfig        `json:"build,omitempty,omitzero"`
	Native            *agent.NativeApplication `json:"native,omitempty"`
	Runtime           string                   `json:"runtime,omitempty"`
	Rails             *agent.RailsConfig       `json:"rails,omitempty"`
	RuntimeProfile    string                   `json:"runtime_profile,omitempty"`
	AdditionalDomains []string                 `json:"additional_domains,omitempty"`
	// RedirectDomains mirror the hosting agent's redirect domains of the
	// application (redirects.go).
	RedirectDomains    []agent.RedirectDomain `json:"redirect_domains,omitempty"`
	CustomerHosts      bool                   `json:"customer_hosts"`
	GitHubInstallation int64                  `json:"github_installation,omitempty"`
	Previews           PreviewConfig          `json:"previews,omitempty"`
	PreviewParent      string                 `json:"preview_parent,omitempty"`
	Generation         string                 `json:"generation,omitempty"`
	Retiring           bool                   `json:"retiring,omitempty"`
	ID                 string                 `json:"id"`
	ServerID           string                 `json:"server_id"`
	Repository         string                 `json:"repository"`
	Branch             string                 `json:"branch"`
	Domain             string                 `json:"domain"`
	AutoDeploy         bool                   `json:"auto_deploy"`
	HookID             int64                  `json:"hook_id,omitempty"`
	Secret             string                 `json:"secret,omitempty"`
}
type saved struct {
	ObservedApps              map[string]ObservedApplication `json:"observed_apps,omitempty"`
	ApplicationDefaults       map[string]string              `json:"application_defaults"`
	RuntimeDefaults           map[string]map[string]string   `json:"runtime_defaults,omitempty"`
	Updates                   UpdateSettings                 `json:"updates"`
	GitHubApp                 *githubAppConfig               `json:"github_app,omitempty"`
	BackupStorageLegacyPrefix string                         `json:"backup_storage_legacy_prefix,omitempty"`
	BackupStorage             *storage.Config                `json:"backup_storage,omitempty"`
	Incidents                 map[string]Incident            `json:"incidents,omitempty"`
	Moves                     map[string]MoveJob             `json:"moves,omitempty"`
	Apps                      map[string]Application         `json:"apps"`
	Servers                   []Server                       `json:"servers"`
	// AppArchives are the deleted applications the panel can restore, by
	// the agent's archive id (archive.go).
	AppArchives map[string]ControlArchive `json:"app_archives,omitempty"`

	// BackupRetention is the workspace's off-site retention; nil until an
	// operator saves it, and until then nothing is cleaned up on its own.
	// BackupOrphans records when each database folder without an owning
	// server was first noticed (backup_retention.go).
	BackupRetention *retention.Settings  `json:"backup_retention,omitempty"`
	BackupOrphans   map[string]time.Time `json:"backup_orphans,omitempty"`

	// PanelMove is the move of this control panel to another server and
	// PanelArrival what a moved-in panel remembers (panel_move.go).
	PanelMove    *PanelMove    `json:"panel_move,omitempty"`
	PanelArrival *PanelArrival `json:"panel_arrival,omitempty"`
}
type Control struct {
	// stateWriter writes the sealed state file outside mu (state_writer.go).
	stateWriter stateWriter
	// credentialsFixture replaces deploymentCredentials in unit tests that run
	// without the framework's auth services; nil in production.
	credentialsFixture func(context.Context, Application, string) (agent.DeployRequest, error)
	nativeSyncMu       sync.Mutex
	nativeSync         map[string]NativeInventoryStatus
	releaseCheck       func(*http.Request) (string, error)
	updateMu           sync.Mutex
	githubMu           sync.Mutex
	cfg                Config
	mu                 sync.Mutex
	registryMu         sync.RWMutex
	registry           []Server
	data               saved
	client             *http.Client
	operatorID         string
	teamMu             sync.Mutex
	// Application moves (move*.go). moveRunning is guarded by mu; the
	// resolver, HTTPS probe and poll interval are replaced in tests.
	moveRunning      map[string]bool
	moveBase         context.Context
	moveResolver     moveResolver
	moveProbe        func(ctx context.Context, address, host string) error
	movePollInterval time.Duration
	// moveLive is the running step's live progress by move, guarded by mu
	// (move_progress.go); moveHeartbeat replaces the heartbeat interval in
	// tests.
	moveLive      map[string]*moveLiveStep
	moveHeartbeat time.Duration
	// moveRetryDelays, moveStallWarn and moveStallCancel replace the retry
	// backoff and stall limits in tests (move_retry.go, move_progress.go).
	moveRetryDelays []time.Duration
	moveStallWarn   time.Duration
	moveStallCancel time.Duration
	// moveProbes caches disk probes of servers for estimates.
	moveProbes map[string]moveProbeResult
	// moveTunnels are the running replication tunnels through the panel by
	// move and database (move_tunnel.go), guarded by moveTunnelMu.
	moveTunnelMu sync.Mutex
	moveTunnels  map[string]*moveTunnel
	// moveTunnelBackoff and moveTunnelDialLimit replace the tunnel's
	// first retry delay and stream handshake limit in tests.
	moveTunnelBackoff   time.Duration
	moveTunnelDialLimit time.Duration
	// moveWork tracks the move runners and tunnels the panel started;
	// stopMoveWork cancels and waits for them (move_work.go).
	moveWork moveWork
	// activityCache is the fleet's deployment read for the activity
	// drawer (activity.go), reused for activityCacheFor.
	activityMu    sync.Mutex
	activityCache *activityFleet
	// Control panel move (panel_move.go): panelRunning is guarded by mu,
	// panelJobs counts running queue jobs for the cutover; the rest are
	// replaced in tests.
	panelRunning        bool
	panelDNSMu          sync.Mutex // one DNS check at a time
	panelJobs           atomic.Int64
	panelMachineID      func() string
	panelPollInterval   time.Duration
	panelRetryDelays    []time.Duration
	panelBundleTransfer func(context.Context, Server, UpdateBundle, string) error
	panelDatabase       func(context.Context) (bool, int64, error)
	panelDumpFixture    func(context.Context, io.Writer) error
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
	liveControls.Store(c, struct{}{})
	return c, nil
}
func (c *Control) path() string { return filepath.Join(c.cfg.DataDir, "control.enc.json") }

// saveState writes the sealed state file. Unit tests replace it with an
// atomic write that skips fsync (control_state_test.go): durability is not
// what they test, and a shared disk's fsync stalls of seconds under load
// held c.mu long enough to time out unrelated waits.
var saveState = state.Save

func (c *Control) save() error {
	plain, err := json.Marshal(c.data)
	if err != nil {
		return err
	}
	sealed, err := credentials.Encrypt(c.cfg.Key, plain)
	if err != nil {
		return err
	}
	if !stateWritesAsync {
		return saveState(c.path(), sealed)
	}
	c.stateWriter.submit(c.path(), sealed)
	return nil
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
	if c.cfg.Standby {
		// A standby panel never reaches here (cmd/web serves it a page);
		// should one boot anyway, it touches nothing and schedules nothing.
		return nil
	}
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
	if _, err = tx.Exec(ctx, auth.SessionTable+auth.TokenTable+auth.AccountTable+auth.UserTable+auth.IdentityTable+auth.ConnectionTable+auth.MemberTable+auth.FactorTable+auth.PreferenceTable+audit.Table+jobs.JobTable+jobs.ScheduleTable+mail.OutboxTable+analytics.Tables+consoleTable); err != nil {
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
	q := jobs.FromServices(s)
	q.Handle("deploy.push", c.gatedJob(false, c.auditedJob("deploy.push", c.dispatchPush)), jobs.Concurrency(2))
	q.Handle("native.inventory", c.gatedJob(true, func(ctx context.Context, _ json.RawMessage) error { c.syncNativeInventory(ctx); return nil }), jobs.Concurrency(1))
	if err := q.Schedule("native.inventory", jobs.Every(time.Minute), nil); err != nil {
		return err
	}
	if _, err := q.Enqueue(ctx, "native.inventory", nil); err != nil {
		return err
	}
	q.Handle("deploy.preview", c.gatedJob(false, c.auditedJob("deploy.preview", c.dispatchPreview)), jobs.Concurrency(2))
	q.Handle("deploy.task", c.gatedJob(false, c.auditedJob("deploy.task", c.dispatchTask)), jobs.Concurrency(4))
	q.Handle("ops.tick", c.gatedJob(true, c.operationsTick), jobs.Concurrency(1))
	q.Handle("errors.sync", c.gatedJob(true, c.configureCollectors), jobs.Concurrency(1))
	if err := q.Schedule("errors.sync", jobs.Every(time.Minute), nil); err != nil {
		return err
	}
	if _, err := q.Enqueue(ctx, "errors.sync", nil); err != nil {
		return err
	}
	if err := q.Schedule("ops.tick", jobs.Every(time.Minute), nil); err != nil {
		return err
	}
	c.moveBase = audit.System(context.WithoutCancel(ctx), "move")
	q.Handle("move.tick", c.gatedJob(true, func(ctx context.Context, _ json.RawMessage) error { c.moveTick(ctx); return nil }), jobs.Concurrency(1))
	if err := q.Schedule("move.tick", jobs.Every(time.Minute), nil); err != nil {
		return err
	}
	// Moves that replicate through the panel get their tunnels back at once
	// after a restart, not only at the first tick.
	c.moveTunnelSync()
	// A panel restarted during its own move resumes it.
	c.panelMoveResume()
	q.Handle("backups.retention", c.gatedJob(true, func(ctx context.Context, _ json.RawMessage) error { return c.retentionTick(ctx) }), jobs.Concurrency(1))
	if err := q.Schedule("backups.retention", jobs.Every(6*time.Hour), nil); err != nil {
		return err
	}
	q.Handle("updates.tick", c.gatedJob(true, func(ctx context.Context, _ json.RawMessage) error { return c.updateTick(ctx) }), jobs.Concurrency(1))
	return q.Schedule("updates.tick", jobs.Every(time.Minute), nil)
}
func (c *Control) token(ctx context.Context) (string, error) {
	conn, err := auth.From(ctx).Connection(ctx, c.operatorID, "github")
	if errors.Is(err, auth.ErrNotConnected) || githubConnectorMissing(err) {
		// No personal grant (or no OAuth connector at all, as on panels
		// that use only the GitHub App): callers fall back to the App.
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

type agentHTTPError struct {
	status  int
	message string
}

func (e *agentHTTPError) Error() string {
	return fmt.Sprintf("agent returned %d: %s", e.status, e.message)
}

// failAgent answers a request whose agent call failed. A request the agent
// refused (invalid, conflicting, too large, rate limited) keeps the agent's
// status and message, so the panel shows why, for example "an update is
// already running". Everything else is a bad gateway: an agent 401/403 is
// the control plane's own credential problem, never the browser's session,
// and an agent 404 means the agent lacks the route or resource.
func failAgent(w http.ResponseWriter, err error) {
	var refused *agentHTTPError
	if errors.As(err, &refused) && agentRefusal[refused.status] {
		message := refused.message
		if message == "" {
			message = http.StatusText(refused.status)
		}
		agent.Fail(w, refused.status, errors.New(message))
		return
	}
	agent.Fail(w, http.StatusBadGateway, err)
}

var agentRefusal = map[int]bool{http.StatusBadRequest: true, http.StatusConflict: true, http.StatusRequestEntityTooLarge: true,
	http.StatusUnprocessableEntity: true, http.StatusLocked: true, http.StatusTooManyRequests: true}

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
	client := c.client
	if timeout, ok := r.Context().Value(agentTimeoutKey{}).(time.Duration); ok && timeout > client.Timeout {
		copy := *client
		copy.Timeout = timeout
		client = &copy
	}
	if path == "/v1/console" {
		copy := *client
		copy.Timeout = agent.ConsoleResponseTimeout
		client = &copy
	}
	if strings.HasSuffix(path, "/state") {
		copy := *client
		copy.Timeout = 110 * time.Second
		client = &copy
	}
	res, err := client.Do(req)
	if err != nil {
		if path == "/v1/console" {
			var timeout net.Error
			if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
				return errors.New("console response timed out; command outcome is unknown. Check the server before running the command again")
			}
			return errors.New("hosting agent connection interrupted; command outcome is unknown. Check agent connectivity before running the command again")
		}
		// The cause (a refused connection, a handshake or a deadline)
		// follows the generic text so job logs say what failed.
		return fmt.Errorf("agent unavailable: %s", moveBounded(err.Error(), 200))
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
		return &agentHTTPError{status: res.StatusCode, message: e.Error}
	}
	if out == nil {
		return nil
	}
	maxResponse := int64(4 << 20)
	if path == "/v1/console" {
		maxResponse = agent.ConsoleMaxResponse
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return errors.New("agent response could not be read")
	}
	if int64(len(data)) > maxResponse {
		return errors.New("agent response exceeds the size limit; upgrade the hosting agent for lightweight deployment history")
	}
	return json.Unmarshal(data, out)
}
func (c *Control) Handler(frontend http.Handler) http.Handler {
	mux := http.NewServeMux()
	private := http.NewServeMux()
	c.routes(func(pattern string, fn http.HandlerFunc) {
		private.HandleFunc(pattern, c.protectedRoute(pattern, c.panelReadOnlyGuard(pattern, c.moveLock(pattern, c.nativeContainerGuard(pattern, fn)))))
	})
	// While the panel moves, webhooks, checkout tokens and agent error
	// reports are refused too: what this panel records then is not copied.
	mux.HandleFunc("POST /hooks/github/{id}", c.panelReadOnlyGuard("POST /hooks/github/{id}", c.webhook))
	mux.HandleFunc("POST /hooks/github-app", c.panelReadOnlyGuard("POST /hooks/github-app", c.githubAppWebhook))
	mux.HandleFunc("POST /api/agent/checkout-token", c.panelReadOnlyGuard("POST /api/agent/checkout-token", c.checkoutToken))
	mux.HandleFunc("POST /api/agent/errors", c.panelReadOnlyGuard("POST /api/agent/errors", c.ingestErrors))
	mux.Handle("/api/control/", c.Protect(private))
	mux.Handle("/", frontend)
	return mux
}

// routes registers every panel API route through handle; Handler wraps each
// in authorization, the move lock and the native-runtime guard.
func (c *Control) routes(handle func(string, http.HandlerFunc)) {
	for _, pattern := range []string{"GET /api/control/github/app/status", "POST /api/control/github/app/register", "GET /api/control/github/app/manifest-callback", "POST /api/control/github/app/install", "GET /api/control/github/app/install-callback", "DELETE /api/control/github/app"} {
		handle(pattern, c.githubAppRoute)
	}
	handle("GET /api/control/servers/{server}/native/discovery", c.nativeDiscovery)
	handle("GET /api/control/native-inventory", c.nativeInventoryStatus)
	handle("POST /api/control/servers/{server}/native/apps", c.registerNative)
	handle("GET /api/control/native-apps/{id}", c.nativeAppRoute)
	handle("GET /api/control/native-apps/{id}/{action}", c.nativeAppRoute)
	handle("POST /api/control/native-apps/{id}/services/{unit}/{action}", c.nativeAppRoute)
	handle("POST /api/control/observed-apps", c.importObservedApplications)
	handle("GET /api/control/application-defaults", c.applicationDefaults)
	handle("GET /api/control/apps/{id}/migration-plan", c.migrationPlan)
	c.moveRoutes(handle)
	c.panelMoveRoutes(handle)
	handle("PUT /api/control/application-defaults", c.applicationDefaults)
	handle("POST /api/control/apps/{id}/apply-defaults", c.applyDefaults)
	handle("GET /api/control/team", c.team)
	handle("POST /api/control/team", c.team)
	handle("DELETE /api/control/team/{subject}", c.team)
	handle("GET /api/control/audit", c.auditLog)
	for _, pattern := range []string{"GET /api/control/servers/{server}/ssh-access", "POST /api/control/servers/{server}/ssh-access/keys", "DELETE /api/control/servers/{server}/ssh-access/keys/{key}", "PATCH /api/control/servers/{server}/ssh-access"} {
		handle(pattern, c.sshAccess)
	}
	handle("GET /api/control/servers/{server}/upgrade", c.upgradeServer)
	handle("POST /api/control/servers/{server}/upgrade", c.upgradeServer)
	for _, pattern := range []string{"GET /api/control/servers/{server}/maintenance", "PATCH /api/control/servers/{server}/maintenance", "POST /api/control/servers/{server}/maintenance/run"} {
		handle(pattern, c.serverMaintenance)
	}
	c.hostAdminRoutes(handle)
	handle("POST /api/control/servers/{server}/host/ingress/error-pages", c.adoptErrorPages)
	handle("GET /api/control/servers/{server}/containers", c.serverContainers)
	handle("POST /api/control/servers/{server}/containers/restart", c.serverContainers)
	handle("PUT /api/control/apps/{id}/previews", c.previewConfig)
	handle("GET /api/control/tasks", c.tasks)
	handle("PUT /api/control/apps/{id}/tasks/{task}", c.taskAction)
	handle("POST /api/control/apps/{id}/tasks/{task}/{action}", c.taskAction)
	handle("GET /api/control/apps/{id}/tasks/{task}/{action}", c.taskAction)
	handle("GET /api/control/server-health", c.serverHealth)
	handle("GET /api/control/updates", c.updates)
	handle("POST /api/control/updates/{action}", c.updates)
	handle("PATCH /api/control/updates", c.updates)
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
	handle("GET /api/control/servers/{server}/console", c.consoleStatus)
	handle("POST /api/control/servers/{server}/console", c.runConsole)
	handle("POST /api/control/servers/{server}/console/jobs", c.startConsoleJob)
	handle("POST /api/control/servers/{server}/console/jobs/{job}/cancel", c.cancelConsoleJob)
	handle("GET /api/control/servers/{server}/console/jobs/{job}", c.consoleJobOutput)
	handle("GET /api/control/apps/{id}/cache", c.cacheSettings)
	handle("POST /api/control/apps/{id}/cache", c.cacheSettings)
	handle("GET /api/control/databases", c.databases)
	handle("GET /api/control/servers/{server}/retained-resources", c.serverRetained)
	handle("DELETE /api/control/servers/{server}/retained-resources/{id}", c.removeRetained)
	handle("POST /api/control/apps/{id}/database", c.configureDatabase)
	handle("POST /api/control/apps/{id}/database/{action}", c.databaseAction)
	handle("PATCH /api/control/apps/{id}/backups", c.backupPolicy)
	handle("GET /api/control/servers/{server}/databases/{id}/backups/{backup}", c.downloadBackup)
	handle("GET /api/control/infrastructure", c.infrastructure)
	handle("PUT /api/control/storage", c.configureStorage)
	handle("GET /api/control/storage/backups", c.remoteBackups)
	handle("GET /api/control/storage/backups/download", c.remoteBackupDownload)
	c.backupRetentionRoutes(handle)
	handle("POST /api/control/servers/{server}/databases/{id}/backups/{backup}/upload", c.retryBackupUpload)
	handle("POST /api/control/servers/{server}/databases/{id}/backups/{backup}/restore", c.restoreBackupInPlace)
	handle("PUT /api/control/mail", c.configureMail)
	handle("POST /api/control/mail/test", c.testMail)
	handle("GET /api/control/apps/{id}/settings", c.settings)
	handle("POST /api/control/apps/{id}/master-key", c.generateMasterKey)
	handle("PATCH /api/control/apps/{id}/settings", c.updateSettings)
	handle("GET /api/control/apps/{id}/redirects", c.redirects)
	handle("PUT /api/control/apps/{id}/redirects", c.updateRedirects)
	handle("POST /api/control/apps/{id}/environment/{key}/reveal", c.revealCredential)
	handle("DELETE /api/control/apps/{id}/webhook", c.disableHook)
	handle("GET /api/control/apps", c.apps)
	handle("GET /api/control/apps/{id}/processes", c.applicationProcesses)
	handle("POST /api/control/apps", c.create)
	handle("DELETE /api/control/apps/{id}", c.deleteApp)
	for _, action := range agentLifecycleActions {
		handle("POST /api/control/apps/{id}/"+action, c.appLifecycle(action))
	}
	handle("GET /api/control/archives", c.archives)
	handle("POST /api/control/archives/{id}/restore", c.restoreArchive)
	handle("DELETE /api/control/archives/{id}", c.purgeArchive)
	handle("POST /api/control/apps/{id}/reload", c.reloadApp)
	handle("POST /api/control/servers/{server}/databases/{id}/{action}", c.databaseResourceAction)
	handle("PATCH /api/control/servers/{server}/databases/{id}/backups", c.databaseResourcePolicy)
	handle("PATCH /api/control/servers/{server}/databases/{id}/backups/before-deploy", c.databaseBeforeDeploy)
	handle("GET /api/control/servers/{server}/databases/{id}/tables", c.databaseTables)
	handle("PATCH /api/control/servers/{server}/databases/{id}/backups/excluded-tables", c.databaseExcludedTables)
	handle("POST /api/control/apps/{id}/deploy", c.deploy)
	handle("POST /api/control/apps/{id}/rollback", c.rollback)
	handle("POST /api/control/apps/{id}/webhook", c.hook)
	handle("POST /api/control/apps/{id}/github-app", c.migrateGitHubApp)
	handle("GET /api/control/apps/{id}/logs", c.logs)
	handle("GET /api/control/apps/{id}/ports", c.appPorts)
	handle("POST /api/control/apps/{id}/owner-claim", c.ownerClaim)
	handle("GET /api/control/apps/{id}/errors", c.appErrors)
	handle("GET /api/control/apps/{id}/security", c.appErrors)
	handle("GET /api/control/apps/{id}/analytics-errors", c.analyticsErrors)
	handle("GET /api/control/activity", c.activity)
	handle("GET /api/control/deployments", c.deployments)
	handle("GET /api/control/deployments/{id}", c.deployment)
	handle("GET /api/control/deliveries", c.deliveries)
	handle("GET /api/control/github/repos", c.repos)
	handle("GET /api/control/github/branches", c.branches)
	handle("GET /api/control/repos/detect", c.detectRepository)
	handle("POST /api/control/native-apps", c.createNative)
	handle("POST /api/control/github/config", c.configureGitHub)
}
func (c *Control) status(w http.ResponseWriter, r *http.Request) {
	token, err := c.token(r.Context())
	app := c.githubApp()
	appConfigured := app != nil
	appConnected := appConfigured && len(app.Installations) > 0
	agent.JSON(w, 200, map[string]any{"native_inventory": c.nativeInventoryStates(), "control_version": buildinfo.Version, "github_public_https": strings.HasPrefix(c.cfg.PublicURL, "https://"), "github_app_configured": appConfigured, "github_app_connected": appConnected, "github_app_slug": func() string {
		if app != nil {
			return app.App.Slug
		}
		return ""
	}(), "github_connected": appConnected || (err == nil && token != ""), "github_configured": appConfigured || githubConfigured(), "server_count": len(c.servers()), "roles": heldRoles(r.Context()), "github_owner": auth.CurrentUser(r.Context()).ID == c.operatorID})
}
func (c *Control) apps(w http.ResponseWriter, r *http.Request) {
	type view struct {
		Management *agent.ManagementCapabilities `json:"management_capabilities,omitempty"`
		Observed   *ObservedApplication          `json:"observed,omitempty"`
		Application
		Stopped        bool                 `json:"stopped"`
		Maintenance    agent.Maintenance    `json:"maintenance"`
		Restoring      bool                 `json:"restoring,omitempty"`
		DomainStatuses []agent.DomainStatus `json:"domain_statuses,omitempty"`
		DomainStatus   *agent.DomainStatus  `json:"domain_status,omitempty"`
		Current        *agent.Release       `json:"current,omitempty"`
		AgentError     string               `json:"agent_error,omitempty"`
		// Warnings are the current release's conditions to fix (mail ports
		// without a certificate, for example).
		Warnings []string `json:"warnings,omitempty"`
		// Move is the move that holds the application; its settings are
		// locked until the move finishes or is aborted (move_lock.go).
		Move *moveSummary `json:"move,omitempty"`
	}
	moves := c.activeMoveSummaries()
	c.mu.Lock()
	out := make([]view, 0, len(c.data.Apps))
	for _, a := range c.data.Apps {
		a.Secret = ""
		a.Previews.Env = nil
		v := view{Application: a}
		if move, held := moves[a.ID]; held {
			v.Move = &move
		}
		out = append(out, v)
	}
	for id, observation := range c.data.ObservedApps {
		copy := observation
		out = append(out, view{Application: Application{ID: id, ServerID: "observed:" + copy.Source, Runtime: copy.Runtime, Repository: copy.Name, Domain: copy.Domain}, Observed: &copy})
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
					out[i].CustomerHosts = a.CustomerHosts
					out[i].Stopped = a.Stopped
					out[i].Current = a.Current
					out[i].DomainStatus = a.DomainStatus
					out[i].DomainStatuses = a.DomainStatuses
					out[i].AdditionalDomains = a.AdditionalDomains
					out[i].RedirectDomains = a.RedirectDomains
					out[i].RuntimeProfile = a.RuntimeProfile
					out[i].Native = a.Native
					out[i].Management = a.Management
					out[i].Maintenance = a.Maintenance
					out[i].Restoring = a.Restoring
					out[i].Warnings = a.Warnings
					break
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b view) int { return cmp.Compare(a.ID, b.ID) })
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
		EnvKinds map[string]string      `json:"env_kinds,omitempty"`
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
	if a.Runtime == "go" {
		a.Runtime = ""
	}
	autoDeploy := a.AutoDeploy
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
	if autoDeploy && (a.GitHubInstallation <= 0 || !strings.HasPrefix(c.cfg.PublicURL, "https://")) {
		agent.Fail(w, 400, errors.New("auto-deploy requires a public HTTPS control panel and a repository authorized through the connected GitHub App"))
		return
	}
	a.HookID = 0
	a.Secret = ""
	a.AutoDeploy = autoDeploy
	a.Retiring = false
	a.Generation = random()
	spec := agent.App{Astro: a.Astro, Build: a.Build, Runtime: a.Runtime, Rails: a.Rails, RuntimeProfile: a.RuntimeProfile, ID: a.ID, Repository: a.Repository, Branch: a.Branch, Domain: a.Domain, AdditionalDomains: a.AdditionalDomains, CustomerHosts: a.CustomerHosts, Env: input.Env, EnvKinds: input.EnvKinds}
	if err := c.verifyApplicationRuntime(r, a.ServerID, spec); err != nil {
		agent.Fail(w, 409, err)
		return
	}
	if err := spec.Validate(); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if autoDeploy && (c.data.GitHubApp == nil || c.data.GitHubApp.Installations[a.GitHubInstallation] == "") {
		agent.Fail(w, 409, errors.New("GitHub installation changed; reconnect the repository before enabling auto-deploy"))
		return
	}
	if len(c.data.Apps) >= 500 {
		agent.Fail(w, 409, errors.New("application limit reached"))
		return
	}
	if _, exists := c.data.Apps[a.ID]; exists {
		agent.Fail(w, 409, errors.New("application ID already exists"))
		return
	}
	for _, other := range c.data.Apps {
		if spec.DomainsOverlap(agent.App{Domain: other.Domain, AdditionalDomains: other.AdditionalDomains}) {
			agent.Fail(w, 409, errors.New("domain already registered"))
			return
		}
	}
	if owner := redirectedHostOwner(c.data.Apps, spec.Hostnames()); owner != "" {
		agent.Fail(w, 409, errors.New("domain already registered as a redirect domain of "+owner))
		return
	}
	if err := c.agentCall(r, a.ServerID, "PUT", "/v1/apps/"+a.ID, spec, nil); err != nil {
		failAgent(w, err)
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
			} else if err := c.agentCall(r, a.ServerID, "PUT", "/v1/backup-storage", backupStorageConfig(*c.data.BackupStorage), nil); err != nil {
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
	d, err := c.startDeployment(r, a.ID)
	if err != nil {
		var call deployAgentError
		if errors.As(err, &call) {
			failAgent(w, call.error)
			return
		}
		agent.Fail(w, 409, err)
		return
	}
	agent.JSON(w, 202, d)
}

// deployAgentError is a failed agent call of a deployment, answered with
// the agent's status (failAgent).
type deployAgentError struct{ error }

func (e deployAgentError) Unwrap() error { return e.error }

// startDeployment deploys the application's branch.
func (c *Control) startDeployment(r *http.Request, id string) (agent.Deployment, error) {
	a, ok := c.app(id)
	if !ok || a.Retiring {
		return agent.Deployment{}, errors.New("unknown application")
	}
	path := "/v1/apps/" + a.ID + "/deploy"
	if a.Native != nil {
		var status agent.NativeStatus
		if err := c.agentCall(r, a.ServerID, "GET", "/v1/native/apps/"+a.ID, nil, &status); err != nil {
			return agent.Deployment{}, err
		}
		if !status.DeploymentSupported {
			return agent.Deployment{}, errors.New("native deployment ownership requires the reviewed host configuration")
		}
		path = "/v1/native/apps/" + a.ID + "/deploy"
	}
	credentials := c.deploymentCredentials
	if c.credentialsFixture != nil {
		credentials = c.credentialsFixture
	}
	credentialRequest, err := credentials(r.Context(), a, "")
	if err != nil {
		return agent.Deployment{}, err
	}
	if a.Native == nil {
		if err := c.syncStorageIfConfigured(r, a.ServerID); err != nil {
			return agent.Deployment{}, deployAgentError{err}
		}
	}
	var d agent.Deployment
	if err := c.agentCall(r, a.ServerID, "POST", path, credentialRequest, &d); err != nil {
		return agent.Deployment{}, deployAgentError{err}
	}
	return d, nil
}
func (c *Control) rollback(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	path := "/v1/apps/" + a.ID + "/rollback"
	if a.Native != nil {
		path = "/v1/native/apps/" + a.ID + "/rollback"
	}
	if err := c.agentCall(r, a.ServerID, "POST", path, nil, nil); err != nil {
		failAgent(w, err)
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
	path := "/v1/apps/" + a.ID + "/logs"
	if a.Native != nil {
		c.nativeRuntimeLogs(w, r, a)
		return
	}
	if err := c.agentCall(r, a.ServerID, "GET", path, nil, &out); err != nil {
		failAgent(w, err)
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
	serverErrors := map[string]string{}
	for _, result := range c.readFleet(r, "/v1/deployments?logs=0") {
		s := result.Server
		var rows []agent.Deployment
		err := result.Err
		if err == nil {
			err = json.Unmarshal(result.Body, &rows)
		}
		if err != nil {
			unavailable = append(unavailable, s.ID)
			serverErrors[s.ID] = err.Error()
			continue
		}
		for _, d := range rows {
			out = append(out, row{Deployment: d, ServerID: s.ID})
		}
	}
	agent.JSON(w, 200, map[string]any{"deployments": out, "unavailable_servers": unavailable, "server_errors": serverErrors})
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
		failAgent(w, err)
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

func (c *Control) ownerClaim(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var out agent.OwnerClaim
	if err := c.agentCall(r, a.ServerID, "POST", "/v1/apps/"+a.ID+"/owner-claim", map[string]string{}, &out); err != nil {
		failAgent(w, err)
		return
	}
	agent.JSON(w, 200, out)
}

// githubConnectorMissing reports the framework's error for a panel that has
// no GitHub OAuth connector configured. The framework exposes no sentinel for
// it, so the stable message prefix is matched.
func githubConnectorMissing(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), `auth: no connector "github"`)
}
