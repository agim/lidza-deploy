package agent

// Native applications on a container host. The hosting installer used to set
// a server up for containers only: native management (applications built and
// run directly on the host) needs a deployment account and its root named in
// the agent's configuration, which nothing created. The installer now does
// that for new servers; POST /v1/host/native does it for a server that is
// already running, then restarts the agent so the configuration is loaded
// the same way as at every start.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// nativeDefaultUser is the deployment account of installer-prepared hosts;
// managed SSH access uses the same account.
const nativeDefaultUser = "deploy"

// HostNativeStatus reports whether this server can run native applications.
type HostNativeStatus struct {
	Configured bool   `json:"configured"`
	User       string `json:"user,omitempty"`
	Root       string `json:"root,omitempty"`
	// Supported is false when this agent cannot enable native management
	// itself; Reason says why.
	Supported  bool   `json:"supported"`
	Reason     string `json:"reason,omitempty"`
	Restarting bool   `json:"restarting,omitempty"`
}

var (
	hostNativeEUID = os.Geteuid
	// hostNativeRestart restarts this agent's own unit without waiting for
	// it; tests replace it.
	hostNativeRestart = restartOwnUnit
	ownUnitPattern    = regexp.MustCompile(`/([A-Za-z0-9_.@-]+\.service)$`)
)

func (m *Manager) hostNativeStatus() HostNativeStatus {
	if m.cfg.Native != nil {
		return HostNativeStatus{Configured: true, User: m.cfg.Native.User, Root: m.cfg.Native.Root, Supported: true}
	}
	status := HostNativeStatus{Supported: true}
	switch {
	case m.cfg.IngressOnly:
		status.Supported, status.Reason = false, "This agent only reads the host's web routes; it does not run applications."
	case runtime.GOOS != "linux" || hostNativeEUID() != 0:
		status.Supported, status.Reason = false, "The agent does not run as root on this server, so it cannot create the deployment account."
	case m.cfg.nativeConfigPath == "":
		status.Supported, status.Reason = false, "The agent's configuration file is unknown."
	}
	return status
}

// enableHostNative creates the deployment account when it is missing, names
// it and its home in the configuration file, and restarts the agent.
func (m *Manager) enableHostNative(ctx context.Context) (HostNativeStatus, error) {
	status := m.hostNativeStatus()
	if status.Configured {
		return status, nil
	}
	if !status.Supported {
		return status, errors.New(status.Reason)
	}
	nativeCreateMu.Lock()
	defer nativeCreateMu.Unlock()
	native, err := provisionNativeHost(ctx, m.cfg.nativeConfigPath, m.nativeRootExec)
	if err != nil {
		return status, err
	}
	if err := hostNativeRestart(); err != nil {
		return status, fmt.Errorf("native applications are configured, but the agent could not restart itself (%v); restart the agent service", err)
	}
	return HostNativeStatus{User: native.User, Root: native.Root, Supported: true, Restarting: true}, nil
}

// provisionNativeHost creates the deployment account when it is missing and
// names it and its home in the configuration file.
func provisionNativeHost(ctx context.Context, configPath string, run func(context.Context, ...string) (string, error)) (NativeHostConfig, error) {
	account, err := nativeLookupUser(nativeDefaultUser)
	if err != nil {
		// The same account managed SSH access creates: a login shell and no password.
		if out, err := run(ctx, "useradd", "--create-home", "--shell", "/bin/bash", "--password", "*", nativeDefaultUser); err != nil {
			return NativeHostConfig{}, fmt.Errorf("the deployment account could not be created: %s", lastLines(out))
		}
		if account, err = nativeLookupUser(nativeDefaultUser); err != nil {
			return NativeHostConfig{}, errors.New("the deployment account was created but cannot be looked up")
		}
	}
	native := NativeHostConfig{Root: filepath.Clean(account.HomeDir), User: nativeDefaultUser}
	if err := native.validate(); err != nil {
		return NativeHostConfig{}, err
	}
	if resolved, err := filepath.EvalSymlinks(native.Root); err != nil || resolved != native.Root {
		return NativeHostConfig{}, fmt.Errorf("the deployment account's home %s must exist and must not be a symbolic link", native.Root)
	}
	if err := writeNativeConfig(configPath, native); err != nil {
		return NativeHostConfig{}, err
	}
	return native, nil
}

// installerDataDir is the agent data directory of servers the hosting
// installer set up; other layouts are configured by whoever built them.
const installerDataDir = "/var/lib/lidza-agent"

// InstallerServer reports a server the hosting installer set up, whose agent
// runs as root and may keep it ready on its own.
func InstallerServer(cfg Config) bool {
	return !cfg.IngressOnly && cfg.DataDir == installerDataDir && runtime.GOOS == "linux" && hostNativeEUID() == 0
}

// EnsureNativeHost makes a server the hosting installer set up ready for
// native applications when its agent starts: servers installed before the
// installer did this get the deployment account and the native section with
// their next agent update, without anyone asking. It reports what it did;
// on any failure the agent starts as before and the server's Agent tab
// offers the same step with the reason.
func EnsureNativeHost(ctx context.Context, cfg *Config) (enabled bool, err error) {
	if cfg.Native != nil || cfg.IngressOnly || cfg.DataDir != installerDataDir || cfg.nativeConfigPath == "" || runtime.GOOS != "linux" || hostNativeEUID() != 0 {
		return false, nil
	}
	native, err := provisionNativeHost(ctx, cfg.nativeConfigPath, func(ctx context.Context, args ...string) (string, error) {
		return hostCommand(ctx, args[0], args[1:]...)
	})
	if err != nil {
		return false, err
	}
	cfg.Native = &native
	return true, nil
}

// writeNativeConfig adds the native section to the configuration file,
// keeping every other key, its mode and its owner. The result must load like
// any configuration before it replaces the file.
func writeNativeConfig(path string, native NativeHostConfig) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("the agent's configuration file is not a regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return errors.New("the agent's configuration file is not plain JSON; add the native section by hand")
	}
	if _, exists := document["native"]; exists {
		return errors.New("the configuration file already has a native section; restart the agent to load it")
	}
	if document["native"], err = json.Marshal(native); err != nil {
		return err
	}
	next, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(append(next, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if _, err := LoadConfig(temp.Name()); err != nil {
		return fmt.Errorf("the new configuration does not load: %w", err)
	}
	if err := os.Chmod(temp.Name(), info.Mode().Perm()); err != nil {
		return err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(temp.Name(), int(stat.Uid), int(stat.Gid)); err != nil && hostNativeEUID() == 0 {
			return err
		}
	}
	return os.Rename(temp.Name(), path)
}

// restartOwnUnit queues a restart of the systemd unit this process runs in
// and returns at once; systemd then stops and starts the agent.
func restartOwnUnit() error {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	unit := ""
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if match := ownUnitPattern.FindStringSubmatch(line); match != nil {
			unit = match[1]
		}
	}
	if unit == "" {
		return errors.New("the agent does not run as a systemd service")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = hostCommand(ctx, "systemctl", "--no-block", "restart", unit)
	return err
}

func (m *Manager) hostNativeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/host/native", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, m.hostNativeStatus()) })
	mux.HandleFunc("POST /v1/host/native", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		status, err := m.enableHostNative(ctx)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		code := 200
		if status.Restarting {
			code = 202
		}
		JSON(w, code, status)
	})
}
