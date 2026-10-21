package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agim/lidza/packs/storage"
)

func TestBackupProgressStopsAndReportsBytes(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	ctx := context.WithValue(context.Background(), diagnosticKey{}, func(s string) { mu.Lock(); defer mu.Unlock(); lines = append(lines, s) })
	var bytes atomic.Int64
	bytes.Store(2 << 20)
	stop := backupProgressEvery(ctx, "Dumping PostgreSQL", bytes.Load, time.Millisecond)
	deadline := time.After(time.Second)
	for {
		mu.Lock()
		n := len(lines)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no heartbeat")
		case <-time.After(time.Millisecond):
		}
	}
	stop()
	stop()
	mu.Lock()
	n := len(lines)
	text := strings.Join(lines, "")
	mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != n || !strings.Contains(text, "2.0 MiB processed") {
		t.Fatal(lines)
	}
}

func TestPredeploymentBackupDeadlineReleasesSlotAndRetainsRelease(t *testing.T) {
	for _, stage := range []string{"slot", "dump", "s3"} {
		t.Run(stage, func(t *testing.T) {
			bin := t.TempDir()
			script := "#!/bin/sh\ncase \"$1\" in\nrun) exec sleep 30;;\n*) exit 0;;\nesac\n"
			if stage == "s3" {
				script = "#!/bin/sh\ncase \"$1\" in\nrun) printf 'fixture-dump';;\nesac\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			m := testManager(t, &fakeRuntime{})
			app := testApp("backup-deadline")
			app.Current = &Release{ID: "previous", Container: "previous"}
			m.mu.Lock()
			m.data.Apps[app.ID] = app
			m.data.Databases[app.ID] = Database{AppID: app.ID, Ready: true, URL: "postgres://app:fixture-secret@db/app"}
			m.data.Deployments = []Deployment{{ID: "backup-test"}}
			m.mu.Unlock()
			if stage == "slot" {
				m.databaseSlots <- struct{}{}
				m.databaseSlots <- struct{}{}
				defer func() { <-m.databaseSlots; <-m.databaseSlots }()
			}
			if stage == "s3" {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
				defer server.Close()
				m.mu.Lock()
				d := m.data.Databases[app.ID]
				d.Backup.Offsite = true
				m.data.Databases[app.ID] = d
				m.data.BackupStorage = &storage.Config{Provider: "s3", Endpoint: server.URL, Bucket: "test", Region: "us-east-1", AccessKey: "fixture-key", SecretKey: "fixture-secret"}
				m.mu.Unlock()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			ctx = m.deploymentDiagnostics(ctx, job{app: app, deployment: "backup-test"})
			start := time.Now()
			err := m.backupBeforeRelease(ctx, []string{app.ID})
			if err == nil || !strings.Contains(err.Error(), "deadline exceeded") || time.Since(start) > 2*time.Second {
				t.Fatal(err, time.Since(start))
			}
			d := m.databaseViews()[0]
			if d.Operation != "" || m.Current(app.ID).ID != "previous" || (stage != "slot" && len(m.databaseSlots) != 0) {
				t.Fatal("backup did not release state", d)
			}
			log := m.Deployments()[0].Log
			want := map[string]string{"slot": "Waiting for an available", "dump": "Dumping PostgreSQL", "s3": "Uploading required S3"}[stage]
			if !strings.Contains(log, want) || !strings.Contains(log, "current release retained") || strings.Contains(log, "fixture-secret") {
				t.Fatal(log)
			}
			if stage == "s3" {
				if len(d.Backups) != 1 || d.Backups[0].Offsite || d.Backups[0].UploadError == "" {
					t.Fatal("local backup lost", d)
				}
				raw, e := os.ReadFile(filepath.Join(m.backupDir(app.ID), d.Backups[0].ID+".dump"))
				if e != nil || string(raw) != "fixture-dump" {
					t.Fatal(e, string(raw))
				}
			}
		})
	}
}

func TestPostgresLockFailureIsClassifiedWithoutRawOutput(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho 'ERROR: canceling statement due to lock timeout fixture-secret' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	err := dockerStream(context.Background(), nil, io.Discard, nil, "run", "pg_dump")
	if err == nil || !strings.Contains(err.Error(), "lock wait exceeded") || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho 'ERROR: canceling statement due to statement timeout; query was LOCK TABLE fixture-secret' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	err = dockerStream(context.Background(), nil, io.Discard, nil, "run", "pg_dump")
	if err == nil || !strings.Contains(err.Error(), "lock wait exceeded") || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = dockerStream(ctx, nil, io.Discard, nil, "run", "pg_dump"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPredeploymentBackupTimeoutConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value int
		valid bool
	}{{0, true}, {1, true}, {120, true}, {-1, false}, {121, false}} {
		file := filepath.Join(t.TempDir(), "agent.json")
		raw := fmt.Sprintf(`{"data_dir":"/var/lib/lidza-agent","api_key":"%s","predeployment_backup_minutes":%d}`, strings.Repeat("a", 32), tc.value)
		if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(file)
		if (err == nil) != tc.valid {
			t.Fatalf("minutes %d: %v", tc.value, err)
		}
	}
}
