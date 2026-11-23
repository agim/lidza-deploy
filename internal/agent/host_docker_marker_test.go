package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests must not depend on whether the machine running them was itself set
// up by the hosting installer.
func init() {
	dockerManagedMarker = filepath.Join(os.TempDir(), "lidza-agent-test-absent", "docker-managed")
}

func TestInstallerPreparedHostManagesDockerBeforeItsFirstContainer(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	saved := dockerManagedMarker
	t.Cleanup(func() { dockerManagedMarker = saved })
	dockerManagedMarker = filepath.Join(t.TempDir(), "docker-managed")
	if m.dockerManaged() {
		t.Fatal("a host that merely has Docker must be left alone")
	}
	if err := os.WriteFile(dockerManagedMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.dockerManaged() {
		t.Fatal("a host prepared by the hosting installer manages Docker even with no container application yet")
	}
}
