package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	gh "github.com/agim/lidza-deploy/internal/providers/github"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/pkg/webhook"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func exerciseAppConnection(t *testing.T, c *Control, ctx context.Context, call func(string, string, string, []*http.Cookie, string) *httptest.ResponseRecorder, cookies []*http.Cookie) {
	t.Helper()
	t.Chdir(t.TempDir())
	originalURL, originalGH := c.cfg.PublicURL, c.cfg.GitHub
	defer func() { c.cfg.PublicURL = originalURL; c.cfg.GitHub = originalGH }()
	c.cfg.PublicURL = "https://deploy.example.com"
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app := gh.App{ID: 42, Slug: "self-hosted-deploy", PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), WebhookSecret: "github-app-fixture-secret"}
	minted := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app-manifests/code/conversions":
			json.NewEncoder(w).Encode(app)
		case "/app/installations/7":
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "app_id": 42, "account": map[string]string{"login": "acme"}, "permissions": map[string]string{"contents": "read", "pull_requests": "read"}})
		case "/app/installations/8":
			json.NewEncoder(w).Encode(map[string]any{"id": 8, "app_id": 99})
		case "/repos/acme/portal/installation":
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "app_id": 42})
		case "/app/installations/7/access_tokens":
			minted++
			json.NewEncoder(w).Encode(map[string]any{"token": "fresh-installation-token", "expires_at": time.Now().Add(time.Hour)})
		case "/repos/acme/portal/branches":
			if r.Header.Get("Authorization") != "Bearer fresh-installation-token" {
				t.Fatal("branch request omitted installation credentials")
			}
			json.NewEncoder(w).Encode([]map[string]string{{"name": "master"}, {"name": "release"}})
		case "/installation/repositories":
			json.NewEncoder(w).Encode(map[string]any{"repositories": []gh.Repository{{FullName: "acme/portal", Private: true}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	c.cfg.GitHub = &gh.Client{API: remote.URL}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		return call(method, path, body, cookies, c.cfg.PublicURL)
	}
	if w := request("GET", "/api/control/github/app/manifest-callback?state=invalid&code=code", ""); w.Code != 400 {
		t.Fatal("invalid state accepted", w.Code)
	}
	begin := request("POST", "/api/control/github/app/register", `{}`)
	if begin.Code != 200 {
		t.Fatal("registration failed", begin.Code, begin.Body)
	}
	var registration struct {
		Action   string
		Manifest map[string]any
	}
	json.Unmarshal(begin.Body.Bytes(), &registration)
	target, _ := url.Parse(registration.Action)
	state := target.Query().Get("state")
	if state == "" || !strings.HasPrefix(registration.Action, "https://github.com/settings/apps/new?") || registration.Manifest["public"] != false {
		t.Fatal("invalid manifest form")
	}
	completePath := "/api/control/github/app/manifest-callback?state=" + url.QueryEscape(state) + "&code=code"
	if w := request("GET", completePath, ""); w.Code != 303 {
		t.Fatal("manifest callback", w.Code, w.Body)
	}
	if w := request("GET", completePath, ""); w.Code != 400 {
		t.Fatal("manifest replay accepted")
	}
	for _, path := range []string{"/api/control/status", "/api/control/github/app/status"} {
		w := request("GET", path, "")
		if strings.Contains(w.Body.String(), app.PEM) || strings.Contains(w.Body.String(), app.WebhookSecret) {
			t.Fatal("app credentials exposed")
		}
	}
	installation := func(id string) *httptest.ResponseRecorder {
		w := request("POST", "/api/control/github/app/install", `{}`)
		if w.Code != 200 {
			t.Fatal("install start", w.Code)
		}
		var out struct{ URL string }
		json.Unmarshal(w.Body.Bytes(), &out)
		u, _ := url.Parse(out.URL)
		return request("GET", "/api/control/github/app/install-callback?installation_id="+id+"&state="+url.QueryEscape(u.Query().Get("state")), "")
	}
	if w := installation("8"); w.Code != 409 {
		t.Fatal("another app's installation accepted", w.Code)
	}
	if w := installation("7"); w.Code != 303 {
		t.Fatal("installation callback", w.Code, w.Body)
	}
	if w := request("GET", "/api/control/github/repos", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"installation_id":7`) {
		t.Fatal("selected repositories missing", w.Code, w.Body)
	}
	if w := request("GET", "/api/control/github/branches?repository=acme/portal&installation=7", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"master"`) {
		t.Fatal("private repository branches unavailable", w.Code, w.Body)
	}
	if w := request("GET", "/api/control/github/branches?repository=../bad&installation=7", ""); w.Code != 400 {
		t.Fatal("unsafe repository accepted", w.Code)
	}
	c.mu.Lock()
	a := c.data.Apps["portal"]
	a.Repository = "acme/portal"
	a.ServerID = "one"
	a.GitHubInstallation = 7
	c.data.Apps[a.ID] = a
	err = c.save()
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	// Explicit migration updates existing preview credential assignments too.
	c.mu.Lock()
	a.GitHubInstallation = 0
	c.data.Apps[a.ID] = a
	child := a
	child.ID = "portal-pr-99"
	child.PreviewParent = a.ID
	c.data.Apps[child.ID] = child
	c.mu.Unlock()
	if w := request("POST", "/api/control/apps/portal/github-app", `{}`); w.Code != 200 {
		t.Fatal("legacy migration failed", w.Code, w.Body)
	}
	migrated, _ := c.app(a.ID)
	preview, _ := c.app(child.ID)
	if migrated.GitHubInstallation != 7 || preview.GitHubInstallation != 7 {
		t.Fatal("migration omitted app or existing preview")
	}
	a = migrated
	c.mu.Lock()
	delete(c.data.Apps, child.ID)
	c.mu.Unlock()
	legacyRequest := httptest.NewRequest("POST", "/hooks/github/portal", nil).WithContext(ctx)
	legacyRequest.SetPathValue("id", a.ID)
	legacyResponse := httptest.NewRecorder()
	c.webhook(legacyResponse, legacyRequest)
	if legacyResponse.Code != 404 {
		t.Fatal("legacy webhook still active after migration")
	}
	credentials, err := c.deploymentCredentials(ctx, a, "queued-deploy")
	if err != nil || credentials.Token != "" || credentials.CredentialTicket == "" {
		t.Fatal("queue received a token instead of a ticket", err)
	}
	issue := func(token, server string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"ticket": credentials.CredentialTicket})
		r := httptest.NewRequest("POST", "/api/agent/checkout-token", strings.NewReader(string(body)))
		r = r.WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Lidza-Server", server)
		w := httptest.NewRecorder()
		c.checkoutToken(w, r)
		return w
	}
	if w := issue("wrong", "one"); w.Code != 401 {
		t.Fatal("bad agent key accepted")
	}
	before := minted
	if w := issue(strings.Repeat("a", 32), "one"); w.Code != 200 || !strings.Contains(w.Body.String(), "fresh-installation-token") {
		t.Fatal("checkout mint failed", w.Code, w.Body)
	}
	if minted != before+1 {
		t.Fatal("checkout did not mint a fresh token")
	}
	if w := issue(strings.Repeat("a", 32), "one"); w.Code != 401 {
		t.Fatal("checkout ticket replay accepted")
	}
	restored, err := New(c.cfg)
	if err != nil || restored.githubApp() == nil || restored.githubApp().Installations[7] != "acme" {
		t.Fatal("app connection did not survive reload", err)
	}
	data, _ := os.ReadFile(c.path())
	if strings.Contains(string(data), app.WebhookSecret) || strings.Contains(string(data), "RSA PRIVATE KEY") {
		t.Fatal("app secrets not encrypted")
	}
	// Framework signature and durable dedup protect the shared App webhook.
	c.mu.Lock()
	a = c.data.Apps["portal"]
	a.AutoDeploy = true
	c.data.Apps[a.ID] = a
	c.mu.Unlock()
	payload := []byte(`{"ref":"refs/heads/main","repository":{"full_name":"acme/portal"},"installation":{"id":7}}`)
	delivery := "app-push-" + random()
	hook := func(signature string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/hooks/github-app", bytes.NewReader(payload)).WithContext(ctx)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-GitHub-Event", "push")
		r.Header.Set("X-GitHub-Delivery", delivery)
		r.Header.Set("X-Hub-Signature-256", signature)
		w := httptest.NewRecorder()
		c.githubAppWebhook(w, r)
		return w
	}
	if w := hook("sha256=invalid"); w.Code != 401 {
		t.Fatal("invalid App webhook signature accepted", w.Code)
	}
	signature := "sha256=" + webhook.HMACSignature(app.WebhookSecret, payload)
	for i := 0; i < 2; i++ {
		if w := hook(signature); w.Code < 200 || w.Code >= 300 {
			t.Fatal("signed/replayed App webhook rejected", w.Code, w.Body)
		}
	}
	// Registration remains bound to the local installation owner, independent of GitHub identity.
	wrongState, err := auth.From(ctx).IssueToken(ctx, "deploy.github.manifest", "someone-else ", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if w := request("GET", "/api/control/github/app/manifest-callback?code=code&state="+wrongState, ""); w.Code != 400 {
		t.Fatal("another owner's registration accepted")
	}
}
