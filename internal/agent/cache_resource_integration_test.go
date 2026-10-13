package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDockerExistingCacheHealthDoesNotRequireNewCapacity(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for cache resource admission regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := CacheResource{Mode: "local", Network: "lidza-cache-health-" + newID()[:12], LocalPassword: strings.Repeat("a9", 32), Ready: true}
	c.URL = "redis://:" + c.LocalPassword + "@" + c.Network + ":6379/0"
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_, _ = resourceCommand(cleanup, "rm", "-f", c.Network)
		_, _ = resourceCommand(cleanup, "volume", "rm", c.Network+"-data")
		_, _ = resourceCommand(cleanup, "network", "rm", c.Network)
	})
	if err := provisionLocalCache(ctx, c); err != nil {
		t.Fatal(err)
	}
	m := testManager(t, &Docker{Root: t.TempDir()})
	a := testApp("cache-budget-fixture")
	a.Env["CACHE_URL"] = c.URL
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	memory, cpus, err := resourceHost()
	if err != nil {
		t.Fatal(err)
	}
	policy := defaultResourcePolicy(memory, cpus)
	policy.MemoryMode = "strict"
	policy.HostMemoryMB = memory // Deliberately overcommitted legacy reservations.
	m.mu.Lock()
	m.data.ResourcePolicy = &policy
	m.data.Caches[a.ID] = c
	m.mu.Unlock()
	before, err := resourceCommand(ctx, "inspect", "--format", "{{.HostConfig.Memory}}", c.Network)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.preflightRuntime(ctx, a, cacheManifest(t, `["lidza/cache"]`)); err != nil {
		t.Fatal("healthy existing cache mistaken for a new launch", err)
	}
	after, err := resourceCommand(ctx, "inspect", "--format", "{{.HostConfig.Memory}}", c.Network)
	if err != nil || after != before {
		t.Fatal("health check changed existing memory limit", before, after, err)
	}
	if _, _, err = m.prepareResourceBuild(ctx, a); err == nil || !strings.Contains(err.Error(), "RAM budgets reserve") {
		t.Fatal("unsafe build admitted or capacity failure hidden", err)
	}
	wrong := c
	wrong.LocalPassword = "wrong-password-for-fixture"
	if localCacheReady(ctx, wrong) {
		t.Fatal("health check accepted an unauthenticated cache")
	}
	if _, err = resourceCommand(ctx, "stop", c.Network); err != nil {
		t.Fatal(err)
	}
	err = m.provisionCache(ctx, c)
	if err == nil || !strings.Contains(publicCacheFailure(err).Error(), "RAM budgets reserve") {
		t.Fatal("stopped cache bypassed admission or lost resource error", err)
	}
	running, err := resourceCommand(ctx, "inspect", "--format", "{{.State.Running}}", c.Network)
	if err != nil || running != "false" {
		t.Fatal("overcommitted service was restarted", running, err)
	}
}
