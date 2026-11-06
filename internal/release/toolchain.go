package release

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Toolchains installs official Go, Node.js and Ruby releases into a
// root-owned directory that applications read but cannot change:
//
//	<Dir>/go/<version>     for example /usr/local/lib/lidza/go/1.24.3
//	<Dir>/node/<version>   for example /usr/local/lib/lidza/node/22.11.0
//	<Dir>/ruby/<version>   for example /usr/local/lib/lidza/ruby/3.4.4
//
// Every archive is checked against the checksum its publisher lists
// (go.dev's release index, nodejs.org's SHASUMS256.txt, the GitHub release
// asset digest of ruby/ruby-builder, cache.ruby-lang.org's index) before it
// is unpacked. Installed versions are reused; an install is atomic.
type Toolchains struct {
	Dir    string
	Client *http.Client // default: a client with a 10-minute timeout
	// IPv4Client retries a failed fetch over IPv4; default: an IPv4-only client.
	IPv4Client *http.Client
	// RustDownload is the rustup distribution base URL; default static.rust-lang.org.
	RustDownload string
	// GoIndex lists Go releases with their files and checksums.
	GoIndex string // default https://go.dev/dl/?mode=json&include=all
	// GoDownload is the base URL of Go archives.
	GoDownload string // default https://go.dev/dl/
	// NodeDist is the base URL of Node.js releases (index.json, v<version>/).
	NodeDist string // default https://nodejs.org/dist/
	// HostGo is a Go installation already present on the host that is used
	// when its version matches; "" disables it.
	HostGo   string
	OS, Arch string // default runtime.GOOS, runtime.GOARCH
	Log      io.Writer

	// Ruby sources (ruby_toolchain.go).
	//
	// RubyBuilder is the GitHub API base of the prebuilt Ruby publisher;
	// default https://api.github.com/repos/ruby/ruby-builder, "-" disables
	// prebuilt Rubies.
	RubyBuilder string
	// RubyPrefix is where ruby/ruby-builder binaries expect to live
	// (<RubyPrefix>/<version>/<x64|arm64>); default /opt/hostedtoolcache/Ruby.
	RubyPrefix string
	// RubyIndex lists Ruby source tarballs with their SHA-256; default
	// https://cache.ruby-lang.org/pub/ruby/index.txt.
	RubyIndex string
	// OSRelease is the os-release file that names the host's distribution;
	// default /etc/os-release.
	OSRelease string
	// Run runs one source-build command (configure, make) as root; nil runs
	// it directly. Agents pass a runner that applies their build limits.
	Run func(ctx context.Context, c ToolchainCommand) error
	// BuildJobs is make's -j for source builds; default the CPU count.
	BuildJobs int
	// InstallPackages installs missing distribution packages that building
	// Ruby (and gems with native extensions) needs; nil installs nothing.
	InstallPackages func(ctx context.Context, packages []string) error
}

const maxToolchainBytes = 2 << 30

var (
	goVersionPattern   = regexp.MustCompile(`^[1-9][0-9]*\.[0-9]+(\.[0-9]+)?$`)
	nodeVersionPattern = regexp.MustCompile(`^v?([0-9]+)(\.[0-9]+)?(\.[0-9]+)?$`)
	exactVersion       = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

func (t Toolchains) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// ipv4Client fetches over IPv4 only. Some download CDNs answer a host's IPv6
// address with errors for files their IPv4 edges serve, so a failed fetch is
// retried once this way.
func (t Toolchains) ipv4Client() *http.Client {
	if t.IPv4Client != nil {
		return t.IPv4Client
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", address)
	}
	return &http.Client{Timeout: 10 * time.Minute, Transport: transport}
}

// fetch GETs url, retrying over IPv4 when the first attempt fails or does
// not answer 200. The caller closes the returned body.
func (t Toolchains) fetch(ctx context.Context, url string) (*http.Response, error) {
	var first error
	for attempt, client := range []*http.Client{t.client(), t.ipv4Client()} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		response, err := client.Do(request)
		if err == nil && response.StatusCode == http.StatusOK {
			return response, nil
		}
		if err == nil {
			response.Body.Close()
			err = fmt.Errorf("HTTP %d", response.StatusCode)
		}
		if attempt == 0 {
			first = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		return nil, fmt.Errorf("%w (over IPv4: %v)", first, err)
	}
	return nil, first
}

func (t Toolchains) platform() (string, string) {
	goos, arch := t.OS, t.Arch
	if goos == "" {
		goos = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	return goos, arch
}

func (t Toolchains) logf(format string, args ...any) {
	if t.Log != nil {
		fmt.Fprintf(t.Log, "==> "+format+"\n", args...)
	}
}

func versionKey(version string) []int {
	var key []int
	for _, part := range strings.Split(strings.TrimPrefix(version, "v"), ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		key = append(key, n)
	}
	return key
}

func newestVersion(versions []string) string {
	slices.SortFunc(versions, func(a, b string) int { return slices.Compare(versionKey(a), versionKey(b)) })
	if len(versions) == 0 {
		return ""
	}
	return versions[len(versions)-1]
}

// matchesVersion reports whether version (exact, no "v") satisfies want,
// which is exact or a major[.minor] prefix.
func matchesVersion(version, want string) bool {
	want = strings.TrimPrefix(want, "v")
	return version == want || strings.HasPrefix(version, want+".")
}

// installed returns the newest installed version under kind matching want.
func (t Toolchains) installed(kind, want string) string {
	entries, err := os.ReadDir(filepath.Join(t.Dir, kind))
	if err != nil {
		return ""
	}
	var matches []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && exactVersion.MatchString(name) && matchesVersion(name, want) {
			matches = append(matches, name)
		}
	}
	return newestVersion(matches)
}

// Installed returns the root of the newest installed kind ("go" or "node")
// matching want (exact or a major[.minor] prefix; "" matches any), or "".
func (t Toolchains) Installed(kind, want string) string {
	if kind != "go" && kind != "node" && kind != "ruby" {
		return ""
	}
	want = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(want, "go"), "v"), "ruby-")
	if want == "" {
		entries, _ := os.ReadDir(filepath.Join(t.Dir, kind))
		var all []string
		for _, entry := range entries {
			if entry.IsDir() && exactVersion.MatchString(entry.Name()) {
				all = append(all, entry.Name())
			}
		}
		if version := newestVersion(all); version != "" {
			return filepath.Join(t.Dir, kind, version)
		}
		return ""
	}
	if version := t.installed(kind, want); version != "" {
		return filepath.Join(t.Dir, kind, version)
	}
	return ""
}

// lock serializes installs across deploys of different applications.
func (t Toolchains) lock() (func(), error) {
	unlock, _, err := t.flock(false)
	return unlock, err
}

// TryLock takes the install lock without waiting, so a cleanup never
// removes a version while an install runs. held reports another holder.
func (t Toolchains) TryLock() (unlock func(), held bool, err error) {
	return t.flock(true)
}

func (t Toolchains) flock(nonBlocking bool) (func(), bool, error) {
	if err := MkdirAllReadable(t.Dir); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(filepath.Join(t.Dir, ".install.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	how := syscall.LOCK_EX
	if nonBlocking {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return func() { file.Close() }, false, nil
}

// Go returns the root of a Go installation for want (a go.mod version such
// as 1.24 or 1.24.3), installing it when necessary. A minor version
// selects its newest installed or published patch release.
func (t Toolchains) Go(ctx context.Context, want string) (string, error) {
	want = strings.TrimPrefix(strings.TrimSpace(want), "go")
	if want == "" {
		want = "1"
	} else if !goVersionPattern.MatchString(want) {
		return "", fmt.Errorf("release: unsupported Go version %q", want)
	}
	if version := t.installed("go", want); version != "" {
		return filepath.Join(t.Dir, "go", version), nil
	}
	if root := t.hostGo(want); root != "" {
		return root, nil
	}
	unlock, err := t.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if version := t.installed("go", want); version != "" {
		return filepath.Join(t.Dir, "go", version), nil
	}
	goos, arch := t.platform()
	index := t.GoIndex
	if index == "" {
		index = "https://go.dev/dl/?mode=json&include=all"
	}
	var releases []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
		Files   []struct {
			Filename string `json:"filename"`
			OS       string `json:"os"`
			Arch     string `json:"arch"`
			SHA256   string `json:"sha256"`
			Kind     string `json:"kind"`
		} `json:"files"`
	}
	if err := t.getJSON(ctx, index, &releases); err != nil {
		return "", fmt.Errorf("release: Go release index: %w", err)
	}
	type candidate struct{ version, file, sum string }
	found := map[string]candidate{}
	var versions []string
	for _, release := range releases {
		version := strings.TrimPrefix(release.Version, "go")
		if !release.Stable || !exactVersion.MatchString(version) && !regexp.MustCompile(`^[0-9]+\.[0-9]+$`).MatchString(version) {
			continue
		}
		// Releases before 1.21 named their first release 1.x, not 1.x.0.
		exact := version
		if strings.Count(exact, ".") == 1 {
			exact += ".0"
		}
		if !matchesVersion(exact, want) && version != want {
			continue
		}
		for _, file := range release.Files {
			if file.OS == goos && file.Arch == arch && file.Kind == "archive" && strings.HasSuffix(file.Filename, ".tar.gz") {
				found[exact] = candidate{version: exact, file: file.Filename, sum: file.SHA256}
				versions = append(versions, exact)
			}
		}
	}
	chosen, ok := found[newestVersion(versions)]
	if !ok {
		return "", fmt.Errorf("release: no published Go %s for %s/%s", want, goos, arch)
	}
	base := t.GoDownload
	if base == "" {
		base = "https://go.dev/dl/"
	}
	t.logf("installing Go %s", chosen.version)
	if err := t.install(ctx, "go", chosen.version, strings.TrimSuffix(base, "/")+"/"+chosen.file, chosen.sum); err != nil {
		return "", err
	}
	return filepath.Join(t.Dir, "go", chosen.version), nil
}

// hostGo returns HostGo when its VERSION file names a release matching want.
func (t Toolchains) hostGo(want string) string {
	if t.HostGo == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(t.HostGo, "VERSION"))
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	version := strings.TrimPrefix(strings.TrimSpace(line), "go")
	if exactVersion.MatchString(version) && matchesVersion(version, want) {
		if _, err := os.Stat(filepath.Join(t.HostGo, "bin", "go")); err == nil {
			return t.HostGo
		}
	}
	return ""
}

// Node returns the root of a Node.js installation for want: an exact
// version, a major[.minor] prefix, "lts/*", "lts/<codename>", or "" for the
// newest long-term-support release. It installs the release when needed.
func (t Toolchains) Node(ctx context.Context, want string) (string, error) {
	want = strings.TrimSpace(want)
	lts := ""
	switch lower := strings.ToLower(want); {
	case want == "" || lower == "lts" || lower == "lts/*":
		lts = "*"
	case strings.HasPrefix(lower, "lts/"):
		lts = strings.TrimPrefix(lower, "lts/")
	case lower == "node" || lower == "latest" || lower == "current":
		want = "*"
	case nodeVersionPattern.MatchString(want):
		want = strings.TrimPrefix(want, "v")
		if version := t.installed("node", want); version != "" {
			return filepath.Join(t.Dir, "node", version), nil
		}
	default:
		return "", fmt.Errorf("release: unsupported Node.js version %q", want)
	}
	unlock, err := t.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if lts == "" && want != "*" {
		if version := t.installed("node", want); version != "" {
			return filepath.Join(t.Dir, "node", version), nil
		}
	}
	dist := strings.TrimSuffix(t.NodeDist, "/")
	if dist == "" {
		dist = "https://nodejs.org/dist"
	}
	var releases []struct {
		Version string `json:"version"`
		LTS     any    `json:"lts"` // false or the codename
	}
	if err := t.getJSON(ctx, dist+"/index.json", &releases); err != nil {
		return "", fmt.Errorf("release: Node.js release index: %w", err)
	}
	var versions []string
	for _, release := range releases {
		version := strings.TrimPrefix(release.Version, "v")
		if !exactVersion.MatchString(version) {
			continue
		}
		codename, _ := release.LTS.(string)
		switch {
		case lts == "*" && codename != "":
		case lts != "" && strings.EqualFold(codename, lts):
		case lts == "" && (want == "*" || matchesVersion(version, want)):
		default:
			continue
		}
		versions = append(versions, version)
	}
	version := newestVersion(versions)
	if version == "" {
		return "", fmt.Errorf("release: no published Node.js release matches %q", want)
	}
	root := filepath.Join(t.Dir, "node", version)
	if _, err := os.Stat(filepath.Join(root, "bin", "node")); err == nil {
		return root, nil
	}
	goos, arch := t.platform()
	switch arch {
	case "amd64":
		arch = "x64"
	case "386":
		arch = "x86"
	}
	file := fmt.Sprintf("node-v%s-%s-%s.tar.gz", version, goos, arch)
	sums, err := t.get(ctx, dist+"/v"+version+"/SHASUMS256.txt", 1<<20)
	if err != nil {
		return "", fmt.Errorf("release: Node.js checksums: %w", err)
	}
	sum := ""
	scanner := bufio.NewScanner(strings.NewReader(string(sums)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[1] == file {
			sum = fields[0]
		}
	}
	if sum == "" {
		return "", fmt.Errorf("release: Node.js %s publishes no %s", version, file)
	}
	t.logf("installing Node.js %s", version)
	if err := t.install(ctx, "node", version, dist+"/v"+version+"/"+file, sum); err != nil {
		return "", err
	}
	return root, nil
}

func (t Toolchains) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	response, err := t.fetch(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("GET %s: response too large", url)
	}
	return body, nil
}

func (t Toolchains) getJSON(ctx context.Context, url string, v any) error {
	body, err := t.get(ctx, url, 64<<20)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// install downloads url, verifies its SHA-256 and unpacks it, without its
// top-level directory, into <Dir>/<kind>/<version>.
func (t Toolchains) install(ctx context.Context, kind, version, url, sum string) error {
	if !exactVersion.MatchString(version) {
		return errors.New("release: toolchain metadata is invalid")
	}
	base := filepath.Join(t.Dir, kind)
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
	return publishStaging(staging, filepath.Join(base, version))
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// download fetches url into a spool file under base and verifies its
// SHA-256 against sum. The caller closes and removes the returned file;
// nothing is left behind when it fails.
func (t Toolchains) download(ctx context.Context, base, url, sum string) (*os.File, error) {
	if !sha256Pattern.MatchString(sum) {
		return nil, errors.New("release: toolchain metadata is invalid")
	}
	if err := MkdirAllReadable(base); err != nil {
		return nil, err
	}
	archive, err := os.CreateTemp(base, ".download-")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) {
		archive.Close()
		os.Remove(archive.Name())
		return nil, err
	}
	response, err := t.fetch(ctx, url)
	if err != nil {
		return fail(fmt.Errorf("release: download %s: %w", url, err))
	}
	defer response.Body.Close()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(response.Body, maxToolchainBytes+1))
	if err != nil {
		return fail(fmt.Errorf("release: download %s: %w", url, err))
	}
	if written > maxToolchainBytes {
		return fail(fmt.Errorf("release: %s is too large", url))
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != sum {
		return fail(fmt.Errorf("release: %s checksum mismatch (published %s, downloaded %s)", url, sum, got))
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return archive, nil
}

// unpackStaging unpacks a verified archive into a new staging directory
// under base with readable, unwritable permissions. The caller removes it.
func (t Toolchains) unpackStaging(base string, archive io.Reader, url string) (string, error) {
	staging, err := os.MkdirTemp(base, ".unpack-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		os.RemoveAll(staging)
		return "", err
	}
	if err := untarStripped(archive, staging); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("release: unpack %s: %w", url, err)
	}
	if err := readableTree(staging); err != nil {
		os.RemoveAll(staging)
		return "", err
	}
	return staging, nil
}

// readableTree makes a toolchain tree world-readable and unwritable except
// by its owner. Applications read and run toolchains as other users:
// directories are world-readable whatever the installer's umask.
func readableTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.Type()&os.ModeSymlink != 0 {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o100 != 0 {
			mode = 0o755
		}
		return os.Chmod(path, mode)
	})
}

// publishStaging moves a finished staging tree to target atomically; a
// target another install published first wins.
func publishStaging(staging, target string) error {
	if err := os.Rename(staging, target); err != nil {
		if _, statErr := os.Stat(target); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

// untarStripped unpacks a gzip tarball into dest, dropping the first path
// element. Entries and links must stay inside dest; ownership comes from
// the installing user and nothing is writable except by that user.
func untarStripped(r io.Reader, dest string) error {
	compressed, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	var total int64
	inside := func(path string) bool {
		rel, err := filepath.Rel(dest, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(header.Name)
		if strings.HasPrefix(name, "/") {
			return fmt.Errorf("absolute archive entry %q", header.Name)
		}
		_, rest, _ := strings.Cut(strings.TrimPrefix(name, "./"), "/")
		if rest == "" {
			continue
		}
		path := filepath.Join(dest, filepath.FromSlash(rest))
		if !inside(path) {
			return fmt.Errorf("archive entry %q leaves the destination", header.Name)
		}
		mode := os.FileMode(header.Mode).Perm() &^ 0o022
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			total += header.Size
			if total > 4*maxToolchainBytes {
				return errors.New("archive unpacks too large")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o400)
			if err != nil {
				return err
			}
			if _, err := io.Copy(file, io.LimitReader(archive, header.Size)); err != nil {
				file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			target := header.Linkname
			if filepath.IsAbs(target) || !inside(filepath.Join(filepath.Dir(path), target)) {
				return fmt.Errorf("archive link %q leaves the destination", header.Name)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(target, path); err != nil {
				return err
			}
		case tar.TypeLink:
			_, linkRest, _ := strings.Cut(strings.TrimPrefix(filepath.ToSlash(header.Linkname), "./"), "/")
			source := filepath.Join(dest, filepath.FromSlash(linkRest))
			if linkRest == "" || !inside(source) {
				return fmt.Errorf("archive link %q leaves the destination", header.Name)
			}
			if err := os.Link(source, path); err != nil {
				return err
			}
		default:
			// Device nodes, FIFOs and other special files are never needed.
		}
	}
}

// MkdirAllReadable creates dir and its missing parents with mode 0755
// regardless of the process umask, and leaves existing directories alone.
func MkdirAllReadable(dir string) error {
	dir = filepath.Clean(dir)
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := MkdirAllReadable(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return os.Chmod(dir, 0o755)
}

// NodeRequirement returns the Node.js version a release pins in
// .node-version or .nvmrc, or "".
func NodeRequirement(dir string) string {
	for _, name := range []string{".node-version", ".nvmrc"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() > 256 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		line, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// RustTargets are the compilation targets every Rust installation carries:
// Līdza packs written in Rust build to WebAssembly.
var RustTargets = []string{"wasm32-wasip1"}

// Rust returns the root of a Rust toolchain (its bin/ holds cargo and
// rustc) with RustTargets installed. The toolchain is installed once with
// the official rustup-init, verified against its published SHA-256, under
// <Dir>/rust, readable by every account; builds call the toolchain's cargo
// directly with their own CARGO_HOME, so rustup is never invoked by them.
func (t Toolchains) Rust(ctx context.Context) (string, error) {
	unlock, err := t.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	base := filepath.Join(t.Dir, "rust")
	rustupHome, cargoHome := filepath.Join(base, "rustup"), filepath.Join(base, "cargo")
	rustup := filepath.Join(cargoHome, "bin", "rustup")
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + base, "RUSTUP_HOME=" + rustupHome, "CARGO_HOME=" + cargoHome}
	run := func(args ...string) error {
		// Installed files must be readable by application accounts, whatever
		// the agent's own umask is.
		cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", `umask 022; exec "$@"`, "rust"}, args...)...)
		cmd.Env = env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			tail := out.String()
			if len(tail) > 2000 {
				tail = tail[len(tail)-2000:]
			}
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(tail))
		}
		return nil
	}
	if _, err := os.Stat(rustup); err != nil {
		if err := MkdirAllReadable(base); err != nil {
			return "", err
		}
		goos, arch := t.platform()
		triple := map[string]string{"linux/amd64": "x86_64-unknown-linux-gnu", "linux/arm64": "aarch64-unknown-linux-gnu"}[goos+"/"+arch]
		if triple == "" {
			return "", fmt.Errorf("release: no Rust installer for %s/%s", goos, arch)
		}
		url := t.RustDownload
		if url == "" {
			url = "https://static.rust-lang.org/rustup/dist/"
		}
		url = strings.TrimSuffix(url, "/") + "/" + triple + "/rustup-init"
		sumFile, err := t.get(ctx, url+".sha256", 4<<10)
		if err != nil {
			return "", fmt.Errorf("release: Rust installer checksum: %w", err)
		}
		sum := strings.Fields(string(sumFile))
		if len(sum) == 0 || !sha256Pattern.MatchString(sum[0]) {
			return "", errors.New("release: Rust installer checksum is invalid")
		}
		installer, err := t.get(ctx, url, 64<<20)
		if err != nil {
			return "", fmt.Errorf("release: Rust installer: %w", err)
		}
		digest := sha256.Sum256(installer)
		if hex.EncodeToString(digest[:]) != sum[0] {
			return "", errors.New("release: Rust installer does not match its published SHA-256")
		}
		// rustup-init chooses what to do from its file name, so it must keep it.
		spool, err := os.MkdirTemp(base, ".installer-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(spool)
		path := filepath.Join(spool, "rustup-init")
		if err := os.WriteFile(path, installer, 0o700); err != nil {
			return "", err
		}
		t.logf("installing Rust (stable, minimal, %s)", strings.Join(RustTargets, ", "))
		args := []string{path, "-y", "--no-modify-path", "--profile", "minimal", "--default-toolchain", "stable"}
		for _, target := range RustTargets {
			args = append(args, "--target", target)
		}
		if err := run(args...); err != nil {
			return "", fmt.Errorf("release: install Rust: %w", err)
		}
	} else if err := run(append([]string{rustup, "target", "add"}, RustTargets...)...); err != nil {
		return "", fmt.Errorf("release: add Rust targets: %w", err)
	}
	toolchains, err := filepath.Glob(filepath.Join(rustupHome, "toolchains", "stable-*"))
	if err != nil || len(toolchains) != 1 {
		return "", fmt.Errorf("release: expected one stable Rust toolchain under %s", rustupHome)
	}
	if _, err := os.Stat(filepath.Join(toolchains[0], "bin", "cargo")); err != nil {
		return "", errors.New("release: the Rust toolchain has no cargo")
	}
	return toolchains[0], nil
}

// NeedsRust reports whether a release tree builds Rust code: a Cargo
// manifest at its root or in a pack.
func NeedsRust(dir string) bool {
	if exists(dir, "Cargo.toml") {
		return true
	}
	for _, pattern := range []string{"packs/*/Cargo.toml", "packs/*/rust/Cargo.toml"} {
		if matches, _ := filepath.Glob(filepath.Join(dir, pattern)); len(matches) > 0 {
			return true
		}
	}
	return false
}
