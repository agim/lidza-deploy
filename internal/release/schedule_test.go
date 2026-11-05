package release

import (
	"slices"
	"strings"
	"testing"
)

func TestReplaceScheduleBlockAdoptsLegacyIdentifiersAndPathPrefixes(t *testing.T) {
	existing := strings.Join([]string{
		"MAILTO=\"\"",
		"# Begin Whenever generated tasks for: /home/deploy/app/releases/20250101000000/config/schedule.rb at: 2025-01-01 00:00:00 +0000",
		"0 1 * * * old-one",
		"# End Whenever generated tasks for: /home/deploy/app/releases/20250101000000/config/schedule.rb at: 2025-01-01 00:00:00 +0000",
		"",
		"# Begin Whenever generated tasks for: /home/deploy/app/releases/20250202000000/config/schedule.rb",
		"0 2 * * * old-two",
		"# End Whenever generated tasks for: /home/deploy/app/releases/20250202000000/config/schedule.rb",
		"",
		"# Begin Whenever generated tasks for: legacy-name",
		"0 3 * * * old-three",
		"# End Whenever generated tasks for: legacy-name",
		"",
		"# Begin Whenever generated tasks for: /home/deploy/other/releases/20250101000000/config/schedule.rb",
		"0 4 * * * other-app",
		"# End Whenever generated tasks for: /home/deploy/other/releases/20250101000000/config/schedule.rb",
		"",
		"# Begin Whenever generated tasks for: app",
		"0 5 * * * current",
		"# End Whenever generated tasks for: app",
		"0 6 * * * /usr/bin/manual",
	}, "\n") + "\n"
	got, err := ReplaceScheduleBlock(existing, "app", "0 7 * * * new", "/home/deploy/app/releases/", "legacy-name")
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"old-one", "old-two", "old-three", "current", "20250101000000/config/schedule.rb at:"} {
		if strings.Contains(got, gone) {
			t.Fatalf("legacy entry %q survived:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"MAILTO=\"\"", "other-app", "/usr/bin/manual", "for: /home/deploy/other/releases/"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("unrelated line %q lost:\n%s", kept, got)
		}
	}
	if strings.Count(got, "for: app") != 2 || !strings.HasSuffix(got, "# Begin Whenever generated tasks for: app\n0 7 * * * new\n# End Whenever generated tasks for: app\n") {
		t.Fatalf("new block missing or duplicated:\n%s", got)
	}
	// Idempotent: the same call again changes nothing.
	again, err := ReplaceScheduleBlock(got, "app", "0 7 * * * new", "/home/deploy/app/releases/", "legacy-name")
	if err != nil || again != got {
		t.Fatal("second replacement was not idempotent", err)
	}
	// A prefix that is not a directory prefix must be spelled out as such.
	if _, err := ReplaceScheduleBlock(existing, "app", "x", ""); err == nil {
		t.Fatal("empty adopted identifier accepted")
	}
	if _, err := ReplaceScheduleBlock(existing, "app", "x", "/"); err == nil {
		t.Fatal("root prefix accepted")
	}
	// An unclosed legacy block is refused rather than swallowing the rest of the crontab.
	broken := "# Begin Whenever generated tasks for: legacy-name\n0 3 * * * x\n0 6 * * * manual\n"
	if _, err := ReplaceScheduleBlock(broken, "app", "x", "legacy-name"); err == nil || !strings.Contains(err.Error(), "legacy-name") {
		t.Fatal("unclosed legacy block accepted", err)
	}
	// Without adoption, legacy blocks are left exactly as they are.
	plain, err := ReplaceScheduleBlock(existing, "app", "0 7 * * * new")
	if err != nil || !strings.Contains(plain, "old-one") || !strings.Contains(plain, "old-three") || strings.Contains(plain, "current") {
		t.Fatal("non-adopting replacement touched legacy blocks", err)
	}
}

func TestWheneverSetAndRailsScheduleStep(t *testing.T) {
	set, err := wheneverSet("production", "/srv/app/current", map[string]string{"output": "log/cron.log", "bundle_command": "bundle exec"})
	if err != nil || set != "environment=production&path=/srv/app/current&bundle_command=bundle exec&output=log/cron.log" {
		t.Fatal(set, err)
	}
	for name, vars := range map[string]map[string]string{
		"ampersand":   {"output": "a&b"},
		"equals":      {"output": "a=b"},
		"environment": {"environment": "staging"},
		"path":        {"path": "/elsewhere"},
		"badkey":      {"bad key": "x"},
	} {
		if _, err := wheneverSet("production", "/srv/app/current", vars); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	l := Layout{Root: "/srv/app"}
	step := Rails(RailsOptions{ScheduleRoles: []string{"worker", "cron"}, ScheduleVariables: map[string]string{"output": "log/cron.log"}}).Schedule(l)
	want := []string{"bundle", "exec", "whenever", "--load-file", "config/schedule.rb", "--set",
		"environment=production&path=/srv/app/current&output=log/cron.log", "--roles", "worker,cron"}
	if step.Name != "whenever" || !slices.Equal(step.Args, want) {
		t.Fatalf("schedule step %q %v", step.Name, step.Args)
	}
	if plain := Rails(RailsOptions{}).Schedule(l); slices.Contains(plain.Args, "--roles") || plain.Args[6] != "environment=production&path=/srv/app/current" {
		t.Fatalf("default schedule step %v", plain.Args)
	}
	invalid := Rails(RailsOptions{ScheduleVariables: map[string]string{"output": "a&b"}}).Schedule(l)
	if !strings.HasPrefix(invalid.Name, "whenever: ") || !slices.Equal(invalid.Args, []string{"false"}) {
		t.Fatalf("invalid variables did not produce a failing step: %q %v", invalid.Name, invalid.Args)
	}
	if role := Rails(RailsOptions{ScheduleRoles: []string{"wor ker"}}).Schedule(l); !slices.Equal(role.Args, []string{"false"}) {
		t.Fatalf("invalid role accepted: %v", role.Args)
	}
}
