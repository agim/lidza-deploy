package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/platform/state"
	"github.com/agim/lidza/pkg/credentials"
)

func handoffFixture(t *testing.T, target string) (string, HandoffRequest) {
	t.Helper()
	t.Setenv(credentials.EnvMasterKey, "")
	t.Setenv(credentials.EnvKeyOff, "")
	dir := t.TempDir()
	if _, err := credentials.Generate(dir); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Set(dir, map[string]string{"PUBLIC_URL": "https://deploy.example.com", "AUTH_SECRET": "original-auth-secret", "DATABASE_URL": "postgres://user:private-password@old/database"}); err != nil {
		t.Fatal(err)
	}
	key, err := credentials.Key(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{"servers": []map[string]any{{"id": "hosting", "name": "Hosting", "url": "http://127.0.0.1:9090", "token": strings.Repeat("t", 32), "future_server_field": "preserved"}}, "apps": map[string]any{"app": map[string]any{"id": "app", "server_id": "hosting", "repository": "acme/app", "domain": "app.example.com", "secret": "webhook-secret", "future_private_app": map[string]string{"value": "preserved"}}}, "github_app": map[string]string{"private_key": "github-private-key"}, "unknown_private_field": []string{"opaque-value"}}
	plain, _ := json.Marshal(raw)
	sealed, err := credentials.Encrypt(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Save(filepath.Join(dir, "control.enc.json"), sealed); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(filepath.Join(dir, "setup-complete.json"), true); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(filepath.Join(dir, "servers.json"), raw["servers"]); err != nil {
		t.Fatal(err)
	}
	return dir, HandoffRequest{Origin: "https://deploy.example.com", Endpoints: []HandoffEndpoint{{ID: "hosting", From: "http://127.0.0.1:9090", To: target}}}
}

func TestHandoffStagesOpaqueStateWithoutChangingSourceOrKeys(t *testing.T) {
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/v1/apps" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
			t.Error("unexpected agent request")
		}
		json.NewEncoder(w).Encode([]agent.App{{ID: "app", Repository: "acme/app", Domain: "app.example.com"}})
	}))
	defer remote.Close()
	dir, request := handoffFixture(t, remote.URL)
	before, digest, err := readHandoffFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	review, err := ReviewControlHandoff(dir, request)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || review.SourceSHA256 != digest || review.ReadyToStart || review.DatabaseIncluded {
		t.Fatal("review performed work or claimed migration completion")
	}
	encoded, _ := json.Marshal(review)
	for _, secret := range []string{strings.Repeat("t", 32), "original-auth-secret", "private-password", "webhook-secret", "github-private-key", "opaque-value"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("review leaked secret", secret)
		}
	}
	out := filepath.Join(t.TempDir(), "staged")
	review, err = StageControlHandoff(context.Background(), dir, out, digest, request)
	if err != nil {
		t.Fatal(err)
	}
	if !review.EndpointsVerified || calls.Load() != 1 || review.ReadyToStart {
		t.Fatal("endpoint check or staged status incorrect")
	}
	after, afterDigest, err := readHandoffFiles(dir)
	if err != nil || afterDigest != digest || len(before) != len(after) {
		t.Fatal("source changed")
	}
	for _, name := range []string{credentials.MasterKeyFile, credentials.File, "setup-complete.json"} {
		b, e := os.ReadFile(filepath.Join(out, name))
		if e != nil || string(b) != string(before[name]) {
			t.Fatal("retained key/credentials/setup changed", name, e)
		}
	}
	key, _ := credentials.Key(out)
	var sealed string
	data, _ := os.ReadFile(filepath.Join(out, "control.enc.json"))
	if json.Unmarshal(data, &sealed) != nil {
		t.Fatal("invalid sealed state")
	}
	plain, err := credentials.Decrypt(key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"future_private_app", "unknown_private_field", "github-private-key", "webhook-secret", "future_server_field", remote.URL} {
		if !strings.Contains(string(plain), value) {
			t.Fatal("opaque state lost", value)
		}
	}
	info, _ := os.Stat(out)
	if info.Mode().Perm() != 0700 {
		t.Fatal("staged secrets directory permissions")
	}
	info, _ = os.Stat(filepath.Join(out, credentials.MasterKeyFile))
	if info.Mode().Perm() != 0600 {
		t.Fatal("staged key permissions")
	}
}

func TestHandoffRejectsUnreviewedStateAndWrongAgent(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]agent.App{{ID: "app", Repository: "wrong/app", Domain: "app.example.com"}})
	}))
	defer remote.Close()
	dir, request := handoffFixture(t, remote.URL)
	review, err := ReviewControlHandoff(dir, request)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "stage")
	if _, err := StageControlHandoff(context.Background(), dir, out, review.SourceSHA256, request); err == nil {
		t.Fatal("unrelated destination agent accepted")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("failed verification wrote package")
	}
	if err := os.WriteFile(filepath.Join(dir, "new-state"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := StageControlHandoff(context.Background(), dir, out, review.SourceSHA256, request); err == nil {
		t.Fatal("stale review accepted")
	}
	request.Endpoints[0].From = "http://127.0.0.1:9091"
	if _, err := ReviewControlHandoff(dir, request); err == nil {
		t.Fatal("wrong original endpoint accepted")
	}
	request.Endpoints = nil
	if _, err := ReviewControlHandoff(dir, request); err == nil {
		t.Fatal("implicit loopback remapping accepted")
	}
}

func TestHandoffRejectsSymlinksOriginChangesAndInPlaceOutput(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]agent.App{{ID: "app", Repository: "acme/app", Domain: "app.example.com"}})
	}))
	defer remote.Close()
	dir, request := handoffFixture(t, remote.URL)
	request.Origin = "https://new.example.com"
	if _, err := ReviewControlHandoff(dir, request); err == nil {
		t.Fatal("canonical origin changed")
	}
	request.Origin = "https://deploy.example.com"
	review, err := ReviewControlHandoff(dir, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StageControlHandoff(context.Background(), dir, filepath.Join(dir, "package"), review.SourceSHA256, request); err == nil {
		t.Fatal("staging wrote into source")
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(dir, "redirect")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReviewControlHandoff(dir, request); err == nil {
		t.Fatal("snapshot followed symlink")
	}
}
