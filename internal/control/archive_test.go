package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	gh "github.com/agim/lidza-deploy/internal/providers/github"
)

// archiveAgent adds the archive routes to a move fake (alpha).
type archiveAgent struct {
	mu       sync.Mutex
	archives map[string]agent.AppArchiveView
	purged   []agent.ArchivePurge
	requests []agent.AppArchiveRequest
	restores []agent.ArchiveRestoreRequest
}

func newArchiveFixture(t *testing.T) (*moveFixture, *archiveAgent) {
	t.Helper()
	fx := newMoveFixture(t)
	fake := &archiveAgent{archives: map[string]agent.AppArchiveView{}}
	fx.a.mux.HandleFunc("POST /v1/apps/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		var req agent.AppArchiveRequest
		json.NewDecoder(r.Body).Decode(&req)
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.requests = append(fake.requests, req)
		now := time.Now().UTC()
		view := agent.AppArchiveView{ID: "archive-1", AppID: r.PathValue("id"), Kind: "container", DeletedAt: now, ExpiresAt: now.Add(agent.AppArchiveRetention), Phase: "archived",
			DeletedBy: req.DeletedBy, Reason: req.Reason, Hostnames: []string{"shop.example.com", "www.shop.example.com"},
			Kept: []agent.ArchiveItem{{Kind: "settings", Name: "shop"}, {Kind: "volume", Name: "lidza-db-shop-data", SizeBytes: 2048, Measured: true}}, Deleted: []agent.ArchiveItem{{Kind: "backups", Name: "shop", Count: 3}}, IDAvailable: true}
		fake.archives[view.ID] = view
		agent.JSON(w, 200, view)
	})
	fx.a.mux.HandleFunc("GET /v1/archives", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		out := agent.AppArchivesView{Archives: []agent.AppArchiveView{}, Purged: slices.Clone(fake.purged)}
		for _, a := range fake.archives {
			out.Archives = append(out.Archives, a)
		}
		agent.JSON(w, 200, out)
	})
	fx.a.mux.HandleFunc("POST /v1/archives/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		var req agent.ArchiveRestoreRequest
		json.NewDecoder(r.Body).Decode(&req)
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.restores = append(fake.restores, req)
		a := fake.archives[r.PathValue("id")]
		result := agent.ArchiveRestoreResult{AppID: a.AppID, Skipped: map[string]string{}}
		for _, host := range a.Hostnames {
			if owner := req.Taken[host]; owner != "" {
				result.Skipped[host] = owner
			} else {
				result.Hostnames = append(result.Hostnames, host)
			}
		}
		delete(fake.archives, a.ID)
		agent.JSON(w, 200, result)
	})
	fx.a.mux.HandleFunc("DELETE /v1/archives/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req agent.ArchivePurgeRequest
		json.NewDecoder(r.Body).Decode(&req)
		fake.mu.Lock()
		defer fake.mu.Unlock()
		a, ok := fake.archives[r.PathValue("id")]
		if !ok || req.Confirm != a.AppID {
			agent.Fail(w, 409, nil)
			return
		}
		delete(fake.archives, a.ID)
		purge := agent.ArchivePurge{ArchiveID: a.ID, AppID: a.AppID, Trigger: "manual", At: time.Now()}
		fake.purged = append(fake.purged, purge)
		agent.JSON(w, 200, purge)
	})
	return fx, fake
}

func archiveCall(t *testing.T, handler http.HandlerFunc, method, target, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func TestDeleteAppArchivesReleasesHostnamesAndRemovesWebhook(t *testing.T) {
	fx, fake := newArchiveFixture(t)
	c := fx.c
	var hooks []string
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hooks = append(hooks, r.Method+" "+r.URL.Path)
		if r.Method == "POST" {
			w.Write([]byte(`{"id":77}`))
			return
		}
		w.WriteHeader(204)
	}))
	defer github.Close()
	c.cfg.GitHub = &gh.Client{API: github.URL}
	oldToken := archiveGitHubToken
	archiveGitHubToken = func(*Control, context.Context) (string, error) { return "token", nil }
	t.Cleanup(func() { archiveGitHubToken = oldToken })
	c.mu.Lock()
	c.data.Apps["shop"] = Application{ID: "shop", ServerID: "alpha", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", AdditionalDomains: []string{"www.shop.example.com"}, AutoDeploy: true, HookID: 42, Secret: "s", Generation: "g1"}
	c.mu.Unlock()

	fx.b.with(func() {
		fx.b.retained = []agent.RetainedResourceView{
			{ID: "copy-shop", AppID: "shop", Kind: "application", Origin: &agent.RetainedOrigin{MoveID: "m0", SourceServer: "alpha", Switched: true}, Components: []agent.RetainedComponent{}},
			{ID: "removed-other", AppID: "other", Kind: "application", Components: []agent.RetainedComponent{}},
		}
	})
	if w := archiveCall(t, c.deleteApp, "DELETE", "/api/control/apps/shop", "shop", `{"confirm":"sho"}`); w.Code != 400 {
		t.Fatalf("unconfirmed: %d %s", w.Code, w.Body)
	}
	w := archiveCall(t, c.deleteApp, "DELETE", "/api/control/apps/shop", "shop", `{"confirm":"shop","reason":"replaced by v2"}`)
	if w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	var view ArchiveView
	json.Unmarshal(w.Body.Bytes(), &view)
	if view.ID != "archive-1" || view.ServerName != "Alpha" || view.DaysLeft != 90 || !strings.Contains(view.Webhook, "removed") {
		t.Fatalf("view = %+v", view)
	}
	if len(fake.requests) != 1 || fake.requests[0].Reason != "replaced by v2" {
		t.Fatalf("agent requests = %+v", fake.requests)
	}
	fx.b.with(func() {
		if len(fx.b.retainedRemovals) != 1 || fx.b.retainedRemovals[0].Confirm != "copy-shop" || !fx.b.retainedRemovals[0].MoveLeftover {
			t.Fatalf("move copies removed: %+v", fx.b.retainedRemovals)
		}
	})
	if !strings.Contains(view.MoveCopies, "1 move copies") {
		t.Fatalf("move copies = %q", view.MoveCopies)
	}
	if !slices.Contains(hooks, "DELETE /repos/acme/shop/hooks/42") {
		t.Fatalf("webhook calls = %v", hooks)
	}
	c.mu.Lock()
	_, live := c.data.Apps["shop"]
	record := c.data.AppArchives["archive-1"]
	owner := c.hostOwnerLocked("www.shop.example.com")
	c.mu.Unlock()
	if live || record.App.Repository != "acme/shop" || record.App.Secret != "" || owner != "" {
		t.Fatalf("live=%v record=%+v owner=%q", live, record, owner)
	}

	// The hostnames are free: another application takes one.
	c.mu.Lock()
	c.data.Apps["taker"] = Application{ID: "taker", ServerID: "beta", Repository: "acme/taker", Branch: "main", Domain: "www.shop.example.com"}
	c.mu.Unlock()

	list := archiveCall(t, c.archives, "GET", "/api/control/archives", "", "")
	var archives ArchivesView
	json.Unmarshal(list.Body.Bytes(), &archives)
	if list.Code != 200 || len(archives.Archives) != 1 || archives.RetentionDays != 90 || archives.Archives[0].Repository != "acme/shop" || archives.Archives[0].KeptBytes != 0 && archives.Archives[0].KeptBytes != 2048 {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}

	restore := archiveCall(t, c.restoreArchive, "POST", "/api/control/archives/archive-1/restore", "archive-1", `{}`)
	if restore.Code != 200 {
		t.Fatalf("restore: %d %s", restore.Code, restore.Body)
	}
	var restored RestoreView
	json.Unmarshal(restore.Body.Bytes(), &restored)
	if fake.restores[0].Taken["www.shop.example.com"] != "taker" || restored.Result.Skipped["www.shop.example.com"] != "taker" {
		t.Fatalf("taken = %+v result = %+v", fake.restores[0].Taken, restored.Result)
	}
	if restored.App.Domain != "shop.example.com" || len(restored.App.AdditionalDomains) != 0 || restored.Deployment == nil || restored.DeployError != "" {
		t.Fatalf("restored = %+v", restored)
	}
	if !slices.Contains(hooks, "POST /repos/acme/shop/hooks") || !strings.Contains(restored.Webhook, "registered") {
		t.Fatalf("webhook calls = %v (%s)", hooks, restored.Webhook)
	}
	c.mu.Lock()
	a := c.data.Apps["shop"]
	_, archived := c.data.AppArchives["archive-1"]
	c.mu.Unlock()
	if a.HookID != 77 || !a.AutoDeploy || archived {
		t.Fatalf("app = %+v archived=%v", a, archived)
	}
	if len(fx.a.deploys) != 1 {
		t.Fatalf("deploys = %+v", fx.a.deploys)
	}
}

func TestDeleteAppRefusedDuringMove(t *testing.T) {
	fx, fake := newArchiveFixture(t)
	c := fx.c
	c.mu.Lock()
	c.data.Apps["shop"] = Application{ID: "shop", ServerID: "alpha", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com"}
	c.data.Moves = map[string]MoveJob{"m1": {ID: "m1", AppID: "shop", SourceServer: "alpha", TargetServer: "beta", State: "copying", Updated: time.Now()}}
	c.mu.Unlock()
	if w := archiveCall(t, c.deleteApp, "DELETE", "/api/control/apps/shop", "shop", `{"confirm":"shop"}`); w.Code != 409 || len(fake.requests) != 0 {
		t.Fatalf("delete during move: %d %s", w.Code, w.Body)
	}
}

func TestArchivePurgeAndExpiryRecords(t *testing.T) {
	fx, fake := newArchiveFixture(t)
	c := fx.c
	c.mu.Lock()
	c.data.Apps["shop"] = Application{ID: "shop", ServerID: "alpha", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com"}
	c.mu.Unlock()
	if w := archiveCall(t, c.deleteApp, "DELETE", "/api/control/apps/shop", "shop", `{"confirm":"shop"}`); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := archiveCall(t, c.purgeArchive, "DELETE", "/api/control/archives/archive-1", "archive-1", `{"confirm":"nope"}`); w.Code != 400 {
		t.Fatalf("unconfirmed purge: %d %s", w.Code, w.Body)
	}
	if w := archiveCall(t, c.purgeArchive, "DELETE", "/api/control/archives/archive-1", "archive-1", `{"confirm":"shop"}`); w.Code != 200 {
		t.Fatalf("purge: %d %s", w.Code, w.Body)
	}
	c.mu.Lock()
	n := len(c.data.AppArchives)
	c.mu.Unlock()
	if n != 0 {
		t.Fatal("the panel kept a purged archive")
	}

	// An archive the agent purged on its own (expiry) leaves the list.
	c.mu.Lock()
	c.data.AppArchives = map[string]ControlArchive{"archive-old": {ID: "archive-old", AppID: "old", ServerID: "alpha"}}
	c.mu.Unlock()
	fake.mu.Lock()
	fake.purged = append(fake.purged, agent.ArchivePurge{ArchiveID: "archive-old", AppID: "old", Trigger: "expiry", At: time.Now()})
	fake.mu.Unlock()
	list := archiveCall(t, c.archives, "GET", "/api/control/archives", "", "")
	var archives ArchivesView
	json.Unmarshal(list.Body.Bytes(), &archives)
	if len(archives.Archives) != 0 || len(archives.Purged) != 2 || archives.Purged[0].ServerName != "Alpha" {
		t.Fatalf("list: %s", list.Body)
	}
	c.mu.Lock()
	n = len(c.data.AppArchives)
	c.mu.Unlock()
	if n != 0 {
		t.Fatal("an expired archive stayed in the panel")
	}
}

func TestArchiveRoutesAreRegistered(t *testing.T) {
	fx := newMoveFixture(t)
	var patterns []string
	fx.c.routes(func(pattern string, _ http.HandlerFunc) { patterns = append(patterns, pattern) })
	for _, pattern := range []string{"GET /api/control/archives", "POST /api/control/archives/{id}/restore", "DELETE /api/control/archives/{id}"} {
		if !slices.Contains(patterns, pattern) {
			t.Fatalf("%s is not registered", pattern)
		}
	}
	if routePermission("DELETE /api/control/archives/{id}") != "infrastructure.manage" {
		t.Fatal("permanent deletion needs infrastructure management")
	}
}
