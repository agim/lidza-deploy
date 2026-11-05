package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoexistStartupAndCleanupDoNotRunHostCommands(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "invoked")
	for _, name := range []string{"docker", "journalctl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf invoked >> '"+marker+"'\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	root := t.TempDir()
	cfg := Config{HostMode: "coexist", DataDir: root, APIKey: strings.Repeat("x", 32)}
	m, err := NewManager(context.Background(), cfg, &Docker{Root: filepath.Join(root, "builds")})
	if err != nil {
		t.Fatal(err)
	}
	m.StartMaintenance()
	m.maintenanceTick(time.Now())
	if err := m.startMaintenance(CleanupRequest{Backups: true, BuildCache: true, Logs: true}, time.Now()); err == nil {
		t.Fatal("host cleanup accepted")
	}
	if _, err := m.performMaintenance(context.Background(), CleanupRequest{BuildCache: true, Logs: true}); err == nil {
		t.Fatal("direct host cleanup accepted")
	}
	if err := m.setMaintenancePolicy(defaultMaintenancePolicy()); err == nil {
		t.Fatal("host maintenance policy enabled")
	}
	if !m.maintenanceStatus().HostAutomationBlocked {
		t.Fatal("missing blocked capability")
	}
	m.queueSwapCheck()
	m.migrateConsoleService()
	m.migrateControlBootService()
	m.Close()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("coexist startup or cleanup ran a host command")
	}
	if _, err := os.Stat(filepath.Join(root, "upgrade", "request")); !os.IsNotExist(err) {
		t.Fatal("host migration request queued")
	}
}

func TestCoexistPreservesManagedDefaultsAndRejectsUnknownMode(t *testing.T) {
	for _, mode := range []string{"", "managed", "coexist"} {
		if err := (Config{HostMode: mode}).validateHostMode(); err != nil {
			t.Fatal(mode, err)
		}
	}
	root := filepath.Join(t.TempDir(), "must-not-create")
	if m, err := NewManager(context.Background(), Config{HostMode: "coexsit", DataDir: root}, &fakeRuntime{}); err == nil {
		m.Close()
		t.Fatal("unknown host mode accepted")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("unknown host mode wrote state")
	}
	t.Setenv("LIDZA_AGENT_TOKEN", strings.Repeat("x", 32))
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"host_mode":"coexsit"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("unknown host mode loaded")
	}
}
