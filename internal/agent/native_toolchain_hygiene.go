package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// Toolchain cleanup. Go, Node.js, Ruby and Rust versions accumulate in the
// managed toolchain root and in the version managers of application
// accounts (asdf, rbenv, rustup) as applications move to newer releases. A
// version is removed only when all of these hold:
//
//   - no kept release of any application on the host names it: neither its
//     toolchain links (<release>/.lidza-toolchain) nor its pin files
//     (go.mod, .node-version, .nvmrc, package.json engines, .ruby-version,
//     .tool-versions, Gemfile, rust-toolchain[.toml]). Kept releases include
//     the current and previous release and any release a process runs from;
//   - no running process executes from it (/proc/<pid>/exe);
//   - nothing in it changed for toolchainMinimumAge;
//   - for an account's version manager, an application of that account uses
//     the language at all (otherwise the account keeps all its versions);
//   - it is not the account's global default (~/.tool-versions,
//     ~/.rbenv/version, rustup's default), unless no application relies on
//     that default and another installed version of the language is in use.
//
// Every registered native application must be deployed by the release
// engine, or the deployment account's versions are kept: an application
// built another way may use any of them. Nothing runs while a deploy or
// build is in progress, and removals take the managed install lock so a
// concurrent install never races them. System packages are never touched.

const toolchainMinimumAge = 7 * 24 * time.Hour

var (
	goLinePattern        = regexp.MustCompile(`(?m)^go\s+([0-9]+\.[0-9]+(\.[0-9]+)?)\s*$`)
	goToolchainPattern   = regexp.MustCompile(`(?m)^toolchain\s+go([0-9]+\.[0-9]+\.[0-9]+)\s*$`)
	rustVersionPattern   = regexp.MustCompile(`[0-9]+\.[0-9]+(\.[0-9]+)?`)
	enginesNodePattern   = regexp.MustCompile(`([0-9]+(\.[0-9]+){0,2})`)
	numericVersionString = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,2}$`)
)

// toolchainInstall is one installed toolchain version.
type toolchainInstall struct {
	lang    string // Go, Node.js, Ruby, Rust
	version string
	path    string
	manager string // managed, asdf, rbenv, rustup
	account string // "" for the managed root
	home    string
}

func (i toolchainInstall) label() string {
	where := i.manager
	if i.account != "" {
		where += ", " + i.account
	}
	return fmt.Sprintf("%s %s (%s)", i.lang, i.version, where)
}

// toolchainPinSet is what the applications on the host use.
type toolchainPinSet struct {
	versions map[string]map[string]bool // lang → exact version or prefix
	paths    []string                   // linked installations
	uses     map[string]map[string]bool // home → lang → an application uses it
	defaults map[string]map[string]bool // home → lang → an application uses the account default
}

func newToolchainPinSet() *toolchainPinSet {
	return &toolchainPinSet{versions: map[string]map[string]bool{}, uses: map[string]map[string]bool{}, defaults: map[string]map[string]bool{}}
}

func (s *toolchainPinSet) pin(lang, version string) bool {
	version = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(version), "v"), "go")
	version = strings.TrimPrefix(version, "ruby-")
	if !numericVersionString.MatchString(version) {
		return false
	}
	if s.versions[lang] == nil {
		s.versions[lang] = map[string]bool{}
	}
	s.versions[lang][version] = true
	return true
}

func mark(table map[string]map[string]bool, home, lang string) {
	if table[home] == nil {
		table[home] = map[string]bool{}
	}
	table[home][lang] = true
}

// names reports whether a pin names version (exact, or a major[.minor] prefix).
func (s *toolchainPinSet) names(lang, version string) bool {
	for pin := range s.versions[lang] {
		if version == pin || strings.HasPrefix(version, pin+".") {
			return true
		}
	}
	return false
}

func (s *toolchainPinSet) linked(path string) bool {
	return slices.ContainsFunc(s.paths, func(target string) bool { return nativeReleaseInside(path, target) })
}

// addRelease records what one release directory uses.
func (s *toolchainPinSet) addRelease(dir, home string) {
	tools := filepath.Join(dir, release.ToolchainDir)
	for _, name := range []string{"go", "node", "ruby", "rust"} {
		if target, err := os.Readlink(filepath.Join(tools, name)); err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(tools, target)
			}
			s.paths = append(s.paths, filepath.Clean(target))
		}
	}
	used := map[string]bool{}
	pinned := map[string]bool{}
	pin := func(lang, version string) {
		if s.pin(lang, version) {
			pinned[lang] = true
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
		used["Go"] = true
		if match := goToolchainPattern.FindSubmatch(raw); match != nil {
			pin("Go", string(match[1]))
		}
		if match := goLinePattern.FindSubmatch(raw); match != nil {
			pin("Go", string(match[1]))
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		used["Node.js"] = true
		pin("Node.js", release.NodeRequirement(dir))
		var manifest struct {
			Engines map[string]string `json:"engines"`
		}
		if json.Unmarshal(raw, &manifest) == nil {
			if match := enginesNodePattern.FindString(manifest.Engines["node"]); match != "" {
				pin("Node.js", match)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Gemfile")); err == nil {
		used["Ruby"] = true
		if version, _ := release.RubyRequirement(dir); version != "" {
			pin("Ruby", version)
		}
	}
	if release.NeedsRust(dir) {
		used["Rust"] = true
	}
	for _, name := range []string{"rust-toolchain", "rust-toolchain.toml"} {
		if raw, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			used["Rust"] = true
			for _, version := range rustVersionPattern.FindAllString(string(raw), -1) {
				pin("Rust", version)
			}
		}
	}
	for tool, version := range release.ToolVersions(dir) {
		if lang := asdfLanguage(tool); lang != "" {
			used[lang] = true
			pin(lang, version)
		}
	}
	for lang := range used {
		mark(s.uses, home, lang)
		if !pinned[lang] {
			// An unpinned release runs whatever the account selects by default.
			mark(s.defaults, home, lang)
		}
	}
}

// asdfLanguage maps an asdf plugin name to its language.
func asdfLanguage(plugin string) string {
	switch plugin {
	case "ruby":
		return "Ruby"
	case "nodejs":
		return "Node.js"
	case "golang", "go":
		return "Go"
	case "rust":
		return "Rust"
	}
	return ""
}

// managedToolchainInstalls lists the versions in the managed toolchain root.
func managedToolchainInstalls(dir string) []toolchainInstall {
	var installs []toolchainInstall
	for kind, lang := range map[string]string{"go": "Go", "node": "Node.js", "ruby": "Ruby"} {
		entries, _ := os.ReadDir(filepath.Join(dir, kind))
		for _, entry := range entries {
			if entry.IsDir() && numericVersionString.MatchString(entry.Name()) {
				installs = append(installs, toolchainInstall{lang: lang, version: entry.Name(), path: filepath.Join(dir, kind, entry.Name()), manager: "managed"})
			}
		}
	}
	installs = append(installs, rustupInstalls(filepath.Join(dir, "rust", "rustup"), "", "")...)
	return installs
}

// rustupInstalls lists the versioned toolchains of a rustup home; channel
// toolchains (stable, beta, nightly) follow their channel and stay.
func rustupInstalls(rustupHome, account, home string) []toolchainInstall {
	var installs []toolchainInstall
	entries, _ := os.ReadDir(filepath.Join(rustupHome, "toolchains"))
	for _, entry := range entries {
		version, _, _ := strings.Cut(entry.Name(), "-") // 1.78.0-x86_64-unknown-linux-gnu
		if entry.IsDir() && numericVersionString.MatchString(version) {
			installs = append(installs, toolchainInstall{lang: "Rust", version: version, path: filepath.Join(rustupHome, "toolchains", entry.Name()), manager: "rustup", account: account, home: home})
		}
	}
	return installs
}

// accountToolchainInstalls lists the versions in an account's version managers.
func accountToolchainInstalls(home, account string) []toolchainInstall {
	var installs []toolchainInstall
	for _, plugin := range []string{"ruby", "nodejs", "golang", "rust"} {
		entries, _ := os.ReadDir(filepath.Join(home, ".asdf", "installs", plugin))
		for _, entry := range entries {
			if entry.IsDir() && numericVersionString.MatchString(entry.Name()) {
				installs = append(installs, toolchainInstall{lang: asdfLanguage(plugin), version: entry.Name(), path: filepath.Join(home, ".asdf", "installs", plugin, entry.Name()), manager: "asdf", account: account, home: home})
			}
		}
	}
	entries, _ := os.ReadDir(filepath.Join(home, ".rbenv", "versions"))
	for _, entry := range entries {
		if entry.IsDir() && numericVersionString.MatchString(entry.Name()) {
			installs = append(installs, toolchainInstall{lang: "Ruby", version: entry.Name(), path: filepath.Join(home, ".rbenv", "versions", entry.Name()), manager: "rbenv", account: account, home: home})
		}
	}
	return append(installs, rustupInstalls(filepath.Join(home, ".rustup"), account, home)...)
}

// accountDefault reports whether install is its account's global default.
func accountDefault(install toolchainInstall) bool {
	switch install.manager {
	case "asdf":
		for tool, version := range release.ToolVersions(install.home) {
			if asdfLanguage(tool) == install.lang && version == install.version {
				return true
			}
		}
	case "rbenv":
		raw, _ := os.ReadFile(filepath.Join(install.home, ".rbenv", "version"))
		return strings.TrimSpace(string(raw)) == install.version
	case "rustup":
		raw, _ := os.ReadFile(filepath.Join(install.home, ".rustup", "settings.toml"))
		return regexp.MustCompile(`(?m)^default_toolchain\s*=\s*"` + regexp.QuoteMeta(filepath.Base(install.path)) + `"`).Match(raw)
	}
	return false
}

// runningExecutables are the executables of running processes.
func runningExecutables() []string {
	var targets []string
	procs, _ := os.ReadDir(hostPath("/proc"))
	for _, proc := range procs {
		if proc.Name()[0] < '0' || proc.Name()[0] > '9' {
			continue
		}
		if target, err := os.Readlink(filepath.Join(hostPath("/proc"), proc.Name(), "exe")); err == nil {
			targets = append(targets, strings.TrimSuffix(target, " (deleted)"))
		}
	}
	return targets
}

func runFrom(executables []string, path string) bool {
	return slices.ContainsFunc(executables, func(exe string) bool { return nativeReleaseInside(path, exe) })
}

// planToolchainHygiene lists unused toolchain versions; notes explain
// versions kept on purpose and skipped steps.
func (m *Manager) planToolchainHygiene(apps []nativeHygieneApp, now time.Time) ([]CleanupItem, []string) {
	for _, app := range apps {
		if app.busy {
			return nil, []string{"Toolchains: skipped while " + app.id + " is deploying or changing"}
		}
	}
	if app, _, held := m.build.gate.current(); held {
		return nil, []string{"Toolchains: skipped while " + app + " is building"}
	}
	tools := nativeToolchains()
	if _, err := os.Stat(tools.Dir); err == nil {
		unlock, held, err := tools.TryLock()
		if held {
			return nil, []string{"Toolchains: skipped while a toolchain is being installed"}
		}
		if err == nil {
			unlock()
		}
	}
	pins := newToolchainPinSet()
	homes := map[string]string{} // home → account
	for _, app := range apps {
		for _, dir := range app.release {
			pins.addRelease(dir, app.home)
		}
		if app.home != "" {
			homes[app.home] = app.account
		}
	}
	// Applications the release engine does not deploy use the deployment
	// account's versions in ways nothing here can see.
	unknown := m.nativeAppsOutsideReleases()
	var notes []string
	installs := managedToolchainInstalls(tools.Dir)
	for _, home := range slices.Sorted(func(yield func(string) bool) {
		for home := range homes {
			if !yield(home) {
				return
			}
		}
	}) {
		account := homes[home]
		if len(unknown) > 0 && m.cfg.Native != nil && account == m.cfg.Native.User {
			notes = append(notes, fmt.Sprintf("Toolchains of account %s: kept, %s is not deployed by the release engine", account, strings.Join(unknown, ", ")))
			continue
		}
		installs = append(installs, accountToolchainInstalls(home, account)...)
	}
	executables := runningExecutables()
	cutoff := now.Add(-toolchainMinimumAge)
	type key struct{ lang, manager, home string }
	inUse := map[key][]string{}
	keep := map[string]bool{}
	for _, install := range installs {
		if pins.linked(install.path) || pins.names(install.lang, install.version) || runFrom(executables, install.path) {
			keep[install.path] = true
			k := key{install.lang, install.manager, install.home}
			inUse[k] = append(inUse[k], install.version)
		}
	}
	var items []CleanupItem
	for _, install := range installs {
		if keep[install.path] {
			continue
		}
		if install.manager != "managed" && !pins.uses[install.home][install.lang] {
			// No application of this account uses the language.
			continue
		}
		size, newest := treeFacts(install.path)
		if newest.After(cutoff) {
			continue
		}
		reason := fmt.Sprintf("no kept release of any application names it and no process runs it; unchanged for %d days", int(now.Sub(newest).Hours()/24))
		if install.manager != "managed" && accountDefault(install) {
			others := inUse[key{install.lang, install.manager, install.home}]
			switch {
			case pins.defaults[install.home][install.lang]:
				notes = append(notes, fmt.Sprintf("Kept %s: the account's global default, used by releases that pin no %s version", install.label(), install.lang))
				continue
			case len(others) == 0:
				notes = append(notes, fmt.Sprintf("Kept %s: the account's global default, and no other installed %s is in use", install.label(), install.lang))
				continue
			}
			slices.SortFunc(others, func(a, b string) int {
				return slices.Compare(nativeToolchainVersionKey(a), nativeToolchainVersionKey(b))
			})
			reason = fmt.Sprintf("the account's global default, but no application uses it (applications use %s %s); %s", install.lang, strings.Join(others, ", "), reason)
		}
		items = append(items, CleanupItem{Kind: "toolchain", Name: install.label(), Path: install.path, Bytes: size, Reason: reason,
			apply: removeToolchain(install, tools, cutoff)})
	}
	slices.SortFunc(items, func(a, b CleanupItem) int { return strings.Compare(a.Path, b.Path) })
	return items, notes
}

// nativeAppsOutsideReleases are registered native applications that the
// release engine does not deploy.
func (m *Manager) nativeAppsOutsideReleases() []string {
	m.mu.Lock()
	ids := make([]string, 0, len(m.data.NativeApps))
	for id := range m.data.NativeApps {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	slices.Sort(ids)
	var outside []string
	for _, id := range ids {
		if _, _, err := m.nativeReleasePolicy(id); err != nil {
			outside = append(outside, id)
		}
	}
	return outside
}

// removeToolchain removes one version under the managed install lock,
// unless it changed or started running since the plan. A prebuilt Ruby's
// prefix link goes with it.
func removeToolchain(install toolchainInstall, tools release.Toolchains, cutoff time.Time) func() error {
	return func() error {
		if _, err := os.Stat(tools.Dir); err == nil {
			unlock, held, err := tools.TryLock()
			if err != nil {
				return err
			}
			if held {
				return fmt.Errorf("a toolchain install is running; %s is kept for the next cleanup", install.label())
			}
			defer unlock()
		}
		if _, newest := treeFacts(install.path); newest.After(cutoff) {
			return fmt.Errorf("%s changed since the cleanup was planned; kept", install.label())
		}
		if runFrom(runningExecutables(), install.path) {
			return fmt.Errorf("%s started running since the cleanup was planned; kept", install.label())
		}
		if install.manager == "managed" && install.lang == "Ruby" {
			if link := tools.RubyPrefixLink(install.version); link != "" {
				if target, err := os.Readlink(link); err == nil && filepath.Clean(target) == install.path {
					if err := os.Remove(link); err != nil {
						return err
					}
					_ = os.Remove(filepath.Dir(link)) // the version directory, once empty
				}
			}
		}
		return removeTree(install.path)()
	}
}

// toolchainTotals is the summary line of removed toolchain versions.
func toolchainTotals(count int, bytes int64) string {
	return "Toolchains: removed " + strconv.Itoa(count) + " unused version(s), " + bytesText(bytes) + " freed"
}
