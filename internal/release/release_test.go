package release

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, file)), 0o755); err != nil {
			t.Fatal(err)
		}
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
	mu        sync.Mutex
	states    map[string]ServiceState
	restarted []string
	fail      map[string]bool
}

func (f *fakeServices) State(_ context.Context, unit string) (ServiceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[unit], nil
}

func (f *fakeServices) Restart(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, unit)
	if f.fail[unit] {
		f.states[unit] = ServiceState{Active: "failed", Sub: "failed"}
	}
	return nil
}

type fakeCrontab struct{ body string }

func (f *fakeCrontab) Read(context.Context) (string, error)       { return f.body, nil }
func (f *fakeCrontab) Write(_ context.Context, body string) error { f.body = body; return nil }

func running() ServiceState {
	return ServiceState{Active: "active", Sub: "running", UnitFile: "enabled"}
}

// testProfile writes build evidence with plain shell steps.
func testProfile() Profile {
	return Profile{
		Name:   "test",
		Shared: Shared{Dirs: []string{"log", "public/uploads"}},
		Build: []Step{
			{Name: "build", Args: []string{"sh", "-c", "cat VERSION > built.txt"}},
			{Name: "skipped", Args: []string{"sh", "-c", "exit 9"}, When: func(string) bool { return false }},
		},
		Release: []Step{{Name: "migrate", Args: []string{"sh", "-c", "cat VERSION >> ../../shared/migrations.log"},
			Database: true, When: func(dir string) bool { return exists(dir, "db/migrate") }}},
		Schedule: func(l Layout) *Step {
			return &Step{Name: "schedule", Args: []string{"sh", "-c", "echo '## [message] ignored'; cat config/cron"},
				When: func(dir string) bool { return exists(dir, "config/cron") }}
		},
	}
}

func testPlan(t *testing.T, url string) (Plan, *fakeServices, *fakeCrontab, *bytes.Buffer) {
	root := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	services := &fakeServices{states: map[string]ServiceState{
		"app-server.service":   running(),
		"app-worker.service":   running(),
		"app-disabled.service": {Active: "inactive", Sub: "dead", UnitFile: "disabled"},
	}}
	tab := &fakeCrontab{body: "MAILTO=\"\"\n0 1 * * * /usr/bin/other\n"}
	var log bytes.Buffer
	tick := time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC)
	return Plan{
		App: "app", Layout: Layout{Root: root}, Source: Source{URL: url}, Ref: "main", Profile: testProfile(),
		Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root}, Exec: LocalExecutor{},
		Services: services, Units: []string{"app-server.service", "app-worker.service", "app-disabled.service"},
		Settle: time.Second, Crontab: tab, Keep: 3, Log: &log,
		Now: func() time.Time { tick = tick.Add(time.Minute); return tick },
	}, services, tab, &log
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestDeployPublishesRestartsRunningUnitsAndSyncsSchedule(t *testing.T) {
	url, commit := gitFixture(t)
	commit("config/cron", "*/5 * * * * /bin/true\n")
	first := commit("VERSION", "one\n")
	p, services, tab, log := testPlan(t, url)
	result, err := Deploy(context.Background(), p)
	if err != nil {
		t.Fatal(err, log.String())
	}
	l := p.Layout
	if result.Commit != first || result.Previous != "" || result.Migrated {
		t.Fatalf("unexpected first result %+v", result)
	}
	if current, _ := l.CurrentID(); current != result.Release {
		t.Fatal("current was not published")
	}
	if got := readFile(t, filepath.Join(l.Current(), "built.txt")); got != "one\n" {
		t.Fatalf("build output %q", got)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(l.Current(), RevisionFile))); got != first {
		t.Fatal("REVISION not recorded")
	}
	target, _ := os.Readlink(filepath.Join(l.Release(result.Release), "public/uploads"))
	if target != "../../../shared/public/uploads" {
		t.Fatalf("shared link %q", target)
	}
	if link, _ := os.Readlink(l.Current()); link != "releases/"+result.Release {
		t.Fatalf("current should be relative, got %q", link)
	}
	if !slices.Equal(services.restarted, []string{"app-server.service", "app-worker.service"}) {
		t.Fatalf("restarted %v", services.restarted)
	}
	if _, skipped := result.Skipped["app-disabled.service"]; !skipped {
		t.Fatal("disabled unit was not reported as skipped")
	}
	want := "MAILTO=\"\"\n0 1 * * * /usr/bin/other\n\n# Begin Whenever generated tasks for: app\n*/5 * * * * /bin/true\n# End Whenever generated tasks for: app\n"
	if tab.body != want || !result.ScheduleChanged {
		t.Fatalf("crontab:\n%s", tab.body)
	}
}

func TestDeployRequiresBackupBeforeMigrationsAndKeepsCurrentOnFailure(t *testing.T) {
	url, commit := gitFixture(t)
	commit("VERSION", "one\n")
	p, _, _, log := testPlan(t, url)
	first, err := Deploy(context.Background(), p)
	if err != nil {
		t.Fatal(err, log.String())
	}
	commit("db/migrate/1.rb", "")
	commit("VERSION", "two\n")
	if _, err := Deploy(context.Background(), p); err == nil || !strings.Contains(err.Error(), "backup is required") {
		t.Fatal("migration ran without a backup", err)
	}
	backups := 0
	p.Backup = func(context.Context) error { backups++; return errors.New("offsite upload failed") }
	if _, err := Deploy(context.Background(), p); err == nil {
		t.Fatal("failed backup did not stop the deploy")
	}
	if current, _ := p.Layout.CurrentID(); current != first.Release {
		t.Fatal("failed deploy changed current")
	}
	if ids, _ := p.Layout.List(); len(ids) != 1 {
		t.Fatalf("failed releases were not removed: %v", ids)
	}
	p.Backup = func(context.Context) error { backups++; return nil }
	second, err := Deploy(context.Background(), p)
	if err != nil || !second.Migrated || backups != 2 {
		t.Fatal("migration deploy failed", err, backups, log.String())
	}
	if got := readFile(t, filepath.Join(p.Layout.Shared(), "migrations.log")); got != "two\n" {
		t.Fatalf("migration log %q", got)
	}
}

func TestDeployRepublishesPreviousWhenHealthFails(t *testing.T) {
	url, commit := gitFixture(t)
	commit("VERSION", "one\n")
	p, services, _, log := testPlan(t, url)
	first, err := Deploy(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	commit("VERSION", "broken\n")
	p.Health = func(context.Context) error {
		if strings.Contains(readFile(t, filepath.Join(p.Layout.Current(), "built.txt")), "broken") {
			return errors.New("HTTP 500")
		}
		return nil
	}
	services.restarted = nil
	result, err := Deploy(context.Background(), p)
	if err == nil || !result.RolledBack {
		t.Fatal("unhealthy release was kept", err, log.String())
	}
	if current, _ := p.Layout.CurrentID(); current != first.Release {
		t.Fatal("previous release was not republished")
	}
	if len(services.restarted) != 4 {
		t.Fatalf("expected restart of new then previous release, got %v", services.restarted)
	}
}

func TestDeployRefusesWhenRestartedUnitFails(t *testing.T) {
	url, commit := gitFixture(t)
	commit("VERSION", "one\n")
	p, services, _, _ := testPlan(t, url)
	services.fail = map[string]bool{"app-worker.service": true}
	if _, err := Deploy(context.Background(), p); err == nil || !strings.Contains(err.Error(), "failed after restart") {
		t.Fatal("failed unit was accepted", err)
	}
}

func TestRollbackRestoresPreviousReleaseAndSchedule(t *testing.T) {
	url, commit := gitFixture(t)
	commit("config/cron", "0 * * * * /bin/one\n")
	commit("VERSION", "one\n")
	p, _, tab, _ := testPlan(t, url)
	first, err := Deploy(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	commit("config/cron", "0 * * * * /bin/two\n")
	second, err := Deploy(context.Background(), p)
	if err != nil || !strings.Contains(tab.body, "/bin/two") {
		t.Fatal("second deploy", err, tab.body)
	}
	back, err := Rollback(context.Background(), p, "")
	if err != nil || back.Release != first.Release || back.Previous != second.Release {
		t.Fatal("rollback", err, back)
	}
	if current, _ := p.Layout.CurrentID(); current != first.Release || !strings.Contains(tab.body, "/bin/one") || strings.Contains(tab.body, "/bin/two") {
		t.Fatal("rollback did not restore release and schedule", tab.body)
	}
	if _, err := Rollback(context.Background(), p, first.Release); err == nil {
		t.Fatal("rollback to the current release was accepted")
	}
}

func TestPruneKeepsRecentPublishedAndInProgressReleases(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < 6; i++ {
		id, err := l.Create(now.Add(time.Duration(i) * time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(l.Release(id), RevisionFile), []byte("x\n"), 0o644)
		if i != 1 && i != 5 {
			if err := l.Complete(id); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, id)
	}
	if err := l.Activate(ids[3], ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Activate(ids[5], ids[3]); err == nil {
		t.Fatal("incomplete release was published")
	}
	if err := l.Activate(ids[4], ids[2]); err == nil {
		t.Fatal("publication ignored a changed current")
	}
	removed, err := l.Prune(2)
	if err != nil {
		t.Fatal(err)
	}
	// Kept: current (3), newest two published (3, 4), in-progress (5).
	// Removed: published 0 and 2, abandoned incomplete 1.
	slices.Sort(removed)
	if !slices.Equal(removed, []string{ids[0], ids[1], ids[2]}) {
		t.Fatalf("removed %v from %v", removed, ids)
	}
	if prev, _ := l.Previous(); prev != "" {
		t.Fatal("previous should be gone after pruning", prev)
	}
}

func TestReplaceScheduleBlockAdoptsExistingMarkers(t *testing.T) {
	existing := "A=1\n# Begin Whenever generated tasks for: app at: 2026-01-01 00:00:00 +0000\nold\n# End Whenever generated tasks for: app at: 2026-01-01 00:00:00 +0000\n\n# Begin Whenever generated tasks for: other\nkeep\n# End Whenever generated tasks for: other\n"
	got, err := ReplaceScheduleBlock(existing, "app", "new")
	if err != nil || strings.Contains(got, "old") || !strings.Contains(got, "keep") || strings.Count(got, "for: app") != 2 {
		t.Fatal(err, got)
	}
	removed, err := ReplaceScheduleBlock(got, "app", "")
	if err != nil || strings.Contains(removed, "for: app") || !strings.Contains(removed, "keep") {
		t.Fatal(err, removed)
	}
	if _, err := ReplaceScheduleBlock("# Begin Whenever generated tasks for: app\n", "app", "x"); err == nil {
		t.Fatal("unclosed block accepted")
	}
}

func TestSourceRejectsCredentialURLs(t *testing.T) {
	for _, url := range []string{"https://token@github.com/a/b.git", "http://github.com/a/b.git", "git@github.com:a/b.git"} {
		if err := (Source{URL: url}).validate(); err == nil {
			t.Fatal("accepted", url)
		}
	}
	env := strings.Join(Source{URL: "https://github.com/a/b.git", Token: "secret"}.env(true), "\n")
	if strings.Contains(env, "secret") || !strings.Contains(env, "GIT_CONFIG_KEY_0=http.extraHeader") {
		t.Fatal("token must be passed encoded through git config environment only")
	}
}

func TestCreateIgnoresRestrictiveUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	l := Layout{Root: t.TempDir()}
	id, err := l.Create(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{l.Releases(), l.Release(id)} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o775 {
			t.Fatalf("%s mode %v, want 0775", path, info.Mode().Perm())
		}
	}
}
