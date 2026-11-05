package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	n := agent.NativeApplication{ID: "rails-shop", Name: "shop", Domain: "shop.example.com", Repository: "org/shop", Branch: "master", HealthPath: "/up", Port: "9190", Services: []string{"shop-server.service", "shop-worker.service"}}
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
