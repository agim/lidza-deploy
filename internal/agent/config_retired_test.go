package agent

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// host_mode no longer changes behaviour: any value loads with one notice,
// and the agent manages its host fully. native_only is refused, so an
// observer configuration never starts silently as a full manager.
func TestRetiredConfigKeys(t *testing.T) {
	t.Setenv("LIDZA_AGENT_TOKEN", strings.Repeat("x", 32))
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	path := filepath.Join(t.TempDir(), "config.json")
	for _, mode := range []string{"managed", "coexist", "anything"} {
		logs.Reset()
		if err := os.WriteFile(path, []byte(`{"host_mode":"`+mode+`","data_dir":"/var/lib/lidza-agent"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err != nil {
			t.Fatal(mode, err)
		}
		if strings.Count(logs.String(), "host_mode is no longer used") != 1 {
			t.Fatalf("%s: want one deprecation notice, got %q", mode, logs.String())
		}
	}
	logs.Reset()
	if err := os.WriteFile(path, []byte(`{"data_dir":"/var/lib/lidza-agent"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil || logs.Len() != 0 {
		t.Fatal("plain configuration logged a notice or failed", err, logs.String())
	}
	if err := os.WriteFile(path, []byte(`{"native_only":true,"native":{"root":"/home/deploy","user":"deploy"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "native_only is no longer supported") {
		t.Fatal("native_only accepted", err)
	}
}

// Host operations are available to every full agent; only an ingress
// reader refuses them.
func TestHostOperationsAllowedForEveryFullAgent(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if err := m.hostOperationsAllowed(); err != nil {
		t.Fatal(err)
	}
	if err := m.setMaintenancePolicy(defaultMaintenancePolicy()); err != nil {
		t.Fatal(err)
	}
	m.cfg.IngressOnly = true
	if m.hostOperationsAllowed() == nil {
		t.Fatal("ingress-only agent allowed host operations")
	}
}
