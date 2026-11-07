package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// leftoverFixture adds to the adopted fixture what a host takeover leaves:
// user units bound below the directory, system units named for the
// application, error-page routes and configured hostname reservations.
type leftoverFixture struct {
	*adoptedArchiveFixture
	app, userUnits, systemUnits string
	caddy                       *fakeCaddyAdmin
}

func newLeftoverFixture(t *testing.T) *leftoverFixture {
	t.Helper()
	f := &leftoverFixture{adoptedArchiveFixture: newAdoptedArchiveFixture(t)}
	f.app = filepath.Join(f.home, "MyRails")
	f.userUnits = filepath.Join(f.home, ".config", "systemd", "user")
	f.systemUnits = t.TempDir()
	previous := nativeSystemUnitDir
	nativeSystemUnitDir = f.systemUnits
	t.Cleanup(func() { nativeSystemUnitDir = previous })
	m := f.m
	inner := m.nativeCommand
	m.nativeCommand = func(ctx context.Context, args []string) (string, error) {
		if len(args) > 2 && args[0] == "systemctl" && args[1] == "show" {
			f.mu.Lock()
			f.calls = append(f.calls, slices.Clone(args))
			f.mu.Unlock()
			var blocks []string
			for _, unit := range args[4:] {
				blocks = append(blocks, "Id="+unit+"\nActiveState=active\nSubState=listening\nUnitFileState=enabled")
			}
			return strings.Join(blocks, "\n\n"), nil
		}
		return inner(ctx, args)
	}
	errorRoute := func(id string, hosts ...string) map[string]any {
		route := map[string]any{"match": []any{map[string]any{"host": hosts}}, "handle": []any{map[string]any{"handler": "subroute", "routes": []any{
			map[string]any{"handle": []any{map[string]any{"handler": "vars", "root": "/etc/caddy/pages"}}},
			map[string]any{"handle": []any{map[string]any{"handler": "file_server"}}},
		}}}}
		if id != "" {
			route["@id"] = id
		}
		return route
	}
	f.caddy = newFakeCaddyAdmin(t, map[string]any{"apps": map[string]any{"http": map[string]any{"servers": map[string]any{
		"srv0": map[string]any{"listen": []any{":443"}, "routes": []any{}, "errors": map[string]any{"routes": []any{
			errorRoute("", "rails.example.com"),
			errorRoute("", "other.example.com", "rails.example.com"),
			errorRoute("lidza-errors-other", "rails.example.com", "other.example.com"),
		}}},
	}}}})
	m.cfg.Native.Caddy = &NativeCaddyConfig{Admin: f.caddy.URL(), Server: "srv0"}
	m.cfg.ReservedDomains = []string{"rails.example.com", "kept.example.com"}
	return f
}

func (f *leftoverFixture) write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeLeftovers writes the units a takeover left, and returns the ones
// that are the application's and the ones that must stay.
func (f *leftoverFixture) writeLeftovers(t *testing.T, dir string) (own, others []string) {
	t.Helper()
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	user := map[string]string{
		// Bound below the directory, not to <dir>/current: not in the
		// registration, but the application's.
		"myrails-old-server.service": "[Service]\nWorkingDirectory=" + filepath.Join(f.app, "releases", "one") + "\n",
		"legacy-mail.service":        "[Service]\nWorkingDirectory=/srv/elsewhere\nEnvironmentFile=-" + filepath.Join(f.app, ".env") + "\n",
		"myrails-server.socket":      "[Socket]\nListenStream=127.0.0.1:9180\n",
	}
	userOthers := map[string]string{
		"other-server.socket":   "[Socket]\nListenStream=127.0.0.1:9300\n", // triggers the live application's unit
		"myrails2-web.service":  "[Service]\nWorkingDirectory=" + f.app + "2/current\n",
		"escape-web.service":    "[Service]\nWorkingDirectory=" + filepath.Join(f.app, "escape") + "\n",
		"myrails-other.socket":  "[Socket]\nListenStream=127.0.0.1:9180\nService=other-server.service\n",
		"lonely-worker.service": "[Service]\nExecStart=/usr/bin/true\n",
	}
	system := map[string]string{
		"myrails-server.socket": "[Socket]\nListenStream=127.0.0.1:9180\n",                                // by port
		"my-rails-web.socket":   "[Socket]\nListenStream=/run/web.sock\nService=myrails-server.service\n", // triggers a proven unit
		"myrails-cache.service": "[Service]\nWorkingDirectory=" + filepath.Join(f.app, "current") + "\n",  // names the directory
		"myrails-cache.timer":   "[Timer]\nOnCalendar=hourly\n",                                           // triggers the unit above
	}
	systemOthers := map[string]string{
		"myrails2-server.socket":              "[Socket]\nListenStream=127.0.0.1:9180\n",
		"app-rails-example-com-server.socket": "[Socket]\nListenStream=127.0.0.1:9180\n",
		"myrails-other-server.socket":         "[Socket]\nListenStream=127.0.0.1:9180\n",
		"myrails-misc.socket":                 "[Socket]\nListenStream=127.0.0.1:9999\n",
		"other-server.service":                "[Service]\nWorkingDirectory=" + filepath.Join(f.app, "current") + "\n",
	}
	for name, content := range user {
		f.write(t, filepath.Join(f.userUnits, name), content)
		own = append(own, filepath.Join(f.userUnits, name))
	}
	for name, content := range userOthers {
		f.write(t, filepath.Join(f.userUnits, name), content)
		others = append(others, filepath.Join(f.userUnits, name))
	}
	for name, content := range system {
		f.write(t, filepath.Join(f.systemUnits, name), content)
		own = append(own, filepath.Join(f.systemUnits, name))
	}
	for name, content := range systemOthers {
		f.write(t, filepath.Join(f.systemUnits, name), content)
		others = append(others, filepath.Join(f.systemUnits, name))
	}
	return own, others
}

func (f *leftoverFixture) errorRoutes() []any {
	routes, _ := f.caddy.at("/config/apps/http/servers/srv0/errors/routes").([]any)
	return routes
}

func errorRouteHosts(route any) []string {
	var hosts []string
	for _, set := range route.(map[string]any)["match"].([]any) {
		for _, h := range set.(map[string]any)["host"].([]any) {
			hosts = append(hosts, h.(string))
		}
	}
	return hosts
}

func assertGone(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s was not removed", path)
		}
	}
}

func assertKept(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s was touched: %v", path, err)
		}
	}
}

func TestArchiveLeftoversDeleteRemovesBoundUnitsErrorRoutesAndReservationsAndRestoreRecreatesThem(t *testing.T) {
	f := newLeftoverFixture(t)
	m := f.m
	own, others := f.writeLeftovers(t, f.app)
	contents := map[string]string{}
	for _, path := range own {
		data, _ := os.ReadFile(path)
		contents[path] = string(data)
	}
	if !m.hostReserved("rails.example.com") {
		t.Fatal("fixture: the hostname is not reserved")
	}

	view, err := m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{DeletedBy: "op"})
	if err != nil {
		t.Fatal(err)
	}
	assertGone(t, own)
	assertKept(t, others)
	for _, unit := range []string{"myrails-server.socket", "my-rails-web.socket", "myrails-cache.service", "myrails-cache.timer"} {
		if !f.ran("systemctl", "disable", "--now", "--quiet", unit) {
			t.Fatalf("system %s not stopped", unit)
		}
	}
	if !f.ran("systemctl", "daemon-reload") || !f.ran("systemctl", "--user", "disable", "--now", "--quiet", "myrails-old-server.service") {
		t.Fatalf("calls %v", f.calls)
	}
	for _, other := range []string{"myrails2-server.socket", "app-rails-example-com-server.socket", "myrails-other-server.socket", "other-server.socket"} {
		f.mu.Lock()
		touched := slices.ContainsFunc(f.calls, func(args []string) bool { return slices.Contains(args, other) && slices.Contains(args, "disable") })
		f.mu.Unlock()
		if touched {
			t.Fatalf("%s was stopped", other)
		}
	}
	// Error routes: the exclusive one is gone, the shared one lost only the
	// application's hostname, another application's route is untouched.
	routes := f.errorRoutes()
	if len(routes) != 2 || !slices.Equal(errorRouteHosts(routes[0]), []string{"other.example.com"}) ||
		!slices.Equal(errorRouteHosts(routes[1]), []string{"rails.example.com", "other.example.com"}) {
		t.Fatalf("error routes %v", routes)
	}
	// Reservations: released in the state, the configuration unchanged.
	if m.hostReserved("rails.example.com") || !m.hostReserved("kept.example.com") || !slices.Contains(m.cfg.ReservedDomains, "rails.example.com") {
		t.Fatal("reservation not released")
	}
	archive := m.archiveSnapshot(view.ID)
	if archive.Native.Leftovers == nil || len(archive.Native.Leftovers.Units) != len(own) || len(archive.Native.Leftovers.ErrorRoutes) != 2 || !slices.Equal(archive.ReleasedReservations, []string{"rails.example.com"}) {
		t.Fatalf("archive leftovers %+v reservations %v", archive.Native.Leftovers, archive.ReleasedReservations)
	}
	if _, ok := itemNamed(archive.Deleted, "unit", "system myrails-server.socket"); !ok {
		t.Fatalf("deleted inventory %+v", archive.Deleted)
	}
	// A redirect onto the released hostname is accepted.
	if _, err := m.SetRedirects(context.Background(), "other", []RedirectDomain{{Host: "rails.example.com"}}); err != nil && strings.Contains(err.Error(), "reserved") {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(m.Redirects("other"), func(r RedirectDomain) bool { return r.Host == "rails.example.com" }) {
		t.Fatalf("redirect not stored: %+v", m.Redirects("other"))
	}
	if _, err := m.SetRedirects(context.Background(), "other", nil); err != nil && strings.Contains(err.Error(), "reserved") {
		t.Fatal(err)
	}
	// Configured policies of the archived id are not offered for adoption.
	m.cfg.Native.Releases = map[string]NativeReleasePolicy{"my-rails": {Enabled: true, Root: f.app, Units: []string{"myrails-cache.service"}}}
	f.write(t, filepath.Join(f.systemUnits, "myrails-cache.service"), "[Service]\n")
	discovered, err := m.nativeDiscovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(discovered, func(a NativeApplication) bool { return a.Name == "MyRails" }) {
		t.Fatalf("archived application offered again: %+v", discovered)
	}
	os.Remove(filepath.Join(f.systemUnits, "myrails-cache.service"))
	m.cfg.Native.Releases = nil

	// Restore recreates the files, their states, the routes and the reservation.
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if _, err := m.RestoreArchive(context.Background(), view.ID, ArchiveRestoreRequest{}); err != nil {
		t.Fatal(err)
	}
	for path, content := range contents {
		if data, err := os.ReadFile(path); err != nil || string(data) != content {
			t.Fatalf("%s not restored: %v", path, err)
		}
	}
	if !f.ran("systemctl", "enable", "--now", "--no-block", "--quiet", "myrails-server.socket") {
		t.Fatalf("socket not restarted: %v", f.calls)
	}
	routes = f.errorRoutes()
	var withOwn int
	for _, r := range routes {
		if slices.Contains(errorRouteHosts(r), "rails.example.com") {
			withOwn++
		}
	}
	// The exclusive route comes back at its place; the copy of the shared
	// route's hostname is identical to it here, so it is not added twice.
	if len(routes) != 3 || withOwn != 2 || !slices.Equal(errorRouteHosts(routes[0]), []string{"rails.example.com"}) {
		t.Fatalf("error routes after restore %v", routes)
	}
	if !m.hostReserved("rails.example.com") {
		t.Fatal("restore did not reserve the hostname again")
	}
}

func TestArchiveLeftoversSweepFinishesAnEarlierDeletionIdempotently(t *testing.T) {
	f := newLeftoverFixture(t)
	m := f.m
	// The deletion ran on an agent without the sweep: simulate by deleting
	// first and putting the leftovers back afterwards.
	saved := m.cfg.Native.Caddy
	m.cfg.Native.Caddy = nil
	reserved := m.cfg.ReservedDomains
	m.cfg.ReservedDomains = nil
	view, err := m.DeleteApp(context.Background(), "my-rails", AppArchiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.Native.Caddy, m.cfg.ReservedDomains = saved, reserved
	// The directory is in the archive; the escaping link moved with it.
	own, others := f.writeLeftovers(t, m.archiveSnapshot(view.ID).Native.Files)
	m.sweepArchiveLeftovers(context.Background())
	assertGone(t, own)
	assertKept(t, others)
	if len(f.errorRoutes()) != 2 || m.hostReserved("rails.example.com") {
		t.Fatalf("sweep left routes %v reserved=%v", f.errorRoutes(), m.hostReserved("rails.example.com"))
	}
	before := m.archiveSnapshot(view.ID)
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	f.caddy.reset()
	m.sweepArchiveLeftovers(context.Background())
	after := m.archiveSnapshot(view.ID)
	beforeJSON, _ := json.Marshal(before.Native.Leftovers)
	afterJSON, _ := json.Marshal(after.Native.Leftovers)
	if string(beforeJSON) != string(afterJSON) || len(f.callsOf("systemctl")) != 0 || f.caddy.called("DELETE") != 0 || f.caddy.called("PATCH") != 0 {
		t.Fatalf("second sweep changed something: %v", f.calls)
	}
	// A directory that is back (recreated by hand) is left alone.
	if err := os.MkdirAll(f.app, 0o700); err != nil {
		t.Fatal(err)
	}
	f.write(t, filepath.Join(f.systemUnits, "myrails-server.socket"), "[Socket]\nListenStream=127.0.0.1:9180\n")
	m.sweepArchiveLeftovers(context.Background())
	assertKept(t, []string{filepath.Join(f.systemUnits, "myrails-server.socket")})
}

func TestSplitErrorRouteExclusiveAndShared(t *testing.T) {
	own := []string{"rails.example.com"}
	raw := func(v any) json.RawMessage { return mustJSON(v) }
	hosts, exclusive, _, restore := splitErrorRoute(raw(map[string]any{"match": []any{map[string]any{"host": []any{"Rails.example.com"}}}}), "my-rails", own)
	if !exclusive || len(hosts) != 1 || restore == nil {
		t.Fatal("exclusive route not recognised")
	}
	hosts, exclusive, kept, restore := splitErrorRoute(raw(map[string]any{"@id": "ops", "match": []any{
		map[string]any{"host": []any{"rails.example.com", "a.example.com"}},
		map[string]any{"host": []any{"rails.example.com"}, "path": []any{"/x"}},
	}}), "my-rails", own)
	if exclusive || len(hosts) != 1 || len(kept) != 1 || !jsonEqual(raw(kept), raw([]any{map[string]any{"host": []any{"a.example.com"}}})) {
		t.Fatalf("shared: exclusive=%v kept=%v", exclusive, kept)
	}
	if strings.Contains(string(restore), `"@id"`) || !strings.Contains(string(restore), `"/x"`) {
		t.Fatalf("restore copy %s", restore)
	}
	// A matcher set without hosts matches every host: never exclusive.
	if _, exclusive, _, _ := splitErrorRoute(raw(map[string]any{"match": []any{map[string]any{"host": []any{"rails.example.com"}}, map[string]any{"path": []any{"/"}}}}), "my-rails", own); exclusive {
		t.Fatal("a host-less matcher set made the route exclusive")
	}
	if hosts, _, _, _ := splitErrorRoute(raw(map[string]any{"@id": "lidza-errors-other", "match": []any{map[string]any{"host": []any{"rails.example.com"}}}}), "my-rails", own); hosts != nil {
		t.Fatal("another application's managed route was claimed")
	}
}

func TestPathWithinNeedsASeparatorBoundaryAndStaysInside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		root: true, root + "/current": true, root + "2/current": false, root + "/../x": false,
		root + "/escape": false, root + "/missing/dir": true, "relative/path": false,
	} {
		if got := pathWithin(path, root, ""); got != want {
			t.Fatalf("pathWithin(%s) = %v", path, got)
		}
	}
	// Moved into the archive: resolved in the archived copy.
	files := filepath.Join(t.TempDir(), "root")
	if err := os.Rename(root, files); err != nil {
		t.Fatal(err)
	}
	if pathWithin(root+"/escape", root, files) || !pathWithin(root+"/current", root, files) {
		t.Fatal("archived copy not used")
	}
}
