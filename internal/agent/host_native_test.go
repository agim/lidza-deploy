package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnableHostNativeCreatesTheAccountWritesTheConfigurationAndRestarts(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	dir := t.TempDir()
	home := filepath.Join(dir, "home-deploy")
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte(`{"listen":"127.0.0.1:9090","proxy_listen":"127.0.0.1:8081","data_dir":"`+dir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.cfg.nativeConfigPath = config
	t.Setenv("LIDZA_AGENT_TOKEN", strings.Repeat("k", 32)) // the service environment supplies it; a configuration only loads with it
	savedLookup, savedEUID, savedRestart := nativeLookupUser, hostNativeEUID, hostNativeRestart
	t.Cleanup(func() { nativeLookupUser, hostNativeEUID, hostNativeRestart = savedLookup, savedEUID, savedRestart })
	created, restarts := false, 0
	var commands [][]string
	nativeLookupUser = func(name string) (*user.User, error) {
		if name != "deploy" || !created {
			return nil, user.UnknownUserError(name)
		}
		return &user.User{Username: "deploy", HomeDir: home}, nil
	}
	m.nativeCommand = func(_ context.Context, args []string) (string, error) {
		commands = append(commands, args)
		if args[0] == "useradd" {
			created = true
			return "", os.Mkdir(home, 0o750)
		}
		return "", errors.New("unexpected command")
	}
	hostNativeRestart = func() error { restarts++; return nil }

	// An agent that is not root cannot create the account: it says so and changes nothing.
	hostNativeEUID = func() int { return 1000 }
	if status := m.hostNativeStatus(); status.Configured || status.Supported || !strings.Contains(status.Reason, "root") {
		t.Fatalf("non-root status %+v", status)
	}
	if _, err := m.enableHostNative(context.Background()); err == nil || len(commands) != 0 || restarts != 0 {
		t.Fatalf("non-root agent enabled native management: %v %v", err, commands)
	}

	hostNativeEUID = func() int { return 0 }
	if status := m.hostNativeStatus(); status.Configured || !status.Supported {
		t.Fatalf("status before enabling %+v", status)
	}
	status, err := m.enableHostNative(context.Background())
	if err != nil || !status.Restarting || status.User != "deploy" || status.Root != home {
		t.Fatalf("enable: %v %+v", err, status)
	}
	// The same account managed SSH access creates: a login shell, no password.
	if len(commands) != 1 || strings.Join(commands[0], " ") != "useradd --create-home --shell /bin/bash --password * deploy" || restarts != 1 {
		t.Fatalf("commands %v restarts %d", commands, restarts)
	}
	// The configuration keeps its other keys and its mode, and loads.
	raw, _ := os.ReadFile(config)
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil || document["listen"] != "127.0.0.1:9090" || document["data_dir"] != dir {
		t.Fatalf("configuration lost keys: %v %s", err, raw)
	}
	loaded, err := LoadConfig(config)
	if err != nil || loaded.Native == nil || loaded.Native.User != "deploy" || loaded.Native.Root != home {
		t.Fatalf("written configuration: %v %+v", err, loaded.Native)
	}
	if info, _ := os.Stat(config); info.Mode().Perm() != 0o600 {
		t.Fatalf("configuration mode %v", info.Mode())
	}
	// Until the restart loads it, a second request must not write it twice.
	if _, err := m.enableHostNative(context.Background()); err == nil || !strings.Contains(err.Error(), "already has a native section") || restarts != 1 {
		t.Fatalf("second enable: %v restarts %d", err, restarts)
	}
	// After the restart the agent reports it as configured and enabling is a no-op.
	m.cfg.Native = loaded.Native
	if status, err := m.enableHostNative(context.Background()); err != nil || !status.Configured || status.Restarting || restarts != 1 || len(commands) != 1 {
		t.Fatalf("configured agent: %v %+v", err, status)
	}
}

func TestAgentStartEnablesNativeApplicationsOnInstallerServersOnly(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home-deploy")
	if err := os.Mkdir(home, 0o750); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config.json")
	write := func() {
		if err := os.WriteFile(config, []byte(`{"listen":"127.0.0.1:9090","data_dir":"/var/lib/lidza-agent"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	t.Setenv("LIDZA_AGENT_TOKEN", strings.Repeat("k", 32))
	savedLookup, savedEUID, savedCommand := nativeLookupUser, hostNativeEUID, hostCommand
	t.Cleanup(func() { nativeLookupUser, hostNativeEUID, hostCommand = savedLookup, savedEUID, savedCommand })
	nativeLookupUser = func(string) (*user.User, error) { return &user.User{Username: "deploy", HomeDir: home}, nil }
	hostCommand = func(context.Context, string, ...string) (string, error) {
		t.Fatal("an existing deployment account was created again")
		return "", nil
	}
	hostNativeEUID = func() int { return 0 }
	untouched := func(name string, cfg Config) {
		t.Helper()
		if enabled, err := EnsureNativeHost(context.Background(), &cfg); enabled || err != nil || cfg.Native != nil {
			t.Fatalf("%s: enabled=%v err=%v native=%+v", name, enabled, err, cfg.Native)
		}
		if raw, _ := os.ReadFile(config); strings.Contains(string(raw), "native") {
			t.Fatalf("%s: the configuration file was changed", name)
		}
	}
	// Other layouts, readers of the web routes and non-root agents are left as they are.
	untouched("custom data directory", Config{DataDir: "/var/lib/other-agent", nativeConfigPath: config})
	untouched("ingress only", Config{DataDir: installerDataDir, IngressOnly: true, nativeConfigPath: config})
	hostNativeEUID = func() int { return 1000 }
	untouched("not root", Config{DataDir: installerDataDir, nativeConfigPath: config})
	hostNativeEUID = func() int { return 0 }
	// A server the installer set up before it prepared native applications.
	cfg := Config{DataDir: installerDataDir, nativeConfigPath: config}
	if enabled, err := EnsureNativeHost(context.Background(), &cfg); !enabled || err != nil || cfg.Native == nil || cfg.Native.Root != home || cfg.Native.User != "deploy" {
		t.Fatalf("installer server: enabled=%v err=%v native=%+v", enabled, err, cfg.Native)
	}
	loaded, err := LoadConfig(config)
	if err != nil || loaded.Native == nil || loaded.Native.Root != home {
		t.Fatalf("configuration after start: %v %+v", err, loaded.Native)
	}
	// The next start finds it configured and does nothing.
	if enabled, err := EnsureNativeHost(context.Background(), &loaded); enabled || err != nil {
		t.Fatalf("second start: enabled=%v err=%v", enabled, err)
	}
	// A failure leaves the agent container-only; it still starts.
	write()
	nativeLookupUser = func(name string) (*user.User, error) { return nil, user.UnknownUserError(name) }
	hostCommand = func(context.Context, string, ...string) (string, error) {
		return "useradd: cannot lock /etc/passwd", errors.New("exit 1")
	}
	cfg = Config{DataDir: installerDataDir, nativeConfigPath: config}
	if enabled, err := EnsureNativeHost(context.Background(), &cfg); enabled || err == nil || cfg.Native != nil {
		t.Fatalf("failed account creation: enabled=%v err=%v", enabled, err)
	}
}
