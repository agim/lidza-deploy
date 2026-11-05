package agent

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeStaticCaddyKeepsOpenDownloadAndAssetsAcrossPublishAndRollback(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for isolated native static publication acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := command(ctx, "", nil, "docker", "info", "--format", "{{.Name}}"); err != nil || os.Getenv("DOCKER_HOST") != "" {
		t.Fatal("requires an isolated local Docker engine", err)
	}
	m, a, app := nativeStaticPublisherFixture(t)
	root := m.cfg.Native.Root
	if err := filepath.WalkDir(app, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, 1000, 1000)
	}); err != nil {
		t.Fatal(err)
	}
	// Expose only this disposable fixture to its mapped non-root serving user.
	for p := root; strings.HasPrefix(p, filepath.Join(os.TempDir(), "TestNativeStaticCaddy")); p = filepath.Dir(p) {
		if err := os.Chmod(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	payload := strings.Repeat("existing-download-", 1<<20)
	if err := os.WriteFile(filepath.Join(app, "current/public/download.txt"), []byte(payload), 0644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "test.Caddyfile")
	if err := os.WriteFile(config, []byte("{\n admin off\n auto_https off\n persist_config off\n}\n:3000 {\n root * "+filepath.Join(app, "current/public")+"\n file_server\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	name := "lidza-native-static-test-" + newID()
	defer command(context.Background(), "", nil, "docker", "rm", "-f", "-v", name)
	if _, err := command(ctx, "", nil, "docker", "run", "--detach", "--name", name, "--user", "1000:1000", "--read-only", "--tmpfs", "/tmp:rw,nosuid,size=32m", "--env", "HOME=/tmp", "--env", "XDG_CONFIG_HOME=/tmp/config", "--env", "XDG_DATA_HOME=/tmp/data", "--mount", "type=bind,source="+root+",target="+root+",readonly", "--publish", "127.0.0.1::3000", "caddy:2-alpine", "caddy", "run", "--config", config, "--adapter", "caddyfile"); err != nil {
		t.Fatal(err)
	}
	port, err := command(ctx, "", nil, "docker", "inspect", "--format", `{{(index (index .NetworkSettings.Ports "3000/tcp") 0).HostPort}}`, name)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Minute}
	base := "http://127.0.0.1:" + port
	var response *http.Response
	for attempt := 0; attempt < 50; attempt++ {
		response, err = client.Get(base + "/download.txt")
		if err == nil && response.StatusCode == 200 {
			break
		}
		if response != nil {
			response.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || response == nil || response.StatusCode != 200 {
		t.Fatal("fixture static server did not become ready", err)
	}
	defer response.Body.Close()
	prefix := make([]byte, 1024)
	if _, err = io.ReadFull(response.Body, prefix); err != nil {
		t.Fatal(err)
	}
	release, err := m.prepareNativeStatic(ctx, a, newID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.publishNativeStatic(a.ID, release); err != nil {
		t.Fatal(err)
	}
	remainder, err := io.ReadAll(response.Body)
	if err != nil || string(prefix)+string(remainder) != payload {
		t.Fatal("publication interrupted an existing file download", err)
	}
	assertGet := func(path, contains string) {
		t.Helper()
		r, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		if r.StatusCode != 200 || !strings.Contains(string(body), contains) {
			t.Fatal("static release or retained asset unavailable", path, r.StatusCode)
		}
	}
	assertGet("/", "New release")
	assertGet("/_astro/old.hash.js", "old hashed asset")
	if err = m.rollbackNativeStatic(a.ID); err != nil {
		t.Fatal(err)
	}
	assertGet("/", "Live")
	assertGet("/_astro/new.hash.js", "new hashed asset")
}
