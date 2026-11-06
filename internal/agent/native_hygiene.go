package agent

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// Disk hygiene for native applications, the counterpart of the Docker steps
// (docker_hygiene.go), so servers without Docker are kept as tidy:
//
//   - releases beyond the application's keep count; the current and the
//     previous release, and any release a running process still uses (its
//     working directory or executable), always stay;
//   - shared/tmp/cache files older than the cleanup age once the cache
//     grows past nativeCacheBloat (bootsnap, Vite and similar caches rebuild
//     themselves);
//   - the release engine's scratch directory (.cache/tmp) between releases;
//   - package caches over their size cap: Go build and module caches, npm
//     and Cargo caches, in the application root and the account's home;
//   - Go, Node.js, Ruby and Rust versions, in the managed toolchain root
//     and in account version managers, that nothing uses
//     (native_toolchain_hygiene.go);
//   - rotated or compressed logs in shared/log older than the cleanup age;
//   - core dumps older than the cleanup age.
//
// Applications with a release or other native operation running are left
// for the next cleanup.

const (
	nativeCacheBloat = 256 << 20
	goBuildCacheCap  = 2 << 30
	goModCacheCap    = 4 << 30
	npmCacheCap      = 1 << 30
	cargoCacheCap    = 2 << 30
	nativeKeepFloor  = 5 // release.Plan's default keep
	coreDumpDir      = "/var/lib/systemd/coredump"
)

var (
	rotatedLog = regexp.MustCompile(`(\.log\.[0-9]+|\.(gz|xz|bz2|zst|[0-9]+))$`)
	coreFile   = regexp.MustCompile(`^core(\.[0-9]+)?$`)
)

// nativeHygieneApp is one native application the cleanup may tidy.
type nativeHygieneApp struct {
	id      string
	layout  release.Layout
	keep    int
	home    string
	account string
	busy    bool     // a release or other operation is running; left alone
	release []string // kept release directories
}

func (m *Manager) nativeHygieneApps() []nativeHygieneApp {
	m.mu.Lock()
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range m.data.NativeApps {
			if !yield(id) {
				return
			}
		}
	})
	busy := map[string]bool{}
	for _, id := range ids {
		_, transition := m.data.NativeTransitions[id]
		busy[id] = transition || m.nativeOperations[id] || m.busy(id)
	}
	m.mu.Unlock()
	var apps []nativeHygieneApp
	for _, id := range ids {
		_, policy, err := m.nativeReleasePolicy(id)
		if err != nil {
			continue
		}
		app := nativeHygieneApp{id: id, layout: release.Layout{Root: policy.Root}, keep: cmp.Or(policy.Keep, nativeKeepFloor), busy: busy[id]}
		if account, err := m.nativeReleaseAccount(policy); err == nil {
			app.home, app.account = account.Home, account.Name
		}
		apps = append(apps, app)
	}
	return apps
}

// runningReleases are the releases a running process works in or runs
// from, read from /proc.
func runningReleases(releases string) []string {
	var ids []string
	procs, _ := os.ReadDir(hostPath("/proc"))
	prefix := filepath.Clean(releases) + string(filepath.Separator)
	for _, proc := range procs {
		if proc.Name()[0] < '0' || proc.Name()[0] > '9' {
			continue
		}
		for _, link := range []string{"cwd", "exe"} {
			target, err := os.Readlink(filepath.Join(hostPath("/proc"), proc.Name(), link))
			if rest, ok := strings.CutPrefix(target, prefix); err == nil && ok {
				id, _, _ := strings.Cut(rest, string(filepath.Separator))
				if !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

// oldFiles are the regular files below dir last changed before cutoff that
// match keep (nil matches every file), with their total size.
func oldFiles(dir string, cutoff time.Time, match func(name string) bool) ([]string, int64) {
	var files []string
	var size int64
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() || (match != nil && !match(entry.Name())) {
			return nil
		}
		if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
			files = append(files, path)
			size += info.Size()
		}
		return nil
	})
	return files, size
}

func removeFiles(files []string, cutoff time.Time) func() error {
	return func() error {
		for _, path := range files {
			// Changed since the plan: it is in use again.
			if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
}

// removeTree deletes a directory, first making read-only module cache
// entries writable, as `go clean -modcache` does.
// errHygieneBusy marks an item left alone because its application became
// busy between planning and removal; the next cleanup reconsiders it.
var errHygieneBusy = errors.New("the application is busy; left for the next cleanup")

// nativeAppBusy reports a release, transition or operation running for id
// right now. Cleanup items check it again at removal time: a release that
// starts after the plan was made must not lose files under its feet.
func (m *Manager) nativeAppBusy(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, transition := m.data.NativeTransitions[id]
	return transition || m.nativeOperations[id] || m.busy(id)
}

// unlessBusy runs apply only while the application is not busy.
func (m *Manager) unlessBusy(id string, apply func() error) func() error {
	return func() error {
		if m.nativeAppBusy(id) {
			return errHygieneBusy
		}
		return apply()
	}
}

// emptyTree removes everything inside path but keeps the directory itself:
// a release that is preparing the same directory keeps a valid path.
func emptyTree(path string) func() error {
	return func() error {
		entries, err := os.ReadDir(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		for _, entry := range entries {
			if err := removeTree(filepath.Join(path, entry.Name()))(); err != nil {
				return err
			}
		}
		return nil
	}
}

func removeTree(path string) func() error {
	return func() error {
		_ = filepath.WalkDir(path, func(p string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
		return os.RemoveAll(path)
	}
}

// planNativeHygiene lists what the cleanup removes for native applications.
func (m *Manager) planNativeHygiene(now time.Time, olderThan time.Duration) ([]CleanupItem, []string) {
	if m.cfg.IngressOnly || m.cfg.Native == nil || !hygieneHostRoot() {
		return nil, nil
	}
	cutoff := now.Add(-olderThan)
	var items []CleanupItem
	add := func(item CleanupItem) {
		if item.Bytes > 0 || item.apply != nil && item.Kind == "native-release" {
			items = append(items, item)
		}
	}
	capped := func(owner, path string, limit int64, why string) {
		if size, _ := treeFacts(path); size > limit {
			add(CleanupItem{Kind: "native-cache", Name: path, Bytes: size, Reason: owner + ": " + why + " over " + bytesText(limit), apply: removeTree(path)})
		}
	}
	homes := map[string][]nativeHygieneApp{}
	var all []nativeHygieneApp
	for _, app := range m.nativeHygieneApps() {
		if app.busy {
			all = append(all, app)
			continue
		}
		l := app.layout
		current, err := l.CurrentID()
		if err != nil || current == "" {
			continue
		}
		protect := runningReleases(l.Releases())
		if previous, err := l.Previous(); err == nil {
			protect = append(protect, previous)
		}
		candidates, err := l.PruneCandidates(max(app.keep, 2), protect...)
		if err == nil {
			for _, id := range candidates {
				path := l.Release(id)
				size, _ := treeFacts(path)
				add(CleanupItem{Kind: "native-release", Name: app.id + " release " + id, Bytes: size, Reason: fmt.Sprintf("beyond the %d kept releases", app.keep), apply: m.unlessBusy(app.id, removeTree(path))})
			}
		}
		kept, _ := l.List()
		for _, id := range kept {
			if !slices.Contains(candidates, id) {
				app.release = append(app.release, l.Release(id))
			}
		}
		// shared/tmp/cache, once it is bloated.
		cache := filepath.Join(l.Shared(), "tmp", "cache")
		if size, _ := treeFacts(cache); size > nativeCacheBloat {
			files, bytes := oldFiles(cache, cutoff, nil)
			add(CleanupItem{Kind: "native-cache", Name: cache, Bytes: bytes, Reason: fmt.Sprintf("%s: %d cache files unused for %s", app.id, len(files), olderThan.Round(time.Hour)), apply: m.unlessBusy(app.id, removeFiles(files, cutoff))})
		}
		// The release engine's scratch space between releases.
		for _, scratch := range []string{nativeReleaseScratchDir(l.Root), bundlerCheckDir(l.Root)} {
			if size, _ := treeFacts(scratch); size > 0 {
				add(CleanupItem{Kind: "native-scratch", Name: scratch, Bytes: size, Reason: app.id + ": release scratch space", apply: m.unlessBusy(app.id, emptyTree(scratch))})
			}
		}
		managed := filepath.Join(l.Root, ".cache")
		goBuild := filepath.Join(managed, "go-build")
		if size, _ := treeFacts(goBuild); size > goBuildCacheCap {
			files, bytes := oldFiles(goBuild, cutoff, nil)
			add(CleanupItem{Kind: "native-cache", Name: goBuild, Bytes: bytes, Reason: app.id + ": Go build cache over " + bytesText(goBuildCacheCap), apply: removeFiles(files, cutoff)})
		}
		capped(app.id, filepath.Join(managed, "go", "pkg", "mod"), goModCacheCap, "Go module cache")
		capped(app.id, filepath.Join(managed, "npm"), npmCacheCap, "npm cache")
		capped(app.id, filepath.Join(managed, "cargo", "registry"), cargoCacheCap, "Cargo registry cache")
		// Rotated logs.
		logs, bytes := oldFiles(filepath.Join(l.Shared(), "log"), cutoff, rotatedLog.MatchString)
		add(CleanupItem{Kind: "app-log", Name: filepath.Join(l.Shared(), "log"), Bytes: bytes, Reason: fmt.Sprintf("%s: %d rotated logs older than %s", app.id, len(logs), olderThan.Round(time.Hour)), apply: removeFiles(logs, cutoff)})
		// Core dumps in the application's own trees.
		var cores []string
		var coreBytes int64
		for _, dir := range append([]string{l.Shared()}, app.release...) {
			files, size := oldFiles(dir, cutoff, coreFile.MatchString)
			cores, coreBytes = append(cores, files...), coreBytes+size
		}
		add(CleanupItem{Kind: "core-dump", Name: app.id, Bytes: coreBytes, Reason: fmt.Sprintf("%d core dumps older than %s", len(cores), olderThan.Round(time.Hour)), apply: removeFiles(cores, cutoff)})
		all = append(all, app)
		if app.home != "" {
			homes[app.home] = append(homes[app.home], app)
		}
	}
	for _, home := range slices.Sorted(func(yield func(string) bool) {
		for home := range homes {
			if !yield(home) {
				return
			}
		}
	}) {
		owner := "account " + filepath.Base(home)
		goBuild := filepath.Join(home, ".cache", "go-build")
		if size, _ := treeFacts(goBuild); size > goBuildCacheCap {
			files, bytes := oldFiles(goBuild, cutoff, nil)
			add(CleanupItem{Kind: "native-cache", Name: goBuild, Bytes: bytes, Reason: owner + ": Go build cache over " + bytesText(goBuildCacheCap), apply: removeFiles(files, cutoff)})
		}
		capped(owner, filepath.Join(home, "go", "pkg", "mod"), goModCacheCap, "Go module cache")
		capped(owner, filepath.Join(home, ".npm", "_cacache"), npmCacheCap, "npm cache")
		capped(owner, filepath.Join(home, ".cargo", "registry"), cargoCacheCap, "Cargo registry cache")
	}
	toolchains, notes := m.planToolchainHygiene(all, now)
	items = append(items, toolchains...)
	files, size := oldFiles(hostPath(coreDumpDir), cutoff, nil)
	add(CleanupItem{Kind: "core-dump", Name: coreDumpDir, Bytes: size, Reason: fmt.Sprintf("%d systemd core dumps older than %s", len(files), olderThan.Round(time.Hour)), apply: removeFiles(files, cutoff)})
	return items, notes
}
