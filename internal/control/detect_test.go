package control

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	gh "github.com/agim/lidza-deploy/internal/providers/github"
)

// detectFixture serves a GitHub App installation that can read acme/shop, a
// Rails application with Sidekiq on branch main.
func detectFixture(t *testing.T, truncated bool) (*Control, *atomic.Int32) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app := gh.App{ID: 42, Slug: "deploy", PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))}
	files := map[string]string{
		"Gemfile":               "gem 'rails'\ngem 'pg'\ngem 'sidekiq'\n",
		"Gemfile.lock":          "GEM\n  specs:\n    pg (1.5.6)\n    rails (8.0.1)\n    sidekiq (7.3.0)\n",
		"config/application.rb": "module Shop; end\n",
		"bin/rails":             "",
	}
	reads := &atomic.Int32{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		encode := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		p := r.URL.Path
		if strings.HasPrefix(p, "/repos/acme/shop/") && p != "/repos/acme/shop/installation" && r.Header.Get("Authorization") != "Bearer detect-token" {
			t.Errorf("%s requested without the installation token", p)
		}
		switch {
		case p == "/repos/acme/shop/installation":
			encode(map[string]any{"id": 7, "app_id": 42})
		case p == "/app/installations/7":
			encode(map[string]any{"id": 7, "app_id": 42, "permissions": map[string]string{"contents": "read", "pull_requests": "read"}})
		case p == "/app/installations/7/access_tokens":
			encode(map[string]any{"token": "detect-token", "expires_at": time.Now().Add(time.Hour)})
		case p == "/repos/acme/shop":
			encode(map[string]string{"default_branch": "main"})
		case p == "/repos/acme/shop/branches":
			encode([]map[string]string{{"name": "main"}, {"name": "release/1"}})
		case p == "/repos/acme/shop/branches/main":
			encode(map[string]any{"commit": map[string]any{"sha": "abc123", "commit": map[string]any{"tree": map[string]string{"sha": "tree1"}}}})
		case p == "/repos/acme/shop/git/trees/tree1":
			if r.URL.Query().Get("recursive") != "1" {
				t.Error("tree must be listed recursively")
			}
			tree := []map[string]any{{"path": "config", "type": "tree"}, {"path": "bin", "type": "tree"}, {"path": "link", "type": "blob", "mode": "120000", "sha": "l"}}
			for name, content := range files {
				tree = append(tree, map[string]any{"path": name, "type": "blob", "mode": "100644", "sha": "sha-" + name, "size": len(content)})
			}
			encode(map[string]any{"tree": tree, "truncated": truncated})
		case strings.HasPrefix(p, "/repos/acme/shop/git/blobs/sha-"):
			reads.Add(1)
			content := files[strings.TrimPrefix(p, "/repos/acme/shop/git/blobs/sha-")]
			encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64"})
		case strings.HasPrefix(p, "/repos/acme/shop/contents"):
			if r.URL.Query().Get("ref") != "abc123" {
				t.Error("contents must be read at the resolved commit")
			}
			name := strings.TrimPrefix(strings.TrimPrefix(p, "/repos/acme/shop/contents"), "/")
			if content, ok := files[name]; ok {
				reads.Add(1)
				encode(map[string]any{"type": "file", "size": len(content), "content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64"})
				return
			}
			var entries []map[string]string
			seen := map[string]bool{}
			for file := range files {
				rest, ok := strings.CutPrefix(file, name)
				if name != "" {
					rest, ok = strings.CutPrefix(file, name+"/")
				}
				if !ok {
					continue
				}
				entry, kind := rest, "file"
				if i := strings.Index(rest, "/"); i >= 0 {
					entry, kind = rest[:i], "dir"
				}
				if !seen[entry] {
					seen[entry] = true
					entries = append(entries, map[string]string{"name": entry, "type": kind})
				}
			}
			if entries == nil {
				http.NotFound(w, r)
				return
			}
			encode(entries)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	c, err := New(Config{PublicURL: "https://deploy.example.com", User: "operator@example.com", Password: "fixture-password-16", DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32)), GitHub: &gh.Client{API: remote.URL}})
	if err != nil {
		t.Fatal(err)
	}
	c.data.GitHubApp = &githubAppConfig{App: app, Installations: map[int64]string{7: "acme"}}
	return c, reads
}

func TestRepositoryDetectionReadsTheBranchTree(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		c, reads := detectFixture(t, truncated)
		out, err := c.repositoryDetection(context.Background(), "acme/shop", "", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if out.Repository != "acme/shop" || out.Branch != "main" || out.DefaultBranch != "main" || out.Commit != "abc123" || out.Installation != 7 || len(out.Branches) != 2 {
			t.Fatalf("unexpected repository view %+v", out)
		}
		if out.Mode != "native" || out.Runtime != "rails" || !out.NeedsPostgres || !out.NeedsRedis || len(out.Processes) != 2 || out.Processes[1].Command != "bundle exec sidekiq" {
			t.Fatalf("unexpected detection (truncated=%v) %+v", truncated, out.Result)
		}
		before := reads.Load()
		if _, err := c.repositoryDetection(context.Background(), "acme/shop", "", "", 0); err != nil || reads.Load() != before {
			t.Fatal("a repeated detection within the cache window must not read the repository again", err)
		}
	}
}

func TestRepositoryDetectionReportsUnknownBranch(t *testing.T) {
	c, _ := detectFixture(t, false)
	if _, err := c.repositoryDetection(context.Background(), "acme/shop", "missing", "", 0); err == nil || !strings.Contains(err.Error(), "missing was not found") {
		t.Fatal("missing branch not reported", err)
	}
}

func TestValidateNativeCreate(t *testing.T) {
	valid := agent.NativeCreateRequest{ID: "shop", Repository: "acme/shop", Branch: "main", Runtime: "rails", Domain: "shop.example.com", HealthPath: "/up", Processes: []agent.NativeProcessSpec{{Name: "web", Command: "bin/rails server", Web: true}, {Name: "worker", Command: "bundle exec sidekiq"}}, Data: agent.NativeDataRequest{Postgres: true, Redis: true, BackupHours: 24}}
	if err := validateNativeCreate(valid); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*agent.NativeCreateRequest){
		"runtime":      func(r *agent.NativeCreateRequest) { r.Runtime = "php" },
		"id":           func(r *agent.NativeCreateRequest) { r.ID = "Shop" },
		"no web":       func(r *agent.NativeCreateRequest) { r.Processes[0].Web = false },
		"two webs":     func(r *agent.NativeCreateRequest) { r.Processes[1].Web = true },
		"duplicate":    func(r *agent.NativeCreateRequest) { r.Processes[1].Name = "web" },
		"multiline":    func(r *agent.NativeCreateRequest) { r.Processes[1].Command = "a\nb" },
		"health":       func(r *agent.NativeCreateRequest) { r.HealthPath = "http://evil/" },
		"both db":      func(r *agent.NativeCreateRequest) { r.Data.ExternalDatabaseURL = "postgres://db/app" },
		"database env": func(r *agent.NativeCreateRequest) { r.Env = map[string]string{"DATABASE_URL": "postgres://db/app"} },
		"source":       func(r *agent.NativeCreateRequest) { r.SourceDirectory = "../other" },
		"static procs": func(r *agent.NativeCreateRequest) { r.Runtime = "static" },
		"external url": func(r *agent.NativeCreateRequest) {
			r.Data.Postgres = false
			r.Data.ExternalDatabaseURL = "mysql://db/app"
		},
		"static output": func(r *agent.NativeCreateRequest) { r.Runtime, r.Processes, r.StaticOutput = "static", nil, "../dist" },
	}
	for name, mutate := range cases {
		r := valid
		r.Processes = append([]agent.NativeProcessSpec(nil), valid.Processes...)
		mutate(&r)
		if validateNativeCreate(r) == nil {
			t.Errorf("%s: invalid request accepted", name)
		}
	}
	static := agent.NativeCreateRequest{ID: "site", Repository: "acme/site", Branch: "main", Runtime: "static", Domain: "www.example.com", StaticOutput: "dist"}
	if err := validateNativeCreate(static); err != nil {
		t.Fatal(err)
	}
}

func TestCreateNativeForwardsToTheAgentAndRegisters(t *testing.T) {
	var received agent.NativeCreateRequest
	calls := []string{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		native := agent.NativeApplication{ID: "shop", Name: "shop", Runtime: "rails", Domain: "shop.example.com", Repository: "acme/shop", Branch: "main", HealthPath: "/up", Services: []string{"app-shop-web.service", "app-shop-worker.service"}}
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/native/apps":
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &received); err != nil {
				t.Error(err)
			}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(native)
		case "GET /v1/native/apps/shop":
			_ = json.NewEncoder(w).Encode(agent.NativeStatus{Application: native, ReleaseManaged: true, DeploymentSupported: true})
		case "POST /v1/native/apps/shop/deploy":
			_ = json.NewEncoder(w).Encode(agent.Deployment{ID: "d1", AppID: "shop", Status: "queued"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	c, err := New(Config{PublicURL: "https://deploy.example.com", User: "operator@example.com", Password: "fixture-password-16", DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32)), Servers: []Server{{ID: "one", URL: remote.URL, Token: strings.Repeat("a", 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	c.credentialsFixture = func(context.Context, Application, string) (agent.DeployRequest, error) {
		return agent.DeployRequest{Token: "checkout"}, nil
	}
	body := `{"server_id":"one","deploy":true,"id":"shop","repository":"acme/shop","branch":"main","runtime":"rails","domain":"shop.example.com","health_path":"/up","processes":[{"name":"web","command":"bin/rails server","web":true},{"name":"worker","command":"bundle exec sidekiq"}],"data":{"postgres":true,"redis":true,"backup_hours":24,"offsite":true},"env":{"RAILS_MASTER_KEY":"0123"},"env_kinds":{"RAILS_MASTER_KEY":"credential"}}`
	w := httptest.NewRecorder()
	c.createNative(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var out struct {
		Application
		Warning    string            `json:"warning"`
		Deployment *agent.Deployment `json:"deployment"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if received.ID != "shop" || received.Runtime != "rails" || len(received.Processes) != 2 || !received.Data.Postgres || !received.Data.Redis || received.Data.Offsite || received.Env["RAILS_MASTER_KEY"] != "0123" {
		t.Fatalf("agent received %+v", received)
	}
	if out.Deployment == nil || out.Deployment.ID != "d1" || !strings.Contains(out.Warning, "S3") {
		t.Fatalf("unexpected response %s", w.Body)
	}
	a := c.data.Apps["shop"]
	if a.Native == nil || len(a.Native.Services) != 2 || a.ServerID != "one" || a.Runtime != "rails" || a.Generation == "" {
		t.Fatalf("application not registered as native: %+v", a)
	}
	w = httptest.NewRecorder()
	c.createNative(w, httptest.NewRequest("POST", "/", strings.NewReader(strings.Replace(body, `"deploy":true`, `"deploy":false`, 1))))
	if w.Code != 409 || strings.Count(strings.Join(calls, ","), "POST /v1/native/apps,") != 1 {
		t.Fatal("a duplicate application must be refused before the agent is called", w.Code, calls)
	}
}
