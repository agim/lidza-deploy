package control

import (
	"context"
	"encoding/json"
	"github.com/agim/lidza-deploy/internal/agent"
	gh "github.com/agim/lidza-deploy/internal/providers/github"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func exercisePreviews(t *testing.T, c *Control, ctx context.Context) {
	t.Helper()
	var mu sync.Mutex
	remoteApps := map[string]agent.App{}
	deploys := 0
	removes := 0
	prState := "open"
	var receivedEnv map[string]string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/apps" && r.Method == "GET":
			out := []agent.App{}
			for _, a := range remoteApps {
				out = append(out, a)
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/deploy"):
			deploys++
			json.NewEncoder(w).Encode(agent.Deployment{ID: "preview-job"})
		case strings.HasPrefix(r.URL.Path, "/v1/previews/"):
			removes++
			delete(remoteApps, strings.TrimPrefix(r.URL.Path, "/v1/previews/"))
			w.Write([]byte(`{}`))
		case r.Method == "PUT":
			var a agent.App
			if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
				t.Error(err)
			}
			if a.Env != nil {
				receivedEnv = a.Env
			}
			remoteApps[a.ID] = a
			w.Write([]byte(`{}`))
		default:
			t.Error("unexpected preview agent request", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/repos/acme/portal/installation" {
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "app_id": 42})
			return
		}
		if r.URL.Path == "/app/installations/7" {
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "app_id": 42, "permissions": map[string]string{"contents": "read", "pull_requests": "read"}})
			return
		}
		if r.URL.Path == "/app/installations/7/access_tokens" {
			json.NewEncoder(w).Encode(map[string]any{"token": "preview-installation-token", "expires_at": time.Now().Add(time.Hour)})
			return
		}
		if r.URL.Path != "/repos/acme/portal/pulls/7" {
			t.Error(r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"state": prState, "base": map[string]string{"ref": "main"}})
	}))
	defer github.Close()
	oldGH := c.cfg.GitHub
	oldServers := c.servers()
	defer func() { c.cfg.GitHub = oldGH; c.registryMu.Lock(); c.registry = oldServers; c.registryMu.Unlock() }()
	c.cfg.GitHub = &gh.Client{API: github.URL}
	c.registryMu.Lock()
	c.registry = []Server{{ID: "one", URL: remote.URL, Token: strings.Repeat("a", 32)}}
	c.registryMu.Unlock()
	c.mu.Lock()
	parent := c.data.Apps["portal"]
	parent.Repository = "acme/portal"
	parent.Branch = "main"
	parent.Previews = PreviewConfig{Enabled: true, BaseDomain: "preview.example.com", Env: map[string]string{"APP_MODE": "preview"}}
	c.data.Apps[parent.ID] = parent
	c.mu.Unlock()
	body, _ := json.Marshal(previewJob{AppID: parent.ID, Number: 7, Action: "opened", Delivery: "first", Generation: parent.Generation})
	if err := c.dispatchPreview(ctx, body); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	a := remoteApps[previewID(parent.ID, 7)]
	if deploys != 1 || !a.Preview || a.PullRequest != 7 || receivedEnv["APP_MODE"] != "preview" || receivedEnv["DATABASE_URL"] != "" || receivedEnv["APP_SECRET"] != "" {
		t.Fatal("preview isolation or dispatch incorrect")
	}
	prState = "closed"
	mu.Unlock()
	// A late opening/synchronize job must clean up a currently closed PR, not redeploy it.
	if err := c.dispatchPreview(ctx, body); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if deploys != 1 || removes != 1 || len(remoteApps) != 0 {
		t.Fatal("late event resurrected closed PR")
	}
	mu.Unlock()
	if _, ok := c.app(previewID(parent.ID, 7)); ok {
		t.Fatal("closed preview metadata remained")
	}
}
