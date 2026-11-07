package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Leftovers of a deleted native application: host configuration that
// belongs to it but that the deletion's own inventory did not name.
//
//   - user units of the deployment account whose WorkingDirectory,
//     ExecStart executable or EnvironmentFile is the application's directory
//     or a path under it, and user sockets and timers that trigger one of
//     its proven services (same base name, or <app>-<role>);
//   - system units named exactly <app>-<role>.(service|socket|timer), where
//     <app> is the application's id or directory name and <role> has no
//     hyphen, that name the application's directory, or are sockets that
//     listen on its recorded port, or sockets and timers that trigger one
//     of its proven units (of either scope);
//   - the hostnames of its error-page routes (the errors block of every
//     web server): a route that matches only its hostnames is removed, a
//     route shared with other hostnames loses only these;
//   - its hostnames in the access-log logger_names map of every web server;
//   - the failed state systemd keeps for a unit whose file was removed
//     ("not-found failed"), cleared with reset-failed;
//   - for an adopted application deleted by an earlier agent (sweep only):
//     its PostgreSQL databases, recorded and disabled as a deletion does
//     now (decideAdoptedDatabases), read from the kept directory's
//     environment; and the backup lock files of its databases.
//
// A unit or hostname that any live application claims is never touched.
// Everything removed is kept in the archive (unit contents, owners, modes
// and states; the error routes) and a restore recreates it. The deletion
// runs this once; archiveLeftoversLoop re-applies it at start and hourly to
// every archived native application, so a deletion made by an earlier agent
// is finished after an update.

// ArchivedLeftovers is what the leftover sweep removed.
type ArchivedLeftovers struct {
	Units       []ArchivedLeftoverUnit `json:"units,omitempty"`
	ErrorRoutes []ArchivedErrorRoute   `json:"error_routes,omitempty"`
	LoggerNames []ArchivedLoggerName   `json:"logger_names,omitempty"`
	// ResetFailed is set once the failed state of every removed unit was
	// cleared, so no "not-found failed" unit stays listed.
	ResetFailed bool `json:"reset_failed,omitempty"`
}

// ArchivedLoggerName is a hostname's entry in a web server's access-log
// logger_names map (logs.logger_names), removed with the application.
type ArchivedLoggerName struct {
	Server string          `json:"server"`
	Host   string          `json:"host"`
	Value  json.RawMessage `json:"value"`
}

// ArchivedLeftoverUnit is a removed unit file and the service manager it
// belonged to.
type ArchivedLeftoverUnit struct {
	ArchivedUnit
	Scope  string `json:"scope"` // user or system
	Reason string `json:"reason"`
}

// ArchivedErrorRoute is an error-page route that served the application's
// hostnames. Route is what a restore inserts at Index: the removed route
// when Exclusive, otherwise a copy of the shared route that matches only
// the removed Hosts.
type ArchivedErrorRoute struct {
	Server    string          `json:"server"`
	Index     int             `json:"index"`
	Exclusive bool            `json:"exclusive"`
	Hosts     []string        `json:"hosts"`
	Route     json.RawMessage `json:"route"`
}

var (
	leftoverUnitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{0,149}\.(service|socket|timer)$`)
	// The sweep runs this long after the agent starts, then every interval.
	archiveLeftoversStart    = 30 * time.Second
	archiveLeftoversInterval = time.Hour
)

// leftoverTarget is what identifies a deleted application on the host.
type leftoverTarget struct {
	appID string
	names []string // unit name prefixes: the id and the directory name
	root  string
	files string // where the directory is kept in the archive
	port  string
	hosts []string
	// proven are the units the deletion's inventory proved, by scope.
	proven map[string]map[string]bool
}

func (m *Manager) leftoverTargetFor(a AppArchive) (leftoverTarget, bool) {
	if a.Native == nil {
		return leftoverTarget{}, false
	}
	n := a.Native
	t := leftoverTarget{appID: a.AppID, files: n.Files, port: n.App.Port, proven: map[string]map[string]bool{"user": {}, "system": {}}}
	switch {
	case a.Kind == archiveKindAdopted && n.Adopted != nil:
		t.root = n.Adopted.Root
		if n.Adopted.Policy != nil && n.Adopted.Policy.Port != "" {
			t.port = n.Adopted.Policy.Port
		}
		scope := n.Adopted.Scope
		if scope != "system" {
			scope = "user"
		}
		for _, u := range n.Adopted.Units {
			if !u.Kept {
				t.proven[scope][u.Name] = true
			}
		}
	case a.Kind == archiveKindNative:
		t.root = n.Policy.Root
		t.port = cmpOr(n.Policy.Port, t.port)
		for _, unit := range n.Policy.Units {
			t.proven["system"][unit] = true
		}
	default:
		return leftoverTarget{}, false
	}
	if t.root == "" || !filepath.IsAbs(t.root) || filepath.Clean(t.root) == "/" {
		return leftoverTarget{}, false
	}
	t.root = filepath.Clean(t.root)
	for _, name := range []string{a.AppID, n.App.Name, strings.ToLower(n.App.Name)} {
		if name != "" && nativeNamePattern.MatchString(name) && !slices.Contains(t.names, name) {
			t.names = append(t.names, name)
		}
	}
	for _, list := range [][]string{a.Hostnames, {n.App.Domain}, n.App.AdditionalDomains} {
		for _, host := range list {
			if host != "" && !slices.ContainsFunc(t.hosts, func(h string) bool { return strings.EqualFold(h, host) }) {
				t.hosts = append(t.hosts, strings.ToLower(host))
			}
		}
	}
	return t, true
}

// namedUnit reports a unit named <app>-<role>.(service|socket|timer).
func (t leftoverTarget) namedUnit(unit string) bool {
	for _, name := range t.names {
		if regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `-[A-Za-z0-9_]+\.(service|socket|timer)$`).MatchString(unit) {
			return true
		}
	}
	return false
}

// pathWithin reports a path that is root or under it, with a separator
// boundary. When the path exists, its resolved form must stay under the
// resolved root, so a symlink inside the directory that leads elsewhere
// does not count. Once the directory moved into the archive (files), the
// same path is resolved there.
func pathWithin(path, root, files string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	path, root = filepath.Clean(path), filepath.Clean(root)
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return false
	}
	inside := func(p, dir string) bool {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return true // gone, or a link into the moved directory
		}
		realDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return false
		}
		return resolved == realDir || strings.HasPrefix(resolved, realDir+string(filepath.Separator))
	}
	if _, err := os.Lstat(root); err == nil {
		return inside(path, root)
	}
	if files == "" {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return inside(filepath.Join(files, rel), files)
}

// unitDirectives parses a unit file into its directives (all sections).
func unitDirectives(content string) map[string][]string {
	out := map[string][]string{}
	for _, line := range strings.Split(strings.ReplaceAll(content, "\\\n", " "), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.ContainsAny(line[:1], "#;[") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		out[key] = append(out[key], strings.TrimSpace(value))
	}
	return out
}

func unitPathToken(value string) string {
	return strings.Trim(strings.TrimLeft(strings.TrimSpace(value), "-@:+!"), `"'`)
}

// unitBoundTo reports a unit whose WorkingDirectory, ExecStart executable
// or EnvironmentFile is root or under it.
func unitBoundTo(d map[string][]string, root, files string) bool {
	var paths []string
	for _, v := range d["WorkingDirectory"] {
		paths = append(paths, unitPathToken(v))
	}
	for _, v := range d["EnvironmentFile"] {
		paths = append(paths, unitPathToken(v))
	}
	for _, v := range d["ExecStart"] {
		if fields := strings.Fields(strings.TrimLeft(v, "-@:+!")); len(fields) > 0 {
			paths = append(paths, unitPathToken(fields[0]))
		}
	}
	return slices.ContainsFunc(paths, func(p string) bool { return pathWithin(p, root, files) })
}

// unitNamesDir reports a unit that names root (or a path under it) in any
// directive value.
func unitNamesDir(d map[string][]string, root, files string) bool {
	for _, values := range d {
		for _, v := range values {
			for _, field := range strings.Fields(v) {
				candidates := []string{unitPathToken(field)}
				if _, after, ok := strings.Cut(field, "="); ok {
					candidates = append(candidates, unitPathToken(after))
				}
				for _, c := range candidates {
					if pathWithin(strings.TrimRight(c, ";&|"), root, files) {
						return true
					}
				}
			}
		}
	}
	return false
}

// unitListensOn reports a socket unit listening on port.
func unitListensOn(d map[string][]string, port string) bool {
	if port == "" {
		return false
	}
	for _, key := range []string{"ListenStream", "ListenDatagram", "ListenSequentialPacket"} {
		for _, v := range d[key] {
			v = strings.TrimSpace(v)
			if i := strings.LastIndex(v, ":"); i >= 0 {
				v = v[i+1:]
			}
			if _, err := strconv.Atoi(v); err == nil && v == port {
				return true
			}
		}
	}
	return false
}

// unitTriggers returns the unit a socket or timer activates.
func unitTriggers(unit string, d map[string][]string) string {
	base, kind := strings.TrimSuffix(unit, filepath.Ext(unit)), filepath.Ext(unit)
	key := map[string]string{".socket": "Service", ".timer": "Unit"}[kind]
	if key == "" {
		return ""
	}
	if values := d[key]; len(values) > 0 {
		return strings.TrimSpace(values[len(values)-1])
	}
	return base + ".service"
}

// claimedUnits are the units of every live native application but except
// (the application being deleted is still registered during its deletion).
func (m *Manager) claimedUnits(except string) map[string]bool {
	out := map[string]bool{}
	m.mu.Lock()
	for id, a := range m.data.NativeApps {
		if id == except {
			continue
		}
		for _, u := range a.Services {
			out[u] = true
		}
	}
	for id, p := range m.data.NativeCreated {
		if id == except {
			continue
		}
		for _, u := range slices.Concat(p.Units, p.Preserve) {
			out[u] = true
		}
	}
	m.mu.Unlock()
	if m.cfg.Native != nil {
		for id, p := range m.cfg.Native.Releases {
			if id == except {
				continue
			}
			for _, u := range slices.Concat(p.Units, p.Preserve) {
				out[u] = true
			}
		}
	}
	return out
}

type leftoverUnit struct {
	ArchivedLeftoverUnit
	directives map[string][]string
}

// findLeftoverUnits returns the target's leftover unit files in one scope.
func (m *Manager) findLeftoverUnits(t leftoverTarget, scope string) ([]leftoverUnit, error) {
	if scope == "user" && m.cfg.Native == nil {
		return nil, nil
	}
	dir := m.adoptedUnitDir(scope)
	if real, err := filepath.EvalSymlinks(dir); err != nil || real != filepath.Clean(dir) {
		return nil, nil // missing, or a symlink: nothing of ours
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	claimed := m.claimedUnits(t.appID)
	var files []leftoverUnit
	for _, e := range entries {
		name := e.Name()
		if !leftoverUnitPattern.MatchString(name) || claimed[name] {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		u := leftoverUnit{ArchivedLeftoverUnit: ArchivedLeftoverUnit{Scope: scope, ArchivedUnit: ArchivedUnit{Name: name, Path: path, Content: string(content), Mode: uint32(info.Mode().Perm())}}, directives: unitDirectives(string(content))}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			u.UID, u.GID = int(st.Uid), int(st.Gid)
		}
		if trigger := unitTriggers(name, u.directives); trigger != "" && claimed[trigger] {
			continue
		}
		files = append(files, u)
	}
	proven := maps.Clone(t.proven[scope])
	if proven == nil {
		proven = map[string]bool{}
	}
	if scope == "system" {
		// A system socket named for the application may front the service
		// its user manager ran (the name is the proof's other half).
		maps.Copy(proven, t.proven["user"])
	}
	found := map[string]bool{}
	for i, u := range files {
		kind := filepath.Ext(u.Name)
		switch {
		case scope == "user" && kind == ".service" && unitBoundTo(u.directives, t.root, t.files):
			files[i].Reason = "bound to " + t.root
		case scope == "system" && t.namedUnit(u.Name) && unitNamesDir(u.directives, t.root, t.files):
			files[i].Reason = "names " + t.root
		case scope == "system" && t.namedUnit(u.Name) && kind == ".socket" && unitListensOn(u.directives, t.port):
			files[i].Reason = "listens on the application's port " + t.port
		default:
			continue
		}
		found[u.Name] = true
		proven[u.Name] = true
	}
	for changed := true; changed; {
		changed = false
		for i, u := range files {
			kind := filepath.Ext(u.Name)
			if found[u.Name] || kind == ".service" {
				continue
			}
			trigger := unitTriggers(u.Name, u.directives)
			if !proven[trigger] {
				continue
			}
			named := t.namedUnit(u.Name) || scope == "user" && strings.TrimSuffix(u.Name, kind) == strings.TrimSuffix(trigger, filepath.Ext(trigger))
			if !named {
				continue
			}
			files[i].Reason = "triggers " + trigger
			found[u.Name], proven[u.Name], changed = true, true, true
		}
	}
	out := slices.DeleteFunc(files, func(u leftoverUnit) bool { return !found[u.Name] })
	slices.SortFunc(out, func(a, b leftoverUnit) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (m *Manager) unitCtlOutput(ctx context.Context, scope string, args ...string) (string, error) {
	if scope == "system" {
		return m.nativeRootExec(ctx, append([]string{"systemctl"}, args...)...)
	}
	return m.nativeExec(ctx, append([]string{"systemctl", "--user"}, args...)...)
}

// leftoverUnitStates reads the enabled and active state of units.
func (m *Manager) leftoverUnitStates(ctx context.Context, scope string, units []string) (map[string]NativeService, error) {
	out, err := m.unitCtlOutput(ctx, scope, append([]string{"show", "--property=Id,ActiveState,SubState,UnitFileState", "--no-pager"}, units...)...)
	if err != nil {
		return nil, fmt.Errorf("the state of %s could not be read: %w", strings.Join(units, ", "), err)
	}
	states := map[string]NativeService{}
	for _, block := range strings.Split(out, "\n\n") {
		s := NativeService{}
		for _, line := range strings.Split(block, "\n") {
			key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
			switch key {
			case "Id":
				s.Unit = value
			case "ActiveState":
				s.Active = value
			case "UnitFileState":
				s.UnitFileState = value
			}
		}
		if slices.Contains(units, s.Unit) {
			states[s.Unit] = s
		}
	}
	return states, nil
}

// removeLeftoverUnits stops, disables and removes leftover units, keeping
// each in the archive before its file goes.
func (m *Manager) removeLeftoverUnits(ctx context.Context, archiveID, scope string, units []leftoverUnit) error {
	if len(units) == 0 {
		return nil
	}
	names := make([]string, 0, len(units))
	for _, u := range units {
		names = append(names, u.Name)
	}
	states, err := m.leftoverUnitStates(ctx, scope, names)
	if err != nil {
		return err
	}
	for _, u := range units {
		record := u.ArchivedLeftoverUnit
		record.Enabled, record.Active = states[u.Name].enabled(), states[u.Name].running()
		if err := m.updateArchive(archiveID, func(x *AppArchive) {
			if x.Native.Leftovers == nil {
				x.Native.Leftovers = &ArchivedLeftovers{}
			}
			if !slices.ContainsFunc(x.Native.Leftovers.Units, func(r ArchivedLeftoverUnit) bool { return r.Scope == scope && r.Name == u.Name }) {
				x.Native.Leftovers.Units = append(x.Native.Leftovers.Units, record)
			}
		}); err != nil {
			return err
		}
		if err := m.adoptedUnitCtl(ctx, scope, "disable", "--now", "--quiet", u.Name); err != nil {
			return fmt.Errorf("could not stop %s: %w", u.Name, err)
		}
		if err := os.Remove(u.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("could not remove the unit file of %s", u.Name)
		}
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "unit", Name: scope + " " + u.Name, Detail: "stopped, disabled and removed: " + u.Reason + " (kept in the archive for a restore)"})
	}
	if err := m.adoptedUnitCtl(ctx, scope, "daemon-reload"); err != nil {
		return errors.New("systemd could not reload after removing the application's units")
	}
	m.resetFailedUnits(ctx, scope, names)
	return nil
}

// resetFailedUnits clears the failed state of units whose files were
// removed, so systemd stops listing them. A unit that is not loaded has
// nothing to clear and makes systemctl fail; that is ignored (the command's
// output is not available to tell it from other failures, and a failed
// state left over is cosmetic).
func (m *Manager) resetFailedUnits(ctx context.Context, scope string, units []string) {
	if scope == "user" && m.cfg.Native == nil {
		return
	}
	for _, unit := range units {
		if leftoverUnitPattern.MatchString(unit) {
			_ = m.adoptedUnitCtl(ctx, scope, "reset-failed", unit)
		}
	}
}

// resetRemovedUnits clears, once per archive, the failed state of every
// unit file its deletion or sweeps removed (an earlier agent removed them
// without it).
func (m *Manager) resetRemovedUnits(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	if a.Native == nil || a.Native.Leftovers != nil && a.Native.Leftovers.ResetFailed {
		return nil
	}
	byScope := map[string][]string{}
	if ad := a.Native.Adopted; ad != nil {
		for _, u := range ad.Units {
			if !u.Kept {
				byScope[cmpOr(ad.Scope, "user")] = append(byScope[cmpOr(ad.Scope, "user")], u.Name)
			}
		}
	}
	if a.Native.Leftovers != nil {
		for _, u := range a.Native.Leftovers.Units {
			byScope[u.Scope] = append(byScope[u.Scope], u.Name)
		}
	}
	if a.Kind == archiveKindNative {
		byScope["system"] = append(byScope["system"], a.Native.Policy.Units...)
	}
	for _, scope := range []string{"user", "system"} {
		m.resetFailedUnits(ctx, scope, byScope[scope])
	}
	return m.updateArchive(archiveID, func(x *AppArchive) {
		if x.Native.Leftovers == nil {
			x.Native.Leftovers = &ArchivedLeftovers{}
		}
		x.Native.Leftovers.ResetFailed = true
	})
}

// ---- error routes ---------------------------------------------------------------------

func hostIn(hosts []string, host string) bool {
	return slices.ContainsFunc(hosts, func(h string) bool { return strings.EqualFold(h, host) })
}

// splitErrorRoute decides what an error route is to the application. It
// returns the hosts of own it matches; exclusive when every matcher set is
// host-bound to own hosts only. For a shared route, kept is the matcher
// list without own hosts and restore the route matching only them.
func splitErrorRoute(raw json.RawMessage, appID string, own []string) (hosts []string, exclusive bool, kept []any, restore json.RawMessage) {
	var route map[string]any
	if json.Unmarshal(raw, &route) != nil {
		return nil, false, nil, nil
	}
	id, _ := route["@id"].(string)
	if id == nativeCaddyErrorRouteID(appID) {
		return own, true, nil, raw
	}
	if strings.HasPrefix(id, "lidza-") {
		return nil, false, nil, nil // another application's or the agent's own
	}
	sets, _ := route["match"].([]any)
	exclusive = len(sets) > 0
	var ownSets []any
	for _, rawSet := range sets {
		set, ok := rawSet.(map[string]any)
		if !ok {
			exclusive = false
			kept = append(kept, rawSet)
			continue
		}
		values, hasHost := set["host"].([]any)
		if !hasHost {
			exclusive = false
			kept = append(kept, set)
			continue
		}
		var mine, others []any
		for _, v := range values {
			if host, ok := v.(string); ok && hostIn(own, host) {
				mine = append(mine, host)
				if !hostIn(hosts, host) {
					hosts = append(hosts, host)
				}
			} else {
				others = append(others, v)
			}
		}
		if len(others) > 0 {
			exclusive = false
			filtered := maps.Clone(set)
			filtered["host"] = others
			kept = append(kept, filtered)
		}
		if len(mine) > 0 {
			restricted := maps.Clone(set)
			restricted["host"] = mine
			ownSets = append(ownSets, restricted)
		}
	}
	if len(hosts) == 0 {
		return nil, false, nil, nil
	}
	if exclusive {
		return hosts, true, nil, raw
	}
	copyRoute := maps.Clone(route)
	delete(copyRoute, "@id")
	copyRoute["match"] = ownSets
	return hosts, false, kept, mustJSON(copyRoute)
}

type caddyErrorsServer struct {
	Errors *struct {
		Routes []json.RawMessage `json:"routes"`
	} `json:"errors"`
}

// unclaimedHosts are the target's hostnames that no live application,
// redirect, reservation or the control panel uses.
func (m *Manager) unclaimedHosts(t leftoverTarget) []string {
	control := m.controlState()
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(t.hosts), func(h string) bool { return m.hostnameOwnerLocked(h, t.appID, control) != "" })
}

// withdrawErrorRouteHosts removes hosts from every server's error routes.
func (m *Manager) withdrawErrorRouteHosts(ctx context.Context, archiveID string, t leftoverTarget) error {
	caddy, ok := m.nativeCaddy(ctx)
	if !ok || len(t.hosts) == 0 {
		return nil
	}
	own := m.unclaimedHosts(t)
	if len(own) == 0 {
		return nil
	}
	var servers map[string]caddyErrorsServer
	if found, err := caddy.get(ctx, "/config/apps/http/servers", &servers); err != nil || !found {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if !nativeCaddyServerName.MatchString(name) || servers[name].Errors == nil {
			continue
		}
		routes := servers[name].Errors.Routes
		for i := len(routes) - 1; i >= 0; i-- {
			hosts, exclusive, kept, restore := splitErrorRoute(routes[i], t.appID, own)
			if len(hosts) == 0 {
				continue
			}
			path := "/config/apps/http/servers/" + name + "/errors/routes/" + strconv.Itoa(i)
			var current json.RawMessage
			if found, err := caddy.get(ctx, path, &current); err != nil || !found || !jsonEqual(current, routes[i]) {
				return errors.New("the web server's error routes changed while they were read; retry")
			}
			record := ArchivedErrorRoute{Server: name, Index: i, Exclusive: exclusive, Hosts: hosts, Route: restore}
			if err := m.updateArchive(archiveID, func(x *AppArchive) {
				if x.Native.Leftovers == nil {
					x.Native.Leftovers = &ArchivedLeftovers{}
				}
				x.Native.Leftovers.ErrorRoutes = append(x.Native.Leftovers.ErrorRoutes, record)
			}); err != nil {
				return err
			}
			var err error
			detail := "error-page route of " + name + " that matched only these hostnames: removed"
			if exclusive {
				_, err = caddy.call(ctx, http.MethodDelete, path, nil)
			} else {
				_, err = caddy.call(ctx, http.MethodPatch, path+"/match", mustJSON(kept))
				detail = "removed from a shared error-page route of " + name + "; its other hostnames are unchanged"
			}
			if err != nil {
				return err
			}
			m.archiveDeleted(archiveID, ArchiveItem{Kind: "error route", Name: name + " " + strings.Join(hosts, ", "), Detail: detail + " (kept in the archive for a restore)"})
		}
	}
	return nil
}

// restoreErrorRoutes puts the application's hostnames back into the error
// routes, limited to the hostnames the restore keeps.
func (m *Manager) restoreErrorRoutes(ctx context.Context, leftovers *ArchivedLeftovers, hostnames []string) error {
	if leftovers == nil || len(leftovers.ErrorRoutes) == 0 {
		return nil
	}
	caddy, ok := m.nativeCaddy(ctx)
	if !ok {
		return nil
	}
	records := slices.Clone(leftovers.ErrorRoutes)
	slices.SortStableFunc(records, func(a, b ArchivedErrorRoute) int {
		if c := strings.Compare(a.Server, b.Server); c != 0 {
			return c
		}
		return a.Index - b.Index
	})
	for _, r := range records {
		if !nativeCaddyServerName.MatchString(r.Server) {
			continue
		}
		var route map[string]any
		if json.Unmarshal(r.Route, &route) != nil {
			continue
		}
		var sets []any
		list, _ := route["match"].([]any)
		for _, rawSet := range list {
			set, ok := rawSet.(map[string]any)
			if !ok {
				continue
			}
			values, _ := set["host"].([]any)
			var keep []any
			for _, v := range values {
				if host, ok := v.(string); ok && hostIn(hostnames, host) {
					keep = append(keep, host)
				}
			}
			if len(keep) > 0 {
				set = maps.Clone(set)
				set["host"] = keep
				sets = append(sets, set)
			}
		}
		if len(sets) == 0 {
			continue
		}
		route["match"] = sets
		raw := mustJSON(route)
		base := "/config/apps/http/servers/" + r.Server + "/errors"
		var block struct {
			Routes []json.RawMessage `json:"routes"`
		}
		found, err := caddy.get(ctx, base, &block)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(block.Routes, func(existing json.RawMessage) bool { return jsonEqual(existing, raw) }) {
			continue // restored by an earlier attempt
		}
		switch {
		case !found:
			_, err = caddy.call(ctx, http.MethodPost, base, mustJSON(map[string]any{"routes": []json.RawMessage{raw}}))
		case r.Index < len(block.Routes):
			_, err = caddy.call(ctx, http.MethodPut, base+"/routes/"+strconv.Itoa(r.Index), raw)
		default:
			_, err = caddy.call(ctx, http.MethodPost, base+"/routes", raw)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- access-log logger names ------------------------------------------------------------

type caddyLogsServer struct {
	Logs *struct {
		LoggerNames map[string]json.RawMessage `json:"logger_names"`
	} `json:"logs"`
}

func loggerNamePath(server, host string) string {
	return "/config/apps/http/servers/" + server + "/logs/logger_names/" + url.PathEscape(host)
}

// withdrawLoggerNames removes the target's hostnames from the access-log
// logger_names of every web server, keeping each entry in the archive.
func (m *Manager) withdrawLoggerNames(ctx context.Context, archiveID string, t leftoverTarget) error {
	caddy, ok := m.nativeCaddy(ctx)
	if !ok || len(t.hosts) == 0 {
		return nil
	}
	own := m.unclaimedHosts(t)
	if len(own) == 0 {
		return nil
	}
	var servers map[string]caddyLogsServer
	if found, err := caddy.get(ctx, "/config/apps/http/servers", &servers); err != nil || !found {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if !nativeCaddyServerName.MatchString(name) || servers[name].Logs == nil {
			continue
		}
		names := servers[name].Logs.LoggerNames
		for _, host := range slices.Sorted(maps.Keys(names)) {
			if !hostIn(own, host) || !validLoggerHost(host) {
				continue
			}
			record := ArchivedLoggerName{Server: name, Host: host, Value: slices.Clone(names[host])}
			if err := m.updateArchive(archiveID, func(x *AppArchive) {
				if x.Native.Leftovers == nil {
					x.Native.Leftovers = &ArchivedLeftovers{}
				}
				if !slices.ContainsFunc(x.Native.Leftovers.LoggerNames, func(r ArchivedLoggerName) bool { return r.Server == name && r.Host == host }) {
					x.Native.Leftovers.LoggerNames = append(x.Native.Leftovers.LoggerNames, record)
				}
			}); err != nil {
				return err
			}
			if _, err := caddy.call(ctx, http.MethodDelete, loggerNamePath(name, host), nil); err != nil {
				return err
			}
			m.archiveDeleted(archiveID, ArchiveItem{Kind: "log name", Name: name + " " + host, Detail: "removed from the web server's access-log logger_names (kept in the archive for a restore)"})
		}
	}
	return nil
}

var loggerHostPattern = regexp.MustCompile(`^[A-Za-z0-9*][A-Za-z0-9.*-]{0,252}$`)

func validLoggerHost(host string) bool { return loggerHostPattern.MatchString(host) }

// restoreLoggerNames puts the application's hostnames back into the
// access-log logger_names, limited to the hostnames the restore keeps; an
// entry present meanwhile is left as it is.
func (m *Manager) restoreLoggerNames(ctx context.Context, leftovers *ArchivedLeftovers, hostnames []string) error {
	if leftovers == nil || len(leftovers.LoggerNames) == 0 {
		return nil
	}
	caddy, ok := m.nativeCaddy(ctx)
	if !ok {
		return nil
	}
	for _, r := range leftovers.LoggerNames {
		if !nativeCaddyServerName.MatchString(r.Server) || !validLoggerHost(r.Host) || !hostIn(hostnames, r.Host) || len(r.Value) == 0 {
			continue
		}
		server := "/config/apps/http/servers/" + r.Server
		if found, err := caddy.get(ctx, server, nil); err != nil || !found {
			if err != nil {
				return err
			}
			continue // the server is gone
		}
		var logs struct {
			LoggerNames map[string]json.RawMessage `json:"logger_names"`
		}
		found, err := caddy.get(ctx, server+"/logs", &logs)
		if err != nil {
			return err
		}
		if _, present := logs.LoggerNames[r.Host]; present {
			continue
		}
		switch {
		case !found:
			_, err = caddy.call(ctx, http.MethodPost, server+"/logs", mustJSON(map[string]any{"logger_names": map[string]json.RawMessage{r.Host: r.Value}}))
		case logs.LoggerNames == nil:
			_, err = caddy.call(ctx, http.MethodPost, server+"/logs/logger_names", mustJSON(map[string]json.RawMessage{r.Host: r.Value}))
		default:
			_, err = caddy.call(ctx, http.MethodPost, loggerNamePath(r.Server, r.Host), r.Value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- deletion, sweep and restore ---------------------------------------------------------

// archiveLeftovers removes the leftovers of one archived application. The
// caller holds the application's operation.
func (m *Manager) archiveLeftovers(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	t, ok := m.leftoverTargetFor(a)
	if !ok {
		return nil
	}
	// First: a hostname still reserved reads as owned by "a reserved
	// application", which would keep it in the error routes.
	if err := m.releaseArchiveReservations(archiveID); err != nil {
		return err
	}
	for _, scope := range []string{"user", "system"} {
		units, err := m.findLeftoverUnits(t, scope)
		if err != nil {
			return err
		}
		if err := m.removeLeftoverUnits(ctx, archiveID, scope, units); err != nil {
			return err
		}
		// A system unit proven here proves the user sockets that trigger it
		// and the other way round on the next pass; one pass per scope is
		// enough for units that name their service.
		for _, u := range units {
			t.proven[scope][u.Name] = true
		}
	}
	if err := m.resetRemovedUnits(ctx, archiveID); err != nil {
		return err
	}
	if err := m.withdrawErrorRouteHosts(ctx, archiveID, t); err != nil {
		return err
	}
	return m.withdrawLoggerNames(ctx, archiveID, t)
}

// sweepArchiveLeftovers re-applies archiveLeftovers to every archived
// native application whose directory and identity nothing uses again.
func (m *Manager) sweepArchiveLeftovers(ctx context.Context) {
	m.mu.Lock()
	var ids []string
	for id, a := range m.data.AppArchives {
		if a.Phase == archivePhaseArchived && a.Native != nil && (a.Kind == archiveKindAdopted || a.Kind == archiveKindNative) {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	slices.Sort(ids)
	for _, id := range ids {
		if err := m.sweepArchiveLeftoversOf(ctx, id); err != nil {
			log.Printf("archive %s: leftovers: %v", id, err)
		}
	}
}

func (m *Manager) sweepArchiveLeftoversOf(ctx context.Context, archiveID string) error {
	m.mu.Lock()
	a, ok := m.data.AppArchives[archiveID]
	if !ok || a.Phase != archivePhaseArchived || a.Native == nil {
		m.mu.Unlock()
		return nil
	}
	t, ok := m.leftoverTargetFor(a)
	if !ok {
		m.mu.Unlock()
		return nil
	}
	_, native := m.data.NativeApps[a.AppID]
	_, container := m.data.Apps[a.AppID]
	reused := native || container || m.nativeOperations[a.AppID]
	for _, other := range m.data.NativeApps {
		reused = reused || other.Name == a.Native.App.Name || filepath.Join(m.nativeRootLocked(), other.Name) == t.root
	}
	if reused {
		m.mu.Unlock()
		return nil
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[a.AppID] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.nativeOperations, a.AppID); m.mu.Unlock() }()
	// The directory is back (restored or recreated by hand): its units are
	// in use again.
	if _, err := os.Lstat(t.root); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := m.archiveLeftovers(ctx, archiveID); err != nil {
		return err
	}
	// What a deletion by an earlier agent did not do.
	if a.Kind == archiveKindAdopted && a.Native.Adopted != nil {
		env := archivedNativeEnvironment(a.Native.Files, t.root)
		if err := m.decideAdoptedDatabases(ctx, archiveID, env); err != nil {
			return err
		}
	}
	return m.archiveRemoveBackupLocks(archiveID)
}

// nativeEnvironmentFiles are the environment files nativeEnvironment
// reads, relative to the application's directory.
var nativeEnvironmentFiles = []string{filepath.Join("current", ".env"), filepath.Join("current", ".asdf-vars"), ".asdf-vars"}

// archivedNativeEnvironment reads the environment of an archived native
// application from its kept directory (files), as nativeEnvironment does
// from the live one. The current link names the original directory (root):
// it is followed inside the kept copy, never outside it.
func archivedNativeEnvironment(files, root string) map[string]string {
	values := map[string]string{}
	if files == "" || !filepath.IsAbs(files) {
		return values
	}
	files = filepath.Clean(files)
	inside := func(p string) bool {
		rel, err := filepath.Rel(files, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	for _, rel := range nativeEnvironmentFiles {
		dir := files
		if first, rest, nested := strings.Cut(rel, string(filepath.Separator)); nested {
			link := filepath.Join(files, first)
			target, err := os.Readlink(link)
			switch {
			case err != nil:
				dir = link // a plain directory
			case filepath.IsAbs(target) && root != "" && pathWithin(target, root, ""):
				r, _ := filepath.Rel(root, filepath.Clean(target))
				dir = filepath.Join(files, r)
			case filepath.IsAbs(target):
				dir = filepath.Clean(target)
			default:
				dir = filepath.Join(files, target)
			}
			rel = rest
		}
		path := filepath.Join(dir, rel)
		if !inside(path) {
			continue
		}
		if real, err := filepath.EvalSymlinks(path); err != nil || !inside(real) {
			continue
		}
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			continue
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 {
			file.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(file, 128<<10))
		file.Close()
		if err == nil {
			parseNativeEnvironment(data, values)
		}
	}
	return values
}

func (m *Manager) nativeRootLocked() string {
	if m.cfg.Native == nil {
		return ""
	}
	return m.cfg.Native.Root
}

// archiveLeftoversLoop runs the sweep at start and hourly.
func (m *Manager) archiveLeftoversLoop() {
	defer m.wg.Done()
	timer := time.NewTimer(archiveLeftoversStart)
	defer timer.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-timer.C:
		}
		bounded, cancel := context.WithTimeout(m.ctx, archiveOperationLimit)
		m.sweepArchiveLeftovers(bounded)
		cancel()
		timer.Reset(archiveLeftoversInterval)
	}
}

// restoreLeftoverFiles writes the removed unit files back and reloads
// their service managers.
func (m *Manager) restoreLeftoverFiles(ctx context.Context, leftovers *ArchivedLeftovers) error {
	if leftovers == nil || len(leftovers.Units) == 0 {
		return nil
	}
	reload := map[string]bool{}
	for _, u := range leftovers.Units {
		if u.Scope != "user" && u.Scope != "system" || u.Scope == "user" && m.cfg.Native == nil {
			continue
		}
		if filepath.Dir(u.Path) != m.adoptedUnitDir(u.Scope) || filepath.Base(u.Path) != u.Name || !leftoverUnitPattern.MatchString(u.Name) {
			return fmt.Errorf("the unit file of %s is outside the unit directory", u.Name)
		}
		if _, err := os.Lstat(u.Path); err == nil {
			continue // recreated meanwhile; never overwritten
		}
		mode := os.FileMode(u.Mode) & 0o777
		if err := os.WriteFile(u.Path, []byte(u.Content), mode); err != nil {
			return fmt.Errorf("could not write the unit file of %s", u.Name)
		}
		if err := os.Chmod(u.Path, mode); err != nil {
			return err
		}
		if err := os.Lchown(u.Path, u.UID, u.GID); err != nil && os.Geteuid() == 0 {
			return fmt.Errorf("could not hand the unit file of %s to its owner", u.Name)
		}
		reload[u.Scope] = true
	}
	for _, scope := range slices.Sorted(maps.Keys(reload)) {
		if err := m.adoptedUnitCtl(ctx, scope, "daemon-reload"); err != nil {
			return errors.New("systemd could not reload the application's units")
		}
	}
	return nil
}

// restoreLeftoverStates enables and starts the restored units as they were;
// skip names units whose state the application's own restore handles.
func (m *Manager) restoreLeftoverStates(ctx context.Context, leftovers *ArchivedLeftovers, skip func(scope, name string) bool) error {
	if leftovers == nil {
		return nil
	}
	for _, u := range leftovers.Units {
		if skip != nil && skip(u.Scope, u.Name) || u.Scope == "user" && m.cfg.Native == nil {
			continue
		}
		var args []string
		switch {
		case u.Enabled && u.Active:
			args = []string{"enable", "--now", "--no-block", "--quiet", u.Name}
		case u.Enabled:
			args = []string{"enable", "--quiet", u.Name}
		case u.Active:
			args = []string{"start", "--no-block", u.Name}
		default:
			continue
		}
		if err := m.adoptedUnitCtl(ctx, u.Scope, args...); err != nil {
			return fmt.Errorf("could not start %s: %w", u.Name, err)
		}
	}
	return nil
}
