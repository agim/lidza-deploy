package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// Managed Ruby. Applications with managed toolchains get the CRuby release
// their repository pins (.ruby-version, .tool-versions or the Gemfile),
// installed once per host by release.Toolchains.Ruby under the managed
// toolchain root and linked into each release like Go and Node.js. Gems
// stay per application: Bundler installs into <root>/shared/bundle
// (.bundle/config), and anything installed with `gem install` goes to
// GEM_HOME=<root>/shared/gems. Source builds run as root in a transient
// unit under the host's build limits, and on servers whose packages the
// agent manages the build dependencies are installed first.

// nativeManagedGemHome is GEM_HOME for an application with managed toolchains.
func nativeManagedGemHome(root string) string { return filepath.Join(root, "shared", "gems") }

// nativeManagedGemEnvironment keeps gems inside the application. GEM_PATH
// ends with a separator so RubyGems still appends its own default
// directories (the gems that ship with Ruby).
func nativeManagedGemEnvironment(root string) map[string]string {
	home := nativeManagedGemHome(root)
	return map[string]string{"GEM_HOME": home, "GEM_PATH": home + ":"}
}

// nativeToolchainInstaller is the managed toolchain installer for one
// deploy: its lines go to the deploy log and source builds run under the
// host's build limits.
func (m *Manager) nativeToolchainInstaller(app string, log io.Writer) release.Toolchains {
	installer := nativeToolchains()
	installer.Log = log
	if installer.Run == nil {
		installer.Run = m.nativeToolchainRunner(app, log)
	}
	if installer.BuildJobs == 0 {
		installer.BuildJobs = max(1, m.build.cpuQuota()/100)
	}
	if installer.InstallPackages == nil && m.nativeManagesPackages() {
		installer.InstallPackages = nativeInstallPackages(log)
	}
	return installer
}

// nativeManagesPackages reports whether the agent may install distribution
// packages here: the root agent of a native host it manages, on Debian or
// Ubuntu with apt.
func (m *Manager) nativeManagesPackages() bool {
	if m.cfg.Native == nil || m.hostOperationsAllowed() != nil || os.Geteuid() != 0 || !hostToolAvailable("apt-get") {
		return false
	}
	ok, _ := aptHost()
	return ok
}

// nativeInstallPackages installs the packages that are missing, if any.
func nativeInstallPackages(log io.Writer) func(ctx context.Context, packages []string) error {
	return func(ctx context.Context, packages []string) error {
		var missing []string
		for _, name := range packages {
			out, err := hostCommand(ctx, "dpkg-query", "-W", "-f=${Status}", name)
			if err != nil || !strings.Contains(out, "install ok installed") {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		buildLogf(log, "Installing build packages: %s", strings.Join(missing, " "))
		if out, err := hostCommand(ctx, "apt-get", "update", "-q", "-o", "DPkg::Lock::Timeout=300"); err != nil {
			return fmt.Errorf("apt-get update: %s", lastLines(out))
		}
		args := append([]string{"install", "-y", "-q", "--no-install-recommends", "-o", "DPkg::Lock::Timeout=300",
			"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}, missing...)
		if out, err := hostCommand(ctx, "apt-get", args...); err != nil {
			return fmt.Errorf("apt-get install %s: %s", strings.Join(missing, " "), lastLines(out))
		}
		return nil
	}
}

// nativeToolchainRunner runs toolchain build commands under the host's
// build resources: admission against free memory, a memory and CPU cap,
// peak lines in the deploy log and one retry after running out of memory.
func (m *Manager) nativeToolchainRunner(app string, log io.Writer) func(ctx context.Context, c release.ToolchainCommand) error {
	return func(ctx context.Context, c release.ToolchainCommand) error {
		step := buildStep{App: "toolchains", Step: c.Name, Build: true, Log: log}
		if app != "" {
			step.App = app
		}
		return m.runBuildStep(ctx, step, func(ctx context.Context, limits buildLimits) (int, error) {
			return runToolchainUnit(ctx, c, limits)
		})
	}
}

// runToolchainUnit runs one build command as root in a transient unit with
// the build limits, and reports its memory peak.
func runToolchainUnit(ctx context.Context, c release.ToolchainCommand, limits buildLimits) (int, error) {
	if os.Geteuid() != 0 {
		return 0, errors.New("toolchain builds require the root agent")
	}
	if len(c.Args) == 0 || !filepath.IsAbs(c.Dir) {
		return 0, fmt.Errorf("toolchain command %s is invalid", c.Name)
	}
	unit := "lidza-toolchain-build-" + newID()[:12] + ".service"
	argv := []string{"--quiet", "--wait", "--pipe", "--collect", "--expand-environment=no", "--unit=" + unit,
		"--property=WorkingDirectory=" + c.Dir, "--property=RuntimeMaxSec=7200", "--property=KillMode=control-group"}
	argv = append(argv, buildResourceProperties(limits)...)
	for _, entry := range c.Env {
		argv = append(argv, "--setenv="+entry)
	}
	argv = append(argv, "--", "/bin/sh", "-c", `umask 022; exec "$@"`, "build")
	argv = append(argv, c.Args...)
	stop := watchUnitMemory(unit)
	out, err := command(ctx, "", nil, "systemd-run", argv...)
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
		return peak, fmt.Errorf("%w: %s", err, lastLines(out))
	}
	return peak, nil
}
