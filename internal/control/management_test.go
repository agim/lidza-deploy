package control

import (
	"context"
	"encoding/json"
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestManagementPersistenceAndAgentSettings(t *testing.T) {
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
	call := func(fn http.HandlerFunc, method, id, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/", strings.NewReader(body))
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		fn(w, r)
		return w
	}
	server := func(token string) string {
		b, _ := json.Marshal(Server{ID: "one", Name: "Production", URL: remote.URL, Token: token})
		return string(b)
	}
	if w := call(c.addServer, "POST", "", server(strings.Repeat("b", 32))); w.Code != 502 {
		t.Fatal("bad credentials accepted", w.Code)
	}
	if w := call(c.addServer, "POST", "", server(key)); w.Code != 201 || strings.Contains(w.Body.String(), key) {
		t.Fatal("add server", w.Code, w.Body)
	}
	if w := call(c.addServer, "POST", "", server(key)); w.Code != 409 {
		t.Fatal("duplicate accepted")
	}
	if w := call(c.editServer, "PUT", "one", server("")); w.Code != 200 {
		t.Fatal("retain token", w.Code, w.Body)
	}
	if w := call(c.create, "POST", "", `{"id":"portal","server_id":"one","repository":"acme/portal","branch":"main","domain":"portal.example.com","env":{"SECRET":"never-return-this","OLD":"remove-me"}}`); w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	if w := call(c.removeServer, "DELETE", "one", ""); w.Code != 409 {
		t.Fatal("removed occupied server")
	}
	previewSecret := "private-preview-environment"
	app, _ := c.app("portal")
	app.Previews = PreviewConfig{Enabled: true, BaseDomain: "preview.example.com", Env: map[string]string{"API_TOKEN": previewSecret}}
	c.data.Apps[app.ID] = app
	if w := call(c.updateSettings, "PATCH", "portal", `{"branch":"release","domain":"new.example.com","env_changes":{"NEW":"new-secret","OLD":null}}`); w.Code != 200 || strings.Contains(w.Body.String(), previewSecret) {
		t.Fatal(w.Code, w.Body)
	}
	w := call(c.settings, "GET", "portal", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "never-return-this") || strings.Contains(w.Body.String(), "new-secret") || !strings.Contains(w.Body.String(), `["AUTH_SECRET","DB_MIGRATE","NEW","SECRET"]`) {
		t.Fatal("settings keys", w.Code, w.Body)
	}
	// Confirm credentials and metadata survive a restart, independent of the seed file.
	c2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.servers()) != 1 || c2.servers()[0].Token != key {
		t.Fatal("server not persisted")
	}
	a, _ := c2.app("portal")
	if a.Branch != "release" || a.Domain != "new.example.com" {
		t.Fatal("settings not persisted")
	}
	if a.Previews.Env["API_TOKEN"] != previewSecret {
		t.Fatal("response filtering removed stored preview secret")
	}
	data, err := os.ReadFile(c.path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), key) || strings.Contains(string(data), "portal") {
		t.Fatal("control state is not encrypted")
	}
	a.AutoDeploy = true
	a.Secret = "webhook-secret"
	c.data.Apps["portal"] = a
	if w := call(c.disableHook, "DELETE", "portal", ""); w.Code != 200 || strings.Contains(w.Body.String(), "webhook-secret") || strings.Contains(w.Body.String(), previewSecret) {
		t.Fatal("disable", w.Code, w.Body)
	}
	// Disabling also cancels pending dispatch, even with no auth services in the context.
	if err := c.dispatchPush(context.Background(), json.RawMessage(`{"app_id":"portal","delivery":"queued"}`)); err != nil {
		t.Fatal(err)
	}
	c2, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, _ = c2.app("portal")
	if a.AutoDeploy {
		t.Fatal("disabled flag not persisted")
	}
	if a.Previews.Env["API_TOKEN"] != previewSecret {
		t.Fatal("disabling webhook removed stored preview secret")
	}
	if w := call(c.retire, "DELETE", "portal", `{"confirm":"wrong"}`); w.Code != 400 {
		t.Fatal("confirmation ignored")
	}
	if w := call(c.retire, "DELETE", "portal", `{"confirm":"portal"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if len(manager.Apps()) != 0 {
		t.Fatal("remote app not removed")
	}
	if _, ok := c.app("portal"); ok {
		t.Fatal("control app not removed")
	}
	if w := call(c.retire, "DELETE", "portal", `{"confirm":"portal"}`); w.Code != 200 {
		t.Fatal("retry failed", w.Code)
	}
	if w := call(c.removeServer, "DELETE", "one", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	cfg.Servers = []Server{{ID: "seed", URL: remote.URL, Token: key}}
	c2, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.servers()) != 0 {
		t.Fatal("deleted registry reimported seed")
	}
}

func TestPartialFleetHistoryAndStalePush(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":"release","app_id":"portal","status":"live"}]`))
	}))
	defer healthy.Close()
	offline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer offline.Close()
	cfg := Config{PublicURL: "http://127.0.0.1:3000", User: "operator@example.com", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(), Servers: []Server{{ID: "healthy", URL: healthy.URL, Token: strings.Repeat("a", 32)}, {ID: "offline", URL: offline.URL, Token: strings.Repeat("b", 32)}}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c.deployments(w, httptest.NewRequest("GET", "/", nil))
	var out struct {
		Deployments []struct {
			ID       string `json:"id"`
			ServerID string `json:"server_id"`
		}
		Unavailable []string `json:"unavailable_servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(out.Deployments) != 1 || out.Deployments[0].ServerID != "healthy" || len(out.Unavailable) != 1 || out.Unavailable[0] != "offline" {
		t.Fatal("partial history lost", w.Body)
	}
	c.data.Apps["portal"] = Application{ID: "portal", Generation: "new-instance", AutoDeploy: true}
	// No service context: a stale job must exit before attempting to obtain GitHub credentials.
	if err := c.dispatchPush(context.Background(), json.RawMessage(`{"app_id":"portal","generation":"deleted-instance","delivery":"old"}`)); err != nil {
		t.Fatal("stale job not ignored", err)
	}
}
