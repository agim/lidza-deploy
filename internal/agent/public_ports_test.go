package agent

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicPortRegistrationUsesRecordedResourceOwnership(t *testing.T) {
	a := testApp("port-app")
	id := "release-one"
	manifest := fixtureStackManifest()
	manifest.Services[0].PublicPorts = []StackPort{{Port: 25, Protocol: "tcp"}}
	r := &Release{ID: id, Stack: &StackRelease{Network: "lidza-stack-incarnation-one-" + id, Containers: []StackContainer{{Container: stackContainerName(a, id, "app"), Service: manifest.Services[0]}, {Container: stackContainerName(a, id, "mail"), Service: manifest.Services[1]}}}}
	got := releasePortRegistrations(r)
	if len(got) != 1 || got[0].AppID != a.ID || got[0].Owner != "incarnation-one" || len(got[0].Ports) != 1 {
		t.Fatalf("invalid namespace/ownership registration: %+v", got)
	}
}
func TestAppPortInventoryRequiresAuthAndFiltersOtherApps(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("port-app")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	saved := m.data.Apps[a.ID]
	saved.Current = &Release{ID: "live", Container: "recorded-container"}
	m.data.Apps[a.ID] = saved
	m.mu.Unlock()
	// Model helper response with unrelated and retained-release entries. The
	// private agent API must expose only this app's current release.
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"ports\":[{\"app_id\":\"port-app\",\"release\":\"live\",\"container\":\"recorded-container\",\"host_port\":25},{\"app_id\":\"other-app\",\"release\":\"live\",\"host_port\":7882},{\"app_id\":\"port-app\",\"release\":\"old\",\"host_port\":7881}]}'\n"
	if err := os.WriteFile(filepath.Join(bin, "python3"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	m.runtime = &Docker{Root: t.TempDir()}
	handler := Handler(m)
	req := httptest.NewRequest("GET", "/v1/apps/port-app/ports", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("unauthenticated port inventory exposed", w.Code)
	}
	req = httptest.NewRequest("GET", "/v1/apps/port-app/ports", nil)
	req.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var out PortInventory
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(out.Ports) != 1 || out.Ports[0].Container != "recorded-container" || strings.Contains(w.Body.String(), "other-app") {
		t.Fatal("inventory leaked unrelated release", w.Code, w.Body.String())
	}
	if _, err := m.appPorts(context.Background(), "missing"); err == nil {
		t.Fatal("unknown app accepted")
	}
}

func TestPrivateOnlyLifecycleDoesNotRequireAnUnrelatedFirewall(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "python3"), []byte("#!/bin/sh\necho unexpected-firewall-call >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	d := &Docker{Root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(d.Root, ".public-ports"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Root, ".public-ports", "registry.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.setReleaseRunning(context.Background(), nil, false); err != nil {
		t.Fatal("empty lifecycle reached unrelated firewall", err)
	}
	r := &Release{ID: "private", Container: "private-app", Stack: &StackRelease{Network: "lidza-stack-private-private", Containers: []StackContainer{{Container: "private-app", Service: StackService{Name: "app", Role: "app"}}}}}
	if err := d.setReleaseRunning(context.Background(), r, true); err != nil {
		t.Fatal("private-only lifecycle reached unrelated firewall", err)
	}
}
