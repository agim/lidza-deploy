package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func logAccessCall(t *testing.T, m *Manager, method, body string) (int, LogAccessStatus) {
	t.Helper()
	mux := http.NewServeMux()
	m.logAccessRoutes(mux)
	req := httptest.NewRequest(method, "/v1/host/log-access", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var status LogAccessStatus
	_ = json.Unmarshal(w.Body.Bytes(), &status)
	return w.Code, status
}

// logAccessHost is a host whose /etc/group the fake gpasswd edits.
type logAccessHost struct {
	path  string
	mu    sync.Mutex
	calls [][]string
	fail  error
}

func newLogAccessHost(t *testing.T, group string) *logAccessHost {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &logAccessHost{path: filepath.Join(root, "etc", "group")}
	h.write(t, group)
	saved := hostPath
	hostPath = func(path string) string { return filepath.Join(root, path) }
	t.Cleanup(func() { hostPath = saved })
	return h
}

func (h *logAccessHost) write(t *testing.T, group string) {
	t.Helper()
	if err := os.WriteFile(h.path, []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *logAccessHost) read(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// run is gpasswd --add/--delete USER GROUP on the fixture's /etc/group.
func (h *logAccessHost) run(_ context.Context, args []string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, slices.Clone(args))
	if h.fail != nil {
		return "", h.fail
	}
	if len(args) != 4 || args[0] != "gpasswd" {
		return "", errors.New("unexpected command")
	}
	raw, err := os.ReadFile(h.path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	for i, line := range lines {
		fields := strings.Split(line, ":")
		if fields[0] != args[3] {
			continue
		}
		members := []string{}
		if fields[3] != "" {
			members = strings.Split(fields[3], ",")
		}
		switch args[1] {
		case "--add":
			if !slices.Contains(members, args[2]) {
				members = append(members, args[2])
			}
		case "--delete":
			members = slices.DeleteFunc(members, func(member string) bool { return member == args[2] })
		}
		fields[3] = strings.Join(members, ",")
		lines[i] = strings.Join(fields, ":")
	}
	return "", os.WriteFile(h.path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func logAccessJobs(m *Manager) []HostJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(m.hostUpdatesStateLocked().Jobs), func(job HostJob) bool { return job.Kind != logAccessJobKind })
}

func TestLogAccessSettingIsPersistedAndAppliedToOnlyThatGroup(t *testing.T) {
	m, _ := nativeFixture(t)
	account := m.cfg.Native.User
	group := "adm:x:4:syslog," + account + "\nsystemd-journal:x:101:syslog\ndocker:x:998:" + account + "\n"
	h := newLogAccessHost(t, group)
	m.nativeCommand = h.run
	ctx := context.Background()

	code, status := logAccessCall(t, m, "GET", "")
	if code != 200 || !status.Supported || status.Enabled || status.Member || status.Source != "config" || status.Account != account || status.Group != "systemd-journal" {
		t.Fatalf("initial: %d %+v", code, status)
	}
	// Never set: the agent leaves the membership alone, even a manual one.
	h.write(t, "systemd-journal:x:101:"+account+"\n")
	m.logAccessTick(time.Now())
	if len(h.calls) != 0 {
		t.Fatalf("reconciled an unset setting: %v", h.calls)
	}
	h.write(t, group)
	if code, _ := logAccessCall(t, m, "PUT", `{}`); code != 400 {
		t.Fatalf("missing enabled accepted: %d", code)
	}

	code, status = logAccessCall(t, m, "PUT", `{"enabled":true}`)
	if code != 200 || !status.Enabled || !status.Member || status.Source != "setting" || status.Applied == nil || status.Error != "" {
		t.Fatalf("enable: %d %+v", code, status)
	}
	want := "adm:x:4:syslog," + account + "\nsystemd-journal:x:101:syslog," + account + "\ndocker:x:998:" + account + "\n"
	if got := h.read(t); got != want {
		t.Fatalf("group file after enabling:\n%s", got)
	}
	if !slices.Equal(h.calls[0], []string{"gpasswd", "--add", account, "systemd-journal"}) {
		t.Fatalf("add: %v", h.calls)
	}
	if jobs := logAccessJobs(m); len(jobs) != 1 || jobs[0].Status != "succeeded" || jobs[0].Trigger != "manual" || !strings.Contains(jobs[0].Summary, "new SSH sessions") {
		t.Fatalf("job: %+v", jobs)
	}
	// Idempotent: nothing runs while the membership matches.
	m.logAccessTick(time.Now())
	if len(h.calls) != 1 || len(logAccessJobs(m)) != 1 {
		t.Fatalf("reconciled a matching membership: %v", h.calls)
	}
	// A membership removed by hand is put back.
	h.write(t, group)
	m.logAccessTick(time.Now())
	if got := h.read(t); got != want || len(h.calls) != 2 {
		t.Fatalf("manual removal not reconciled: %v\n%s", h.calls, got)
	}

	// The setting survives a restart.
	cfg := m.cfg
	m.Close()
	reopened, err := NewManager(ctx, cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.nativeCommand = h.run
	if _, status := logAccessCall(t, reopened, "GET", ""); !status.Enabled || status.Source != "setting" || !status.Member {
		t.Fatalf("stored setting lost on restart: %+v", status)
	}

	// Off removes only the systemd-journal membership.
	code, status = logAccessCall(t, reopened, "PUT", `{"enabled":false}`)
	if code != 200 || status.Enabled || status.Member {
		t.Fatalf("disable: %d %+v", code, status)
	}
	if got := h.read(t); got != group {
		t.Fatalf("group file after disabling:\n%s", got)
	}
	if last := h.calls[len(h.calls)-1]; !slices.Equal(last, []string{"gpasswd", "--delete", account, "systemd-journal"}) {
		t.Fatalf("delete: %v", h.calls)
	}
	for _, call := range h.calls {
		if call[0] != "gpasswd" || call[3] != "systemd-journal" {
			t.Fatalf("touched another group: %v", call)
		}
	}
	// A membership added by hand while off is removed again.
	h.write(t, want)
	reopened.logAccessTick(time.Now())
	if got := h.read(t); got != group {
		t.Fatalf("manual addition not reconciled:\n%s", got)
	}
}

func TestLogAccessReportsFailuresOnce(t *testing.T) {
	m, _ := nativeFixture(t)
	h := newLogAccessHost(t, "systemd-journal:x:101:\n")
	h.fail = errors.New("gpasswd: permission denied")
	m.nativeCommand = h.run
	code, status := logAccessCall(t, m, "PUT", `{"enabled":true}`)
	if code != 200 || !status.Enabled || status.Member || !strings.Contains(status.Error, "could not add") {
		t.Fatalf("failed apply: %d %+v", code, status)
	}
	m.logAccessTick(time.Now())
	if jobs := logAccessJobs(m); len(jobs) != 1 || jobs[0].Status != "failed" {
		t.Fatalf("a repeated failure was recorded again: %+v", jobs)
	}
	h.fail = nil
	m.logAccessTick(time.Now())
	if _, status := logAccessCall(t, m, "GET", ""); !status.Member || status.Error != "" {
		t.Fatalf("recovery: %+v", status)
	}

	// Without the group, enabling reports it instead of creating one.
	h.write(t, "adm:x:4:\n")
	before := len(h.calls)
	m.logAccessTick(time.Now())
	if _, status := logAccessCall(t, m, "GET", ""); !strings.Contains(status.Error, "does not exist") || len(h.calls) != before {
		t.Fatalf("missing group: %+v %v", status, h.calls)
	}
}

func TestLogAccessNeedsASharedDeploymentAccount(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if code, status := logAccessCall(t, m, "GET", ""); code != 200 || status.Supported || status.Reason == "" {
		t.Fatalf("status without native management: %d %+v", code, status)
	}
	if code, _ := logAccessCall(t, m, "PUT", `{"enabled":true}`); code != 409 {
		t.Fatalf("set without native management: %d", code)
	}
	m.cfg.Native = &NativeHostConfig{Root: t.TempDir()}
	if code, status := logAccessCall(t, m, "PUT", `{"enabled":true}`); code != 409 || status.Supported {
		t.Fatalf("set without an account: %d", code)
	}
	m.cfg.Native.User = "root"
	if code, _ := logAccessCall(t, m, "PUT", `{"enabled":true}`); code != 409 {
		t.Fatalf("set for root: %d", code)
	}
}
