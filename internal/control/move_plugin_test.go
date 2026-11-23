package control

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func pluginMoveSource(fx *moveFixture) {
	native := agent.NativeApplication{ID: "shop", Name: "shop", Runtime: "go", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", Services: []string{"app-shop-web.service"}}
	fx.c.data.Apps["shop"] = Application{ID: "shop", ServerID: "alpha", Native: &native, Runtime: "go", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", GitHubInstallation: 7, Generation: "g-source"}
	spec := agent.NativeCreateRequest{ID: "shop", Runtime: "go", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", HealthPath: "/readyz",
		Processes: []agent.NativeProcessSpec{{Name: "web", Command: "./shop", Web: true}},
		Data:      agent.NativeDataRequest{Plugins: []agent.NativePluginService{{Service: "search", Options: json.RawMessage(`{"languages":["en"]}`)}}}}
	fx.a.source = &agent.MoveSource{AppID: "shop", Runtime: "go", Native: true, Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", Commit: moveTestCommit,
		EnvironmentKeys: []string{"PORT", "SEARCH_URL"}, Databases: []agent.MoveSourceDatabase{}, Volumes: []string{}, Tasks: []string{}, Blockers: []string{}, Notes: []string{}, CreateSpec: &spec, SpecRecorded: true,
		Plugins: []agent.MoveSourcePlugin{{Service: "search", Plugin: "meili", Version: "1.0", Resource: "shop-plsource", Copyable: true, SizeBytes: 4096, Backup: agent.BackupPolicy{Hours: 24, Keep: 1}}}}
	fx.a.env = agent.MoveEnvironment{AppID: "shop", Native: true, Values: map[string]string{"PORT": "3001", "SEARCH_URL": "http://127.0.0.1:7700/shop"}, Kinds: map[string]string{}}
	fx.a.nativeSource = &agent.NativeStatus{Application: native}
	fx.a.dumps["shop-plsource"] = []byte("the search index, as its plugin backed it up")
}

// A plugin's service moves with its application: backed up on the source,
// relayed and restored by the destination's plugin, in the bulk copy and
// again in the final sync.
func TestMoveCopiesAPluginServiceThroughItsPlugin(t *testing.T) {
	fx := newMoveFixture(t)
	pluginMoveSource(fx)
	fx.b.plugins = []agent.PluginRecord{{Name: "meili", Version: "1.1", Provides: "search", Hooks: []string{"describe", "provision", "release", "backup", "restore"}}}
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatalf("start: %d %+v", status, view.MoveJob)
	}
	planned := slices.IndexFunc(view.Plan.Databases, func(d MovePlanDatabase) bool { return d.ID == "shop-plsource" })
	if planned < 0 || view.Plan.Databases[planned].Engine != "search" || view.Plan.Databases[planned].Action != "copy" || view.Plan.Databases[planned].SizeBytes != 4096 {
		t.Fatalf("plan: %+v", view.Plan.Databases)
	}
	if !slices.ContainsFunc(view.Plan.Notes, func(note string) bool { return strings.Contains(note, "version 1.0 here and 1.1 on the destination") }) {
		t.Fatalf("the differing plugin versions are not noted: %v", view.Plan.Notes)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if len(job.Plugins) != 1 || job.Plugins[0].TargetID != "shop-pl0001" || len(job.Plugins[0].Copied) != 64 || job.Plugins[0].Bytes != int64(len(fx.a.dumps["shop-plsource"])) {
		t.Fatalf("job: %+v", job.Plugins)
	}
	fx.b.with(func() {
		if fx.b.created == nil || len(fx.b.created.Data.Plugins) != 1 || fx.b.created.Data.Plugins[0].Service != "search" || string(fx.b.created.Data.Plugins[0].Options) != `{"languages":["en"]}` {
			t.Fatalf("the destination was not asked for the service with its options: %+v", fx.b.created)
		}
		if got := fx.b.restores["plugin:shop-pl0001"]; len(got) != 1 || string(got[0]) != "the search index, as its plugin backed it up" {
			t.Fatalf("restores after the bulk copy: %q", got)
		}
	})
	// The final sync, with the source stopped, copies the service again.
	fx.a.with(func() { fx.a.dumps["shop-plsource"] = []byte("the search index after the last write") })
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if got := fx.b.restores["plugin:shop-pl0001"]; len(got) != 2 || string(got[1]) != "the search index after the last write" {
			t.Fatalf("restores after the final sync: %q", got)
		}
	})
}

// The review refuses a destination that could not restore the service,
// before anything is copied.
func TestMovePlanRefusesADestinationWithoutThePlugin(t *testing.T) {
	blockers := func(fx *moveFixture) []string {
		status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
		if status == 202 {
			t.Fatalf("the move started: %+v", view.Plan.Blockers)
		}
		plan, _, err := fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta"})
		if err != nil {
			t.Fatal(err)
		}
		return plan.Blockers
	}
	has := func(list []string, part string) bool {
		return slices.ContainsFunc(list, func(b string) bool { return strings.Contains(b, part) })
	}
	fx := newMoveFixture(t)
	pluginMoveSource(fx)
	if got := blockers(fx); !has(got, "cannot run plugins") {
		t.Fatalf("an agent from before plugins: %v", got)
	}
	fx.b.plugins = []agent.PluginRecord{}
	if got := blockers(fx); !has(got, "no plugin that provides the search service; install the meili plugin") {
		t.Fatalf("no plugin: %v", got)
	}
	fx.b.plugins = []agent.PluginRecord{{Name: "meili", Version: "0.9", Provides: "search", Hooks: []string{"describe", "provision", "release"}}}
	if got := blockers(fx); !has(got, "cannot restore a backup") {
		t.Fatalf("a plugin without restore: %v", got)
	}
	// A service its own plugin cannot back up is the source's blocker.
	fx.b.plugins = []agent.PluginRecord{{Name: "meili", Version: "1.0", Provides: "search", Hooks: []string{"backup", "restore"}}}
	fx.a.with(func() {
		fx.a.source.Plugins[0].Copyable, fx.a.source.Plugins[0].Reason = false, "its plugin cannot back it up"
		fx.a.source.Blockers = []string{"The search service of the meili plugin cannot be copied: the plugin on this server has no backup. Move its data by hand."}
	})
	if got := blockers(fx); !has(got, "cannot be copied") {
		t.Fatalf("a service without backups: %v", got)
	}
}
