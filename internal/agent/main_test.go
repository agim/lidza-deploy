package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agim/lidza-deploy/internal/release"
)

// TestMain keeps every test away from the host's managed toolchains: the
// managed root and the prebuilt Ruby prefix live in a temporary directory,
// so no test can install into or remove from /usr/local/lib/lidza or /opt.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lidza-agent-toolchains-")
	if err != nil {
		panic(err)
	}
	nativeToolchainDir = filepath.Join(dir, "lidza")
	nativeToolchains = func() release.Toolchains {
		return release.Toolchains{Dir: nativeToolchainDir, RubyPrefix: filepath.Join(dir, "hostedtoolcache", "Ruby"), RubyBuilder: "-"}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
