package agent

import (
	"bytes"
	"context"
	"github.com/agim/lidza/packs/storage"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRealS3BackupUploadListAndDownload(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	name := "lidza-s3-test-" + newID()[:10]
	authFile := filepath.Join(t.TempDir(), "s3.json")
	if err := os.WriteFile(authFile, []byte(`{"identities":[{"name":"backup-fixture","credentials":[{"accessKey":"fixture-admin","secretKey":"fixture-secret-16"}],"actions":["Admin","Read","Write","List","Tagging"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, "", nil, "docker", "run", "-d", "--rm", "--name", name, "--memory", "512m", "--tmpfs", "/data:rw,size=256m", "-v", authFile+":/etc/s3.json:ro", "-p", "127.0.0.1::8333", "chrislusf/seaweedfs:4.12@sha256:d7f3fdf6fb9c9375551e95bc985abae74a157789d868341e9e84a999ed8ea21c", "server", "-s3", "-s3.config=/etc/s3.json", "-dir=/data", "-volume.max=10", "-master.volumeSizeLimitMB=10", "-ip=127.0.0.1", "-ip.bind=0.0.0.0"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if t.Failed() {
			logs, _ := command(cleanup, "", nil, "docker", "logs", "--tail", "30", name)
			t.Log(logs)
		}
		command(cleanup, "", nil, "docker", "rm", "-f", name)
	}()
	address, err := command(ctx, "", nil, "docker", "port", name, "8333/tcp")
	if err != nil {
		t.Fatal(err)
	}
	cfg := storage.Config{Provider: "s3", Endpoint: "http://" + strings.TrimSpace(address), Bucket: "private-backups", Region: "us-east-1", Prefix: "lidza-deploy", AccessKey: "fixture-admin", SecretKey: "fixture-secret-16", Timeout: 10 * time.Second}
	target, err := storage.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = target.EnsureBucket(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	wrong := cfg
	wrong.SecretKey = "wrong-secret"
	unauthorized, _ := storage.New(wrong)
	if _, err := unauthorized.List(ctx, "databases/", 10); err == nil {
		t.Fatal("unsigned/invalid credential access accepted by private object server")
	}
	data := bytes.Repeat([]byte("database-backup-fixture\n"), 1000)
	file := filepath.Join(t.TempDir(), "backup.dump")
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	key := "databases/fixture/predeployment.dump"
	// Bucket creation precedes the gateway's first volume-registration heartbeat.
	// Retry only fixture startup errors; production upload errors remain visible.
	deadline = time.Now().Add(30 * time.Second)
	for {
		err = uploadBackup(ctx, cfg, file, key, int64(len(data)))
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "HTTP 500") && !strings.Contains(err.Error(), "HTTP 503") {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	objects, err := target.List(ctx, "databases/", 1000)
	if err != nil || len(objects) != 1 || objects[0].Key != key || objects[0].Size != int64(len(data)) {
		t.Fatal(objects, err)
	}
	reader, _, err := target.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	received, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(received, data) {
		t.Fatal("remote download bytes changed", err)
	}
	// A second rolling upload replaces the same object rather than retaining two copies.
	if err = uploadBackup(ctx, cfg, file, key, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	objects, err = target.List(ctx, "databases/", 1000)
	if err != nil || len(objects) != 1 {
		t.Fatal("rolling remote copy duplicated", objects, err)
	}
}
