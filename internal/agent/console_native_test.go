package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConsoleParsesRailsNodeAndAccessLogLines(t *testing.T) {
	stamp := time.Now().UTC().Add(-time.Minute)
	line := func(i int, s string) string {
		return stamp.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano) + " " + s + "\n"
	}
	request, job, probe, other, scan := "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", "9f8e7d6c-5b4a-4c3d-8e2f-1a0b9c8d7e6f", "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e", "c3d4e5f6-a7b8-4c9d-0e1f-2a3b4c5d6e7f", "d4e5f6a7-b8c9-4d0e-1f2a-3b4c5d6e7f80"
	logs := line(0, "["+request+`] Started GET "/orders?token=private-query" for 127.0.0.1 at 2026-11-05 10:00:00 +0000`) +
		line(1, "["+request+"] Processing by OrdersController#index as HTML") +
		line(2, "["+request+"] Completed 500 Internal Server Error in 12ms (ActiveRecord: 1.2ms | Allocations: 1234)") +
		line(3, "F, [2026-11-05T10:00:00.123456 #1234] FATAL -- : ["+request+"]   ") +
		line(4, "["+request+"] NoMethodError (undefined method 'total' for nil runtime-secret):") +
		line(5, "["+request+"]   ") +
		line(6, "["+request+"] app/controllers/orders_controller.rb:10:in 'index'") +
		line(7, "["+request+"] /usr/lib/ruby/gems/3.3.0/gems/actionpack-7.1.3/lib/action_controller/metal/basic_implicit_render.rb:6:in 'send_action'") +
		line(8, "[ActiveJob] [ReportJob] ["+job+"] Error performing ReportJob (Job ID: "+job+") from Async(default) in 3.1ms: PG::ConnectionBad (could not connect to server):") +
		line(9, "["+probe+`] Started GET "/.env" for 10.0.0.9 at 2026-11-05 10:00:01 +0000`) +
		line(10, "["+probe+`] ActionController::RoutingError (No route matches [GET] "/.env"):`) +
		line(11, "Error [ERR_MODULE_NOT_FOUND]: Cannot find package 'left-pad' imported from /srv/app/server.js") +
		line(12, "    at packageResolve (node:internal/modules/esm/resolve:873:9)") +
		line(13, "    at moduleResolve (node:internal/modules/esm/resolve:946:18)") +
		line(14, `{"level":50,"time":1762336800000,"msg":"request failed","err":{"type":"TypeError","message":"x is not a function","stack":"TypeError: x is not a function\n    at handler (/srv/app/routes.js:12:3)"}}`) +
		line(15, `10.0.0.9 - - [05/Nov/2026:10:00:02 +0000] "GET /.git/config HTTP/1.1" 404 153 "-" "curl/8.0"`) +
		line(16, "["+other+`] Started GET "/products" for 10.0.0.9 at 2026-11-05 10:00:03 +0000`) +
		line(17, "["+other+"] Completed 200 OK in 5ms (Views: 3.1ms | ActiveRecord: 0.4ms)") +
		line(18, "[1234] ! Terminating timed out worker (Worker 0 PID: 5678)") +
		line(19, "[WARN] something benign") +
		line(20, "["+scan+`] Started GET "/wp-admin/.env" for 10.0.0.9 at 2026-11-05 10:00:04 +0000`) +
		line(21, "["+scan+"] Completed 404 Not Found in 2ms (ActiveRecord: 0.0ms)")
	events, _ := parseConsole(logs, testApp("rails"), Release{ID: "r1", Container: "lidza-app-rails-web.service"}, strings.Repeat("g", 32), ConsoleCursor{Time: stamp.Add(-time.Second)}, 100)
	messages := []string{}
	for _, e := range events {
		messages = append(messages, e.Message)
	}
	want := []string{
		"HTTP 500 Internal Server Error",
		"NoMethodError (undefined method 'total' for nil [redacted])",
		"Error performing ReportJob (Job ID: " + job + ") from Async(default) in 3.1ms: PG::ConnectionBad (could not connect to server)",
		"Suspected secret-file probe: HTTP 404",
		"Error [ERR_MODULE_NOT_FOUND]: Cannot find package 'left-pad' imported from /srv/app/server.js",
		"request failed: TypeError: x is not a function",
		"Suspected secret-file probe: HTTP 404",
		"! Terminating timed out worker (Worker 0 PID: 5678)",
		"Suspected secret-file probe: HTTP 404",
	}
	if !slices.Equal(messages, want) {
		t.Fatalf("records\n got %q\nwant %q", messages, want)
	}
	text := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if text(events[0].Route) != "/orders" || text(events[0].Method) != "GET" || text(events[0].RequestID) != request {
		t.Fatal("Rails 500 lost its Started request context", events[0])
	}
	if text(events[1].Route) != "/orders" || !strings.Contains(text(events[1].Stack), "orders_controller.rb:10") || !strings.Contains(text(events[1].Stack), "basic_implicit_render.rb:6") || strings.Contains(text(events[1].Stack), request) {
		t.Fatal("Rails exception route or backtrace lost", text(events[1].Stack))
	}
	if text(events[2].RequestID) != job {
		t.Fatal("job id tag not used as the request id", events[2])
	}
	if events[3].Security == nil || events[3].Security.Category != "secret-file" || events[3].Security.Outcome != "rejected" || text(events[3].Route) != "/.env" || text(events[3].RequestID) != probe {
		t.Fatal("Rails routing probe misclassified", events[3])
	}
	if !strings.Contains(text(events[4].Stack), "packageResolve") || !strings.Contains(text(events[4].Stack), "moduleResolve") {
		t.Fatal("Node frames not attached", text(events[4].Stack))
	}
	if !strings.Contains(text(events[5].Stack), "routes.js:12") {
		t.Fatal("pino error stack not read", events[5])
	}
	if events[6].Security == nil || text(events[6].Route) != "/.git/config" || text(events[6].Method) != "GET" {
		t.Fatal("access-log probe misclassified", events[6])
	}
	if events[8].Security == nil || text(events[8].Route) != "/wp-admin/.env" || text(events[8].RequestID) != scan {
		t.Fatal("Started/Completed probe misclassified", events[8])
	}
	payload, _ := json.Marshal(events)
	if strings.Contains(string(payload), "runtime-secret") || strings.Contains(string(payload), "private-query") {
		t.Fatal("secret or query leaked into reports")
	}
}

func TestJournalConsoleLinesConvertsEntries(t *testing.T) {
	at := time.Date(2026, 11, 5, 10, 0, 0, 123456000, time.UTC)
	micros := strconv.FormatInt(at.UnixMicro(), 10)
	raw := `{"__REALTIME_TIMESTAMP":"` + micros + `","MESSAGE":"first line"}` + "\n" +
		`{"__REALTIME_TIMESTAMP":"` + micros + `","MESSAGE":"two\nlines\n"}` + "\n" +
		`{"__REALTIME_TIMESTAMP":"` + micros + `","MESSAGE":[104,105]}` + "\n" +
		`{"__REALTIME_TIMESTAMP":"not-a-number","MESSAGE":"skipped"}` + "\n" +
		"-- No entries --\n"
	got := journalConsoleLines(raw)
	stamp := at.Format(time.RFC3339Nano)
	want := stamp + " first line\n" + stamp + " two\n" + stamp + " lines\n"
	if got != want {
		t.Fatalf("journal conversion\n got %q\nwant %q", got, want)
	}
}

func TestConsoleCollectsNativeServiceJournals(t *testing.T) {
	m, _ := nativeFixture(t)
	if _, err := m.nativeRegistration(NativeApplication{ID: "my-rails", Name: "MyRails", Domain: "rails.example.com"}); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "shop.env")
	if err := os.WriteFile(envFile, []byte("SECRET_KEY_BASE=created-secret-value\nRAILS_LOG_TO_STDOUT=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created, discovered, static := strings.Repeat("c", 32), strings.Repeat("d", 32), strings.Repeat("s", 32)
	m.mu.Lock()
	m.data.NativeApps["shop"] = NativeApplication{ID: "shop", Name: "shop", Runtime: "rails", Domain: "shop.example.com", Services: []string{"lidza-app-shop-web.service"}}
	m.data.NativeApps["site"] = NativeApplication{ID: "site", Name: "site", Runtime: "astro", Domain: "site.example.com", DocumentRoot: "public"}
	m.data.NativeApps["unassigned"] = NativeApplication{ID: "unassigned", Name: "unassigned", Runtime: "rails", Services: []string{"lidza-app-unassigned-web.service"}}
	m.data.NativeCreated = map[string]NativeReleasePolicy{"shop": {Enabled: true, Runtime: "rails", Root: "/srv/lidza/apps/shop", Repository: "acme/shop", Branch: "main", User: "lidza-shop", UnitScope: "system", Units: []string{"lidza-app-shop-web.service"}, EnvFiles: []string{envFile}}}
	m.data.NativeReleases = map[string]NativeReleaseHistory{"shop": {Current: NativeReleaseRef{ID: "20261105100000", Commit: "abc123", Path: "/srv/lidza/apps/shop/releases/20261105100000"}}}
	m.mu.Unlock()

	var mu sync.Mutex
	calls := [][]string{}
	at := time.Now().UTC().Add(-time.Minute)
	entry := func(message string) string {
		data, _ := json.Marshal(map[string]any{"__REALTIME_TIMESTAMP": strconv.FormatInt(at.UnixMicro(), 10), "MESSAGE": message})
		return string(data) + "\n"
	}
	m.nativeCommand = func(_ context.Context, args []string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if args[0] != "journalctl" {
			return "", nil
		}
		calls = append(calls, slices.Clone(args))
		if !slices.Contains(args, "--output=json") || !slices.Contains(args, "--lines=1000") || !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--since=@") }) {
			t.Error("journal read missing collector flags", args)
		}
		switch args[1] {
		case "--unit=lidza-app-shop-web.service":
			return entry("[a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d] NoMethodError (undefined method 'total' for nil created-secret-value):") + entry("[a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d] app/models/order.rb:12:in 'total'"), nil
		case "--user-unit=myrails-server.service":
			return entry(`{"msg":"request","path":"/.env","method":"GET","status":404,"request_id":"req-1"}`), nil
		case "--user-unit=myrails-worker.service":
			return "", nil
		}
		t.Error("unexpected journal read", args)
		return "", nil
	}
	var deliveredMu sync.Mutex
	delivered := []ConsoleError{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch ConsoleBatch
		if json.NewDecoder(r.Body).Decode(&batch) != nil {
			t.Error("invalid console delivery")
		}
		deliveredMu.Lock()
		delivered = append(delivered, batch.Errors...)
		deliveredMu.Unlock()
		JSON(w, 200, map[string]string{"status": "stored"})
	}))
	defer remote.Close()
	m.mu.Lock()
	m.data.ErrorReporting = &ErrorReporting{URL: remote.URL + "/api/agent/errors", Server: "server-one", Apps: map[string]string{"shop": created, "my-rails": discovered, "site": static}}
	m.mu.Unlock()

	m.consoleTick(context.Background())

	mu.Lock()
	units := []string{}
	for _, call := range calls {
		units = append(units, call[1])
	}
	mu.Unlock()
	slices.Sort(units)
	if !slices.Equal(units, []string{"--unit=lidza-app-shop-web.service", "--user-unit=myrails-server.service", "--user-unit=myrails-worker.service"}) {
		t.Fatal("collector read the wrong units", units)
	}
	deliveredMu.Lock()
	count := len(delivered)
	byApp := map[string]ConsoleError{}
	for _, e := range delivered {
		byApp[e.AppID] = e
	}
	deliveredMu.Unlock()
	if count != 2 {
		t.Fatal("native reports not delivered", count)
	}
	shop, rails := byApp["shop"], byApp["my-rails"]
	if shop.Generation != created || shop.Container != "lidza-app-shop-web.service" || shop.Release != "20261105100000" || shop.Commit != "abc123" || shop.Message != "NoMethodError (undefined method 'total' for nil [redacted])" || shop.Stack == nil || !strings.Contains(*shop.Stack, "order.rb:12") {
		t.Fatal("created application report incomplete or unmasked", shop)
	}
	if rails.Generation != discovered || rails.Container != "myrails-server.service" || rails.Security == nil || rails.Security.Category != "secret-file" {
		t.Fatal("discovered application probe not attributed", rails)
	}
	m.mu.Lock()
	_, cursorKept := m.data.ErrorCursors[created+":lidza-app-shop-web.service"]
	outbox := len(m.data.ErrorOutbox)
	m.mu.Unlock()
	if !cursorKept || outbox != 0 {
		t.Fatal("native cursor or outbox state wrong", cursorKept, outbox)
	}
	m.consoleTick(context.Background())
	deliveredMu.Lock()
	count = len(delivered)
	deliveredMu.Unlock()
	if count != 2 {
		t.Fatal("journal replay duplicated native reports", count)
	}
}
