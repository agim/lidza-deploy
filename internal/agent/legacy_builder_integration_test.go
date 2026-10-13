package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

func TestDockerUpgradeRemovesOnlyLegacyBoundedBuilder(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for the legacy Buildx cleanup regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	digest := sha256.Sum256([]byte(root))
	name := "lidza-" + hex.EncodeToString(digest[:6])
	if _, err := command(ctx, "", nil, "docker", "buildx", "create", "--name", name, "--driver", "docker-container", "--driver-opt", "memory=256m,cpu-period=100000,cpu-quota=10000"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = command(context.Background(), "", nil, "docker", "buildx", "rm", name) })
	removeLegacyBoundedBuilder(ctx, root)
	if _, err := command(ctx, "", nil, "docker", "buildx", "inspect", name); err == nil {
		t.Fatal("legacy bounded builder still exists")
	}
}
