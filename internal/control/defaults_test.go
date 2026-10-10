package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestApplicationDefaultsPersistenceAndReviewedApply(t *testing.T) {
	key := strings.Repeat("a", 32)
	manager, err := agent.NewManager(context.Background(), agent.Config{DataDir: t.TempDir(), APIKey: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	remote := httptest.NewServer(agent.Handler(manager))
	defer remote.Close()
	cfg := Config{PublicURL: "http://127.0.0.1:3000", User: "operator@example.com", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir()}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	call := func(fn http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/", strings.NewReader(body))
		r.SetPathValue("id", "portal")
		w := httptest.NewRecorder()
		fn(w, r)
		return w
	}
	server, _ := json.Marshal(Server{ID: "one", Name: "Hosting", URL: remote.URL, Token: key})
	if w := call(c.addServer, "POST", string(server)); w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	if w := call(c.create, "POST", `{"id":"portal","server_id":"one","repository":"acme/portal","branch":"main","domain":"portal.example.com","env":{"ADMIN_USERS":"app@example.com"}}`); w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	for _, body := range []string{`{"values":{"AUTH_SECRET":"shared"}}`, `{"values":{"DATABASE_URL":"shared"}}`, `{"values":{"DB_MIGRATE":"yes"}}`} {
		if w := call(c.applicationDefaults, "PUT", body); w.Code != 400 {
			t.Fatal("unsafe default accepted", w.Code)
		}
	}
	w := call(c.applicationDefaults, "PUT", `{"values":{"DB_MIGRATE":"false","ADMIN_USERS":"shared@example.com","MAIL_FROM":"","LOG_LEVEL":"info"}}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var profile defaultsView
	json.Unmarshal(w.Body.Bytes(), &profile)
	if _, ok := profile.Values["MAIL_FROM"]; ok {
		t.Fatal("empty value retained")
	}
	c2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c2.applicationDefaultsSnapshot()["ADMIN_USERS"] != "shared@example.com" {
		t.Fatal("defaults not persisted")
	}
	input := map[string]any{"keys": []string{"ADMIN_USERS", "LOG_LEVEL"}, "revision": "stale", "replace": false}
	raw, _ := json.Marshal(input)
	if w := call(c.applyDefaults, "POST", string(raw)); w.Code != 409 {
		t.Fatal("stale review accepted")
	}
	input["revision"] = profile.Revision
	raw, _ = json.Marshal(input)
	if w := call(c.applyDefaults, "POST", string(raw)); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	// Test the write-only snapshot directly without publishing values through API.
	settings, _ := manager.Settings("portal")
	if settings.EnvSources["ADMIN_USERS"] != "" || settings.EnvSources["LOG_LEVEL"] != "workspace" {
		t.Fatal("missing-only policy not respected")
	}
	input["replace"] = true
	raw, _ = json.Marshal(input)
	if w := call(c.applyDefaults, "POST", string(raw)); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	settings, _ = manager.Settings("portal")
	if settings.EnvSources["ADMIN_USERS"] != "workspace" {
		t.Fatal("approved replacement not applied")
	}
	// A deliberately empty profile survives restart, without restoring built-ins.
	if w := call(c.applicationDefaults, "PUT", `{"values":{}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	c3, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(c3.applicationDefaultsSnapshot()) != 0 {
		t.Fatal("empty profile restored built-ins")
	}
	for _, pattern := range []string{"PUT /api/control/application-defaults", "POST /api/control/apps/{id}/apply-defaults"} {
		if routePermission(pattern) != "infrastructure.manage" {
			t.Fatal("defaults mutation should require administrator")
		}
	}
}
