package agent

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// nativeReleaseAccount is the application owner that runs release commands.
type nativeReleaseAccount struct {
	Name string
	Home string
	UID  int
	GID  int
}

// nativeReleaseAccount resolves the account of one policy: its own User
// when set, otherwise the native host's deployment user.
func (m *Manager) nativeReleaseAccount(policy NativeReleasePolicy) (nativeReleaseAccount, error) {
	if m.cfg.Native == nil {
		return nativeReleaseAccount{}, errors.New("native management is not configured")
	}
	name := m.cfg.Native.User
	if policy.User != "" {
		name = policy.User
	}
	account, err := nativeLookupUser(name)
	if err != nil {
		return nativeReleaseAccount{}, errors.New("native deployment user is unavailable")
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid <= 0 {
		return nativeReleaseAccount{}, errors.New("native releases require a non-root user")
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return nativeReleaseAccount{}, err
	}
	if !filepath.IsAbs(account.HomeDir) || strings.ContainsAny(account.HomeDir, " :\t\r\n") {
		return nativeReleaseAccount{}, errors.New("native deployment home directory needs review")
	}
	return nativeReleaseAccount{Name: account.Username, Home: account.HomeDir, UID: uid, GID: gid}, nil
}

// nativeLookupUser resolves an account; tests replace it.
var nativeLookupUser = user.Lookup

// nativeReleaseSearchPath is the owner's toolchain path: version-manager
// shims first, then the system directories.
func nativeReleaseSearchPath(home string) string {
	return home + "/.asdf/shims:" + home + "/.asdf/bin:/usr/local/bin:/usr/bin:/bin"
}

// nativeReleaseReservedEnvironment lists keys an application environment
// file may not override: they select which code runs before the application
// starts, and the release engine sets them itself.
func nativeReleaseReservedEnvironment(key string) bool {
	return slices.Contains([]string{"PATH", "HOME", "USER", "SHELL", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT",
		"BASH_ENV", "ENV", "BUNDLE_GEMFILE", "PWD", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}, key)
}

// nativeReleaseFileEnvironment reads the policy's literal environment
// files, later files overriding earlier ones, without the reserved keys.
// Values are never logged; nativeReleaseMask hides them when application
// output reaches the deploy log.
func nativeReleaseFileEnvironment(policy NativeReleasePolicy) (map[string]string, error) {
	values := map[string]string{}
	for _, path := range policy.EnvFiles {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 {
			return nil, fmt.Errorf("native release environment file %s must be a regular file under 128 KiB", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("native release environment file %s is unreadable", path)
		}
		if err := parseNativeLiteralEnvironment(data, values); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	for key, value := range values {
		if nativeReleaseReservedEnvironment(key) {
			delete(values, key)
			continue
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("native release environment contains line breaks or null bytes")
		}
	}
	return values, nil
}

// nativeReleaseEnvironment composes the complete environment for build,
// release and schedule commands from the policy's literal environment files.
func nativeReleaseEnvironment(account nativeReleaseAccount, policy NativeReleasePolicy) ([]string, error) {
	values, err := nativeReleaseFileEnvironment(policy)
	if err != nil {
		return nil, err
	}
	values["PATH"] = nativeReleaseSearchPath(account.Home)
	if policy.ManagedToolchains() {
		// The agent links the release's verified toolchains under
		// <root>/.lidza-build before any step runs; caches stay inside the
		// application root, the only tree a build may write.
		values["PATH"] = nativeManagedBuildPath(policy.Root)
		for key, value := range nativeManagedCacheEnvironment(policy.Root) {
			values[key] = value
		}
		if policy.Runtime == "rails" || policy.Runtime == "auto" {
			for key, value := range nativeManagedGemEnvironment(policy.Root) {
				values[key] = value
			}
		}
	}
	values["HOME"] = account.Home
	values["USER"] = account.Name
	values["LOGNAME"] = account.Name
	if _, ok := values["LANG"]; !ok {
		values["LANG"] = "C.UTF-8"
	}
	// Package managers' update and funding notices are noise in build logs
	// and read like warnings; an application can still turn them back on.
	for key, value := range map[string]string{"npm_config_update_notifier": "false", "npm_config_fund": "false", "NO_UPDATE_NOTIFIER": "1"} {
		if _, ok := values[key]; !ok {
			values[key] = value
		}
	}
	if _, set := values["npm_config_cache"]; !set || !policy.ManagedToolchains() {
		values["npm_config_cache"] = "/tmp/npm"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env, nil
}
