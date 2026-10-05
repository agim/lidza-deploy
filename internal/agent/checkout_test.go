package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type credentialRuntime struct {
	fakeRuntime
	token string
}

func (f *credentialRuntime) Deploy(ctx context.Context, a App, id, token string) (*Release, error) {
	f.mu.Lock()
	f.token = token
	f.mu.Unlock()
	return f.fakeRuntime.Deploy(ctx, a, id, token)
}
func TestQueuedDeploymentObtainsCredentialOnlyWhenCheckoutStarts(t *testing.T) {
	gate := make(chan struct{})
	rt := &credentialRuntime{fakeRuntime: fakeRuntime{gate: gate}}
	m := testManager(t, rt)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+m.cfg.APIKey || r.Header.Get("X-Lidza-Server") != "hosting" {
			t.Error("agent identity missing")
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "fresh-after-queue"})
	}))
	defer server.Close()
	for _, id := range []string{"first", "second"} {
		if err := m.Upsert(testApp(id)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := m.Enqueue("first", DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Enqueue("second", DeployRequest{CredentialURL: server.URL + "/api/agent/checkout-token", CredentialTicket: strings.Repeat("t", 64), CredentialServer: "hosting"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("credentials acquired while still queued")
	}
	close(gate)
	waitDeployment(t, m, first.ID)
	if waitDeployment(t, m, second.ID).Status != "live" {
		t.Fatal("checkout failed")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.token != "fresh-after-queue" || calls != 1 {
		t.Fatal("just-in-time credential was not used")
	}
}
func TestCheckoutAuthorizationRejectsUnsafeOriginsAndMixedSecrets(t *testing.T) {
	for _, in := range []DeployRequest{{CredentialURL: "http://remote.example/api/agent/checkout-token", CredentialTicket: strings.Repeat("t", 64), CredentialServer: "host"}, {CredentialURL: "https://user:pass@control.example/api/agent/checkout-token", CredentialTicket: strings.Repeat("t", 64), CredentialServer: "host"}, {CredentialURL: "https://control.example/api/agent/checkout-token", CredentialTicket: strings.Repeat("t", 64), CredentialServer: "host", Token: "stale"}} {
		if validateCheckoutCredential(in) == nil {
			t.Fatal("unsafe checkout request accepted")
		}
	}
}
