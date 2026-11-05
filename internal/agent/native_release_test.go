package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

func decodeReleaseJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatal(err, body)
	}
}

// nativeReleaseFixture registers a Rails-shaped native application whose
// current release has the conventional timestamp layout, with a local git
// source and fakes for the owner's systemctl, crontab and health endpoint.
type nativeReleaseFixture struct {
	m        *Manager
	app      NativeApplication
	root     string
	original string
	source   string
	commit   func(file, body string) string
	units    *fakeUserUnits
	crontab  *fakeCrontab
	health   *httptest.Server
}

type fakeUserUnits struct {
	mu        sync.Mutex
	states    map[string]string // unit -> ActiveState/SubState
	restarted []string
}

func (f *fakeUserUnits) run(_ context.Context, c release.Command) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	args := c.Args
	if len(args) < 4 || args[0] != "systemctl" || args[1] != "--user" {
		return errors.New("unexpected control command: " + strings.Join(args, " "))
	}
	for _, entry := range c.Env {
		if strings.HasPrefix(entry, "XDG_RUNTIME_DIR=") && !strings.HasPrefix(entry, "XDG_RUNTIME_DIR=/run/user/") {
			return errors.New("missing runtime dir")
		}
	}
	switch args[3] {
	case "show":
		unit := args[4]
		state, ok := f.states[unit]
		if !ok {
			state = "inactive/dead"
		}
		active, sub, _ := strings.Cut(state, "/")
		file := "enabled"
		if active == "inactive" {
			file = "disabled"
		}
		if c.Stdout != nil {
			c.Stdout.Write([]byte("ActiveState=" + active + "\nSubState=" + sub + "\nUnitFileState=" + file + "\n"))
		}
		return nil
	case "restart":
		f.restarted = append(f.restarted, args[4])
		return nil
	}
	return errors.New("unexpected systemctl verb " + args[3])
}

type fakeCrontab struct {
	mu   sync.Mutex
	body string
	fail bool
}

func (f *fakeCrontab) run(_ context.Context, stdin string, args []string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch strings.Join(args, " ") {
	case "-u agim -l":
		if f.body == "" {
			return "", "no crontab for agim\n", errors.New("exit status 1")
		}
		return f.body, "", nil
	case "-u agim -":
		if f.fail {
			return "", "bad minute\n", errors.New("exit status 1")
		}
		f.body = stdin
		return "", "", nil
	}
	return "", "", errors.New("unexpected crontab arguments")
}

func newNativeReleaseFixture(t *testing.T) *nativeReleaseFixture {
	t.Helper()
	m := testManager(t, &fakeRuntime{})
	home := t.TempDir()
	root := filepath.Join(home, "MyRails")
	original := "20260101000000"
	releaseDir := filepath.Join(root, "releases", original)
	for _, dir := range []string{releaseDir, filepath.Join(root, "shared"), filepath.Join(home, ".config", "systemd", "user")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(releaseDir, "Gemfile"):   "source 'https://rubygems.org'\n",
		filepath.Join(releaseDir, "REVISION"):  strings.Repeat("0", 40) + "\n",
		filepath.Join(root, ".env.production"): "export SECRET_KEY_BASE='keep-me'\nDATABASE_URL=postgres://localhost/app\nPATH=/evil\nLD_PRELOAD=/evil.so\n",
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("releases", original), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	units := filepath.Join(home, ".config", "systemd", "user")
	for _, unit := range []string{"myrails-server.service", "myrails-worker.service", "myrails-monitor.service"} {
		body := "[Service]\nWorkingDirectory=" + filepath.Join(root, "current") + "\nEnvironmentFile=-" + filepath.Join(root, ".env.production") + "\nExecStart=/app/private-command\n"
		if err := os.WriteFile(filepath.Join(units, unit), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m.cfg.Native = &NativeHostConfig{Root: home, User: "agim", Releases: map[string]NativeReleasePolicy{}}
	app, err := m.nativeRegistration(NativeApplication{ID: "rails-myrails", Name: "MyRails", Repository: "acme/myrails", Branch: "main", Domain: "myrails.example.com", Runtime: "rails"})
	if err != nil {
		t.Fatal(err)
	}
	// Local source repository standing in for GitHub.
	work := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1", "HOME="+work)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "--initial-branch=main")
	commit := func(file, body string) string {
		if err := os.WriteFile(filepath.Join(work, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "--quiet", "-m", file)
		return git("rev-parse", "HEAD")
	}
	health := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "myrails.example.com" || r.URL.Path != "/up" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(health.Close)
	f := &nativeReleaseFixture{m: m, app: app, root: root, original: original, source: "file://" + work, commit: commit,
		units:   &fakeUserUnits{states: map[string]string{"myrails-server.service": "active/running", "myrails-worker.service": "active/running"}},
		crontab: &fakeCrontab{}, health: health}
	m.nativeReleaseRun = release.LocalExecutor{}.Run
	m.nativeReleaseUserRun = f.units.run
	m.nativeCrontabCommand = f.crontab.run
	m.nativeReleaseSource = func(NativeReleasePolicy, string) release.Source { return release.Source{URL: f.source} }
	m.nativeReleaseHealthAddress = strings.TrimPrefix(health.URL, "https://")
	m.nativeReleaseHealthTLS = &tls.Config{InsecureSkipVerify: true, ServerName: "myrails.example.com"}
	return f
}

func (f *nativeReleaseFixture) policy() NativeReleasePolicy {
	return NativeReleasePolicy{Enabled: true, Root: f.root, Repository: "acme/myrails", Branch: "main", Runtime: "node",
		Domain: "myrails.example.com", Units: []string{"myrails-server.service", "myrails-worker.service"},
		Preserve: []string{"myrails-monitor.service"}, EnvFiles: []string{filepath.Join(f.root, ".env.production")}, Keep: 3}
}

func (f *nativeReleaseFixture) call(t *testing.T, handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.SetPathValue("id", f.app.ID)
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func TestNativeReleasePolicyMustDescribeTheRegisteredApplication(t *testing.T) {
	f := newNativeReleaseFixture(t)
	if _, _, err := f.m.nativeReleasePolicy(f.app.ID); err == nil {
		t.Fatal("release management accepted without a policy")
	}
	if w := f.call(t, f.m.nativeReleaseRoute(false), ""); w.Code != 409 {
		t.Fatal("release route accepted without a policy", w.Code, w.Body.String())
	}
	for name, mutate := range map[string]func(*NativeReleasePolicy){
		"disabled":   func(p *NativeReleasePolicy) { p.Enabled = false },
		"repository": func(p *NativeReleasePolicy) { p.Repository = "acme/other" },
		"branch":     func(p *NativeReleasePolicy) { p.Branch = "release" },
		"root":       func(p *NativeReleasePolicy) { p.Root = filepath.Join(filepath.Dir(f.root), "Other") },
		"domain":     func(p *NativeReleasePolicy) { p.Domain = "other.example.com" },
		"overlap":    func(p *NativeReleasePolicy) { p.Preserve = []string{"myrails-server.service"} },
		"keep-one":   func(p *NativeReleasePolicy) { p.Keep = 1 },
	} {
		policy := f.policy()
		mutate(&policy)
		f.m.cfg.Native.Releases[f.app.ID] = policy
		if _, _, err := f.m.nativeReleasePolicy(f.app.ID); err == nil {
			t.Fatal("policy accepted with mismatch:", name)
		}
	}
	f.m.cfg.Native.Releases[f.app.ID] = f.policy()
	if _, _, err := f.m.nativeReleasePolicy(f.app.ID); err != nil {
		t.Fatal(err)
	}
	status, err := f.m.nativeStatus(context.Background(), f.app.ID)
	if err != nil || !status.ReleaseManaged || !status.DeploymentSupported {
		t.Fatal("status does not advertise release management", err, status.ReleaseManaged)
	}
	// The panel enables Deploy, Rollback and auto-deploy from these capabilities.
	if c := f.m.nativeManagementCapabilities(f.app); !c.Deploy || !c.Rollback || !c.Logs || !c.Configure || !c.Environment || c.Routing == nil || *c.Routing {
		t.Fatalf("release-managed capabilities %+v", c)
	}
	// A branch saved from the panel is followed by the release policy.
	f.m.mu.Lock()
	f.m.data.NativeSettings = map[string]NativeSettingsOverride{f.app.ID: {Branch: "staging"}}
	f.m.mu.Unlock()
	if _, policy, err := f.m.nativeReleasePolicy(f.app.ID); err != nil || policy.Branch != "staging" {
		t.Fatal("saved branch not followed", policy.Branch, err)
	}
}

func TestNativeReleaseDeploysAndRollsBackThroughOwnerUnits(t *testing.T) {
	f := newNativeReleaseFixture(t)
	f.m.cfg.Native.Releases[f.app.ID] = f.policy()
	first := f.commit("VERSION", "one\n")
	w := f.call(t, f.m.nativeReleaseRoute(false), `{"key":"delivery-1"}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var queued Deployment
	decodeReleaseJSON(t, w.Body.String(), &queued)
	if again := f.call(t, f.m.nativeReleaseRoute(false), `{"key":"delivery-1"}`); again.Code != 202 || !strings.Contains(again.Body.String(), queued.ID) {
		t.Fatal("delivery key not deduplicated", again.Code, again.Body.String())
	}
	done := waitDeployment(t, f.m, queued.ID)
	if done.Status != "live" || done.Commit != first || done.Kind != "deploy" {
		t.Fatalf("deployment %+v\n%s", done, done.Log)
	}
	layout := release.Layout{Root: f.root}
	current, err := layout.CurrentID()
	if err != nil || current == f.original || !release.ValidID(current) {
		t.Fatal("current was not republished", current, err)
	}
	if revision, _ := layout.Revision(current); revision != first {
		t.Fatal("REVISION mismatch", revision)
	}
	if !slices.Equal(f.units.restarted, []string{"myrails-server.service", "myrails-worker.service"}) {
		t.Fatal("restart order", f.units.restarted)
	}
	if strings.Contains(done.Log, "keep-me") || strings.Contains(done.Log, "postgres://") {
		t.Fatal("deployment log leaked environment values")
	}
	view, err := f.m.nativeReleases(f.app.ID)
	if err != nil || view.Current != current || view.Previous != f.original || len(view.Releases) != 2 {
		t.Fatalf("release view %+v %v", view, err)
	}
	if history := f.m.data.NativeReleases[f.app.ID]; history.Current.ID != current || history.Previous.ID != f.original {
		t.Fatal("dashboard history not recorded", history)
	}
	// An exact commit of the deployed branch is accepted; a foreign ref is not.
	if w := f.call(t, f.m.nativeReleaseRoute(false), `{"ref":"feature/x"}`); w.Code != 409 {
		t.Fatal("foreign ref accepted", w.Code)
	}
	f.units.restarted = nil
	w = f.call(t, f.m.nativeReleaseRoute(true), "")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	decodeReleaseJSON(t, w.Body.String(), &queued)
	done = waitDeployment(t, f.m, queued.ID)
	if done.Status != "live" || done.Kind != "rollback" || done.Commit != strings.Repeat("0", 40) {
		t.Fatalf("rollback %+v\n%s", done, done.Log)
	}
	if current, _ := layout.CurrentID(); current != f.original {
		t.Fatal("rollback did not restore the original release", current)
	}
	if len(f.units.restarted) != 2 {
		t.Fatal("rollback did not restart running units", f.units.restarted)
	}
}

func TestNativeReleaseArgvConfinesEachCommandKind(t *testing.T) {
	account := nativeReleaseAccount{Name: "deploy", Home: "/home/deploy", UID: 1000, GID: 1000}
	base := release.Command{Name: "x", Dir: "/home/deploy/app/releases/1", Args: []string{"true"}}
	offline := strings.Join(nativeReleaseArgv(account, "/home/deploy/app", "u.service", "/env", base), "\n")
	network := base
	network.Network = true
	online := strings.Join(nativeReleaseArgv(account, "/home/deploy/app", "u.service", "/env", network), "\n")
	database := network
	database.Database = true
	migrate := strings.Join(nativeReleaseArgv(account, "/home/deploy/app", "u.service", "/env", database), "\n")
	databaseOnly := base
	databaseOnly.Database = true
	precompile := strings.Join(nativeReleaseArgv(account, "/home/deploy/app", "u.service", "/env", databaseOnly), "\n")
	if strings.Contains(precompile, "PrivateNetwork") || strings.Contains(precompile, "IPAddressDeny") || strings.Contains(precompile, "/run/postgresql") {
		t.Fatal("database step without network flag must still reach TCP databases", precompile)
	}
	for name, argv := range map[string]string{"offline": offline, "online": online, "migrate": migrate} {
		for _, want := range []string{"--property=ReadWritePaths=/home/deploy/app", "--property=ProtectSystem=strict", "--reuid=1000", "--regid=1000",
			"--bounding-set=-all", "--no-new-privs", "--property=NoNewPrivileges=yes", "--property=WorkingDirectory=/home/deploy/app/releases/1", "/usr/bin/env\n-i\n"} {
			if !strings.Contains(argv, want) {
				t.Fatalf("%s lacks %q", name, want)
			}
		}
	}
	if !strings.Contains(offline, "PrivateNetwork=yes") || !strings.Contains(offline, "-/run/postgresql") {
		t.Fatal("offline build is not isolated from network and databases")
	}
	if strings.Contains(online, "PrivateNetwork") || !strings.Contains(online, "IPAddressDeny=localhost") || !strings.Contains(online, "IPAddressAllow=127.0.0.53/32") || !strings.Contains(online, "-/run/postgresql") {
		t.Fatal("package install must reach the internet but not local services")
	}
	if strings.Contains(migrate, "PrivateNetwork") || strings.Contains(migrate, "IPAddressDeny") || strings.Contains(migrate, "/run/postgresql") {
		t.Fatal("database step must reach databases")
	}
	if nativeReleaseInside("/home/deploy/app", "/home/deploy/app2") || !nativeReleaseInside("/home/deploy/app", "/home/deploy/app/repo") {
		t.Fatal("root containment")
	}
}

func TestNativeReleaseEnvironmentDropsReservedKeysAndRequiresLiterals(t *testing.T) {
	account := nativeReleaseAccount{Name: "deploy", Home: "/home/deploy", UID: 1000, GID: 1000}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.env")
	os.WriteFile(good, []byte("export A='1'\nB=two\nPATH=/evil\nLD_PRELOAD=/x\nBUNDLE_GEMFILE=/old/Gemfile\n"), 0o600)
	env, err := nativeReleaseEnvironment(account, NativeReleasePolicy{EnvFiles: []string{good}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"A=1", "B=two", "PATH=/home/deploy/.asdf/shims:", "HOME=/home/deploy", "USER=deploy", "LANG=C.UTF-8"} {
		if !strings.Contains(joined, want) {
			t.Fatal("missing", want, joined)
		}
	}
	if strings.Contains(joined, "/evil") || strings.Contains(joined, "LD_PRELOAD") || strings.Contains(joined, "BUNDLE_GEMFILE") {
		t.Fatal("reserved key leaked", joined)
	}
	bad := filepath.Join(dir, "bad.env")
	os.WriteFile(bad, []byte("A=$(id)\n"), 0o600)
	if _, err := nativeReleaseEnvironment(account, NativeReleasePolicy{EnvFiles: []string{bad}}); err == nil {
		t.Fatal("shell expansion accepted")
	}
	if _, err := nativeReleaseEnvironment(account, NativeReleasePolicy{EnvFiles: []string{filepath.Join(dir, "missing.env")}}); err == nil {
		t.Fatal("missing environment file accepted")
	}
}

func TestNativeReleaseCrontabKeepsForeignEntries(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	fake := &fakeCrontab{}
	m.nativeCrontabCommand = fake.run
	tab := nativeReleaseCrontab{m: m, user: "agim", identifier: "app"}
	ctx := context.Background()
	if body, err := tab.Read(ctx); err != nil || body != "" {
		t.Fatal("missing crontab must read as empty", body, err)
	}
	fake.body = "MAILTO=\"\"\n0 1 * * * /usr/bin/backup\n# Begin Whenever generated tasks for: app\nold\n# End Whenever generated tasks for: app\n"
	next, err := release.ReplaceScheduleBlock(fake.body, "app", "*/5 * * * * /bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if err := tab.Write(ctx, next); err != nil || !strings.Contains(fake.body, "/usr/bin/backup") || !strings.Contains(fake.body, "/bin/true") || strings.Contains(fake.body, "old") {
		t.Fatal("write", err, fake.body)
	}
	if err := tab.Write(ctx, "# Begin Whenever generated tasks for: app\nx\n# End Whenever generated tasks for: app\n"); err == nil {
		t.Fatal("write dropping foreign entries was accepted")
	}
	if changed, err := release.SyncSchedule(ctx, tab, "app", ""); err != nil || !changed || strings.Contains(fake.body, "for: app") || !strings.Contains(fake.body, "/usr/bin/backup") {
		t.Fatal("block removal", changed, err, fake.body)
	}
	fake.fail = true
	if err := tab.Write(ctx, fake.body+"\n# Begin Whenever generated tasks for: app\ny\n# End Whenever generated tasks for: app\n"); err == nil {
		t.Fatal("crontab failure ignored")
	}
}

func TestNativeReleasePreservedUnitsAreNeverRestarted(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	units := &fakeUserUnits{states: map[string]string{"app-monitor.service": "active/running"}}
	m.nativeReleaseUserRun = units.run
	services := m.nativeReleaseServices(nativeReleaseAccount{Name: "agim", Home: "/home/agim", UID: 1000, GID: 1000},
		NativeReleasePolicy{Preserve: []string{"app-monitor.service"}})
	if err := services.Restart(context.Background(), "app-monitor.service"); err == nil || len(units.restarted) != 0 {
		t.Fatal("preserved unit restarted")
	}
	if state, err := services.State(context.Background(), "app-monitor.service"); err != nil || !state.Running() {
		t.Fatal("state read failed", state, err)
	}
}

func TestNativeReleaseBackupFailsClosedWithoutDeclaredDatabases(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if err := m.nativeReleaseBackup("rails-x", NativeReleasePolicy{})(context.Background()); err == nil {
		t.Fatal("release steps allowed without declared databases")
	}
	if err := m.nativeReleaseBackup("rails-x", NativeReleasePolicy{Databases: []string{"db"}})(context.Background()); err == nil {
		t.Fatal("undeclared database binding accepted")
	}
	m.data.NativeDatabases = map[string]NativeDatabaseBinding{"db": {Policy: NativeDatabasePolicy{AppID: "rails-x", Roles: []string{"primary"}}}}
	var gotRoles []string
	m.nativeBackupRun = func(_ context.Context, appID string, roles []string) ([]NativeDatabaseBackupProof, error) {
		gotRoles = roles
		return nil, nil
	}
	if err := m.nativeReleaseBackup("rails-x", NativeReleasePolicy{Databases: []string{"db"}})(context.Background()); err != nil || !slices.Equal(gotRoles, []string{"primary"}) {
		t.Fatal("backup gate not called with the bound roles", err, gotRoles)
	}
}

func TestNativeReleaseHealthProbesThroughLocalIngress(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	var hosts []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.Host+r.URL.Path)
		if r.URL.Path == "/down" {
			w.WriteHeader(503)
		}
	}))
	defer server.Close()
	m.nativeReleaseHealthAddress = strings.TrimPrefix(server.URL, "https://")
	m.nativeReleaseHealthTLS = &tls.Config{InsecureSkipVerify: true}
	if m.nativeReleaseHealth("", "/up") != nil {
		t.Fatal("health without a domain must be skipped")
	}
	if err := m.nativeReleaseHealth("app.example.com", "")(context.Background()); err != nil || !slices.Contains(hosts, "app.example.com/up") {
		t.Fatal("health probe", err, hosts)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.nativeReleaseHealth("app.example.com", "/down")(ctx); err == nil {
		t.Fatal("503 accepted")
	}
}

func TestNativeReleaseJobTemplateLoadsEnvironmentFiles(t *testing.T) {
	got, err := nativeReleaseJobTemplate([]string{"/home/deploy/app/.env", "/home/deploy/.vars"})
	if err != nil || got != "/bin/bash -l -c 'set -a; . /home/deploy/app/.env; . /home/deploy/.vars; set +a; :job'" {
		t.Fatal(got, err)
	}
	if got, err := nativeReleaseJobTemplate(nil); err != nil || got != "/bin/bash -l -c ':job'" {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"relative/.env", "/home/deploy/app/.env'; rm -rf /", "/home/deploy/a b", "/x&y=z"} {
		if _, err := nativeReleaseJobTemplate([]string{bad}); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	m := &Manager{}
	profile, err := m.nativeReleaseProfile(NativeReleasePolicy{Runtime: "rails", EnvFiles: []string{"/home/deploy/app/.env"}}, release.Layout{Root: t.TempDir()})
	if err != nil || profile.Schedule == nil {
		t.Fatal(err)
	}
	step := profile.Schedule(release.Layout{Root: "/home/deploy/app"})
	joined := strings.Join(step.Args, " ")
	if !strings.Contains(joined, "job_template=/bin/bash -l -c 'set -a; . /home/deploy/app/.env; set +a; :job'") || !strings.Contains(joined, "path=/home/deploy/app/current") {
		t.Fatal(joined)
	}
}

// TestNativeReleaseExecutorPropertiesParse feeds every transient-unit
// property the executor emits to a real systemd-run, so a value systemd
// refuses to parse (as "IPAddressDeny=localhost link-local" once was) fails
// here instead of on the host. Only parse failures count: the user manager
// may legitimately refuse to start some system-only properties.
func TestNativeReleaseExecutorPropertiesParse(t *testing.T) {
	if _, err := exec.LookPath("systemd-run"); err != nil || os.Getenv("XDG_RUNTIME_DIR") == "" {
		t.Skip("needs systemd-run and a user manager")
	}
	if err := exec.Command("systemd-run", "--user", "--quiet", "--wait", "--collect", "/bin/true").Run(); err != nil {
		t.Skip("user manager unavailable:", err)
	}
	account := nativeReleaseAccount{Name: "deploy", UID: 1000, GID: 1000, Home: "/home/deploy"}
	base := release.Command{Name: "build", Dir: "/home/deploy/app/releases/1", Args: []string{"true"}}
	network := base
	network.Network = true
	database := base
	database.Database = true
	seen := map[string]bool{}
	for _, command := range []release.Command{base, network, database} {
		for _, arg := range nativeReleaseArgv(account, "/home/deploy/app", "u.service", "/env", command) {
			property, ok := strings.CutPrefix(arg, "--property=")
			if !ok || seen[property] {
				continue
			}
			seen[property] = true
			out, _ := exec.Command("systemd-run", "--user", "--quiet", "--wait", "--collect", "-p", property, "/bin/true").CombinedOutput()
			if strings.Contains(string(out), "Failed to parse") || strings.Contains(string(out), "Invalid argument") {
				t.Errorf("systemd refuses property %q: %s", property, strings.TrimSpace(string(out)))
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("executor emitted no properties")
	}
}

func TestNativeReleaseSettingsEditEnvironmentAndBranch(t *testing.T) {
	f := newNativeReleaseFixture(t)
	f.m.cfg.Native.Releases[f.app.ID] = f.policy()
	envFile := filepath.Join(f.root, ".env.production")
	if err := os.Chmod(envFile, 0o640); err != nil {
		t.Fatal(err)
	}
	settings, err := f.m.nativeSettings(f.app.ID)
	if err != nil || settings.Capabilities == nil || !settings.Capabilities.Configure || !settings.Capabilities.Environment || !settings.Capabilities.Deploy {
		t.Fatalf("release-managed settings %+v %v", settings.Capabilities, err)
	}
	if !slices.Contains(settings.EnvKeys, "SECRET_KEY_BASE") || !slices.Contains(settings.ReadonlyEnvKeys, "PATH") || slices.Contains(settings.ReadonlyEnvKeys, "SECRET_KEY_BASE") {
		t.Fatalf("environment view keys=%v readonly=%v", settings.EnvKeys, settings.ReadonlyEnvKeys)
	}
	value := func(s string) *string { return &s }
	err = f.m.patchNativeSettings(f.app.ID, SettingsPatch{Domain: f.app.Domain, EnvChanges: map[string]*string{
		"SECRET_KEY_BASE": value("rotated"), "GREETING": value("hello world"), "DATABASE_URL": nil}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(envFile)
	got := string(raw)
	if !strings.Contains(got, "SECRET_KEY_BASE=rotated\n") || !strings.Contains(got, "GREETING='hello world'\n") || strings.Contains(got, "DATABASE_URL") || !strings.Contains(got, "PATH=/evil\n") {
		t.Fatalf("environment file after edit:\n%s", got)
	}
	if info, _ := os.Stat(envFile); info.Mode().Perm() != 0o640 {
		t.Fatal("environment file mode changed", info.Mode().Perm())
	}
	values, err := nativeReleaseEnvValues(f.m.cfg.Native.Releases[f.app.ID])
	if err != nil || values["GREETING"] != "hello world" || values["SECRET_KEY_BASE"] != "rotated" {
		t.Fatal("edited file does not parse back", values, err)
	}
	for name, patch := range map[string]SettingsPatch{
		"host key":   {EnvChanges: map[string]*string{"PATH": value("/tmp")}},
		"port":       {EnvChanges: map[string]*string{"PORT": value("1")}},
		"newline":    {EnvChanges: map[string]*string{"BAD": value("a\nb")}},
		"quote":      {EnvChanges: map[string]*string{"BAD": value("it's")}},
		"hostname":   {Domain: "other.example"},
		"no backup":  {BackupBeforeDeploy: new(bool)},
		"bad branch": {Branch: "-x"},
	} {
		if err := f.m.patchNativeSettings(f.app.ID, patch); err == nil {
			t.Fatal("accepted", name)
		}
	}
	if err := f.m.patchNativeSettings(f.app.ID, SettingsPatch{Branch: "staging"}); err != nil {
		t.Fatal(err)
	}
	_, policy, err := f.m.nativeReleasePolicy(f.app.ID)
	if err != nil || policy.Branch != "staging" {
		t.Fatal("saved branch is not followed by the release policy", policy.Branch, err)
	}
	if settings, _ := f.m.nativeSettings(f.app.ID); settings.Branch != "staging" {
		t.Fatal("settings do not show the saved branch", settings.Branch)
	}
}

func TestNativeReleaseSettingsApplyWorkspaceDefaults(t *testing.T) {
	f := newNativeReleaseFixture(t)
	f.m.cfg.Native.Releases[f.app.ID] = f.policy()
	envFile := filepath.Join(f.root, ".env.production")
	value := func(s string) *string { return &s }
	settings, err := f.m.nativeSettings(f.app.ID)
	if err != nil || settings.ReleaseRuntime != "node" { // the fixture policy runs the Node.js profile
		t.Fatal("release runtime not reported", settings.ReleaseRuntime, err)
	}
	if err := f.m.patchNativeSettings(f.app.ID, SettingsPatch{EnvChanges: map[string]*string{"RAILS_ENV": value("staging"), "EMPTY_SETTING": value("")}}); err != nil {
		t.Fatal(err)
	}
	// Missing-only keeps every key already in the environment files, including
	// empty assignments, and appends only the missing ones.
	err = f.m.patchNativeSettings(f.app.ID, SettingsPatch{FromDefaults: true, OnlyMissing: true, EnvChanges: map[string]*string{
		"RAILS_ENV": value("production"), "RACK_ENV": value("production"), "EMPTY_SETTING": value("1"), "RAILS_LOG_TO_STDOUT": value("1")}})
	if err != nil {
		t.Fatal(err)
	}
	values, err := nativeReleaseEnvValues(f.m.cfg.Native.Releases[f.app.ID])
	if err != nil || values["RAILS_ENV"] != "staging" || values["RACK_ENV"] != "production" || values["EMPTY_SETTING"] != "" || values["RAILS_LOG_TO_STDOUT"] != "1" || values["SECRET_KEY_BASE"] != "keep-me" {
		t.Fatal("missing-only defaults", values, err)
	}
	before, _ := os.ReadFile(envFile)
	if err := f.m.patchNativeSettings(f.app.ID, SettingsPatch{FromDefaults: true, OnlyMissing: true, EnvChanges: map[string]*string{"RAILS_ENV": value("production")}}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(envFile); string(after) != string(before) {
		t.Fatal("missing-only defaults rewrote an unchanged environment file")
	}
	// Replacement overwrites the approved keys in place.
	if err := f.m.patchNativeSettings(f.app.ID, SettingsPatch{FromDefaults: true, EnvChanges: map[string]*string{"RAILS_ENV": value("production")}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(envFile)
	got := string(raw)
	if !strings.Contains(got, "RAILS_ENV=production\n") || strings.Contains(got, "RAILS_ENV=staging") || strings.Count(got, "RAILS_ENV=") != 1 || !strings.Contains(got, "PATH=/evil\n") {
		t.Fatalf("environment file after replacement:\n%s", got)
	}
	for name, changes := range map[string]map[string]*string{
		"host key":   {"PATH": value("/tmp")},
		"port":       {"PORT": value("1")},
		"lidza":      {"LIDZA_X": value("1")},
		"credential": {"SECRET_KEY_BASE": value("shared")},
		"database":   {"DATABASE_URL": value("postgres://shared")},
		"quote":      {"GREETING": value("it's")},
		"delete":     {"RAILS_ENV": nil},
	} {
		if err := f.m.patchNativeSettings(f.app.ID, SettingsPatch{FromDefaults: true, EnvChanges: changes}); err == nil {
			t.Fatal("accepted default", name)
		}
	}
	if after, _ := os.ReadFile(envFile); string(after) != got {
		t.Fatal("refused defaults changed the environment file")
	}
}
