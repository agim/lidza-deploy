package release

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// scriptedExecutor records the commands a profile runs, prints canned
// output for some and fails others.
type scriptedExecutor struct {
	commands []Command
	output   map[string]string
	fail     map[string]bool
	onRun    func(c Command)
}

func (r *scriptedExecutor) Run(_ context.Context, c Command) error {
	r.commands = append(r.commands, c)
	if out := r.output[c.Name]; out != "" && c.Stdout != nil {
		io.WriteString(c.Stdout, out)
	}
	if r.onRun != nil {
		r.onRun(c)
	}
	if r.fail[c.Name] {
		return errors.New("exit status 1")
	}
	return nil
}

func (r *scriptedExecutor) names() []string {
	var names []string
	for _, c := range r.commands {
		names = append(names, c.Name)
	}
	return names
}

// railsRelease prepares a release directory with a Gemfile and the bundle
// configuration the Rails profile writes, and a bundle path holding one file.
func railsRelease(t *testing.T, deployment bool) (dir, bundle string) {
	t.Helper()
	root := t.TempDir()
	dir = filepath.Join(root, "releases", "20261106140000")
	bundle = filepath.Join(root, "shared", "bundle")
	for _, path := range []string{filepath.Join(dir, ".bundle"), bundle} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	config := "---\nBUNDLE_PATH: \"" + bundle + "\"\nBUNDLE_WITHOUT: \"development:test\"\n"
	if deployment {
		config += "BUNDLE_DEPLOYMENT: \"true\"\n"
	}
	for path, body := range map[string]string{
		filepath.Join(dir, "Gemfile"):           "source 'https://rubygems.org'\n",
		filepath.Join(dir, ".bundle", "config"): config,
		filepath.Join(bundle, "stale.gem"):      strings.Repeat("g", 2048),
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, bundle
}

func railsBuild(t *testing.T, dir string, x Executor, opts RailsOptions) string {
	t.Helper()
	var log bytes.Buffer
	p := Plan{App: "app", Layout: Layout{Root: filepath.Dir(filepath.Dir(dir))}, Exec: x, Log: &log, Profile: Rails(opts),
		Env: []string{"PATH=/usr/bin", "HOME=/home/app"}}
	if err := p.build(context.Background(), dir); err != nil {
		t.Fatal(err, log.String())
	}
	return log.String()
}

func TestRailsCleansUnusedGemsAfterInstallAndReportsOnce(t *testing.T) {
	dir, bundle := railsRelease(t, true)
	x := &scriptedExecutor{output: map[string]string{"bundle clean": "Removing nokogiri (1.15.0)\nRemoving rack (2.2.0)\n"},
		onRun: func(c Command) {
			if c.Name == "bundle clean" {
				os.Remove(filepath.Join(bundle, "stale.gem"))
			}
		}}
	log := railsBuild(t, dir, x, RailsOptions{})
	if names := x.names(); !slices.Equal(names, []string{"bundle install", "bundle clean"}) {
		t.Fatal("steps", names)
	}
	clean := x.commands[1]
	if !slices.Equal(clean.Args, []string{"bundle", "clean"}) || clean.Dir != dir || clean.Network || clean.Database {
		t.Fatalf("bundle clean command %+v", clean)
	}
	for _, want := range []string{"RAILS_ENV=production", "BUNDLE_GEMFILE=" + filepath.Join(dir, "Gemfile"), "PATH=/usr/bin"} {
		if !slices.Contains(clean.Env, want) {
			t.Fatalf("bundle clean environment lacks %s: %v", want, clean.Env)
		}
	}
	if log != "==> bundle install\n==> bundle clean removed 2 unused gems from "+bundle+", freeing 2.0 KiB\n" {
		t.Fatalf("log:\n%s", log)
	}
}

func TestRailsBundleCleanIsSilentWhenNothingIsRemoved(t *testing.T) {
	dir, _ := railsRelease(t, true)
	x := &scriptedExecutor{}
	if log := railsBuild(t, dir, x, RailsOptions{}); log != "==> bundle install\n" {
		t.Fatalf("log:\n%s", log)
	}
	if names := x.names(); !slices.Equal(names, []string{"bundle install", "bundle clean"}) {
		t.Fatal("steps", names)
	}
}

func TestRailsBundleCleanFailureIsAWarning(t *testing.T) {
	dir, bundle := railsRelease(t, true)
	x := &scriptedExecutor{fail: map[string]bool{"bundle clean": true}, output: map[string]string{"bundle clean": "Could not locate Gemfile\n"}}
	log := railsBuild(t, dir, x, RailsOptions{})
	want := "==> warning: bundle clean failed, unused gems stay in " + bundle + " (exit status 1): Could not locate Gemfile\n"
	if !strings.Contains(log, want) {
		t.Fatalf("log:\n%s", log)
	}
}

func TestRailsBundleCleanOnlyRunsForDeploymentBundlesAndCanBeDisabled(t *testing.T) {
	dir, _ := railsRelease(t, false)
	x := &scriptedExecutor{}
	railsBuild(t, dir, x, RailsOptions{})
	if names := x.names(); !slices.Equal(names, []string{"bundle install"}) {
		t.Fatal("bundle clean ran without deployment mode", names)
	}
	dir, _ = railsRelease(t, true)
	x = &scriptedExecutor{}
	off := false
	railsBuild(t, dir, x, RailsOptions{Clean: &off})
	if names := x.names(); !slices.Equal(names, []string{"bundle install"}) {
		t.Fatal("bundle clean ran although disabled", names)
	}
	if slices.ContainsFunc(Rails(RailsOptions{Clean: &off}).Build, func(s Step) bool { return s.Name == "bundle clean" }) {
		t.Fatal("disabled profile still carries the step")
	}
	// A relative or missing path is never cleaned.
	if err := os.WriteFile(filepath.Join(dir, ".bundle", "config"), []byte("BUNDLE_PATH: vendor/bundle\nBUNDLE_DEPLOYMENT: \"true\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if bundleDeploymentPath(dir) != "" {
		t.Fatal("relative bundle path accepted")
	}
}
