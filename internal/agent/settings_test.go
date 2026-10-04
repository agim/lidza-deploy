package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSettingsPreserveSecretsAndRejectBusyChanges(t *testing.T) {
	m := testManager(t, &fakeRuntime{gate: make(chan struct{})})
	for _, id := range []string{"one", "two"} {
		if err := m.Upsert(testApp(id)); err != nil {
			t.Fatal(err)
		}
	}
	value := "new-secret"
	patch := SettingsPatch{Branch: "release", Domain: "updated.example.com", EnvChanges: map[string]*string{"NEW": &value}}
	if err := m.PatchSettings("one", patch); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a := m.data.Apps["one"]
	m.mu.Unlock()
	if a.Env["APP_SECRET"] != "runtime-secret" || a.Env["NEW"] != value {
		t.Fatal("lost variables")
	}
	settings, err := m.Settings("one")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(settings)
	if strings.Contains(string(data), value) || strings.Contains(string(data), "runtime-secret") {
		t.Fatal("leaked secret")
	}
	patch.EnvChanges = map[string]*string{"APP_SECRET": nil}
	if err := m.PatchSettings("one", patch); err != nil {
		t.Fatal(err)
	}
	// Rejected patches must not mutate the previous environment map.
	patch.EnvChanges = map[string]*string{"NEW": nil}
	patch.Domain = "two.example.com"
	if err := m.PatchSettings("one", patch); err == nil {
		t.Fatal("duplicate domain accepted")
	}
	patch.Domain = "updated.example.com"
	if _, err := m.Enqueue("one", DeployRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := m.PatchSettings("one", patch); err == nil {
		t.Fatal("busy app edited")
	}
	if err := m.Retire(context.Background(), "one"); err == nil {
		t.Fatal("active deployment removed")
	}
	// Stop the queued worker and verify the saved settings survive restart.
	m.cancel()
	m.wg.Wait()
	reloaded, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	reloaded.mu.Lock()
	a = reloaded.data.Apps["one"]
	reloaded.mu.Unlock()
	if _, exists := a.Env["APP_SECRET"]; exists || a.Env["NEW"] != value {
		t.Fatal("persisted patch incorrect")
	}
}

func TestSettingsAutomaticallyReloadAndRetainHealthyRelease(t *testing.T) {
	rt := &fakeRuntime{}
	m := testManager(t, rt)
	a := testApp("reload")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	d, err := m.Enqueue(a.ID, DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waitDeployment(t, m, d.ID)
	original := m.Current(a.ID)
	value := "updated"
	domain := "changed.example.com"
	if err = m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: domain, EnvChanges: map[string]*string{"FEATURE": &value}}); err != nil {
		t.Fatal(err)
	}
	jobs := m.Deployments()
	reload := jobs[len(jobs)-1]
	if reload.Kind != "reload" {
		t.Fatal("settings did not queue reload")
	}
	if waitDeployment(t, m, reload.ID).Status != "live" {
		t.Fatal("reload failed")
	}
	current := m.Current(a.ID)
	if current.ID == original.ID || current.Commit != original.Commit || m.Target(a.Domain) != nil || m.Target(domain) == nil {
		t.Fatal("image reuse or domain routing incorrect")
	}
	rt.mu.Lock()
	rt.fail = true
	rt.mu.Unlock()
	if err = m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: domain, EnvChanges: map[string]*string{"FEATURE": &value}}); err != nil {
		t.Fatal(err)
	}
	jobs = m.Deployments()
	if waitDeployment(t, m, jobs[len(jobs)-1].ID).Status != "failed" || m.Current(a.ID).ID != current.ID {
		t.Fatal("failed reload replaced healthy release")
	}
	rt.mu.Lock()
	rt.fail = false
	rt.mu.Unlock()
	retry, err := m.Reload(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if waitDeployment(t, m, retry.ID).Status != "live" {
		t.Fatal("reload retry failed")
	}
}

func TestLegacyDatabaseBindingsMigrateOnlyOnce(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if err := m.Upsert(testApp("legacy")); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.data.BindingsVersion = 0
	m.data.Databases["legacy"] = Database{AppID: "legacy", Mode: "local", URL: "postgres://app:fixture@db/app"}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	m.Close()
	next, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := next.Settings("legacy"); got.Bindings["DATABASE_URL"] != "legacy" {
		t.Fatal("legacy database not attached")
	}
	// A new resource coinciding with another app ID must not get attached on restart.
	if err = next.Upsert(testApp("unrelated")); err != nil {
		t.Fatal(err)
	}
	next.mu.Lock()
	next.data.Databases["unrelated"] = Database{AppID: "unrelated", Mode: "local"}
	if err = next.save(); err != nil {
		t.Fatal(err)
	}
	next.mu.Unlock()
	next.Close()
	last, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer last.Close()
	if got, _ := last.Settings("unrelated"); len(got.Bindings) != 0 {
		t.Fatal("database unintentionally attached after migration")
	}
}

func TestSharedDatabaseSurvivesOwnerRemoval(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	for _, id := range []string{"owner", "consumer"} {
		if err := m.Upsert(testApp(id)); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	m.data.Databases["shared"] = Database{AppID: "shared", Mode: "external", Ready: true, URL: "postgres://app:fixture-secret@host/app?sslmode=require", Backup: BackupPolicy{Keep: 7}}
	m.mu.Unlock()
	for _, id := range []string{"owner", "consumer"} {
		if err := m.configureDatabase(id, DatabaseRequest{Mode: "existing", ID: "shared", EnvKey: "DATABASE_URL"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.configureDatabase("consumer", DatabaseRequest{Mode: "existing", ID: "shared", EnvKey: "ANALYTICS_DATABASE_URL"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Retire(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	views := m.databaseViews()
	if len(views) != 1 || views[0].Retained || len(views[0].Attachments) != 2 {
		t.Fatal("shared database lost its active attachments")
	}
	safe, _ := json.Marshal(views)
	if strings.Contains(string(safe), "fixture-secret") {
		t.Fatal("database credentials leaked")
	}
	if err := m.configureDatabase("consumer", DatabaseRequest{Mode: "existing", ID: "missing", EnvKey: "DATABASE_URL"}); err == nil {
		t.Fatal("missing database accepted")
	}
	settings, _ := m.Settings("consumer")
	if settings.Bindings["DATABASE_URL"] != "shared" {
		t.Fatal("failed switch changed primary")
	}
}
