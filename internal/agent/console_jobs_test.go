package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConsoleJobLiveOutputIdempotencyAndOwnerScope(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	input := ConsoleJobRequest{ID: "console-test-00001", Owner: "operator", Command: "printf first; sleep 0.3; printf second; exit 7"}
	j, err := m.queueConsoleJob(input, executeConsoleCommand)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for j.view(0).Output == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	first := j.view(0)
	if first.State != "running" || first.Output != "first" {
		t.Fatalf("no live output: %#v", first)
	}
	retry, err := m.queueConsoleJob(input, executeConsoleCommand)
	if err != nil || retry != j {
		t.Fatal("lost acknowledgement repeated command", err)
	}
	other := input
	other.Owner = "another-operator"
	if _, err := m.queueConsoleJob(other, executeConsoleCommand); err == nil {
		t.Fatal("owner collision accepted")
	}
	other = input
	other.Command = "printf altered"
	if _, err := m.queueConsoleJob(other, executeConsoleCommand); err == nil {
		t.Fatal("same ID accepted different command")
	}
	other = input
	other.ID = "console-test-00002"
	if _, err := m.queueConsoleJob(other, executeConsoleCommand); err == nil {
		t.Fatal("concurrent root command accepted")
	}
	for j.view(0).State == "running" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	last := j.view(first.NextOffset)
	if last.State != "finished" || last.ExitCode != 7 || last.Output != "second" {
		t.Fatalf("completion lost: %#v", last)
	}
	for _, tc := range []struct {
		owner  string
		status int
	}{{"operator", 200}, {"someone-else", 404}} {
		r := httptest.NewRequest("GET", "/v1/console/jobs/"+input.ID+"?owner="+tc.owner+"&offset=0", nil)
		r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
		w := httptest.NewRecorder()
		Handler(m).ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatal("owner access", w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "/v1/console/jobs/"+input.ID+"?owner=operator&offset=0", nil)
	w := httptest.NewRecorder()
	Handler(m).ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("unauthenticated console output exposed", w.Code)
	}
}

func TestConsoleJobOutputChunksAndBound(t *testing.T) {
	partial := &consoleJob{state: "running", started: time.Now()}
	_, _ = partial.Write([]byte{0xc3})
	if partial.view(0).Output != "" {
		t.Fatal("incomplete UTF-8 was emitted")
	}
	_, _ = partial.Write([]byte{0xbc})
	if partial.view(0).Output != "ü" {
		t.Fatal("split UTF-8 output was corrupted")
	}
	m := testManager(t, &fakeRuntime{})
	j, err := m.queueConsoleJob(ConsoleJobRequest{ID: "console-chunks-0001", Owner: "operator", Command: "fixture"}, func(ctx context.Context, command string, w io.Writer) (ConsoleResult, error) {
		_, err := io.WriteString(w, strings.Repeat("ü", ConsoleMaxOutput))
		return ConsoleResult{ExitCode: 0}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for j.view(0).State == "running" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	offset := 0
	var text strings.Builder
	for {
		view := j.view(offset)
		encoded, _ := json.Marshal(view)
		if len(view.Output) > 64<<10 || !view.Truncated || view.OutputSize != ConsoleMaxOutput || !json.Valid(encoded) {
			t.Fatalf("unbounded output: %#v", view)
		}
		text.WriteString(view.Output)
		offset = view.NextOffset
		if offset >= view.OutputSize {
			break
		}
	}
	if text.String() != strings.Repeat("ü", ConsoleMaxOutput/2) {
		t.Fatal("incremental output corrupted or duplicated")
	}
}

func TestConsoleJobRejectsInvalidAndUnprivilegedRequests(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	execute := func(context.Context, string, io.Writer) (ConsoleResult, error) {
		t.Error("invalid command executed")
		return ConsoleResult{}, nil
	}
	for _, input := range []ConsoleJobRequest{
		{ID: "bad", Owner: "operator", Command: "printf fixture"},
		{ID: "console-invalid-01", Command: "printf fixture"},
		{ID: "console-invalid-01", Owner: "operator", Command: " "},
		{ID: "console-invalid-01", Owner: "operator", Command: "printf\x00fixture"},
		{ID: "console-invalid-01", Owner: "operator", Command: strings.Repeat("x", consoleMaxInput+1)},
	} {
		if _, err := m.queueConsoleJob(input, execute); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	if os.Geteuid() != 0 {
		if _, err := m.startConsoleJob(ConsoleJobRequest{ID: "console-notroot-01", Owner: "operator", Command: "printf fixture"}); err == nil {
			t.Fatal("non-root console accepted")
		}
	}
}

func TestConsoleJobStopsOnAgentShutdown(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	j, err := m.queueConsoleJob(ConsoleJobRequest{ID: "console-cancel-0001", Owner: "operator", Command: "printf started; sleep 30"}, executeConsoleCommand)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for j.view(0).Output == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	m.Close()
	view := j.view(0)
	if view.State != "interrupted" || view.ExitCode != 124 || view.Error == "" || view.Output != "started" {
		t.Fatalf("shutdown lost status: %#v", view)
	}
}

func TestConsoleMissingResultIsExplicit(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	r := httptest.NewRequest("GET", "/v1/console/jobs/not-found?owner=operator&offset=0", nil)
	r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	w := httptest.NewRecorder()
	Handler(m).ServeHTTP(w, r)
	var result map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 404 || !strings.Contains(result["error"], "Check command outcome") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestConsoleHistoryEvictsOutputWithoutReexecutingCommands(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	var calls atomic.Int32
	execute := func(ctx context.Context, command string, w io.Writer) (ConsoleResult, error) {
		calls.Add(1)
		_, err := io.WriteString(w, command)
		return ConsoleResult{}, err
	}
	var first *consoleJob
	var firstInput ConsoleJobRequest
	for i := 0; i < 10; i++ {
		input := ConsoleJobRequest{ID: "console-history-" + strings.Repeat("x", i+1), Owner: "operator", Command: "fixture"}
		j, err := m.queueConsoleJob(input, execute)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = j
			firstInput = input
		}
		deadline := time.Now().Add(5 * time.Second)
		for j.view(0).State == "running" && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	if view := first.view(0); view.Output != "" || view.Error == "" {
		t.Fatal("old output was not evicted", view)
	}
	j, err := m.queueConsoleJob(firstInput, execute)
	if err != nil || j != first || calls.Load() != 10 {
		t.Fatal("history eviction allowed duplicate execution", err, calls.Load())
	}
	first.mu.Lock()
	first.finished = time.Now().Add(-31 * time.Minute)
	first.mu.Unlock()
	r := httptest.NewRequest("GET", "/v1/console/jobs/"+firstInput.ID+"?owner=operator&offset=0", nil)
	r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	w := httptest.NewRecorder()
	Handler(m).ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("expired output available", w.Code)
	}
}

func TestConsoleCancelPreservesOutputAndRequiresOwner(t *testing.T) {
	for _, command := range []string{"printf started; sleep 30", "trap '' INT; printf started; sleep 30"} {
		t.Run(command, func(t *testing.T) {
			m := testManager(t, &fakeRuntime{})
			j, err := m.queueConsoleJob(ConsoleJobRequest{ID: "console-cancel-owner-01", Owner: "operator", Command: command}, executeConsoleCommand)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for j.view(0).Output == "" && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			request := func(owner string, authenticated bool) int {
				r := httptest.NewRequest("POST", "/v1/console/jobs/"+j.id+"/cancel", strings.NewReader(`{"owner":"`+owner+`"}`))
				if authenticated {
					r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
				}
				w := httptest.NewRecorder()
				Handler(m).ServeHTTP(w, r)
				return w.Code
			}
			if status := request("operator", false); status != 401 {
				t.Fatal("unauthenticated cancellation accepted", status)
			}
			if status := request("someone-else", true); status != 404 {
				t.Fatal("another operator cancelled command", status)
			}
			if j.view(0).State != "running" {
				t.Fatal("unauthorized request stopped command")
			}
			started := time.Now()
			if status := request("operator", true); status != 202 {
				t.Fatal(status)
			}
			if status := request("operator", true); status != 202 {
				t.Fatal("repeat cancel failed", status)
			}
			deadline = time.Now().Add(7 * time.Second)
			for j.view(0).State == "running" && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			view := j.view(0)
			if view.State != "cancelled" || view.ExitCode != 130 || view.Output != "started" || view.Error != "" {
				t.Fatalf("cancel result lost: %#v", view)
			}
			if time.Since(started) > 6*time.Second {
				t.Fatal("ignored interrupt was not force-stopped")
			}
			if status := request("operator", true); status != 202 {
				t.Fatal("cancel after completion failed", status)
			}
			// Cancelling releases the host slot for the next command.
			if _, err := m.queueConsoleJob(ConsoleJobRequest{ID: "console-after-cancel-01", Owner: "operator", Command: "printf next"}, executeConsoleCommand); err != nil {
				t.Fatal(err)
			}
		})
	}
}
