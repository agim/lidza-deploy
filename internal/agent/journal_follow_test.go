package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testJournal is a fake journalctl: one export file per scope that the
// script follows like `journalctl --follow`, honouring --after-cursor and
// rejecting a cursor it does not hold. Every start is logged to "calls".
type testJournal struct {
	t   *testing.T
	dir string
	mu  sync.Mutex
}

const fakeJournalScript = `#!/bin/sh
scope=$1; shift
echo "$scope $*" >> "$DIR/calls"
after=""
for a in "$@"; do case "$a" in --after-cursor=*) after="${a#--after-cursor=}";; esac; done
file="$DIR/$scope.json"
touch "$file"
start=1
if [ -n "$after" ]; then
	n=$(grep -nF "\"__CURSOR\":\"$after\"" "$file" | head -n 1 | cut -d: -f1)
	if [ -z "$n" ]; then echo "Failed to seek to cursor: Invalid argument" >&2; exit 1; fi
	start=$((n+1))
fi
exec tail -n +$start -f "$file"
`

func fakeJournal(t *testing.T, m *Manager) *testJournal {
	t.Helper()
	j := &testJournal{t: t, dir: t.TempDir()}
	script := filepath.Join(j.dir, "journalctl")
	if err := os.WriteFile(script, []byte(fakeJournalScript), 0o755); err != nil {
		t.Fatal(err)
	}
	j.attach(m)
	return j
}

// attach routes a manager's journal streams to this fake.
func (j *testJournal) attach(m *Manager) {
	script := filepath.Join(j.dir, "journalctl")
	m.journalSpawn = func(ctx context.Context, user bool, args []string) (*exec.Cmd, error) {
		c := exec.CommandContext(ctx, script, append([]string{journalScopeKey(user)}, args...)...)
		c.Env = append(os.Environ(), "DIR="+j.dir)
		return c, nil
	}
	j.t.Cleanup(m.stopJournals)
}

func (j *testJournal) add(user bool, lines ...string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(j.dir, journalScopeKey(user)+".json"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		j.t.Fatal(err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			j.t.Fatal(err)
		}
	}
}

// calls are the journalctl starts, as "scope args...".
func (j *testJournal) calls() []string {
	raw, _ := os.ReadFile(filepath.Join(j.dir, "calls"))
	out := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// journalSinceOf is the --since value of a scope's first start.
func journalSinceOf(calls []string, scope string) string {
	for _, call := range calls {
		if !strings.HasPrefix(call, scope+" ") {
			continue
		}
		for _, arg := range strings.Fields(call) {
			if value, ok := strings.CutPrefix(arg, "--since=@"); ok {
				return value
			}
		}
	}
	return ""
}

func journalTestEntry(cursor string, at time.Time, field, unit, message string) string {
	entry := map[string]any{"__CURSOR": cursor, "__REALTIME_TIMESTAMP": strconv.FormatInt(at.UnixMicro(), 10), "MESSAGE": message}
	if field != "" {
		entry[field] = unit
	}
	data, _ := json.Marshal(entry)
	return string(data)
}

// journalHarness is a manager with created system-unit applications, each
// with a web and a worker unit, reporting to a recording endpoint.
type journalHarness struct {
	m         *Manager
	journal   *testJournal
	mu        sync.Mutex
	delivered []ConsoleError
	remote    *httptest.Server
}

func newJournalHarness(t *testing.T, apps int) *journalHarness {
	t.Helper()
	h := &journalHarness{m: testManager(t, &fakeRuntime{})}
	h.remote = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch ConsoleBatch
		if json.NewDecoder(r.Body).Decode(&batch) != nil {
			t.Error("invalid console delivery")
		}
		h.mu.Lock()
		h.delivered = append(h.delivered, batch.Errors...)
		h.mu.Unlock()
		JSON(w, 200, map[string]string{"status": "stored"})
	}))
	t.Cleanup(h.remote.Close)
	h.m.nativeCommand = func(_ context.Context, args []string) (string, error) {
		if args[0] == "journalctl" {
			t.Error("collector read a unit journal on its own", args)
		}
		return "", nil
	}
	h.journal = fakeJournal(t, h.m)
	h.m.mu.Lock()
	h.m.data.ErrorReporting = &ErrorReporting{URL: h.remote.URL + "/api/agent/errors", Server: "server-one", Apps: map[string]string{}}
	h.m.mu.Unlock()
	for i := range apps {
		h.addApp(fmt.Sprintf("app%02d", i))
	}
	return h
}

func (h *journalHarness) addApp(id string) {
	units := []string{"lidza-app-" + id + "-web.service", "lidza-app-" + id + "-worker.service"}
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if h.m.data.NativeApps == nil {
		h.m.data.NativeApps = map[string]NativeApplication{}
	}
	if h.m.data.NativeCreated == nil {
		h.m.data.NativeCreated = map[string]NativeReleasePolicy{}
	}
	if h.m.data.NativeReleases == nil {
		h.m.data.NativeReleases = map[string]NativeReleaseHistory{}
	}
	h.m.data.NativeApps[id] = NativeApplication{ID: id, Name: id, Runtime: "rails", Domain: id + ".example.com", Services: units}
	h.m.data.NativeCreated[id] = NativeReleasePolicy{Enabled: true, Runtime: "rails", Root: "/srv/lidza/apps/" + id, Repository: "acme/" + id, Branch: "main", User: "lidza-" + id, UnitScope: "system", Units: units}
	h.m.data.NativeReleases[id] = NativeReleaseHistory{Current: NativeReleaseRef{ID: "r-" + id, Commit: "c-" + id}}
	h.m.data.ErrorReporting.Apps[id] = strings.Repeat(id[len(id)-1:], 32)
}

func (h *journalHarness) reports() []ConsoleError {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.delivered)
}

func errorLine(unit string) string { return "RuntimeError (failure in " + unit + "):" }

// errorReport is the report message of an errorLine.
func errorReport(unit string) string { return strings.TrimSuffix(errorLine(unit), ":") }

func TestNativeJournalOneStreamDemultiplexesManyUnits(t *testing.T) {
	h := newJournalHarness(t, 20)
	at := time.Now().UTC().Add(-time.Minute)
	want := map[string]string{} // unit -> app
	n := 0
	for i := range 20 {
		id := fmt.Sprintf("app%02d", i)
		for _, unit := range []string{"lidza-app-" + id + "-web.service", "lidza-app-" + id + "-worker.service"} {
			n++
			h.journal.add(false, journalTestEntry("c"+strconv.Itoa(n), at.Add(time.Duration(n)*time.Millisecond), "_SYSTEMD_UNIT", unit, errorLine(unit)))
			want[unit] = id
		}
	}
	// systemd's own messages about a unit carry UNIT=, not _SYSTEMD_UNIT.
	h.journal.add(false,
		journalTestEntry("pid1", at.Add(time.Second), "UNIT", "lidza-app-app03-web.service", errorLine("systemd")),
		journalTestEntry("other", at.Add(time.Second), "_SYSTEMD_UNIT", "unrelated.service", "RuntimeError (not collected):"),
		journalTestEntry("binary", at.Add(time.Second), "_SYSTEMD_UNIT", "lidza-app-app04-web.service", ""))

	for range 5 {
		h.m.consoleTick(context.Background())
	}
	if spawned, calls := h.m.journalSpawned(), h.journal.calls(); spawned != 1 || len(calls) != 1 {
		t.Fatalf("40 units over 5 ticks spawned %d journalctl processes (%q); want one stream, the former per-unit reader spawned %d", spawned, calls, 40*5)
	}
	call := h.journal.calls()[0]
	for unit := range want {
		if !strings.Contains(call, " --unit="+unit) {
			t.Fatal("stream does not match unit", unit, call)
		}
	}
	got := map[string]string{}
	pid1 := false
	for _, r := range h.reports() {
		if strings.Contains(r.Message, "not collected") {
			t.Fatal("an unrelated unit's entry was reported", r)
		}
		if r.Message == errorReport("systemd") {
			pid1 = r.Container == "lidza-app-app03-web.service" && r.AppID == "app03"
			continue
		}
		if r.Message != errorReport(r.Container) {
			t.Fatal("report attributed to the wrong unit", r.Container, r.Message)
		}
		if _, dup := got[r.Container]; dup {
			t.Fatal("duplicated report", r.Container)
		}
		got[r.Container] = r.AppID
		if r.Release != "r-"+r.AppID || r.Generation != strings.Repeat(r.AppID[len(r.AppID)-1:], 32) {
			t.Fatal("report lost its release or incarnation", r)
		}
	}
	if len(got) != len(want) || !pid1 {
		t.Fatalf("demultiplexed %d of %d units (systemd message attributed: %v)", len(got), len(want), pid1)
	}
	for unit, app := range want {
		if got[unit] != app {
			t.Fatal("unit reported under the wrong application", unit, got[unit], app)
		}
	}
}

func TestNativeJournalResumesFromPersistedCursorAfterRestart(t *testing.T) {
	h := newJournalHarness(t, 2)
	at := time.Now().UTC().Add(-time.Minute)
	h.journal.add(false,
		journalTestEntry("c1", at, "_SYSTEMD_UNIT", "lidza-app-app00-web.service", errorLine("first")),
		journalTestEntry("c2", at.Add(time.Millisecond), "_SYSTEMD_UNIT", "lidza-app-app01-web.service", "plain output"))
	h.m.consoleTick(context.Background())
	h.m.mu.Lock()
	saved := h.m.data.JournalCursors["system"]
	h.m.mu.Unlock()
	if saved.Cursor != "c2" || len(saved.Units) != 4 {
		t.Fatal("resume point not persisted after the last parsed entry", saved)
	}
	if len(h.reports()) != 1 {
		t.Fatal("first tick did not report", h.reports())
	}
	cfg := h.m.cfg
	h.m.Close()

	h.journal.add(false, journalTestEntry("c3", at.Add(2*time.Millisecond), "_SYSTEMD_UNIT", "lidza-app-app01-worker.service", errorLine("after restart")))
	reopened, err := NewManager(context.Background(), cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.nativeCommand = h.m.nativeCommand
	h.journal.attach(reopened)
	reopened.consoleTick(context.Background())
	calls := h.journal.calls()
	if len(calls) != 2 || !strings.Contains(calls[1], "--after-cursor=c2") || strings.Contains(calls[1], "--since=") {
		t.Fatal("restart did not resume after the persisted cursor", calls)
	}
	reports := h.reports()
	if len(reports) != 2 || reports[1].Message != errorReport("after restart") || reports[1].AppID != "app01" {
		t.Fatal("restart lost or replayed entries", reports)
	}
}

func TestNativeJournalRestartsStreamWhenUnitsChange(t *testing.T) {
	h := newJournalHarness(t, 1)
	at := time.Now().UTC().Add(-time.Minute)
	h.journal.add(false, journalTestEntry("c1", at, "_SYSTEMD_UNIT", "lidza-app-app00-web.service", errorLine("one")))
	h.m.consoleTick(context.Background())
	h.m.consoleTick(context.Background())
	if calls := h.journal.calls(); len(calls) != 1 {
		t.Fatal("unchanged units restarted the stream", calls)
	}
	h.m.journal.mu.Lock()
	first := h.m.journal.streams[false]
	h.m.journal.mu.Unlock()

	h.addApp("app07")
	h.journal.add(false, journalTestEntry("c2", at.Add(time.Millisecond), "_SYSTEMD_UNIT", "lidza-app-app07-worker.service", errorLine("added")))
	h.m.consoleTick(context.Background())
	calls := h.journal.calls()
	// A new unit has no resume point of its own: the stream starts at the
	// oldest unit cursor so the new unit gets its usual lookback.
	if len(calls) != 2 || !strings.Contains(calls[1], "--unit=lidza-app-app07-worker.service") || !strings.Contains(calls[1], "--since=@") {
		t.Fatal("adding an application did not restart the stream with its units", calls)
	}
	select {
	case <-first.done:
	default:
		t.Fatal("the previous stream was left running")
	}
	reports := h.reports()
	if len(reports) != 2 || reports[1].AppID != "app07" {
		t.Fatal("added unit not collected, or earlier entries replayed", reports)
	}

	h.m.mu.Lock()
	delete(h.m.data.NativeApps, "app07")
	delete(h.m.data.ErrorReporting.Apps, "app07")
	h.m.mu.Unlock()
	h.m.consoleTick(context.Background())
	calls = h.journal.calls()
	if len(calls) != 3 || strings.Contains(calls[2], "app07") || !strings.Contains(calls[2], "--after-cursor=c2") {
		t.Fatal("removing an application did not restart the stream from its cursor", calls)
	}
	if len(h.reports()) != 2 {
		t.Fatal("restart replayed reports", h.reports())
	}
}

func TestNativeJournalFallsBackWhenCursorIsInvalid(t *testing.T) {
	h := newJournalHarness(t, 1)
	units := []string{"lidza-app-app00-web.service", "lidza-app-app00-worker.service"}
	h.m.mu.Lock()
	h.m.data.JournalCursors = map[string]JournalCursor{"system": {Cursor: "s=gone;i=1", Units: units}}
	h.m.mu.Unlock()
	h.journal.add(false, journalTestEntry("c1", time.Now().UTC().Add(-time.Minute), "_SYSTEMD_UNIT", units[1], errorLine("recent")))
	h.m.consoleTick(context.Background())
	calls := h.journal.calls()
	if len(calls) != 2 || !strings.Contains(calls[0], "--after-cursor=s=gone;i=1") || !strings.Contains(calls[1], "--since=@") {
		t.Fatal("invalid cursor did not fall back to a time window", calls)
	}
	if reports := h.reports(); len(reports) != 1 || reports[0].Container != units[1] {
		t.Fatal("fallback stream did not collect", reports)
	}
	h.m.mu.Lock()
	saved := h.m.data.JournalCursors["system"]
	h.m.mu.Unlock()
	if saved.Cursor != "c1" {
		t.Fatal("invalid cursor not replaced", saved)
	}
}

func TestNativeJournalBuffersAreBounded(t *testing.T) {
	s := &journalStream{units: []string{"a.service"}, buffers: map[string]*journalBuffer{"a.service": {}}}
	set := map[string]bool{"a.service": true}
	at := time.Now().UTC()
	for i := range journalBufferLines + 500 {
		s.add([]byte(journalTestEntry("c"+strconv.Itoa(i), at.Add(time.Duration(i)*time.Microsecond), "_SYSTEMD_UNIT", "a.service", "line "+strconv.Itoa(i))), set)
	}
	b := s.buffers["a.service"]
	if len(b.lines) != journalBufferLines || !b.dropped || b.lines[0].text[len(b.lines[0].text)-8:] != "line 500" {
		t.Fatal("buffer did not keep the newest lines", len(b.lines), b.lines[0].text)
	}
	feed, limited := s.take("a.service")
	if strings.Count(feed, "\n") != journalFeedLines || !limited {
		t.Fatal("one parse is not bounded like the former read", strings.Count(feed, "\n"), limited)
	}
	big := strings.Repeat("x", 100<<10)
	for i := range 4 {
		s.add([]byte(journalTestEntry("b"+strconv.Itoa(i), at.Add(time.Hour), "_SYSTEMD_UNIT", "a.service", big)), set)
	}
	if b.bytes > journalBufferBytes {
		t.Fatal("buffer exceeds its byte bound", b.bytes)
	}
}

func TestNativeJournalResumePointIgnoresParsedLines(t *testing.T) {
	s := &journalStream{units: []string{"a.service", "b.service"}, buffers: map[string]*journalBuffer{"a.service": {}, "b.service": {}}, lastCursor: "start"}
	set := map[string]bool{"a.service": true, "b.service": true}
	old := time.Now().UTC().Add(-72 * time.Hour)
	recent := time.Now().UTC()
	s.add([]byte(journalTestEntry("a1", old, "_SYSTEMD_UNIT", "a.service", "quiet unit's last line")), set)
	s.add([]byte(journalTestEntry("b1", recent, "_SYSTEMD_UNIT", "b.service", "busy")), set)
	s.add([]byte(journalTestEntry("b2", recent.Add(time.Millisecond), "_SYSTEMD_UNIT", "b.service", "busier")), set)
	if cursor, _ := s.resumeCursor(); cursor != "start" {
		t.Fatal("unparsed lines must hold the resume point", cursor)
	}
	s.consumed("a.service", old) // the last line stays buffered for repeat counting
	s.consumed("b.service", recent)
	if cursor, _ := s.resumeCursor(); cursor != "b1" {
		t.Fatal("resume point should precede the oldest unparsed line", cursor)
	}
	s.consumed("b.service", recent.Add(time.Millisecond))
	if cursor, _ := s.resumeCursor(); cursor != "b2" {
		t.Fatal("a quiet unit's parsed line held the resume point back", cursor)
	}
}

// A journald restart (the journal size policy restarts it) can end a
// follower; the next tick starts a new one after the last entry, so
// nothing is lost or reported twice.
func TestNativeJournalRestartsAfterJournaldEndsTheStream(t *testing.T) {
	h := newJournalHarness(t, 1)
	unit := "lidza-app-app00-web.service"
	at := time.Now().UTC().Add(-time.Minute)
	h.journal.add(false, journalTestEntry("c1", at, "_SYSTEMD_UNIT", unit, errorLine("before")))
	h.m.consoleTick(context.Background())
	if len(h.reports()) != 1 {
		t.Fatal("first tick did not report", h.reports())
	}
	h.m.journal.mu.Lock()
	first := h.m.journal.streams[false]
	h.m.journal.mu.Unlock()
	// The follower's journalctl is killed under it, as when journald restarts;
	// the stream stays registered, so only its exit tells the collector.
	first.cancel()
	<-first.done

	h.journal.add(false, journalTestEntry("c2", at.Add(time.Millisecond), "_SYSTEMD_UNIT", unit, errorLine("after")))
	h.m.consoleTick(context.Background())
	calls := h.journal.calls()
	if len(calls) != 2 || !strings.Contains(calls[1], "--after-cursor=c1") {
		t.Fatal("the ended stream was not restarted from its cursor", calls)
	}
	reports := h.reports()
	if len(reports) != 2 || reports[1].Message != errorReport("after") {
		t.Fatal("entries after the restart were lost or replayed", reports)
	}
	h.m.consoleTick(context.Background())
	if calls := h.journal.calls(); len(calls) != 2 {
		t.Fatal("a healthy restarted stream was restarted again", calls)
	}
}
