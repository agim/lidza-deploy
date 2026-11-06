package agent

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// bundlerFixture is a Rails application with two kept releases: the current
// one pins Bundler 2.3.5 but runs with the host default 2.6.9 (its runtime
// BUNDLER_VERSION); the previous one pins 2.4.10 and runs with it.
func bundlerFixture(t *testing.T) (*nativeReleaseFixture, string) {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	savedLookup, savedPath := nativeLookupUser, hostPath
	nativeLookupUser = func(string) (*user.User, error) { return current, nil }
	empty := t.TempDir()
	hostPath = func(path string) string { return filepath.Join(empty, path) } // not an apt host: no package refresh
	t.Cleanup(func() { nativeLookupUser, hostPath = savedLookup, savedPath })
	f := newNativeReleaseFixture(t)
	policy := f.policy()
	policy.Runtime = "rails"
	f.m.cfg.Native.Releases[f.app.ID] = policy
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	releaseDir := filepath.Join(f.root, "releases", f.original)
	write(filepath.Join(releaseDir, "Gemfile.lock"), "PATH\n  remote: vendor/gems/local\n  specs:\n    local (0.1.0)\n\nPATH\n  remote: ../outside\n  specs:\n    outside (1.0)\n\nGEM\n  remote: https://rubygems.org/\n  specs:\n    rake (13.0.0)\n\nBUNDLED WITH\n   2.3.5\n")
	write(filepath.Join(releaseDir, "vendor/gems/local/local.gemspec"), "Gem::Specification.new\n")
	write(filepath.Join(releaseDir, ".ruby-version"), "3.3.0\n")
	write(filepath.Join(releaseDir, release.RuntimeEnvFile), "BUNDLER_VERSION=2.6.9\n")
	if err := os.Symlink("/etc", filepath.Join(releaseDir, "vendor/gems/local/linked")); err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(f.root, "releases", "20251201000000")
	write(filepath.Join(previous, "Gemfile"), "source 'https://rubygems.org'\n")
	write(filepath.Join(previous, "REVISION"), strings.Repeat("1", 40)+"\n")
	write(filepath.Join(previous, "Gemfile.lock"), "GEM\n  specs:\n\nBUNDLED WITH\n   2.4.10\n")
	f.m.data.NativeRuby = map[string]release.RubyToolchain{f.app.ID: {Ruby: "3.3.0", RubyGems: "3.6.9", DefaultBundler: "2.6.9", Installed: []string{"2.4.10", "2.3.5", "2.1.4"}, Pinned: "2.3.5", Selected: "2.6.9"}}
	gems := t.TempDir()
	return f, gems
}

func TestBundlerPlanKeepsVersionsAReleaseRunsWith(t *testing.T) {
	f, _ := bundlerFixture(t)
	status := f.m.bundlerStatus()
	if len(status.Groups) != 1 {
		t.Fatalf("groups %+v", status.Groups)
	}
	group := status.Groups[0]
	if group.Ruby != "3.3.0" || group.Default != "2.6.9" || !slices.Equal(group.Apps, []string{f.app.ID}) {
		t.Fatalf("group %+v", group)
	}
	if !slices.Equal(group.Remove, []string{"2.3.5", "2.1.4"}) {
		t.Fatal("removable", group.Remove)
	}
	if len(group.Keep) != 1 || group.Keep[0].Version != "2.4.10" || !strings.Contains(group.Keep[0].Reason, "20251201000000") {
		t.Fatal("kept", group.Keep)
	}
	for _, invalid := range []BundlerRemoval{{}, {Remove: []BundlerSelection{{User: "u", Ruby: "3.3.0", Version: "2.3.5; rm -rf /"}}}} {
		if invalid.Validate() == nil {
			t.Fatal("invalid selection accepted", invalid)
		}
	}
}

func TestBundlerRemovalChecksEveryBundleFirst(t *testing.T) {
	f, gems := bundlerFixture(t)
	var mu sync.Mutex
	var commands []string
	checks := 0
	failCheck := false
	f.m.nativeReleaseRun = func(_ context.Context, c release.Command) error {
		mu.Lock()
		defer mu.Unlock()
		command := strings.Join(c.Args, " ")
		commands = append(commands, command)
		switch {
		case command == "gem list --local --exact bundler":
			fmt.Fprintln(c.Stdout, "bundler (2.4.10, 2.3.5, default: 2.6.9)")
		case strings.HasPrefix(command, "ruby -e"):
			fmt.Fprintln(c.Stdout, gems)
		case command == "bundle exec ruby -e 1":
			checks++
			scratch := bundlerCheckDir(f.root)
			if c.Dir != scratch {
				return fmt.Errorf("check ran in %s", c.Dir)
			}
			for _, want := range []string{"Gemfile", "Gemfile.lock", ".ruby-version", "vendor/gems/local/local.gemspec"} {
				if _, err := os.Stat(filepath.Join(scratch, want)); err != nil {
					return fmt.Errorf("scratch copy lacks %s", want)
				}
			}
			if _, err := os.Lstat(filepath.Join(scratch, "vendor/gems/local/linked")); err == nil {
				return fmt.Errorf("a symbolic link was copied")
			}
			if !slices.Contains(c.Env, "BUNDLE_GEMFILE="+filepath.Join(scratch, "Gemfile")) || !slices.Contains(c.Env, "BUNDLER_VERSION=2.6.9") || !slices.Contains(c.Env, "BUNDLE_FROZEN=true") {
				return fmt.Errorf("check environment %v", c.Env)
			}
			if failCheck {
				return fmt.Errorf("Could not find gem")
			}
		}
		return nil
	}
	wait := func() HostJob {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if job := f.m.bundlerStatus().Job; job != nil && job.Status != "running" {
				return *job
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("bundler job did not finish")
		return HostJob{}
	}
	account, _ := user.Current()
	selection := BundlerRemoval{Remove: []BundlerSelection{{User: account.Username, Ruby: "3.3.0", Version: "2.3.5"}, {User: account.Username, Ruby: "3.3.0", Version: "2.4.10"}}}

	// An application that does not resolve its bundle stops the removal.
	failCheck = true
	if err := f.m.startBundlerCleanup(selection); err != nil {
		t.Fatal(err)
	}
	if job := wait(); job.Status != "failed" || !strings.Contains(job.Summary, "nothing was removed") {
		t.Fatalf("failed check did not stop the removal: %+v", job)
	}
	if slices.ContainsFunc(commands, func(c string) bool { return strings.HasPrefix(c, "gem uninstall") }) {
		t.Fatal("removed despite the failed check", commands)
	}

	failCheck = false
	commands, checks = nil, 0
	if err := f.m.startBundlerCleanup(selection); err != nil {
		t.Fatal(err)
	}
	job := wait()
	if job.Status != "succeeded" || !strings.Contains(job.Summary, "Bundler 2.3.5 from Ruby 3.3.0") {
		t.Fatalf("job %+v\n%s", job, job.Output)
	}
	if !slices.Contains(commands, "gem uninstall bundler --version 2.3.5 --executables --ignore-dependencies") || slices.ContainsFunc(commands, func(c string) bool { return strings.Contains(c, "2.4.10") && strings.HasPrefix(c, "gem uninstall") }) {
		t.Fatal("commands", commands)
	}
	if checks != 2 {
		t.Fatal("bundle checks before and after", checks)
	}
	if _, err := os.Stat(bundlerCheckDir(f.root)); !os.IsNotExist(err) {
		t.Fatal("scratch copy left behind")
	}
	if installed := f.m.data.NativeRuby[f.app.ID].Installed; slices.Contains(installed, "2.3.5") {
		t.Fatal("record still lists the removed version", installed)
	}
	// The Ruby warning no longer names the removed version.
	warnings, _ := f.m.nativeRubyHealth()
	for _, warning := range warnings {
		if strings.Contains(warning, "2.3.5,") || strings.HasSuffix(warning, "2.3.5") {
			t.Fatal("warning still lists 2.3.5", warning)
		}
	}
	// Nothing removable left from the selection.
	if err := f.m.startBundlerCleanup(BundlerRemoval{Remove: []BundlerSelection{{User: account.Username, Ruby: "3.3.0", Version: "2.4.10"}}}); err == nil {
		t.Fatal("a version a release runs with was accepted")
	}
	// The route refuses an invalid body.
	w := f.call(t, func(w http.ResponseWriter, r *http.Request) {
		mux := http.NewServeMux()
		f.m.bundlerRoutes(mux)
		r.Method, r.URL.Path = "POST", "/v1/host/ruby/bundlers/remove"
		mux.ServeHTTP(w, r)
	}, `{"remove":[]}`)
	if w.Code != 400 {
		t.Fatal("empty selection accepted", w.Code)
	}
}
