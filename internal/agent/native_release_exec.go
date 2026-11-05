package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// nativeReleaseExecutor runs release commands as the application owner inside
// a transient, resource-bounded systemd unit. Build steps run without network
// unless the step needs it; database steps are the only ones that can reach
// the application's databases. Nothing from the agent's environment leaks in.
type nativeReleaseExecutor struct {
	m       *Manager
	account nativeReleaseAccount
	root    string // application root; the only writable tree
	id      string // deployment identity, for unit names
	memory  int    // MiB per build command; 0 means the default
}

const nativeReleaseDefaultBuildMiB = 2048

func (x nativeReleaseExecutor) memoryMiB() int {
	if x.memory > 0 {
		return x.memory
	}
	return nativeReleaseDefaultBuildMiB
}

func (x nativeReleaseExecutor) Run(ctx context.Context, c release.Command) error {
	if x.m.nativeReleaseRun != nil {
		return x.m.nativeReleaseRun(ctx, c)
	}
	if os.Geteuid() != 0 {
		return errors.New("native release commands require the root agent")
	}
	if len(c.Args) == 0 || !filepath.IsAbs(c.Dir) || !nativeReleaseInside(x.root, c.Dir) {
		return fmt.Errorf("native release command %s must run inside the application root", c.Name)
	}
	// Node sizes its heap from the machine, not the unit's memory cap, and a
	// large front-end build then dies at the cap instead of collecting garbage.
	env := c.Env
	if !slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, "NODE_OPTIONS=") }) {
		env = append(append([]string{}, env...), "NODE_OPTIONS=--max-old-space-size="+strconv.Itoa(x.memoryMiB()*3/4))
	}
	envFile, err := nativeReleaseEnvironmentFile(x.root, x.account, env)
	if err != nil {
		return err
	}
	defer os.Remove(envFile)
	unit := "lidza-native-release-" + x.id + "-" + newID()[:8] + ".service"
	argv := nativeReleaseArgv(x.account, x.root, unit, envFile, c, x.memoryMiB())
	cmd := exec.CommandContext(ctx, "systemd-run", argv...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	if cmd.Stdout == nil {
		cmd.Stdout = io.Discard
	}
	cmd.Stderr = c.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = cmd.Stdout
	}
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	if ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = command(cleanup, "", nil, "systemctl", "stop", unit)
	}
	if err != nil {
		return fmt.Errorf("%s failed: %w", c.Name, err)
	}
	return nil
}

func nativeReleaseInside(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// nativeReleaseEnvironmentFile writes the command environment where only the
// owner can read it. The confined shell sources it after privileges drop.
func nativeReleaseEnvironmentFile(root string, account nativeReleaseAccount, env []string) (string, error) {
	file, err := os.CreateTemp(root, ".lidza-release-env-")
	if err != nil {
		return "", err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		os.Remove(file.Name())
		return "", err
	}
	if err := file.Chown(account.UID, account.GID); err != nil {
		os.Remove(file.Name())
		return "", err
	}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !envPattern.MatchString(key) || strings.ContainsAny(value, "\r\n\x00") {
			os.Remove(file.Name())
			return "", errors.New("native release environment entry is invalid")
		}
		if _, err := fmt.Fprintf(file, "export %s='%s'\n", key, strings.ReplaceAll(value, "'", "'\\''")); err != nil {
			os.Remove(file.Name())
			return "", err
		}
	}
	return file.Name(), nil
}

// nativeReleaseArgv builds the systemd-run invocation for one command.
func nativeReleaseArgv(account nativeReleaseAccount, root, unit, envFile string, c release.Command, memoryMiB ...int) []string {
	limit := int64(nativeReleaseDefaultBuildMiB) << 20
	if len(memoryMiB) > 0 && memoryMiB[0] > 0 {
		limit = int64(memoryMiB[0]) << 20
	}
	inaccessible := "/run/user /run/systemd/private /run/dbus/system_bus_socket -/var/run/docker.sock"
	if !c.Database {
		inaccessible += " -/run/postgresql -/var/run/postgresql -/run/mysqld -/var/run/mysqld -/run/redis -/run/mongodb"
	}
	searchPath := nativeReleaseSearchPath(account.Home)
	argv := []string{"--quiet", "--wait", "--pipe", "--collect", "--expand-environment=no", "--unit=" + unit,
		"--property=User=root", "--property=WorkingDirectory=" + c.Dir,
		"--property=MemoryMax=" + strconv.FormatInt(limit, 10), "--property=MemorySwapMax=0", "--property=OOMScoreAdjust=500",
		"--property=CPUQuota=50%", "--property=IOWeight=10", "--property=TasksMax=256", "--property=RuntimeMaxSec=1800",
		"--property=ProtectSystem=strict", "--property=ProtectHome=read-only", "--property=ReadWritePaths=" + root,
		"--property=TemporaryFileSystem=/tmp:rw,nosuid,nodev,size=512M,mode=1777",
		"--property=InaccessiblePaths=" + inaccessible,
		"--property=NoNewPrivileges=yes", "--property=CapabilityBoundingSet=CAP_SYS_ADMIN CAP_SETUID CAP_SETGID CAP_SETPCAP", "--property=KillMode=control-group",
		"--setenv=HOME=" + account.Home, "--setenv=PATH=" + searchPath}
	switch {
	case !c.Network && !c.Database:
		// No network at all: unix database sockets are hidden above.
		argv = append([]string{"--property=PrivateNetwork=yes", "--property=RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
			"--property=AmbientCapabilities=CAP_SYS_ADMIN CAP_SETUID CAP_SETGID CAP_SETPCAP"}, argv...)
	case !c.Database:
		// Package installs reach the internet but not loopback services.
		// systemd-run parses each property value as one prefix, so the two
		// symbolic ranges are separate properties (a space-separated list is
		// refused with "Failed to parse IP address prefix").
		// Name resolution commonly goes through a loopback stub resolver
		// (systemd-resolved at 127.0.0.53/127.0.0.54); allow entries take
		// precedence over the denied ranges, so only those addresses open.
		argv = append([]string{"--property=IPAddressDeny=localhost", "--property=IPAddressDeny=link-local",
			"--property=IPAddressAllow=127.0.0.53/32", "--property=IPAddressAllow=127.0.0.54/32"}, argv...)
	}
	// Database steps keep the host network: databases are commonly reached
	// over TCP on a private network or loopback, not only unix sockets.
	// Repository code starts only after setpriv irreversibly drops uid, gid
	// and every capability, inside private mount and PID namespaces.
	argv = append(argv, "--", "/usr/bin/unshare", "--mount", "--pid", "--fork", "--mount-proc", "--",
		"/usr/bin/setpriv", "--reuid="+strconv.Itoa(account.UID), "--regid="+strconv.Itoa(account.GID), "--init-groups",
		"--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all", "--no-new-privs", "--",
		"/usr/bin/env", "-i", "HOME="+account.Home, "PATH="+searchPath,
		"/bin/sh", "-c", "set -eu; set -a; . \"$1\"; set +a; shift; exec \"$@\"", "lidza-release-env", envFile)
	return append(argv, c.Args...)
}

// nativeReleaseUserExec runs short control commands (the owner's systemctl)
// as the application owner without a sandbox, because they must reach the
// owner's service manager socket. No PAM session is opened, so a stopped
// user manager is never started.
type nativeReleaseUserExec struct {
	m       *Manager
	account nativeReleaseAccount
}

func (x nativeReleaseUserExec) Run(ctx context.Context, c release.Command) error {
	if x.m.nativeReleaseUserRun != nil {
		return x.m.nativeReleaseUserRun(ctx, c)
	}
	if len(c.Args) == 0 {
		return errors.New("native release control command is empty")
	}
	env := append([]string{"-i"}, c.Env...)
	var cmd *exec.Cmd
	if os.Geteuid() == x.account.UID {
		cmd = exec.CommandContext(ctx, "/usr/bin/env", append(env, c.Args...)...)
	} else if os.Geteuid() == 0 {
		argv := []string{"--reuid=" + strconv.Itoa(x.account.UID), "--regid=" + strconv.Itoa(x.account.GID), "--init-groups", "--no-new-privs", "--", "/usr/bin/env"}
		cmd = exec.CommandContext(ctx, "/usr/bin/setpriv", append(append(argv, env...), c.Args...)...)
	} else {
		return errors.New("native release control commands require the root agent")
	}
	cmd.Dir = c.Dir
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	if cmd.Stdout == nil {
		cmd.Stdout = io.Discard
	}
	cmd.Stderr = c.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = cmd.Stdout
	}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", c.Name, err)
	}
	return nil
}

// nativeReleaseLog streams engine output into the deployment log, keeping
// the most recent output where failures are reported.
func nativeReleaseLog(ctx context.Context) io.Writer {
	writer := &limitedBuffer{}
	if start, ok := ctx.Value(diagnosticStreamKey{}).(func(string) func(string)); ok {
		writer.diagnostic = start("release")
	}
	return writer
}
