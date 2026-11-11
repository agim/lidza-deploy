package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestSettingsApplyClassificationPerRuntime(t *testing.T) {
	subjects := map[string]settingsSubject{
		"container go":      {Runtime: "go", AgentAppURL: true},
		"container rails":   {Runtime: "rails", AgentAppURL: true},
		"container astro":   {Runtime: "astro", AgentAppURL: true},
		"container own URL": {Runtime: "go"},
		"native rails":      {Runtime: "rails", Native: true},
		"native node":       {Runtime: "node", Native: true},
		"native go":         {Runtime: "go", Native: true},
		"native auto":       {Runtime: "auto", Native: true},
		"native static":     {Runtime: "static", Native: true},
	}
	// want[change][subject]; a missing subject takes want[change][""].
	want := map[string]map[string]settingsEffect{
		"env:SECRET_KEY_BASE":   {"": effectRestart, "native static": effectDeploy},
		"env:LIDZA_MASTER_KEY":  {"": effectRestart, "native static": effectDeploy},
		"env:RAILS_MASTER_KEY":  {"": effectRestart, "native static": effectDeploy},
		"env:PUBLIC_API_URL":    {"": effectRestart, "container astro": effectDeploy, "native node": effectDeploy, "native auto": effectDeploy, "native static": effectDeploy},
		"database_bindings":     {"": effectRestart},
		"branch":                {"": effectDeploy},
		"build":                 {"": effectDeploy},
		"astro":                 {"": effectDeploy},
		"runtime_profile":       {"": effectDeploy},
		"toolchains":            {"": effectDeploy},
		"domain":                {"": effectImmediate, "container go": effectRestart, "container rails": effectRestart, "container astro": effectRestart},
		"additional_domains":    {"": effectImmediate},
		"customer_hosts":        {"": effectImmediate},
		"backup_before_deploy":  {"": effectImmediate},
		"env_kind:API_TOKEN":    {"": effectImmediate},
		"a_field_added_someday": {"": effectDeploy},
	}
	for change, bySubject := range want {
		for name, subject := range subjects {
			expected, ok := bySubject[name]
			if !ok {
				expected = bySubject[""]
			}
			if got := subject.effect(change); got != expected {
				t.Errorf("%s, %s: %s, want %s", name, change, got, expected)
			}
		}
	}
	go1 := subjects["container go"]
	if got := go1.classify(nil); got != effectNone {
		t.Fatal("no change applied", got)
	}
	if got := go1.classify([]string{"additional_domains", "env:A"}); got != effectRestart {
		t.Fatal("a restart outranks immediate changes", got)
	}
	if got := go1.classify([]string{"env:A", "branch", "customer_hosts"}); got != effectDeploy {
		t.Fatal("a deploy outranks a restart", got)
	}
	// The console's view is the same table.
	effects := subjects["container astro"].effects()
	if effects.Env != SettingsAppliedRestart || !slices.Equal(effects.BuildEnvPrefixes, []string{"PUBLIC_"}) || effects.Fields["branch"] != SettingsAppliedDeploy || effects.Fields["domain"] != SettingsAppliedRestart || effects.Fields["additional_domains"] != SettingsAppliedImmediate {
		t.Fatalf("container astro effects %+v", effects)
	}
	if static := subjects["native static"].effects(); static.Env != SettingsAppliedDeploy || len(static.BuildEnvPrefixes) != 0 || static.Fields["domain"] != SettingsAppliedImmediate {
		t.Fatalf("native static effects %+v", static)
	}
}

// envRecordingRuntime records the environment each release starts with.
type envRecordingRuntime struct {
	*fakeRuntime
	mu      sync.Mutex
	reloads []map[string]string
}

func (r *envRecordingRuntime) Reload(ctx context.Context, a App, id string) (*Release, error) {
	r.mu.Lock()
	r.reloads = append(r.reloads, a.runtimeEnvironment())
	r.mu.Unlock()
	return r.fakeRuntime.Reload(ctx, a, id)
}

func TestSettingsSaveRestartsContainerWithNewEnvironmentAndDeploysBuildChanges(t *testing.T) {
	rt := &envRecordingRuntime{fakeRuntime: &fakeRuntime{}}
	m := testManager(t, rt)
	a := testApp("apply")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	d, err := m.Enqueue(a.ID, DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waitDeployment(t, m, d.ID)
	logs := captureLog(t)
	secret := "rotated-runtime-secret-value"
	result, err := m.PatchSettingsApply(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, EnvChanges: map[string]*string{"APP_SECRET": &secret}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != SettingsAppliedRestart || result.Deployment == nil || result.DeploymentID != result.Deployment.ID || result.Deployment.Kind != "reload" || !slices.Equal(result.Changes, []string{"env:APP_SECRET"}) {
		t.Fatalf("environment save result %+v", result)
	}
	if waitDeployment(t, m, result.DeploymentID).Status != "live" {
		t.Fatal("restart failed")
	}
	rt.mu.Lock()
	started := rt.reloads[len(rt.reloads)-1]
	rt.mu.Unlock()
	if started["APP_SECRET"] != secret {
		t.Fatal("the restarted release did not get the saved environment")
	}
	raw, _ := json.Marshal(result)
	history, _ := json.Marshal(m.Deployments())
	if strings.Contains(string(raw), secret) || strings.Contains(string(history), secret) || strings.Contains(logs.String(), secret) {
		t.Fatal("a saved credential reached a result, the history or the log")
	}

	// A build change saves and asks for a deployment; the agent starts no reload.
	before := len(m.Deployments())
	result, err = m.PatchSettingsApply(a.ID, SettingsPatch{Branch: "next", Domain: a.Domain})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != SettingsAppliedDeploy || result.Deployment != nil || len(m.Deployments()) != before {
		t.Fatalf("branch save result %+v", result)
	}
	if settings, _ := m.Settings(a.ID); settings.Branch != "next" || settings.ApplyEffects == nil || settings.ApplyEffects.Fields["branch"] != SettingsAppliedDeploy {
		t.Fatalf("branch not saved or effects missing %+v", settings.ApplyEffects)
	}

	// Additional hostnames and the backup policy apply without a restart.
	off := false
	result, err = m.PatchSettingsApply(a.ID, SettingsPatch{Branch: "next", Domain: a.Domain, AdditionalDomains: &[]string{"www.apply.example.com"}, BackupBeforeDeploy: &off})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != SettingsAppliedImmediate || result.Deployment != nil || len(m.Deployments()) != before {
		t.Fatalf("immediate save result %+v", result)
	}

	// A stopped application keeps the change for its start.
	m.mu.Lock()
	stoppedApp := m.data.Apps[a.ID]
	stoppedApp.Stopped = true
	m.data.Apps[a.ID] = stoppedApp
	m.mu.Unlock()
	other := "while-stopped"
	result, err = m.PatchSettingsApply(a.ID, SettingsPatch{Branch: "next", Domain: a.Domain, EnvChanges: map[string]*string{"FEATURE": &other}})
	if err != nil || result.Applied != SettingsAppliedImmediate || result.Deployment != nil || !strings.Contains(result.Message, "stopped") {
		t.Fatalf("stopped save result %+v %v", result, err)
	}
}

func TestMasterKeyGenerationRestartsTheApplication(t *testing.T) {
	rt := &envRecordingRuntime{fakeRuntime: &fakeRuntime{}}
	m := testManager(t, rt)
	a := testApp("keyed")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	d, err := m.Enqueue(a.ID, DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waitDeployment(t, m, d.ID)
	m.mu.Lock()
	current := m.data.Apps[a.ID]
	delete(current.Env, "LIDZA_MASTER_KEY")
	m.data.Apps[a.ID] = current
	m.mu.Unlock()
	result, err := m.GenerateMasterKeyApply(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != SettingsAppliedRestart || result.Deployment == nil {
		t.Fatalf("master key result %+v", result)
	}
	waitDeployment(t, m, result.DeploymentID)
	rt.mu.Lock()
	key := rt.reloads[len(rt.reloads)-1]["LIDZA_MASTER_KEY"]
	rt.mu.Unlock()
	if len(key) != 64 {
		t.Fatal("the restarted release did not get the generated key")
	}
	if raw, _ := json.Marshal(result); strings.Contains(string(raw), key) {
		t.Fatal("the key reached the result")
	}
}

func TestNativeSettingsSaveRestartsThroughTheReleaseEngine(t *testing.T) {
	f := newNativeReleaseFixture(t)
	f.m.cfg.Native.Releases[f.app.ID] = f.policy()
	f.m.cfg.Native.ServiceControl = true
	envFile := filepath.Join(f.root, ".env.production")
	logs := captureLog(t)
	secret := "rotated-native-secret"
	result, err := f.m.patchNativeSettings(f.app.ID, SettingsPatch{EnvChanges: map[string]*string{"SECRET_KEY_BASE": &secret}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != SettingsAppliedRestart || result.Deployment == nil || result.Deployment.Kind != "restart" {
		t.Fatalf("native environment save %+v", result)
	}
	if raw, _ := os.ReadFile(envFile); !strings.Contains(string(raw), "SECRET_KEY_BASE="+secret+"\n") {
		t.Fatalf("environment file the units load:\n%s", raw)
	}
	done := waitDeployment(t, f.m, result.DeploymentID)
	if done.Status != "live" {
		t.Fatal("restart failed", done.Error)
	}
	f.units.mu.Lock()
	restarted := slices.Clone(f.units.restarted)
	f.units.mu.Unlock()
	// Units restart in the policy's order; the preserved monitor is left alone.
	if !slices.Equal(restarted, []string{"myrails-server.service", "myrails-worker.service"}) {
		t.Fatal("restarted units", restarted)
	}
	raw, _ := json.Marshal(result)
	history, _ := json.Marshal(f.m.Deployments())
	if strings.Contains(string(raw), secret) || strings.Contains(string(history), secret) || strings.Contains(logs.String(), secret) {
		t.Fatal("a saved credential reached a result, the history or the log")
	}

	// The branch is build-affecting: the panel deploys it.
	result, err = f.m.patchNativeSettings(f.app.ID, SettingsPatch{Branch: "staging"})
	if err != nil || result.Applied != SettingsAppliedDeploy || result.Deployment != nil {
		t.Fatalf("native branch save %+v %v", result, err)
	}
	settings, err := f.m.nativeSettings(f.app.ID)
	if err != nil || settings.ApplyEffects == nil || settings.ApplyEffects.Fields["toolchains"] != SettingsAppliedDeploy || settings.ApplyEffects.Env != SettingsAppliedRestart {
		t.Fatalf("native effects %+v %v", settings.ApplyEffects, err)
	}
}

func TestAdoptedSettingsSaveWithServiceControlOffNeedsARestart(t *testing.T) {
	f := newNativeReleaseFixture(t)
	f.m.cfg.Native.Releases[f.app.ID] = f.policy()
	f.m.cfg.Native.ServiceControl = false
	value := "saved-but-not-running"
	before := len(f.m.Deployments())
	result, err := f.m.patchNativeSettings(f.app.ID, SettingsPatch{EnvChanges: map[string]*string{"GREETING": &value}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != SettingsAppliedRestartRequired || result.Deployment != nil || !strings.Contains(result.Message, serviceControlOffReason) || len(f.m.Deployments()) != before {
		t.Fatalf("service control off %+v", result)
	}
	if values, _ := nativeReleaseEnvValues(f.policy()); values["GREETING"] != value {
		t.Fatal("the change was not kept")
	}
	f.units.mu.Lock()
	defer f.units.mu.Unlock()
	if len(f.units.restarted) != 0 {
		t.Fatal("a unit restarted with service control off", f.units.restarted)
	}
}
