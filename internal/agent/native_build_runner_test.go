package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeBuildScopeUsesNonRootPidNamespaceAndReadOnlyOtherReleases(t *testing.T) {
	if os.Getenv("TEST_NATIVE_SCOPE") != "1" {
		t.Skip("set TEST_NATIVE_SCOPE=1 for the isolated local systemd build acceptance")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires root on a disposable local systemd host")
	}
	m := testManager(t, &fakeRuntime{})
	root, err := os.MkdirTemp("/var/tmp", "lidza-native-scope-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	app := filepath.Join(root, "scope")
	candidate := filepath.Join(app, "releases/candidate")
	old := filepath.Join(app, "releases/current")
	for _, dir := range []string{candidate, old} {
		if err = os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	os.Chmod(root, 0755)
	if err = os.Symlink(old, filepath.Join(app, "current")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("APP_BRANCH=main\n"), 0600)
	if err = filepath.WalkDir(app, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, 1000, 1000)
	}); err != nil {
		t.Fatal(err)
	}
	m.cfg.Native = &NativeHostConfig{Root: root, User: "agim"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var diagnostic string
	ctx = context.WithValue(ctx, diagnosticStreamKey{}, func(name string) func(string) { return func(output string) { diagnostic = name + "\n" + output } })
	output, err := m.runNativeBuild(ctx, NativeApplication{Name: "scope", Branch: "main"}, candidate, newID(), []string{"/bin/sh", "-c", `test "$$" = 1; test "$(id -u)" = 1000; grep -q 'CapBnd:.*0000000000000000' /proc/self/status; test ! -w "$1"; test -w "$2"; test ! -S /run/user/1000/bus; printf scope-ok`, "scope-check", old, candidate})
	if err != nil || !strings.Contains(output, "scope-ok") {
		t.Fatal("native build isolation did not verify", err, diagnostic)
	}
}

func TestNativeBuildEnvironmentPreservesLiteralsAndRejectsShell(t *testing.T) {
	m, app := nativeAstroFixture(t)
	for _, example := range []struct {
		source, value string
		valid         bool
	}{
		{"SECRET='value with spaces and \"quotes\"'", "value with spaces and \"quotes\"", true},
		{"SECRET='literal$`value'", "literal$`value", true},
		{`SECRET="literal-value"`, "literal-value", true},
		{"SECRET=abc-123", "abc-123", true},
		{"SECRET=$(touch /tmp/forbidden)", "", false},
		{`SECRET="escaped\nvalue"`, "", false},
		{"echo surprise", "", false},
	} {
		os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte(example.source+"\n"), 0600)
		values, err := m.nativeBuildEnvironment("marketing")
		if example.valid && (err != nil || values["SECRET"] != example.value) || !example.valid && err == nil {
			t.Fatal("literal environment parsing did not preserve or refuse the source", err)
		}
	}
}
