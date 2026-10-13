package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerAppResourceBudgetsThrottleAndContainOOM(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for kernel-enforced Docker resource tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "app")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../tests/resourcefixture")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(b))
	}
	rt := &Docker{Root: t.TempDir()}
	rt.checkout = func(ctx context.Context, a App, dir, token string) error {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		b, err := os.ReadFile(binary)
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, "app"), b, 0755); err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY --chmod=755 app /app\nENTRYPOINT [\"/app\"]\n"), 0600); err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, "lidza.json"), []byte(`{"name":"resources-fixture","frontend":{"template":"htmx"}}`), 0600); err != nil {
			return err
		}
		for _, args := range [][]string{{"init", "-b", "main"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "resource fixture"}} {
			command := exec.CommandContext(ctx, "git", args...)
			command.Dir = dir
			if b, err := command.CombinedOutput(); err != nil {
				t.Fatal(err, string(b))
			}
		}
		return nil
	}
	m, err := NewManager(ctx, Config{DataDir: t.TempDir()}, rt)
	if err != nil {
		t.Fatal(err)
	}
	id := "resource-" + newID()[:12]
	total, cores, hostErr := resourceHost()
	if hostErr != nil {
		t.Fatal(hostErr)
	}
	strict := defaultResourcePolicy(total, cores)
	strict.MemoryMode = "strict"
	strict.BuildMemoryMB = 1024 // Preserve the explicit legacy strict build cap.
	m.mu.Lock()
	m.data.ResourcePolicy = &strict
	m.mu.Unlock()
	a := testApp(id)
	limits := AppResources{MemoryMB: 512, WebMemoryMB: 128, WorkerMemoryMB: 64, CPUs: 0.4, Weight: 1024}
	a.Resources = &limits
	a.Env = map[string]string{"PRIVATE_RESOURCE_FIXTURE": "resource-fixture-secret"}
	if err = m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	releases := []*Release{}
	t.Cleanup(func() {
		m.Close()
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		for _, name := range names {
			_, _ = resourceCommand(cleanup, "rm", "-f", name)
		}
		for _, r := range releases {
			_ = rt.Remove(cleanup, r)
		}
		_, _ = resourceCommand(cleanup, "buildx", "rm", resourceBuilderName(rt.Root))
	})
	first, err := rt.Deploy(ctx, a, newID(), "")
	if err != nil {
		t.Fatal("bounded deployment", err)
	}
	releases = append(releases, first)
	m.mu.Lock()
	saved := m.data.Apps[id]
	saved.Current = first
	m.data.Apps[id] = saved
	err = m.save()
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	worker := Task{ID: "cpu", AppID: id, Mode: "worker", Command: []string{"/app", "burn"}, Enabled: true}
	if err = m.putTask(id, worker); err != nil {
		t.Fatal(err)
	}
	name, _, err := m.startTaskContainer(ctx, saved, worker, "")
	if err != nil {
		t.Fatal(err)
	}
	names = append(names, name)
	report, err := m.resourceReport(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	view := report.Apps[0]
	sumCPU := float64(0)
	sumRAM := int64(0)
	for _, c := range view.Containers {
		if c.Running {
			sumCPU += c.CPUQuota
			sumRAM += c.MemoryLimit
			if c.MemoryBytes == nil || c.CPUPercent == nil {
				t.Fatal("real stats unavailable", c)
			}
		}
		if c.Name == name {
			if c.MemoryLimit != 64*mib || c.CPUQuota > 0.21 || c.CPUShares != 512 {
				t.Fatal("worker took another app share", c)
			}
			if *c.CPUPercent > 35 {
				t.Fatal("CPU quota not enforced by kernel", *c.CPUPercent)
			}
		}
	}
	if sumCPU > 0.4001 || sumRAM > limits.MemoryMB*mib {
		t.Fatal("app aggregate limits exceeded", sumCPU, sumRAM)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "resource-fixture-secret") || strings.Contains(string(encoded), "PRIVATE_RESOURCE_FIXTURE") {
		t.Fatal("resource report leaked environment")
	}
	// Existing CPU protection still applies while legacy RAM reservations are
	// overcommitted; maintenance must not silently reduce memory to make them fit.
	memory, cpus, e := resourceHost()
	if e != nil {
		t.Fatal(e)
	}
	policy := defaultResourcePolicy(memory, cpus)
	policy.HostMemoryMB = memory
	m.mu.Lock()
	oldPolicy := m.data.ResourcePolicy
	oldApp := m.data.Apps[id]
	changed := oldApp
	l2 := limits
	l2.CPUs = 0.2
	changed.Resources = &l2
	m.data.ResourcePolicy = &policy
	m.data.Apps[id] = changed
	m.mu.Unlock()
	m.resourceMu.Lock()
	_, e = m.reconcileResourcePlan(ctx, nil, nil, false)
	m.resourceMu.Unlock()
	if e != nil {
		t.Fatal("existing CPU protection blocked by RAM overcommit", e)
	}
	inventory, e := resourceInventory(ctx)
	if e != nil {
		t.Fatal(e)
	}
	cpuSum := float64(0)
	for _, c := range inventory {
		if c.AppID == id && c.Running {
			cpuSum += c.CPUQuota
			if c.Kind == "web" && c.MemoryLimit != 128*mib {
				t.Fatal("maintenance changed memory", c)
			}
		}
	}
	if cpuSum > 0.2001 {
		t.Fatal("overcommitted fleet CPU protection missing", cpuSum)
	}
	m.mu.Lock()
	m.data.ResourcePolicy = oldPolicy
	m.data.Apps[id] = oldApp
	m.mu.Unlock()
	huge := limits
	huge.MemoryMB = 1048576
	if err = m.saveAppResources(ctx, id, huge); err == nil {
		t.Fatal("accepted host overcommit")
	}
	// Two live web copies plus workers are reserved. The fifth enabled worker
	// would make 2*128 + 5*64 exceed this app's fixed 512 MiB budget.
	for i := 0; i < 3; i++ {
		t2 := worker
		t2.ID = "extra-" + string(rune('a'+i))
		if err = m.putTask(id, t2); err != nil {
			t.Fatal(err)
		}
	}
	overflow := worker
	overflow.ID = "overflow"
	if err = m.putTask(id, overflow); err == nil {
		t.Fatal("enabled worker without budget headroom")
	}
	oomName := "lidza-task-" + id + "-oom"
	args, done, err := m.prepareResourceLaunch(ctx, saved, oomName, "worker")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	names = append(names, oomName)
	create := append([]string{"run", "-d", "--name", oomName, "--label", "io.lidza.task=" + id, "--entrypoint", "/app"}, args...)
	create = append(create, first.Image, "allocate")
	if _, err = resourceCommand(ctx, create...); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	killed := false
	for time.Now().Before(deadline) {
		raw, e := resourceCommand(ctx, "inspect", "--format", "{{.State.OOMKilled}} {{.State.ExitCode}}", oomName)
		if e == nil && raw == "true 137" {
			killed = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !killed {
		t.Fatal("memory overrun was not contained by the kernel")
	}
	done()
	if err = rt.Ready(ctx, first); err != nil {
		t.Fatal("worker OOM killed the live web app", err)
	}
	// Previous releases are stopped after activation and restarted safely on rollback.
	second, err := rt.Reload(ctx, saved, newID())
	if err != nil {
		t.Fatal(err)
	}
	releases = append(releases, second)
	m.mu.Lock()
	saved = m.data.Apps[id]
	saved.Previous = first
	saved.Current = second
	m.data.Apps[id] = saved
	m.mu.Unlock()
	if _, err = resourceCommand(ctx, "stop", "--time", "5", first.Container); err != nil {
		t.Fatal(err)
	}
	if err = m.Rollback(ctx, id); err != nil {
		t.Fatal("bounded rollback", err)
	}
	if err = rt.Ready(ctx, m.Current(id)); err != nil {
		t.Fatal(err)
	}
	builder := "buildx_buildkit_" + resourceBuilderName(rt.Root) + "0"
	raw, err := resourceCommand(ctx, "inspect", "--format", "{{.HostConfig.Memory}} {{.HostConfig.NanoCpus}}", builder)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "1073741824 ") {
		t.Fatal("builder memory is not bounded", raw)
	}
}
