package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCappedConsoleBuffer(t *testing.T) {
	var buffer cappedConsoleBuffer
	input := strings.Repeat("x", ConsoleMaxOutput+128)
	n, err := buffer.Write([]byte(input))
	if err != nil || n != len(input) || buffer.Len() != ConsoleMaxOutput || !buffer.truncated {
		t.Fatalf("bounded output: n=%d len=%d truncated=%v err=%v", n, buffer.Len(), buffer.truncated, err)
	}
}

func TestConsoleDeadlinePreservesOutput(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("agent console requires root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := runConsole(ctx, "printf 'partial output'; sleep 30")
	if err != nil || result.ExitCode != 124 || !strings.Contains(result.Output, "partial output") || !strings.Contains(result.Output, "terminated") {
		t.Fatalf("deadline lost command output: %#v %v", result, err)
	}
	if result.Duration > 5 {
		t.Fatalf("command process group was not stopped: %#v", result)
	}
}

func TestRunConsoleAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("agent console requires the production root service account")
	}
	result, err := runConsole(context.Background(), "printf 'console-ready'")
	if err != nil || result.ExitCode != 0 || result.Output != "console-ready" {
		t.Fatalf("root command: %#v %v", result, err)
	}
	result, err = runConsole(context.Background(), "printf 'failed'; exit 7")
	if err != nil || result.ExitCode != 7 || result.Output != "failed" {
		t.Fatalf("exit result: %#v %v", result, err)
	}
}

func TestConsoleOverridesShortHTTPWriteDeadline(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("agent console requires root")
	}
	m := testManager(t, &fakeRuntime{})
	server := httptest.NewUnstartedServer(Handler(m))
	server.Config.WriteTimeout = 50 * time.Millisecond
	server.Start()
	defer server.Close()
	req, err := http.NewRequest("POST", server.URL+"/v1/console", strings.NewReader(`{"command":"sleep 0.2; printf 'finished'"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal("console lost its response at the server write deadline:", err)
	}
	defer response.Body.Close()
	var result ConsoleResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || response.StatusCode != 200 || result.Output != "finished" {
		t.Fatalf("console response: status=%d result=%#v err=%v", response.StatusCode, result, err)
	}
}
