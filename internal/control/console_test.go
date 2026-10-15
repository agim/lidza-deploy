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

func TestConsoleJobsUseShortRequestsAndProtectedReads(t *testing.T) {
	for _, pattern := range []string{"POST /api/control/servers/{server}/console/jobs", "GET /api/control/servers/{server}/console/jobs/{job}"} {
		if routePermission(pattern) != "infrastructure.manage" {
			t.Fatal("console job exposed to viewers", pattern)
		}
	}
	c := &Control{client: &http.Client{Timeout: 25 * time.Second, Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 30*time.Second {
			t.Fatal("job request waits for command completion")
		}
		return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(`{"id":"console-request-01","state":"running"}`)), Header: make(http.Header)}, nil
	})}}
	var job agent.ConsoleJobView
	if err := c.agentRequest(httptest.NewRequest("POST", "/", nil), Server{URL: "http://agent.example"}, "POST", "/v1/console/jobs", agent.ConsoleJobRequest{ID: "console-request-01", Owner: "operator", Command: "fixture"}, &job); err != nil || job.State != "running" {
		t.Fatal(job, err)
	}
	for _, status := range []int{404, 409, 500} {
		w := httptest.NewRecorder()
		consoleJobError(w, &agentHTTPError{status: status, message: "fixture"})
		expected := status
		if status == 500 {
			expected = 502
		}
		if w.Code != expected {
			t.Fatal("lost actionable job status", status, w.Code)
		}
	}
}
