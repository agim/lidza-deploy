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
	"sync"
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
	app     string // application whose build peaks are learned
	memory  int    // MiB per build command from the policy; 0 adapts to the host
	// writable are further directories the command may change, for example
	// the account's Ruby gem directory when unused Bundler versions are
	// removed (native_bundler.go).
	writable []string
}

// Run runs one command under the host's build resources: build steps get
// a memory cap from free memory and their learned peak, wait for memory
// when it is short, and are retried once with more memory when they run
// out (build_resources.go).
func (x nativeReleaseExecutor) Run(ctx context.Context, c release.Command) error {
	// Resource lines go to the step's log, never to its standard output,
	// which can be data (git archive) or parsed (schedules).
	step := buildStep{App: x.app, Step: c.Name, Explicit: x.memory, Build: c.Build, Log: c.Stderr}
	if step.App == "" {
		step.App = filepath.Base(x.root)
	}
	return x.m.runBuildStep(ctx, step, func(ctx context.Context, limits buildLimits) (int, error) {
		if x.m.nativeReleaseRun != nil {
			return 0, x.m.nativeReleaseRun(ctx, c)
		}
		return x.runUnit(ctx, c, limits)
	})
}

// runUnit runs one attempt of c in a transient unit and reports its peak.
func (x nativeReleaseExecutor) runUnit(ctx context.Context, c release.Command, limits buildLimits) (int, error) {
	if os.Geteuid() != 0 {
		return 0, errors.New("native release commands require the root agent")
	}
	if len(c.Args) == 0 || !filepath.IsAbs(c.Dir) || !nativeReleaseInside(x.root, c.Dir) {
		return 0, fmt.Errorf("native release command %s must run inside the application root", c.Name)
	}
	// Node sizes its heap from the machine, not the unit's memory cap, and a
	// large front-end build then dies at the cap instead of collecting garbage.
	env := nativeReleaseNodeHeap(c.Env, limits.MemoryMiB)
	envFile, err := nativeReleaseEnvironmentFile(x.root, x.account, env)
	if err != nil {
		return 0, err
	}
	defer os.Remove(envFile)
	// Each attempt starts from a clean scratch directory, unless another
	// command of the same release is using it right now (the source export
	// streams while its consumer runs): the directory is then only kept.
	release, err := nativeScratchUse(x.root, x.account)
	if err != nil {
		return 0, err
	}
	defer release()
	unit := "lidza-native-release-" + x.id + "-" + newID()[:8] + ".service"
	argv := nativeReleaseArgv(x.account, x.root, unit, envFile, c, limits, x.writable...)
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
	stop := watchUnitMemory(unit)
	err = cmd.Run()
	sampled := stop()
	if ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = command(cleanup, "", nil, "systemctl", "stop", unit)
	}
	outcome := nativeReleaseUnitOutcome(unit, err != nil)
	peak := int(max(sampled, outcome.PeakBytes) >> 20)
	if err != nil {
		if outcome.OutOfMemory {
			return peak, fmt.Errorf("%s %w (build limit %d MiB)", c.Name, errBuildOutOfMemory, limits.MemoryMiB)
		}
		return peak, fmt.Errorf("%s failed: %w", c.Name, err)
	}
	return peak, nil
}

// nativeReleaseNodeHeap gives Node three quarters of the step's cap as its
// heap unless the environment already sets NODE_OPTIONS.
func nativeReleaseNodeHeap(env []string, memoryMiB int) []string {
	if memoryMiB <= 0 || slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, "NODE_OPTIONS=") }) {
		return env
	}
	return append(append([]string{}, env...), "NODE_OPTIONS=--max-old-space-size="+strconv.Itoa(memoryMiB*3/4))
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
func nativeReleaseArgv(account nativeReleaseAccount, root, unit, envFile string, c release.Command, limits buildLimits, writable ...string) []string {
	inaccessible := "/run/user /run/systemd/private /run/dbus/system_bus_socket -/var/run/docker.sock"
	if !c.Database {
		inaccessible += " -/run/postgresql -/var/run/postgresql -/run/mysqld -/var/run/mysqld -/run/redis -/run/mongodb"
	}
	searchPath := nativeReleaseSearchPath(account.Home)
	argv := []string{"--quiet", "--wait", "--pipe", "--collect", "--expand-environment=no", "--unit=" + unit,
		"--property=User=root", "--property=WorkingDirectory=" + c.Dir}
	argv = append(argv, buildResourceProperties(limits)...)
	argv = append(argv, "--property=TasksMax=256", "--property=RuntimeMaxSec=1800",
		"--property=ProtectSystem=strict", "--property=ProtectHome=read-only", "--property=ReadWritePaths="+strings.Join(append([]string{root}, writable...), " "),
		"--property=TemporaryFileSystem=/tmp:rw,nosuid,nodev,size=512M,mode=1777",
		"--property=InaccessiblePaths="+inaccessible,
		"--property=NoNewPrivileges=yes", "--property=CapabilityBoundingSet=CAP_SYS_ADMIN CAP_SETUID CAP_SETGID CAP_SETPCAP", "--property=KillMode=control-group",
		"--setenv=HOME="+account.Home, "--setenv=PATH="+searchPath)
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
		// Large temporary files (linkers, package builds) go to the
		// application's disk, not the small private /tmp.
		"TMPDIR="+nativeReleaseScratchDir(root), "GOTMPDIR="+nativeReleaseScratchDir(root),
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

// nativeReleaseScratchDir is a build step's temporary directory: on the
// application's disk, inside the writable root, so a linker or package build
// is not limited by the size of the sandbox's private /tmp.
func nativeReleaseScratchDir(root string) string { return filepath.Join(root, ".cache", "tmp") }

// nativeScratchUses counts the commands using each application root's
// scratch directory, so concurrent commands of one release never remove it
// from under each other.
var (
	nativeScratchMu   sync.Mutex
	nativeScratchUses = map[string]int{}
)

// nativeScratchUse prepares the scratch directory for a command and marks
// it in use until the returned function runs. The first user of a root
// empties the directory; later concurrent users only make sure it exists.
func nativeScratchUse(root string, account nativeReleaseAccount) (func(), error) {
	nativeScratchMu.Lock()
	defer nativeScratchMu.Unlock()
	var err error
	if nativeScratchUses[root] == 0 {
		err = nativeReleaseScratch(root, account)
	} else {
		err = nativeReleaseScratchOnce(nativeReleaseScratchDir(root), account)
	}
	if err != nil {
		return nil, err
	}
	nativeScratchUses[root]++
	return func() {
		nativeScratchMu.Lock()
		defer nativeScratchMu.Unlock()
		if nativeScratchUses[root]--; nativeScratchUses[root] <= 0 {
			delete(nativeScratchUses, root)
		}
	}, nil
}

// nativeReleaseScratch empties the scratch directory before a step and
// leaves it owned by the release account, private to it.
func nativeReleaseScratch(root string, account nativeReleaseAccount) error {
	dir := nativeReleaseScratchDir(root)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// The server cleanup leaves a busy application alone, but it may have
	// planned this directory just before the release began: a directory
	// that disappears underneath is simply made again.
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = nativeReleaseScratchOnce(dir, account); err == nil || !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return err
}

func nativeReleaseScratchOnce(dir string, account nativeReleaseAccount) error {
	for _, path := range []string{filepath.Dir(dir), dir} {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s must be a directory", path)
		}
		if err := os.Lchown(path, account.UID, account.GID); err != nil {
			return err
		}
	}
	return os.Chmod(dir, 0o700)
}
