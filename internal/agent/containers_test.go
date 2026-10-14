package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagedContainerInventoryAndGuardedRestart(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("portal")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	a.Current = &Release{Container: "web-current"}
	a.Previous = &Release{Container: "web-previous"}
	m.data.Apps[a.ID] = a
	m.data.Tasks["portal:worker"] = Task{ID: "mailer", AppID: a.ID, Mode: "worker", Container: "worker-mailer"}
	m.data.Databases["portal"] = Database{AppID: a.ID, Mode: "local", Network: "db-portal"}
	m.data.Caches[a.ID] = CacheResource{Mode: "local", Network: "cache-portal"}
	m.data.ClickHouses[a.ID] = ClickHouseResource{Network: "clickhouse-portal"}
	m.mu.Unlock()

	bin := t.TempDir()
	restarted := filepath.Join(bin, "restarted")
	script := `#!/bin/sh
case "$1" in
  container)
    printf '%s\n' '{"ID":"1234567890abcdef","Names":"web-current","State":"running","Status":"Up 1 minute (unhealthy)"}' '{"ID":"abcdef1234567890","Names":"web-previous","State":"exited","Status":"Exited (0) 1 hour ago"}' '{"ID":"22","Names":"worker-mailer","State":"running","Status":"Up 1 minute"}' '{"ID":"33","Names":"db-portal","State":"running","Status":"Up 1 minute"}' '{"ID":"44","Names":"cache-portal","State":"running","Status":"Up 1 minute"}' '{"ID":"55","Names":"clickhouse-portal","State":"running","Status":"Up 1 minute"}'
    ;;
  stats)
    printf '%s\n' '{"Name":"web-current","CPUPerc":"2.50%","MemUsage":"64MiB / 512MiB","MemPerc":"12.50%","NetIO":"1MB / 2MB","BlockIO":"3MB / 4MB","PIDs":"7"}'
    ;;
  restart)
    printf '%s' "$4" > ` + restarted + `
    printf '%s\n' "$4"
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	mux := http.NewServeMux()
	m.containerRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/containers", nil))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var inventory ContainerInventory
	if err := json.Unmarshal(w.Body.Bytes(), &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory.Containers) != 6 {
		t.Fatal(inventory.Containers)
	}
	web := inventory.Containers[4]
	for _, value := range inventory.Containers {
		if value.Name == "web-current" {
			web = value
		}
	}
	if web.AppID != a.ID || web.CPU != "2.50%" || web.Memory != "64MiB / 512MiB" || web.Health != "unhealthy" || !web.NeedsAttention || !web.Restartable {
		t.Fatal(web)
	}

	body := bytes.NewBufferString(`{"name":"web-previous"}`)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/containers/restart", body))
	if w.Code != http.StatusConflict {
		t.Fatal("retained release restart was allowed", w.Code)
	}
	body = bytes.NewBufferString(`{"name":"unrelated-host-container"}`)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/containers/restart", body))
	if w.Code != http.StatusNotFound {
		t.Fatal("unowned restart was allowed", w.Code)
	}
	body = bytes.NewBufferString(`{"name":"web-current"}`)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/containers/restart", body))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	value, err := os.ReadFile(restarted)
	if err != nil || strings.TrimSpace(string(value)) != "web-current" {
		t.Fatal("owned restart did not reach Docker", string(value), err)
	}
}

func TestContainerInventoryRealDocker(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for real container inventory and restart")
	}
	name := fmt.Sprintf("lidza-inventory-test-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := command(ctx, "", nil, "docker", "run", "--detach", "--name", name, "valkey/valkey:8-alpine"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_, _ = command(cleanup, "", nil, "docker", "rm", "--force", name)
	})
	m := testManager(t, &fakeRuntime{})
	a := testApp("inventory")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.data.Caches[a.ID] = CacheResource{Mode: "local", Network: name, Ready: true}
	m.mu.Unlock()
	var view ContainerView
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		inventory := m.containerInventory(context.Background())
		if len(inventory.Containers) == 1 && inventory.Containers[0].State == "running" && inventory.Containers[0].Memory != "" {
			view = inventory.Containers[0]
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if view.Name != name || view.Role != "Valkey cache" || view.CPU == "" || !view.Restartable {
		t.Fatal(view)
	}
	mux := http.NewServeMux()
	m.containerRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/containers/restart", bytes.NewBufferString(`{"name":"`+name+`"}`)))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestHealthFromContainerStatus(t *testing.T) {
	for value, expected := range map[string]string{"Up 2 minutes (healthy)": "healthy", "Up 2 seconds (health: starting)": "starting", "Exited (1) 1 second ago": ""} {
		if got := healthFromStatus(value); got != expected {
			t.Fatalf("%q: got %q, want %q", value, got, expected)
		}
	}
}
