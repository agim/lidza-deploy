package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type secretLogRuntime struct{ fakeRuntime }

func (*secretLogRuntime) Logs(context.Context, *Release) (string, error) {
	return "started successfully\nAPP_SECRET=runtime-secret\npostgres://app:database-password@db/app\npassword=database-password", nil
}

func TestApplicationLogResponseRedactsConfiguredSecrets(t *testing.T) {
	m := testManager(t, &secretLogRuntime{})
	a := testApp("logs")
	a.Env["DATABASE_URL"] = "postgres://app:database-password@db/app"
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	d, err := m.Enqueue(a.ID, DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if waitDeployment(t, m, d.ID).Status != "live" {
		t.Fatal("fixture deployment failed")
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/apps/logs/logs", nil)
	r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	w := httptest.NewRecorder()
	Handler(m).ServeHTTP(w, r)
	var response struct {
		Logs string `json:"logs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || strings.Contains(response.Logs, "runtime-secret") || strings.Contains(response.Logs, "database-password") || !strings.Contains(response.Logs, "started successfully") || strings.Count(response.Logs, "[redacted]") != 3 {
		t.Fatal("logs exposed configured secrets or lost diagnostics", w.Code)
	}
}

func TestLimitedBufferStringWritesReachTheDiagnosticStream(t *testing.T) {
	var seen string
	b := &limitedBuffer{diagnostic: func(s string) { seen = s }}
	if _, err := io.WriteString(b, "line one\n"); err != nil || seen != "line one\n" {
		t.Fatalf("io.WriteString bypassed the stream: %q %v", seen, err)
	}
}
