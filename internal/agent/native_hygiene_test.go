package agent

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A native-only server: no docker binary, no daemon socket.
func TestCleanupOnNativeOnlyHostSkipsDockerAndTidiesReleases(t *testing.T) {
	f, _ := bundlerFixture(t) // a native Rails application with two releases
	m := f.m
	// The account's home is a fixture: the cleanup also tidies home caches.
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	fixtureAccount := *account
	fixtureAccount.HomeDir = t.TempDir()
	nativeLookupUser = func(string) (*user.User, error) { return &fixtureAccount, nil }
	savedRoot, savedPATH := hygieneHostRoot, os.Getenv("PATH")
	hygieneHostRoot = func() bool { return true }
	t.Setenv("PATH", t.TempDir()) // no docker command
	t.Cleanup(func() { hygieneHostRoot = savedRoot; os.Setenv("PATH", savedPATH) })
	policy := m.cfg.Native.Releases[f.app.ID]
	policy.Keep = 2
	m.cfg.Native.Releases[f.app.ID] = policy
	old := time.Now().Add(-30 * 24 * time.Hour)
	write := func(rel string, size int, modified time.Time) string {
		path := filepath.Join(f.root, rel)
		writeAged(t, path, size, modified)
		return path
	}
	// A third, oldest release beyond keep=2, plus a fresh one in progress.
	write("releases/20250101000000/REVISION", 41, old)
	write("releases/20250101000000/app.rb", 5000, old)
	os.Chtimes(filepath.Join(f.root, "releases/20250101000000"), old, old)
	os.Chtimes(filepath.Join(f.root, "releases/20251201000000"), old.Add(time.Hour), old.Add(time.Hour))
	scratch := write(".cache/tmp/linker.o", 700, time.Now())
	rotated := write("shared/log/production.log.1.gz", 900, old)
	live := write("shared/log/production.log", 900, old)
	recentRotated := write("shared/log/production.log.2", 900, time.Now())
	core := write("shared/core.1234", 1200, old)
	for i := 0; i < 3; i++ { // a bloated bootsnap cache with stale and fresh entries
		write("shared/tmp/cache/bootsnap/old"+string(rune('a'+i)), 100<<20, old)
	}
	fresh := write("shared/tmp/cache/bootsnap/fresh", 1<<20, time.Now())
	var commands []string
	m.maintenanceRun = func(_ context.Context, name string, args ...string) (string, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if name == "docker" {
			return "", errors.New("docker must not run")
		}
		return "Vacuuming done, freed 0B of archived journals from /var/log/journal.\n", nil
	}
	if err := m.startMaintenance(CleanupRequest{Backups: true, BuildCache: true, Logs: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	result := waitMaintenance(t, m)
	if result.Error != "" {
		t.Fatalf("cleanup failed on a native-only host: %s\n%v", result.Error, result.Messages)
	}
	report := strings.Join(result.Messages, "\n")
	for _, want := range []string{"Docker: not installed on this server; skipped", "Local backups: nothing to prune", "Journal: nothing older than 7 days to vacuum", "native-release " + f.app.ID + " release 20250101000000", "native-scratch", "app-log", "core-dump", "native-cache"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "freed 0B") || strings.Contains(report, "failed") {
		t.Fatalf("no-op or failure lines in the report:\n%s", report)
	}
	if slices.ContainsFunc(commands, func(c string) bool { return strings.HasPrefix(c, "docker") }) {
		t.Fatal("Docker commands ran", commands)
	}
	for _, gone := range []string{filepath.Join(f.root, "releases/20250101000000"), scratch, rotated, core, filepath.Join(f.root, "shared/tmp/cache/bootsnap/olda")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatal("not removed", gone)
		}
	}
	for _, kept := range []string{filepath.Join(f.root, "releases", f.original), filepath.Join(f.root, "releases/20251201000000"), live, recentRotated, fresh} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatal("removed", kept)
		}
	}
}

func TestDockerAvailabilityDistinguishesMissingStoppedAndBroken(t *testing.T) {
	bin, root := t.TempDir(), t.TempDir()
	savedPath, savedCommand := hostPath, hostCommand
	hostPath = func(path string) string { return filepath.Join(root, path) }
	enabled := "disabled"
	hostCommand = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "systemctl" {
			return enabled, nil
		}
		return "", errors.New("Cannot connect to the Docker daemon")
	}
	t.Cleanup(func() { hostPath, hostCommand = savedPath, savedCommand })
	t.Setenv("PATH", bin)
	t.Setenv("DOCKER_HOST", "")
	if got := dockerCleanupAvailability(context.Background()); got.ok || got.warning || !strings.Contains(got.reason, "not installed") {
		t.Fatal("missing docker", got)
	}
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	if got := dockerCleanupAvailability(context.Background()); got.ok || got.warning || !strings.Contains(got.reason, "stopped and disabled") {
		t.Fatal("intentionally stopped daemon", got)
	}
	enabled = "enabled"
	writeAged(t, filepath.Join(root, "var/run/docker.sock"), 0, time.Now())
	if got := dockerCleanupAvailability(context.Background()); got.ok || !got.warning || !strings.Contains(got.reason, "not reachable") {
		t.Fatal("enabled but unreachable daemon", got)
	}
	// The cleanup reports the warning and keeps running its other steps.
	m := testManager(t, &fakeRuntime{})
	dockerPresent(t, dockerAvailability{reason: "Docker: installed and enabled, but the daemon is not reachable; Docker cleanup skipped", warning: true})
	m.maintenanceRun = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "docker" {
			t.Error("docker ran while unreachable")
		}
		return "Vacuuming done, freed 12.0M of archived journals from /var/log/journal.", nil
	}
	if err := m.startMaintenance(CleanupRequest{BuildCache: true, Logs: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	result := waitMaintenance(t, m)
	report := strings.Join(result.Messages, "\n")
	if result.Error != "" || !strings.Contains(report, "not reachable") || !strings.Contains(report, "Journal: freed 12 MiB") {
		t.Fatalf("%q\n%s", result.Error, report)
	}
}

func TestImageBuildsUseBuildKit(t *testing.T) {
	saved := probeDockerBuild
	t.Cleanup(func() {
		probeDockerBuild = saved
		buildModeMu.Lock()
		buildModeChecked = time.Time{}
		buildModeMu.Unlock()
	})
	for _, c := range []struct {
		mode dockerBuildMode
		args string
		env  []string
	}{
		{dockerBuildMode{Buildx: true}, "buildx build --load --progress=plain --tag lidza/shop:d1 .", nil},
		{dockerBuildMode{Env: "DOCKER_BUILDKIT=1"}, "build --progress=plain --tag lidza/shop:d1 .", []string{"DOCKER_BUILDKIT=1"}},
	} {
		probeDockerBuild = func(context.Context) dockerBuildMode { return c.mode }
		buildModeMu.Lock()
		buildModeChecked = time.Time{}
		buildModeMu.Unlock()
		args, env := dockerBuildInvocation(context.Background(), []string{"build", "--progress=plain", "--tag", "lidza/shop:d1", "."})
		if strings.Join(args, " ") != c.args || !slices.Equal(env, c.env) {
			t.Fatal(c.mode, args, env)
		}
	}
}

func TestCleanupLinesAreSummaries(t *testing.T) {
	banner := "DEPRECATED: The legacy builder is deprecated and will be removed in a future release.\n            Install the buildx component to build images with BuildKit:\n            https://docs.docker.com/go/buildx/\n"
	if got := summarizeBuilderPrune(banner+"ID\tRECLAIMABLE\nabc\ttrue\nTotal:\t1.2GB\n", nil); got != "Docker build cache: freed 1.2GB" {
		t.Fatal(got)
	}
	if got := summarizeBuilderPrune(banner+"Total reclaimed space: 0B\n", nil); got != "Docker build cache: nothing to free" {
		t.Fatal(got)
	}
	if got := summarizeBuilderPrune(banner+"permission denied", errors.New("exit status 1")); strings.Contains(got, "DEPRECATED") || !strings.Contains(got, "permission denied") {
		t.Fatal("error output", got)
	}
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if slices.Contains(args, "--disk-usage") {
			return "Archived and active journals take up 24.0M in the file system.", nil
		}
		return "Vacuuming done, freed 0B of archived journals from /run/log/journal.\nVacuuming done, freed 0B of archived journals from /var/log/journal.", nil
	}
	if got, err := vacuumJournal(context.Background(), run, 7); err != nil || got != "Journal: nothing older than 7 days to vacuum (24.0M in use)" {
		t.Fatal(got, err)
	}
}
