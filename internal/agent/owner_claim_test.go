package agent

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerClaimFiles(t *testing.T) {
	d := &Docker{Root: t.TempDir()}
	a := testApp("claim")
	a.Env["AUTH_OWNER_CLAIM"] = "true"
	dir := d.ownerClaimDirectory(a)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	token := strings.Repeat("s", 64)
	write("status.json", `{"state":"unclaimed"}`)
	write("token", token)
	out, err := d.OwnerClaim(context.Background(), a)
	if err != nil || out.Token != token {
		t.Fatal("token unavailable", err)
	}
	// Claim completion suppresses a stale token, including a platform-supplied token.
	a.Env["AUTH_OWNER_CLAIM_TOKEN"] = token
	write("status.json", `{"state":"claimed"}`)
	out, err = d.OwnerClaim(context.Background(), a)
	if err != nil || out.Token != "" || out.State != "claimed" {
		t.Fatal("claimed app exposed token")
	}
	delete(a.Env, "AUTH_OWNER_CLAIM_TOKEN")
	write("status.json", `{"state":"unclaimed"}`)
	if err := os.Remove(filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}
	if _, err = d.OwnerClaim(context.Background(), a); err == nil {
		t.Fatal("token followed a symlink outside private directory")
	}
	write("status.json", `{"state":"invented"}`)
	if _, err = d.OwnerClaim(context.Background(), a); err == nil {
		t.Fatal("invalid status accepted")
	}
	a.Env["AUTH_OWNER_CLAIM"] = "false"
	out, err = d.OwnerClaim(context.Background(), a)
	if err != nil || out.State != "disabled" || out.Token != "" {
		t.Fatal("disabled claim exposed token")
	}
}

type claimFixtureRuntime struct{ fakeRuntime }

func (*claimFixtureRuntime) OwnerClaim(context.Context, App) (OwnerClaim, error) {
	return OwnerClaim{State: "unclaimed", Token: strings.Repeat("z", 64)}, nil
}
func TestOwnerClaimEndpointAuthentication(t *testing.T) {
	m := testManager(t, &claimFixtureRuntime{})
	a := testApp("claimroute")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	d, err := m.Enqueue(a.ID, DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if waitDeployment(t, m, d.ID).Status != "live" {
		t.Fatal("deployment failed")
	}
	for _, authorized := range []bool{false, true} {
		r := httptest.NewRequest("POST", "/v1/apps/claimroute/owner-claim", strings.NewReader("{}"))
		if authorized {
			r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
		}
		w := httptest.NewRecorder()
		Handler(m).ServeHTTP(w, r)
		if authorized {
			if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), strings.Repeat("z", 64)) {
				t.Fatal("authorized token retrieval failed", w.Code)
			}
		} else if w.Code != 401 || strings.Contains(w.Body.String(), strings.Repeat("z", 64)) {
			t.Fatal("unauthenticated token disclosure", w.Code)
		}
	}
}
