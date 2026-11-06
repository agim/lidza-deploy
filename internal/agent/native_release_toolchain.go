package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// Repositories often pin a runtime by major (or major.minor) version, for
// example ".node-version: 20". Version managers that select a runtime from
// these files need an exact installed version, so each release resolves a
// partial pin to the highest matching installed version and records it in
// the release tree. The build and the processes started from the release
// then use the same runtime.

var (
	nativeToolchainPartial = regexp.MustCompile(`^v?([0-9]+)(\.[0-9]+)?$`)
	nativeToolchainExact   = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)
)

type nativeToolchainPin struct {
	file   string // pin file in the release root
	plugin string // installs/<plugin> under the version manager directory
}

var nativeToolchainPins = []nativeToolchainPin{{".node-version", "nodejs"}, {".nvmrc", "nodejs"}, {".ruby-version", "ruby"}}

func nativeToolchainVersionKey(version string) []int {
	var key []int
	for _, part := range strings.Split(version, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		key = append(key, n)
	}
	return key
}

// resolveNativeToolchain returns the highest installed version under
// installs/<plugin> that matches the pin, or the pin unchanged when it is
// already exact.
func resolveNativeToolchain(installs, pin string) (string, bool, error) {
	pin = strings.TrimSpace(pin)
	if pin == "" || nativeToolchainExact.MatchString(pin) || !nativeToolchainPartial.MatchString(pin) {
		return pin, false, nil
	}
	prefix := strings.TrimPrefix(pin, "v")
	entries, err := os.ReadDir(installs)
	if err != nil {
		return "", false, fmt.Errorf("no installed runtimes found for pin %s", pin)
	}
	var matches, installed []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || nativeToolchainVersionKey(name) == nil {
			continue
		}
		installed = append(installed, name)
		if name == prefix || strings.HasPrefix(name, prefix+".") {
			matches = append(matches, name)
		}
	}
	if len(matches) == 0 {
		slices.Sort(installed)
		return "", false, fmt.Errorf("version %s is not installed on this host (installed: %s)", pin, strings.Join(installed, ", "))
	}
	slices.SortFunc(matches, func(a, b string) int {
		return slices.Compare(nativeToolchainVersionKey(a), nativeToolchainVersionKey(b))
	})
	return matches[len(matches)-1], true, nil
}

// nativeReleasePinToolchains resolves partial runtime pins in a new release
// against the owner's installed versions.
func nativeReleasePinToolchains(home string) func(l release.Layout, id, commit string) error {
	return func(l release.Layout, id, _ string) error {
		dir := l.Release(id)
		for _, pin := range nativeToolchainPins {
			path := filepath.Join(dir, pin.file)
			info, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || !info.Mode().IsRegular() || info.Size() > 256 {
				continue
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			version, changed, err := resolveNativeToolchain(filepath.Join(home, ".asdf", "installs", pin.plugin), string(raw))
			if err != nil {
				return fmt.Errorf("%s: %w", pin.file, err)
			}
			if !changed {
				continue
			}
			if err := os.WriteFile(path, []byte(version+"\n"), info.Mode().Perm()); err != nil {
				return err
			}
		}
		return nil
	}
}

// Managed toolchains. Applications with their own account (and Go
// applications) do not use a version manager: the agent installs official,
// checksum-verified Go, Node.js and Ruby releases into a root-owned directory and
// links the versions one release needs into <release>/.lidza-toolchain.
// <root>/.lidza-build points at the release being built, so build steps
// find its toolchains on PATH; processes use <root>/current/.lidza-toolchain
// and therefore follow every rollback.

// nativeToolchainDir holds installed toolchains: go/<version>,
// node/<version>, ruby/<version> and rust/.
var nativeToolchainDir = "/usr/local/lib/lidza"

// nativeToolchains is the installer; tests replace its URLs.
var nativeToolchains = func() release.Toolchains {
	return release.Toolchains{Dir: nativeToolchainDir, HostGo: "/usr/local/go"}
}

const nativeSystemPath = "/usr/local/bin:/usr/bin:/bin"

func nativeToolchainPath(base string) string {
	return base + "/bin:" + base + "/go/bin:" + base + "/node/bin:" + base + "/ruby/bin:" + base + "/rust/bin:" + nativeSystemPath
}

// nativeManagedBuildPath is PATH for build, release and schedule commands.
func nativeManagedBuildPath(root string) string {
	return nativeToolchainPath(filepath.Join(root, ".lidza-build"))
}

// nativeManagedRuntimePath is PATH for the application's processes.
func nativeManagedRuntimePath(root string) string {
	return nativeToolchainPath(filepath.Join(root, "current", release.ToolchainDir))
}

// nativeManagedCacheEnvironment keeps module and package caches inside the
// application root, so builds are cached per application and isolated.
func nativeManagedCacheEnvironment(root string) map[string]string {
	cache := filepath.Join(root, ".cache")
	return map[string]string{
		"GOPATH":           filepath.Join(cache, "go"),
		"GOMODCACHE":       filepath.Join(cache, "go", "pkg", "mod"),
		"GOCACHE":          filepath.Join(cache, "go-build"),
		"npm_config_cache": filepath.Join(cache, "npm"),
		"CARGO_HOME":       filepath.Join(cache, "cargo"),
	}
}

// nativeHostHasCommand reports whether a system directory provides name.
func nativeHostHasCommand(name string) bool {
	for _, dir := range filepath.SplitList(nativeSystemPath) {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

// nativeReleaseManagedToolchains prepares a new release's toolchains: the
// Go release its go.mod asks for, Node.js when it has a package.json (the
// pinned version, else the host's node, else the newest LTS), and for a
// Gemfile the Ruby it pins (else the host's ruby and bundle, else a clear
// failure).
func (m *Manager) nativeReleaseManagedToolchains(app string, account nativeReleaseAccount, policy NativeReleasePolicy, log io.Writer) func(l release.Layout, id, commit string) error {
	return func(l release.Layout, id, _ string) error {
		// A Ruby built from source takes a while on small servers.
		ctx, cancel := context.WithTimeout(m.ctx, time.Hour)
		defer cancel()
		dir := l.Release(id)
		tools := filepath.Join(dir, release.ToolchainDir)
		if info, err := os.Lstat(tools); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) || err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s in the repository conflicts with the release toolchain directory", release.ToolchainDir)
		}
		if err := os.RemoveAll(tools); err != nil {
			return err
		}
		for _, path := range []string{tools, filepath.Join(tools, "bin")} {
			if err := os.Mkdir(path, 0o755); err != nil {
				return err
			}
			if err := os.Lchown(path, account.UID, account.GID); err != nil {
				return err
			}
		}
		installer := m.nativeToolchainInstaller(app, log)
		link := func(name, target string) error {
			path := filepath.Join(tools, name)
			if err := os.Symlink(target, path); err != nil {
				return err
			}
			return os.Lchown(path, account.UID, account.GID)
		}
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			mod, err := release.ReadGoModule(dir)
			if err != nil {
				return err
			}
			root, err := installer.Go(ctx, mod.GoVersion())
			if err != nil {
				return fmt.Errorf("Go %s: %w", mod.GoVersion(), err)
			}
			if err := link("go", root); err != nil {
				return err
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
			pin := release.NodeRequirement(dir)
			root := ""
			switch {
			case pin != "":
				if root, err = installer.Node(ctx, pin); err != nil {
					return fmt.Errorf("Node.js %s: %w", pin, err)
				}
			case nativeHostHasCommand("node"):
				// The host's own Node.js serves applications without a pin.
			default:
				if root = installer.Installed("node", ""); root == "" {
					if root, err = installer.Node(ctx, ""); err != nil {
						return fmt.Errorf("Node.js: %w", err)
					}
				}
			}
			if root != "" {
				if err := link("node", root); err != nil {
					return err
				}
			}
		}
		if release.NeedsRust(dir) {
			root, err := installer.Rust(ctx)
			if err != nil {
				return fmt.Errorf("Rust: %w", err)
			}
			if err := link("rust", root); err != nil {
				return err
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "Gemfile")); err == nil {
			want, source := release.RubyRequirement(dir)
			switch {
			case want != "":
				root, err := installer.Ruby(ctx, want)
				if err != nil {
					return fmt.Errorf("Ruby %s (%s): %w", want, source, err)
				}
				if err := link("ruby", root); err != nil {
					return err
				}
			case !nativeHostHasCommand("ruby") || !nativeHostHasCommand("bundle"):
				return errors.New("this Rails application pins no Ruby version: add a .ruby-version file (for example 3.4.4) so the agent installs that Ruby, or install ruby and bundle on the host, then deploy again")
			}
			// The application's own gem directory (GEM_HOME), shared by its releases.
			gems := nativeManagedGemHome(policy.Root)
			for _, dir := range []struct {
				path string
				mode os.FileMode
			}{{filepath.Dir(gems), 0o755}, {gems, 0o750}} {
				path := dir.path
				if err := os.Mkdir(path, dir.mode); err != nil {
					if errors.Is(err, os.ErrExist) {
						continue
					}
					return err
				}
				if err := os.Chmod(path, dir.mode); err != nil {
					return err
				}
				if err := os.Lchown(path, account.UID, account.GID); err != nil {
					return err
				}
			}
		}
		for _, path := range []string{filepath.Join(policy.Root, ".cache")} {
			if err := os.Mkdir(path, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			if err := os.Lchown(path, account.UID, account.GID); err != nil {
				return err
			}
		}
		// Point the build link at this release atomically.
		pointer := filepath.Join(policy.Root, ".lidza-build")
		next := pointer + ".next"
		_ = os.Remove(next)
		if err := os.Symlink(filepath.Join("releases", id, release.ToolchainDir), next); err != nil {
			return err
		}
		if err := os.Lchown(next, account.UID, account.GID); err != nil {
			return err
		}
		return os.Rename(next, pointer)
	}
}
