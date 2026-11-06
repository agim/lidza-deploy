package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// quickHealth shortens the health check's retry window for a test.
func quickHealth(t *testing.T) {
	t.Helper()
	deadline, retry := nativeHealthDeadline, nativeHealthRetry
	nativeHealthDeadline, nativeHealthRetry = 50*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { nativeHealthDeadline, nativeHealthRetry = deadline, retry })
}

func TestNativeReleaseExplainsFailedHealthCheckInTheDeploymentLog(t *testing.T) {
	f := newNativeReleaseFixture(t)
	f.m.cfg.Native.Releases[f.app.ID] = f.policy()
	f.commit("VERSION", "one\n")
	w := f.call(t, f.m.nativeReleaseRoute(false), "")
	var queued Deployment
	decodeReleaseJSON(t, w.Body.String(), &queued)
	if done := waitDeployment(t, f.m, queued.ID); done.Status != "live" {
		t.Fatalf("first deployment %+v\n%s", done, done.Log)
	}
	// The next release answers its health check with an error page that
	// echoes a configured secret; so does the republished previous release.
	quickHealth(t)
	down := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("upstream unavailable; SECRET_KEY_BASE keep-me rejected " + strings.Repeat("x", 500)))
	}))
	defer down.Close()
	f.m.nativeReleaseHealthAddress = strings.TrimPrefix(down.URL, "https://")
	f.commit("VERSION", "two\n")
	f.units.journals = nil
	w = f.call(t, f.m.nativeReleaseRoute(false), "")
	decodeReleaseJSON(t, w.Body.String(), &queued)
	done := waitDeployment(t, f.m, queued.ID)
	if done.Status != "failed" || !strings.Contains(done.Error, "HTTP 503") {
		t.Fatalf("deployment %+v\n%s", done, done.Log)
	}
	for _, want := range []string{
		"==> last health response: HTTP 503\nupstream unavailable; SECRET_KEY_BASE [redacted] rejected x",
		"==> myrails-server.service since ",
		"==> myrails-worker.service since ",
		": ActiveState=activating SubState=auto-restart Result=exit-code NRestarts=3 ExecMainStatus=1 ExecMainCode=exited\nboot failed: SECRET_KEY_BASE [redacted] is not accepted\n",
		"==> new release failed (release: health check failed: HTTP 503 from myrails.example.com/up); republishing ",
	} {
		if !strings.Contains(done.Log, want) {
			t.Fatalf("missing %q in deployment log:\n%s", want, done.Log)
		}
	}
	if strings.Contains(done.Log, "keep-me") || strings.Contains(done.Log, strings.Repeat("x", 301)) {
		t.Fatalf("secret or unbounded body in deployment log:\n%s", done.Log)
	}
	if n := strings.Count(done.Log, "==> last health response"); n != 2 {
		t.Fatalf("expected an explanation for the release and for the rollback, got %d:\n%s", n, done.Log)
	}
	if strings.Contains(done.Log, "myrails-monitor.service since") {
		t.Fatal("preserved unit was explained", done.Log)
	}
	if len(f.units.journals) != 4 || !strings.HasPrefix(f.units.journals[0], "-u myrails-server.service --since @") ||
		!strings.HasSuffix(f.units.journals[0], " -n 40 -o cat --no-pager") {
		t.Fatalf("journal reads %q", f.units.journals)
	}
}

func TestNativeSystemServicesInspectReadsStateAndJournalQuietly(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	var calls []string
	m.nativeCommand = func(_ context.Context, args []string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "systemctl":
			return "ActiveState=failed\nSubState=failed\nResult=exit-code\nNRestarts=5\nExecMainStatus=1\nExecMainCode=exited\n", nil
		case "journalctl":
			return "AUTH_SECRET is not set\n", nil
		}
		return "", errors.New("unexpected " + args[0])
	}
	var logged strings.Builder
	ctx := context.WithValue(context.Background(), diagnosticKey{}, func(line string) { logged.WriteString(line) })
	since := time.Date(2026, 11, 6, 14, 2, 11, 0, time.UTC)
	report, err := nativeSystemServices{m: m}.Inspect(ctx, "shop-web.service", since)
	if err != nil {
		t.Fatal(err)
	}
	if release.SummarizeUnitState(report.State) != "ActiveState=failed SubState=failed Result=exit-code NRestarts=5 ExecMainStatus=1 ExecMainCode=exited" || report.Log != "AUTH_SECRET is not set\n" {
		t.Fatalf("report %+v", report)
	}
	if len(calls) != 2 || calls[0] != "systemctl show shop-web.service --no-pager --property="+release.UnitReportProperties ||
		calls[1] != "journalctl -u shop-web.service --since @1793973731 -n 40 -o cat --no-pager" {
		t.Fatalf("calls %q", calls)
	}
	if logged.Len() != 0 {
		t.Fatalf("the probes themselves reached the log: %q", logged.String())
	}
	if _, err := (nativeSystemServices{m: m}).Inspect(ctx, "../etc", since); err == nil {
		t.Fatal("invalid unit inspected")
	}
}

func TestNativeReleaseSystemPlanKeepsUnitsToExplain(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	plan := release.Plan{Units: []string{"shop-web.service"}}
	policy := NativeReleasePolicy{Units: []string{"shop-web.service", "shop-worker.service", "shop-import.service"},
		Preserve: []string{"shop-import.service"}, UnitScope: "system", User: "shop"}
	m.nativeReleaseSystemPlan(&plan, "shop", policy)
	if plan.Services != nil || strings.Join(plan.Units, " ") != "shop-web.service shop-worker.service" {
		t.Fatalf("services %v units %v", plan.Services, plan.Units)
	}
	if _, ok := plan.Inspector.(nativeSystemServices); !ok {
		t.Fatalf("inspector %T", plan.Inspector)
	}
}

func TestNativeReleaseMaskHidesEnvironmentFileValues(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(file, []byte("SECRET_KEY_BASE=abc123secret\nDATABASE_URL=postgres://app:pw@localhost/app\nPORT=3000\nRAILS_ENV=production\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mask := nativeReleaseMask(NativeReleasePolicy{EnvFiles: []string{file}})
	if mask == nil {
		t.Fatal("no mask for an environment with secrets")
	}
	got := mask("key abc123secret url postgres://app:pw@localhost/app password pw port 3000 env production")
	if got != "key [redacted] url [redacted] password [redacted] port 3000 env production" {
		t.Fatal(got)
	}
	if nativeReleaseMask(NativeReleasePolicy{}) != nil {
		t.Fatal("mask without environment files")
	}
	if nativeReleaseMask(NativeReleasePolicy{EnvFiles: []string{filepath.Join(t.TempDir(), "missing")}}) != nil {
		t.Fatal("unreadable environment must not break the deploy")
	}
}

func TestNativeHealthChecksReportTheLastResponse(t *testing.T) {
	quickHealth(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("upstream down " + strings.Repeat("y", 1000)))
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	err := nativeLoopbackHealth(port, "app.example.com", "/healthz")(context.Background())
	var response *release.HealthResponse
	if !errors.As(err, &response) || response.Status != 503 || !strings.HasPrefix(response.Body, "upstream down ") || len(response.Body) != release.HealthBodyLimit {
		t.Fatalf("err %v response %+v", err, response)
	}
	if err.Error() != "HTTP 503 from /healthz on port "+port {
		t.Fatal(err)
	}
	// A refused connection carries no response.
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	_, closed, _ := net.SplitHostPort(listener.Addr().String())
	listener.Close()
	var none *release.HealthResponse
	if err := nativeLoopbackHealth(closed, "", "")(context.Background()); err == nil || errors.As(err, &none) {
		t.Fatal(err)
	}
}
