package control

import (
	"bytes"
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
