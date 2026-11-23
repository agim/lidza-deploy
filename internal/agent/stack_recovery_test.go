package agent

import (
	"context"
	"strings"
	"testing"
)

// An interrupted release switch whose recovery failed at start is recorded
// and shown, but it is not work in progress: the application can be
// deployed again and the agent can update and clean up.
func TestAnUnsettledReleaseSwitchDoesNotCountAsBusy(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.mu.Lock()
	m.data.Apps["shop"] = App{ID: "shop"}
	m.data.StackTransitions = map[string]StackTransition{"shop": {Candidate: &Release{ID: "r2"}}}
	busy, cleanupBusy := m.busy("shop"), m.cleanupBusyLocked()
	m.mu.Unlock()
	if !busy || !cleanupBusy {
		t.Fatal("a release switch in progress must count as busy")
	}
	m.mu.Lock()
	m.stackRecoveryFailed = map[string]string{"shop": "the previous release did not become ready"}
	busy, cleanupBusy = m.busy("shop"), m.cleanupBusyLocked()
	m.mu.Unlock()
	if busy || cleanupBusy {
		t.Fatal("a switch whose recovery failed still blocks the application and the server")
	}
	found := false
	for _, check := range m.hostReadiness(context.Background()).Checks {
		if check.ID == "release-switch-shop" {
			found = !check.OK && strings.Contains(check.Detail, "did not become ready") && strings.Contains(check.Detail, "deploy it again")
		}
	}
	if !found {
		t.Fatal("the unsettled switch is not in the server's readiness")
	}
	// With another runtime than Docker there is nothing to recover, and a
	// switch that names no release is dropped instead of stopping the start.
	if err := m.recoverStackTransitions(); err != nil {
		t.Fatal(err)
	}
}
