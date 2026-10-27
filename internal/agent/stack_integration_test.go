package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	// Other containers' limits are ceilings, not reserved RAM. Neither a
	// ceiling above physical memory nor an unbounded unrelated container
	// should block deploy, reload or rollback. These idle fixtures allocate
	// no large buffers and are removed independently of the app resources.
	for _, memory := range []string{"0", "64g"} {
		name := "lidza-capacity-fixture-" + newID()[:10]
		args := []string{"run", "--rm", "-d", "--name", name, "--network", "none"}
		if memory != "0" {
			args = append(args, "--memory", memory, "--memory-swap", memory)
		}
		args = append(args, cacheImage, "sleep", "600")
		if _, err := command(ctx, "", nil, "docker", args...); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = command(cleanup, "", nil, "docker", "rm", "-f", name)
		})
	}
	manifest := fixtureStackManifest()
	if os.Getenv("TEST_STACK_V2") == "1" {
		manifest = realStackV2Manifest(t, ctx, manifest)
	}
	if os.Getenv("TEST_PUBLIC_PORTS") == "1" {
		manifest.Services[0].PublicPorts = []StackPort{{Port: 25, Protocol: "tcp"}, {Port: 7882, Protocol: "udp"}}
		manifest.Services[0].Environment["FIXTURE_PUBLIC_PORTS"] = "true"
	}
	if os.Getenv("TEST_STACK_GATEWAY") == "1" {
		manifest.GatewayTransport = &StackGateway{Version: 1, Paths: []string{"/gateway-protected"}}
	}
	manifest.BootstrapReadiness = &StackBootstrapReadiness{Path: "/api/v1/health", ReadyzBodyContains: "Server setup is required", SetupPath: "/setup"}
	manifest.Configuration = []StackConfiguration{
		{Name: "MAIL_PROVIDER", Stage: "setup"},
		{Name: "MAIL_FROM", Stage: "setup"},
		{Name: "LLM_PROVIDER", Stage: "optional"},
	}
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
		for name, data := range map[string]string{"lidza.json": `{"name":"fixture","packs":["lidza/db","lidza/storage","lidza/mail","lidza/llm"],"frontend":{"template":"htmx"}}`, "Dockerfile": "FROM scratch\nCOPY --chmod=755 fixture /fixture\nUSER 65532:65532\nENTRYPOINT [\"/fixture\"]\n"} {
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
	if os.Getenv("TEST_STACK_GATEWAY") == "1" {
		cfg := gatewayTestConfig(t)
		m.cfg.Gateway = &cfg
		if err := m.initGateway(); err != nil {
			t.Fatal(err)
		}
	}
	var tlsFixture *publicTLSFixture
	if os.Getenv("TEST_STACK_PUBLIC_TLS") == "1" {
		tlsFixture = newPublicTLSFixture(t)
		m.cfg.ServiceTLS = &ServiceTLSConfig{CertificateDirectory: tlsFixture.c.source, TrustRoots: filepath.Join(filepath.Dir(tlsFixture.c.root), "ca.pem")}
		c, err := newServiceTLSController(m.cfg)
		if err != nil {
			t.Fatal(err)
		}
		m.serviceTLS = c
		rt.serviceTLS = c
		tlsFixture.c = c
		manifest.Services[1].PublicTLS = &StackPublicTLS{Hostname: "primary", UID: os.Getuid()}
	}
	a := testApp("stack-" + newID()[:10])
	if tlsFixture != nil {
		tlsFixture.a = a
		tlsFixture.b = ServiceTLSBinding{ResourceID: a.resourceID(), Service: "mail", Hostname: a.Domain, UID: os.Getuid()}
		cert, key := tlsFixture.issue(t, []string{a.Domain}, time.Now().Add(time.Hour))
		tlsFixture.store(t, cert, key)
	}
	a.RuntimeProfile = "minimum"
	a.Env["LIDZA_MASTER_KEY"] = strings.Repeat("1", 64)
	a.Env["RELEASE_TEXT"] = "one-"
	a.Env["FIXTURE_NEEDS_SETUP"] = "true"
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
	assertGateway := func() {
		if os.Getenv("TEST_STACK_PUBLIC_TLS") == "1" {
			assertDockerPublicTLS(t, m, a.ID)
		}
		if os.Getenv("TEST_STACK_GATEWAY") == "1" {
			assertDockerGateway(t, m, a.ID)
		}
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
				if d.ID == job.ID && (d.Status == "live" || d.Status == "failed" || d.Status == "setup_required") {
					if d.Status == "live" || d.Status == "setup_required" {
						assertGateway()
					}
					return d
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("stack deployment did not finish")
		return Deployment{}
	}
	m.mu.Lock()
	for _, key := range []string{"MAIL_PROVIDER", "MAIL_FROM", "LLM_PROVIDER"} {
		if m.data.Apps[a.ID].Env[key] != "" {
			t.Fatalf("fixture supplied %s, hiding empty-settings bootstrap regression", key)
		}
	}
	m.mu.Unlock()
	// App-specific deploy requirements fail with actionable guidance before
	// building, independently of setup-owned framework settings.
	manifest.Configuration = append(manifest.Configuration, StackConfiguration{Name: "VENDOR_CREDENTIAL", Stage: "deploy", Label: "Vendor credential", Help: "Import the production credential.", Secret: true})
	blocked := deploy(false)
	if blocked.Status != "failed" || !strings.Contains(blocked.Error, "Vendor credential") || !strings.Contains(blocked.Error, "Import the production credential.") || m.Current(a.ID) != nil {
		t.Fatalf("missing deploy requirement was not safely blocked: %+v", blocked)
	}
	manifest.Configuration = manifest.Configuration[:len(manifest.Configuration)-1]
	first := deploy(false)
	if first.Status != "setup_required" {
		t.Fatalf("first deployment: %s\n%s", first.Error, first.Log)
	}
	r1 := m.Current(a.ID)
	releases = append(releases, r1)
	expectedServices := 3
	if manifest.Version == 2 {
		expectedServices = 4
	}
	if len(r1.Stack.Containers) != expectedServices || len(r1.Stack.Routes) != 1 {
		t.Fatal("missing stack metadata")
	}
	for _, c := range r1.Stack.Containers {
		out, err := command(ctx, "", nil, "docker", "inspect", "--format", "{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}} {{.HostConfig.Privileged}}", c.Container)
		budget, _ := profileByID("minimum")
		expected := fmt.Sprintf("%d %d false", manifest.serviceBudget(budget, c.Service).MemoryMiB*1048576, manifest.serviceBudget(budget, c.Service).MemoryMiB*1048576)
		if err != nil || out != expected {
			t.Fatalf("unsafe limits: %s != %s (%v)", out, expected, err)
		}
	}
	assertPublicPorts := func() {
		if os.Getenv("TEST_PUBLIC_PORTS") != "1" {
			return
		}
		inventory, err := m.appPorts(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		public := 0
		for _, port := range inventory.Ports {
			if port.Declared {
				public++
				if port.Publication != "published" || port.Firewall != "allow_rule_installed" || port.External != "unverified" {
					t.Fatalf("bad public inventory: %+v", port)
				}
			}
		}
		if public != 2 {
			t.Fatalf("missing public inventory: %+v", inventory)
		}
		if result, err := command(ctx, "", nil, "ip", "netns", "exec", "ports-client", "curl", "--silent", "--show-error", "--noproxy", "*", "--fail", "--max-time", "3", "http://198.18.0.1:25"); err != nil || result != "public-tcp" {
			t.Fatal("external namespace TCP forwarding failed", err, result)
		}
		code := "import socket; s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(3); s.sendto(b'public-udp',('198.18.0.1',7882)); assert s.recv(1024)==b'public-udp'"
		if _, err := command(ctx, "", nil, "ip", "netns", "exec", "ports-client", "python3", "-c", code); err != nil {
			t.Fatal("external namespace UDP forwarding failed", err)
		}
	}
	assertPublicPorts()
	if os.Getenv("TEST_PUBLIC_PORTS") == "1" {
		name := "lidza-unrelated-public-" + newID()[:8]
		if _, err := command(ctx, "", nil, "docker", "run", "-d", "--name", name, "--publish", "0.0.0.0:16379:6379", cacheImage); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = command(context.Background(), "", nil, "docker", "rm", "-f", name) })
		// The service is reachable locally, but the external namespace remains
		// blocked by its operator rule: no wildcard/container-network allowance.
		if _, err := command(ctx, "", nil, "python3", "-c", "import socket; s=socket.create_connection(('127.0.0.1',16379),3); s.close()"); err != nil {
			t.Fatal("unrelated service was not listening", err)
		}
		code := "import socket; s=socket.socket(); s.settimeout(1); result=s.connect_ex(('198.18.0.1',16379)); assert result!=0; s.close()"
		if _, err := command(ctx, "", nil, "ip", "netns", "exec", "ports-client", "python3", "-c", code); err != nil {
			t.Fatal("undeclared public port was broadened", err)
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
	if !r1.Stack.Bootstrap {
		t.Fatal("first-install release was mislabeled as fully ready")
	}
	for _, path := range []string{"/readyz", "/protected"} {
		target := proxy.URL + path
		if path == "/readyz" {
			target = "http://127.0.0.1:" + r1.Port + path
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
		req.Host = a.Domain
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 503 {
			t.Fatalf("setup API guard bypassed for %s: %d", path, res.StatusCode)
		}
	}
	if !strings.Contains(request("GET", "/setup"), "app") {
		t.Fatal("guarded setup UI unavailable")
	}
	if request("POST", "/setup/publish") != "published" {
		t.Fatal("setup publication failed")
	}
	if request("GET", "/rtc/room") != "rtc:/room" {
		t.Fatal("SFU prefix routing failed")
	}
	if request("POST", "/persist") != "preserved" {
		t.Fatal("storage attachment failed")
	}
	if manifest.Version == 2 {
		exerciseStackV2Selection(t, ctx, m, a.ID, proxy.URL, a.Domain, request, deploy, &releases)
		r1 = m.Current(a.ID)
	}
	// A host-owned RTC port must cause a clean failed candidate, never eviction.
	listener, listenErr := net.Listen("tcp", "127.0.0.1:7881")
	if listenErr != nil {
		t.Fatal("isolated RTC test port occupied", listenErr)
	}
	conflict := fixtureStackManifest()
	conflict.Services[0].PublicPorts = []StackPort{{Port: 7881, Protocol: "tcp"}}
	m.mu.Lock()
	liveApp := m.data.Apps[a.ID]
	liveApp.Networks = []string{m.data.Databases[a.ID].Network}
	liveApp.StorageVolume = m.data.StorageVolumes[a.ID]
	m.mu.Unlock()
	_, conflictErr := rt.runStack(ctx, liveApp, newID(), r1.Commit, conflict, stackImages(r1))
	listener.Close()
	if conflictErr == nil {
		t.Fatal("host port conflict was ignored")
	}
	m.reconcileRoutes()
	assertGateway()
	if request("GET", "/persist") != "preserved" {
		t.Fatal("port conflict failed to recover current stack")
	}
	inventory := m.managedContainers()
	sidecars := 0
	for _, c := range inventory {
		if c.AppID == a.ID && strings.HasPrefix(c.Role, "Stack") {
			sidecars++
		}
	}
	sample := m.containerInventory(ctx)
	healthy := 0
	for _, c := range sample.Containers {
		if strings.HasPrefix(c.Role, "Stack ·") && c.Health == "healthy" {
			healthy++
		}
	}
	if healthy != expectedServices {
		t.Fatalf("sidecar health unavailable: %+v", sample)
	}
	expectedInventory := expectedServices
	m.mu.Lock()
	if previous := m.data.Apps[a.ID].Previous; previous != nil && previous.Stack != nil {
		expectedInventory += len(previous.Stack.Containers)
	}
	m.mu.Unlock()
	if sidecars != expectedInventory {
		t.Fatalf("inventory missing sidecars: %d", sidecars)
	}
	// A new checkout after publication must accept database-owned settings too.
	second := deploy(false)
	if second.Status != "live" {
		t.Fatalf("established redeployment: %s\n%s", second.Error, second.Log)
	}
	r2 := m.Current(a.ID)
	if r2.Stack.Bootstrap {
		t.Fatal("published setup did not transition to genuine readiness")
	}
	releases = append(releases, r2)
	if request("GET", "/persist") != "preserved" {
		t.Fatal("reload lost storage")
	}
	assertPublicPorts()
	if os.Getenv("TEST_STACK_GATEWAY") == "1" {
		m.mu.Lock()
		broken := m.data.Apps[a.ID]
		broken.Env["FIXTURE_GATEWAY_FAIL"] = "true"
		m.data.Apps[a.ID] = broken
		m.mu.Unlock()
		failedTLS := deploy(false)
		if failedTLS.Status != "failed" || !strings.Contains(failedTLS.Error, "private app mTLS health") || m.Current(a.ID).ID != r2.ID {
			t.Fatalf("broken private TLS candidate did not retain previous release: %s %s", failedTLS.Status, failedTLS.Error)
		}
		m.mu.Lock()
		fixed := m.data.Apps[a.ID]
		delete(fixed.Env, "FIXTURE_GATEWAY_FAIL")
		m.data.Apps[a.ID] = fixed
		m.mu.Unlock()
		m.reconcileRoutes()
		assertGateway()
	}
	// Simulate process death during a rollback after the old writers stopped.
	if err := m.recordStackTransition(a.ID, r2, r1, true); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if os.Getenv("TEST_PUBLIC_PORTS") == "1" {
		if _, err := command(ctx, "", nil, "iptables", "-w", "5", "-D", "DOCKER-USER", "-m", "comment", "--comment", "lidza-public-owner-v1", "-j", "LIDZA-PUBLIC"); err != nil {
			t.Fatal(err)
		}
	}
	if err := rt.setReleaseRunning(ctx, r2, false); err != nil {
		t.Fatal(err)
	}
	if err := rt.setReleaseRunning(ctx, r1, true); err != nil {
		t.Fatal(err)
	}
	recoveredRuntime := &Docker{Root: rt.Root, checkout: rt.checkout, readinessTimeout: 10 * time.Second}
	recovered, err := NewManager(context.Background(), m.cfg, recoveredRuntime)
	if err != nil {
		t.Fatal("crash recovery", err)
	}
	m = recovered
	proxy.Close()
	proxy = httptest.NewServer(Proxy(m))
	defer proxy.Close()
	rt = recoveredRuntime
	t.Cleanup(m.Close)
	m.reconcileRoutes()
	if request("GET", "/persist") != "preserved" {
		t.Fatal("agent restart lost the active stack")
	}
	m.mu.Lock()
	pending := len(m.data.StackTransitions)
	m.mu.Unlock()
	if pending != 0 {
		t.Fatal("successful crash recovery left a transition pending")
	}
	if err := m.Rollback(ctx, a.ID); err != nil {
		t.Fatal("rollback", err)
	}
	assertGateway()
	if m.Current(a.ID).ID != r1.ID {
		t.Fatal("rollback target not activated")
	}
	if request("GET", "/rtc/room") != "rtc:/room" {
		t.Fatal("rollback SFU route stale")
	}
	assertPublicPorts()
	m.mu.Lock()
	bad := m.data.Apps[a.ID]
	bad.Env["FIXTURE_RESET_SETUP"] = "true"
	m.data.Apps[a.ID] = bad
	m.mu.Unlock()
	failed := deploy(false)
	if failed.Status != "failed" || !strings.Contains(failed.Error, "failed app/service readiness") || !strings.Contains(failed.Log, "Server setup is required") {
		t.Fatalf("established setup regression was not rejected at runtime readiness: %s\n%s", failed.Error, failed.Log)
	}
	// Restarting Docker containers can assign new ephemeral host ports; use
	// the reconciled route and wait for ordinary readiness after restart.
	m.reconcileRoutes()
	if m.Current(a.ID).ID != r1.ID {
		t.Fatal("established failure replaced the prior release")
	}
	if err := rt.waitReady(ctx, m.Current(a.ID)); err != nil {
		t.Fatal("restored prior release did not regain genuine readiness", err)
	}
	if err := rt.Ready(ctx, m.Current(a.ID)); err != nil {
		t.Fatal("restored sidecar health failed", err)
	}
	if request("GET", "/persist") != "preserved" {
		t.Fatal("failure did not restore old stack")
	}
	assertPublicPorts()
	m.mu.Lock()
	bad = m.data.Apps[a.ID]
	delete(bad.Env, "FIXTURE_RESET_SETUP")
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
	if tlsFixture != nil {
		if _, err := os.Stat(filepath.Join(m.serviceTLS.root, tlsFixture.b.ResourceID)); !os.IsNotExist(err) {
			t.Fatal("retire retained copied public keys", err)
		}
	}
	if os.Getenv("TEST_PUBLIC_PORTS") == "1" {
		rules, err := command(ctx, "", nil, "iptables", "-S", "LIDZA-PUBLIC")
		if err != nil || strings.Contains(rules, "-j ACCEPT") {
			t.Fatal("removal retained public allow rules", err, rules)
		}
		operator, err := command(ctx, "", nil, "iptables", "-S", "DOCKER-USER")
		if err != nil || !strings.Contains(operator, "fixture-deny-client") {
			t.Fatal("operator firewall rule changed", err)
		}
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

func TestDockerStackCleanupPreservesUnmanagedCollisions(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for cleanup ownership acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner := "cleanup-" + newID()[:10]
	id := newID()
	network := "lidza-stack-" + owner + "-" + id
	container := "lidza-" + owner + "-" + id + "-office"
	if _, err := command(ctx, "", nil, "docker", "network", "create", "--label", "io.lidza.stack=unrelated-fixture", network); err != nil {
		t.Fatal(err)
	}
	defer command(context.Background(), "", nil, "docker", "network", "rm", network)
	if _, err := command(ctx, "", nil, "docker", "create", "--name", container, "--label", "io.lidza.stack=unrelated-fixture", "postgres:17-alpine"); err != nil {
		t.Fatal(err)
	}
	defer command(context.Background(), "", nil, "docker", "rm", "-f", container)
	rt := &Docker{Root: t.TempDir()}
	r := &Release{ID: id, Stack: &StackRelease{Network: network, Containers: []StackContainer{{Container: container, Image: "postgres:17-alpine", Service: StackService{Name: "office"}}}}}
	if err := rt.Remove(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"container", "network"} {
		name := container
		if kind == "network" {
			name = network
		}
		args := []string{kind, "ls", "--filter", "name=" + name, "--format", "{{.ID}}"}
		if kind == "container" {
			args = append(args, "--all")
		}
		out, err := command(ctx, "", nil, "docker", args...)
		if err != nil || out == "" {
			t.Fatal("cleanup removed an unmanaged collision", kind, err)
		}
	}
}

func TestDockerStackInitSignalsDroppedUID(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for supervisor capability acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	script := `gosu 65532:65532 sh -ec 'touch /tmp/dropped; exec sleep 30' & child=$!; while [ ! -f /tmp/dropped ]; do sleep 0.05; done; `
	for _, allowed := range []bool{false, true} {
		args := []string{"run", "--rm", "--network", "none", "--memory", "64m", "--pids-limit", "32", "--tmpfs", "/tmp:rw,nosuid,size=4m"}
		policy := stackPolicyArgs(StackService{Role: "app", Policy: "init"})
		for i := 0; i < len(policy); i++ {
			if !allowed && policy[i] == "--cap-add" && i+1 < len(policy) && policy[i+1] == "KILL" {
				i++
				continue
			}
			args = append(args, policy[i])
		}
		check := script + `if kill -0 "$child" 2>/dev/null; then exit 1; fi`
		if allowed {
			check = script + `kill -0 "$child"; kill -TERM "$child"; wait "$child" || true`
		}
		args = append(args, "--entrypoint", "sh", "postgres:17-alpine", "-ec", check)
		if _, err := command(ctx, "", nil, "docker", args...); err != nil {
			t.Fatalf("cross-UID supervisor signal capability (KILL=%v): %v", allowed, err)
		}
	}
}
