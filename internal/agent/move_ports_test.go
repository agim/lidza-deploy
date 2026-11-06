package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestMoveSourceReportsPublishedPortsOfTheCurrentRelease(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("mailer")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	manifest := fixtureStackManifest()
	manifest.Services[0].PublicPorts = []StackPort{{Port: 587, Protocol: "tcp"}, {Port: 25, Protocol: ""}, {Port: 25, Protocol: "tcp"}}
	id := "release-one"
	m.mu.Lock()
	stored := m.data.Apps[a.ID]
	stored.Current = &Release{ID: id, Commit: "0123456789abcdef0123456789abcdef01234567", Stack: &StackRelease{Network: "lidza-stack-mailer-" + id, Containers: []StackContainer{{Container: stackContainerName(a, id, "app"), Service: manifest.Services[0]}, {Container: stackContainerName(a, id, "mail"), Service: manifest.Services[1]}}}}
	m.data.Apps[a.ID] = stored
	m.mu.Unlock()
	w := moveRequest(t, Handler(m), "GET", "/v1/move/apps/mailer", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var source MoveSource
	if err := json.Unmarshal(w.Body.Bytes(), &source); err != nil {
		t.Fatal(err)
	}
	if want := []StackPort{{Port: 25, Protocol: "tcp"}, {Port: 587, Protocol: "tcp"}}; len(source.PublicPorts) != 2 || source.PublicPorts[0] != want[0] || source.PublicPorts[1] != want[1] {
		t.Fatalf("public ports: %+v", source.PublicPorts)
	}
	// An application without a stack reports an empty list, not a missing field.
	plain := testApp("plain")
	if err := m.Upsert(plain); err != nil {
		t.Fatal(err)
	}
	w = moveRequest(t, Handler(m), "GET", "/v1/move/apps/plain", nil, nil)
	if !strings.Contains(w.Body.String(), `"public_ports":[]`) {
		t.Fatalf("an application without published ports reports an empty list: %s", w.Body)
	}
}

func TestParseListeningPortsKeepsPublicAndWildcardBinds(t *testing.T) {
	tcp := strings.Join([]string{
		"LISTEN 0 4096 0.0.0.0:22 0.0.0.0:*",
		"LISTEN 0 4096 127.0.0.1:5432 0.0.0.0:*",
		"LISTEN 0 4096 [::1]:6379 [::]:*",
		"LISTEN 0 4096 *:443 *:*",
		"LISTEN 0 4096 [::]:22 [::]:*",
		`LISTEN 0 100 203.0.113.9:25 0.0.0.0:* users:(("master",pid=1234,fd=13))`,
		`LISTEN 0 100 [::]:25 [::]:* users:(("master",pid=1234,fd=14))`,
		`LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:(("nginx",pid=88,fd=6),("nginx",pid=87,fd=6))`,
		"LISTEN 0 4096 [fe80::1%eth0]:8080 [::]:*",
		"garbage",
	}, "\n")
	udp := "UNCONN 0 0 0.0.0.0:51820 0.0.0.0:*\nUNCONN 0 0 127.0.0.53%lo:53 0.0.0.0:*\n"
	got := parseListeningPorts(tcp, udp)
	want := []ListeningPort{{22, "tcp", ""}, {25, "tcp", "master"}, {80, "tcp", "nginx"}, {443, "tcp", ""}, {51820, "udp", ""}}
	if len(got) != len(want) {
		t.Fatalf("ports: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ports: %+v, want %+v", got, want)
		}
	}
}

func TestMoveHostReportsListeningPortsOrThatTheyAreUnknown(t *testing.T) {
	previous := hostCommand
	t.Cleanup(func() { hostCommand = previous })
	hostCommand = func(_ context.Context, name string, args ...string) (string, error) {
		switch {
		case name == "ss" && strings.Contains(strings.Join(args, " "), "-t"):
			return "LISTEN 0 4096 0.0.0.0:25 0.0.0.0:*\n", nil
		case name == "ss":
			return "", nil
		}
		return "", errors.New("not available")
	}
	ports := moveListeningPorts(context.Background())
	if len(ports) != 1 || ports[0] != (ListeningPort{Port: 25, Protocol: "tcp"}) {
		t.Fatalf("listening ports: %+v", ports)
	}
	hostCommand = func(context.Context, string, ...string) (string, error) { return "", errors.New("ss: not found") }
	if moveListeningPorts(context.Background()) != nil {
		t.Fatal("when the sockets cannot be listed the answer is unknown (nil), not empty")
	}
	m := testManager(t, &fakeRuntime{})
	w := moveRequest(t, Handler(m), "GET", "/v1/move/host", nil, nil)
	var host MoveHost
	if err := json.Unmarshal(w.Body.Bytes(), &host); err != nil || w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if host.ListeningPorts != nil || !strings.Contains(w.Body.String(), `"listening_ports":null`) {
		t.Fatalf("an unknown port list is null in the host description: %s", w.Body)
	}
}
