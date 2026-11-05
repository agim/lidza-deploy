package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/release"
)

func TestNativeReleasePinToolchainsResolvesPartialPins(t *testing.T) {
	home := t.TempDir()
	for _, v := range []string{"nodejs/18.16.0", "nodejs/20.9.0", "nodejs/20.20.2", "nodejs/22.16.0", "ruby/3.4.5"} {
		if err := os.MkdirAll(filepath.Join(home, ".asdf/installs", v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	l := release.Layout{Root: t.TempDir()}
	dir := l.Release("20260101000000")
	os.MkdirAll(dir, 0o755)
	write := func(name, body string) { os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644) }
	read := func(name string) string { raw, _ := os.ReadFile(filepath.Join(dir, name)); return string(raw) }
	write(".node-version", "20\n")
	write(".nvmrc", "v22\n")
	write(".ruby-version", "3.4.5\n")
	if err := nativeReleasePinToolchains(home)(l, "20260101000000", ""); err != nil {
		t.Fatal(err)
	}
	if read(".node-version") != "20.20.2\n" || read(".nvmrc") != "22.16.0\n" || read(".ruby-version") != "3.4.5\n" {
		t.Fatalf("pins %q %q %q", read(".node-version"), read(".nvmrc"), read(".ruby-version"))
	}
	write(".node-version", "24\n")
	err := nativeReleasePinToolchains(home)(l, "20260101000000", "")
	if err == nil || !strings.Contains(err.Error(), "24 is not installed") || !strings.Contains(err.Error(), "20.20.2") {
		t.Fatal("missing runtime not reported with installed versions", err)
	}
	write(".node-version", "lts/iron\n")
	if err := nativeReleasePinToolchains(home)(l, "20260101000000", ""); err != nil || read(".node-version") != "lts/iron\n" {
		t.Fatal("non-numeric pins must be left for the version manager", err)
	}
}
