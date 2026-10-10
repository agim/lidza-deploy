package control

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestSignedSelfUpdateUsesVerifiedReleaseAndLocalHelper(t *testing.T) {
	secret := strings.Repeat("s", 64)
	queued := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 32) {
			t.Error("missing agent authentication")
		}
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(agent.UpgradeStatus{Current: "v0.2.12", Latest: "v0.2.14", Supported: true})
			return
		}
		var value struct {
			Version string `json:"version"`
		}
		json.NewDecoder(r.Body).Decode(&value)
		if value.Version != "v0.2.14" {
			t.Error("incorrect release queued")
		}
		queued++
		w.WriteHeader(202)
	}))
	defer remote.Close()
	c, err := New(Config{User: "operator@example.com", Password: strings.Repeat("p", 20), PublicURL: "https://deploy.example.com", DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32)), SelfUpdateSecret: secret, SelfUpdateServer: "local", Servers: []Server{{ID: "local", Name: "Local", URL: remote.URL, Token: strings.Repeat("a", 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"action":"published","repository":{"full_name":"agim/lidza-deploy"},"release":{"tag_name":"v0.2.14"}}`
	call := func(body, event string, signed bool) int {
		r := httptest.NewRequest("POST", "/hooks/self-update", strings.NewReader(body))
		r.Header.Set("X-GitHub-Event", event)
		if signed {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(body))
			r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		w := httptest.NewRecorder()
		c.selfUpdate(w, r)
		return w.Code
	}
	if code := call(payload, "release", false); code != 401 {
		t.Fatal("unsigned update accepted", code)
	}
	if code := call(payload, "release", true); code != 202 || queued != 1 {
		t.Fatal("signed update not queued", code, queued)
	}
	for _, body := range []string{strings.ReplaceAll(payload, "agim/lidza-deploy", "other/repo"), strings.ReplaceAll(payload, "v0.2.14", "v0.2.11"), strings.ReplaceAll(payload, "v0.2.14", "v0.2.15"), strings.ReplaceAll(payload, `"tag_name"`, `"prerelease":true,"tag_name"`)} {
		if code := call(body, "release", true); code != 200 || queued != 1 {
			t.Fatal("unapproved release queued", code, queued)
		}
	}
	if code := call(`{}`, "ping", true); code != 200 {
		t.Fatal("ping failed", code)
	}
	c.cfg.SelfUpdateSecret = ""
	if code := call(payload, "release", true); code != 404 {
		t.Fatal("unconfigured webhook active", code)
	}
}
