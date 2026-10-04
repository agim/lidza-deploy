// Package onboarding owns the deployment product's first-run workflow. Līdza
// remains responsible for application boot, auth and encrypted credentials.
package onboarding

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/control"
	"github.com/agim/lidza-deploy/internal/platform/state"
	"github.com/agim/lidza/pkg/credentials"
	"github.com/jackc/pgx/v5"
)

type Input struct {
	PublicURL    string          `json:"public_url"`
	Email        string          `json:"email"`
	Password     string          `json:"password"`
	DatabaseMode string          `json:"database_mode"`
	DatabaseURL  string          `json:"database_url"`
	GitHubID     string          `json:"github_id"`
	GitHubSecret string          `json:"github_secret"`
	Network      Network         `json:"network"`
	Agent        *control.Server `json:"agent,omitempty"`
}
type Options struct {
	Dir      string
	Origin   string
	Context  context.Context
	Boot     func(context.Context) (*lidza.Booted, error)
	Frontend http.Handler
	// Test seams are supplied only by package tests, never by HTTP requests.
	database func(context.Context, Input) (string, error)
	network  func(context.Context, Input) error
}
type Setup struct {
	opts     Options
	mu       sync.RWMutex
	apply    sync.Mutex
	token    string
	active   *lidza.Booted
	complete bool
}

func random() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func New(opts Options) (*Setup, error) {
	if opts.Origin != "" {
		u, err := url.Parse(opts.Origin)
		if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))) {
			return nil, errors.New("setup origin must be HTTPS or local HTTP")
		}
	}

	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if err := os.MkdirAll(opts.Dir, 0700); err != nil {
		return nil, err
	}
	s := &Setup{opts: opts}
	if err := state.Load(filepath.Join(opts.Dir, "setup-complete.json"), &s.complete); err != nil {
		return nil, err
	}
	if s.complete {
		b, err := opts.Boot(opts.Context)
		if err != nil {
			return nil, err
		}
		s.active = b
		return s, nil
	}
	path := filepath.Join(opts.Dir, "setup-token")
	token, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		s.token = random()
		if err = os.WriteFile(path, []byte(s.token), 0600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		s.token = strings.TrimSpace(string(token))
	}
	if len(s.token) != 64 {
		return nil, errors.New("invalid setup credential file")
	}
	if _, err := credentials.Generate(opts.Dir); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Setup) Close(ctx context.Context) error {
	s.mu.RLock()
	b := s.active
	s.mu.RUnlock()
	if b != nil {
		return b.Close(ctx)
	}
	return nil
}
func (s *Setup) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	active := s.active
	s.mu.RUnlock()
	if active != nil {
		if strings.HasPrefix(r.URL.Path, "/api/setup") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/setup.html" {
			http.Redirect(w, r, "/login.html", 303)
			return
		}
		active.Handler.ServeHTTP(w, r)
		return
	}
	w.Header().Set("X-Lidza-Setup", "required")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	switch r.URL.Path {
	case "/healthz":
		agent.JSON(w, 200, map[string]string{"status": "ok"})
		return
	case "/readyz":
		agent.JSON(w, 503, map[string]string{"status": "setup_required"})
		return
	case "/", "/console.html", "/login.html":
		http.Redirect(w, r, "/setup.html", 303)
		return
	case "/setup.html", "/setup.js", "/style.css":
		s.opts.Frontend.ServeHTTP(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/setup/") {
		http.NotFound(w, r)
		return
	}
	// Only same-origin HTTPS or a local/tunneled setup origin is accepted.
	origin := s.opts.Origin
	if origin == "" {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "use the local setup tunnel", 403)
			return
		}
		origin = "http://" + r.Host
	}
	if r.Header.Get("Origin") != origin {
		http.Error(w, "origin rejected", 403)
		return
	}
	want := sha256.Sum256([]byte(s.token))
	got := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		http.Error(w, "invalid setup credential", 401)
		return
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/setup/check":
		agent.JSON(w, 200, map[string]any{"status": "claimed", "managed_database": dockerAvailable(), "public_url": s.opts.Origin})
		return
	case r.Method == "POST" && r.URL.Path == "/api/setup/complete":
		s.finish(w, r)
		return
	default:
		http.NotFound(w, r)
	}
}
func validate(input Input) error {
	u, err := url.Parse(input.PublicURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("enter an origin such as https://deploy.example.com")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")) {
		return errors.New("a public control panel requires HTTPS")
	}
	if !strings.Contains(input.Email, "@") || len(input.Password) < 16 {
		return errors.New("enter an operator email and a password of at least 16 characters")
	}
	if (input.GitHubID == "") != (input.GitHubSecret == "") {
		return errors.New("GitHub client ID and secret must be supplied together")
	}
	if input.DatabaseMode != "managed" && input.DatabaseMode != "external" {
		return errors.New("choose a managed or existing database")
	}
	if input.DatabaseMode == "external" && input.DatabaseURL == "" {
		return errors.New("enter the existing database URL")
	}
	return nil
}
func (s *Setup) finish(w http.ResponseWriter, r *http.Request) {
	if !s.apply.TryLock() {
		agent.Fail(w, 409, errors.New("setup is already running"))
		return
	}
	defer s.apply.Unlock()
	s.mu.RLock()
	done := s.active != nil
	s.mu.RUnlock()
	if done {
		http.NotFound(w, r)
		return
	}
	var input Input
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if s.opts.Origin != "" && input.PublicURL != s.opts.Origin {
		agent.Fail(w, 400, errors.New("control-panel address must match the hostname chosen during installation"))
		return
	}
	if err := validate(input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	dburl := input.DatabaseURL
	var err error
	if s.opts.database != nil {
		dburl, err = s.opts.database(ctx, input)
	} else if input.DatabaseMode == "managed" {
		dburl, err = s.managedDatabase(ctx)
	}
	if err != nil {
		agent.Fail(w, 409, err)
		return
	}
	conn, err := pgx.Connect(ctx, dburl)
	if err != nil {
		agent.Fail(w, 400, errors.New("could not connect to PostgreSQL; check its address, credentials and TLS configuration"))
		return
	}
	err = conn.Ping(ctx)
	conn.Close(ctx)
	if err != nil {
		agent.Fail(w, 400, errors.New("PostgreSQL health check failed"))
		return
	}
	if input.Agent == nil {
		var paired []control.Server
		if err = state.Load(filepath.Join(s.opts.Dir, "servers.json"), &paired); err != nil {
			agent.Fail(w, 500, errors.New("could not read paired agents"))
			return
		}
		for _, server := range paired {
			if err = validateAgent(ctx, server); err != nil {
				agent.Fail(w, 409, err)
				return
			}
		}
	}
	if input.Agent != nil {
		// Validation and connection check reuse the actual product server contract.
		if err = validateAgent(ctx, *input.Agent); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		if err = state.Save(filepath.Join(s.opts.Dir, "servers.json"), []control.Server{*input.Agent}); err != nil {
			agent.Fail(w, 500, errors.New("could not save agent connection"))
			return
		}
	}
	if s.opts.network != nil {
		err = s.opts.network(ctx, input)
	} else if s.opts.Origin != "" {
		// The installer already configured the hostname and Caddy route.
		// Verify its public TLS endpoint before persisting the canonical origin.
		if strings.HasPrefix(s.opts.Origin, "https://") {
			err = verifyHTTPS(ctx, s.opts.Origin)
		}
	} else {
		err = configureNetwork(ctx, input)
	}
	if err != nil {
		agent.Fail(w, 409, err)
		return
	}
	old, err := credentials.Read(s.opts.Dir)
	if err != nil {
		agent.Fail(w, 500, errors.New("could not read encrypted setup state"))
		return
	}
	secret := old["AUTH_SECRET"]
	if secret == "" {
		secret = random()
	}
	values := map[string]string{"PUBLIC_URL": input.PublicURL, "APP_URL": input.PublicURL, "CONTROL_USER": input.Email, "CONTROL_PASSWORD": input.Password, "DATABASE_URL": dburl, "AUTH_SECRET": secret, "AUTH_COOKIE_SECURE": "true", "AUTH_CONNECT": "", "AUTH_CONNECT_GITHUB_CLIENT_ID": input.GitHubID, "AUTH_CONNECT_GITHUB_CLIENT_SECRET": input.GitHubSecret}
	if strings.HasPrefix(input.PublicURL, "http://") {
		values["AUTH_COOKIE_SECURE"] = "false"
	}
	if input.GitHubID != "" {
		values["AUTH_CONNECT"] = "github"
	}
	if _, e := os.Stat(filepath.Join(s.opts.Dir, "servers.json")); e == nil {
		values["CONTROL_SERVERS_FILE"] = filepath.Join(s.opts.Dir, "servers.json")
	}
	if err = credentials.Set(s.opts.Dir, values); err != nil {
		agent.Fail(w, 500, errors.New("could not save encrypted configuration"))
		return
	}
	booted, err := s.opts.Boot(s.opts.Context)
	if err != nil {
		agent.Fail(w, 409, errors.New("application startup failed; verify database permissions and retry setup"))
		return
	}
	if err = state.Save(filepath.Join(s.opts.Dir, "setup-complete.json"), true); err != nil {
		booted.Close(context.Background())
		agent.Fail(w, 500, errors.New("could not persist setup completion; retry"))
		return
	}
	s.mu.Lock()
	s.active = booted
	s.complete = true
	s.mu.Unlock()
	// Completion is durable before the one-time credential is removed.
	_ = os.Remove(filepath.Join(s.opts.Dir, "setup-token"))
	_ = credentials.Unset(s.opts.Dir, "CONTROL_PASSWORD")
	agent.JSON(w, 200, map[string]string{"status": "complete", "redirect": input.PublicURL + "/login.html"})
}
