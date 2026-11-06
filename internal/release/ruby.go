package release

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Ruby toolchain compatibility. Each RubyGems release ships a default
// Bundler: RubyGems 3.x.y ships Bundler 2.x.y (same minor and patch) and
// RubyGems 4.x.y ships Bundler 4.x.y. When a Gemfile.lock records an older
// Bundler under BUNDLED WITH, RubyGems activates that version if it is
// installed (Bundler installs it otherwise), and an old Bundler running on a
// newer RubyGems redefines RubyGems constants, printing "already initialized
// constant Gem::Platform::JAVA" on every command. Such a release uses the
// host's default Bundler instead, through BUNDLER_VERSION. Nothing is ever
// installed or removed on the host.

// RubyToolchain describes the Ruby and Bundler one release was built with.
type RubyToolchain struct {
	Ruby           string    `json:"ruby"`
	RubyGems       string    `json:"rubygems"`
	DefaultBundler string    `json:"default_bundler,omitempty"`
	Installed      []string  `json:"installed_bundlers,omitempty"` // other installed Bundler versions
	Pinned         string    `json:"pinned_bundler,omitempty"`     // BUNDLED WITH in Gemfile.lock
	Selected       string    `json:"bundler_version,omitempty"`    // BUNDLER_VERSION the release uses; "" leaves the choice to RubyGems
	Checked        time.Time `json:"checked"`
}

// ToolchainCheck is the outcome of a profile's toolchain check: environment
// entries for build, release and runtime processes, and what was found.
type ToolchainCheck struct {
	Env  []string
	Ruby *RubyToolchain
}

// RuntimeEnvFile is the release's runtime environment written by the engine
// (KEY=value lines, systemd EnvironmentFile syntax), relative to the release.
// Process managers read it through current/, so it follows every rollback.
var RuntimeEnvFile = filepath.Join(ToolchainDir, "env")

// runtimeEnvKeys are the only keys the engine writes to, or reads back from,
// a release's runtime environment file.
var runtimeEnvKeys = []string{"BUNDLER_VERSION"}

var (
	bundledWithPattern = regexp.MustCompile(`(?m)^BUNDLED WITH\r?\n\s+([0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.]*)\s*$`)
	gemVersionPattern  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.]*$`)
)

// LockedBundler returns the Bundler version Gemfile.lock records under
// BUNDLED WITH, or "".
func LockedBundler(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "Gemfile.lock"))
	if err != nil {
		return ""
	}
	if match := bundledWithPattern.FindSubmatch(raw); match != nil {
		return string(match[1])
	}
	return ""
}

// gemVersionKey returns the numeric components of a version ("2.6.9" -> 2,6,9),
// stopping at the first non-numeric part (pre-release suffixes).
func gemVersionKey(version string) []int {
	var key []int
	for _, part := range strings.Split(version, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			break
		}
		key = append(key, n)
	}
	return key
}

// majorMinor returns the first two components of a version, or nil.
func majorMinor(version string) []int {
	key := gemVersionKey(version)
	if len(key) < 2 {
		return nil
	}
	return key[:2]
}

// PairedBundler returns the default Bundler that ships with a RubyGems
// version: RubyGems 3.x.y ↔ Bundler 2.x.y, RubyGems 4.x.y and later ↔ the
// same version. Older or unparsable versions return "".
func PairedBundler(rubygems string) string {
	key := gemVersionKey(rubygems)
	if len(key) < 3 || !gemVersionPattern.MatchString(rubygems) {
		return ""
	}
	switch {
	case key[0] == 3:
		return fmt.Sprintf("2.%d.%d", key[1], key[2])
	case key[0] >= 4:
		return rubygems
	}
	return ""
}

// ChooseBundler returns the Bundler version a release must use instead of
// the pinned one, or "" when the pinned version is fine (or unknown). The
// pinned version is replaced only when its major.minor is older than the
// host's default Bundler; defaultBundler may be empty, and is then derived
// from the RubyGems version.
func ChooseBundler(rubygems, defaultBundler, pinned string) string {
	if defaultBundler == "" {
		defaultBundler = PairedBundler(rubygems)
	}
	want, have := majorMinor(defaultBundler), majorMinor(pinned)
	if want == nil || have == nil || slices.Compare(have, want) >= 0 {
		return ""
	}
	return defaultBundler
}

// NewerBundler reports a pinned Bundler whose major.minor is newer than the
// host's default (derived from the RubyGems version when defaultBundler is
// empty): RubyGems cannot activate it, so the release would fail. Unknown
// versions report false.
func NewerBundler(rubygems, defaultBundler, pinned string) bool {
	if defaultBundler == "" {
		defaultBundler = PairedBundler(rubygems)
	}
	want, have := majorMinor(defaultBundler), majorMinor(pinned)
	return want != nil && have != nil && slices.Compare(have, want) > 0
}

// StaleBundlers returns the installed Bundler versions older (by
// major.minor) than the default one, newest first.
func StaleBundlers(defaultBundler string, installed []string) []string {
	want := majorMinor(defaultBundler)
	if want == nil {
		return nil
	}
	var stale []string
	for _, version := range installed {
		if have := majorMinor(version); have != nil && slices.Compare(have, want) < 0 {
			stale = append(stale, version)
		}
	}
	slices.SortFunc(stale, func(a, b string) int { return slices.Compare(gemVersionKey(b), gemVersionKey(a)) })
	return stale
}

// parseGemList reads `gem list -e bundler` output, for example
// "bundler (2.7.1, default: 4.0.23, 2.6.9)", into the default version and
// the other installed versions.
func parseGemList(out string) (defaultVersion string, others []string) {
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		rest, ok := strings.CutPrefix(line, "bundler (")
		if !ok || !strings.HasSuffix(rest, ")") {
			continue
		}
		for _, entry := range strings.Split(strings.TrimSuffix(rest, ")"), ",") {
			entry = strings.TrimSpace(entry)
			version, isDefault := strings.CutPrefix(entry, "default: ")
			// Platform-specific entries such as "2.6.9 x86_64-linux".
			version, _, _ = strings.Cut(version, " ")
			if !gemVersionPattern.MatchString(version) {
				continue
			}
			if isDefault {
				defaultVersion = version
			} else if !slices.Contains(others, version) {
				others = append(others, version)
			}
		}
	}
	return defaultVersion, others
}

// ParseBundlerList reads `gem list --local -e bundler` output into the
// default Bundler version and the other installed versions.
func ParseBundlerList(out string) (defaultVersion string, others []string) {
	return parseGemList(out)
}

// GemVersion reports whether value is a plain gem version such as 2.6.9.
func GemVersion(value string) bool { return gemVersionPattern.MatchString(value) }

// rubyProbeScript prints the Ruby version, the RubyGems version and the
// installed Bundler versions of the Ruby the release selects.
const rubyProbeScript = `ruby -e 'print RUBY_VERSION' && echo && gem --version && gem list --local -e bundler`

// probeRuby runs the probe inside the release with the build environment, so
// version managers select the release's Ruby.
func probeRuby(ctx context.Context, x Executor, dir string, env []string) (RubyToolchain, error) {
	out, err := output(ctx, x, Command{Name: "ruby toolchain", Dir: dir, Args: []string{"/bin/sh", "-c", rubyProbeScript}, Env: env})
	if err != nil {
		return RubyToolchain{}, err
	}
	lines := strings.SplitN(out, "\n", 3)
	if len(lines) < 2 {
		return RubyToolchain{}, errors.New("ruby toolchain: unexpected output")
	}
	tool := RubyToolchain{Ruby: strings.TrimSpace(lines[0]), RubyGems: strings.TrimSpace(lines[1])}
	if !gemVersionPattern.MatchString(tool.Ruby) || !gemVersionPattern.MatchString(tool.RubyGems) {
		return RubyToolchain{}, errors.New("ruby toolchain: unexpected version output")
	}
	if len(lines) == 3 {
		tool.DefaultBundler, tool.Installed = parseGemList(lines[2])
	}
	if tool.DefaultBundler == "" {
		tool.DefaultBundler = PairedBundler(tool.RubyGems)
	}
	return tool, nil
}

// checkRubyToolchain is the Rails profile's toolchain check. A release
// without BUNDLED WITH, or one whose pinned Bundler already matches the
// host, runs exactly as before.
func checkRubyToolchain(ctx context.Context, x Executor, dir string, env []string, logf func(string, ...any)) ToolchainCheck {
	pinned := LockedBundler(dir)
	if pinned == "" {
		return ToolchainCheck{}
	}
	if _, set := envValue(env, "BUNDLER_VERSION"); set {
		// The application chose its Bundler explicitly.
		return ToolchainCheck{}
	}
	tool, err := probeRuby(ctx, x, dir, env)
	if err != nil {
		logf("could not read this host's RubyGems version (%v); Bundler %s is left to RubyGems", err, pinned)
		return ToolchainCheck{}
	}
	tool.Pinned = pinned
	tool.Checked = time.Now().UTC()
	check := ToolchainCheck{Ruby: &tool}
	if use := ChooseBundler(tool.RubyGems, tool.DefaultBundler, pinned); use != "" {
		tool.Selected = use
		check.Env = []string{"BUNDLER_VERSION=" + use}
		logf("Bundler %s is older than this host's RubyGems %s; using Bundler %s", pinned, tool.RubyGems, use)
	}
	return check
}

// writeRuntimeEnv records the release's runtime environment entries.
func (l Layout) writeRuntimeEnv(id string, env []string) error {
	var body strings.Builder
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if !slices.Contains(runtimeEnvKeys, key) || !gemVersionPattern.MatchString(value) {
			return fmt.Errorf("release: %s is not a runtime environment entry", key)
		}
		body.WriteString(key + "=" + value + "\n")
	}
	if err := l.mkdirOwned(l.Release(id), ToolchainDir); err != nil {
		return err
	}
	path := filepath.Join(l.Release(id), RuntimeEnvFile)
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("release: %s is not a regular file", RuntimeEnvFile)
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		return err
	}
	return l.chown(path)
}

// RuntimeEnv reads the runtime environment entries the engine recorded in a
// release; anything else in the file is ignored.
func (l Layout) RuntimeEnv(id string) []string {
	path := filepath.Join(l.Release(id), RuntimeEnvFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var env []string
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && slices.Contains(runtimeEnvKeys, key) && gemVersionPattern.MatchString(value) {
			env = append(env, key+"="+value)
		}
	}
	return env
}
