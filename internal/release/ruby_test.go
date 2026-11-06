package release

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestPairedBundler(t *testing.T) {
	for rubygems, want := range map[string]string{
		"3.5.22": "2.5.22", "3.6.9": "2.6.9", "3.7.1": "2.7.1",
		"4.0.23": "4.0.23", "4.1.0": "4.1.0",
		"2.7.6": "", "": "", "3.6": "", "garbage": "",
	} {
		if got := PairedBundler(rubygems); got != want {
			t.Errorf("PairedBundler(%q) = %q, want %q", rubygems, got, want)
		}
	}
}

func TestChooseBundler(t *testing.T) {
	for _, c := range []struct{ rubygems, def, pinned, want string }{
		{"4.0.23", "", "2.6.9", "4.0.23"},       // older major: use the host's default
		{"4.0.23", "", "2.7.2", "4.0.23"},       // any 2.x on RubyGems 4
		{"4.0.23", "4.0.23", "4.0.10", ""},      // same major.minor: unchanged
		{"4.0.23", "", "4.1.0", ""},             // newer pin: left to RubyGems
		{"3.6.9", "", "2.5.23", "2.6.9"},        // older minor on RubyGems 3
		{"3.6.9", "", "2.6.2", ""},              // matching minor: unchanged
		{"3.7.1", "", "2.7.1", ""},              // exact match: unchanged
		{"4.0.23", "4.0.20", "2.6.9", "4.0.20"}, // the reported default wins
		{"4.0.23", "", "", ""},                  // no pin
		{"2.7.6", "", "1.17.2", ""},             // unknown pairing
	} {
		if got := ChooseBundler(c.rubygems, c.def, c.pinned); got != c.want {
			t.Errorf("ChooseBundler(%q, %q, %q) = %q, want %q", c.rubygems, c.def, c.pinned, got, c.want)
		}
	}
}

func TestNewerBundler(t *testing.T) {
	for _, c := range []struct {
		rubygems, def, pinned string
		want                  bool
	}{
		{"4.0.23", "", "4.1.0", true},       // newer minor than RubyGems 4.0 ships
		{"4.0.23", "4.0.23", "5.0.0", true}, // newer major
		{"3.6.9", "", "2.7.1", true},        // RubyGems 3.6 pairs with Bundler 2.6
		{"4.0.23", "", "4.0.30", false},     // same major.minor
		{"4.0.23", "", "2.6.9", false},      // older: the release uses the default
		{"4.0.23", "", "", false},           // no pin
		{"2.7.6", "", "4.1.0", false},       // unknown pairing
	} {
		if got := NewerBundler(c.rubygems, c.def, c.pinned); got != c.want {
			t.Errorf("NewerBundler(%q, %q, %q) = %v, want %v", c.rubygems, c.def, c.pinned, got, c.want)
		}
	}
}

func TestParseGemListAndStaleBundlers(t *testing.T) {
	def, others := parseGemList("\n*** LOCAL GEMS ***\n\nbundler (2.7.1, default: 4.0.23, 2.6.9 x86_64-linux)\n")
	if def != "4.0.23" || !slices.Equal(others, []string{"2.7.1", "2.6.9"}) {
		t.Fatalf("parsed %q %v", def, others)
	}
	if def, others := parseGemList("bundler (default: 2.6.9)"); def != "2.6.9" || len(others) != 0 {
		t.Fatalf("default only: %q %v", def, others)
	}
	if got := StaleBundlers("4.0.23", []string{"2.6.9", "4.0.10", "2.7.1", "4.1.0"}); !slices.Equal(got, []string{"2.7.1", "2.6.9"}) {
		t.Fatalf("stale %v", got)
	}
}

func TestLockedBundler(t *testing.T) {
	dir := t.TempDir()
	if LockedBundler(dir) != "" {
		t.Fatal("no lockfile must give no pin")
	}
	lock := "GEM\n  remote: https://rubygems.org/\n  specs:\n    rack (3.1.8)\n\nPLATFORMS\n  ruby\n\nBUNDLED WITH\n   2.6.9\n"
	os.WriteFile(filepath.Join(dir, "Gemfile.lock"), []byte(lock), 0o644)
	if got := LockedBundler(dir); got != "2.6.9" {
		t.Fatalf("pin %q", got)
	}
	os.WriteFile(filepath.Join(dir, "Gemfile.lock"), []byte(strings.Replace(lock, "\n", "\r\n", -1)), 0o644)
	if got := LockedBundler(dir); got != "2.6.9" {
		t.Fatalf("CRLF pin %q", got)
	}
}

// rubyHostExecutor answers the Ruby probe like a host whose RubyGems is
// 4.0.23 and records the environment of every other command.
type rubyHostExecutor struct {
	mu       sync.Mutex
	rubygems string
	probes   int
	envs     map[string][]string
}

func (x *rubyHostExecutor) Run(ctx context.Context, c Command) error {
	x.mu.Lock()
	if c.Name == "ruby toolchain" {
		x.probes++
		x.mu.Unlock()
		fmt.Fprintf(c.Stdout, "3.4.4\n%s\n\n*** LOCAL GEMS ***\n\nbundler (2.6.9, default: %s)\n", x.rubygems, PairedBundler(x.rubygems))
		return nil
	}
	x.envs[c.Name] = c.Env
	x.mu.Unlock()
	return LocalExecutor{}.Run(ctx, c)
}

func rubyPlan(t *testing.T, bundled, rubygems string) (Plan, *rubyHostExecutor) {
	url, commit := gitFixture(t)
	commit("config/cron", "*/5 * * * * /bin/true\n")
	if bundled != "" {
		commit("Gemfile.lock", "GEM\n  specs:\n\nBUNDLED WITH\n   "+bundled+"\n")
	}
	commit("VERSION", "one\n")
	p, _, _, _ := testPlan(t, url)
	x := &rubyHostExecutor{rubygems: rubygems, envs: map[string][]string{}}
	p.Exec = x
	p.Profile.Toolchain = checkRubyToolchain
	return p, x
}

func TestDeployUsesHostBundlerWhenTheLockfilePinsAnOlderOne(t *testing.T) {
	p, x := rubyPlan(t, "2.6.9", "4.0.23")
	result, err := Deploy(context.Background(), p)
	if err != nil {
		t.Fatal(err, p.Log)
	}
	want := "BUNDLER_VERSION=4.0.23"
	for _, step := range []string{"build", "schedule"} {
		if !slices.Contains(x.envs[step], want) {
			t.Fatalf("%s env lacks %s: %v", step, want, x.envs[step])
		}
	}
	if got := readFile(t, filepath.Join(p.Layout.Current(), RuntimeEnvFile)); got != want+"\n" {
		t.Fatalf("runtime env %q", got)
	}
	if r := result.Ruby; r == nil || r.Ruby != "3.4.4" || r.Pinned != "2.6.9" || r.Selected != "4.0.23" || !slices.Equal(r.Installed, []string{"2.6.9"}) {
		t.Fatalf("result %+v", result.Ruby)
	}
	log := fmt.Sprint(p.Log)
	if strings.Count(log, "Bundler 2.6.9 is older than this host's RubyGems 4.0.23; using Bundler 4.0.23") != 1 {
		t.Fatalf("log:\n%s", log)
	}
	// A rollback renders the schedule with the target release's toolchain.
	second, err := Deploy(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	x.envs = map[string][]string{}
	p.Env = slices.DeleteFunc(slices.Clone(p.Env), func(e string) bool { return strings.HasPrefix(e, "BUNDLER_VERSION=") })
	if _, err := Rollback(context.Background(), p, result.Release); err != nil || second.Release == result.Release {
		t.Fatal(err)
	}
	if !slices.Contains(x.envs["schedule"], want) {
		t.Fatalf("rollback schedule env %v", x.envs["schedule"])
	}
}

func TestDeployLeavesMatchingOrMissingBundlerPinsAlone(t *testing.T) {
	for _, c := range []struct{ bundled, rubygems string }{{"4.0.10", "4.0.23"}, {"2.6.9", "3.6.9"}, {"", "4.0.23"}} {
		p, x := rubyPlan(t, c.bundled, c.rubygems)
		result, err := Deploy(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(x.envs["build"], func(e string) bool { return strings.HasPrefix(e, "BUNDLER_VERSION=") }) {
			t.Fatalf("%+v: BUNDLER_VERSION set", c)
		}
		if _, err := os.Lstat(filepath.Join(p.Layout.Current(), RuntimeEnvFile)); !os.IsNotExist(err) {
			t.Fatalf("%+v: runtime env written", c)
		}
		if strings.Contains(fmt.Sprint(p.Log), "Bundler") {
			t.Fatalf("%+v: logged %s", c, p.Log)
		}
		if c.bundled == "" {
			if x.probes != 0 || result.Ruby != nil {
				t.Fatal("a lockfile without BUNDLED WITH must not be probed")
			}
		} else if result.Ruby == nil || result.Ruby.Selected != "" {
			t.Fatalf("%+v: result %+v", c, result.Ruby)
		}
	}
}

func TestDeployRespectsAnExplicitBundlerVersion(t *testing.T) {
	p, x := rubyPlan(t, "2.6.9", "4.0.23")
	p.Env = append(p.Env, "BUNDLER_VERSION=2.6.9")
	if _, err := Deploy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if x.probes != 0 || !slices.Contains(x.envs["build"], "BUNDLER_VERSION=2.6.9") {
		t.Fatal("the application's own BUNDLER_VERSION was replaced")
	}
}

func TestRuntimeEnvReadsOnlyEngineKeys(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	dir := filepath.Join(l.Release("r1"), ToolchainDir)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "env"), []byte("PATH=/evil\nBUNDLER_VERSION=4.0.23\nBUNDLER_VERSION=$(x)\n"), 0o644)
	if got := l.RuntimeEnv("r1"); !slices.Equal(got, []string{"BUNDLER_VERSION=4.0.23"}) {
		t.Fatalf("runtime env %v", got)
	}
	if err := l.writeRuntimeEnv("r1", []string{"PATH=/x"}); err == nil {
		t.Fatal("non-runtime key written")
	}
}
