package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type tarEntry struct {
	name, body, link string
	mode             int64
	kind             byte
}

func tarball(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: entry.kind, Linkname: entry.link, Size: int64(len(entry.body))}
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if header.Typeflag != tar.TypeReg {
			header.Size = 0
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			archive.Write([]byte(entry.body))
		}
	}
	archive.Close()
	compressed.Close()
	return buffer.Bytes()
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestToolchainsInstallVerifiedGoAndReuseIt(t *testing.T) {
	archive := tarball(t, []tarEntry{{name: "go/", kind: tar.TypeDir, mode: 0o755}, {name: "go/bin/go", body: "#!/bin/sh\n", mode: 0o755}, {name: "go/VERSION", body: "go1.24.5\n", mode: 0o644}})
	requests := 0
	sum := digest(archive)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/index":
			json.NewEncoder(w).Encode([]map[string]any{
				{"version": "go1.25.0", "stable": true, "files": []map[string]string{{"filename": "go1.25.0.linux-amd64.tar.gz", "os": "linux", "arch": "amd64", "kind": "archive", "sha256": sum}}},
				{"version": "go1.24.5", "stable": true, "files": []map[string]string{{"filename": "go1.24.5.linux-amd64.tar.gz", "os": "linux", "arch": "amd64", "kind": "archive", "sha256": sum}}},
				{"version": "go1.24.4", "stable": true, "files": []map[string]string{{"filename": "go1.24.4.linux-amd64.tar.gz", "os": "linux", "arch": "amd64", "kind": "archive", "sha256": sum}}},
				{"version": "go1.24rc1", "stable": false, "files": []map[string]string{}},
			})
		case "/dl/go1.24.5.linux-amd64.tar.gz":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	tools := Toolchains{Dir: t.TempDir(), GoIndex: server.URL + "/index", GoDownload: server.URL + "/dl/", OS: "linux", Arch: "amd64"}
	root, err := tools.Go(context.Background(), "1.24")
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(tools.Dir, "go", "1.24.5") {
		t.Fatalf("root = %s", root)
	}
	if info, err := os.Stat(filepath.Join(root, "bin", "go")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("go binary: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(root, "bin")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("toolchain directories must be readable by applications: %v %v", info, err)
	}
	before := requests
	if again, err := tools.Go(context.Background(), "go1.24"); err != nil || again != root || requests != before {
		t.Fatalf("an installed version must be reused without network: %s %v (%d requests)", again, err, requests-before)
	}
	if _, err := tools.Go(context.Background(), "1.24.4"); err == nil {
		t.Fatal("the 1.24.4 archive is missing from the server and must fail")
	}
}

func TestToolchainsRejectChecksumMismatch(t *testing.T) {
	archive := tarball(t, []tarEntry{{name: "go/bin/go", body: "x", mode: 0o755}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index" {
			json.NewEncoder(w).Encode([]map[string]any{{"version": "go1.24.5", "stable": true, "files": []map[string]string{
				{"filename": "go1.24.5.linux-amd64.tar.gz", "os": "linux", "arch": "amd64", "kind": "archive", "sha256": strings.Repeat("0", 64)}}}})
			return
		}
		w.Write(archive)
	}))
	defer server.Close()
	tools := Toolchains{Dir: t.TempDir(), GoIndex: server.URL + "/index", GoDownload: server.URL, OS: "linux", Arch: "amd64"}
	if _, err := tools.Go(context.Background(), "1.24.5"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(tools.Dir, "go")); len(entries) != 0 {
		t.Fatalf("nothing may remain after a failed install: %v", entries)
	}
}

func TestToolchainsInstallNodeFromItsChecksumList(t *testing.T) {
	archive := tarball(t, []tarEntry{
		{name: "node-v22.11.0-linux-x64/bin/node", body: "#!/bin/sh\n", mode: 0o755},
		{name: "node-v22.11.0-linux-x64/lib/node_modules/npm/bin/npm-cli.js", body: "//\n", mode: 0o644},
		{name: "node-v22.11.0-linux-x64/bin/npm", kind: tar.TypeSymlink, link: "../lib/node_modules/npm/bin/npm-cli.js"},
	})
	file := "node-v22.11.0-linux-x64.tar.gz"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			fmt.Fprint(w, `[{"version":"v23.1.0","lts":false},{"version":"v22.11.0","lts":"Jod"},{"version":"v20.18.0","lts":"Iron"}]`)
		case "/v22.11.0/SHASUMS256.txt":
			fmt.Fprintf(w, "%s  node-v22.11.0-darwin-arm64.tar.gz\n%s  %s\n", strings.Repeat("1", 64), digest(archive), file)
		case "/v22.11.0/" + file:
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	tools := Toolchains{Dir: t.TempDir(), NodeDist: server.URL, OS: "linux", Arch: "amd64"}
	root, err := tools.Node(context.Background(), "") // newest LTS
	if err != nil || root != filepath.Join(tools.Dir, "node", "22.11.0") {
		t.Fatalf("root = %s, %v", root, err)
	}
	if target, err := os.Readlink(filepath.Join(root, "bin", "npm")); err != nil || target != "../lib/node_modules/npm/bin/npm-cli.js" {
		t.Fatalf("npm link = %s, %v", target, err)
	}
	if again, err := tools.Node(context.Background(), "22"); err != nil || again != root {
		t.Fatalf("partial pin: %s %v", again, err)
	}
	if tools.Installed("node", "") != root || tools.Installed("node", "20") != "" {
		t.Fatal("Installed must report installed versions only")
	}
}

func TestUntarRefusesEscapes(t *testing.T) {
	for name, entries := range map[string][]tarEntry{
		"parent":   {{name: "top/../../evil", body: "x", mode: 0o644}},
		"absolute": {{name: "/etc/evil", body: "x", mode: 0o644}},
		"link":     {{name: "top/evil", kind: tar.TypeSymlink, link: "../../etc/passwd"}},
		"abs link": {{name: "top/evil", kind: tar.TypeSymlink, link: "/etc/passwd"}},
	} {
		dest := t.TempDir()
		if err := untarStripped(bytes.NewReader(tarball(t, entries)), dest); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNodeRequirementReadsPins(t *testing.T) {
	dir := writeTree(t, map[string]string{".nvmrc": "lts/iron\n"})
	if got := NodeRequirement(dir); got != "lts/iron" {
		t.Fatalf("pin = %q", got)
	}
	if got := NodeRequirement(t.TempDir()); got != "" {
		t.Fatalf("no pin = %q", got)
	}
}

func TestToolchainFetchRetriesOverIPv4(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer broken.Close()
	working := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "archive") }))
	defer working.Close()
	// The first client reaches an edge that answers 404; the IPv4 client reaches one that serves the file.
	redirect := func(target string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			u, _ := url.Parse(target)
			r.URL.Scheme, r.URL.Host = u.Scheme, u.Host
			return http.DefaultTransport.RoundTrip(r)
		})}
	}
	tc := Toolchains{Client: redirect(broken.URL), IPv4Client: redirect(working.URL)}
	body, err := tc.get(context.Background(), "https://dl.example.com/go1.27.3.linux-amd64.tar.gz", 1<<10)
	if err != nil || string(body) != "archive" {
		t.Fatalf("fetch did not fall back to IPv4: %q %v", body, err)
	}
	tc.IPv4Client = redirect(broken.URL)
	if _, err := tc.get(context.Background(), "https://dl.example.com/missing.tar.gz", 1<<10); err == nil || !strings.Contains(err.Error(), "over IPv4") {
		t.Fatalf("expected both attempts reported, got %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNeedsRustFindsPackManifests(t *testing.T) {
	if NeedsRust(writeTree(t, map[string]string{"go.mod": "module x\n"})) {
		t.Fatal("a Go-only tree needs no Rust")
	}
	if !NeedsRust(writeTree(t, map[string]string{"packs/parser/rust/Cargo.toml": "[package]\n"})) {
		t.Fatal("a Rust pack was not detected")
	}
	if !NeedsRust(writeTree(t, map[string]string{"Cargo.toml": "[package]\n"})) {
		t.Fatal("a root Cargo manifest was not detected")
	}
}

func TestRustInstallsVerifiedToolchainOnce(t *testing.T) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("Rust installs are supported on linux/amd64 and linux/arm64")
	}
	// A stand-in rustup-init that lays out what the real one installs.
	installer := []byte("#!/bin/sh\nset -eu\n[ \"$(basename \"$0\")\" = rustup-init ] || { echo \"error: unknown proxy name: '$(basename \"$0\")'\" >&2; exit 1; }\nt=\"$RUSTUP_HOME/toolchains/stable-test\"\nmkdir -p \"$t/bin\" \"$CARGO_HOME/bin\"\nprintf '#!/bin/sh\\n' > \"$t/bin/cargo\"\nprintf '#!/bin/sh\\necho \"$@\" >> \"$RUSTUP_HOME/calls\"\\n' > \"$CARGO_HOME/bin/rustup\"\nchmod 755 \"$t/bin/cargo\" \"$CARGO_HOME/bin/rustup\"\necho \"$@\" > \"$RUSTUP_HOME/installed-with\"\n")
	sum := sha256.Sum256(installer)
	served := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/rustup-init.sha256"):
			fmt.Fprintf(w, "%x  rustup-init\n", sum)
		case strings.HasSuffix(r.URL.Path, "/rustup-init"):
			served++
			w.Write(installer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	tc := Toolchains{Dir: t.TempDir(), RustDownload: server.URL}
	root, err := tc.Rust(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(root) != "stable-test" {
		t.Fatalf("toolchain root %s", root)
	}
	args, _ := os.ReadFile(filepath.Join(tc.Dir, "rust", "rustup", "installed-with"))
	if !strings.Contains(string(args), "--profile minimal") || !strings.Contains(string(args), "--target wasm32-wasip1") {
		t.Fatalf("installer arguments: %s", args)
	}
	if _, err := tc.Rust(context.Background()); err != nil || served != 1 {
		t.Fatalf("second call reinstalled (%d downloads) or failed: %v", served, err)
	}
	calls, _ := os.ReadFile(filepath.Join(tc.Dir, "rust", "rustup", "calls"))
	if !strings.Contains(string(calls), "target add wasm32-wasip1") {
		t.Fatalf("an existing installation did not ensure its targets: %q", calls)
	}

	tampered := Toolchains{Dir: t.TempDir(), RustDownload: server.URL}
	sum[0] ^= 0xff
	if _, err := tampered.Rust(context.Background()); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("a tampered installer was accepted: %v", err)
	}
}
