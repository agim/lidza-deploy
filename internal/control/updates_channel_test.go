package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	gh "github.com/agim/lidza-deploy/internal/providers/github"
	"github.com/agim/lidza-deploy/internal/updates"
)

type channelFixture struct {
	control    *Control
	github     *httptest.Server
	key        ed25519.PrivateKey
	archive    []byte
	manifest   []byte
	signature  []byte
	version    string
	releases   func() []map[string]any
	agents     *fakeAgents
	mu         sync.Mutex
	tamper     bool
	objectAuth []string
}

type fakeAgents struct {
	mu        sync.Mutex
	states    map[string]agent.UpgradeStatus
	manifests map[string][]byte
	archives  map[string][]byte
	queued    []string
}

func signedRelease(t *testing.T, repository, version string, key ed25519.PrivateKey, archive []byte) (manifest, signature []byte) {
	t.Helper()
	sum := sha256.Sum256(archive)
	m := updates.Manifest{Schema: 1, Channel: "private", Repository: repository, Version: version, Commit: strings.Repeat("c", 40), BuiltAt: time.Now().UTC(),
		Assets: []updates.Asset{{Name: updates.ArchiveName("amd64"), Arch: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive))}}}
	manifest, _ = json.Marshal(m)
	signature, _ = json.Marshal(updates.Sign(key, manifest))
	return manifest, signature
}

func newChannelFixture(t *testing.T) *channelFixture {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	saved, savedOwner := channelTrust, channelOwner
	channelTrust = func() updates.Trust { return updates.Trust{Keys: []ed25519.PublicKey{pub}} }
	channelOwner = func(*Control, *http.Request) bool { return true }
	t.Cleanup(func() { channelTrust, channelOwner = saved, savedOwner })
	f := &channelFixture{key: priv, version: "v0.9.0-private.1", archive: []byte("private bundle archive")}
	f.manifest, f.signature = signedRelease(t, "acme/deploy-private", f.version, priv, f.archive)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app := gh.App{ID: 42, Slug: "fixture", PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}))}
	f.releases = func() []map[string]any {
		return []map[string]any{
			{"id": 10, "tag_name": "v0.9.1", "draft": false, "prerelease": false, "assets": []map[string]any{{"id": 100, "name": "lidza-agent-linux-amd64.tar.gz"}}},
			{"id": 11, "tag_name": "v0.9.5-private.1", "draft": true, "prerelease": false, "assets": []map[string]any{{"id": 110, "name": "manifest.json"}, {"id": 111, "name": "manifest.sig"}, {"id": 112, "name": "lidza-agent-linux-amd64.tar.gz"}}},
			{"id": 12, "tag_name": f.version, "draft": false, "prerelease": false, "assets": []map[string]any{{"id": 1, "name": "manifest.json"}, {"id": 2, "name": "manifest.sig"}, {"id": 3, "name": "lidza-agent-linux-amd64.tar.gz"}}},
		}
	}
	f.github = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/acme/deploy-private/installation":
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "app_id": 42, "account": map[string]string{"login": "acme"}})
		case r.URL.Path == "/repos/acme/other/installation":
			w.WriteHeader(404)
		case r.URL.Path == "/app/installations/7":
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "app_id": 42, "account": map[string]string{"login": "acme"}, "permissions": map[string]string{"contents": "read", "pull_requests": "read"}})
		case r.URL.Path == "/app/installations/7/access_tokens":
			json.NewEncoder(w).Encode(map[string]any{"token": "ghs_fixture", "expires_at": time.Now().Add(time.Hour)})
		case r.URL.Path == "/repos/acme/deploy-private/releases":
			if r.Header.Get("Authorization") != "Bearer ghs_fixture" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(f.releases())
		case strings.HasPrefix(r.URL.Path, "/repos/acme/deploy-private/releases/assets/"):
			if r.Header.Get("Authorization") != "Bearer ghs_fixture" || r.Header.Get("Accept") != "application/octet-stream" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Location", f.github.URL+"/objects/"+strings.TrimPrefix(r.URL.Path, "/repos/acme/deploy-private/releases/assets/"))
			w.WriteHeader(302)
		case strings.HasPrefix(r.URL.Path, "/objects/"):
			f.mu.Lock()
			f.objectAuth = append(f.objectAuth, r.Header.Get("Authorization"))
			tamper := f.tamper
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/octet-stream")
			switch strings.TrimPrefix(r.URL.Path, "/objects/") {
			case "1":
				w.Write(f.manifest)
			case "2":
				if tamper {
					var sig updates.Signature
					json.Unmarshal(f.signature, &sig)
					sig.Signature = strings.Repeat("A", len(sig.Signature))
					json.NewEncoder(w).Encode(sig)
					return
				}
				w.Write(f.signature)
			case "3":
				w.Write(f.archive)
			default:
				w.WriteHeader(404)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.github.Close)
	token := strings.Repeat("a", 32)
	cfg := Config{PublicURL: "https://deploy.example", User: "operator@example.com", Password: strings.Repeat("p", 20), DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32)),
		Servers: []Server{{ID: "local", Name: "GUI", URL: "http://127.0.0.1:9090", Token: token}, {ID: "remote", Name: "Remote", URL: "https://agent.example", Token: token}},
		GitHub:  &gh.Client{API: f.github.URL, HTTP: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.data.GitHubApp = &githubAppConfig{App: app, Installations: map[int64]string{7: "acme"}}
	f.control = c
	f.agents = &fakeAgents{states: map[string]agent.UpgradeStatus{
		"local":  {Current: "preview-fd846a6", State: "idle", Supported: true, Channel: "private", Arch: "amd64"},
		"remote": {Current: "preview-fd846a6", State: "idle", Supported: true, Channel: "private", Arch: "amd64"},
	}, manifests: map[string][]byte{}, archives: map[string][]byte{}}
	agents := f.agents
	c.client = &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("agent token not forwarded")
		}
		id := "remote"
		if strings.HasPrefix(r.URL.Host, "127.") {
			id = "local"
		}
		agents.mu.Lock()
		defer agents.mu.Unlock()
		code, out := 200, any(agents.states[id])
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/upgrade/manifest":
			var in struct {
				Manifest  string            `json:"manifest"`
				Signature updates.Signature `json:"signature"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			if _, _, err := channelTrust().Verify([]byte(in.Manifest), mustJSON(in.Signature)); err != nil {
				code, out = 409, map[string]string{"error": err.Error()}
				break
			}
			agents.manifests[id] = []byte(in.Manifest)
			out = map[string]string{"version": f.version, "arch": "amd64"}
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/v1/upgrade/bundle/"):
			body, _ := io.ReadAll(r.Body)
			if agents.manifests[id] == nil || r.Header.Get("Content-Type") != "application/octet-stream" || r.ContentLength != int64(len(body)) {
				code, out = 409, map[string]string{"error": "manifest first"}
				break
			}
			agents.archives[id] = body
			out = map[string]any{"verified": true}
		case r.Method == "POST" && r.URL.Path == "/v1/upgrade":
			var in struct{ Version string }
			json.NewDecoder(r.Body).Decode(&in)
			if string(agents.archives[id]) != string(f.archive) || in.Version != f.version {
				code, out = 409, map[string]string{"error": "bundle not staged"}
				break
			}
			agents.queued = append(agents.queued, id)
			status := agents.states[id]
			status.State = "queued"
			agents.states[id] = status
			code, out = 202, map[string]string{"status": "queued"}
		}
		body, _ := json.Marshal(out)
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	return f
}

func (f *channelFixture) post(t *testing.T, action, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/control/updates/"+action, strings.NewReader(body))
	r.SetPathValue("action", action)
	w := httptest.NewRecorder()
	f.control.updates(w, r)
	return w
}

func TestSignedChannelVerifiesSignedReleasesAndStagesBundlesBeforeUpgrade(t *testing.T) {
	f := newChannelFixture(t)
	c := f.control
	// Stale public information from before the private build is never shown.
	c.data.Updates.Latest = "v0.2.105"
	w := httptest.NewRecorder()
	c.updates(w, httptest.NewRequest("GET", "/api/control/updates", nil))
	var view struct {
		Settings UpdateSettings `json:"settings"`
		Channel  map[string]any `json:"channel"`
		Auto     bool           `json:"automatic_updates_supported"`
	}
	json.NewDecoder(w.Body).Decode(&view)
	if view.Settings.Latest != "" || view.Auto || view.Channel["configured"] != false || view.Channel["keys_pinned"] != true {
		t.Fatalf("unconfigured channel view %+v", view)
	}
	if w := f.post(t, "channel", `{"repository":"acme/other"}`); w.Code != 409 {
		t.Fatal("repository outside the GitHub App accepted", w.Code, w.Body)
	}
	if w := f.post(t, "channel", `{"repository":"acme/deploy-private"}`); w.Code != 200 {
		t.Fatal("channel configuration failed", w.Code, w.Body)
	}
	settings := c.updateSettingsSnapshot()
	if settings.Channel == nil || settings.Channel.Installation != 7 || settings.Latest != f.version || settings.Bundle == nil || settings.Bundle.Version != f.version || settings.CheckError != "" {
		t.Fatalf("channel check %+v", settings)
	}
	for _, auth := range f.objectAuth {
		if auth != "" {
			t.Fatal("installation token leaked to asset storage")
		}
	}
	// Public v0.9.1 and the draft private release were ignored.
	if settings.Latest == "v0.9.1" || settings.Latest == "v0.9.5-private.1" {
		t.Fatal("wrong release selected")
	}
	if w := f.post(t, "install", `{}`); w.Code != 202 {
		t.Fatal("install refused", w.Code, w.Body)
	}
	tick := func() {
		t.Helper()
		if err := c.updateTick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	f.agents.mu.Lock()
	queued := append([]string{}, f.agents.queued...)
	remoteArchive := f.agents.archives["remote"]
	f.agents.mu.Unlock()
	if len(queued) != 1 || queued[0] != "remote" || string(remoteArchive) != string(f.archive) {
		t.Fatal("remote agent did not receive the verified bundle before its upgrade", queued)
	}
	targets := c.updateSettingsSnapshot().Targets
	if targets[0].ServerID != "remote" || targets[0].State != "updating" {
		t.Fatalf("targets %+v", targets)
	}
	// Remote finishes; the panel host follows with its own transfer.
	f.agents.mu.Lock()
	f.agents.states["remote"] = agent.UpgradeStatus{Current: f.version, State: "succeeded", Supported: true, Channel: "private", Arch: "amd64"}
	f.agents.mu.Unlock()
	tick()
	tick()
	f.agents.mu.Lock()
	queued = append([]string{}, f.agents.queued...)
	f.agents.mu.Unlock()
	if len(queued) != 2 || queued[1] != "local" {
		t.Fatal("panel host was not queued after the remote host", queued)
	}
	if _, err := c.cachedArchive(context.Background(), *settings.Bundle, "arm64"); err == nil {
		t.Fatal("missing architecture accepted")
	}
	// Automatic updates may now be enabled.
	w = httptest.NewRecorder()
	c.updates(w, httptest.NewRequest("PATCH", "/api/control/updates", strings.NewReader(`{"automatic":true}`)))
	if w.Code != 200 {
		t.Fatal("automatic updates refused on a configured channel", w.Code, w.Body)
	}
}

func TestSignedChannelRefusesBadSignaturesForeignManifestsAndPublicPlans(t *testing.T) {
	f := newChannelFixture(t)
	c := f.control
	if w := f.post(t, "channel", `{"repository":"acme/deploy-private"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	f.mu.Lock()
	f.tamper = true
	f.mu.Unlock()
	if w := f.post(t, "check", `{}`); w.Code != 502 || !strings.Contains(w.Body.String(), "refused") {
		t.Fatal("tampered signature accepted", w.Code, w.Body)
	}
	if c.updateSettingsSnapshot().Latest != f.version {
		t.Fatal("a failed check must not forget the last verified release")
	}
	f.mu.Lock()
	f.tamper = false
	f.manifest, f.signature = signedRelease(t, "someone/else", f.version, f.key, f.archive)
	f.mu.Unlock()
	if w := f.post(t, "check", `{}`); w.Code != 502 || !strings.Contains(w.Body.String(), "names someone/else") {
		t.Fatal("manifest for another repository accepted", w.Code, w.Body)
	}
	// A retained public plan is never dispatched on the private channel.
	v := c.updateSettingsSnapshot()
	v.Version, v.Targets = "v0.9.1", []UpdateTarget{{ServerID: "remote", State: "waiting"}}
	c.saveUpdateSettings(v)
	if err := c.updateTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.agents.mu.Lock()
	queued := len(f.agents.queued)
	f.agents.mu.Unlock()
	if after := c.updateSettingsSnapshot().Targets[0]; after.State != "failed" || queued != 0 {
		t.Fatalf("public plan dispatched on the private channel: %+v queued=%d", after, queued)
	}
	// Clearing the channel disables automatic updates again.
	if w := f.post(t, "channel", `{"repository":""}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w := httptest.NewRecorder()
	c.updates(w, httptest.NewRequest("PATCH", "/api/control/updates", strings.NewReader(`{"automatic":true}`)))
	if w.Code != 409 {
		t.Fatal("automatic updates enabled without a channel", w.Code)
	}
	if calls := fmt.Sprint(len(f.objectAuth)); calls == "" {
		t.Fatal(calls)
	}
}

func TestSignedChannelPerServerUpgradeAndStaleFailedTarget(t *testing.T) {
	f := newChannelFixture(t)
	c := f.control
	if w := f.post(t, "channel", `{"repository":"acme/deploy-private"}`); w.Code != 200 {
		t.Fatal("channel configuration failed", w.Code, w.Body)
	}
	call := func(method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/control/servers/remote/upgrade", strings.NewReader(`{}`))
		r.SetPathValue("server", "remote")
		w := httptest.NewRecorder()
		c.upgradeServer(w, r)
		return w
	}
	w := call("GET")
	var view map[string]any
	json.NewDecoder(w.Body).Decode(&view)
	if w.Code != 200 || view["latest"] != f.version || view["channel"] != "private" || view["supported"] != true {
		t.Fatalf("per-server view %d %+v", w.Code, view)
	}
	if w := call("POST"); w.Code != 202 {
		t.Fatal("per-server private upgrade refused", w.Code, w.Body)
	}
	f.agents.mu.Lock()
	queued, archive := append([]string{}, f.agents.queued...), f.agents.archives["remote"]
	f.agents.states["remote"] = agent.UpgradeStatus{Current: f.version, State: "succeeded", Supported: true, Channel: "private", Arch: "amd64"}
	f.agents.mu.Unlock()
	if len(queued) != 1 || queued[0] != "remote" || string(archive) != string(f.archive) {
		t.Fatal("bundle not transferred before the per-server upgrade", queued)
	}
	if w := call("POST"); w.Code != 409 {
		t.Fatal("upgrade to the running release accepted", w.Code)
	}
	// A batch target that failed earlier no longer blocks once its host runs
	// the plan's release or a newer one.
	c.mu.Lock()
	c.data.Updates.Version = "v0.8.0-private.1"
	c.data.Updates.Targets = []UpdateTarget{{ServerID: "remote", Name: "Remote", State: "failed", Message: "old failure"},
		{ServerID: "local", Name: "GUI", State: "waiting"}}
	c.mu.Unlock()
	if err := c.updateTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.updateSettingsSnapshot().Targets[0]; got.State != "succeeded" {
		t.Fatalf("stale failed target kept blocking: %+v", got)
	}
}

func TestPublicBuildFollowsPublishedReleasesUntilKeysArePinned(t *testing.T) {
	saved := channelTrust
	t.Cleanup(func() { channelTrust = saved })
	c := &Control{}
	channelTrust = func() updates.Trust { return updates.Trust{} }
	if !c.updatesAvailable() || c.channelConfigured() {
		t.Fatal("a build without pinned keys must follow the published releases")
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	channelTrust = func() updates.Trust { return updates.Trust{Keys: []ed25519.PublicKey{pub}} }
	if c.updatesAvailable() {
		t.Fatal("a signed build without a configured channel offered public releases")
	}
}
