package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

func TestPredeploymentBackupLimitFollowsHistoryAndSize(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name    string
		floor   time.Duration
		history []BackupRecord
		size    int64
		want    time.Duration
		why     []string
	}{
		{name: "unknown", want: 10 * time.Minute, why: []string{"minimum decides", "no timed backup yet", "current size unknown"}},
		{name: "configured minimum", floor: 30 * time.Minute, size: 100 << 20, want: 30 * time.Minute, why: []string{"minimum 30 minutes"}},
		// A 24 GiB database at 4 MiB/s needs about 102 minutes, plus 5.
		{name: "large database", size: 24 << 30, want: 107 * time.Minute, why: []string{"database size decides", "24.0 GiB"}},
		{name: "last backup", size: 1 << 30, want: 85 * time.Minute, history: []BackupRecord{
			{Created: now.Add(-48 * time.Hour), Seconds: 300, Size: 1 << 30},
			{Created: now.Add(-24 * time.Hour), Seconds: 40 * 60, Size: 3 << 30},
			// Newer, but failed off-site or untimed: not evidence.
			{Created: now.Add(-time.Hour), Seconds: 10, OffsiteRequested: true, UploadError: "upload failed"},
			{Created: now, Size: 1 << 30},
		}, why: []string{"last backup decides", "took 40 minutes", "85 minutes"}},
		{name: "ceiling", size: 200 << 30, history: []BackupRecord{{Created: now, Seconds: 3 * 3600}}, want: 4 * time.Hour, why: []string{"ceiling decides", "capped at 240 minutes"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := predeploymentBackupLimit(c.floor, c.history, c.size)
			if got.Limit != c.want {
				t.Fatalf("limit %s, want %s (%s)", got.Limit, c.want, got.Why)
			}
			for _, part := range c.why {
				if !strings.Contains(got.Why, part) {
					t.Fatalf("reason %q lacks %q", got.Why, part)
				}
			}
		})
	}
}

func TestBackupLimitForNeverExceedsTheOperationsDeadline(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	limits := map[string]backupLimit{"db": {Limit: 2 * time.Hour, Why: "database size decides"}}
	ctx, cancel := context.WithTimeout(withBackupLimits(context.Background(), limits), 3*time.Minute)
	defer cancel()
	got := m.backupLimitFor(ctx, "db")
	if got.Limit > 3*time.Minute || !strings.Contains(got.Why, "remaining") {
		t.Fatal(got)
	}
	if got := m.backupLimitFor(withBackupLimits(context.Background(), limits), "db"); got.Limit != 2*time.Hour {
		t.Fatal("precomputed release limit ignored", got)
	}
}

// The deploy log states the computed limit and, for a native release, that
// release steps and publication follow, not an image build. A successful
// backup records its duration for the next limit.
func TestPredeploymentBackupStatesLimitAndWhatFollows(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\ncase \"$1\" in\nrun) printf 'fixture-dump';;\nesac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	m := testManager(t, &fakeRuntime{})
	app := testApp("backup-limit")
	m.mu.Lock()
	m.data.Apps[app.ID] = app
	m.data.Databases[app.ID] = Database{AppID: app.ID, Ready: true, URL: "postgres://app:fixture-secret@db/app",
		Backups: []BackupRecord{{ID: "old", Created: time.Now().Add(-time.Hour), Seconds: 20 * 60, Size: 1 << 30}}}
	m.data.Deployments = []Deployment{{ID: "limit-test"}}
	m.mu.Unlock()
	ctx := m.deploymentDiagnostics(context.Background(), job{app: app, deployment: "limit-test"})
	backup := nativeReleaseBackupThen(func(ctx context.Context) error { return m.backupBeforeRelease(ctx, []string{app.ID}) }, release.Rails(release.RailsOptions{}))
	if err := backup(ctx); err != nil {
		t.Fatal(err)
	}
	log := m.Deployments()[0].Log
	for _, want := range []string{"Pre-deployment backup limit for backup-limit: 45 minutes (last backup decides", "(limit: 45 minutes).", "The release steps (db:prepare) run after this completes; then the new release is published"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "image build") || strings.Contains(log, "S3 upload") {
		t.Fatal("container or off-site wording in a native local backup:\n", log)
	}
	backups := m.databaseViews()[0].Backups
	if latest := backups[len(backups)-1]; latest.Kind != predeploymentBackup || latest.Seconds <= 0 {
		t.Fatal("backup duration not recorded", latest)
	}
	// Container deployments name the image build.
	m.mu.Lock()
	m.data.Deployments = []Deployment{{ID: "container"}}
	m.mu.Unlock()
	ctx = m.deploymentDiagnostics(context.Background(), job{app: app, deployment: "container"})
	if err := m.backupBeforeRelease(withBackupThen(ctx, "The image build starts after this completes."), []string{app.ID}); err != nil {
		t.Fatal(err)
	}
	if log := m.Deployments()[0].Log; !strings.Contains(log, "The image build starts after this completes.") {
		t.Fatal(log)
	}
}

// By default every bound database stays gated: an unready secondary
// database fails the release before any backup. The per-database
// before-deploy policies are covered in before_deploy_test.go.
func TestNativeReleaseGateCoversEveryBoundDatabaseByDefault(t *testing.T) {
	m, a, _, _ := nativeGateFixture(t)
	d := m.data.Databases["native-scraper"]
	d.Ready = false
	m.data.Databases["native-scraper"] = d
	if targets := m.nativeReleaseBackupTargetsLocked(a.ID); len(targets) != 2 {
		t.Fatal("release backup targets", targets)
	}
	ctx := m.deploymentDiagnostics(context.Background(), job{app: App{ID: a.ID}, deployment: "gate"})
	_, err := m.backupBeforeNativeRailsRelease(ctx, a.ID, []string{"primary", "queue"})
	if log := m.data.Deployments[0].Log; err == nil || !strings.Contains(err.Error(), "ready resources") || strings.Contains(log, "Skipping") {
		t.Fatal("default must gate every bound database", err, log)
	}
}

func TestNativeDumpRuntimeFollowsTheDeadline(t *testing.T) {
	if got := nativeDumpRuntimeSeconds(context.Background()); got != "1800" {
		t.Fatal(got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if got := nativeDumpRuntimeSeconds(ctx); got < "7200" || got > "7260" {
		t.Fatal(got)
	}
	long, stop := context.WithTimeout(context.Background(), 9*time.Hour)
	defer stop()
	if got := nativeDumpRuntimeSeconds(long); got != "18000" {
		t.Fatal(got)
	}
}
