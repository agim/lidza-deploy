package release

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWheneverScheduleDeployAndRollback renders schedules with the real
// Whenever gem through the Rails profile and checks what reaches the crontab
// across two deploys, a rollback, adoption of blocks left by an earlier tool,
// and removal when the schedule file disappears.
//
// Opt in with LIDZA_RELEASE_WHENEVER_IT=1. It needs Ruby with bundler on
// LIDZA_RELEASE_TOOLCHAIN_PATH (or PATH) and network access to rubygems.org
// on the first run; LIDZA_RELEASE_WHENEVER_CACHE names a directory that keeps
// the installed gems between runs.
func TestWheneverScheduleDeployAndRollback(t *testing.T) {
	if os.Getenv("LIDZA_RELEASE_WHENEVER_IT") != "1" {
		t.Skip("set LIDZA_RELEASE_WHENEVER_IT=1 to run against the whenever gem")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env := realAppEnv(t, t.TempDir())
	url, commit := gitFixture(t)
	work := strings.TrimPrefix(url, "file://")
	commit("Gemfile", "source \"https://rubygems.org\"\ngem \"whenever\"\ngem \"rake\"\n")
	lock := exec.CommandContext(ctx, "bundle", "lock")
	lock.Dir = work
	lock.Env = withEnv(env, "BUNDLE_GEMFILE="+filepath.Join(work, "Gemfile"), "BUNDLE_PATH="+filepath.Join(t.TempDir(), "lock-gems"))
	if out, err := lock.CombinedOutput(); err != nil {
		t.Fatalf("bundle lock: %v\n%s", err, out)
	}
	first := commit("config/schedule.rb", `set :output, "log/cron.log"
env :MAILTO, "ops@example.com"
job_type :report, "cd :path && bin/report :task :output"
every 5.minutes do
  runner "Heartbeat.ping"
end
every 1.day, at: "2:30 am" do
  rake "cache:clear"
end
every :hour, roles: [:worker] do
  report "nightly"
end
every :sunday, at: "4am", roles: [:web] do
  command "echo weekly"
end
`)
	root := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if cache := os.Getenv("LIDZA_RELEASE_WHENEVER_CACHE"); cache != "" {
		if err := os.MkdirAll(cache, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "shared"), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("cp", "-a", cache, filepath.Join(root, "shared", "bundle")).CombinedOutput(); err != nil {
			t.Fatalf("seed gem cache: %v %s", err, out)
		}
		t.Cleanup(func() {
			exec.Command("sh", "-c", "rm -rf "+shellQuote(cache)+" && cp -a "+shellQuote(filepath.Join(root, "shared", "bundle"))+" "+shellQuote(cache)).Run()
		})
	}
	legacyPath := filepath.Join(root, "releases", "20250101000000", "config", "schedule.rb")
	tab := &fakeCrontab{body: strings.Join([]string{
		"MAILTO=\"\"",
		"0 1 * * * /usr/bin/other",
		"# Begin Whenever generated tasks for: " + legacyPath + " at: 2025-01-01 00:00:00 +0000",
		"0 2 * * * /bin/bash -l -c 'cd " + filepath.Dir(filepath.Dir(legacyPath)) + " && bundle exec rake legacy'",
		"# End Whenever generated tasks for: " + legacyPath + " at: 2025-01-01 00:00:00 +0000",
		"# Begin Whenever generated tasks for: app-old-name",
		"0 3 * * * legacy-by-name",
		"# End Whenever generated tasks for: app-old-name",
	}, "\n") + "\n"}
	plan := Plan{
		App: "app", Layout: Layout{Root: root}, Source: Source{URL: url}, Ref: "main",
		Profile: Rails(RailsOptions{Shared: &Shared{Dirs: []string{"log"}}, ScheduleRoles: []string{"worker"}}),
		Env:     env, Exec: LocalExecutor{}, SkipBackup: true, Crontab: tab, Keep: 3, Log: testLog{t},
		AdoptSchedules: []string{filepath.Join(root, "releases") + "/", "app-old-name"},
	}
	current := filepath.Join(root, "current")
	heartbeat := "/bin/bash -l -c 'cd " + current + " && bundle exec script/runner -e production '\\''Heartbeat.ping'\\'' >> log/cron.log 2>&1'"
	cacheClear := "30 2 * * * /bin/bash -l -c 'cd " + current + " && RAILS_ENV=production bundle exec rake cache:clear --silent >> log/cron.log 2>&1'"
	nightly := "0 * * * * /bin/bash -l -c 'cd " + current + " && bin/report nightly >> log/cron.log 2>&1'"

	one, err := Deploy(ctx, plan)
	if err != nil {
		t.Fatal("first deploy", err)
	}
	if !one.ScheduleChanged {
		t.Fatal("first deploy did not install the schedule")
	}
	body := tab.body
	for _, want := range []string{"MAILTO=\"\"\n0 1 * * * /usr/bin/other\n", "# Begin Whenever generated tasks for: app\n", "MAILTO=ops@example.com",
		"0,5,10,15,20,25,30,35,40,45,50,55 * * * * " + heartbeat, cacheClear, nightly, "# End Whenever generated tasks for: app\n"} {
		if !strings.Contains(body, want) {
			t.Fatalf("crontab lacks %q:\n%s", want, body)
		}
	}
	for _, gone := range []string{"rake legacy", "legacy-by-name", "app-old-name", "20250101000000", "echo weekly", "## [message]", filepath.Join(root, "releases", one.Release)} {
		if strings.Contains(body, gone) {
			t.Fatalf("crontab still contains %q:\n%s", gone, body)
		}
	}
	if strings.Count(body, "Begin Whenever") != 1 {
		t.Fatalf("expected exactly one block:\n%s", body)
	}

	commit("config/schedule.rb", "set :output, \"log/cron.log\"\nevery 10.minutes do\n  rake \"reports:refresh\"\nend\n")
	two, err := Deploy(ctx, plan)
	if err != nil || !two.ScheduleChanged {
		t.Fatal("second deploy", err, two.ScheduleChanged)
	}
	body = tab.body
	refresh := "0,10,20,30,40,50 * * * * /bin/bash -l -c 'cd " + current + " && RAILS_ENV=production bundle exec rake reports:refresh --silent >> log/cron.log 2>&1'"
	if !strings.Contains(body, refresh) || strings.Contains(body, "Heartbeat") || strings.Count(body, "Begin Whenever") != 1 || !strings.Contains(body, "0 1 * * * /usr/bin/other\n") {
		t.Fatalf("second deploy did not replace the block:\n%s", body)
	}

	back, err := Rollback(ctx, plan, "")
	if err != nil || back.Release != one.Release || !back.ScheduleChanged {
		t.Fatal("rollback", err, back)
	}
	if body = tab.body; !strings.Contains(body, heartbeat) || !strings.Contains(body, nightly) || strings.Contains(body, "reports:refresh") || strings.Count(body, "Begin Whenever") != 1 {
		t.Fatalf("rollback did not restore the first schedule:\n%s", body)
	}

	// Removing the schedule file removes the block and nothing else.
	rm := exec.Command("git", "rm", "--quiet", "config/schedule.rb")
	rm.Dir = work
	if out, err := rm.CombinedOutput(); err != nil {
		t.Fatalf("git rm: %v %s", err, out)
	}
	commit("VERSION", first+"\n")
	three, err := Deploy(ctx, plan)
	if err != nil || !three.ScheduleChanged {
		t.Fatal("third deploy", err, three.ScheduleChanged)
	}
	if tab.body != "MAILTO=\"\"\n0 1 * * * /usr/bin/other\n" {
		t.Fatalf("schedule removal left:\n%s", tab.body)
	}
	t.Logf("whenever rendered and installed through %d deploys and 1 rollback; other crontab lines untouched", 3)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
