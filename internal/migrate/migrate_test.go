package migrate

import (
	"bytes"
	"context"
	"net"
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

func gitFixture(t *testing.T) (url string, commit func(file, body string) string) {
	t.Helper()
	work := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1", "HOME="+work)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "--initial-branch=main")
	commit = func(file, body string) string {
		os.MkdirAll(filepath.Dir(filepath.Join(work, file)), 0o755)
		if err := os.WriteFile(filepath.Join(work, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "--quiet", "-m", file)
		return git("rev-parse", "HEAD")
	}
	return "file://" + work, commit
}

type fakeServices struct {
	mu     sync.Mutex
	states map[string]release.ServiceState
	calls  []string
}

func newFakeServices(running ...string) *fakeServices {
	f := &fakeServices{states: map[string]release.ServiceState{}}
	for _, unit := range running {
		f.states[unit] = release.ServiceState{Active: "active", Sub: "running", UnitFile: "enabled"}
	}
	return f
}

func (f *fakeServices) record(action, unit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, action+" "+unit)
}

func (f *fakeServices) State(_ context.Context, unit string) (release.ServiceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.states[unit]
	if !ok {
		return release.ServiceState{Active: "inactive", Sub: "dead"}, nil
	}
	return state, nil
}

func (f *fakeServices) set(unit string, state release.ServiceState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[unit] = state
}

func (f *fakeServices) Restart(_ context.Context, unit string) error {
	f.record("restart", unit)
	return nil
}
func (f *fakeServices) DaemonReload(context.Context) error { f.record("daemon-reload", ""); return nil }
func (f *fakeServices) Enable(_ context.Context, unit string) error {
	f.record("enable", unit)
	return nil
}
func (f *fakeServices) Start(_ context.Context, unit string) error {
	f.record("start", unit)
	f.set(unit, release.ServiceState{Active: "active", Sub: "running", UnitFile: "enabled"})
	return nil
}
func (f *fakeServices) Stop(_ context.Context, unit string) error {
	f.record("stop", unit)
	f.set(unit, release.ServiceState{Active: "inactive", Sub: "dead", UnitFile: "enabled"})
	return nil
}

type fakeCrontab struct{ body string }

func (f *fakeCrontab) Read(context.Context) (string, error)       { return f.body, nil }
func (f *fakeCrontab) Write(_ context.Context, body string) error { f.body = body; return nil }

// fakeDumper copies a "database" held in a file so the transfer path is
// exercised without PostgreSQL; the real tools are covered in postgres_test.go.
type fakeDumper struct{ stores map[string]string }

func (d fakeDumper) Dump(_ context.Context, url, path string) error {
	raw, err := os.ReadFile(d.stores[url])
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func (d fakeDumper) Restore(_ context.Context, url, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(d.stores[url], raw, 0o600)
}

func testProfile() release.Profile {
	return release.Profile{Name: "test", Shared: release.Shared{Dirs: []string{"log", "public/uploads"}},
		Build: []release.Step{{Name: "build", Args: []string{"sh", "-c", "cat VERSION > built.txt"}}}}
}

func deployFixture(t *testing.T, root, url string, services release.Services, units []string) release.Plan {
	t.Helper()
	plan := release.Plan{App: "app", Layout: release.Layout{Root: root}, Source: release.Source{URL: url}, Ref: "main",
		Profile: testProfile(), Env: []string{"PATH=" + os.Getenv("PATH")}, Exec: release.LocalExecutor{},
		Services: services, Units: units, Settle: time.Second, Keep: 3, SkipBackup: true}
	if _, err := release.Deploy(context.Background(), plan); err != nil {
		t.Fatal("fixture deploy", err)
	}
	return plan
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestExportImportAndCutoverMoveAnApplication(t *testing.T) {
	ctx := context.Background()
	url, commit := gitFixture(t)
	want := commit("VERSION", "one\n")
	base := t.TempDir()
	sourceRoot, targetRoot := filepath.Join(base, "src", "app"), filepath.Join(base, "dst", "app")
	os.MkdirAll(sourceRoot, 0o755)
	sourceServices := newFakeServices("app-server.service", "app-worker.service")
	units := []string{"app-server.socket", "app-server.service", "app-worker.service"}
	deployFixture(t, sourceRoot, url, sourceServices, units[1:])

	// Source state that must travel: uploads, an environment file, unit files,
	// a crontab block and a database. Caches and logs must not travel.
	write := func(path, body string) {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(sourceRoot, "shared/public/uploads/photo.jpg"), "jpeg")
	write(filepath.Join(sourceRoot, "shared/bundle/ruby/gem.rb"), "cache")
	write(filepath.Join(sourceRoot, "shared/log/production.log"), "noise")
	write(filepath.Join(sourceRoot, ".env"), "SECRET_KEY_BASE=abc\nPORT=9150\n")
	sourceUnits := filepath.Join(base, "src", "units")
	for _, unit := range units {
		write(filepath.Join(sourceUnits, unit), "[Service]\nWorkingDirectory="+sourceRoot+"/current\nEnvironmentFile=-"+sourceRoot+"/.env\n")
	}
	stores := map[string]string{"postgres://src/app": filepath.Join(base, "src-db"), "postgres://dst/app": filepath.Join(base, "dst-db")}
	write(stores["postgres://src/app"], "rows=1")
	sourceTab := &fakeCrontab{body: "MAILTO=\"\"\n# Begin Whenever generated tasks for: app at: 2026-01-01\n0 * * * * /bin/job\n# End Whenever generated tasks for: app at: 2026-01-01\n0 2 * * * /bin/other\n"}
	source := App{Name: "app", Layout: release.Layout{Root: sourceRoot}, Domain: "app.example.com", Port: "9150",
		Units: units, EnvFiles: []string{filepath.Join(sourceRoot, ".env")}, UnitDir: sourceUnits,
		Databases: map[string]string{"primary": "postgres://src/app"}}
	if blockers := PreflightSource(ctx, source).Blockers(); len(blockers) > 0 {
		t.Fatal("source preflight", blockers)
	}

	bundle := filepath.Join(base, "bundle")
	var log bytes.Buffer
	manifest, err := Export(ctx, source, bundle, ExportOptions{Dumper: fakeDumper{stores}, Crontab: sourceTab, Log: &log})
	if err != nil {
		t.Fatal(err, log.String())
	}
	if manifest.Commit != want || manifest.Schedule != "0 * * * * /bin/job" || len(manifest.Databases) != 1 {
		t.Fatalf("manifest %+v", manifest)
	}
	if !exists(filepath.Join(bundle, "shared/public/uploads/photo.jpg")) || exists(filepath.Join(bundle, "shared/bundle")) || exists(filepath.Join(bundle, "shared/log/production.log")) {
		t.Fatal("bundle shared selection is wrong")
	}
	if info, _ := os.Stat(filepath.Join(bundle, manifest.EnvFiles[0].Path)); info.Mode().Perm() != 0o600 {
		t.Fatal("environment file in bundle is not private")
	}
	if manifest.EnvFiles[0].Target != ".env" {
		t.Fatalf("environment target %q should be relative to the root", manifest.EnvFiles[0].Target)
	}
	if _, err := Export(ctx, source, bundle, ExportOptions{Dumper: fakeDumper{stores}}); err == nil {
		t.Fatal("export over an existing bundle was accepted")
	}

	remote := filepath.Join(base, "remote", "bundle")
	if err := (LocalTransport{}).Push(ctx, bundle, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(remote); err != nil {
		t.Fatal("transferred bundle does not verify", err)
	}

	targetServices := newFakeServices()
	targetUnits := filepath.Join(base, "dst", "units")
	target := App{Name: "app", Layout: release.Layout{Root: targetRoot}, Domain: "app.example.com", Port: "9150",
		Units: units, EnvFiles: []string{filepath.Join(targetRoot, ".env")}, UnitDir: targetUnits,
		Databases: map[string]string{"primary": "postgres://dst/app"}}
	report := PreflightTarget(ctx, target, targetServices, Requirements{FreeBytes: 1 << 20, Paths: []string{"/bin/sh"}})
	if blockers := report.Blockers(); len(blockers) > 0 {
		t.Fatal("target preflight", blockers)
	}
	targetTab := &fakeCrontab{body: "0 3 * * * /bin/target-only\n"}
	var targetPlan release.Plan
	imported, err := Import(ctx, target, remote, ImportOptions{Services: targetServices, Dumper: fakeDumper{stores},
		Databases: map[string]string{"primary": "postgres://dst/app"}, Log: &log,
		Deploy: func(ctx context.Context, commit string) error {
			targetPlan = release.Plan{App: "app", Layout: target.Layout, Source: release.Source{URL: url}, Ref: commit,
				Profile: testProfile(), Env: []string{"PATH=" + os.Getenv("PATH")}, Exec: release.LocalExecutor{},
				Services: targetServices, Units: units[1:], Settle: time.Second, Keep: 3, SkipBackup: true}
			_, err := release.Deploy(ctx, targetPlan)
			return err
		}})
	if err != nil {
		t.Fatal(err, log.String())
	}
	if imported.Commit != want {
		t.Fatal("imported manifest differs")
	}
	if current, _ := target.Layout.CurrentID(); current == "" || read(t, filepath.Join(targetRoot, "current/REVISION")) != want+"\n" {
		t.Fatal("target did not publish the source commit")
	}
	if read(t, filepath.Join(targetRoot, "current/built.txt")) != "one\n" || read(t, filepath.Join(targetRoot, "shared/public/uploads/photo.jpg")) != "jpeg" {
		t.Fatal("target build or shared files are wrong")
	}
	if exists(filepath.Join(targetRoot, "shared/bundle/ruby")) {
		t.Fatal("excluded cache was copied")
	}
	if read(t, filepath.Join(targetRoot, ".env")) != "SECRET_KEY_BASE=abc\nPORT=9150\n" {
		t.Fatal("environment file not restored")
	}
	if unit := read(t, filepath.Join(targetUnits, "app-server.service")); !strings.Contains(unit, targetRoot+"/current") || strings.Contains(unit, sourceRoot) {
		t.Fatalf("unit paths were not rewritten:\n%s", unit)
	}
	if read(t, stores["postgres://dst/app"]) != "rows=1" {
		t.Fatal("database was not restored")
	}
	calls := strings.Join(targetServices.calls, "\n")
	if !strings.Contains(calls, "daemon-reload") || !strings.Contains(calls, "enable app-server.socket") || strings.Contains(calls, "start ") {
		t.Fatalf("import must enable without starting:\n%s", calls)
	}
	if strings.Contains(targetTab.body, "/bin/job") {
		t.Fatal("schedule was installed before cutover")
	}
	if _, err := Import(ctx, target, remote, ImportOptions{Services: targetServices, Deploy: func(context.Context, string) error { return nil }}); err == nil {
		t.Fatal("import over a published target was accepted")
	}

	// Cutover: freeze the source, move the final data, start the target.
	maintenance := Maintenance{Dir: filepath.Join(sourceRoot, "shared/maintenance")}
	freeze, err := FreezeSource(ctx, maintenance, sourceServices, Workers(units), "Moving servers", &log)
	if err != nil || !maintenance.Enabled() || !slices.Equal(freeze.Stopped, []string{"app-worker.service"}) {
		t.Fatal("freeze", err, freeze)
	}
	if state, _ := sourceServices.State(ctx, "app-server.service"); !state.Running() {
		t.Fatal("web unit must keep running behind the maintenance page")
	}
	write(stores["postgres://src/app"], "rows=2")
	write(filepath.Join(sourceRoot, "shared/public/uploads/late.jpg"), "late")
	final := filepath.Join(base, "bundle-final")
	finalManifest, err := Export(ctx, source, final, ExportOptions{Dumper: fakeDumper{stores}, Crontab: sourceTab})
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(final, "shared/maintenance")) {
		t.Fatal("maintenance directory must not travel")
	}
	os.MkdirAll(filepath.Join(targetRoot, "shared/bundle/ruby"), 0o755) // built on the target meanwhile
	if err := ImportData(ctx, target, final, finalManifest, ImportOptions{Dumper: fakeDumper{stores}, Databases: map[string]string{"primary": "postgres://dst/app"}}); err != nil {
		t.Fatal(err)
	}
	if read(t, stores["postgres://dst/app"]) != "rows=2" || !exists(filepath.Join(targetRoot, "shared/public/uploads/late.jpg")) || !exists(filepath.Join(targetRoot, "shared/bundle/ruby")) {
		t.Fatal("final sync lost data or removed the target's rebuilt cache")
	}
	if err := StartServices(ctx, targetServices, units, &log); err != nil {
		t.Fatal(err)
	}
	calls = strings.Join(targetServices.calls, "\n")
	if strings.Index(calls, "start app-server.socket") > strings.Index(calls, "start app-server.service") {
		t.Fatal("sockets must start before services")
	}
	if changed, err := InstallSchedule(ctx, targetTab, finalManifest); err != nil || !changed || !strings.Contains(targetTab.body, "/bin/job") || !strings.Contains(targetTab.body, "/bin/target-only") {
		t.Fatal("schedule install", err, targetTab.body)
	}
	if err := ThawSource(ctx, maintenance, sourceServices, freeze, &log); err != nil || maintenance.Enabled() {
		t.Fatal("thaw", err)
	}
	if state, _ := sourceServices.State(ctx, "app-worker.service"); !state.Running() {
		t.Fatal("thaw did not restart the stopped worker")
	}
	if len(Steps()) < 10 {
		t.Fatal("runbook steps missing")
	}
}

func TestScheduleBlock(t *testing.T) {
	block, err := ScheduleBlock("a\n# Begin Whenever generated tasks for: x\n1\n2\n# End Whenever generated tasks for: x\n", "x")
	if err != nil || block != "1\n2" {
		t.Fatal(err, block)
	}
	if block, err := ScheduleBlock("nothing", "x"); err != nil || block != "" {
		t.Fatal(err, block)
	}
	if _, err := ScheduleBlock("# Begin Whenever generated tasks for: x\n1\n", "x"); err == nil {
		t.Fatal("unclosed block accepted")
	}
}

func TestPreflightTargetReportsConflicts(t *testing.T) {
	ctx := context.Background()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	root := filepath.Join(t.TempDir(), "app")
	unitDir := filepath.Join(t.TempDir(), "units")
	os.MkdirAll(unitDir, 0o755)
	os.WriteFile(filepath.Join(unitDir, "taken.service"), nil, 0o644)
	app := App{Name: "app", Layout: release.Layout{Root: root}, Port: strings.TrimPrefix(listener.Addr().String(), "127.0.0.1:"),
		Units: []string{"taken.service", "free.service"}, UnitDir: unitDir}
	services := newFakeServices("free.service")
	report := PreflightTarget(ctx, app, services, Requirements{FreeBytes: 1, Paths: []string{"/definitely/missing"}})
	var checks []string
	for _, f := range report.Blockers() {
		checks = append(checks, f.Check)
	}
	slices.Sort(checks)
	if !slices.Equal(checks, []string{"ports", "toolchain", "units", "units"}) {
		t.Fatalf("blockers %v (port %d)", report.Findings, port)
	}
	if free, err := freeBytes(root); err != nil || free == 0 {
		t.Fatal("free space must resolve through missing parents", free, err)
	}
}

func TestMaintenanceToggleAndSite(t *testing.T) {
	m := Maintenance{Dir: filepath.Join(t.TempDir(), "maintenance")}
	if m.Enabled() {
		t.Fatal("enabled before marker")
	}
	if err := m.Enable("Back <soon>"); err != nil || !m.Enabled() || !strings.Contains(read(t, m.Page()), "Back &lt;soon&gt;") {
		t.Fatal("enable", err)
	}
	if err := m.Disable(); err != nil || m.Enabled() || !exists(m.Page()) {
		t.Fatal("disable should keep the page", err)
	}
	site, err := m.CaddySite("app.example.com", "127.0.0.1:9150", "/up")
	if err != nil || !strings.Contains(site, "try_files enabled") || !strings.Contains(site, "status 503") || !strings.Contains(site, "not path /up") {
		t.Fatal(err, site)
	}
	if _, err := m.CaddySite("bad domain", "127.0.0.1:9150", ""); err == nil {
		t.Fatal("invalid domain accepted")
	}
	caddy := os.Getenv("LIDZA_CADDY_BINARY")
	if caddy == "" {
		caddy, _ = exec.LookPath("caddy")
	}
	if _, err := os.Stat(caddy); caddy == "" || err != nil {
		t.Skip("no caddy binary to validate the site block")
	}
	config := filepath.Join(t.TempDir(), "Caddyfile")
	os.WriteFile(config, []byte("{\n\tauto_https off\n\tadmin off\n}\n"+site), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, caddy, "validate", "--adapter", "caddyfile", "--config", config)
	cmd.Env = []string{"HOME=" + t.TempDir(), "XDG_DATA_HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("caddy validate: %v\n%s\n%s", err, out, site)
	}
}

func TestSSHTransportUsesOnlyTheGivenCommand(t *testing.T) {
	joined := shellJoin([]string{"ssh", "-S", "/run/x.sock", "-o", "ControlMaster=no", "-o", "ConnectionAttempts=1", "it's"})
	if !strings.Contains(joined, `'-S' '/run/x.sock'`) || !strings.Contains(joined, `'it'\''s'`) {
		t.Fatal(joined)
	}
	if err := (SSHTransport{}).Push(context.Background(), t.TempDir(), "/tmp/x"); err == nil {
		t.Fatal("empty transport accepted")
	}
}
