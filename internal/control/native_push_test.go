package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func exerciseNativePushDispatch(t *testing.T, c *Control, ctx context.Context) {
	t.Helper()
	n := agent.NativeApplication{ID: "astro-site", Name: "site", Runtime: "astro", DocumentRoot: "public", Repository: "org/site", Branch: "main", Domain: "site.example.com", HealthPath: "/", Services: []string{}}
	posts, reads := 0, 0
	approved := false
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/native/apps/astro-site":
			reads++
			json.NewEncoder(w).Encode(agent.NativeStatus{Application: n, DeploymentSupported: approved})
		case "POST /v1/native/apps/astro-site/deploy":
			posts++
			var request agent.DeployRequest
			json.NewDecoder(r.Body).Decode(&request)
			if request.Key != "github:delivery" || request.Token != "private-fixture-token" {
				t.Error("native push omitted idempotency or framework credentials")
			}
			json.NewEncoder(w).Encode(agent.Deployment{ID: "native-job"})
		default:
			t.Error("native push used a container, storage, service or routing API", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	oldRegistry := c.servers()
	c.registryMu.Lock()
	c.registry = append(c.registry, Server{ID: "static-fixture", URL: remote.URL, Token: "fixture-agent-token"})
	c.registryMu.Unlock()
	a := Application{ID: n.ID, Native: &n, ServerID: "static-fixture", Repository: n.Repository, Branch: n.Branch, Domain: n.Domain, Generation: "native-generation", AutoDeploy: true}
	c.mu.Lock()
	c.data.Apps[a.ID] = a
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.data.Apps, a.ID)
		c.mu.Unlock()
		c.registryMu.Lock()
		c.registry = oldRegistry
		c.registryMu.Unlock()
	}()
	raw, _ := json.Marshal(pushJob{AppID: a.ID, Generation: a.Generation, Delivery: "delivery"})
	if err := c.dispatchPush(ctx, raw); err != nil || posts != 0 {
		t.Fatal("unapproved push performed publication", err)
	}
	approved = true
	if err := c.dispatchPush(ctx, raw); err != nil || posts != 1 {
		t.Fatal("approved push did not use native publication", err)
	}
	before := reads
	n.Runtime = "rails"
	c.mu.Lock()
	a.Native = &n
	c.data.Apps[a.ID] = a
	c.mu.Unlock()
	if err := c.dispatchPush(ctx, raw); err != nil || posts != 1 || reads != before {
		t.Fatal("Rails push attempted an implicit service action", err)
	}
}
