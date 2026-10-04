package agent

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

type retirementRuntime struct {
	fakeRuntime
	failCleanup bool
}

func (r *retirementRuntime) Remove(ctx context.Context, release *Release) error {
	if r.failCleanup {
		return errors.New("fixture cleanup failure")
	}
	return r.fakeRuntime.Remove(ctx, release)
}
func TestRetirementRevokesRoutingAndSurvivesRetry(t *testing.T) {
	rt := &retirementRuntime{}
	m := testManager(t, rt)
	for _, id := range []string{"one", "two"} {
		if err := m.Upsert(testApp(id)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		d, err := m.Enqueue("one", DeployRequest{})
		if err != nil {
			t.Fatal(err)
		}
		waitDeployment(t, m, d.ID)
	}
	rt.failCleanup = true
	if err := m.Retire(context.Background(), "one"); err == nil {
		t.Fatal("cleanup failure hidden")
	}
	if m.Target("one.example.com") != nil {
		t.Fatal("retiring app still routes")
	}
	req := httptest.NewRequest("GET", "/tls/allow?domain=one.example.com", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	Handler(m).ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("certificate still allowed", w.Code)
	}
	if _, err := m.Enqueue("one", DeployRequest{}); err == nil {
		t.Fatal("retiring app accepted deploy")
	}
	if err := m.Upsert(testApp("one")); err == nil {
		t.Fatal("retiring app overwritten")
	}
	if err := m.PatchSettings("one", SettingsPatch{Branch: "main", Domain: "another.example.com"}); err == nil {
		t.Fatal("retiring app edited")
	}
	restored, err := NewManager(context.Background(), m.cfg, &retirementRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	apps := restored.Apps()
	if !apps[0].Retiring && !apps[1].Retiring {
		t.Fatal("retirement not persisted")
	}
	if err := restored.Retire(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if err := restored.Retire(context.Background(), "one"); err != nil {
		t.Fatal("retry not idempotent", err)
	}
	if apps := restored.Apps(); len(apps) != 1 || apps[0].ID != "two" {
		t.Fatal("wrong app removed", apps)
	}
	if err := restored.Upsert(testApp("one")); err != nil {
		t.Fatal("ID unavailable after removal", err)
	}
}
