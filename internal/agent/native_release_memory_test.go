package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/release"
)

func TestNativeReleaseArgvAppliesBuildLimits(t *testing.T) {
	account := nativeReleaseAccount{Name: "deploy", Home: "/home/deploy", UID: 1000, GID: 1000}
	c := release.Command{Name: "build", Dir: "/home/deploy/app/releases/1", Args: []string{"true"}}
	argv := strings.Join(nativeReleaseArgv(account, "/home/deploy/app", "u.service", "/env", c, buildLimits{MemoryMiB: 4096, CPUQuota: 700}), " ")
	for _, want := range []string{"MemoryMax=4294967296", "MemorySwapMax=0", "CPUQuota=700%", "CPUWeight=20", "IOWeight=10", "Nice=10"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv lacks %s: %s", want, argv)
		}
	}
	if strings.Contains(argv, "CPUQuota=50%") {
		t.Fatal("half a core is no longer the build quota")
	}
	// A retry that allows swap leaves MemorySwapMax unset.
	argv = strings.Join(nativeReleaseArgv(account, "/home/deploy/app", "u.service", "/env", c, buildLimits{MemoryMiB: 8192, Swap: true, CPUQuota: 100}), " ")
	if !strings.Contains(argv, "MemoryMax=8589934592") || strings.Contains(argv, "MemorySwapMax") {
		t.Fatal("swap retry", argv)
	}
	for _, bad := range []int{100, 20000} {
		p := NativeReleasePolicy{Root: "/home/deploy/app", Repository: "o/r", Branch: "main", Runtime: "rails", BuildMemoryMiB: bad}
		if p.Validate() == nil {
			t.Fatal("accepted build memory", bad)
		}
	}
}

func TestNativeReleaseNodeHeapFollowsTheCap(t *testing.T) {
	env := nativeReleaseNodeHeap([]string{"A=1"}, 4096)
	if env[len(env)-1] != "NODE_OPTIONS=--max-old-space-size=3072" {
		t.Fatal(env)
	}
	if env := nativeReleaseNodeHeap([]string{"NODE_OPTIONS=--x"}, 4096); len(env) != 1 {
		t.Fatal("explicit NODE_OPTIONS is kept", env)
	}
}

func TestNativeReleaseStepsUseADiskScratchDirectory(t *testing.T) {
	account := nativeReleaseAccount{Name: "app-shop", Home: "/srv/apps/shop", UID: os.Getuid(), GID: os.Getgid()}
	c := release.Command{Name: "build", Dir: "/srv/apps/shop/releases/1", Args: []string{"true"}}
	argv := strings.Join(nativeReleaseArgv(account, "/srv/apps/shop", "u.service", "/env", c, buildLimits{MemoryMiB: 2048}), "\n")
	for _, want := range []string{"TMPDIR=/srv/apps/shop/.cache/tmp", "GOTMPDIR=/srv/apps/shop/.cache/tmp"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv lacks %s:\n%s", want, argv)
		}
	}
	root := t.TempDir()
	stale := filepath.Join(nativeReleaseScratchDir(root), "go-link-1")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := nativeReleaseScratch(root, account); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("scratch directory was not emptied before the step")
	}
	if info, err := os.Stat(nativeReleaseScratchDir(root)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("scratch directory mode %v, %v", info.Mode().Perm(), err)
	}
}
