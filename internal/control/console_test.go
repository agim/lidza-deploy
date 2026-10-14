package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestConsoleRequestDeadlineAndLargeEscapedOutput(t *testing.T) {
	// Exceeds the old 4 MiB response limit after JSON escaping.
	output := strings.Repeat("\x00", 1<<20)
	client := &http.Client{Timeout: 25 * time.Second}
	client.Transport = updateTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) < agent.ConsoleTimeout {
			t.Errorf("console inherited the ordinary request timeout: %v", deadline)
		}
		body, _ := json.Marshal(agent.ConsoleResult{Output: output})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})
	c := &Control{client: client}
	var result agent.ConsoleResult
	err := c.agentRequest(httptest.NewRequest("POST", "/console", nil), Server{URL: "http://agent.example"}, "POST", "/v1/console", agent.ConsoleRequest{Command: "diagnostics"}, &result)
	if err != nil || result.Output != output {
		t.Fatalf("console response was truncated: %v (got %d bytes)", err, len(result.Output))
	}
	if client.Timeout != 25*time.Second {
		t.Fatal("console changed the shared agent client timeout")
	}
}

func TestConsoleTransportTimeoutIsNotRetried(t *testing.T) {
	calls := 0
	c := &Control{client: &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, context.DeadlineExceeded
	})}}
	err := c.agentRequest(httptest.NewRequest("POST", "/console", nil), Server{URL: "http://agent.example"}, "POST", "/v1/console", agent.ConsoleRequest{Command: "diagnostics"}, nil)
	if err == nil || !strings.Contains(err.Error(), "outcome is unknown") || calls != 1 {
		t.Fatalf("unsafe or misleading timeout handling: calls=%d err=%v", calls, err)
	}
}
