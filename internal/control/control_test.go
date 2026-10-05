package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/agim/lidza"
	"github.com/agim/lidza/packs/audit"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/packs/db"
	"github.com/agim/lidza/packs/jobs"
	"github.com/agim/lidza/pkg/credentials"
	"github.com/agim/lidza/pkg/router"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestFrameworkAuthIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration")
	}
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("JOBS_WORKERS", "0")
	t.Setenv("AUTH_SECRET", strings.Repeat("s", 64))
	t.Setenv("AUTH_COOKIE_SECURE", "false")
	t.Setenv("LIDZA_MASTER_KEY", strings.Repeat("ab", 32))
	t.Setenv("AUTH_CONNECT", "")
	t.Setenv("APP_URL", "http://127.0.0.1:3000")
	var agentCalls int
	var receivedToken string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentCalls++
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 32) {
			t.Error("missing agent credential")
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/deploy") {
			var request struct {
				Token string `json:"github_token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			receivedToken = request.Token
			w.WriteHeader(202)
			io.WriteString(w, `{"id":"job","status":"queued"}`)
		} else if strings.HasSuffix(r.URL.Path, "/errors") {
			io.WriteString(w, `{"status":"ready","errors":[],"limit":500}`)
		} else {
			io.WriteString(w, `[]`)
		}
	}))
	defer remote.Close()
	cfg := Config{Connectors: []auth.Connector{auth.GitHubConnect("test", "test")}, PublicURL: "http://127.0.0.1:3000", User: "operator@example.com", Password: "a-long-local-password-123", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(), Servers: []Server{{ID: "one", URL: remote.URL, Token: strings.Repeat("a", 32)}}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	handler := c.Handler(http.NotFoundHandler())
	app := lidza.App{Name: "test", Packs: []lidza.Pack{db.Pack(), auth.Pack(), audit.Pack(), jobs.Pack()}, OnStart: c.Start, Frontend: handler, Routes: func(r *router.Router) {
		auth.Mount(r, auth.Options{NoRegister: true, Connectors: []auth.Connector{auth.GitHubConnect("test", "test")}})
		r.Handle("/api/control/", handler)
	}}
	boot, err := lidza.Boot(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close(context.Background())
	call := func(method, path, body string, cookies []*http.Cookie, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		for _, v := range cookies {
			r.AddCookie(v)
		}
		w := httptest.NewRecorder()
		boot.Handler.ServeHTTP(w, r)
		return w
	}
	if w := call("GET", "/api/control/apps", "", nil, ""); w.Code != 401 {
		t.Fatal("anonymous access", w.Code)
	}
	login := call("POST", "/api/v1/auth/login", `{"email":"operator@example.com","password":"a-long-local-password-123"}`, nil, "")
	if login.Code != 200 {
		t.Fatal(login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if w := call("GET", "/api/control/apps", "", cookies, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	body := `{"id":"portal","server_id":"one","repository":"acme/portal","branch":"main","domain":"portal.example.com"}`
	if w := call("POST", "/api/control/apps", body, cookies, "https://evil.example"); w.Code != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
	if w := call("POST", "/api/control/apps", body, cookies, cfg.PublicURL); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("POST", "/api/control/apps/portal/deploy", "{}", cookies, cfg.PublicURL); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Management routes share the framework session and origin protections.
	for _, endpoint := range []struct{ method, path string }{
		{"POST", "/api/control/servers"}, {"PUT", "/api/control/servers/one"},
		{"DELETE", "/api/control/servers/one"}, {"PATCH", "/api/control/apps/portal/settings"},
		{"DELETE", "/api/control/apps/portal/webhook"},
		{"DELETE", "/api/control/apps/portal"},
	} {
		if w := call(endpoint.method, endpoint.path, "{}", nil, cfg.PublicURL); w.Code != 401 {
			t.Fatal("anonymous mutation", endpoint.path, w.Code)
		}
		if w := call(endpoint.method, endpoint.path, "{}", cookies, "https://evil.example"); w.Code != 403 {
			t.Fatal("cross-origin mutation", endpoint.path, w.Code)
		}
	}

	// Public repositories must deploy even when the GitHub connector is not configured.
	connectors := c.cfg.Connectors
	c.cfg.Connectors = nil
	if w := call("POST", "/api/control/apps/portal/deploy", "{}", cookies, cfg.PublicURL); w.Code != 202 {
		t.Fatal("public deploy required OAuth", w.Code, w.Body.String())
	}
	c.cfg.Connectors = connectors
	// An actual second framework user cannot access the deployment fleet.
	ctx := lidza.WithServices(context.Background(), boot.Services)
	other, err := auth.From(ctx).CreateUser(ctx, "other-"+random()+"@example.com", "Other", "another-long-password-123")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.From(ctx).Login(ctx, other.Subject, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/api/control/apps", "", auth.From(ctx).Cookies(tokens), ""); w.Code != 403 {
		t.Fatal("nonoperator accessed fleet", w.Code)
	}
	// Team permissions come from the released framework and are read on every
	// request. Changing/removing a role affects an already issued session.
	memberCookies := auth.From(ctx).Cookies(tokens)
	setRole := func(role string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"email": other.Email, "role": role, "password": "never-store-this-member-password"})
		if w := call("POST", "/api/control/team", string(body), cookies, cfg.PublicURL); w.Code != 200 {
			t.Fatal("assign role", role, w.Code, w.Body.String())
		}
	}
	setRole("viewer")
	if w := call("GET", "/api/control/apps/portal/errors", "", nil, ""); w.Code != 401 {
		t.Fatal("unauthenticated app errors exposed", w.Code)
	}
	if w := call("GET", "/api/control/apps/missing/errors", "", memberCookies, ""); w.Code != 404 {
		t.Fatal("unknown app errors exposed", w.Code)
	}
	if w := call("GET", "/api/control/apps/portal/errors", "", memberCookies, ""); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("viewer cannot read assigned app errors", w.Code, w.Body)
	}
	if w := call("GET", "/api/control/apps", "", memberCookies, ""); w.Code != 200 {
		t.Fatal("viewer cannot read fleet", w.Code)
	}
	for _, route := range []struct{ method, path string }{
		{"POST", "/api/control/apps/portal/deploy"}, {"PATCH", "/api/control/apps/portal/settings"},
		{"POST", "/api/control/team"}, {"PUT", "/api/control/mail"},
		{"GET", "/api/control/audit"}, {"GET", "/api/control/team"},
		{"GET", "/api/control/servers/one/databases/portal/backups/snapshot"},
	} {
		before := agentCalls
		if w := call(route.method, route.path, "{}", memberCookies, cfg.PublicURL); w.Code != 403 {
			t.Fatal("viewer authorization", route.path, w.Code, w.Body.String())
		}
		if agentCalls != before {
			t.Fatal("denied route contacted agent", route.path)
		}
	}
	for i := 0; i < 20; i++ {
		if w := call("POST", "/api/control/apps/portal/deploy", "{}", memberCookies, cfg.PublicURL); w.Code != 403 {
			t.Fatal("viewer denial", w.Code)
		}
	}
	setRole("deployer")
	if w := call("POST", "/api/control/apps/portal/deploy", "{}", memberCookies, cfg.PublicURL); w.Code != 202 {
		t.Fatal("deployer cannot deploy", w.Code, w.Body.String())
	}
	for _, route := range []struct{ method, path string }{
		{"POST", "/api/control/team"}, {"DELETE", "/api/control/team/" + other.Subject},
		{"POST", "/api/control/servers"}, {"POST", "/api/control/apps"},
		{"PUT", "/api/control/apps/portal/previews"}, {"PUT", "/api/control/apps/portal/restore"},
		{"POST", "/api/control/apps/portal/restore"}, {"POST", "/api/control/apps/portal/database"},
	} {
		if w := call(route.method, route.path, "{}", memberCookies, cfg.PublicURL); w.Code != 403 {
			t.Fatal("deployer reached admin operation", route.path, w.Code)
		}
	}
	setRole("viewer")
	if w := call("POST", "/api/control/apps/portal/deploy", "{}", memberCookies, cfg.PublicURL); w.Code != 403 {
		t.Fatal("role downgrade did not affect active session", w.Code)
	}
	if w := call("DELETE", "/api/control/team/"+other.Subject, "{}", cookies, cfg.PublicURL); w.Code != 200 {
		t.Fatal("remove member", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/control/apps", "", memberCookies, ""); w.Code != 403 {
		t.Fatal("revoked membership still has access", w.Code)
	}
	if w := call("DELETE", "/api/control/team/"+c.operatorID, "{}", cookies, cfg.PublicURL); w.Code != 409 {
		t.Fatal("owner removal allowed", w.Code)
	}
	ownerDemotion, _ := json.Marshal(map[string]string{"email": cfg.User, "role": "viewer"})
	if w := call("POST", "/api/control/team", string(ownerDemotion), cookies, cfg.PublicURL); w.Code != 409 {
		t.Fatal("owner demotion allowed", w.Code)
	}
	// Database write failure must stop a deployment before the agent is called.
	if _, err := db.From(ctx).Exec(ctx, `CREATE OR REPLACE FUNCTION deploy_test_audit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='control.post.requested' AND NEW.resource='/api/control/apps/{id}/deploy' THEN RAISE EXCEPTION 'fixture audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER deploy_test_audit_failure BEFORE INSERT ON audit_event FOR EACH ROW EXECUTE FUNCTION deploy_test_audit_failure()`); err != nil {
		t.Fatal(err)
	}
	before := agentCalls
	failedAudit := call("POST", "/api/control/apps/portal/deploy", "{}", cookies, cfg.PublicURL)
	_, cleanupErr := db.From(ctx).Exec(ctx, `DROP TRIGGER deploy_test_audit_failure ON audit_event; DROP FUNCTION deploy_test_audit_failure()`)
	if cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if failedAudit.Code != 503 || agentCalls != before {
		t.Fatal("audit failure permitted side effects", failedAudit.Code, agentCalls, before)
	}
	// If only the result write fails, the remote operation may already be
	// accepted. Surface that ambiguity explicitly instead of reporting success.
	if _, err := db.From(ctx).Exec(ctx, `CREATE OR REPLACE FUNCTION deploy_test_audit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='control.post' AND NEW.resource='/api/control/apps/{id}/deploy' THEN RAISE EXCEPTION 'fixture audit result failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER deploy_test_audit_failure BEFORE INSERT ON audit_event FOR EACH ROW EXECUTE FUNCTION deploy_test_audit_failure()`); err != nil {
		t.Fatal(err)
	}
	before = agentCalls
	failedResult := call("POST", "/api/control/apps/portal/deploy", "{}", cookies, cfg.PublicURL)
	_, cleanupErr = db.From(ctx).Exec(ctx, `DROP TRIGGER deploy_test_audit_failure ON audit_event; DROP FUNCTION deploy_test_audit_failure()`)
	if cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if failedResult.Code != 503 || agentCalls != before+1 || !strings.Contains(failedResult.Body.String(), "may have applied") {
		t.Fatal("post-action audit failure misreported", failedResult.Code)
	}
	page := call("GET", "/api/control/audit", "", cookies, "")
	if page.Code != 200 || strings.Contains(page.Body.String(), "never-store-this-member-password") || strings.Contains(page.Body.String(), cfg.Password) {
		t.Fatal("audit unavailable or leaked a secret", page.Code)
	}
	var auditPage audit.Page
	if err := json.Unmarshal(page.Body.Bytes(), &auditPage); err != nil {
		t.Fatal(err)
	}
	foundDenied, foundMember := false, false
	for _, record := range auditPage.Records {
		if record.Actor == other.Subject && record.Outcome == audit.Denied {
			foundDenied = true
		}
		if record.Actor == c.operatorID && record.Action == "team.member.set" && record.Resource == "member/"+other.Subject && record.Meta["role"] != "" {
			foundMember = true
		}
	}
	if !foundDenied || !foundMember {
		t.Fatal("missing authenticated actor, denial, or membership audit event")
	}
	if auditPage.Next == "" {
		t.Fatal("audit pagination fixture did not exercise next cursor")
	}
	if w := call("GET", "/api/control/audit?cursor="+auditPage.Next, "", cookies, ""); w.Code != 200 {
		t.Fatal("audit cursor failed", w.Code)
	}
	// Signed matching push dispatches; wrong branch and forged signatures do not.
	c.mu.Lock()
	a := c.data.Apps["portal"]
	a.AutoDeploy = true
	a.Secret = "webhook-secret"
	c.data.Apps[a.ID] = a
	c.mu.Unlock()
	push := `{"ref":"refs/heads/main","repository":{"full_name":"acme/portal"}}`
	deliveryID := random()
	webhook := func(body, signature string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/hooks/github/portal", strings.NewReader(body))
		r.Header.Set("X-Hub-Signature-256", signature)
		r.Header.Set("X-GitHub-Event", "push")
		r.Header.Set("X-GitHub-Delivery", deliveryID)
		w := httptest.NewRecorder()
		boot.Handler.ServeHTTP(w, r)
		return w
	}
	sign := func(body string) string {
		m := hmac.New(sha256.New, []byte(a.Secret))
		m.Write([]byte(body))
		return "sha256=" + hex.EncodeToString(m.Sum(nil))
	}
	count := agentCalls
	if w := webhook(push, "sha256=00"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	wrong := strings.Replace(push, "main", "other", 1)
	if w := webhook(wrong, sign(wrong)); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if agentCalls != count {
		t.Fatal("invalid webhook dispatched")
	}
	if w := webhook(push, sign(push)); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Retain a grant using the framework's encrypted storage format. The controller
	// retrieves it through auth.Connection; only the agent receives its token.
	key, err := credentials.Key(".")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := credentials.Encrypt(key, []byte("private-fixture-token"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.From(ctx).Exec(ctx, `INSERT INTO auth_connection(id,owner,provider,subject,access_sealed) VALUES($1,$2,'github','fixture',$3) ON CONFLICT(owner,provider) DO UPDATE SET access_sealed=EXCLUDED.access_sealed`, random(), c.operatorID, sealed)
	if err != nil {
		t.Fatal(err)
	}
	defer db.From(ctx).Exec(ctx, `DELETE FROM auth_connection WHERE owner=$1`, c.operatorID)
	// The verified webhook persists an application-only job; dispatch uses the framework connection.
	rows, err := jobs.From(ctx).Recent(ctx, 10)
	if err != nil || len(rows) == 0 {
		t.Fatal("webhook not persisted", err)
	}
	if err := c.dispatchPush(ctx, rows[0].Payload); err != nil {
		t.Fatal(err)
	}
	if receivedToken != "private-fixture-token" {
		t.Fatal("framework grant did not reach private deployment")
	}
	exercisePreviews(t, c, ctx)
	if w := call("DELETE", "/api/control/apps/portal/webhook", "", cookies, cfg.PublicURL); w.Code != 200 {
		t.Fatal("disable autodeploy", w.Code, w.Body)
	}
	count = agentCalls
	if w := webhook(push, sign(push)); w.Code != 204 {
		t.Fatal("disabled webhook queued", w.Code)
	}
	if err := c.dispatchPush(ctx, rows[0].Payload); err != nil {
		t.Fatal(err)
	}
	if agentCalls != count {
		t.Fatal("disabled job reached agent")
	}

	// Līdza issues the connector redirect with PKCE and repository scopes.
	start := call("GET", "/api/v1/auth/connect/github/start?redirect=/console.html", "", cookies, "")
	if start.Code != 302 {
		t.Fatal(start.Code, start.Body.String())
	}
	location := start.Header().Get("Location")
	if !strings.Contains(location, "code_challenge=") || !strings.Contains(location, "repo") {
		t.Fatal("framework connection not configured", location)
	}
	b, err := os.ReadFile(c.path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "portal.example.com") {
		t.Fatal("control state not encrypted")
	}
	// Metadata never serializes credentials.
	w := call("GET", "/api/control/servers", "", cookies, "")
	var servers []Server
	if err = json.Unmarshal(w.Body.Bytes(), &servers); err != nil {
		t.Fatal(err)
	}
	if servers[0].Token != "" {
		t.Fatal("agent token exposed")
	}

	exerciseAppConnection(t, c, ctx, call, cookies)

}
