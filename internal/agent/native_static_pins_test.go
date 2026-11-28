package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A static site's Node.js pin: omitted, it is inherited from the serving
// release; pinned by major version, it follows the newest matching version
// installed on the server; any other change is refused.
func TestStaticSiteNodePinFollowsInstalledVersionsOfItsMajor(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()
	setup := func(t *testing.T, live, requested string) (base, candidate, installs string) {
		t.Helper()
		root := t.TempDir()
		base, candidate, installs = filepath.Join(root, "current"), filepath.Join(root, "candidate"), filepath.Join(root, "installs")
		for _, dir := range []string{base, candidate, filepath.Join(installs, "26.12.0"), filepath.Join(installs, "26.13.1"), filepath.Join(installs, "27.0.0"), filepath.Join(installs, "24.5.0")} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if live != "" {
			os.WriteFile(filepath.Join(base, ".node-version"), []byte(live), 0o644)
		}
		if requested != "" {
			os.WriteFile(filepath.Join(candidate, ".node-version"), []byte(requested), 0o644)
		}
		return
	}
	pin := func(t *testing.T, dir string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, ".node-version"))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, c := range []struct{ name, live, requested, want string }{
		{"omitted: the serving release's version is kept", "26.12.0\n", "", "26.12.0\n"},
		{"a major pin follows the newest installed version of that major", "26.12.0\n", "26\n", "26.13.1\n"},
		{"without a trailing newline too", "26.12.0\n", "26", "26.13.1\n"},
		{"a major.minor pin stays within that minor", "26.13.1\n", "26.12", "26.12.0\n"},
		{"a major pin moves to another major installed on the server", "26.12.0\n", "27", "27.0.0\n"},
		{"a major pin on a site that had none", "", "26", "26.13.1\n"},
		{"the same exact version", "26.12.0\n", "26.12.0\n", "26.12.0\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, candidate, installs := setup(t, c.live, c.requested)
			want, err := prepareNativeRuntimePins(base, candidate, installs, uid, gid)
			if err != nil {
				t.Fatal(err)
			}
			if got := pin(t, candidate); got != c.want {
				t.Fatalf("the release pins %q, want %q", got, c.want)
			}
			if err := verifyNativeRuntimePins(candidate, want); err != nil {
				t.Fatal(err)
			}
			// A build that changes the pin, or adds another, is refused.
			os.WriteFile(filepath.Join(candidate, ".node-version"), []byte("24.5.0\n"), 0o644)
			if err := verifyNativeRuntimePins(candidate, want); err == nil {
				t.Fatal("a pin the build changed was accepted")
			}
			os.WriteFile(filepath.Join(candidate, ".node-version"), []byte(c.want), 0o644)
			os.WriteFile(filepath.Join(candidate, ".tool-versions"), []byte("nodejs 24.5.0\n"), 0o644)
			if err := verifyNativeRuntimePins(candidate, want); err == nil {
				t.Fatal("a pin file the build added was accepted")
			}
		})
	}
	for _, c := range []struct{ name, live, requested, message string }{
		{"another exact version", "26.12.0\n", "26.13.1\n", "pin Node.js by major version"},
		{"an exact version on a site that had no pin", "", "26.13.1\n", "review the runtime first"},
		{"a major that is not installed", "26.12.0\n", "28", "version 28 is not installed on this host (installed: 24.5.0, 26.12.0, 26.13.1, 27.0.0)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, candidate, installs := setup(t, c.live, c.requested)
			if _, err := prepareNativeRuntimePins(base, candidate, installs, uid, gid); err == nil || !strings.Contains(err.Error(), c.message) {
				t.Fatalf("error = %v, want one naming %q", err, c.message)
			}
		})
	}
	t.Run("a pin that is a symbolic link", func(t *testing.T) {
		base, candidate, installs := setup(t, "26.12.0\n", "")
		os.WriteFile(filepath.Join(candidate, "elsewhere"), []byte("26\n"), 0o644)
		os.Symlink("elsewhere", filepath.Join(candidate, ".node-version"))
		if _, err := prepareNativeRuntimePins(base, candidate, installs, uid, gid); err == nil {
			t.Fatal("a symbolic link was accepted as a pin")
		}
	})
	t.Run("a new .tool-versions", func(t *testing.T) {
		base, candidate, installs := setup(t, "26.12.0\n", "26")
		os.WriteFile(filepath.Join(candidate, ".tool-versions"), []byte("nodejs 26.13.1\n"), 0o644)
		if _, err := prepareNativeRuntimePins(base, candidate, installs, uid, gid); err == nil {
			t.Fatal("a new .tool-versions was accepted")
		}
	})
}
