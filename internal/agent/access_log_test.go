package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// hatchboxLogging is the access logging of a host whose Caddyfile logged
// every site to the journal: each hostname mapped to the default logger.
func hatchboxLogs() map[string]any {
	return map[string]any{"logger_names": map[string]any{
		"shop.example.com": []any{""}, "www.shop.example.com": "", "other.example.com": []any{""},
	}}
}

func TestAccessLogMappingKeepsTheJournalAndAddsTheApplicationLogger(t *testing.T) {
	hosts := map[string]string{"shop.example.com": "shop", "www.shop.example.com": "shop"}
	next, changed := accessLogServerLogs(hatchboxLogs(), hosts)
	names := next["logger_names"].(map[string]any)
	if !changed || !slices.Equal(caddyNameList(names["shop.example.com"]), []string{"", "lidza-access-shop"}) || !slices.Equal(caddyNameList(names["www.shop.example.com"]), []string{"", "lidza-access-shop"}) {
		t.Fatalf("mapping = %v", names)
	}
	if !slices.Equal(caddyNameList(names["other.example.com"]), []string{""}) {
		t.Fatal("another application's hostname changed")
	}
	if again, changed := accessLogServerLogs(next, hosts); changed || !jsonEqual(mustJSON(again), mustJSON(next)) {
		t.Fatal("a second pass must change nothing")
	}
	// Switched off: the names go, the hostnames log as before.
	back, changed := accessLogServerLogs(next, nil)
	names = back["logger_names"].(map[string]any)
	if !changed || !slices.Equal(caddyNameList(names["shop.example.com"]), []string{""}) || !slices.Equal(caddyNameList(names["www.shop.example.com"]), []string{""}) {
		t.Fatalf("withdrawn mapping = %v", names)
	}
	// Untouched entries keep their exact form.
	if untouched, _ := accessLogServerLogs(hatchboxLogs(), map[string]string{"shop.example.com": "shop"}); untouched["logger_names"].(map[string]any)["www.shop.example.com"] != "" {
		t.Fatal("an unrelated string entry was rewritten")
	}
}

func TestAccessLogMappingOfHostnamesThatWereNotMapped(t *testing.T) {
	cases := []struct {
		name string
		logs map[string]any
		want []string
	}{
		{"unmapped, logged by default", map[string]any{}, []string{"", "lidza-access-shop"}},
		{"unmapped with a default logger name", map[string]any{"default_logger_name": "sites"}, []string{"sites", "lidza-access-shop"}},
		{"wildcard mapping", map[string]any{"logger_names": map[string]any{"*.example.com": []any{"wild"}}}, []string{"wild", "lidza-access-shop"}},
		{"unmapped hosts skipped", map[string]any{"skip_unmapped_hosts": true}, []string{"lidza-access-shop"}},
		{"hostname skipped", map[string]any{"skip_hosts": []any{"shop.example.com"}}, []string{"lidza-access-shop"}},
	}
	for _, c := range cases {
		next, _ := accessLogServerLogs(c.logs, map[string]string{"shop.example.com": "shop"})
		got := caddyNameList(next["logger_names"].(map[string]any)["shop.example.com"])
		if !slices.Equal(got, c.want) {
			t.Fatalf("%s: %v, want %v", c.name, got, c.want)
		}
		back, _ := accessLogServerLogs(next, nil)
		if _, still := childMap(back, "logger_names")["shop.example.com"]; still && len(c.want) == 1 {
			t.Fatalf("%s: a hostname mapped only for the access log must lose its entry", c.name)
		}
	}
	// A server without access logging: unmapped hostnames go to a logger no
	// log accepts, so the journal stays without them.
	next, _ := accessLogServerLogs(nil, map[string]string{"shop.example.com": "shop"})
	if next["default_logger_name"] != accessLogUnlogged || !slices.Equal(caddyNameList(next["logger_names"].(map[string]any)["shop.example.com"]), []string{"lidza-access-shop"}) {
		t.Fatalf("logs = %v", next)
	}
	if none, changed := accessLogServerLogs(nil, nil); none != nil || changed {
		t.Fatal("nothing to map must not create a logs object")
	}
}

func TestAccessLogLoggingExcludesTheApplicationLogsFromEveryOtherLog(t *testing.T) {
	defs := map[string]map[string]any{"shop": renderAccessLog("shop", "v2.10.2", nil)}
	logging, changed, err := accessLogLogging(nil, defs, false)
	if err != nil || !changed {
		t.Fatal(err)
	}
	logs := logging["logs"].(map[string]any)
	if got := stringList(logs["default"].(map[string]any)["exclude"]); !slices.Equal(got, []string{"http.log.access.lidza-access-shop"}) {
		t.Fatalf("default exclude = %v", got)
	}
	shop := logs["lidza-access-shop"].(map[string]any)
	writer := shop["writer"].(map[string]any)
	if writer["filename"] != accessLogPath("shop") || writer["mode"] != "0640" || writer["roll_keep_days"] != 14 || writer["roll_interval"] != nil || !slices.Equal(stringList(shop["include"]), []string{"http.log.access.lidza-access-shop"}) {
		t.Fatalf("log = %v", shop)
	}
	if !caddyLogAccepts(stringList(shop["include"]), nil, "http.log.access.lidza-access-shop") || caddyLogAccepts(stringList(shop["include"]), nil, "http.log.access") {
		t.Fatal("the application log must take only its own logger")
	}
	if daily := renderAccessLog("shop", "v2.11.7 h1:x", nil)["writer"].(map[string]any); daily["roll_interval"] != int64(86400000000000) {
		t.Fatalf("Caddy 2.11 rotates daily: %v", daily)
	}
	// A second pass changes nothing; withdrawing restores the original.
	if _, changed, _ := accessLogLogging(logging, defs, false); changed {
		t.Fatal("a second pass must change nothing")
	}
	if back, changed, _ := accessLogLogging(logging, nil, false); !changed || back != nil && len(back) != 0 {
		t.Fatalf("withdrawn logging = %v", back)
	}

	// Existing logs: the journal's default with an operator exclusion, a
	// file log of all access entries, and one that takes none of them.
	current := map[string]any{"logs": map[string]any{
		"default":  map[string]any{"exclude": []any{"http.log.access.lidza_scanners"}, "encoder": map[string]any{"format": "json"}},
		"access":   map[string]any{"include": []any{"http.log.access"}, "writer": map[string]any{"output": "file", "filename": "/var/log/caddy/access.log"}},
		"tls":      map[string]any{"include": []any{"tls"}},
		"scanners": map[string]any{"include": []any{"http.log.access.lidza_scanners"}},
	}}
	defs = map[string]map[string]any{"shop": renderAccessLog("shop", "", accessLogEncoder(current))}
	next, _, err := accessLogLogging(current, defs, true)
	if err != nil {
		t.Fatal(err)
	}
	logs = next["logs"].(map[string]any)
	if got := stringList(logs["default"].(map[string]any)["exclude"]); !slices.Equal(got, []string{"http.log.access.lidza_scanners", "http.log.access.lidza-access-shop", "http.log.access.lidza_unlogged"}) {
		t.Fatalf("default exclude = %v", got)
	}
	if got := stringList(logs["access"].(map[string]any)["exclude"]); !slices.Equal(got, []string{"http.log.access.lidza-access-shop", "http.log.access.lidza_unlogged"}) {
		t.Fatalf("access exclude = %v", got)
	}
	if logs["tls"].(map[string]any)["exclude"] != nil || logs["scanners"].(map[string]any)["exclude"] != nil {
		t.Fatal("logs that never took access entries must stay as they are")
	}
	if logs["lidza-access-shop"].(map[string]any)["encoder"].(map[string]any)["format"] != "json" {
		t.Fatal("the application log must encode entries as the journal does")
	}
	back, _, _ := accessLogLogging(next, nil, false)
	if !jsonEqual(mustJSON(back), mustJSON(current)) {
		t.Fatalf("withdrawn logging = %s", mustJSON(back))
	}
	// A log whose includes cannot take an exclusion is reported, not broken.
	mixed := map[string]any{"logs": map[string]any{"mixed": map[string]any{"include": []any{"http.log.access", "tls"}}}}
	if _, _, err := accessLogLogging(mixed, defs, false); err == nil || !strings.Contains(err.Error(), "mixed") {
		t.Fatalf("err = %v", err)
	}
}

func TestCaddyLogAcceptsMirrorsCaddy(t *testing.T) {
	name := "http.log.access.lidza-access-shop"
	for _, c := range []struct {
		include, exclude []string
		want             bool
	}{
		{nil, nil, true},
		{[]string{"http.log.access"}, nil, true},
		{[]string{"http"}, []string{"http.log.access"}, false},
		{[]string{"http.log.access.lidza-access-sho"}, nil, false},
		{nil, []string{"http.log"}, false},
		{nil, []string{"*"}, false},
		{[]string{"tls"}, nil, false},
	} {
		if got := caddyLogAccepts(c.include, c.exclude, name); got != c.want {
			t.Fatalf("%v/%v = %v", c.include, c.exclude, got)
		}
	}
	if ok, known := caddyVersionAtLeast("v2.8.4 h1:abc", 2, 9); !known || ok {
		t.Fatal("2.8.4 is older than 2.9")
	}
	if ok, known := caddyVersionAtLeast("v2.10.0", 2, 9); !known || !ok {
		t.Fatal("2.10.0 is at least 2.9")
	}
	if _, known := caddyVersionAtLeast("", 2, 9); known {
		t.Fatal("an unknown version is unknown")
	}
}

// accessLogFixture is a created application behind a fake Caddy that logs
// every hostname to the journal.
func accessLogFixture(t *testing.T) (*nativeCreateFixture, *fakeCaddyAdmin) {
	t.Helper()
	f := newNativeCreateFixture(t)
	// /var/log exists on every host; /var/log/lidza is the agent's.
	if err := os.RemoveAll(filepath.Dir(accessLogRoot)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(accessLogRoot)), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(accessLogRoot)) })
	caddy := newFakeCaddyAdmin(t, map[string]any{"apps": map[string]any{"http": map[string]any{"servers": map[string]any{
		"srv0": map[string]any{"listen": []any{":443"}, "routes": []any{}, "logs": map[string]any{"logger_names": map[string]any{"other.example.com": []any{""}}}},
	}}}})
	f.m.cfg.Native.Caddy = &NativeCaddyConfig{Admin: caddy.URL(), Server: "srv0", Upstream: "127.0.0.1:18081"}
	body := `{"id":"shop","repository":"acme/shop","branch":"main","runtime":"go","domain":"shop.example.com","additional_domains":["www.shop.example.com"],"data":{}}`
	if w := f.do(t, "POST", "/v1/native/apps", body); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	f.m.accessLogRoutes(f.mux)
	return f, caddy
}

func accessLogStatusOf(t *testing.T, f *nativeCreateFixture, id string) AppAccessLogStatus {
	t.Helper()
	status, err := f.m.accessLogStatus(id)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func TestAccessLogIsOnByDefaultAndReadableOnlyByTheApplication(t *testing.T) {
	f, caddy := accessLogFixture(t)
	ctx := context.Background()
	f.m.reconcileAccessLogs(ctx)

	names := caddy.at("/config/apps/http/servers/srv0/logs/logger_names").(map[string]any)
	if !slices.Equal(caddyNameList(names["shop.example.com"]), []string{"", "lidza-access-shop"}) || !slices.Equal(caddyNameList(names["other.example.com"]), []string{""}) {
		t.Fatalf("logger names = %v", names)
	}
	if got := caddy.strings("/config/logging/logs/default/exclude"); !slices.Equal(got, []string{"http.log.access.lidza-access-shop"}) {
		t.Fatalf("journal exclusions = %v", got)
	}
	if caddy.at("/config/logging/logs/lidza-access-shop/writer/filename") != accessLogPath("shop") {
		t.Fatal("the application log must write the application's file")
	}
	dir := accessLogDir("shop")
	info, err := os.Stat(dir)
	if err != nil || info.Mode()&(os.ModePerm|os.ModeSetgid) != os.ModeSetgid|0o750 {
		t.Fatalf("directory mode: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Clean(accessLogRoot)); err != nil || info.Mode().Perm() != 0o731 {
		t.Fatalf("root mode: %v %v", info, err)
	}
	env, _ := os.ReadFile(filepath.Join(f.appsRoot, "shop", "shared", ".env"))
	if !strings.Contains(string(env), "LIDZA_ACCESS_LOG="+accessLogPath("shop")+"\n") {
		t.Fatalf("environment lacks the access log:\n%s", env)
	}
	status := accessLogStatusOf(t, f, "shop")
	if !status.Supported || !status.Enabled || !status.Active || !status.EnvSet || status.Error != "" || status.Path != accessLogPath("shop") || status.Account != "app-shop" || !slices.Equal(status.Hosts, []string{"shop.example.com", "www.shop.example.com"}) {
		t.Fatalf("status = %+v", status)
	}
	// The path is no secret: application logs show it.
	if slices.Contains(f.m.nativeCreatedSecrets("shop"), accessLogPath("shop")) {
		t.Fatal("the access log path must not be masked")
	}

	// Files Caddy rotated with another mode are put back to 0640.
	rotated := filepath.Join(dir, "access-2026-11-10T00-00-00.000.log.gz")
	if err := os.WriteFile(rotated, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	caddy.reset()
	f.m.reconcileAccessLogs(ctx)
	if info, _ := os.Stat(rotated); info.Mode().Perm() != 0o640 {
		t.Fatalf("rotated file mode %v", info.Mode())
	}
	if caddy.called("POST") != 0 {
		t.Fatal("a pass with nothing to change must not write Caddy's configuration")
	}

	// A new hostname follows the application.
	f.m.mu.Lock()
	a := f.m.data.NativeApps["shop"]
	a.AdditionalDomains = []string{"shop.example.org"}
	f.m.data.NativeApps["shop"] = a
	f.m.mu.Unlock()
	f.m.reconcileAccessLogs(ctx)
	names = caddy.at("/config/apps/http/servers/srv0/logs/logger_names").(map[string]any)
	if _, still := names["www.shop.example.com"]; !still || !slices.Equal(caddyNameList(names["www.shop.example.com"]), []string{""}) || !slices.Equal(caddyNameList(names["shop.example.org"]), []string{"", "lidza-access-shop"}) {
		t.Fatalf("hostname change: %v", names)
	}
}

func TestAccessLogSwitchedOffStopsWritingAndRemovesTheFiles(t *testing.T) {
	f, caddy := accessLogFixture(t)
	ctx := context.Background()
	f.m.reconcileAccessLogs(ctx)
	if err := os.WriteFile(accessLogPath("shop"), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if w := f.do(t, "PUT", "/v1/apps/shop/access-log", `{"enabled":false}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("switch off: %d %s", w.Code, w.Body)
	}
	names := caddy.at("/config/apps/http/servers/srv0/logs/logger_names").(map[string]any)
	if !slices.Equal(caddyNameList(names["shop.example.com"]), []string{""}) {
		t.Fatalf("journal mapping must stay: %v", names)
	}
	if caddy.at("/config/logging/logs/lidza-access-shop") != nil || caddy.at("/config/logging/logs/default") != nil {
		t.Fatalf("logging = %v", caddy.at("/config/logging"))
	}
	if _, err := os.Stat(accessLogDir("shop")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("switching the log off must remove its files")
	}
	env, _ := os.ReadFile(filepath.Join(f.appsRoot, "shop", "shared", ".env"))
	if strings.Contains(string(env), "LIDZA_ACCESS_LOG") {
		t.Fatalf("environment keeps the access log:\n%s", env)
	}
	if status := accessLogStatusOf(t, f, "shop"); status.Enabled || status.Active || status.EnvSet {
		t.Fatalf("status = %+v", status)
	}
	// The setting survives passes and is carried by a move.
	f.m.reconcileAccessLogs(ctx)
	if _, err := os.Stat(accessLogDir("shop")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a switched-off log must stay off")
	}
	if source, err := f.m.nativeMoveSource("shop"); err != nil || source.CreateSpec == nil || !source.CreateSpec.AccessLogOff {
		t.Fatalf("move source = %+v, %v", source.CreateSpec, err)
	}
	on := true
	if err := f.m.setAccessLog(ctx, "shop", AppAccessLogSettings{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if status := accessLogStatusOf(t, f, "shop"); !status.Active || !status.EnvSet {
		t.Fatalf("status after switching on = %+v", status)
	}
	if w := f.do(t, "GET", "/v1/apps/shop/access-log", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"path":"`+accessLogPath("shop")+`"`) || !strings.Contains(w.Body.String(), `"active":true`) {
		t.Fatalf("status route: %d %s", w.Code, w.Body)
	}
}

func TestAccessLogOnAnOldCaddyReportsAndChangesNothing(t *testing.T) {
	f, caddy := accessLogFixture(t)
	previous := accessLogCaddyVersionProbe
	accessLogCaddyVersionProbe = func(context.Context) string { return "v2.8.4 h1:abc" }
	t.Cleanup(func() { accessLogCaddyVersionProbe = previous })
	caddy.reset()
	f.m.reconcileAccessLogs(context.Background())
	if caddy.called("POST") != 0 || caddy.at("/config/logging") != nil {
		t.Fatal("an old Caddy must not be changed")
	}
	status := accessLogStatusOf(t, f, "shop")
	if status.Active || !strings.Contains(status.Error, "Caddy 2.9 or later") || status.CaddyVersion != "v2.8.4" {
		t.Fatalf("status = %+v", status)
	}
}

func TestAccessLogRefusedByCaddyLeavesRoutingAndReportsIt(t *testing.T) {
	f, caddy := accessLogFixture(t)
	caddy.refuse = map[string]bool{"POST /config/logging": true}
	f.m.reconcileAccessLogs(context.Background())
	if names := caddy.at("/config/apps/http/servers/srv0/logs/logger_names").(map[string]any); names["shop.example.com"] != nil {
		t.Fatal("without the log the hostnames must not be mapped")
	}
	if caddy.at("/config/apps/http/servers/srv0/routes/0/@id") != nativeCaddyRouteID("shop") {
		t.Fatal("the route must stay published")
	}
	if status := accessLogStatusOf(t, f, "shop"); status.Active || !strings.Contains(status.Error, "refused") {
		t.Fatalf("status = %+v", status)
	}
}

func TestAccessLogDeletedApplicationIsWithdrawnAndContainersAreNotCovered(t *testing.T) {
	f, caddy := accessLogFixture(t)
	ctx := context.Background()
	f.m.reconcileAccessLogs(ctx)
	if w := f.do(t, "DELETE", "/v1/native/apps/shop?purge=1", ""); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(accessLogDir("shop")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deletion must remove the access log")
	}
	if caddy.at("/config/logging/logs/lidza-access-shop") != nil || caddy.at("/config/logging/logs/default") != nil {
		t.Fatalf("logging after deletion = %v", caddy.at("/config/logging"))
	}
	names, _ := caddy.at("/config/apps/http/servers/srv0/logs/logger_names").(map[string]any)
	for host, value := range names {
		if slices.ContainsFunc(caddyNameList(value), accessLogOurs) {
			t.Fatalf("%s still maps an access log", host)
		}
	}

	f.m.mu.Lock()
	f.m.data.Apps["web"] = testApp("web")
	f.m.mu.Unlock()
	status, err := f.m.accessLogStatus("web")
	if err != nil || status.Supported || !strings.Contains(status.Reason, "Container applications") {
		t.Fatalf("container status = %+v, %v", status, err)
	}
	on := false
	if err := f.m.setAccessLog(ctx, "web", AppAccessLogSettings{Enabled: &on}); err == nil {
		t.Fatal("a container application has no access log to switch")
	}
}

func TestArchivedLoggerNamesLeaveTheAccessLogOut(t *testing.T) {
	if got := withoutAccessLogNames(json.RawMessage(`["","lidza-access-shop"]`)); string(got) != `[""]` {
		t.Fatalf("got %s", got)
	}
	if got := withoutAccessLogNames(json.RawMessage(`["lidza-access-shop"]`)); got != nil {
		t.Fatalf("an entry only the access log made must not be restored: %s", got)
	}
	if got := withoutAccessLogNames(json.RawMessage(`"default"`)); string(got) != `"default"` {
		t.Fatalf("got %s", got)
	}
}

func TestArchiveDeletionWithdrawsTheAccessLogAndRestoresTheJournalMapping(t *testing.T) {
	f := newLeftoverFixture(t)
	m := f.m
	if err := os.MkdirAll(accessLogDir("my-rails"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(accessLogRoot)) })
	if err := os.WriteFile(accessLogPath("my-rails"), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	f.caddy.mu.Lock()
	srv := f.caddy.config["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["srv0"].(map[string]any)
	srv["logs"] = map[string]any{"logger_names": map[string]any{"rails.example.com": []any{"", "lidza-access-my-rails"}, "other.example.com": []any{""}}}
	f.caddy.config["logging"] = map[string]any{"logs": map[string]any{
		"default":               map[string]any{"exclude": []any{"http.log.access.lidza-access-my-rails"}},
		"lidza-access-my-rails": renderAccessLog("my-rails", "", nil),
	}}
	f.caddy.mu.Unlock()
	view, err := m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(accessLogDir("my-rails")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deletion must remove the access log files")
	}
	if f.caddy.at("/config/logging/logs/lidza-access-my-rails") != nil || f.caddy.at("/config/logging/logs/default") != nil {
		t.Fatalf("logging after deletion = %v", f.caddy.at("/config/logging"))
	}
	archive := m.archiveSnapshot(view.ID)
	if _, ok := archiveItemNamed(archive.Deleted, "access-log", accessLogDir("my-rails")); !ok {
		t.Fatalf("deleted inventory %+v", archive.Deleted)
	}
	if len(archive.Native.Leftovers.LoggerNames) != 1 || string(archive.Native.Leftovers.LoggerNames[0].Value) != `[""]` {
		t.Fatalf("archived logger names %+v", archive.Native.Leftovers.LoggerNames)
	}
	if _, err := m.RestoreArchive(context.Background(), view.ID, ArchiveRestoreRequest{}); err != nil {
		t.Fatal(err)
	}
	names := f.caddy.at("/config/apps/http/servers/srv0/logs/logger_names").(map[string]any)
	if !slices.Equal(caddyNameList(names["rails.example.com"]), []string{""}) {
		t.Fatalf("restored mapping %v", names["rails.example.com"])
	}
}
