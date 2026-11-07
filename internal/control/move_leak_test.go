package control

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// moveIsolate fails t when goroutines of the control panel or the tunnel
// package outlive it: every runner, tunnel, leg and heartbeat a test starts
// ends in its cleanups. Called first, its check runs after every other
// cleanup of t (the fake agents closed too).
func moveIsolate(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			left := moveLeftovers()
			if len(left) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("%d goroutines outlive the test:\n\n%s", len(left), strings.Join(left, "\n\n"))
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	})
}

// moveStopWork stops the panel's move runners and tunnels when t ends and
// fails t when they do not stop.
func moveStopWork(t *testing.T, c *Control) {
	t.Helper()
	t.Cleanup(func() {
		stopped := make(chan struct{})
		go func() { c.stopMoveWork(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Errorf("the move runners and tunnels did not stop:\n\n%s", strings.Join(moveLeftovers(), "\n\n"))
		}
	})
}

// moveLeftovers lists the running goroutines of the control panel and the
// tunnel package, besides the tests themselves.
func moveLeftovers() []string {
	buf := make([]byte, 8<<20)
	buf = buf[:runtime.Stack(buf, true)]
	out := []string{}
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "testing.tRunner(") || strings.Contains(g, "control.moveLeftovers(") || strings.Contains(g, "control.moveStopWork.func") {
			continue
		}
		if strings.Contains(g, "lidza-deploy/internal/control.") || strings.Contains(g, "lidza-deploy/internal/movetunnel.") {
			out = append(out, g)
		}
	}
	return out
}
