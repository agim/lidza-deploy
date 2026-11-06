package release

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSystemd puts systemctl and journalctl scripts on a private PATH:
// units are running except app-disabled, show reports a crash loop for the
// report properties, and the journal names a secret. Every call is recorded.
func fakeSystemd(t *testing.T) (env []string, calls func() string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "calls")
	systemctl := "#!/bin/sh\nprintf 'systemctl %s\\n' \"$*\" >> \"$RECORD\"\ncase \"$*\" in\n" +
		"*'show app-disabled.service --property=ActiveState,SubState,UnitFileState'*) printf 'ActiveState=inactive\\nSubState=dead\\nUnitFileState=disabled\\n';;\n" +
		"*'--property=ActiveState,SubState,UnitFileState'*) printf 'ActiveState=active\\nSubState=running\\nUnitFileState=enabled\\n';;\n" +
		"*show*) printf 'ActiveState=activating\\nSubState=auto-restart\\nResult=exit-code\\nNRestarts=3\\nExecMainStatus=1\\nExecMainCode=exited\\n';;\n" +
		"esac\n"
	journalctl := "#!/bin/sh\nprintf 'journalctl %s\\n' \"$*\" >> \"$RECORD\"\n" +
		"printf 'AUTH_SECRET is not set (got hunter2-secret)\\nrefusing to start\\n'\n"
	for name, body := range map[string]string{"systemctl": systemctl, "journalctl": journalctl} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Programs resolve through the test process's PATH, not the command's.
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	env = []string{"PATH=" + os.Getenv("PATH"), "RECORD=" + record, "XDG_RUNTIME_DIR=/run/user/1000"}
	return env, func() string { raw, _ := os.ReadFile(record); return string(raw) }
}

func TestFailedHealthCheckExplainsUnitsAndLastResponseMasked(t *testing.T) {
	url, commit := gitFixture(t)
	commit("VERSION", "one\n")
	p, _, _, log := testPlan(t, url)
	env, calls := fakeSystemd(t)
	p.Services = UserSystemd{Exec: LocalExecutor{}, Env: env}
	p.Mask = func(text string) string { return strings.ReplaceAll(text, "hunter2-secret", "[redacted]") }
	if _, err := Deploy(context.Background(), p); err != nil {
		t.Fatal(err, log.String())
	}
	commit("VERSION", "broken\n")
	// Both the new release and the republished previous one fail, each with
	// a refused HTTP answer whose body is longer than the log shows.
	body := "upstream unavailable (hunter2-secret) " + strings.Repeat("x", 400)
	p.Health = func(context.Context) error {
		return fmt.Errorf("%w from app.example.com/up", &HealthResponse{Status: 503, Body: body})
	}
	log.Reset()
	_, err := Deploy(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") || !strings.Contains(err.Error(), "also failed") {
		t.Fatal("expected both checks to fail", err)
	}
	text := log.String()
	for _, want := range []string{
		"==> last health response: HTTP 503\nupstream unavailable ([redacted]) ",
		"==> app-server.service since 22:",
		"==> app-worker.service since 22:",
		": ActiveState=activating SubState=auto-restart Result=exit-code NRestarts=3 ExecMainStatus=1 ExecMainCode=exited\n",
		"AUTH_SECRET is not set (got [redacted])\nrefusing to start\n",
		"==> new release failed (release: health check failed: HTTP 503 from app.example.com/up); republishing",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	if n := strings.Count(text, "==> last health response"); n != 2 {
		t.Fatalf("expected the explanation for the release and for the rollback, got %d:\n%s", n, text)
	}
	if strings.Contains(text, "hunter2-secret") || strings.Contains(text, strings.Repeat("x", 301)) || !strings.Contains(text, "x…\n") {
		t.Fatalf("secret or unbounded body in log:\n%s", text)
	}
	if strings.Contains(text, "==> app-disabled.service since") {
		t.Fatalf("a unit the restart skipped was explained:\n%s", text)
	}
	recorded := calls()
	if !strings.Contains(recorded, "journalctl --user -u app-server.service --since @") || !strings.Contains(recorded, " -n 40 -o cat --no-pager") {
		t.Fatalf("journal read with unexpected arguments:\n%s", recorded)
	}
	if !strings.Contains(recorded, "show app-server.service --property="+UnitReportProperties) {
		t.Fatalf("unit state read with unexpected arguments:\n%s", recorded)
	}
}

type fakeInspector struct{ units []string }

func (f *fakeInspector) Inspect(_ context.Context, unit string, _ time.Time) (UnitReport, error) {
	f.units = append(f.units, unit)
	if unit == "app-worker.service" {
		return UnitReport{State: "ActiveState=failed\nResult=exit-code"}, nil
	}
	return UnitReport{}, errors.New("journal unavailable")
}

func TestFailedRestartExplainsTheUnitsItReached(t *testing.T) {
	url, commit := gitFixture(t)
	commit("VERSION", "one\n")
	p, services, _, log := testPlan(t, url)
	services.fail = map[string]bool{"app-worker.service": true}
	inspector := &fakeInspector{}
	p.Inspector = inspector
	if _, err := Deploy(context.Background(), p); err == nil || !strings.Contains(err.Error(), "failed after restart") {
		t.Fatal("failed unit was accepted", err)
	}
	// The server was restarted, the worker failed, the disabled unit was
	// skipped and never touched.
	if strings.Join(inspector.units, " ") != "app-server.service app-worker.service" {
		t.Fatal("explained units", inspector.units)
	}
	text := log.String()
	if !strings.Contains(text, "==> app-server.service could not be inspected: journal unavailable\n") ||
		!strings.Contains(text, "==> app-worker.service since 22:0") ||
		!strings.Contains(text, ": ActiveState=failed Result=exit-code\n(no log output since the restart)\n") {
		t.Fatalf("log:\n%s", text)
	}
}

func TestPlanWithoutServicesExplainsItsUnits(t *testing.T) {
	p := Plan{Units: []string{"a.service", "b.service"}}
	if got := p.attempted(&Result{}); strings.Join(got, " ") != "a.service b.service" {
		t.Fatal(got)
	}
	p.Services = &fakeServices{}
	result := &Result{Restarted: []string{"a.service"}, Skipped: map[string]ServiceState{}}
	if got := p.attempted(result); strings.Join(got, " ") != "a.service b.service" {
		t.Fatal("the unit that did not come back is included", got)
	}
	result.Restarted = []string{"a.service", "b.service"}
	if got := p.attempted(result); strings.Join(got, " ") != "a.service b.service" {
		t.Fatal(got)
	}
}

func TestSummarizeUnitStateKeepsPropertyOrder(t *testing.T) {
	got := SummarizeUnitState("NRestarts=4\nActiveState=failed\nUnknown=1\nResult=exit-code\n")
	if got != "ActiveState=failed Result=exit-code NRestarts=4" {
		t.Fatal(got)
	}
	since := time.Date(2026, 11, 6, 14, 2, 11, 0, time.UTC)
	if args := strings.Join(JournalArgs("web.service", since), " "); args != "-u web.service --since @1793973731 -n 40 -o cat --no-pager" {
		t.Fatal(args)
	}
}
