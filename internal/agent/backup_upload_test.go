package agent

import (
	"context"
	"github.com/agim/lidza/packs/storage"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetryRetainedBackupUpload(t *testing.T) {
	var received string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PUT":
			b, _ := io.ReadAll(r.Body)
			received = string(b)
		case "HEAD":
			w.Header().Set("Content-Length", "4")
		default:
			w.WriteHeader(405)
		}
	}))
	defer remote.Close()
	m, err := NewManager(context.Background(), Config{DataDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	id := "retained"
	record := BackupRecord{ID: "fixture", Size: 4, UploadError: "previous failure"}
	m.data.Databases[id] = Database{AppID: id, Backups: []BackupRecord{record}}
	m.data.BackupStorage = &storage.Config{Provider: "s3", Endpoint: remote.URL, Bucket: "private", Region: "nyc3", Prefix: "saved", AccessKey: "access", SecretKey: "secret"}
	if err = os.MkdirAll(m.backupDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(m.backupDir(id), "fixture.dump"), []byte("dump"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = m.retryBackupUpload(id, "missing"); err == nil {
		t.Fatal("missing record accepted")
	}
	if err = m.retryBackupUpload(id, "fixture"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		d := m.data.Databases[id]
		m.mu.Unlock()
		if d.Operation == "" {
			if len(d.Backups) != 1 || !d.Backups[0].Offsite || d.Backups[0].UploadError != "" || d.Backups[0].ObjectKey != "databases/retained/fixture.dump" || received != "dump" {
				t.Fatal(d, received)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("upload did not finish")
}
func TestRetryUploadFailurePreservesLocalRecord(t *testing.T) {
	m, err := NewManager(context.Background(), Config{DataDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.data.Databases["app"] = Database{AppID: "app", Backups: []BackupRecord{{ID: "snapshot", Size: 4}}}
	m.data.BackupStorage = &storage.Config{Provider: "s3", Endpoint: "http://127.0.0.1:1", Bucket: "private", Region: "nyc3", AccessKey: "access", SecretKey: "secret"}
	if err = m.retryBackupUpload("app", "snapshot"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		d := m.data.Databases["app"]
		m.mu.Unlock()
		if d.Operation == "" {
			if len(d.Backups) != 1 || d.Backups[0].Offsite || d.Backups[0].UploadError == "" || !strings.Contains(d.Error, "Local backup retained") {
				t.Fatal(d)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("failure did not finish")
}
