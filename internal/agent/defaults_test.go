package agent

import (
	"context"
	"fmt"
	"maps"
	"testing"
)

func TestDefaultsSnapshotRetriesAndRestart(t *testing.T) {
	m := testManager(t, &fakeRuntime{fail: true})
	a := testApp("defaults")
	a.Env["ADMIN_USERS"] = "app@example.com"
	a.Env["AUTH_OWNER_CLAIM"] = ""
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	defaults := map[string]string{"ADMIN_USERS": "shared@example.com", "AUTH_OWNER_CLAIM": "true", "DB_MIGRATE": "false", "LOG_LEVEL": "info", "MAIL_FROM": ""}
	d, err := m.Enqueue(a.ID, DeployRequest{Defaults: defaults})
	if err != nil {
		t.Fatal(err)
	}
	waitDeployment(t, m, d.ID)
	m.mu.Lock()
	saved := m.data.Apps[a.ID]
	m.mu.Unlock()
	if !saved.DefaultsApplied || saved.Env["DB_MIGRATE"] != "false" || saved.Env["ADMIN_USERS"] != "app@example.com" || saved.Env["AUTH_OWNER_CLAIM"] != "" || saved.EnvSources["LOG_LEVEL"] != "workspace" {
		t.Fatalf("incorrect snapshot: %#v", saved.Env)
	}
	if _, exists := saved.Env["MAIL_FROM"]; exists {
		t.Fatal("injected empty default")
	}
	secret := saved.Env["AUTH_SECRET"]
	m.Close()
	restarted, err := NewManager(context.Background(), m.cfg, &fakeRuntime{fail: true})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	d, err = restarted.Enqueue(a.ID, DeployRequest{Defaults: map[string]string{"LOG_LEVEL": "debug", "MAIL_FROM": "later@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	waitDeployment(t, restarted, d.ID)
	restarted.mu.Lock()
	after := restarted.data.Apps[a.ID]
	restarted.mu.Unlock()
	if !maps.Equal(saved.Env, after.Env) || after.Env["AUTH_SECRET"] != secret {
		t.Fatal("retry or restart changed snapshot")
	}
	if err := restarted.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, EnvChanges: map[string]*string{"LOG_LEVEL": ptrString("warn")}}); err != nil {
		t.Fatal(err)
	}
	settings, _ := restarted.Settings(a.ID)
	if settings.EnvSources["LOG_LEVEL"] != "" {
		t.Fatal("explicit change retained workspace origin")
	}
}

func TestDefaultsExistingAppsAndReview(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	for _, kind := range []string{"live", "failed", "fresh"} {
		a := testApp(kind)
		a.Env["DB_MIGRATE"] = "false"
		if err := m.Upsert(a); err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		if kind == "live" {
			x := m.data.Apps[a.ID]
			x.Current = &Release{ID: "old"}
			m.data.Apps[a.ID] = x
		}
		if kind == "failed" {
			m.data.Deployments = append(m.data.Deployments, Deployment{ID: "old", AppID: a.ID, Status: "failed"})
		}
		x := m.initialDefaults(m.data.Apps[a.ID], map[string]string{"ADMIN_USERS": "shared@example.com", "DB_MIGRATE": "true"})
		m.mu.Unlock()
		if x.Env["DB_MIGRATE"] != "false" {
			t.Fatal("overwrote explicit migration opt-out")
		}
		if (x.Env["ADMIN_USERS"] != "") != (kind == "fresh") {
			t.Fatal("applied defaults to previously deployed app")
		}
	}
	value := "shared@example.com"
	patch := SettingsPatch{Branch: "main", Domain: "fresh.example.com", FromDefaults: true, OnlyMissing: true, EnvChanges: map[string]*string{"ADMIN_USERS": &value, "DB_MIGRATE": ptrString("true")}}
	if err := m.PatchSettings("fresh", patch); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a := m.data.Apps["fresh"]
	m.mu.Unlock()
	if a.Env["DB_MIGRATE"] != "false" || a.Env["ADMIN_USERS"] != value {
		t.Fatal("missing-only application overwrote existing values")
	}
	patch.OnlyMissing = false
	if err := m.PatchSettings("fresh", patch); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps["fresh"]
	m.mu.Unlock()
	if a.Env["DB_MIGRATE"] != "true" || a.EnvSources["ADMIN_USERS"] != "workspace" {
		t.Fatal("approved replacement not applied")
	}
	for _, key := range []string{"AUTH_SECRET", "LIDZA_MASTER_KEY", "DATABASE_URL", "CACHE_URL", "APP_URL"} {
		if ValidateAppDefaults(map[string]string{key: "shared"}) == nil {
			t.Fatal("accepted shared secret or attachment", key)
		}
	}
	if ValidateAppDefaults(map[string]string{"DB_MIGRATE": "yes"}) == nil {
		t.Fatal("accepted invalid boolean")
	}
}
func ptrString(s string) *string { return &s }

func TestDefaultsRejectedQueueDoesNotConsumeSnapshot(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("full")
	for i := 0; i < 97; i++ {
		a.Env[fmt.Sprintf("VAR_%d", i)] = "value"
	}
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Enqueue(a.ID, DeployRequest{Defaults: map[string]string{"LOG_LEVEL": "info"}}); err == nil {
		t.Fatal("accepted oversized environment")
	}
	m.mu.Lock()
	saved := m.data.Apps[a.ID]
	m.mu.Unlock()
	if saved.DefaultsApplied || saved.Env["LOG_LEVEL"] != "" {
		t.Fatal("failed queue consumed defaults snapshot")
	}
}
