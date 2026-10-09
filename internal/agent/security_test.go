package agent

import (
	"strings"
	"testing"
	"time"
)

func TestSecurityClassification(t *testing.T) {
	for _, tc := range []struct{ line, category, outcome string }{
		{`{"msg":"request","path":"/api/.env?token=secret","method":"GET","status":404}`, "secret-file", "rejected"},
		{`{"msg":"request","path":"/api/%252e%252e/proc/self/environ","method":"GET","status":307}`, "path-traversal", "redirected"},
		{`{"msg":"request","path":"/api/fs/exec","method":"POST","status":200}`, "execution-probe", "review-response"},
	} {
		e, p, ok := securityRecord(tc.line)
		if !ok || p.Category != tc.category || p.Outcome != tc.outcome || strings.Contains(e.Route, "?") {
			t.Fatal("misclassified or query leaked", e, p)
		}
	}
	for _, line := range []string{`{"msg":"request","path":"/api/products","method":"GET","status":404}`, `{"msg":"request","path":"/api/.env","method":"GET","status":500}`, `{"msg":"request","path":"/api/.env","method":"GET","status":0}`} {
		if _, _, ok := securityRecord(line); ok {
			t.Fatal("ordinary traffic or app failure misclassified")
		}
	}
	app := testApp("security")
	app.Env["SECRET"] = "my-secret"
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	text := stamp + ` {"msg":"request","path":"/api/.env/my-secret?password=leak","method":"GET","status":404}` + "\n"
	events, cursor := parseConsole(text, app, Release{Container: "container"}, "generation", ConsoleCursor{}, 100)
	if len(events) != 1 || events[0].Security == nil || strings.Contains(*events[0].Route, "my-secret") || strings.Contains(*events[0].Route, "leak") {
		t.Fatal("probe secrecy/collection failed")
	}
	if replay, _ := parseConsole(text, app, Release{Container: "container"}, "generation", cursor, 100); len(replay) != 0 {
		t.Fatal("probe replay not deduplicated")
	}
	// Sample noisy probes so they cannot consume an entire collection batch.
	flood := strings.Repeat(text, 100) + stamp + ` {"level":"ERROR","msg":"real failure"}` + "\n"
	events, _ = parseConsole(flood, app, Release{Container: "container"}, "generation", ConsoleCursor{}, 100)
	if len(events) != 26 || events[25].Security != nil || events[25].Message != "real failure" {
		t.Fatal("probe sampling starved app error", len(events))
	}
}
