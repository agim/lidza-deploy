package plugin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type entry struct {
	name, body string
	mode       int64
	kind       byte
	link       string
}

func archiveOf(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		header := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: e.kind, Linkname: e.link}
		if e.kind == 0 {
			header.Typeflag = tar.TypeReg
		}
		if header.Typeflag != tar.TypeReg {
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// script is a plugin written in shell: enough to exercise the contract.
const script = `#!/bin/sh
request=$(cat)
case "$1" in
 describe) printf '{"ok":true,"messages":["example plugin"],"output":{"env":"%s","cwd":"%s","contract":%s}}' "$(env | sort | tr '\n' ' ')" "$(pwd)" "$(printf '%s' "$request" | sed 's/.*"contract":\([0-9]*\).*/\1/')";;
 check) echo "checking" >&2; printf '{"ok":false,"checks":[{"id":"cpu","title":"Processor","ok":false,"detail":"too old"}]}'; exit 3;;
 status) sleep 30;;
 tune) printf 'not json';;
 repair) printf '{"ok":true}'; exit 1;;
esac
`

func manifestFor(t *testing.T, archive []byte, edit func(*Manifest)) (Manifest, []byte) {
	t.Helper()
	sum := sha256.Sum256(archive)
	m := Manifest{Contract: Contract, Name: "example", Version: "1.2", Provides: "example", Summary: "An example.",
		Hooks: []string{"describe", "check", "status", "tune", "repair"}, Environment: []string{"EXAMPLE_URL"},
		Archives: []Archive{{Arch: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive))}}}
	if edit != nil {
		edit(&m)
	}
	raw, _ := json.Marshal(m)
	return m, raw
}

func TestManifestIsOnlyAcceptedWithATrustedSignature(t *testing.T) {
	archive := archiveOf(t, entry{name: Program, body: script, mode: 0o755})
	_, raw := manifestFor(t, archive, nil)
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	sig, _ := json.Marshal(Sign(private, raw))
	m, keyID, err := Verify(raw, sig, []ed25519.PublicKey{other, public})
	if err != nil || m.Name != "example" || keyID != KeyID(public) || !m.Implements("check") || m.Implements("backup") {
		t.Fatalf("verify: %v %+v %s", err, m, keyID)
	}
	if _, _, err := Verify(raw, sig, nil); err == nil || !strings.Contains(err.Error(), "trusts no plugin signing key") {
		t.Fatalf("no trusted key: %v", err)
	}
	if _, _, err := Verify(raw, sig, []ed25519.PublicKey{other}); err == nil || !strings.Contains(err.Error(), "does not trust") {
		t.Fatalf("untrusted key: %v", err)
	}
	// A changed manifest no longer verifies.
	if _, _, err := Verify(bytes.Replace(raw, []byte(`"1.2"`), []byte(`"9.9"`), 1), sig, []ed25519.PublicKey{public}); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("tampered manifest: %v", err)
	}
	// A signed manifest must still be a valid one.
	for name, edit := range map[string]func(*Manifest){
		"contract":     func(m *Manifest) { m.Contract = 2 },
		"name":         func(m *Manifest) { m.Name = "Bad Name" },
		"unknown hook": func(m *Manifest) { m.Hooks = append(m.Hooks, "format-disk") },
		"no describe":  func(m *Manifest) { m.Hooks = []string{"check"} },
		"environment":  func(m *Manifest) { m.Environment = []string{"path"} },
		"archive":      func(m *Manifest) { m.Archives[0].SHA256 = "abc" },
		"no archive":   func(m *Manifest) { m.Archives = nil },
		"architecture": func(m *Manifest) { m.Archives[0].Arch = "riscv" },
	} {
		_, bad := manifestFor(t, archive, edit)
		badSig, _ := json.Marshal(Sign(private, bad))
		if _, _, err := Verify(bad, badSig, []ed25519.PublicKey{public}); err == nil {
			t.Fatalf("%s: an invalid manifest was accepted", name)
		}
	}
}

func TestInstallUnpacksOnlyWhatTheManifestSigned(t *testing.T) {
	root := t.TempDir()
	archive := archiveOf(t, entry{name: "lib", kind: tar.TypeDir, mode: 0o755}, entry{name: "lib/helper.sh", body: "echo hi\n", mode: 0o644}, entry{name: Program, body: script, mode: 0o755})
	m, _ := manifestFor(t, archive, nil)
	dir, err := Install(root, m, "amd64", bytes.NewReader(archive))
	if err != nil || dir != filepath.Join(root, "example", "1.2") {
		t.Fatalf("install: %v %s", err, dir)
	}
	if info, err := os.Stat(filepath.Join(dir, Program)); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("program: %v %v", err, info)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "lib/helper.sh")); string(raw) != "echo hi\n" {
		t.Fatal("a file of the archive is missing")
	}
	// Installing the same version again changes nothing.
	if again, err := Install(root, m, "amd64", strings.NewReader("not even an archive")); err != nil || again != dir {
		t.Fatalf("reinstall: %v", err)
	}
	if _, err := Install(root, m, "arm64", bytes.NewReader(archive)); err == nil || !strings.Contains(err.Error(), "no build for arm64") {
		t.Fatalf("other architecture: %v", err)
	}
	refused := func(name string, archive []byte, edit func(*Manifest), want string) {
		t.Helper()
		m, _ := manifestFor(t, archive, func(m *Manifest) {
			m.Version = "2." + name
			if edit != nil {
				edit(m)
			}
		})
		if _, err := Install(root, m, "amd64", bytes.NewReader(archive)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", name, err)
		}
		if entries, _ := os.ReadDir(filepath.Join(root, "example")); len(entries) != 1 {
			t.Fatalf("%s: something was left behind: %v", name, entries)
		}
	}
	refused("1", archive, func(m *Manifest) { m.Archives[0].SHA256 = strings.Repeat("0", 64) }, "does not match its signed manifest")
	refused("2", append(append([]byte{}, archive...), 'x'), func(m *Manifest) { m.Archives[0].Size-- }, "does not match its signed manifest")
	refused("3", archiveOf(t, entry{name: "../escape", body: "x", mode: 0o644}, entry{name: Program, body: script, mode: 0o755}), nil, "unsafe path")
	refused("4", archiveOf(t, entry{name: Program, kind: tar.TypeSymlink, link: "/bin/sh"}), nil, "not a file or directory")
	refused("5", archiveOf(t, entry{name: "readme", body: "no program", mode: 0o644}), nil, "no \"plugin\" program")
	refused("6", []byte("plain text, not an archive"), nil, "not a gzip-compressed tar")
}

func TestRunGivesAHookItsRequestLimitsAndNothingOfTheAgent(t *testing.T) {
	root := t.TempDir()
	archive := archiveOf(t, entry{name: Program, body: script, mode: 0o755})
	m, _ := manifestFor(t, archive, nil)
	dir, err := Install(root, m, "amd64", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIDZA_AGENT_TOKEN", "agent-secret-token")
	state := filepath.Join(t.TempDir(), "state")
	run := func(hook string) (Result, error) {
		return Run(context.Background(), dir, Request{Hook: hook, StateDir: state, Server: Server{Arch: "amd64", CPUFlags: []string{"sse4_2"}}})
	}
	result, err := run("describe")
	var out struct {
		Env, Cwd string
		Contract int
	}
	json.Unmarshal(result.Answer.Output, &out)
	if err != nil || !result.Answer.OK || out.Contract != Contract || out.Cwd != state {
		t.Fatalf("describe: %v %+v %+v", err, result.Answer, out)
	}
	// The plugin sees a minimal environment: nothing of the agent's.
	if strings.Contains(out.Env, "agent-secret-token") || strings.Contains(out.Env, "LIDZA_") || !strings.Contains(out.Env, "PATH=/usr/local/sbin") {
		t.Fatalf("plugin environment: %s", out.Env)
	}
	// A failing check is an answer, not an error: its checks and its log come back.
	result, err = run("check")
	if err != nil || result.Answer.OK || len(result.Answer.Checks) != 1 || result.Answer.Checks[0].Detail != "too old" || !strings.Contains(result.Log, "checking") {
		t.Fatalf("check: %v %+v log %q", err, result.Answer, result.Log)
	}
	if _, err = run("tune"); err == nil || !strings.Contains(err.Error(), "did not answer with JSON") {
		t.Fatalf("not JSON: %v", err)
	}
	if _, err = run("repair"); err == nil || !strings.Contains(err.Error(), "answered ok but exited with an error") {
		t.Fatalf("ok with a failing exit: %v", err)
	}
	// A hook that runs past its limit is stopped, with everything it started.
	saved := hookTimeout
	t.Cleanup(func() { hookTimeout = saved })
	hookTimeout = func(string) time.Duration { return 300 * time.Millisecond }
	started := time.Now()
	if _, err = run("status"); err == nil || !strings.Contains(err.Error(), "did not finish within") || time.Since(started) > 8*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(started))
	}
	if _, err := Run(context.Background(), dir, Request{Hook: "describe", StateDir: "relative"}); err == nil {
		t.Fatal("a relative state directory was accepted")
	}
}
