package control

import (
	"context"
	"encoding/json"
	"github.com/agim/lidza/packs/storage"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRemoteBackupInventoryAndPrivateDownload(t *testing.T) {
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") == "" {
			t.Error("unsigned S3 request")
		}
		switch r.Method {
		case "GET":
			if r.URL.Query().Get("prefix") != "saved/databases/" {
				t.Error("wrong prefix", r.URL)
			}
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<ListBucketResult><Contents><Key>saved/databases/app/snapshot.dump</Key><LastModified>2026-10-11T12:00:00Z</LastModified><Size>4096</Size></Contents><Contents><Key>saved/databases/app/not-a-backup.txt</Key><Size>1</Size></Contents></ListBucketResult>`))
		case "HEAD":
			w.Header().Set("Content-Length", "4096")
		default:
			w.WriteHeader(400)
		}
	}))
	defer remote.Close()
	c, err := New(Config{User: "operator@example.com", Password: "fixture-password-16-chars", PublicURL: "http://localhost:3000", DataDir: t.TempDir(), Key: []byte(strings.Repeat("a", 32))})
	if err != nil {
		t.Fatal(err)
	}
	c.data.BackupStorage = &storage.Config{Provider: "s3", Endpoint: remote.URL, Bucket: "private", Region: "nyc3", Prefix: "saved", AccessKey: "ACCESS-SECRET-FIXTURE", SecretKey: "SECRET-FIXTURE", Timeout: time.Second}
	w := httptest.NewRecorder()
	c.remoteBackups(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var result struct {
		Objects []storage.Object `json:"objects"`
		Bucket  string           `json:"bucket"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Objects) != 1 || result.Objects[0].Key != "databases/app/snapshot.dump" || result.Objects[0].Size != 4096 {
		t.Fatal(w.Body)
	}
	if strings.Contains(w.Body.String(), "SECRET-FIXTURE") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("unsafe inventory")
	}
	w = httptest.NewRecorder()
	c.remoteBackupDownload(w, httptest.NewRequest("GET", "/?key=databases/app/snapshot.dump", nil))
	if w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "X-Amz-Signature=") {
		t.Fatal(w.Code, w.Header())
	}
	before := calls
	w = httptest.NewRecorder()
	c.remoteBackupDownload(w, httptest.NewRequest("GET", "/?key=databases/app/../../credentials.dump", nil))
	if w.Code != 400 || calls != before {
		t.Fatal("invalid object contacted provider")
	}

}
func TestRemoteBackupListingFailure(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte("provider secret should not leak"))
	}))
	defer remote.Close()
	c, err := New(Config{User: "operator@example.com", Password: "fixture-password-16-chars", PublicURL: "http://localhost:3000", DataDir: t.TempDir(), Key: []byte(strings.Repeat("b", 32))})
	if err != nil {
		t.Fatal(err)
	}
	c.data.BackupStorage = &storage.Config{Provider: "s3", Endpoint: remote.URL, Bucket: "private", Region: "nyc3", AccessKey: "access", SecretKey: "secret"}
	w := httptest.NewRecorder()
	c.remoteBackups(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 502 || strings.Contains(w.Body.String(), "provider secret") || !strings.Contains(w.Body.String(), "ListBucket") {
		t.Fatal(w.Code, w.Body)
	}
}

func TestSpacesBucketURLUsesFrameworkAddressingOption(t *testing.T) {
	cfg := storage.Config{Provider: "s3", Endpoint: "https://kova.nyc3.digitaloceanspaces.com", Bucket: "kova", Region: "nyc3", AccessKey: "access", SecretKey: "secret", Prefix: "saved"}
	correct := backupStorageConfig(cfg)
	if correct.PathStyle == nil || *correct.PathStyle {
		t.Fatal("bucket-specific endpoint still uses path-style")
	}
	target, err := storage.New(correct)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := target.PresignPut(context.Background(), "databases/app/snapshot.dump", time.Minute, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(signed)
	if u.Host != "kova.nyc3.digitaloceanspaces.com" || u.Path != "/saved/databases/app/snapshot.dump" {
		t.Fatal("unexpected object location", u.Host, u.Path)
	}
	cfg.Endpoint = "https://nyc3.digitaloceanspaces.com"
	if backupStorageConfig(cfg).PathStyle != nil {
		t.Fatal("regional endpoint altered")
	}
	c := &Control{}
	c.data.BackupStorage = &cfg
	_, legacy, configured := c.backupDestination()
	if !configured || legacy != "kova/saved" {
		t.Fatal("regional endpoint lost older extra-folder backups", legacy)
	}
}

func TestStoragePartialCredentialReplacementRejected(t *testing.T) {
	c := &Control{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/", strings.NewReader(`{"endpoint":"https://nyc3.digitaloceanspaces.com","bucket":"kova","region":"nyc3","access_key":"replacement-only"}`))
	c.configureStorage(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "enter both access and secret keys") {
		t.Fatal(w.Code, w.Body)
	}
}
