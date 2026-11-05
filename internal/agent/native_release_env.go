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

// nativeReleaseEnvironment composes the complete environment for build,
// release and schedule commands from the policy's literal environment files.
// Values are never logged.
func nativeReleaseEnvironment(account nativeReleaseAccount, policy NativeReleasePolicy) ([]string, error) {
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
	values["PATH"] = nativeReleaseSearchPath(account.Home)
	if policy.ManagedToolchains() {
		// The agent links the release's verified toolchains under
		// <root>/.lidza-build before any step runs; caches stay inside the
		// application root, the only tree a build may write.
		values["PATH"] = nativeManagedBuildPath(policy.Root)
		for key, value := range nativeManagedCacheEnvironment(policy.Root) {
			values[key] = value
		}
	}
	values["HOME"] = account.Home
	values["USER"] = account.Name
	values["LOGNAME"] = account.Name
	if _, ok := values["LANG"]; !ok {
		values["LANG"] = "C.UTF-8"
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
