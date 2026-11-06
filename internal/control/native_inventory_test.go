package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func nativeInventoryFixture(t *testing.T) (*Control, Server, agent.NativeApplication) {
	t.Helper()
	server := Server{ID: "native", URL: "http://127.0.0.1:9092", Token: strings.Repeat("t", 32)}
	c, err := New(Config{PublicURL: "https://deploy.example.com", User: "owner@example.com", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(), Servers: []Server{server}})
	if err != nil {
		t.Fatal(err)
	}
	n := agent.NativeApplication{ID: "rails-store", Name: "store", Domain: "store.example.com", Repository: "org/store", Branch: "master", HealthPath: "/up", Port: "9190", Services: []string{"store-server.service", "store-worker.service"}}
	return c, server, n
}

func inventoryRemote(n agent.NativeApplication) []agent.App {
	return []agent.App{{ID: n.ID, Native: &n, Runtime: "rails", Repository: n.Repository, Branch: n.Branch, Domain: n.Domain}}
}

func TestNativeInventoryAutomaticImportRetainsDeployPreferencesAndIsIdempotent(t *testing.T) {
	c, server, n := nativeInventoryFixture(t)
	goApp := Application{ID: "go", ServerID: "go-server", Domain: "go.example.com", Repository: "org/go", Branch: "main", AutoDeploy: true, HookID: 42, Secret: "original", Generation: "keep"}
	c.data.Apps[goApp.ID] = goApp
	c.data.ObservedApps = map[string]ObservedApplication{"manual": {Name: "legacy", Source: "other", Runtime: "go"}}
	if err := c.mergeNativeInventory(server, []agent.NativeApplication{n}, inventoryRemote(n)); err != nil {
		t.Fatal(err)
	}
	imported := c.data.Apps[n.ID]
	if imported.AutoDeploy || imported.HookID != 0 || imported.Secret != "" || imported.Generation == "" || !sameNativeApplication(*imported.Native, n) {
		t.Fatal("native sync acquired deployment automation", imported)
	}
	if !reflect.DeepEqual(c.data.Apps[goApp.ID], goApp) || c.data.ObservedApps["manual"].Name != "legacy" {
		t.Fatal("unrelated inventory changed")
	}
	if err := c.mergeNativeInventory(server, []agent.NativeApplication{n}, inventoryRemote(n)); err != nil {
		t.Fatal(err)
	}
	if len(c.data.Apps) != 2 || c.data.Apps[n.ID].Generation != imported.Generation {
		t.Fatal("retry duplicated or replaced app identity")
	}
	reloaded, err := New(c.cfg)
	if err != nil || len(reloaded.data.Apps) != 2 {
		t.Fatal("inventory was not persisted", err)
	}
}

func TestNativeInventoryDiscoversWithoutGuessingAndPromotesWithoutDuplicates(t *testing.T) {
	c, server, n := nativeInventoryFixture(t)
	discovery := []agent.NativeApplication{{Name: n.Name, Services: n.Services}}
	if err := c.mergeNativeInventory(server, discovery, nil); err != nil {
		t.Fatal(err)
	}
	id := "discovered:" + server.ID + ":" + n.Name
	observed := c.data.ObservedApps[id]
	if !observed.Automatic || observed.Domain != "" || observed.Name != n.Name || len(c.data.Apps) != 0 {
		t.Fatal("missing metadata guessed or required manual import")
	}
	if err := c.mergeNativeInventory(server, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !c.data.ObservedApps[id].Missing {
		t.Fatal("disappeared service silently removed")
	}
	if err := c.mergeNativeInventory(server, discovery, inventoryRemote(n)); err != nil {
		t.Fatal(err)
	}
	if len(c.data.ObservedApps) != 0 || len(c.data.Apps) != 1 {
		t.Fatal("promotion left duplicate observations")
	}
}

func TestNativeInventoryConflictsAndSaveFailureKeepOriginalState(t *testing.T) {
	for _, scenario := range []string{"container-owner", "different-server", "changed-binding", "changed-pairing", "save-failure"} {
		t.Run(scenario, func(t *testing.T) {
			c, server, n := nativeInventoryFixture(t)
			original := Application{ID: n.ID, ServerID: server.ID, Native: &n, Domain: n.Domain, Repository: n.Repository, Branch: n.Branch, AutoDeploy: false}
			switch scenario {
			case "container-owner":
				original.Native = nil
			case "different-server":
				original.ServerID = "elsewhere"
			case "changed-binding":
				changed := n
				changed.Services = []string{"different.service"}
				original.Native = &changed
			case "changed-pairing":
				c.registry[0].Token = "rotated"
			case "save-failure":
				c.cfg.DataDir = "/dev/null/no-directory"
			}
			if scenario != "save-failure" {
				c.data.Apps[n.ID] = original
			}
			before, _ := json.Marshal(c.data)
			if err := c.mergeNativeInventory(server, []agent.NativeApplication{n}, inventoryRemote(n)); err == nil {
				t.Fatal("expected conflict or persistence failure")
			}
			after, _ := json.Marshal(c.data)
			if string(before) != string(after) {
				t.Fatal("failed sync changed original inventory")
			}
		})
	}
}

func TestNativeInventoryOnlyReadsSafeAgentEndpoints(t *testing.T) {
	for _, safe := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsafe", true: "safe"}[safe], func(t *testing.T) {
			c, server, n := nativeInventoryFixture(t)
			var mu sync.Mutex
			calls := []string{}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls = append(calls, r.Method+" "+r.URL.Path)
				mu.Unlock()
				if r.Method != "GET" {
					t.Error("sync sent host mutation", r.Method)
				}
				if r.Header.Get("Authorization") != "Bearer "+server.Token {
					t.Error("missing agent authentication")
				}
				switch r.URL.Path {
				case "/v1/native/capabilities":
					json.NewEncoder(w).Encode(map[string]bool{"status_metadata_only": safe, "http_readiness_probes": !safe})
				case "/v1/native/discovery":
					json.NewEncoder(w).Encode([]agent.NativeApplication{n})
				case "/v1/apps":
					json.NewEncoder(w).Encode(inventoryRemote(n))
				default:
					t.Error("unexpected request", r.URL.Path)
					http.Error(w, "blocked", 404)
				}
			}))
			defer remote.Close()
			server.URL = remote.URL
			c.registry = []Server{server}
			c.data.Servers = []Server{server}
			c.syncNativeInventory(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if safe {
				if len(calls) != 3 || len(c.data.Apps) != 1 || c.nativeSync[server.ID].State != "synced" {
					t.Fatal("safe sync incomplete", calls, c.nativeSync)
				}
			} else if len(calls) != 1 || len(c.data.Apps) != 0 || c.nativeSync[server.ID].State != "error" {
				t.Fatal("unsafe agent queried after failed capability gate", calls)
			}
		})
	}
}

func TestNativeInventoryOfflineAgentPreservesInventoryAndReportsFailure(t *testing.T) {
	c, server, n := nativeInventoryFixture(t)
	c.data.Apps[n.ID] = Application{ID: n.ID, Native: &n, ServerID: server.ID, Domain: n.Domain}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "offline", 503) }))
	defer remote.Close()
	server.URL = remote.URL
	c.registry = []Server{server}
	before, _ := json.Marshal(c.data)
	c.syncNativeInventory(context.Background())
	after, _ := json.Marshal(c.data)
	if string(before) != string(after) || c.nativeSync[server.ID].State != "error" {
		t.Fatal("offline agent removed inventory or was marked synced")
	}
}

func createdRemote(apps ...agent.NativeApplication) []agent.App {
	remote := []agent.App{}
	for _, n := range apps {
		remote = append(remote, agent.App{ID: n.ID, Native: &n, Runtime: n.Runtime, Repository: n.Repository, Branch: n.Branch, Domain: n.Domain})
	}
	return remote
}

func TestNativeInventoryAcceptsPolicyUnitsAndCreatedApplications(t *testing.T) {
	c, server, n := nativeInventoryFixture(t)
	// An application that moved to its own account: the agent registers and
	// discovers the policy's system units; its stale user units never appear.
	moved := n
	moved.Services = []string{"store-web.service", "store-jobs.service"}
	// Applications the agent created under its apps root, with units in
	// process order, runtimes the container validator does not know, and a
	// static site without services.
	shop := agent.NativeApplication{ID: "shop", Name: "shop", Runtime: "go", Domain: "shop.example.com", Repository: "acme/shop", Branch: "main", HealthPath: "/readyz", Port: "4100", Services: []string{"lidza-app-shop-web.service", "lidza-app-shop-jobs.service"}}
	api := agent.NativeApplication{ID: "api", Name: "api", Runtime: "node", Domain: "api.example.com", Repository: "acme/api", Branch: "main", HealthPath: "/", Port: "4102", Services: []string{"lidza-app-api-web.service"}}
	site := agent.NativeApplication{ID: "site", Name: "site", Runtime: "static", DocumentRoot: "dist", Domain: "site.example.com", Repository: "acme/site", Branch: "main", HealthPath: "/", Services: []string{}}
	// The panel recorded shop when it created it; the others are adopted.
	c.data.Apps[shop.ID] = Application{ID: shop.ID, ServerID: server.ID, Native: &shop, Runtime: "go", Repository: shop.Repository, Branch: shop.Branch, Domain: shop.Domain, AutoDeploy: true, Generation: "created"}
	discovery := []agent.NativeApplication{moved, shop, api, site}
	remote := slices.Concat(inventoryRemote(moved), createdRemote(shop, api, site))
	if err := c.mergeNativeInventory(server, discovery, remote); err != nil {
		t.Fatal(err)
	}
	if len(c.data.Apps) != 4 || !slices.Equal(c.data.Apps[n.ID].Native.Services, moved.Services) || c.data.Apps["api"].Runtime != "node" || c.data.Apps["site"].Runtime != "static" || len(c.data.ObservedApps) != 0 {
		t.Fatalf("moved and created applications were not adopted: %+v", c.data.Apps)
	}
	if kept := c.data.Apps[shop.ID]; kept.Generation != "created" || !kept.AutoDeploy {
		t.Fatal("the panel's own record of a created application was replaced", kept)
	}
	// Discovery may list services alphabetically while the registration keeps
	// process order; the sets are what must agree.
	sorted := slices.Clone(discovery)
	sorted[1].Services = []string{"lidza-app-shop-jobs.service", "lidza-app-shop-web.service"}
	if err := c.mergeNativeInventory(server, sorted, remote); err != nil || len(c.data.Apps) != 4 {
		t.Fatal("re-sync with reordered services changed the result", err)
	}
}

func TestNativeInventoryMismatchNamesApplicationAndServices(t *testing.T) {
	c, server, n := nativeInventoryFixture(t)
	c.data.Apps[n.ID] = Application{ID: n.ID, ServerID: server.ID, Native: &n, Domain: n.Domain, Repository: n.Repository, Branch: n.Branch}
	before, _ := json.Marshal(c.data)
	partial := n
	partial.Services = []string{"store-server.service"}
	err := c.mergeNativeInventory(server, []agent.NativeApplication{partial}, inventoryRemote(n))
	if want := "rails-store: registered services [store-server.service,store-worker.service] vs discovered [store-server.service]; previous inventory retained."; err == nil || err.Error() != want {
		t.Fatalf("service mismatch message = %v", err)
	}
	if err := c.mergeNativeInventory(server, nil, inventoryRemote(n)); err == nil || !strings.HasPrefix(err.Error(), "rails-store: registered on the agent but absent from its discovery") {
		t.Fatalf("missing discovery message = %v", err)
	}
	rebound := n
	rebound.Services = []string{"store-web.service"}
	if err := c.mergeNativeInventory(server, []agent.NativeApplication{rebound}, inventoryRemote(rebound)); err == nil || !strings.HasPrefix(err.Error(), "rails-store: panel record and agent binding differ (services [store-server.service,store-worker.service] vs [store-web.service])") {
		t.Fatalf("record conflict message = %v", err)
	}
	if err := c.mergeNativeInventory(server, []agent.NativeApplication{n}, nil); err == nil || !strings.HasPrefix(err.Error(), "rails-store: recorded in the panel but not registered on the agent") {
		t.Fatalf("unregistered record message = %v", err)
	}
	if after, _ := json.Marshal(c.data); string(before) != string(after) {
		t.Fatal("a named mismatch changed the inventory")
	}
}

func TestNativeInventoryRejectsMalformedSnapshotsWithoutPartialImports(t *testing.T) {
	for _, scenario := range []string{"duplicate-name", "invalid-unit", "duplicate-unit", "different-services", "missing-existing"} {
		t.Run(scenario, func(t *testing.T) {
			c, server, n := nativeInventoryFixture(t)
			discovery := []agent.NativeApplication{n}
			remote := inventoryRemote(n)
			switch scenario {
			case "duplicate-name":
				discovery = append(discovery, n)
			case "invalid-unit":
				discovery[0].Services = []string{"../bad.service"}
			case "duplicate-unit":
				discovery[0].Services = []string{"web.service", "web.service"}
			case "different-services":
				discovery[0].Services = []string{"other.service"}
			case "missing-existing":
				c.data.Apps[n.ID] = Application{ID: n.ID, ServerID: server.ID, Native: &n}
				remote = nil
			}
			before, _ := json.Marshal(c.data)
			if err := c.mergeNativeInventory(server, discovery, remote); err == nil {
				t.Fatal("malformed snapshot accepted")
			}
			after, _ := json.Marshal(c.data)
			if string(before) != string(after) {
				t.Fatal("malformed snapshot partially imported")
			}
		})
	}
}
