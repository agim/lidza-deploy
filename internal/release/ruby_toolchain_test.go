package release

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestRubyRequirementReadsEachPinFile(t *testing.T) {
	for name, c := range map[string]struct {
		files         map[string]string
		version, file string
	}{
		"ruby-version":         {map[string]string{".ruby-version": "ruby-3.4.4\n", "Gemfile": "ruby \"3.2.2\"\n"}, "3.4.4", ".ruby-version"},
		"ruby-version minor":   {map[string]string{".ruby-version": "3.4\n"}, "3.4", ".ruby-version"},
		"patch level":          {map[string]string{".ruby-version": "2.7.8-p225\n"}, "2.7.8", ".ruby-version"},
		"tool-versions":        {map[string]string{".tool-versions": "nodejs 20.11.0\nruby 3.3.5 system # fallback\n"}, "3.3.5", ".tool-versions"},
		"gemfile":              {map[string]string{"Gemfile": "source \"https://rubygems.org\"\nruby '3.2.2'\ngem \"rails\"\n"}, "3.2.2", "Gemfile"},
		"gemfile parentheses":  {map[string]string{"Gemfile": "ruby(\"3.3.1\")\n"}, "3.3.1", "Gemfile"},
		"gemfile file pointer": {map[string]string{"Gemfile": "ruby file: \".ruby-version\"\n"}, "", ""},
		"gemfile range":        {map[string]string{"Gemfile": "ruby \"~> 3.4.0\"\n"}, "", ""},
		"jruby":                {map[string]string{".ruby-version": "jruby-9.4.8.0\n"}, "", ""},
		"nothing":              {map[string]string{"Gemfile": "gem \"rails\"\n"}, "", ""},
	} {
		version, file := RubyRequirement(writeTree(t, c.files))
		if version != c.version || file != c.file {
			t.Errorf("%s: %q from %q, want %q from %q", name, version, file, c.version, c.file)
		}
	}
}

// rubyFixture serves a fake cache.ruby-lang.org index, ruby/ruby-builder
// API and downloads.
type rubyFixture struct {
	server   *httptest.Server
	tools    Toolchains
	log      *bytes.Buffer
	prebuilt []byte
	source   []byte
	digest   string // published digest of the prebuilt asset
	tag      string // release tag that carries the prebuilt asset
	asset    string
	mu       sync.Mutex
	requests []string
	packages [][]string
}

func newRubyFixture(t *testing.T, distro string) *rubyFixture {
	t.Helper()
	f := &rubyFixture{log: &bytes.Buffer{}, tag: "ruby-3.4.4", asset: "ruby-3.4.4-ubuntu-24.04-x64.tar.gz"}
	root := t.TempDir()
	prefixRoot := filepath.Join(root, "opt", "hostedtoolcache", "Ruby")
	f.prebuilt = tarball(t, []tarEntry{
		{name: "x64/", kind: tar.TypeDir, mode: 0o755},
		{name: "x64/bin/ruby", body: "#!/bin/sh\nprintf 3.4.4\n", mode: 0o755},
		{name: "x64/lib/ruby/3.4.0/x86_64-linux/rbconfig.rb", mode: 0o644,
			body: fmt.Sprintf("  CONFIG[\"prefix\"] = (TOPDIR || DESTDIR + \"%s\")\n", filepath.Join(prefixRoot, "3.4.4", "x64"))},
	})
	f.source = tarball(t, []tarEntry{
		{name: "ruby-3.4.4/configure", mode: 0o755, body: "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in --prefix=*) echo \"${a#--prefix=}\" > .prefix;; esac; done\necho \"$@\" > .configured\n"},
		{name: "ruby-3.4.4/README.md", body: "ruby\n", mode: 0o644},
	})
	f.digest = "sha256:" + digest(f.prebuilt)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.Path)
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/pub/ruby/index.txt":
			fmt.Fprintf(w, "name\turl\tsha1\tsha256\tsha512\n")
			fmt.Fprintf(w, "ruby-3.4.4\t%s/pub/ruby/3.4/ruby-3.4.4.tar.gz\tx\t%s\ty\n", f.server.URL, digest(f.source))
			fmt.Fprintf(w, "ruby-3.4.4\t%s/pub/ruby/3.4/ruby-3.4.4.zip\tx\t%s\ty\n", f.server.URL, strings.Repeat("2", 64))
			fmt.Fprintf(w, "ruby-3.4.3\t%s/pub/ruby/3.4/ruby-3.4.3.tar.gz\tx\t%s\ty\n", f.server.URL, strings.Repeat("3", 64))
			fmt.Fprintf(w, "ruby-3.5.0-preview1\t%s/pub/ruby/3.5/ruby-3.5.0-preview1.tar.gz\tx\t%s\ty\n", f.server.URL, strings.Repeat("4", 64))
		case r.URL.Path == "/pub/ruby/3.4/ruby-3.4.4.tar.gz":
			w.Write(f.source)
		case r.URL.Path == "/api/releases/tags/"+f.tag:
			json.NewEncoder(w).Encode(map[string]any{"assets": []map[string]string{
				{"name": "ruby-3.4.4-darwin-arm64.tar.gz", "browser_download_url": f.server.URL + "/dl/darwin", "digest": "sha256:" + strings.Repeat("5", 64)},
				{"name": f.asset, "browser_download_url": f.server.URL + "/dl/" + f.asset, "digest": f.digest},
			}})
		case r.URL.Path == "/dl/"+f.asset:
			w.Write(f.prebuilt)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	osRelease := filepath.Join(root, "os-release")
	os.WriteFile(osRelease, []byte(distro), 0o644)
	f.tools = Toolchains{Dir: filepath.Join(root, "lib", "lidza"), OS: "linux", Arch: "amd64", Log: f.log,
		RubyIndex: f.server.URL + "/pub/ruby/index.txt", RubyBuilder: f.server.URL + "/api", RubyPrefix: prefixRoot, OSRelease: osRelease,
		InstallPackages: func(_ context.Context, packages []string) error {
			f.mu.Lock()
			f.packages = append(f.packages, packages)
			f.mu.Unlock()
			return nil
		}}
	return f
}

func (f *rubyFixture) requested(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r == path {
			n++
		}
	}
	return n
}

const ubuntu2404 = "PRETTY_NAME=\"Ubuntu 24.04.3 LTS\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n"

func TestRubyInstallsVerifiedPrebuiltBinary(t *testing.T) {
	f := newRubyFixture(t, ubuntu2404)
	root, err := f.tools.Ruby(context.Background(), "ruby-3.4.4")
	if err != nil {
		t.Fatalf("%v\n%s", err, f.log)
	}
	if root != filepath.Join(f.tools.Dir, "ruby", "3.4.4") {
		t.Fatalf("root = %s", root)
	}
	link := f.tools.RubyPrefixLink("3.4.4")
	if target, err := os.Readlink(link); err != nil || target != root {
		t.Fatalf("the build's prefix %s must link to the installation: %q %v", link, target, err)
	}
	if info, err := os.Stat(filepath.Join(root, "bin", "ruby")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("ruby binary: %v %v", info, err)
	}
	if !strings.Contains(f.log.String(), "Ruby 3.4.4: installed (prebuilt, ") || f.requested("/pub/ruby/3.4/ruby-3.4.4.tar.gz") != 0 {
		t.Fatalf("log:\n%s", f.log)
	}
	if len(f.packages) != 1 || !slices.Equal(f.packages[0], RubyBuildPackages) {
		t.Fatalf("build packages: %v", f.packages)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.tools.Dir, "ruby")); len(entries) != 1 {
		t.Fatalf("spool files remain: %v", entries)
	}
	f.log.Reset()
	before := len(f.requests)
	if again, err := f.tools.Ruby(context.Background(), "3.4"); err != nil || again != root || len(f.requests) != before {
		t.Fatalf("an installed version must be reused without network: %s %v", again, err)
	}
	if !strings.Contains(f.log.String(), "Ruby 3.4.4: already installed") {
		t.Fatalf("log:\n%s", f.log)
	}
	if f.tools.Installed("ruby", "3.4") != root {
		t.Fatal("Installed must report Ruby installations")
	}
}

func TestRubyFindsPrebuiltInTheSharedToolcacheRelease(t *testing.T) {
	f := newRubyFixture(t, ubuntu2404)
	f.tag, f.asset = "toolcache", "ruby-3.4.4-ubuntu-24.04.tar.gz"
	if _, err := f.tools.Ruby(context.Background(), "3.4.4"); err != nil || !strings.Contains(f.log.String(), "(prebuilt, ") {
		t.Fatalf("%v\n%s", err, f.log)
	}
}

func TestRubyRefusesPrebuiltWithWrongChecksum(t *testing.T) {
	f := newRubyFixture(t, ubuntu2404)
	f.digest = "sha256:" + strings.Repeat("0", 64)
	_, err := f.tools.Ruby(context.Background(), "3.4.4")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.tools.Dir, "ruby")); len(entries) != 0 {
		t.Fatalf("nothing may remain after a refused download: %v", entries)
	}
	if _, err := os.Lstat(f.tools.RubyPrefixLink("3.4.4")); !os.IsNotExist(err) {
		t.Fatal("a refused build must not be linked")
	}
}

// fakeMake runs build commands with a stand-in make that installs a ruby
// script under DESTDIR + the configured prefix.
func fakeMake(t *testing.T, calls *[]string) func(context.Context, ToolchainCommand) error {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "make"), []byte("#!/bin/sh\nset -eu\nif [ \"${1:-}\" = install ]; then d=\"${2#DESTDIR=}$(cat .prefix)\"; mkdir -p \"$d/bin\"; printf '#!/bin/sh\\nprintf 3.4.4\\n' > \"$d/bin/ruby\"; chmod 700 \"$d/bin/ruby\"; fi\n"), 0o755)
	return func(ctx context.Context, c ToolchainCommand) error {
		*calls = append(*calls, c.Name+": "+strings.Join(c.Args, " "))
		program := c.Args[0]
		if program == "make" {
			program = filepath.Join(bin, "make") // never the host's make
		}
		cmd := exec.CommandContext(ctx, program, c.Args[1:]...)
		cmd.Dir = c.Dir
		cmd.Env = append(slices.DeleteFunc(slices.Clone(c.Env), func(e string) bool { return strings.HasPrefix(e, "PATH=") }), "PATH="+bin+":/usr/bin:/bin")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}
}

func TestRubyBuildsFromVerifiedSourceWhenNoPrebuiltFits(t *testing.T) {
	f := newRubyFixture(t, "ID=debian\nVERSION_ID=\"12\"\n")
	var calls []string
	f.tools.Run, f.tools.BuildJobs = fakeMake(t, &calls), 3
	root, err := f.tools.Ruby(context.Background(), "3.4.4")
	if err != nil {
		t.Fatalf("%v\n%s", err, f.log)
	}
	want := []string{
		"ruby configure: ./configure --prefix=" + root + " --disable-install-doc --enable-shared",
		"ruby make: make -j3",
	}
	if len(calls) != 3 || !slices.Equal(calls[:2], want) || !strings.HasPrefix(calls[2], "ruby make install: make install DESTDIR=") {
		t.Fatalf("build commands: %q", calls)
	}
	if info, err := os.Stat(filepath.Join(root, "bin", "ruby")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed ruby must be readable by applications: %v %v", info, err)
	}
	for _, line := range []string{"builds for Ubuntu only", "Ruby 3.4.4: compiling (make -j 3)", "Ruby 3.4.4: installed (built from source, "} {
		if !strings.Contains(f.log.String(), line) {
			t.Fatalf("log lacks %q:\n%s", line, f.log)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(f.tools.Dir, "ruby")); len(entries) != 1 {
		t.Fatalf("build spool remains: %v", entries)
	}
}

func TestRubyBuildsFromSourceWhenThePrefixBelongsToSomethingElse(t *testing.T) {
	f := newRubyFixture(t, ubuntu2404)
	var calls []string
	f.tools.Run = fakeMake(t, &calls)
	occupied := f.tools.RubyPrefixLink("3.4.4")
	os.MkdirAll(occupied, 0o755) // a runner's own tool cache
	if _, err := f.tools.Ruby(context.Background(), "3.4.4"); err != nil {
		t.Fatalf("%v\n%s", err, f.log)
	}
	if !strings.Contains(f.log.String(), "is in use by another installation") || len(calls) != 3 {
		t.Fatalf("log:\n%s", f.log)
	}
	if info, err := os.Lstat(occupied); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("an existing directory at the prefix must stay untouched")
	}
}

func TestRubyRejectsUnpublishedAndForeignVersions(t *testing.T) {
	f := newRubyFixture(t, ubuntu2404)
	if _, err := f.tools.Ruby(context.Background(), "jruby-9.4.8.0"); err == nil || !strings.Contains(err.Error(), "unsupported Ruby version") {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.tools.Ruby(context.Background(), "3.9.9"); err == nil || !strings.Contains(err.Error(), "not a published release") {
		t.Fatalf("err = %v", err)
	}
	f.tools.Run = fakeMake(t, new([]string))
	if root, err := f.tools.Ruby(context.Background(), "3.4"); err != nil || filepath.Base(root) != "3.4.4" {
		t.Fatalf("a minor pin selects the newest published patch release: %s %v", root, err)
	}
}

func TestToolchainTryLockSeesAnInstallInProgress(t *testing.T) {
	tools := Toolchains{Dir: t.TempDir()}
	unlock, err := tools.lock()
	if err != nil {
		t.Fatal(err)
	}
	if release, held, err := tools.TryLock(); err != nil || !held || release != nil {
		t.Fatalf("TryLock during an install: held %v err %v", held, err)
	}
	unlock()
	release, held, err := tools.TryLock()
	if err != nil || held {
		t.Fatalf("TryLock after the install: held %v err %v", held, err)
	}
	release()
}
