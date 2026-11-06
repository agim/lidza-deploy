package release

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Ruby on demand. A release names its Ruby in .ruby-version, .tool-versions
// or its Gemfile; Toolchains.Ruby installs that CRuby release under
// <Dir>/ruby/<version> from one of two verified sources, in order:
//
//  1. a prebuilt binary from ruby/ruby-builder (the builds setup-ruby uses)
//     for the host's Ubuntu release and architecture, verified against the
//     SHA-256 digest GitHub publishes for the release asset. These builds are
//     compiled for the prefix /opt/hostedtoolcache/Ruby/<version>/<arch> and
//     are not relocatable, so that prefix becomes a link to the installation.
//     The prefix is only linked when nothing else occupies it, and the build
//     must name it in its rbconfig.rb and run on the host before it is kept.
//  2. the official source tarball from cache.ruby-lang.org, verified against
//     the SHA-256 in cache.ruby-lang.org/pub/ruby/index.txt, configured with
//     --prefix=<Dir>/ruby/<version>, built with make -j N under the host's
//     build limits and installed through DESTDIR into a staging directory.
//
// Either way the version appears at its final path in one rename.

// ToolchainCommand is one command of a toolchain source build. It runs as
// the installing user (root on servers) with exactly Env.
type ToolchainCommand struct {
	Name string // short label: "ruby configure", "ruby make", "ruby make install"
	Dir  string
	Args []string
	Env  []string
}

// RubyBuildPackages are the Debian/Ubuntu packages building Ruby needs; gems
// with native extensions need the same compiler and headers. Ruby enables
// YJIT by itself when a Rust compiler (rustc 1.58 or later) is installed.
var RubyBuildPackages = []string{"build-essential", "libssl-dev", "libyaml-dev", "zlib1g-dev", "libffi-dev", "libgmp-dev", "libreadline-dev"}

var (
	rubyVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)
	gemfileRubyPattern = regexp.MustCompile(`(?m)^\s*ruby\s*\(?\s*["']([^"']+)["']`)
	rbconfigPrefix     = regexp.MustCompile(`CONFIG\["prefix"\] = \(TOPDIR \|\| DESTDIR \+ "([^"]+)"\)`)
)

// NormalizeRubyVersion returns a CRuby version from a version file entry:
// "ruby-3.4.4" and "3.4.4" give 3.4.4, "3.4" stays a minor pin. Other Ruby
// implementations (jruby-9.4.8.0, truffleruby) and ranges give "".
func NormalizeRubyVersion(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "ruby-")
	// Patch levels such as 2.7.8-p225 name the same release.
	if base, _, ok := strings.Cut(value, "-p"); ok {
		value = base
	}
	if rubyVersionPattern.MatchString(value) {
		return value
	}
	return ""
}

// readPinFile returns the trimmed contents of a small regular file in dir.
func readPinFile(dir, name string) (string, bool) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// ToolVersions returns the first version .tool-versions in dir lists for
// each tool ("ruby", "nodejs", "golang", "rust", ...).
func ToolVersions(dir string) map[string]string {
	raw, ok := readPinFile(dir, ".tool-versions")
	if !ok {
		return nil
	}
	tools := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line, _, _ := strings.Cut(scanner.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) >= 2 && tools[fields[0]] == "" {
			tools[fields[0]] = fields[1]
		}
	}
	return tools
}

// RubyRequirement returns the CRuby version a release asks for and the file
// that names it: .ruby-version, then the ruby line of .tool-versions, then
// a `ruby "x.y.z"` line in the Gemfile. It returns "" when none pins a
// CRuby version.
func RubyRequirement(dir string) (version, source string) {
	if raw, ok := readPinFile(dir, ".ruby-version"); ok {
		line, _, _ := strings.Cut(strings.TrimSpace(raw), "\n")
		if v := NormalizeRubyVersion(line); v != "" {
			return v, ".ruby-version"
		}
	}
	if v := NormalizeRubyVersion(ToolVersions(dir)["ruby"]); v != "" {
		return v, ".tool-versions"
	}
	if raw, ok := readPinFile(dir, "Gemfile"); ok {
		if match := gemfileRubyPattern.FindStringSubmatch(raw); match != nil {
			if v := NormalizeRubyVersion(match[1]); v != "" {
				return v, "Gemfile"
			}
		}
	}
	return "", ""
}

// rubyPrebuiltUnavailable explains why no prebuilt Ruby fits the host; the
// installer then builds from source.
type rubyPrebuiltUnavailable struct{ reason string }

func (e rubyPrebuiltUnavailable) Error() string { return e.reason }

func (t Toolchains) rubyBuilder() string {
	if t.RubyBuilder == "" {
		return "https://api.github.com/repos/ruby/ruby-builder"
	}
	return strings.TrimSuffix(t.RubyBuilder, "/")
}

func (t Toolchains) rubyPrefix() string {
	if t.RubyPrefix == "" {
		return "/opt/hostedtoolcache/Ruby"
	}
	return t.RubyPrefix
}

// RubyPrefixLink is the prefix a prebuilt Ruby of version was compiled for,
// which links to its installation; "" for an unknown architecture.
func (t Toolchains) RubyPrefixLink(version string) string {
	_, arch := t.platform()
	name := map[string]string{"amd64": "x64", "arm64": "arm64"}[arch]
	if name == "" {
		return ""
	}
	return filepath.Join(t.rubyPrefix(), version, name)
}

// installedRuby returns the newest complete installation matching want.
func (t Toolchains) installedRuby(want string) string {
	version := t.installed("ruby", want)
	if version == "" {
		return ""
	}
	if info, err := os.Stat(filepath.Join(t.Dir, "ruby", version, "bin", "ruby")); err != nil || info.IsDir() {
		return ""
	}
	return version
}

// Ruby returns the root of a CRuby installation for want (3.4.4, ruby-3.4.4
// or a minor pin such as 3.4, which selects its newest installed or
// published patch release), installing it when necessary.
func (t Toolchains) Ruby(ctx context.Context, want string) (string, error) {
	started := time.Now()
	version := NormalizeRubyVersion(want)
	if version == "" {
		return "", fmt.Errorf("release: unsupported Ruby version %q (a CRuby version such as 3.4.4)", want)
	}
	if found := t.installedRuby(version); found != "" {
		t.logf("Ruby %s: already installed", found)
		return filepath.Join(t.Dir, "ruby", found), nil
	}
	unlock, err := t.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if found := t.installedRuby(version); found != "" {
		t.logf("Ruby %s: already installed", found)
		return filepath.Join(t.Dir, "ruby", found), nil
	}
	sources, err := t.rubySources(ctx)
	if err != nil {
		return "", err
	}
	if !exactVersion.MatchString(version) {
		var matches []string
		for candidate := range sources {
			if matchesVersion(candidate, version) {
				matches = append(matches, candidate)
			}
		}
		if version = newestVersion(matches); version == "" {
			return "", fmt.Errorf("release: no published Ruby release matches %q", want)
		}
		if found := t.installedRuby(version); found != "" {
			t.logf("Ruby %s: already installed", found)
			return filepath.Join(t.Dir, "ruby", found), nil
		}
	}
	source, published := sources[version]
	if !published {
		return "", fmt.Errorf("release: Ruby %s is not a published release (cache.ruby-lang.org lists no ruby-%s.tar.gz)", version, version)
	}
	if t.InstallPackages != nil {
		if err := t.InstallPackages(ctx, RubyBuildPackages); err != nil {
			return "", fmt.Errorf("release: Ruby %s build dependencies: %w", version, err)
		}
	}
	how := "prebuilt"
	err = t.rubyPrebuilt(ctx, version)
	var unavailable rubyPrebuiltUnavailable
	if errors.As(err, &unavailable) {
		t.logf("Ruby %s: no usable prebuilt binary (%s); building from source", version, unavailable.reason)
		how = "built from source"
		err = t.rubyFromSource(ctx, version, source)
	}
	if err != nil {
		return "", err
	}
	t.logf("Ruby %s: installed (%s, %s)", version, how, elapsedText(time.Since(started)))
	return filepath.Join(t.Dir, "ruby", version), nil
}

// elapsedText renders a duration as "38 s" or "6 min 12 s".
func elapsedText(d time.Duration) string {
	seconds := int(d.Round(time.Second) / time.Second)
	if seconds < 60 {
		return strconv.Itoa(seconds) + " s"
	}
	return fmt.Sprintf("%d min %d s", seconds/60, seconds%60)
}

type rubySource struct{ url, sum string }

// rubySources reads cache.ruby-lang.org's index of source tarballs: exact
// versions with their .tar.gz URL and SHA-256.
func (t Toolchains) rubySources(ctx context.Context) (map[string]rubySource, error) {
	index := t.RubyIndex
	if index == "" {
		index = "https://cache.ruby-lang.org/pub/ruby/index.txt"
	}
	body, err := t.get(ctx, index, 16<<20)
	if err != nil {
		return nil, fmt.Errorf("release: Ruby release index: %w", err)
	}
	sources := map[string]rubySource{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 4 || !strings.HasSuffix(fields[1], ".tar.gz") {
			continue
		}
		version, ok := strings.CutPrefix(fields[0], "ruby-")
		if ok && exactVersion.MatchString(version) && sha256Pattern.MatchString(fields[3]) {
			sources[version] = rubySource{url: fields[1], sum: fields[3]}
		}
	}
	if len(sources) == 0 {
		return nil, errors.New("release: the Ruby release index lists no source tarballs")
	}
	return sources, nil
}

// osRelease reads the distribution ID and VERSION_ID.
func (t Toolchains) osRelease() (id, version string) {
	path := t.OSRelease
	if path == "" {
		path = "/etc/os-release"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch key {
		case "ID":
			id = value
		case "VERSION_ID":
			version = value
		}
	}
	return id, version
}

// rubyBuilderAsset finds the ruby/ruby-builder archive for one version and
// platform: release ruby-<version> (asset ruby-<v>-<platform>-<arch>.tar.gz),
// then the older shared release toolcache (ruby-<v>-<platform>.tar.gz for
// x64). Only assets with a published SHA-256 digest are used.
func (t Toolchains) rubyBuilderAsset(ctx context.Context, version, platform, arch string) (string, string, error) {
	names := []string{fmt.Sprintf("ruby-%s-%s-%s.tar.gz", version, platform, arch)}
	if arch == "x64" {
		names = append(names, fmt.Sprintf("ruby-%s-%s.tar.gz", version, platform))
	}
	reason := fmt.Sprintf("ruby/ruby-builder publishes no Ruby %s for %s %s", version, platform, arch)
	for _, tag := range []string{"ruby-" + version, "toolcache"} {
		var release struct {
			Assets []struct {
				Name   string `json:"name"`
				URL    string `json:"browser_download_url"`
				Digest string `json:"digest"`
			} `json:"assets"`
		}
		if err := t.getJSON(ctx, t.rubyBuilder()+"/releases/tags/"+tag, &release); err != nil {
			continue
		}
		for _, asset := range release.Assets {
			if !slices.Contains(names, asset.Name) {
				continue
			}
			sum, ok := strings.CutPrefix(asset.Digest, "sha256:")
			if !ok || !sha256Pattern.MatchString(sum) {
				reason = fmt.Sprintf("ruby/ruby-builder publishes %s without a SHA-256 digest", asset.Name)
				continue
			}
			if !strings.HasPrefix(asset.URL, "https://") && !strings.HasPrefix(asset.URL, "http://") {
				continue
			}
			return asset.URL, sum, nil
		}
	}
	return "", "", rubyPrebuiltUnavailable{reason}
}

// rubyPrebuilt installs a verified ruby/ruby-builder binary. It returns
// rubyPrebuiltUnavailable when none fits the host, and any other error (a
// checksum mismatch, a full disk) as a failure.
func (t Toolchains) rubyPrebuilt(ctx context.Context, version string) error {
	if t.RubyBuilder == "-" {
		return rubyPrebuiltUnavailable{"prebuilt Rubies are disabled"}
	}
	goos, _ := t.platform()
	prefix := t.RubyPrefixLink(version)
	if goos != "linux" || prefix == "" {
		return rubyPrebuiltUnavailable{"no prebuilt Rubies for this platform"}
	}
	distro, release := t.osRelease()
	if distro != "ubuntu" || release == "" {
		return rubyPrebuiltUnavailable{"ruby/ruby-builder builds for Ubuntu only"}
	}
	base := filepath.Join(t.Dir, "ruby")
	if info, err := os.Lstat(prefix); err == nil {
		target, _ := os.Readlink(prefix)
		if info.Mode()&os.ModeSymlink == 0 || !strings.HasPrefix(target, base+string(filepath.Separator)) {
			return rubyPrebuiltUnavailable{prefix + " is in use by another installation"}
		}
	}
	url, sum, err := t.rubyBuilderAsset(ctx, version, "ubuntu-"+release, filepath.Base(prefix))
	if err != nil {
		return err
	}
	t.logf("Ruby %s: downloading prebuilt %s", version, filepath.Base(url))
	archive, err := t.download(ctx, base, url, sum)
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	staging, err := t.unpackStaging(base, archive, url)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if got := builtPrefix(staging); got != prefix {
		return rubyPrebuiltUnavailable{fmt.Sprintf("the build expects prefix %q, not %s", got, prefix)}
	}
	if err := pointLink(prefix, staging); err != nil {
		return err
	}
	if err := smokeRuby(ctx, staging, version); err != nil {
		os.Remove(prefix)
		return rubyPrebuiltUnavailable{"it does not run on this host: " + err.Error()}
	}
	target := filepath.Join(base, version)
	if err := publishStaging(staging, target); err != nil {
		os.Remove(prefix)
		return err
	}
	return pointLink(prefix, target)
}

// builtPrefix returns the installation prefix a Ruby tree was configured
// with, from its rbconfig.rb.
func builtPrefix(root string) string {
	matches, _ := filepath.Glob(filepath.Join(root, "lib", "ruby", "*", "*", "rbconfig.rb"))
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if match := rbconfigPrefix.FindSubmatch(raw); match != nil {
			return string(match[1])
		}
	}
	return ""
}

// pointLink makes link a symbolic link to target, replacing an earlier one
// atomically.
func pointLink(link, target string) error {
	if err := MkdirAllReadable(filepath.Dir(link)); err != nil {
		return err
	}
	next := link + ".lidza-next"
	_ = os.Remove(next)
	if err := os.Symlink(target, next); err != nil {
		return err
	}
	if err := os.Rename(next, link); err != nil {
		os.Remove(next)
		return err
	}
	return nil
}

// smokeRuby runs the installed interpreter once and loads the extensions
// applications depend on; a binary that misses a system library fails here.
func smokeRuby(ctx context.Context, root, version string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(root, "bin", "ruby"), "-e",
		`require "rbconfig"; require "openssl"; require "psych"; require "zlib"; print RUBY_VERSION`)
	cmd.Env = []string{"PATH=" + filepath.Join(root, "bin") + ":/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, tail(stderr.String(), 400))
	}
	if got := strings.TrimSpace(out.String()); got != version {
		return fmt.Errorf("it reports Ruby %q", got)
	}
	return nil
}

func tail(text string, n int) string {
	text = strings.TrimSpace(text)
	if len(text) > n {
		text = text[len(text)-n:]
	}
	return text
}

// rubyFromSource builds and installs Ruby from its verified source tarball.
func (t Toolchains) rubyFromSource(ctx context.Context, version string, source rubySource) error {
	base := filepath.Join(t.Dir, "ruby")
	t.logf("Ruby %s: downloading %s", version, filepath.Base(source.url))
	archive, err := t.download(ctx, base, source.url, source.sum)
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	spool, err := os.MkdirTemp(base, ".build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(spool)
	src := filepath.Join(spool, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		return err
	}
	if err := untarStripped(archive, src); err != nil {
		return fmt.Errorf("release: unpack %s: %w", source.url, err)
	}
	target := filepath.Join(base, version)
	dest := filepath.Join(spool, "dest")
	jobs := t.BuildJobs
	if jobs <= 0 {
		jobs = runtime.NumCPU()
	}
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + spool, "LANG=C.UTF-8", "TMPDIR=" + spool}
	steps := []struct {
		label string
		cmd   ToolchainCommand
	}{
		{"configuring", ToolchainCommand{Name: "ruby configure", Args: []string{"./configure", "--prefix=" + target, "--disable-install-doc", "--enable-shared"}}},
		{fmt.Sprintf("compiling (make -j %d)", jobs), ToolchainCommand{Name: "ruby make", Args: []string{"make", "-j" + strconv.Itoa(jobs)}}},
		{"installing", ToolchainCommand{Name: "ruby make install", Args: []string{"make", "install", "DESTDIR=" + dest}}},
	}
	for _, step := range steps {
		step.cmd.Dir, step.cmd.Env = src, env
		t.logf("Ruby %s: %s", version, step.label)
		if err := t.runCommand(ctx, step.cmd); err != nil {
			return fmt.Errorf("release: Ruby %s %s failed: %w", version, strings.TrimPrefix(step.cmd.Name, "ruby "), err)
		}
	}
	staged := filepath.Join(dest, target)
	if info, err := os.Stat(filepath.Join(staged, "bin", "ruby")); err != nil || info.IsDir() {
		return fmt.Errorf("release: the Ruby %s build installed no bin/ruby", version)
	}
	if err := readableTree(staged); err != nil {
		return err
	}
	return publishStaging(staged, target)
}

// runCommand runs one build command through Run, or directly.
func (t Toolchains) runCommand(ctx context.Context, c ToolchainCommand) error {
	if t.Run != nil {
		return t.Run(ctx, c)
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", `umask 022; exec "$@"`, "build"}, c.Args...)...)
	cmd.Dir, cmd.Env = c.Dir, c.Env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, tail(out.String(), 2000))
	}
	return nil
}
