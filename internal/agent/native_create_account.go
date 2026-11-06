package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/agim/lidza-deploy/internal/release"
)

// Every created application runs as its own system account, app-<id>, with
// its own group and no login shell. Its root <apps_root>/<id> is the
// account's home: owned by the account, mode 0750, so other applications
// cannot read it. The root agent writes the environment file
// (<root>/shared/.env, 0600) and the service units; the account owns
// everything the build and the processes write.

const (
	nativeDefaultAppsRoot = "/srv/lidza/apps"
	nativeAppUserPrefix   = "app-"
	// nativeCreatedIDLimit keeps app-<id> within the 32-character account
	// name limit.
	nativeCreatedIDLimit = 32 - len(nativeAppUserPrefix)
)

var nativeDefaultAppPorts = [2]int{4100, 4999}

// nativeForbiddenAppsRoots are trees an apps_root cannot be inside: the
// hardened units hide home directories, and system trees are not ours.
var nativeForbiddenAppsRoots = []string{"/home", "/root", "/run", "/tmp", "/proc", "/sys", "/dev", "/etc", "/usr", "/boot", "/bin", "/sbin", "/lib", "/var/tmp"}

func init() {
	ensureNativeAppAccount = func(ctx context.Context, m *Manager, id string) (NativeAppAccount, error) {
		return m.ensureNativeAppAccount(ctx, id)
	}
}

// nativeAppUser is the account of a created application.
func nativeAppUser(id string) string { return nativeAppUserPrefix + id }

// nativeAppsRoot is the parent directory of created application roots.
func (m *Manager) nativeAppsRoot() string {
	if m.cfg.Native != nil && m.cfg.Native.AppsRoot != "" {
		return m.cfg.Native.AppsRoot
	}
	return nativeDefaultAppsRoot
}

// validNativeAppsRoot rejects roots the hardened units cannot use: home
// directories are hidden from them, and system trees are not ours.
func validNativeAppsRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || strings.ContainsAny(root, " \t\r\n\"'\\$%;:") {
		return errors.New("native apps_root must be a clean absolute path")
	}
	for _, forbidden := range nativeForbiddenAppsRoots {
		if root == forbidden || strings.HasPrefix(root, forbidden+"/") {
			return fmt.Errorf("native apps_root cannot be inside %s", forbidden)
		}
	}
	return nil
}

func validNativeCreatedID(id string) error {
	if !idPattern.MatchString(id) || len(id) > nativeCreatedIDLimit || strings.HasSuffix(id, "-") {
		return fmt.Errorf("application id must be a lowercase slug of at most %d characters", nativeCreatedIDLimit)
	}
	return nil
}

func (m *Manager) ensureNativeAppAccount(ctx context.Context, id string) (NativeAppAccount, error) {
	if err := validNativeCreatedID(id); err != nil {
		return NativeAppAccount{}, err
	}
	base := m.nativeAppsRoot()
	if err := validNativeAppsRoot(base); err != nil {
		return NativeAppAccount{}, err
	}
	if m.cfg.Native != nil && (base == m.cfg.Native.Root || strings.HasPrefix(m.cfg.Native.Root, base+"/")) {
		return NativeAppAccount{}, errors.New("native apps_root must be separate from the existing application root")
	}
	// Accounts must traverse the base to reach their own root.
	if err := release.MkdirAllReadable(base); err != nil {
		return NativeAppAccount{}, err
	}
	if real, err := filepath.EvalSymlinks(base); err != nil || real != base {
		return NativeAppAccount{}, errors.New("native apps_root must not be or contain a symlink")
	}
	if err := os.Chmod(base, 0o755); err != nil {
		return NativeAppAccount{}, err
	}
	root := filepath.Join(base, id)
	name := nativeAppUser(id)
	existing, err := nativeLookupUser(name)
	var unknown user.UnknownUserError
	switch {
	case errors.As(err, &unknown):
		if _, err := m.nativeRootExec(ctx, "useradd", "--system", "--user-group", "--home-dir", root, "--no-create-home",
			"--shell", nativeNoLoginShell(), "--comment", "Lidza application "+id, name); err != nil {
			return NativeAppAccount{}, fmt.Errorf("could not create the account %s", name)
		}
		if existing, err = nativeLookupUser(name); err != nil {
			return NativeAppAccount{}, fmt.Errorf("the account %s is unavailable after creation", name)
		}
	case err != nil:
		return NativeAppAccount{}, fmt.Errorf("could not look up the account %s", name)
	case existing.HomeDir != root:
		// Never adopt an account that belongs to something else.
		return NativeAppAccount{}, fmt.Errorf("the account %s already exists with another home directory", name)
	}
	uid, err := strconv.Atoi(existing.Uid)
	if err != nil || uid <= 0 {
		return NativeAppAccount{}, fmt.Errorf("the account %s must not be root", name)
	}
	gid, err := strconv.Atoi(existing.Gid)
	if err != nil || gid <= 0 {
		return NativeAppAccount{}, fmt.Errorf("the account %s needs its own group", name)
	}
	account := NativeAppAccount{User: name, UID: uid, GID: gid, Home: root, Root: root}
	if err := prepareNativeAppRoot(account); err != nil {
		return NativeAppAccount{}, err
	}
	return account, nil
}

func nativeNoLoginShell() string {
	for _, shell := range []string{"/usr/sbin/nologin", "/sbin/nologin"} {
		if _, err := os.Stat(shell); err == nil {
			return shell
		}
	}
	return "/bin/false"
}

// prepareNativeAppRoot creates the root, shared/ and the environment file,
// all owned by the account. A root left by an earlier account with the same
// name is handed over to the current one.
func prepareNativeAppRoot(account NativeAppAccount) error {
	info, err := os.Lstat(account.Root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(account.Root, 0o750); err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir() || info.Mode()&os.ModeSymlink != 0:
		return errors.New("the application root exists and is not a directory")
	default:
		if owner, ok := nativeFileOwner(info); ok && owner != account.UID {
			if err := nativeChownTree(account.Root, account.UID, account.GID); err != nil {
				return err
			}
		}
	}
	shared := filepath.Join(account.Root, "shared")
	for _, dir := range []string{account.Root, shared} {
		if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s must be a directory", dir)
		}
		if err := os.Chown(dir, account.UID, account.GID); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o750); err != nil {
			return err
		}
	}
	env := filepath.Join(shared, ".env")
	if info, err := os.Lstat(env); err == nil && !info.Mode().IsRegular() {
		return errors.New("the application environment file must be a regular file")
	}
	file, err := os.OpenFile(env, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chown(account.UID, account.GID); err != nil {
		return err
	}
	return file.Chmod(0o600)
}

func nativeFileOwner(info fs.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}

func nativeChownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}

// removeNativeAppAccount deletes the account (never its files; the root is
// removed separately, only on purge) and its crontab.
func (m *Manager) removeNativeAppAccount(ctx context.Context, id string) error {
	name := nativeAppUser(id)
	existing, err := nativeLookupUser(name)
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.HomeDir != filepath.Join(m.nativeAppsRoot(), id) {
		return fmt.Errorf("the account %s does not belong to this application", name)
	}
	_, _ = m.nativeRootExec(ctx, "crontab", "-u", name, "-r")
	if _, err := m.nativeRootExec(ctx, "userdel", name); err != nil {
		return fmt.Errorf("could not remove the account %s", name)
	}
	return nil
}

// nativeAppPorts is the configured loopback range for created web processes.
func (m *Manager) nativeAppPorts() (int, int) {
	ports := nativeDefaultAppPorts
	if m.cfg.Native != nil && m.cfg.Native.AppPorts != [2]int{} {
		ports = m.cfg.Native.AppPorts
	}
	return ports[0], ports[1]
}

// nativePortListenable reports whether a loopback port is free right now;
// tests replace it.
var nativePortListenable = func(port int) bool {
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	listener.Close()
	return true
}

// allocateNativePortLocked picks the lowest free port of the range that no
// application, release or listener of this agent uses. The caller holds
// m.mu and persists the port with the created policy.
func (m *Manager) allocateNativePortLocked() (string, error) {
	low, high := m.nativeAppPorts()
	if low < 1024 || high > 65535 || low > high {
		return "", errors.New("native app_ports must be an increasing range between 1024 and 65535")
	}
	used := m.reservedPortsLocked()
	for port := low; port <= high; port++ {
		if !used[strconv.Itoa(port)] && nativePortListenable(port) {
			return strconv.Itoa(port), nil
		}
	}
	return "", errors.New("no free loopback port is left in the configured app_ports range")
}

// reservedPortsLocked lists the ports that applications, releases and this
// agent's listeners use or will use again, whether or not they listen right
// now (a stopped application keeps its port). The caller holds m.mu.
func (m *Manager) reservedPortsLocked() map[string]bool {
	used := map[string]bool{}
	for _, a := range m.data.NativeApps {
		used[a.Port] = true
	}
	for _, policy := range m.data.NativeCreated {
		used[policy.Port] = true
	}
	if m.cfg.Native != nil {
		for _, policy := range m.cfg.Native.Releases {
			used[policy.Port] = true
		}
	}
	for _, a := range m.data.Apps {
		for _, r := range []*Release{a.Current, a.Previous} {
			if r != nil {
				used[r.Port] = true
			}
		}
	}
	for _, listener := range []string{m.cfg.Listen, m.cfg.ProxyListen, m.cfg.TLSListen} {
		if _, port, err := net.SplitHostPort(listener); err == nil {
			used[port] = true
		}
	}
	return used
}
