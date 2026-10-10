package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

type routeFake struct {
	fakeRuntime
	ports  map[string]string
	during func()
}

func (f *routeFake) PublishedPorts(context.Context, []string) (map[string]string, error) {
	if f.during != nil {
		f.during()
	}
	return f.ports, nil
}

func TestRouteReconciliationPreservesReleaseAndPersists(t *testing.T) {
	rt := &routeFake{ports: map[string]string{"current": "32769", "previous": "32770"}}
	m := testManager(t, rt)
	a := testApp("routes")
	old := &Release{ID: "one", Container: "current", Port: "39999", Commit: "commit"}
	a.Current = old
	a.Previous = &Release{ID: "zero", Container: "previous", Port: "39998"}
	m.mu.Lock()
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	m.reconcileRoutes()
	got := m.Target(a.Domain)
	if got.Port != "32769" || got.ID != "one" || got.Commit != "commit" || old.Port != "39999" {
		t.Fatalf("unsafe route refresh: %+v old=%+v", got, old)
	}
	restarted, err := NewManager(context.Background(), m.cfg, rt)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.Target(a.Domain).Port != "32769" {
		t.Fatal("refreshed route was not persisted")
	}
	if restarted.data.Apps[a.ID].Previous.Port != "32770" {
		t.Fatal("rollback route was not refreshed")
	}
	rt.ports["current"] = ""
	m.reconcileRoutes()
	recorder := httptest.NewRecorder()
	Proxy(m).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://"+a.Domain+"/", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatal("missing binding must not proxy to a stale port")
	}
	rt.ports["current"] = "32769"
	rt.during = func() {
		m.mu.Lock()
		a.Current = &Release{ID: "new", Container: "replacement", Port: "12345"}
		m.data.Apps[a.ID] = a
		m.mu.Unlock()
	}
	m.reconcileRoutes()
	if m.Target(a.Domain).Port != "12345" {
		t.Fatal("concurrent deployment route overwritten")
	}
}

func TestDockerRouteRecoveryAfterContainerRestart(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for real container restart")
	}
	ctx := context.Background()
	name := "lidza-route-test-" + newID()
	if _, err := command(ctx, "", nil, "docker", "run", "-d", "--name", name, "--publish", "127.0.0.1::3000", "caddy:2.10.2-alpine", "caddy", "respond", "--listen", ":3000", "--body", "route recovered"); err != nil {
		t.Fatal(err)
	}
	defer command(ctx, "", nil, "docker", "rm", "-f", name)
	if _, err := command(ctx, "", nil, "docker", "restart", name); err != nil {
		t.Fatal(err)
	}
	d := &Docker{}
	missing := "missing-route-test-" + newID()
	missingPorts, missingErr := d.PublishedPorts(ctx, []string{missing})
	if missingErr != nil || len(missingPorts) != 1 || missingPorts[missing] != "" {
		t.Fatalf("removed container route: %v %v", missingPorts, missingErr)
	}
	// A removed historical container must not prevent recovery of a live route.
	ports, err := d.PublishedPorts(ctx, []string{name, "missing-route-test-" + newID()})
	if err != nil {
		t.Fatal(err)
	}
	port := ports[name]
	if len(ports) != 2 {
		t.Fatal("missing container must be represented as unavailable")
	}
	if port == "" {
		t.Fatal("live port missing")
	}
	m := testManager(t, d)
	a := testApp("route-live")
	a.Current = &Release{Container: name, Port: "1"}
	m.mu.Lock()
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	m.reconcileRoutes()
	if m.Target(a.Domain).Port != port {
		t.Fatal("stale route not recovered")
	}
	request := httptest.NewRequest(http.MethodGet, "http://"+a.Domain+"/", nil)
	recorder := httptest.NewRecorder()
	Proxy(m).ServeHTTP(recorder, request)
	body, _ := io.ReadAll(recorder.Result().Body)
	if recorder.Code != 200 || string(body) != "route recovered" {
		t.Fatalf("proxy did not recover: %d %s", recorder.Code, body)
	}
}
