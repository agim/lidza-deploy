package agent

import (
	"bytes"
	"context"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// toolchainHost is a fake host: a managed toolchain root, an account home
// with asdf Rubies, a /proc and application releases, all in temp dirs.
type toolchainHost struct {
	m        *Manager
	root     string // fake filesystem root for /proc
	tools    release.Toolchains
	home     string
	apps     []nativeHygieneApp
	old, now time.Time
}

func newToolchainHost(t *testing.T) *toolchainHost {
	t.Helper()
	h := &toolchainHost{m: testManager(t, &fakeRuntime{}), root: t.TempDir(), home: t.TempDir(), now: time.Now()}
	h.old = h.now.Add(-40 * 24 * time.Hour)
	base := t.TempDir()
	h.tools = release.Toolchains{Dir: filepath.Join(base, "lidza"), RubyPrefix: filepath.Join(base, "hostedtoolcache", "Ruby"), Arch: "amd64"}
	savedTools, savedPath := nativeToolchains, hostPath
	nativeToolchains = func() release.Toolchains { return h.tools }
	hostPath = func(path string) string { return filepath.Join(h.root, path) }
	t.Cleanup(func() { nativeToolchains, hostPath = savedTools, savedPath })
	os.MkdirAll(filepath.Join(h.root, "proc"), 0o755)
	return h
}

// install creates an aged installation with a 1 MiB file.
func (h *toolchainHost) install(t *testing.T, path string, modified time.Time) {
	t.Helper()
	file := filepath.Join(path, "bin", "tool")
	writeAged(t, file, 1<<20, modified)
	os.Chtimes(filepath.Join(path, "bin"), modified, modified)
	os.Chtimes(path, modified, modified)
}

// app adds an application whose kept releases contain files.
func (h *toolchainHost) app(t *testing.T, id string, releases ...map[string]string) {
	t.Helper()
	app := nativeHygieneApp{id: id, home: h.home, account: "deploy"}
	for i, files := range releases {
		dir := filepath.Join(t.TempDir(), "releases", strconv.Itoa(i))
		for name, body := range files {
			path := filepath.Join(dir, name)
			os.MkdirAll(filepath.Dir(path), 0o755)
			if strings.HasPrefix(body, "->") {
				os.Symlink(strings.TrimPrefix(body, "->"), path)
				continue
			}
			os.WriteFile(path, []byte(body), 0o644)
		}
		app.release = append(app.release, dir)
	}
	h.apps = append(h.apps, app)
}

func (h *toolchainHost) running(t *testing.T, pid int, exe string) {
	t.Helper()
	dir := filepath.Join(h.root, "proc", strconv.Itoa(pid))
	os.MkdirAll(dir, 0o755)
	if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
}

func names(items []CleanupItem) []string {
	var out []string
	for _, item := range items {
		out = append(out, item.Name)
	}
	slices.Sort(out)
	return out
}

func asdfRuby(h *toolchainHost, version string) string {
	return filepath.Join(h.home, ".asdf", "installs", "ruby", version)
}

// The production layout: asdf Rubies of the deploy account, five pinned by
// current applications, the global default unused.
func productionLikeHost(t *testing.T) *toolchainHost {
	h := newToolchainHost(t)
	for _, v := range []string{"3.3.4", "3.3.5", "3.4.1", "3.4.4", "3.4.5", "3.4.10", "4.0.1"} {
		h.install(t, asdfRuby(h, v), h.old)
	}
	h.install(t, asdfRuby(h, "3.2.9"), h.now) // installed moments ago
	os.WriteFile(filepath.Join(h.home, ".tool-versions"), []byte("ruby 4.0.1\nnodejs 22.11.0\n"), 0o644)
	gem := "source 'https://rubygems.org'\n"
	// current and previous release of one app pin different Rubies
	h.app(t, "shop", map[string]string{"Gemfile": gem, ".ruby-version": "3.3.5\n"}, map[string]string{"Gemfile": gem, ".ruby-version": "ruby-3.4.1\n"})
	h.app(t, "blog", map[string]string{"Gemfile": gem, ".tool-versions": "ruby 3.4.4\n"})
	h.app(t, "wiki", map[string]string{"Gemfile": gem + "ruby \"3.4.5\"\n"})
	h.app(t, "crm", map[string]string{"Gemfile": gem, ".ruby-version": "3.4.10\n"})
	// managed Go: one linked, one run by a process, one unused
	for _, v := range []string{"1.24.3", "1.23.1", "1.21.0"} {
		h.install(t, filepath.Join(h.tools.Dir, "go", v), h.old)
	}
	h.app(t, "api", map[string]string{"go.mod": "module api\n\ngo 1.24\n", ".lidza-toolchain/go": "->" + filepath.Join(h.tools.Dir, "go", "1.24.3")})
	h.running(t, 4242, filepath.Join(h.tools.Dir, "go", "1.23.1", "bin", "tool"))
	return h
}

func TestToolchainCleanupKeepsPinnedRunningRecentAndRemovesTheRest(t *testing.T) {
	h := productionLikeHost(t)
	items, notes := h.m.planToolchainHygiene(h.apps, h.now)
	want := []string{"Go 1.21.0 (managed)", "Ruby 3.3.4 (asdf, deploy)", "Ruby 4.0.1 (asdf, deploy)"}
	if got := names(items); !slices.Equal(got, want) {
		t.Fatalf("candidates %v, want %v (notes %v)", got, want, notes)
	}
	for _, item := range items {
		if item.Bytes != 1<<20 || item.Path == "" || item.Kind != "toolchain" {
			t.Fatalf("item %+v", item)
		}
		if item.Name == "Ruby 4.0.1 (asdf, deploy)" && !strings.Contains(item.Reason, "global default") || !strings.Contains(item.Reason, "unchanged for 40 days") {
			t.Fatalf("reason %q", item.Reason)
		}
	}
	messages, failures := applyCleanupItems(context.Background(), nil, items)
	if len(failures) != 0 {
		t.Fatal(failures)
	}
	report := strings.Join(messages, "\n")
	for _, line := range []string{"Removed Ruby 3.3.4 (asdf, deploy): 1 MiB freed", "Removed Go 1.21.0 (managed): 1 MiB freed", "Toolchains: removed 3 unused version(s), 3 MiB freed"} {
		if !strings.Contains(report, line) {
			t.Fatalf("report lacks %q:\n%s", line, report)
		}
	}
	for _, gone := range []string{asdfRuby(h, "3.3.4"), asdfRuby(h, "4.0.1"), filepath.Join(h.tools.Dir, "go", "1.21.0")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatal("not removed", gone)
		}
	}
	for _, v := range []string{"3.3.5", "3.4.1", "3.4.4", "3.4.5", "3.4.10", "3.2.9"} {
		if _, err := os.Stat(asdfRuby(h, v)); err != nil {
			t.Fatal("removed a used or recent Ruby", v)
		}
	}
}

func TestToolchainCleanupGlobalDefaultRules(t *testing.T) {
	// An application without a Ruby pin runs the account default: kept.
	h := productionLikeHost(t)
	h.app(t, "legacy", map[string]string{"Gemfile": "gem 'rails'\n"})
	items, notes := h.m.planToolchainHygiene(h.apps, h.now)
	if slices.Contains(names(items), "Ruby 4.0.1 (asdf, deploy)") || !slices.ContainsFunc(notes, func(n string) bool {
		return strings.Contains(n, "Kept Ruby 4.0.1 (asdf, deploy): the account's global default, used by releases that pin no Ruby")
	}) {
		t.Fatalf("items %v notes %v", names(items), notes)
	}

	// The only installed Ruby nobody pins is the default, and no other is in use: kept.
	h = newToolchainHost(t)
	h.install(t, asdfRuby(h, "3.3.0"), h.old)
	os.WriteFile(filepath.Join(h.home, ".tool-versions"), []byte("ruby 3.3.0\n"), 0o644)
	h.app(t, "shop", map[string]string{"Gemfile": "", ".ruby-version": "3.4.4\n"})
	items, notes = h.m.planToolchainHygiene(h.apps, h.now)
	if len(items) != 0 || !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "no other installed Ruby is in use") }) {
		t.Fatalf("items %v notes %v", names(items), notes)
	}

	// An account whose applications use no Ruby keeps all its Rubies.
	h = newToolchainHost(t)
	h.install(t, asdfRuby(h, "3.1.0"), h.old)
	h.app(t, "site", map[string]string{"package.json": "{}", ".node-version": "22.11.0\n"})
	if items, _ := h.m.planToolchainHygiene(h.apps, h.now); len(items) != 0 {
		t.Fatalf("items %v", names(items))
	}
}

func TestToolchainCleanupWaitsForDeploysAndInstalls(t *testing.T) {
	h := productionLikeHost(t)
	h.apps[0].busy = true
	if items, notes := h.m.planToolchainHygiene(h.apps, h.now); len(items) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "skipped while shop") {
		t.Fatalf("items %v notes %v", names(items), notes)
	}
	h.apps[0].busy = false
	unlock, held, err := h.tools.TryLock()
	if err != nil || held {
		t.Fatal(held, err)
	}
	if items, notes := h.m.planToolchainHygiene(h.apps, h.now); len(items) != 0 || !strings.Contains(strings.Join(notes, ""), "being installed") {
		t.Fatalf("items %v notes %v", names(items), notes)
	}
	unlock()
	items, _ := h.m.planToolchainHygiene(h.apps, h.now)
	// An install starts between the plan and the removal.
	unlock, _, _ = h.tools.TryLock()
	_, failures := applyCleanupItems(context.Background(), nil, items)
	unlock()
	if len(failures) != len(items) || !strings.Contains(failures[0].Error(), "toolchain install is running") {
		t.Fatalf("failures %v", failures)
	}
	if _, err := os.Stat(asdfRuby(h, "3.3.4")); err != nil {
		t.Fatal("removed during an install")
	}
}

func TestToolchainCleanupRemovesManagedRubyAndItsPrefixLink(t *testing.T) {
	h := newToolchainHost(t)
	unused := filepath.Join(h.tools.Dir, "ruby", "3.3.0")
	used := filepath.Join(h.tools.Dir, "ruby", "3.4.4")
	h.install(t, unused, h.old)
	h.install(t, used, h.old)
	link := h.tools.RubyPrefixLink("3.3.0")
	os.MkdirAll(filepath.Dir(link), 0o755)
	os.Symlink(unused, link)
	h.app(t, "shop", map[string]string{"Gemfile": "", ".ruby-version": "3.4.4\n", ".lidza-toolchain/ruby": "->" + used})
	items, notes := h.m.planToolchainHygiene(h.apps, h.now)
	if got := names(items); !slices.Equal(got, []string{"Ruby 3.3.0 (managed)"}) {
		t.Fatalf("items %v notes %v", got, notes)
	}
	if _, failures := applyCleanupItems(context.Background(), nil, items); len(failures) != 0 {
		t.Fatal(failures)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("the prefix link of a removed Ruby must go too")
	}
	if _, err := os.Stat(used); err != nil {
		t.Fatal("removed the linked Ruby")
	}
}

func TestToolchainCleanupPreviewListsUnusedVersions(t *testing.T) {
	h := productionLikeHost(t)
	items, _ := h.m.planToolchainHygiene(h.apps, h.now)
	// Planning is a dry run: nothing is removed.
	for _, item := range items {
		if _, err := os.Stat(item.Path); err != nil {
			t.Fatal("the plan removed", item.Path)
		}
	}
}

func TestManagedRubyEnvironmentAndPrepare(t *testing.T) {
	f, _ := bundlerFixture(t)
	m := f.m
	policy := NativeReleasePolicy{Root: f.root, Runtime: "rails", Toolchains: "managed"}
	env, err := nativeReleaseEnvironment(nativeReleaseAccount{Name: "app", Home: "/srv/app"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	gems := filepath.Join(f.root, "shared", "gems")
	for _, want := range []string{"GEM_HOME=" + gems, "GEM_PATH=" + gems + ":"} {
		if !slices.Contains(env, want) {
			t.Fatalf("env lacks %s: %v", want, env)
		}
	}
	if path := envOf(env, "PATH"); !strings.Contains(path, filepath.Join(f.root, ".lidza-build", "ruby", "bin")) {
		t.Fatalf("PATH = %s", path)
	}
	policy.Toolchains = "account"
	env, _ = nativeReleaseEnvironment(nativeReleaseAccount{Name: "app", Home: "/srv/app"}, policy)
	if envOf(env, "GEM_HOME") != "" {
		t.Fatal("account toolchains keep the account's own gem setup")
	}

	// A managed Ruby that is already installed is linked into the release.
	dir := t.TempDir()
	tools := release.Toolchains{Dir: filepath.Join(dir, "lidza")}
	os.MkdirAll(filepath.Join(tools.Dir, "ruby", "3.4.4", "bin"), 0o755)
	os.WriteFile(filepath.Join(tools.Dir, "ruby", "3.4.4", "bin", "ruby"), []byte("#!/bin/sh\n"), 0o755)
	saved := nativeToolchains
	nativeToolchains = func() release.Toolchains { return tools }
	t.Cleanup(func() { nativeToolchains = saved })
	current, _ := user.Current()
	uid, _ := strconv.Atoi(current.Uid)
	gid, _ := strconv.Atoi(current.Gid)
	l := release.Layout{Root: filepath.Join(dir, "app")}
	id := "20260101000000"
	os.MkdirAll(l.Release(id), 0o755)
	os.WriteFile(filepath.Join(l.Release(id), "Gemfile"), []byte("ruby \"3.4.4\"\n"), 0o644)
	var log bytes.Buffer
	policy = NativeReleasePolicy{Root: l.Root, Runtime: "rails", Toolchains: "managed"}
	if err := m.nativeReleaseManagedToolchains("shop", nativeReleaseAccount{UID: uid, GID: gid}, policy, &log)(l, id, ""); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(l.Release(id), release.ToolchainDir, "ruby")); err != nil || target != filepath.Join(tools.Dir, "ruby", "3.4.4") {
		t.Fatalf("ruby link %q %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(l.Root, "shared", "gems")); err != nil || !info.IsDir() {
		t.Fatal("GEM_HOME was not created", err)
	}
	if !strings.Contains(log.String(), "Ruby 3.4.4: already installed") {
		t.Fatalf("log: %s", log.String())
	}
}

func envOf(env []string, key string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, key+"="); ok {
			return value
		}
	}
	return ""
}
