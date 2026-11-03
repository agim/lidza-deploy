package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agim/lidza/pkg/credentials"
)

func TestMigrationInspectorReadsLegacyStateWithoutAPITokenOrMutation(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("legacy")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	a.Current = &Release{ID: "live", DatabaseIDs: []string{"retained"}}
	m.data.Apps[a.ID] = a
	m.data.Databases["retained"] = Database{Ready: true, Mode: "local"}
	err := m.save()
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	stateBefore, _ := os.ReadFile(m.path())
	keyBefore, _ := os.ReadFile(filepath.Join(m.cfg.DataDir, credentials.MasterKeyFile))
	cfg := filepath.Join(t.TempDir(), "existing.json")
	data, _ := json.Marshal(Config{DataDir: m.cfg.DataDir, Listen: "127.0.0.1:9090", ProxyListen: "127.0.0.1:8081"})
	if err := os.WriteFile(cfg, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIDZA_AGENT_TOKEN", "")
	plans, err := InspectMigrationPlans(context.Background(), cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].AppID != a.ID || len(plans[0].Databases) != 1 || plans[0].ExecutionSupported {
		t.Fatal("legacy dependencies omitted or execution enabled")
	}
	stateAfter, _ := os.ReadFile(m.path())
	keyAfter, _ := os.ReadFile(filepath.Join(m.cfg.DataDir, credentials.MasterKeyFile))
	if string(stateAfter) != string(stateBefore) || string(keyBefore) != string(keyAfter) {
		t.Fatal("inspection changed source inventory or key")
	}
	if _, err := InspectMigrationPlans(context.Background(), cfg, "missing"); err == nil {
		t.Fatal("unknown app accepted")
	}
}
