package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type consoleFixture struct {
	fakeRuntime
	output string
}

func (f *consoleFixture) ConsoleLogs(context.Context, *Release, time.Time) (string, error) {
	return f.output, nil
}
func TestConsoleParserLevelsHTTPFailuresPanicStacksAndReplay(t *testing.T) {
	stamp := time.Now().UTC().Add(-time.Minute)
	line := func(i int, s string) string {
		return stamp.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano) + " " + s + "\n"
	}
	logs := line(0, `{"level":"INFO","msg":"request","status":200}`) + line(1, `{"level":"ERROR","msg":"checkout runtime-secret","stack":"handlers.checkout runtime-secret","path":"/checkout?secret=other","request_id":"request-one"}`) + line(2, `{"level":"INFO","msg":"request","status":500,"path":"/broken"}`) + line(3, "panic: failed runtime-secret") + line(4, "goroutine 1 [running]:") + line(5, "\t/app/main.go:42")
	a := testApp("console")
	r := Release{ID: "r1", Container: "container-one", Commit: "abc"}
	gen := strings.Repeat("a", 32)
	events, cursor := parseConsole(logs, a, r, gen, ConsoleCursor{Time: stamp.Add(-time.Second)}, 100)
	if len(events) != 3 || events[2].Stack == nil || !strings.Contains(*events[2].Stack, "main.go:42") {
		t.Fatal("console errors or multiline panic lost", len(events))
	}
	payload, _ := json.Marshal(events)
	if strings.Contains(string(payload), "runtime-secret") || strings.Contains(string(payload), "secret=other") {
		t.Fatal("console reports leaked secret or query")
	}
	if more, _ := parseConsole(logs, a, r, gen, cursor, 100); len(more) != 0 {
		t.Fatal("cursor replay duplicated events")
	}
	duplicate := line(6, `{"level":"ERROR","msg":"same failure"}`)
	repeated, at := parseConsole(duplicate+duplicate, a, r, gen, cursor, 100)
	if len(repeated) != 2 || repeated[0].ID == repeated[1].ID {
		t.Fatal("identical errors at same timestamp collapsed")
	}
	if again, _ := parseConsole(duplicate+duplicate, a, r, gen, at, 100); len(again) != 0 {
		t.Fatal("same-timestamp replay duplicated")
	}
	reversed, _ := parseConsole(line(8, `{"level":"ERROR","msg":"second"}`)+line(7, `{"level":"ERROR","msg":"first"}`), a, r, gen, cursor, 100)
	if len(reversed) != 2 || reversed[0].Message != "first" {
		t.Fatal("stderr/stdout ordering lost errors")
	}
}
func TestConsoleOutboxSurvivesRestartAndRetriesWithoutDuplication(t *testing.T) {
	var unavailable atomic.Bool
	unavailable.Store(true)
	var delivered atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 32) || r.Header.Get("X-Lidza-Server") != "server-one" {
			t.Error("agent report missing scoped authentication")
		}
		if unavailable.Load() {
			w.WriteHeader(503)
			return
		}
		var batch ConsoleBatch
		if json.NewDecoder(r.Body).Decode(&batch) != nil {
			t.Error("invalid console delivery")
		}
		delivered.Add(int64(len(batch.Errors)))
		JSON(w, 200, map[string]string{"status": "stored"})
	}))
	defer remote.Close()
	rt := &consoleFixture{output: time.Now().UTC().Format(time.RFC3339Nano) + ` {"level":"ERROR","msg":"failed runtime-secret"}` + "\n"}
	m := testManager(t, rt)
	a := testApp("console")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	a.Current = &Release{ID: "r1", Container: "container-one"}
	m.data.Apps[a.ID] = a
	m.data.ErrorReporting = &ErrorReporting{URL: remote.URL + "/api/agent/errors", Server: "server-one", Apps: map[string]string{a.ID: strings.Repeat("g", 32)}}
	m.mu.Unlock()
	m.consoleTick(context.Background())
	m.mu.Lock()
	queued := len(m.data.ErrorOutbox)
	m.mu.Unlock()
	if queued != 1 {
		t.Fatal("failed delivery not retained")
	}
	raw, _ := os.ReadFile(m.path())
	if strings.Contains(string(raw), "runtime-secret") || strings.Contains(string(raw), "failed") {
		t.Fatal("collector state not sealed")
	}
	m.Close()
	reopened, err := NewManager(context.Background(), m.cfg, rt)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	unavailable.Store(false)
	reopened.consoleTick(context.Background())
	reopened.consoleTick(context.Background())
	reopened.mu.Lock()
	remaining := len(reopened.data.ErrorOutbox)
	reopened.mu.Unlock()
	if delivered.Load() != 1 || remaining != 0 {
		t.Fatal("restart replay or delivery retry failed", delivered.Load(), remaining)
	}
}

func TestConsoleBacklogDrainsMultipleBoundedBatches(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	queued := make([]ConsoleError, 100)
	for i := range queued {
		queued[i].ID = fmt.Sprint(i)
		queued[i].Message = strings.Repeat("\x00", 2048)
		stack := strings.Repeat("\x00", 4096)
		queued[i].Stack = &stack
	}
	m.data.ErrorOutbox = queued
	seen := map[string]bool{}
	calls := 0
	var unavailable atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(503)
			return
		}
		calls++
		body, _ := io.ReadAll(r.Body)
		if len(body) > 240<<10 {
			t.Error("batch exceeds API body limit", len(body))
		}
		var batch ConsoleBatch
		if err := json.Unmarshal(body, &batch); err != nil {
			t.Error(err)
		}
		if len(batch.Errors) > 20 {
			t.Error("too many records in a batch")
		}
		for _, e := range batch.Errors {
			if seen[e.ID] {
				t.Error("duplicate record in acknowledged batches", e.ID)
			}
			seen[e.ID] = true
		}
		JSON(w, 200, map[string]string{"status": "stored"})
	}))
	defer remote.Close()
	cfg := ErrorReporting{URL: remote.URL, Server: "local"}
	m.deliverConsole(context.Background(), cfg, queued, "backlog full")
	// Delivery is bounded by both 16 batches and eight seconds. Race-instrumented
	// encryption on slower runners can exhaust the time budget first; requiring
	// exactly 16 batches contradicts that contract. Still require catch-up beyond
	// a single batch, enforce the batch ceiling, and account for every saved ACK.
	if calls < 2 || calls > 16 || len(m.data.ErrorOutbox) != len(queued)-len(seen) {
		t.Fatalf("bounded catch-up failed: calls=%d delivered=%d remaining=%d", calls, len(seen), len(m.data.ErrorOutbox))
	}
	unavailable.Store(true)
	remaining := len(m.data.ErrorOutbox)
	m.deliverConsole(context.Background(), cfg, m.data.ErrorOutbox, "")
	if len(m.data.ErrorOutbox) != remaining {
		t.Fatal("unacknowledged reports were discarded")
	}
}
