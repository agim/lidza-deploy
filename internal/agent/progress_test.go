package agent

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProgressTrackerCountsBytesAndNoticesActivity(t *testing.T) {
	clock := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	tracker := &ProgressTracker{now: func() time.Time { return clock }}
	var done atomic.Int64
	tracker.Stage("Dumping PostgreSQL", 0, done.Load)
	started := clock
	clock = clock.Add(5 * time.Second)
	if p := tracker.Snapshot(); p.DoneBytes != 0 || !p.UpdatedAt.Equal(started) || !p.StartedAt.Equal(started) {
		t.Fatal("an idle phase reports no activity", p)
	}
	done.Store(4096)
	p := tracker.Snapshot()
	if p.DoneBytes != 4096 || !p.UpdatedAt.Equal(clock) || p.TotalBytes != 0 || p.Phase != "Dumping PostgreSQL" {
		t.Fatal("new bytes are activity", p)
	}
	clock = clock.Add(time.Minute)
	if p := tracker.Snapshot(); !p.UpdatedAt.Equal(started.Add(5 * time.Second)) {
		t.Fatal("unchanged bytes keep the last activity", p)
	}
	tracker.Stage("Uploading", 8192, nil)
	if p := tracker.Snapshot(); p.DoneBytes != 0 || p.TotalBytes != 8192 || !p.StartedAt.Equal(clock) {
		t.Fatal("a new phase starts from zero", p)
	}

	// Remote progress keeps its own times, but never later than now.
	remote := Progress{Phase: "Archiving files", DoneBytes: 10, TotalBytes: 20, StartedAt: clock.Add(-time.Minute), UpdatedAt: clock.Add(time.Hour)}
	tracker.Mirror("archiving uploads on alpha", remote)
	if p := tracker.Snapshot(); p.Phase != "archiving uploads on alpha" || p.DoneBytes != 10 || p.TotalBytes != 20 || !p.StartedAt.Equal(clock.Add(-time.Minute)) || !p.UpdatedAt.Equal(clock) {
		t.Fatal(p)
	}

	var none *ProgressTracker
	none.Stage("ignored", 1, nil)
	none.Mirror("ignored", remote)
	if p := none.Snapshot(); p.Phase != "" {
		t.Fatal(p)
	}
}

func TestProgressRegistryTracksRunningOperations(t *testing.T) {
	var registry progressRegistry
	first, endFirst := registry.track("move/m1/uploads", "Measuring files")
	_, endOther := registry.track("move/m2/uploads", "Measuring files")
	defer endOther()
	first.Stage("Archiving files", 100, func() int64 { return 40 })
	sets := registry.prefixed("move/m1/")
	if len(sets) != 1 || sets["uploads"].DoneBytes != 40 || sets["uploads"].TotalBytes != 100 {
		t.Fatal(sets)
	}
	// A retry replaces the tracker; the earlier operation's end keeps it.
	second, endSecond := registry.track("move/m1/uploads", "Measuring files")
	endFirst()
	if p := registry.snapshot("move/m1/uploads"); p == nil || p.Phase != "Measuring files" {
		t.Fatal("the retry's progress was removed", p)
	}
	second.Stage("Archiving files", 0, nil)
	endSecond()
	if p := registry.snapshot("move/m1/uploads"); p != nil {
		t.Fatal("ended progress remains", p)
	}
}

func TestBackupStagesReportIntoTheOperationsProgress(t *testing.T) {
	tracker := NewProgressTracker("Waiting")
	ctx := withProgress(context.Background(), tracker)
	var sent atomic.Int64
	sent.Store(3 << 20)
	stop := backupProgressSized(ctx, "Uploading required S3 copy", sent.Load, 9<<20)
	stop()
	if p := tracker.Snapshot(); p.Phase != "Uploading required S3 copy" || p.DoneBytes != 3<<20 || p.TotalBytes != 9<<20 {
		t.Fatal("stages report without deployment diagnostics", p)
	}
}

func TestDatabaseViewShowsLiveDumpProgress(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\nrun) printf 'PGDMP partial dump'; exec sleep 30;;\n*) exit 0;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	m := testManager(t, &fakeRuntime{})
	app := testApp("progress")
	app.Current = &Release{ID: "current", Container: "current", DatabaseIDs: []string{"progress"}}
	m.mu.Lock()
	m.data.Apps[app.ID] = app
	m.data.Databases[app.ID] = Database{AppID: app.ID, Mode: "local", Ready: true, URL: "postgres://app:fixture-secret@db/app"}
	m.mu.Unlock()
	if err := m.databaseAction(app.ID, "backup"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		views := m.databaseViews()
		if len(views) == 1 && views[0].Progress != nil && views[0].Progress.DoneBytes == int64(len("PGDMP partial dump")) {
			p := views[0].Progress
			if views[0].Operation != "backup" || !strings.HasPrefix(p.Phase, "Dumping PostgreSQL") || p.TotalBytes != 0 || p.StartedAt.IsZero() || p.UpdatedAt.Before(p.StartedAt) {
				t.Fatal(views[0])
			}
			raw, _ := json.Marshal(views[0])
			if strings.Contains(string(raw), "fixture-secret") || !strings.Contains(string(raw), `"progress":{"phase":"Dumping PostgreSQL`) {
				t.Fatal(string(raw))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no live dump progress", views)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMoveArchiveProgressCountsFileBytes(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.txt": strings.Repeat("a", 1000), "nested/b.txt": strings.Repeat("b", 500)})
	var read atomic.Int64
	if _, err := moveWriteArchive(withMoveArchiveCounter(context.Background(), &read), dir, false, compressionGzip, &moveCountingWriter{}, 1<<30); err != nil {
		t.Fatal(err)
	}
	if read.Load() != 1500 {
		t.Fatal("archived file bytes", read.Load())
	}
}

func TestMoveFileProgressRouteListsTheMovesSets(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	ctx, end := m.trackMoveFiles(context.Background(), moveTestMove, "shared-uploads", "Measuring files")
	defer end()
	progressFrom(ctx).Stage("Archiving files", 2048, func() int64 { return 512 })
	_, endOther := m.trackMoveFiles(context.Background(), "abcdefabcdefabcdefabcdef", "shared-uploads", "Measuring files")
	defer endOther()
	_, endDatabase := m.trackDatabase(context.Background(), "shop", "Waiting")
	defer endDatabase()

	request := httptest.NewRequest("GET", "/v1/move/files/"+moveTestMove+"/progress", nil)
	request.SetPathValue("move", moveTestMove)
	recorder := httptest.NewRecorder()
	m.moveFileProgressRoute(recorder, request)
	var out MoveFileProgress
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil || recorder.Code != 200 {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	if len(out.Sets) != 1 || out.Sets["shared-uploads"].Phase != "Archiving files" || out.Sets["shared-uploads"].DoneBytes != 512 || out.Sets["shared-uploads"].TotalBytes != 2048 {
		t.Fatal(out)
	}
	request = httptest.NewRequest("GET", "/v1/move/files/x/progress", nil)
	request.SetPathValue("move", "../x")
	recorder = httptest.NewRecorder()
	m.moveFileProgressRoute(recorder, request)
	if recorder.Code != 400 {
		t.Fatal(recorder.Code)
	}
}

func TestDeploymentOutputRecordsItsLastActivity(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	app := testApp("activity")
	m.mu.Lock()
	m.data.Deployments = []Deployment{{ID: "activity-build", AppID: app.ID, Status: "building"}}
	m.mu.Unlock()
	ctx := m.deploymentDiagnostics(context.Background(), job{app: app, deployment: "activity-build"})
	before := time.Now().UTC().Add(-time.Second)
	commandDiagnostic(ctx, "build", "==> go build")
	d := m.Deployments()[0]
	if d.Updated == nil || d.Updated.Before(before) {
		t.Fatal("output did not record activity", d.Updated)
	}
}
