package control

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The control panel relays plugin requests to the server's agent unchanged:
// the agent verifies and installs; the panel adds nothing and keeps nothing.
func TestServerPluginsAreRelayedToTheAgent(t *testing.T) {
	token := strings.Repeat("a", 32)
	var got []string
	var archive []byte
	old := false
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("agent token not forwarded")
		}
		if old {
			http.NotFound(w, r) // an agent from before plugins
			return
		}
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/plugins":
			io.WriteString(w, `{"plugins":[{"name":"example","version":"1.0"}],"keys":["0123456789abcdef"],"pinned":0,"arch":"amd64"}`)
		case "PUT /v1/plugins/keys":
			io.WriteString(w, `{"plugins":[],"keys":["0123456789abcdef"]}`)
		case "POST /v1/plugins/manifest":
			if !bytes.Contains(body, []byte(`"manifest":"{}"`)) {
				t.Errorf("manifest body %s", body)
			}
			io.WriteString(w, `{"name":"example","version":"1.0","arch":"amd64"}`)
		case "PUT /v1/plugins/example/archive":
			if r.Header.Get("Content-Type") != "application/octet-stream" || r.ContentLength != int64(len(body)) {
				t.Errorf("archive headers %v %d", r.Header, r.ContentLength)
			}
			archive = body
			w.WriteHeader(201)
			io.WriteString(w, `{"name":"example","version":"1.0"}`)
		case "POST /v1/plugins/example/hooks/status":
			io.WriteString(w, `{"answer":{"ok":true}}`)
		case "DELETE /v1/plugins/example":
			io.WriteString(w, `{"status":"removed"}`)
		default:
			w.WriteHeader(409)
			io.WriteString(w, `{"error":"the plugin was signed by a key this server does not trust"}`)
		}
	}))
	defer agentServer.Close()
	c, err := New(Config{PublicURL: "https://deploy.example.com", User: "operator@example.com", Password: strings.Repeat("p", 20), Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(),
		Servers: []Server{{ID: "edge", Name: "Edge", URL: agentServer.URL, Token: token}}})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, name, hook, contentType string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.SetPathValue("server", "edge")
		r.SetPathValue("name", name)
		r.SetPathValue("hook", hook)
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		w := httptest.NewRecorder()
		c.serverPlugins(w, r)
		return w
	}
	if w := do("GET", "/api/control/servers/edge/plugins", "", "", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"example"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if w := do("PUT", "/api/control/servers/edge/plugins/keys", "", "", "application/json", []byte(`{"keys":["`+strings.Repeat("ab", 32)+`"]}`)); w.Code != 200 {
		t.Fatalf("keys: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/api/control/servers/edge/plugins/manifest", "", "", "application/json", []byte(`{"manifest":"{}","signature":{}}`)); w.Code != 200 {
		t.Fatalf("manifest: %d %s", w.Code, w.Body)
	}
	payload := bytes.Repeat([]byte("plugin-archive-bytes"), 4096)
	if w := do("PUT", "/api/control/servers/edge/plugins/example/archive", "example", "", "application/octet-stream", payload); w.Code != 201 || !bytes.Equal(archive, payload) {
		t.Fatalf("archive: %d %s (%d bytes relayed)", w.Code, w.Body, len(archive))
	}
	if w := do("PUT", "/api/control/servers/edge/plugins/example/archive", "example", "", "application/json", payload); w.Code != 400 {
		t.Fatalf("archive with another content type: %d", w.Code)
	}
	if w := do("POST", "/api/control/servers/edge/plugins/example/hooks/status", "example", "status", "application/json", []byte(`{}`)); w.Code != 200 {
		t.Fatalf("status hook: %d %s", w.Code, w.Body)
	}
	// Hooks that change a server are never run on request, and never reach the agent.
	before := len(got)
	if w := do("POST", "/api/control/servers/edge/plugins/example/hooks/provision", "example", "provision", "application/json", []byte(`{}`)); w.Code != 400 || len(got) != before {
		t.Fatalf("changing hook: %d, agent calls %v", w.Code, got[before:])
	}
	if w := do("DELETE", "/api/control/servers/edge/plugins/../etc", "../etc", "", "", nil); w.Code != 400 || len(got) != before {
		t.Fatalf("invalid name: %d", w.Code)
	}
	if w := do("DELETE", "/api/control/servers/edge/plugins/example", "example", "", "", nil); w.Code != 200 {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	// The agent's own refusal reaches the operator as it is.
	if w := do("POST", "/api/control/servers/edge/plugins/other/hooks/check", "other", "check", "application/json", []byte(`{}`)); w.Code != 409 || !strings.Contains(w.Body.String(), "does not trust") {
		t.Fatalf("agent refusal: %d %s", w.Code, w.Body)
	}
	// An agent from before plugins says so, not "404".
	old = true
	if w := do("GET", "/api/control/servers/edge/plugins", "", "", "", nil); w.Code != 404 || !strings.Contains(w.Body.String(), "has no plugins yet") {
		t.Fatalf("older agent: %d %s", w.Code, w.Body)
	}
}

// Installing from a release: the panel fetches the release's three files and
// relays them; what it fetched is exactly what the agent receives.
func TestPluginIsInstalledFromItsPublishedRelease(t *testing.T) {
	token := strings.Repeat("a", 32)
	var manifestSent, archiveSent []byte
	refuse := false
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/plugins":
			io.WriteString(w, `{"plugins":[],"keys":[],"arch":"arm64"}`)
		case "POST /v1/plugins/manifest":
			if refuse {
				w.WriteHeader(409)
				io.WriteString(w, `{"error":"the plugin was signed by a key this server does not trust"}`)
				return
			}
			manifestSent = body
			io.WriteString(w, `{"name":"example","version":"1.2","arch":"arm64"}`)
		case "PUT /v1/plugins/example/archive":
			archiveSent = body
			w.WriteHeader(201)
			io.WriteString(w, `{"name":"example","version":"1.2","provides":"example"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer agentServer.Close()
	c, err := New(Config{PublicURL: "https://deploy.example.com", User: "operator@example.com", Password: strings.Repeat("p", 20), Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(),
		Servers: []Server{{ID: "edge", Name: "Edge", URL: agentServer.URL, Token: token}}})
	if err != nil {
		t.Fatal(err)
	}
	release := map[string]string{
		"https://github.com/acme/deploy-plugin-example/releases/download/v1.2/plugin.json":         `{"contract":1,"name":"example"}` + "\n",
		"https://github.com/acme/deploy-plugin-example/releases/download/v1.2/plugin.sig":          `{"key_id":"0123456789abcdef","algorithm":"ed25519","signature":"c2ln"}` + "\n",
		"https://github.com/acme/deploy-plugin-example/releases/download/v1.2/plugin-arm64.tar.gz": strings.Repeat("archive-bytes", 9000),
	}
	var fetched []string
	saved := pluginReleaseFetch
	t.Cleanup(func() { pluginReleaseFetch = saved })
	pluginReleaseFetch = func(_ context.Context, url string, limit int64, w io.Writer) (int64, error) {
		fetched = append(fetched, url)
		body, ok := release[url]
		if !ok {
			return 0, errors.New("not found")
		}
		n, err := io.WriteString(w, body)
		return int64(n), err
	}
	install := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/control/servers/edge/plugins/install", strings.NewReader(body))
		r.SetPathValue("server", "edge")
		w := httptest.NewRecorder()
		c.serverPlugins(w, r)
		return w
	}
	for _, bad := range []string{`{"repository":"not a repo","version":"1.2"}`, `{"repository":"acme/../etc","version":"1.2"}`, `{"repository":"acme/deploy-plugin-example","version":"latest"}`} {
		if w := install(bad); w.Code != 400 || len(fetched) != 0 {
			t.Fatalf("invalid request %s: %d %s (fetched %v)", bad, w.Code, w.Body, fetched)
		}
	}
	w := install(`{"repository":"acme/deploy-plugin-example","version":"1.2"}`)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"provides":"example"`) {
		t.Fatalf("install: %d %s", w.Code, w.Body)
	}
	// The agent gets the manifest's exact text (without the file's final newline) and the server's own architecture's archive.
	if !bytes.Contains(manifestSent, []byte(`"manifest":"{\"contract\":1,\"name\":\"example\"}"`)) || !bytes.Contains(manifestSent, []byte(`"key_id":"0123456789abcdef"`)) {
		t.Fatalf("manifest relayed: %s", manifestSent)
	}
	if string(archiveSent) != release["https://github.com/acme/deploy-plugin-example/releases/download/v1.2/plugin-arm64.tar.gz"] {
		t.Fatalf("archive relayed: %d bytes", len(archiveSent))
	}
	// The agent's refusal of the signature reaches the operator, and no archive is fetched.
	refuse, fetched = true, nil
	if w := install(`{"repository":"acme/deploy-plugin-example","version":"1.2"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "does not trust") || len(fetched) != 2 {
		t.Fatalf("untrusted: %d %s (fetched %v)", w.Code, w.Body, fetched)
	}
	// A release without the files says which file is missing.
	refuse = false
	if w := install(`{"repository":"acme/deploy-plugin-example","version":"9.9"}`); w.Code != 502 || !strings.Contains(w.Body.String(), "has no plugin.json") {
		t.Fatalf("missing release: %d %s", w.Code, w.Body)
	}
}
