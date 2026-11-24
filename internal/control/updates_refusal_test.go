package control

import (
	"errors"
	"strings"
	"testing"
)

// A server that refuses a release says why, and the panel passes that on
// instead of a bare "busy".
func TestAnUpdateRefusalCarriesTheServersReason(t *testing.T) {
	got := updateRefusal(&agentHTTPError{status: 409, message: "shop is switching releases; the update waits until that finishes"})
	if !strings.Contains(got, "shop is switching releases") || !strings.Contains(got, "Retrying every minute") {
		t.Fatalf("refusal: %q", got)
	}
	if got := updateRefusal(errors.New("dial tcp: connection refused")); got != "The server could not be reached; retrying every minute." {
		t.Fatalf("unreachable server: %q", got)
	}
	if got := updateRefusal(&agentHTTPError{status: 409, message: strings.Repeat("x", 600)}); len(got) > 330 {
		t.Fatalf("an unbounded reason: %d characters", len(got))
	}
}
