package agent

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// A host where Docker's units were masked (persistently or at runtime)
// gets them unmasked before the agent starts the runtime.
func TestDockerMaskedUnitsAreDetected(t *testing.T) {
	previous := hostCommand
	t.Cleanup(func() { hostCommand = previous })
	hostCommand = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "systemctl" && len(args) == 2 && args[0] == "is-enabled" {
			switch args[1] {
			case "docker.service":
				return "masked-runtime\n", errors.New("exit status 1")
			case "docker.socket":
				return "masked\n", errors.New("exit status 1")
			}
			return "enabled\n", nil
		}
		return "", nil
	}
	got := dockerMaskedUnits(context.Background())
	if !slices.Equal(got, []string{"docker.socket", "docker.service"}) {
		t.Fatalf("masked units %v", got)
	}
}
