package agent

import (
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/release"
)

func TestNativeReleaseBuildMemoryFollowsPolicy(t *testing.T) {
	account := nativeReleaseAccount{Name: "deploy", Home: "/home/deploy", UID: 1000, GID: 1000}
	c := release.Command{Name: "build", Dir: "/home/deploy/app/releases/1", Args: []string{"true"}}
	if argv := strings.Join(nativeReleaseArgv(account, "/home/deploy/app", "u.service", "/env", c), " "); !strings.Contains(argv, "MemoryMax=2147483648") {
		t.Fatal("default build memory is not 2 GiB", argv)
	}
	if argv := strings.Join(nativeReleaseArgv(account, "/home/deploy/app", "u.service", "/env", c, 4096), " "); !strings.Contains(argv, "MemoryMax=4294967296") {
		t.Fatal("policy build memory not applied", argv)
	}
	if (nativeReleaseExecutor{memory: 4096}).memoryMiB() != 4096 || (nativeReleaseExecutor{}).memoryMiB() != 2048 {
		t.Fatal("executor memory selection")
	}
	for _, bad := range []int{100, 20000} {
		p := NativeReleasePolicy{Root: "/home/deploy/app", Repository: "o/r", Branch: "main", Runtime: "rails", BuildMemoryMiB: bad}
		if p.Validate() == nil {
			t.Fatal("accepted build memory", bad)
		}
	}
}
