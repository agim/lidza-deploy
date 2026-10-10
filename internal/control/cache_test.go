package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestCacheConnectionProxyIsWriteOnly(t *testing.T) {
	secretURL := "rediss://:cache-private-secret@cache.example.com:6379/0"
	key := strings.Repeat("a", 32)
	var received agent.CacheRequest
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key || r.URL.Path != "/v1/apps/portal/cache" {
			t.Error("cache proxy missing authentication or wrong app")
		}
		if r.Method == "POST" {
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Error(err)
			}
			w.WriteHeader(202)
			json.NewEncoder(w).Encode(map[string]string{"status": "provisioning"})
			return
		}
		// Even additional fields from an agent are excluded from browser responses.
		json.NewEncoder(w).Encode(map[string]any{"mode": "external", "ready": true, "managed": true, "url": secretURL, "local_password": "private-password"})
	}))
	defer remote.Close()
	c, err := New(Config{PublicURL: "http://127.0.0.1:3000", User: "operator@example.com", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(), Servers: []Server{{ID: "one", URL: remote.URL, Token: key}}})
	if err != nil {
		t.Fatal(err)
	}
	c.data.Apps["portal"] = Application{ID: "portal", ServerID: "one"}
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/", strings.NewReader(body))
		r.SetPathValue("id", "portal")
		w := httptest.NewRecorder()
		c.cacheSettings(w, r)
		return w
	}
	body, _ := json.Marshal(agent.CacheRequest{Mode: "external", URL: secretURL})
	if w := call("POST", string(body)); w.Code != 202 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("cache proxy POST", w.Code, w.Body)
	}
	if received.Mode != "external" || received.URL != secretURL {
		t.Fatal("cache credentials did not reach authenticated hosting agent")
	}
	if w := call("GET", ""); w.Code != 200 || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "url") {
		t.Fatal("cache view disclosed connection", w.Code, w.Body)
	}
}
