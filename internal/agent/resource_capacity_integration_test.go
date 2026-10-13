package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDockerAutomaticCapacityDeploysWithUnusedCeilingsAndRecovers(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for automatic resource admission")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	m := testManager(t, &Docker{Root: t.TempDir()})
	total, cpus, err := resourceHost()
	if err != nil {
		t.Fatal(err)
	}
	a := testApp("auto-capacity-" + newID()[:12])
	limits := defaultAppResources()
	limits.MemoryMB = total + 4096 // Ceiling exceeds RAM; it is not allocated.
	a.Resources = &limits
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	c, _, err := m.reserveCacheLocked(a.ID, CacheRequest{Mode: "local"}, true)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	second := c
	second.Network += "-next"
	second.URL = "redis://:" + second.LocalPassword + "@" + second.Network + ":6379/0"
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		for _, cache := range []CacheResource{c, second} {
			_, _ = resourceCommand(cleanup, "rm", "-f", cache.Network)
			_, _ = resourceCommand(cleanup, "volume", "rm", cache.Network+"-data")
			_, _ = resourceCommand(cleanup, "network", "rm", cache.Network)
		}
	})
	if err := m.provisionCache(ctx, c); err != nil {
		t.Fatal("unused app ceilings blocked real automatic cache startup", err)
	}
	report, err := m.resourceReport(ctx, false)
	if err != nil || report.CapacityMode != "automatic" || report.ReservedMemoryMB <= total || report.AvailableMemoryMB <= 0 {
		t.Fatal("automatic mode still reports maxima as reserved capacity", report, err)
	}
	available, err := availableHostMemory()
	if err != nil {
		t.Fatal(err)
	}
	policy := defaultResourcePolicy(total, cpus)
	policy.HostMemoryMB = available // Simulate unavailable protected headroom.
	m.mu.Lock()
	m.data.ResourcePolicy = &policy
	m.mu.Unlock()
	waited := make(chan struct{})
	var once sync.Once
	waitCtx := context.WithValue(ctx, diagnosticKey{}, func(line string) {
		if strings.Contains(line, "waiting for available RAM") {
			once.Do(func() { close(waited) })
		}
	})
	result := make(chan error, 1)
	go func() { result <- m.provisionCache(waitCtx, second) }()
	select {
	case <-waited:
	case err := <-result:
		t.Fatal("did not wait automatically under memory pressure", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if localCacheReady(ctx, second) {
		t.Fatal("service launched before headroom became available")
	}
	relaxed := defaultResourcePolicy(total, cpus)
	relaxed.MemoryMode = "" // Existing saved policies adopt automatic management.
	m.mu.Lock()
	m.data.ResourcePolicy = &relaxed
	m.mu.Unlock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal("queued cache did not recover without user retry", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, cache := range []CacheResource{c, second} {
		memory, err := resourceCommand(ctx, "inspect", "--format", "{{.HostConfig.Memory}}", cache.Network)
		if err != nil || memory != "268435456" || !localCacheReady(ctx, cache) {
			t.Fatal("automatic management lost enforcement or authenticated readiness", memory, err)
		}
	}
	// Exercise the actual build and candidate launch with the same overcommitted
	// ceiling metadata, not just service provisioning and a synthetic report.
	rt := m.runtime.(*Docker)
	binary := filepath.Join(t.TempDir(), "app")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../tests/resourcefixture")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	rt.checkout = func(ctx context.Context, app App, dir, token string) error {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		data, err := os.ReadFile(binary)
		if err != nil {
			return err
		}
		for name, body := range map[string][]byte{"app": data, "Dockerfile": []byte("FROM scratch\nCOPY --chmod=755 app /app\nENTRYPOINT [\"/app\"]\n"), "lidza.json": []byte(`{"name":"automatic-fixture","frontend":{"template":"htmx"}}`)} {
			if err := os.WriteFile(filepath.Join(dir, name), body, 0755); err != nil {
				return err
			}
		}
		for _, args := range [][]string{{"init", "-b", "main"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "automatic fixture"}} {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return errors.New(string(out))
			}
		}
		return nil
	}
	t.Cleanup(func() { _, _ = resourceCommand(context.Background(), "buildx", "rm", resourceBuilderName(rt.Root)) })
	release, err := rt.Deploy(ctx, a, newID(), "")
	if err != nil {
		t.Fatal("automatic build/candidate launch blocked by unused ceilings", err)
	}
	t.Cleanup(func() { _ = rt.Remove(context.Background(), release) })
	builderMemory, err := resourceCommand(ctx, "inspect", "--format", "{{.HostConfig.Memory}}", "buildx_buildkit_"+resourceBuilderName(rt.Root)+"0")
	if err != nil || builderMemory != strconv.FormatInt(relaxed.BuildMemoryMB*mib, 10) {
		t.Fatal("adaptive build cap not enforced", builderMemory, err)
	}
	strict := relaxed
	strict.MemoryMode = "strict"
	m.mu.Lock()
	m.data.ResourcePolicy = &strict
	m.mu.Unlock()
	_, _, err = m.prepareServiceResourcesOnce(ctx, second.Network+"-new", "cache")
	var pressure *resourceMemoryPressure
	if err == nil || errors.As(err, &pressure) || !strings.Contains(err.Error(), "RAM budgets reserve") {
		t.Fatal("explicit strict reservations ignored", err)
	}
}
