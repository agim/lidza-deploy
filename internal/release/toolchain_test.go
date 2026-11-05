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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
