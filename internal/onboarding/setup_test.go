package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/webapp"
	"github.com/agim/lidza-deploy/web"
	"github.com/agim/lidza/pkg/credentials"
	"github.com/jackc/pgx/v5"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupRequest(s *Setup, method, path, body, token, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:3000"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", origin)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func cleanSetupEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LIDZA_MODE", "production")
	t.Setenv("JOBS_WORKERS", "0")
	for _, k := range []string{"DATABASE_URL", "AUTH_SECRET", "AUTH_COOKIE_SECURE", "LIDZA_MASTER_KEY", "PUBLIC_URL", "APP_URL", "CONTROL_USER", "CONTROL_PASSWORD", "AUTH_CONNECT", "AUTH_CONNECT_GITHUB_CLIENT_ID", "AUTH_CONNECT_GITHUB_CLIENT_SECRET", "CONTROL_SERVERS_FILE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Cleanup(func() { credentials.SetOverrides(nil) })
}
func TestSetupOwnershipAndSecretIsolation(t *testing.T) {
	cleanSetupEnv(t)
	dir := t.TempDir()
	opts := Options{Dir: dir, Frontend: web.Handler(), Boot: func(context.Context) (*lidza.Booted, error) { t.Fatal("unexpected boot"); return nil, nil }}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(dir, "setup-token"))
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "setup-token")); info.Mode().Perm() != 0600 {
		t.Fatal("token permissions")
	}
	if w := setupRequest(s, "GET", "/", "", "", ""); w.Code != 303 || w.Header().Get("Location") != "/setup.html" {
		t.Fatal("no wizard", w.Code)
	}
	if w := setupRequest(s, "GET", "/readyz", "", "", ""); w.Code != 503 {
		t.Fatal("unconfigured ready")
	}
	for _, tc := range []struct {
		token, origin string
		code          int
	}{{"", "http://127.0.0.1:3000", 401}, {string(token), "https://evil.example", 403}, {string(token), "http://127.0.0.1:3000", 200}} {
		w := setupRequest(s, "POST", "/api/setup/check", "{}", tc.token, tc.origin)
		if w.Code != tc.code {
			t.Fatal(w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), string(token)) {
			t.Fatal("bootstrap credential returned")
		}
	}
	restarted, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.token != s.token {
		t.Fatal("retry rotated ownership key")
	}
	if w := setupRequest(s, "POST", "/api/setup/complete", `{"password":"short"}`, s.token, "http://127.0.0.1:3000"); w.Code != 400 {
		t.Fatal("invalid setup accepted")
	}
}
func TestFirstRunBootAndRestart(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required")
	}
	cleanSetupEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CONTROL_DATA_DIR", dir)
	admin, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	database := "onboarding_" + random()[:12]
	if _, err = admin.Exec(context.Background(), "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)")
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = database
	// ConnString reflects the original string; replace only its database component.
	uri := strings.Replace(dsn, "/lidza_deploy_test", "/"+database, 1)
	if uri == dsn {
		t.Fatal("test DSN must name lidza_deploy_test")
	}
	opts := Options{Dir: dir, Frontend: web.Handler(), Boot: webapp.Boot}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	input := Input{PublicURL: "http://127.0.0.1:3000", Email: "owner@example.com", Password: "a-long-first-run-password", DatabaseMode: "external", DatabaseURL: "postgres://bad:invalid@127.0.0.1:1/missing?connect_timeout=1&sslmode=disable"}
	body, _ := json.Marshal(input)
	w := setupRequest(s, "POST", "/api/setup/complete", string(body), s.token, input.PublicURL)
	if w.Code != 400 || strings.Contains(w.Body.String(), "bad:invalid") {
		t.Fatal("invalid DB handling", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "setup-complete.json")); !os.IsNotExist(err) {
		t.Fatal("failed setup marked complete")
	}
	input.DatabaseURL = uri
	body, _ = json.Marshal(input)
	// A failure after saving encrypted settings must still leave a retryable wizard.
	s.opts.Boot = func(context.Context) (*lidza.Booted, error) { return nil, errors.New("test startup failure") }
	w = setupRequest(s, "POST", "/api/setup/complete", string(body), s.token, input.PublicURL)
	if w.Code != 409 {
		t.Fatal("startup failure not surfaced", w.Code, w.Body)
	}
	s.opts.Boot = webapp.Boot
	w = setupRequest(s, "POST", "/api/setup/complete", string(body), s.token, input.PublicURL)
	if w.Code != 200 {
		t.Fatal("setup failed", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), input.Password) || strings.Contains(w.Body.String(), input.DatabaseURL) {
		t.Fatal("secret echoed")
	}
	if w := setupRequest(s, "POST", "/api/setup/check", "{}", s.token, input.PublicURL); w.Code != 404 {
		t.Fatal("bootstrap remained open")
	}
	if _, err := os.Stat(filepath.Join(dir, "setup-token")); !os.IsNotExist(err) {
		t.Fatal("one-time credential retained")
	}
	sealed, _ := os.ReadFile(filepath.Join(dir, credentials.File))
	if strings.Contains(string(sealed), input.Password) || strings.Contains(string(sealed), uri) {
		t.Fatal("credentials not encrypted")
	}
	saved, err := credentials.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if saved["CONTROL_PASSWORD"] != "" {
		t.Fatal("operator bootstrap password retained")
	}
	login := setupRequest(s, "POST", "/api/v1/auth/login", `{"email":"owner@example.com","password":"a-long-first-run-password"}`, "", input.PublicURL)
	if login.Code != 200 {
		t.Fatal("initial operator cannot sign in", login.Code, login.Body)
	}
	// Configure GitHub through the authenticated GUI API and verify live reload.
	r := httptest.NewRequest("POST", "/api/control/github/config", strings.NewReader(`{"client_id":"fixture-id","client_secret":"fixture-secret"}`))
	r.Header.Set("Origin", input.PublicURL)
	r.Header.Set("Content-Type", "application/json")
	for _, cookie := range login.Result().Cookies() {
		r.AddCookie(cookie)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("GitHub GUI config", w.Code, w.Body)
	}
	r = httptest.NewRequest("GET", "/api/v1/auth/connect/github/start", nil)
	for _, cookie := range login.Result().Cookies() {
		r.AddCookie(cookie)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 302 || !strings.Contains(w.Header().Get("Location"), "github.com") {
		t.Error("connector not reloaded (upstream framework issue)", w.Code)
	}
	if err = s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := New(opts)
	if err != nil {
		t.Fatal("restart", err)
	}
	defer restored.Close(context.Background())
	if restored.active == nil {
		t.Fatal("restart reopened setup")
	}
	w = httptest.NewRecorder()
	restored.ServeHTTP(w, r)
	if w.Code != 302 || !strings.Contains(w.Header().Get("Location"), "github.com") {
		t.Fatal("GitHub configured before boot did not activate", w.Code)
	}
	if w := setupRequest(restored, "GET", "/readyz", "", "", ""); w.Code != 200 {
		t.Fatal("restarted app not ready", w.Code, w.Body)
	}
}

func TestInstallerOriginControlsBootstrap(t *testing.T) {
	cleanSetupEnv(t)
	s, err := New(Options{Dir: t.TempDir(), Origin: "https://deploy.example.com", Frontend: web.Handler(), Boot: func(context.Context) (*lidza.Booted, error) { t.Fatal("unexpected boot"); return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	w := setupRequest(s, "POST", "/api/setup/check", "{}", s.token, "https://deploy.example.com")
	if w.Code != 200 {
		t.Fatal("public setup origin rejected", w.Code)
	}
	var result struct {
		PublicURL string `json:"public_url"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.PublicURL != "https://deploy.example.com" {
		t.Fatal("installer hostname not supplied to wizard")
	}
	if w = setupRequest(s, "POST", "/api/setup/check", "{}", s.token, "http://127.0.0.1:3000"); w.Code != 403 {
		t.Fatal("public install accepted localhost origin")
	}
	w = setupRequest(s, "POST", "/api/setup/complete", `{"public_url":"https://other.example.com"}`, s.token, "https://deploy.example.com")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "hostname chosen during installation") {
		t.Fatal("wizard changed installer hostname", w.Code)
	}
}
