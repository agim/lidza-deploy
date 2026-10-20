package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerStackProfilesLifecycle(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for isolated real stack acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "fixture")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../tests/stack-fixture")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	manifest := fixtureStackManifest()
	rt := &Docker{Root: t.TempDir(), readinessTimeout: 10 * time.Second}
	rt.checkout = func(ctx context.Context, a App, dir, token string) error {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		data, err := os.ReadFile(binary)
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, "fixture"), data, 0755); err != nil {
			return err
		}
		for name, data := range map[string]string{"lidza.json": `{"name":"fixture","packs":["lidza/storage"],"frontend":{"template":"htmx"}}`, "Dockerfile": "FROM scratch\nCOPY --chmod=755 fixture /fixture\nENTRYPOINT [\"/fixture\"]\n"} {
			if err = os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
				return err
			}
		}
		if err = os.MkdirAll(filepath.Join(dir, "deploy"), 0700); err != nil {
			return err
		}
		raw, _ := json.Marshal(manifest)
		if err = os.WriteFile(filepath.Join(dir, "deploy", "lidza.stack.json"), raw, 0600); err != nil {
			return err
		}
		for _, args := range [][]string{{"init", "-b", "main"}, {"add", "."}, {"-c", "user.name=Stack acceptance", "-c", "user.email=test@example.com", "commit", "-m", "isolated fixture"}} {
			if _, err = command(ctx, dir, nil, "git", args...); err != nil {
				return err
			}
		}
		return nil
	}
	m := testManager(t, rt)
	a := testApp("stack-" + newID()[:10])
	a.RuntimeProfile = "minimum"
	a.Env["LIDZA_MASTER_KEY"] = strings.Repeat("1", 64)
	a.Env["RELEASE_TEXT"] = "one-"
	no := false
	a.BackupBeforeDeploy = &no
	var releases []*Release
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, r := range releases {
			_ = rt.Remove(cleanup, r)
		}
		m.mu.Lock()
		db := m.data.Databases[a.ID]
		storage := m.data.StorageVolumes[a.ID]
		volumes := append([]string{}, m.data.StackVolumes[a.ID]...)
		for _, r := range m.data.RetainedResources {
			if r.AppID == a.ID {
				volumes = append(volumes, r.StackVolumes...)
				if storage == "" {
					storage = r.StorageVolume
				}
			}
		}
		m.mu.Unlock()
		_, _ = command(cleanup, "", nil, "docker", "rm", "-f", db.Network)
		if db.Network != "" {
			_, _ = command(cleanup, "", nil, "docker", "volume", "rm", db.Network+"-data")
			_, _ = command(cleanup, "", nil, "docker", "network", "rm", db.Network)
		}
		if storage != "" {
			volumes = append(volumes, storage)
		}
		for _, v := range volumes {
			_, _ = command(cleanup, "", nil, "docker", "volume", "rm", v)
		}
	})
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if err := m.configureDatabase(a.ID, DatabaseRequest{Mode: "local", Backup: BackupPolicy{Keep: 1}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		m.mu.Lock()
		db := m.data.Databases[a.ID]
		m.mu.Unlock()
		if db.Ready {
			if db.MemoryMiB != 256 || db.CPUs != "0.25" {
				t.Fatal("dedicated DB profile not applied")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("database failed: %s", db.Error)
		}
		time.Sleep(100 * time.Millisecond)
	}
	deploy := func(reload bool) Deployment {
		t.Helper()
		var job Deployment
		var err error
		if reload {
			job, err = m.Reload(a.ID)
		} else {
			job, err = m.Enqueue(a.ID, DeployRequest{})
		}
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			for _, d := range m.Deployments() {
				if d.ID == job.ID && (d.Status == "live" || d.Status == "failed") {
					return d
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("stack deployment did not finish")
		return Deployment{}
	}
	first := deploy(false)
	if first.Status != "live" {
		t.Fatalf("first deployment: %s\n%s", first.Error, first.Log)
	}
	r1 := m.Current(a.ID)
	releases = append(releases, r1)
	if len(r1.Stack.Containers) != 3 || len(r1.Stack.Routes) != 1 {
		t.Fatal("missing stack metadata")
	}
	for _, c := range r1.Stack.Containers {
		out, err := command(ctx, "", nil, "docker", "inspect", "--format", "{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}} {{.HostConfig.Privileged}}", c.Container)
		budget, _ := profileByID("minimum")
		expected := fmt.Sprintf("%d %d false", budget.budget(c.Service.Role).MemoryMiB*1048576, budget.budget(c.Service.Role).MemoryMiB*1048576)
		if err != nil || out != expected {
			t.Fatalf("unsafe limits: %s != %s (%v)", out, expected, err)
		}
	}
	proxy := httptest.NewServer(Proxy(m))
	defer proxy.Close()
	request := func(method, path string) string {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, proxy.URL+path, nil)
		req.Host = a.Domain
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, res.StatusCode, body)
		}
		return string(body)
	}
	if request("GET", "/rtc/room") != "rtc:/room" {
		t.Fatal("SFU prefix routing failed")
	}
	if request("POST", "/persist") != "preserved" {
		t.Fatal("storage attachment failed")
	}
	inventory := m.managedContainers()
	sidecars := 0
	for _, c := range inventory {
		if c.AppID == a.ID && strings.HasPrefix(c.Role, "Stack") {
			sidecars++
		}
	}
	if sidecars != 3 {
		t.Fatalf("inventory missing sidecars: %d", sidecars)
	}
	second := deploy(true)
	if second.Status != "live" {
		t.Fatalf("reload: %s\n%s", second.Error, second.Log)
	}
	r2 := m.Current(a.ID)
	releases = append(releases, r2)
	if request("GET", "/persist") != "preserved" {
		t.Fatal("reload lost storage")
	}
	if err := m.Rollback(ctx, a.ID); err != nil {
		t.Fatal("rollback", err)
	}
	if m.Current(a.ID).ID != r1.ID {
		t.Fatal("rollback target not activated")
	}
	if request("GET", "/rtc/room") != "rtc:/room" {
		t.Fatal("rollback SFU route stale")
	}
	m.mu.Lock()
	bad := m.data.Apps[a.ID]
	bad.Env["FAIL_READY"] = "true"
	m.data.Apps[a.ID] = bad
	m.mu.Unlock()
	failed := deploy(true)
	if failed.Status != "failed" {
		t.Fatal("unhealthy candidate activated")
	}
	m.reconcileRoutes()
	if request("GET", "/persist") != "preserved" {
		t.Fatal("failure did not restore old stack")
	}
	m.mu.Lock()
	bad = m.data.Apps[a.ID]
	delete(bad.Env, "FAIL_READY")
	m.data.Apps[a.ID] = bad
	m.mu.Unlock()
	yes := true
	if err := m.setApplicationState(a.ID, ApplicationState{Stopped: &yes}); err != nil {
		t.Fatal(err)
	}
	for _, c := range r1.Stack.Containers {
		state, _ := command(ctx, "", nil, "docker", "inspect", "--format", "{{.State.Running}}", c.Container)
		if state != "false" {
			t.Fatal("stop left sidecar running")
		}
	}
	if err := m.setApplicationState(a.ID, ApplicationState{Stopped: &no}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Minute)
	for {
		current := m.Current(a.ID)
		if current.ID != r1.ID {
			releases = append(releases, current)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("start reload timed out")
		}
		time.Sleep(100 * time.Millisecond)
	}
	m.reconcileRoutes()
	if request("GET", "/persist") != "preserved" {
		t.Fatal("start lost storage")
	}
	// Changing the profile must not resize or retune an already provisioned DB.
	m.mu.Lock()
	oldDB := m.data.Databases[a.ID]
	m.mu.Unlock()
	profile := "standard"
	if err := m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, RuntimeProfile: &profile}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Minute)
	for {
		m.mu.Lock()
		busy := m.busy(a.ID)
		db := m.data.Databases[a.ID]
		m.mu.Unlock()
		if db.MemoryMiB != oldDB.MemoryMiB || db.CPUs != oldDB.CPUs {
			t.Fatal("existing DB silently resized")
		}
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("profile reload timed out")
		}
		time.Sleep(100 * time.Millisecond)
	}
	releases = append(releases, m.Current(a.ID))
	if err := m.Retire(ctx, a.ID); err != nil {
		t.Fatal("remove stack", err)
	}
	a.RuntimeProfile = "minimum"
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	fresh := m.data.Apps[a.ID]
	m.mu.Unlock()
	if fresh.resourceID() == a.ID {
		t.Fatal("recreated app adopted retained volume identity")
	}
	t.Log("real stack deployment: managed PostgreSQL, resource limits, SFU routing, inventory, persistence, reload, rollback, failed-candidate recovery, stop/start, profile resize isolation and removal passed")
}
