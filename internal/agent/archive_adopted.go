package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Deleting an adopted native application: one that existed on the host
// before this agent (its files under the native root, for example
// /home/deploy/<App>, its units on the deployment user's service manager or
// in the system unit directory, its environment in the application's own
// files). It is archived like every other application, with what belongs
// to it alone:
//
//   - its units are stopped and disabled, and the unit files bound to it are
//     removed (their contents are kept in the archive for a restore);
//   - its crontab schedule block is removed; every other line stays;
//   - the web server routes that serve only its hostnames, its redirect
//     routes and its certificates are removed (same rules as other apps);
//   - its cache is deleted: the Redis database number it uses on a shared
//     instance is flushed when no other application uses that number, and
//     never otherwise;
//   - its databases are kept: a role only it uses loses its login (also a
//     superuser role used by nothing else); a role another application
//     uses keeps its login and only loses CONNECT on this application's
//     database; a database another application uses is left as it is;
//   - its backups are deleted, its directory moves into the archive.
//
// The shared deployment account, its home (other than this application's
// directory and unit files), other applications' directories and the shared
// PostgreSQL and Redis instances are never changed. A restore reverses
// every step.

const archiveKindAdopted = "adopted"

// ArchivedAdopted is what deleting an adopted application changed.
type ArchivedAdopted struct {
	// Root is where the application's directory was.
	Root string `json:"root"`
	// Scope is the service manager of its units: user or system.
	Scope string `json:"scope"`
	// User is the account the units run as; it is never changed.
	User   string               `json:"user"`
	Units  []ArchivedUnit       `json:"units,omitempty"`
	Policy *NativeReleasePolicy `json:"policy,omitempty"`
	// Schedule is the crontab block that was removed.
	Schedule *ArchivedSchedule `json:"schedule,omitempty"`
	// Routes are the web server routes that served only its hostnames.
	Routes    []ArchivedRoute           `json:"routes,omitempty"`
	Databases []ArchivedAdoptedDatabase `json:"databases,omitempty"`
	// Records are the database records the agent kept for it (backup
	// policy and binding), restored with it.
	Records map[string]ArchivedDatabaseRecord `json:"records,omitempty"`
	// Stopped is its stopped state when it was deleted.
	Stopped *NativeStoppedState `json:"stopped,omitempty"`
}

// ArchivedUnit is one unit of an adopted application.
type ArchivedUnit struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    uint32 `json:"mode"`
	UID     int    `json:"uid"`
	GID     int    `json:"gid"`
	Enabled bool   `json:"enabled"`
	Active  bool   `json:"active"`
	// Kept is a unit file that does not name the application's directory:
	// the unit was stopped and disabled, the file left in place.
	Kept bool `json:"kept,omitempty"`
}

// ArchivedSchedule is a removed crontab block.
type ArchivedSchedule struct {
	User       string `json:"user"`
	Identifier string `json:"identifier"`
	Entries    string `json:"entries"`
}

// ArchivedRoute is a removed web server route.
type ArchivedRoute struct {
	Server string          `json:"server"`
	Route  json.RawMessage `json:"route"`
}

// ArchivedAdoptedDatabase is what was done to one database's access.
type ArchivedAdoptedDatabase struct {
	Resource string `json:"resource,omitempty"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Role     string `json:"role"`
	// Action is nologin (the role lost its login), revoke (the role lost
	// CONNECT on the database) or kept (nothing changed).
	Action        string `json:"action"`
	PublicConnect bool   `json:"public_connect,omitempty"`
	Superuser     bool   `json:"superuser,omitempty"`
	SharedWith    string `json:"shared_with,omitempty"`
}

// ArchivedDatabaseRecord is a database record removed with the application.
type ArchivedDatabaseRecord struct {
	Database Database               `json:"database"`
	Binding  *NativeDatabaseBinding `json:"binding,omitempty"`
}

// nativeServiceUnits returns the registration and every unit of a native
// application: the release policy's system units, or the verified user
// units bound to its directory.
func (m *Manager) nativeServiceUnits(id string) (NativeApplication, []string, error) {
	m.mu.Lock()
	a, ok := m.data.NativeApps[id]
	m.mu.Unlock()
	if !ok {
		return a, nil, errLifecycleNotFound
	}
	if _, system := m.nativeSystemPolicy(id); system {
		units, err := m.nativeRuntimeServices(a)
		return a, units, err
	}
	verified, err := m.nativeApp(id)
	if err != nil {
		return a, nil, err
	}
	return verified, slices.Clone(verified.Services), nil
}

func (m *Manager) adoptedUnitDir(scope string) string {
	if scope == "system" {
		return nativeSystemUnitDir
	}
	return filepath.Join(m.cfg.Native.Root, ".config", "systemd", "user")
}

func (m *Manager) adoptedUnitCtl(ctx context.Context, scope string, args ...string) error {
	if scope == "system" {
		_, err := m.nativeRootExec(ctx, append([]string{"systemctl"}, args...)...)
		return err
	}
	_, err := m.nativeExec(ctx, append([]string{"systemctl", "--user"}, args...)...)
	return err
}

// adoptedArchiveSnapshot reads, before anything changes, what deleting an
// adopted application changes. It refuses when the application's
// directory is not its own.
func (m *Manager) adoptedArchiveSnapshot(ctx context.Context, id string) (*ArchivedAdopted, error) {
	if m.cfg.Native == nil {
		return nil, errors.New("native management is not configured on this server")
	}
	a, units, err := m.nativeServiceUnits(id)
	if err != nil {
		return nil, err
	}
	if !nativeNamePattern.MatchString(a.Name) {
		return nil, errors.New("the application's directory name is invalid")
	}
	root := filepath.Join(m.cfg.Native.Root, a.Name)
	if info, err := os.Lstat(root); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return nil, fmt.Errorf("%s is not a directory; review it on the host", root)
	}
	if account, err := user.Lookup(m.cfg.Native.User); err == nil && filepath.Clean(account.HomeDir) == root {
		return nil, errors.New("the application's directory is the deployment account's home; review it on the host")
	}
	m.mu.Lock()
	for other, b := range m.data.NativeApps {
		if other != id && b.Name == a.Name {
			m.mu.Unlock()
			return nil, fmt.Errorf("%s uses the same directory; review it on the host", other)
		}
	}
	stopped, isStopped := m.data.NativeStopped[id]
	m.mu.Unlock()
	snapshot := &ArchivedAdopted{Root: root, Scope: "user", User: m.cfg.Native.User}
	if m.nativeCreatedSystemUnits(id) {
		snapshot.Scope = "system"
	}
	if _, policy, err := m.nativeReleasePolicy(id); err == nil {
		p := cloneJSON(policy)
		snapshot.Policy = &p
		if policy.User != "" {
			snapshot.User = policy.User
		}
	}
	if isStopped {
		s := cloneJSON(stopped)
		snapshot.Stopped = &s
	}
	states, err := m.nativeUnitStates(ctx, id, units)
	if err != nil {
		return nil, err
	}
	dir := m.adoptedUnitDir(snapshot.Scope)
	for _, unit := range units {
		path := filepath.Join(dir, unit)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
			return nil, fmt.Errorf("the unit file of %s is unavailable; review it on the host", unit)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("the unit file of %s is unreadable", unit)
		}
		u := ArchivedUnit{Name: unit, Path: path, Content: string(content), Mode: uint32(info.Mode().Perm()), Enabled: states[unit].enabled(), Active: states[unit].running()}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			u.UID, u.GID = int(st.Uid), int(st.Gid)
		}
		// User units are bound to the directory by their WorkingDirectory
		// (nativeBinding); a system unit must name it.
		u.Kept = snapshot.Scope == "system" && !strings.Contains(u.Content, root+"/") && !strings.Contains(u.Content, root+"\n")
		snapshot.Units = append(snapshot.Units, u)
	}
	return snapshot, nil
}

// archiveAdoptedApp runs the deletion steps of an adopted application.
func (m *Manager) archiveAdoptedApp(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	id := a.AppID
	m.mu.Lock()
	if m.nativeOperations[id] {
		m.mu.Unlock()
		return errors.New("an operation on this application is running")
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[id] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.nativeOperations, id); m.mu.Unlock() }()
	steps := []struct {
		name string
		run  func() error
	}{
		{"units", func() error { return m.archiveAdoptedUnits(ctx, archiveID) }},
		{"schedule", func() error { return m.archiveAdoptedSchedule(ctx, archiveID) }},
		{"routes", func() error { return m.archiveAdoptedRoutes(ctx, archiveID) }},
		{"leftovers", func() error { return m.archiveLeftovers(ctx, archiveID) }},
		{"cache", func() error { return m.archiveAdoptedCache(ctx, archiveID) }},
		{"logins", func() error { return m.archiveAdoptedLogins(ctx, archiveID) }},
		{"files", func() error { return m.archiveAdoptedFiles(ctx, archiveID) }},
		{"state", func() error { return m.archiveAdoptedState(ctx, archiveID) }},
	}
	for _, step := range steps {
		if err := m.archiveStep(archiveID, "adopted-"+step.name, step.run); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) archiveAdoptedUnits(ctx context.Context, archiveID string) error {
	adopted := m.archiveSnapshot(archiveID).Native.Adopted
	var removed, kept []string
	for _, u := range adopted.Units {
		if err := m.adoptedUnitCtl(ctx, adopted.Scope, "disable", "--now", "--quiet", u.Name); err != nil {
			return fmt.Errorf("could not stop %s: %w", u.Name, err)
		}
		if u.Kept {
			kept = append(kept, u.Name)
			continue
		}
		if filepath.Dir(u.Path) != m.adoptedUnitDir(adopted.Scope) || filepath.Base(u.Path) != u.Name {
			return fmt.Errorf("the unit file of %s is outside the unit directory", u.Name)
		}
		if err := os.Remove(u.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("could not remove the unit file of %s", u.Name)
		}
		removed = append(removed, u.Name)
	}
	if len(adopted.Units) > 0 {
		if err := m.adoptedUnitCtl(ctx, adopted.Scope, "daemon-reload"); err != nil {
			return errors.New("systemd could not reload after removing the application's units")
		}
		m.resetFailedUnits(ctx, adopted.Scope, removed)
		detail := "stopped and disabled; unit files removed (kept in the archive for a restore)"
		if len(kept) > 0 {
			detail += "; unit files left in place because they do not name the application's directory: " + strings.Join(kept, ", ")
		}
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "units", Name: strings.Join(append(removed, kept...), ", "), Count: len(adopted.Units), Detail: detail})
	}
	return nil
}

func (m *Manager) archiveAdoptedSchedule(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	adopted := a.Native.Adopted
	if adopted.Policy == nil {
		return nil
	}
	tab := nativeReleaseCrontab{m: m, user: adopted.User, identifier: adopted.Policy.ScheduleIdentifier()}
	entries := ""
	if s := adopted.Stopped; s != nil && s.SchedulePaused {
		// Stopping removed the block already; its entries are kept there.
		entries = s.Schedule
	} else {
		removed, err := pauseNativeSchedule(ctx, tab)
		if err != nil {
			return fmt.Errorf("could not remove the schedule: %w", err)
		}
		entries = removed
	}
	if entries == "" {
		return nil
	}
	if err := m.updateArchive(archiveID, func(x *AppArchive) {
		x.Native.Adopted.Schedule = &ArchivedSchedule{User: tab.user, Identifier: tab.identifier, Entries: entries}
	}); err != nil {
		return err
	}
	m.archiveDeleted(archiveID, ArchiveItem{Kind: "tasks", Name: tab.identifier, Count: strings.Count(entries, "\n") + 1, Detail: "crontab schedule of " + tab.user + " (kept in the archive for a restore; the rest of the crontab is unchanged)"})
	return nil
}

// adoptedRouteServes reports a web server route that serves only the
// application's hostnames and reaches the application (its loopback port
// or its directory). shared is true when it also serves other hostnames.
func adoptedRouteServes(raw json.RawMessage, hosts []string, port, root string) (serves, shared bool) {
	var route caddyRoute
	if json.Unmarshal(raw, &route) != nil || strings.HasPrefix(route.ID, "lidza-") {
		return false, false
	}
	routed := caddyRouteHosts(route)
	if len(routed) == 0 {
		return false, false
	}
	own, other := 0, 0
	for _, host := range routed {
		if slices.ContainsFunc(hosts, func(h string) bool { return strings.EqualFold(h, host) }) {
			own++
		} else {
			other++
		}
	}
	if own == 0 {
		return false, false
	}
	reaches := root != "" && (bytes.Contains(raw, []byte(root+"/")) || bytes.Contains(raw, []byte(`"`+root+`"`)))
	if port != "" {
		for _, dial := range []string{"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port, ":" + port} {
			reaches = reaches || bytes.Contains(raw, []byte(`"`+dial+`"`))
		}
	}
	if !reaches {
		return false, false
	}
	return other == 0, other > 0
}

func (m *Manager) archiveAdoptedRoutes(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	adopted := a.Native.Adopted
	caddy, ok := m.nativeCaddy(ctx)
	if !ok {
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "route", Name: "web server", Detail: "kept: this server's web server has no admin API; remove the site of " + strings.Join(a.Hostnames, ", ") + " from its configuration"})
		return nil
	}
	if err := caddy.withdrawStoppedRoute(ctx, a.AppID); err != nil {
		return err
	}
	if len(adopted.Routes) > 0 {
		return nil // removed by an earlier attempt
	}
	port := a.Native.App.Port
	if adopted.Policy != nil && adopted.Policy.Port != "" {
		port = adopted.Policy.Port
	}
	own := append([]string{a.Native.App.Domain}, a.Native.App.AdditionalDomains...)
	var servers map[string]caddyServer
	if _, err := caddy.get(ctx, "/config/apps/http/servers", &servers); err != nil {
		return err
	}
	var removed []ArchivedRoute
	var shared []string
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if !nativeCaddyServerName.MatchString(name) {
			continue
		}
		routes := servers[name].Routes
		for i := len(routes) - 1; i >= 0; i-- {
			serves, other := adoptedRouteServes(routes[i], own, port, adopted.Root)
			if other {
				shared = append(shared, name+"/"+strconv.Itoa(i))
			}
			if !serves {
				continue
			}
			path := "/config/apps/http/servers/" + name + "/routes/" + strconv.Itoa(i)
			var current json.RawMessage
			if found, err := caddy.get(ctx, path, &current); err != nil || !found || !jsonEqual(current, routes[i]) {
				return errors.New("the web server's routes changed while they were read; delete the application again")
			}
			if _, err := caddy.call(ctx, http.MethodDelete, path, nil); err != nil {
				return err
			}
			removed = append(removed, ArchivedRoute{Server: name, Route: slices.Clone(routes[i])})
			if err := m.updateArchive(archiveID, func(x *AppArchive) { x.Native.Adopted.Routes = slices.Clone(removed) }); err != nil {
				return err
			}
		}
	}
	if len(removed) > 0 {
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "route", Name: "existing web server routes", Count: len(removed), Detail: "routes that served only " + strings.Join(own, ", ") + " (kept in the archive for a restore)"})
	}
	if len(shared) > 0 {
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "route", Name: "shared web server routes", Count: len(shared), Detail: "kept: they also serve other hostnames; remove " + strings.Join(own, ", ") + " from them by hand"})
	}
	return nil
}

// ---- cache -------------------------------------------------------------------

// redisTarget is one Redis database an application uses.
type redisTarget struct {
	Instance string // tcp:<port> or unix:<socket>
	Port     string
	Socket   string
	DB       int
	User     string
	Password string
	External string // the host of a Redis that is not on this server
}

func (t redisTarget) key() string { return t.Instance + "/" + strconv.Itoa(t.DB) }

func (t redisTarget) label() string {
	where := "127.0.0.1:" + t.Port
	if t.Socket != "" {
		where = t.Socket
	}
	return fmt.Sprintf("Redis %s database %d", where, t.DB)
}

// redisTargets are the Redis databases an environment names.
func redisTargets(env map[string]string) []redisTarget {
	var out []redisTarget
	seen := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(env)) {
		raw := strings.TrimSpace(env[key])
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		scheme := strings.ToLower(u.Scheme)
		var t redisTarget
		switch {
		case scheme == "redis" || scheme == "rediss":
			host := strings.ToLower(u.Hostname())
			if host != "" && host != "localhost" && host != "127.0.0.1" && host != "::1" {
				t.External = host
			}
			t.Port = cmpOr(u.Port(), "6379")
			t.Instance = "tcp:" + t.Port
			if db := strings.Trim(u.Path, "/"); db != "" {
				n, err := strconv.Atoi(db)
				if err != nil || n < 0 {
					continue
				}
				t.DB = n
			}
		case scheme == "unix" && strings.Contains(strings.ToUpper(key), "REDIS") && filepath.IsAbs(u.Path):
			t.Socket, t.Instance = u.Path, "unix:"+u.Path
			if db := u.Query().Get("db"); db != "" {
				n, err := strconv.Atoi(db)
				if err != nil || n < 0 {
					continue
				}
				t.DB = n
			}
		default:
			continue
		}
		if u.User != nil {
			if password, set := u.User.Password(); set {
				t.User, t.Password = u.User.Username(), password
			} else {
				t.Password = u.User.Username()
			}
		}
		if t.External != "" {
			t.Instance = "external:" + t.External
		}
		if !seen[t.key()] {
			seen[t.key()] = true
			out = append(out, t)
		}
	}
	return out
}

// otherAppEnvironments returns the environment of every other application
// on this server, by id.
func (m *Manager) otherAppEnvironments(except string) map[string]map[string]string {
	m.mu.Lock()
	natives := maps.Clone(m.data.NativeApps)
	created := maps.Clone(m.data.NativeCreated)
	containers := map[string]map[string]string{}
	for id, a := range m.data.Apps {
		if id != except {
			containers[id] = maps.Clone(a.Env)
		}
	}
	m.mu.Unlock()
	out := containers
	for id, a := range natives {
		if id == except {
			continue
		}
		if policy, ok := created[id]; ok {
			if values, err := nativeReleaseEnvValues(policy); err == nil {
				out[id] = values
			}
			continue
		}
		out[id] = m.nativeEnvironment(a.Name)
	}
	return out
}

func redisCLIArg(value string) string {
	if value == "" || strings.ContainsAny(value, " \t\"'\\") || !strings.ContainsFunc(value, func(r rune) bool { return r > ' ' }) {
		return strconv.Quote(value)
	}
	return value
}

func (m *Manager) archiveAdoptedCache(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	env := m.nativeEnvironment(a.Native.App.Name)
	targets := redisTargets(env)
	if len(targets) == 0 {
		return nil
	}
	users := map[string]string{}
	others := m.otherAppEnvironments(a.AppID)
	for _, other := range slices.Sorted(maps.Keys(others)) {
		for _, t := range redisTargets(others[other]) {
			if _, taken := users[t.key()]; !taken {
				users[t.key()] = other
			}
		}
	}
	h := nativeHostDataHost(m)
	for _, t := range targets {
		switch {
		case t.External != "":
			m.archiveDeleted(archiveID, ArchiveItem{Kind: "cache", Name: "redis " + t.External, Detail: "kept: an external Redis is not managed here"})
			continue
		case users[t.key()] != "":
			m.archiveDeleted(archiveID, ArchiveItem{Kind: "cache", Name: t.label(), Detail: "kept: " + users[t.key()] + " uses the same database number; nothing was flushed"})
			continue
		}
		var script strings.Builder
		if t.Password != "" {
			script.WriteString("AUTH ")
			if t.User != "" {
				script.WriteString(redisCLIArg(t.User) + " ")
			}
			script.WriteString(redisCLIArg(t.Password) + "\n")
		}
		fmt.Fprintf(&script, "SELECT %d\nFLUSHDB\n", t.DB)
		args := []string{"-h", "127.0.0.1", "-p", t.Port}
		if t.Socket != "" {
			args = []string{"-s", t.Socket}
		}
		bounded, cancel := context.WithTimeout(ctx, time.Minute)
		out, err := h.run(bounded, script.String(), "/usr/bin/redis-cli", args...)
		cancel()
		lower := strings.ToLower(out)
		if err != nil || strings.Contains(lower, "err") || strings.Contains(lower, "noauth") || strings.Contains(lower, "wrongpass") || strings.Contains(lower, "denied") {
			return fmt.Errorf("could not flush %s; retry", t.label())
		}
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "cache", Name: t.label(), Detail: "flushed: the database number is this application's alone; the instance and its other databases are unchanged"})
	}
	return nil
}

// ---- databases -----------------------------------------------------------------

// pgTarget is one PostgreSQL database and role an application uses.
type pgTarget struct {
	Resource string
	Port     int
	Database string
	Role     string
}

func (t pgTarget) databaseKey() string { return strconv.Itoa(t.Port) + "/" + t.Database }
func (t pgTarget) roleKey() string     { return strconv.Itoa(t.Port) + "/" + t.Role }

// postgresHostLocal reports a PostgreSQL host that is this server: a
// socket directory, a loopback name or address, an address of one of its
// interfaces (adopted hosts name their private address) or its host name.
// Tests replace it.
var postgresHostLocal = func(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" || host == "localhost" || strings.HasPrefix(host, "/") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return true
		}
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			return false
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
				return true
			}
		}
		return false
	}
	name, err := os.Hostname()
	return err == nil && strings.EqualFold(name, host)
}

// parsePostgresURL parses a connection URL, also one whose password holds
// characters a URL must escape.
func parsePostgresURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err == nil {
		return u, nil
	}
	scheme, rest, ok := strings.Cut(raw, "://")
	at := strings.LastIndex(rest, "@")
	if !ok || at < 0 {
		return nil, err
	}
	name, password, hasPassword := strings.Cut(rest[:at], ":")
	info := url.User(name)
	if hasPassword {
		info = url.UserPassword(name, password)
	}
	return url.Parse(scheme + "://" + info.String() + "@" + rest[at+1:])
}

// localPostgresTarget parses a connection URL to this server's PostgreSQL.
func localPostgresTarget(raw string) (pgTarget, bool) {
	u, err := parsePostgresURL(raw)
	if err != nil || u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return pgTarget{}, false
	}
	host := u.Hostname()
	if q := u.Query().Get("host"); q != "" {
		host = q
	}
	if !postgresHostLocal(host) {
		return pgTarget{}, false
	}
	port := 5432
	if p := cmpOr(u.Port(), u.Query().Get("port")); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return pgTarget{}, false
		}
		port = n
	}
	role := u.Query().Get("user")
	if u.User != nil && u.User.Username() != "" {
		role = u.User.Username()
	}
	t := pgTarget{Port: port, Database: cmpOr(strings.Trim(u.Path, "/"), u.Query().Get("dbname")), Role: role}
	if t.Database == "" || t.Role == "" || strings.Contains(t.Database, "/") {
		return pgTarget{}, false
	}
	return t, true
}

func postgresTargets(env map[string]string) []pgTarget {
	var out []pgTarget
	for _, key := range slices.Sorted(maps.Keys(env)) {
		if key != "DATABASE_URL" && !strings.HasSuffix(key, "_DATABASE_URL") {
			continue
		}
		if t, ok := localPostgresTarget(env[key]); ok {
			out = append(out, t)
		}
	}
	return out
}

// adoptedPostgresTargets are the application's databases: the bound
// resources first (live bindings, or the records its archive keeps), then
// the connection URLs of its environment.
func (m *Manager) adoptedPostgresTargets(a AppArchive, env map[string]string) []pgTarget {
	var out []pgTarget
	add := func(t pgTarget) {
		for i, existing := range out {
			if existing.databaseKey() == t.databaseKey() && existing.Role == t.Role {
				if out[i].Resource == "" {
					out[i].Resource = t.Resource
				}
				return
			}
		}
		out = append(out, t)
	}
	m.mu.Lock()
	for _, resource := range slices.Sorted(maps.Keys(m.data.NativeDatabases)) {
		binding := m.data.NativeDatabases[resource]
		if binding.Policy.AppID != a.AppID {
			continue
		}
		if t, ok := localPostgresTarget(m.data.Databases[resource].URL); ok {
			t.Resource = resource
			add(t)
		}
	}
	m.mu.Unlock()
	if a.Native != nil && a.Native.Adopted != nil {
		for _, resource := range slices.Sorted(maps.Keys(a.Native.Adopted.Records)) {
			if t, ok := localPostgresTarget(a.Native.Adopted.Records[resource].Database.URL); ok {
				t.Resource = resource
				add(t)
			}
		}
	}
	for _, t := range postgresTargets(env) {
		add(t)
	}
	return out
}

// postgresUsers names, by database and by role key, a live application
// (other than except) that uses each one: its environment, its database
// bindings and records, or its host database.
func (m *Manager) postgresUsers(except string, own []string) (databases, roles map[string]string) {
	databases, roles = map[string]string{}, map[string]string{}
	others := m.otherAppEnvironments(except)
	for _, other := range slices.Sorted(maps.Keys(others)) {
		for _, t := range postgresTargets(others[other]) {
			databases[t.databaseKey()] = cmpOr(databases[t.databaseKey()], other)
			roles[t.roleKey()] = cmpOr(roles[t.roleKey()], other)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, binding := range m.data.NativeDatabases {
		if binding.Policy.AppID == except || binding.Policy.AppID == "" {
			continue
		}
		port, _ := strconv.Atoi(cmpOr(binding.Policy.Port, "5432"))
		for _, role := range binding.Policy.Roles {
			key := pgTarget{Port: port, Role: role}.roleKey()
			roles[key] = cmpOr(roles[key], binding.Policy.AppID)
		}
	}
	for _, resource := range slices.Sorted(maps.Keys(m.data.Databases)) {
		d := m.data.Databases[resource]
		if slices.Contains(own, resource) || m.data.NativeDatabases[resource].Policy.AppID == except || !d.Removed.IsZero() {
			continue
		}
		if t, ok := localPostgresTarget(d.URL); ok {
			owner := cmpOr(m.data.NativeDatabases[resource].Policy.AppID, "database "+resource)
			databases[t.databaseKey()] = cmpOr(databases[t.databaseKey()], owner)
			roles[t.roleKey()] = cmpOr(roles[t.roleKey()], owner)
		}
	}
	for other, record := range m.data.NativeHostData {
		if pg := record.Postgres; pg != nil && other != except {
			t := pgTarget{Port: pg.Port, Database: pg.Name, Role: pg.Name}
			databases[t.databaseKey()] = cmpOr(databases[t.databaseKey()], other)
			roles[t.roleKey()] = cmpOr(roles[t.roleKey()], other)
		}
	}
	return databases, roles
}

func (m *Manager) archiveAdoptedLogins(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	return m.decideAdoptedDatabases(ctx, archiveID, m.nativeEnvironment(a.Native.App.Name))
}

// decideAdoptedDatabases records the application's databases in the
// archive (name, size, role) and disables what only it uses: a role no
// live application uses loses its login and its sessions end; a role
// another application shares keeps it and loses CONNECT on this
// application's database; a database another application uses is left
// alone. It runs once per archive (the deletion, or the sweep for an
// archive an earlier agent made); env is the application's environment.
func (m *Manager) decideAdoptedDatabases(ctx context.Context, archiveID string, env map[string]string) error {
	a := m.archiveSnapshot(archiveID)
	if a.Native == nil || a.Native.Adopted == nil || len(a.Native.Adopted.Databases) > 0 {
		return nil // decided by an earlier attempt; the statements below are idempotent there
	}
	targets := m.adoptedPostgresTargets(a, env)
	if len(targets) == 0 {
		return nil
	}
	own := slices.Clone(a.Databases)
	for resource := range a.Native.Adopted.Records {
		own = append(own, resource)
	}
	databases, roles := m.postgresUsers(a.AppID, own)
	h := nativeHostDataHost(m)
	var decided []ArchivedAdoptedDatabase
	for _, t := range targets {
		d := ArchivedAdoptedDatabase{Resource: t.Resource, Port: t.Port, Database: t.Database, Role: t.Role}
		database, err := pgQuoteIdent(t.Database)
		if err != nil {
			return err
		}
		role, err := pgQuoteIdent(t.Role)
		if err != nil {
			return err
		}
		databaseLiteral, _ := pgQuoteLiteral(t.Database)
		roleLiteral, _ := pgQuoteLiteral(t.Role)
		out, err := h.psql(ctx, t.Port, "SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = "+roleLiteral+";\n")
		if err != nil {
			return errors.New("could not read the PostgreSQL role; retry")
		}
		roleExists := strings.TrimSpace(out) != ""
		d.Superuser = strings.TrimSpace(out) == "t"
		var size int64
		measured := false
		if out, err := h.psql(ctx, t.Port, "SELECT pg_catalog.pg_database_size(datname) FROM pg_catalog.pg_database WHERE datname = "+databaseLiteral+";\n"); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil && n >= 0 {
				size, measured = n, true
			}
		}
		roleOwner := roles[t.roleKey()]
		if t.Role == "postgres" {
			roleOwner = "the server's PostgreSQL administration"
		}
		var script string
		switch {
		case !roleExists:
			d.Action = "kept"
		case databases[t.databaseKey()] != "":
			d.Action, d.SharedWith = "kept", databases[t.databaseKey()]
		case roleOwner != "":
			d.Action, d.SharedWith = "revoke", roleOwner
			out, err := h.psql(ctx, t.Port, "SELECT pg_catalog.has_database_privilege('public', "+databaseLiteral+", 'CONNECT');\n")
			if err != nil {
				return errors.New("could not read the database's privileges; retry")
			}
			d.PublicConnect = strings.TrimSpace(out) == "t"
			if d.PublicConnect {
				script += "REVOKE CONNECT ON DATABASE " + database + " FROM PUBLIC;\n"
			}
			script += "REVOKE CONNECT ON DATABASE " + database + " FROM " + role + ";\n" +
				"SELECT count(pg_catalog.pg_terminate_backend(pid)) FROM pg_catalog.pg_stat_activity WHERE datname = " + databaseLiteral + ";\n"
		default:
			d.Action = "nologin"
			script = "ALTER ROLE " + role + " NOLOGIN;\n" +
				"SELECT count(pg_catalog.pg_terminate_backend(pid)) FROM pg_catalog.pg_stat_activity WHERE usename = " + roleLiteral + " OR datname = " + databaseLiteral + ";\n"
		}
		if script != "" {
			if _, err := h.psql(ctx, t.Port, script); err != nil {
				return errors.New("disabling the PostgreSQL access failed; retry")
			}
		}
		decided = append(decided, d)
		detail := "host PostgreSQL database " + t.Database + " (port " + strconv.Itoa(t.Port) + "), role " + t.Role + ": "
		switch {
		case !roleExists:
			detail += "kept: the role does not exist on this server's PostgreSQL; nothing was changed"
		case d.Action == "kept":
			detail += "kept: shared with " + d.SharedWith + " (it uses the same database); unchanged"
		case d.Action == "revoke":
			detail += "role kept: shared with " + d.SharedWith + "; it lost CONNECT on this database and the database's sessions ended"
			if d.Superuser {
				detail += ". The role is a superuser, which CONNECT privileges do not bind: it can still connect"
			}
		default:
			detail += "login disabled (NOLOGIN) and its sessions ended"
			if d.Superuser {
				detail += " (a superuser role no other application uses)"
			}
		}
		m.archiveKept(archiveID, ArchiveItem{Kind: "database", Name: cmpOr(t.Resource, t.Database), Role: t.Role, SizeBytes: size, Measured: measured, Detail: detail})
	}
	return m.updateArchive(archiveID, func(x *AppArchive) { x.Native.Adopted.Databases = decided })
}

// purgeAdoptedDatabases drops, when the archive is purged, the databases
// its deletion recorded and disabled, and the roles that lost their login,
// when no live application uses them by then. There is no final dump (a
// purge of every other kind takes none either: the deletion removed the
// backups). What it did is recorded in the deleted inventory.
func (m *Manager) purgeAdoptedDatabases(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	if a.Kind != archiveKindAdopted || a.Native == nil || a.Native.Adopted == nil {
		return nil
	}
	own := slices.Clone(a.Databases)
	for resource := range a.Native.Adopted.Records {
		own = append(own, resource)
	}
	databases, roles := m.postgresUsers(a.AppID, own)
	h := nativeHostDataHost(m)
	for _, d := range a.Native.Adopted.Databases {
		name := cmpOr(d.Resource, d.Database)
		if _, done := archiveItemNamed(a.Deleted, "database", name); done {
			continue
		}
		if d.Action != "nologin" && d.Action != "revoke" {
			continue
		}
		t := pgTarget{Port: d.Port, Database: d.Database, Role: d.Role}
		if owner := databases[t.databaseKey()]; owner != "" {
			m.archiveDeleted(archiveID, ArchiveItem{Kind: "database", Name: name, Role: d.Role, Detail: "kept on the host PostgreSQL: " + owner + " uses database " + d.Database + " now"})
			continue
		}
		database, err := pgQuoteIdent(d.Database)
		if err != nil {
			return err
		}
		literal, _ := pgQuoteLiteral(d.Database)
		script := "SELECT count(pg_catalog.pg_terminate_backend(pid)) FROM pg_catalog.pg_stat_activity WHERE datname = " + literal + ";\n" +
			"DROP DATABASE IF EXISTS " + database + " WITH (FORCE);\n"
		if _, err := h.psql(ctx, d.Port, script); err != nil {
			return fmt.Errorf("could not drop the PostgreSQL database %s; retry", d.Database)
		}
		detail := "host PostgreSQL database " + d.Database + " dropped"
		switch {
		case d.Action != "nologin":
			detail += "; role " + d.Role + " kept: shared with " + d.SharedWith
		case d.Role == "postgres":
			detail += "; role postgres kept"
		case roles[t.roleKey()] != "":
			detail += "; role " + d.Role + " kept: " + roles[t.roleKey()] + " uses it now"
		default:
			role, err := pgQuoteIdent(d.Role)
			if err != nil {
				return err
			}
			if _, err := h.psql(ctx, d.Port, "DROP ROLE IF EXISTS "+role+";\n"); err != nil {
				detail += "; role " + d.Role + " kept: it still owns objects or holds privileges in other databases"
			} else {
				detail += "; role " + d.Role + " dropped"
			}
		}
		var size int64
		if item, ok := archiveItemNamed(a.Kept, "database", name); ok {
			size = item.SizeBytes
		}
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "database", Name: name, Role: d.Role, SizeBytes: size, Detail: detail})
	}
	return nil
}

// restoreAdoptedLogins reverses archiveAdoptedLogins.
func (m *Manager) restoreAdoptedLogins(ctx context.Context, adopted *ArchivedAdopted) error {
	h := nativeHostDataHost(m)
	for _, d := range adopted.Databases {
		database, err := pgQuoteIdent(d.Database)
		if err != nil {
			return err
		}
		role, err := pgQuoteIdent(d.Role)
		if err != nil {
			return err
		}
		var script string
		switch d.Action {
		case "nologin":
			script = "ALTER ROLE " + role + " LOGIN;\n"
		case "revoke":
			script = "GRANT CONNECT ON DATABASE " + database + " TO " + role + ";\n"
			if d.PublicConnect {
				script += "GRANT CONNECT ON DATABASE " + database + " TO PUBLIC;\n"
			}
		default:
			continue
		}
		if _, err := h.psql(ctx, d.Port, script); err != nil {
			return errors.New("enabling the PostgreSQL access failed; retry")
		}
	}
	return nil
}

// ---- files and state ---------------------------------------------------------------

// moveTree renames a directory, or copies it and removes the original when
// the two paths are on different file systems.
func (m *Manager) moveTree(ctx context.Context, from, to string) error {
	err := os.Rename(from, to)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	partial := to + ".partial"
	_ = os.RemoveAll(partial)
	if _, err := m.nativeRootExec(ctx, "cp", "-a", "--reflink=auto", "--", from, partial); err != nil {
		_ = os.RemoveAll(partial)
		return fmt.Errorf("could not copy %s: %w", from, err)
	}
	if err := os.Rename(partial, to); err != nil {
		return err
	}
	return os.RemoveAll(from)
}

func (m *Manager) archiveAdoptedFiles(ctx context.Context, archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	root := a.Native.Adopted.Root
	holder := filepath.Join(m.nativeAppsRoot(), ".archived", archiveID)
	target := filepath.Join(holder, "root")
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(target); err == nil {
			return m.updateArchive(archiveID, func(x *AppArchive) { x.Native.Files = target })
		}
		return nil
	}
	if err := os.MkdirAll(holder, 0o700); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Dir(holder), holder} {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	if err := m.moveTree(ctx, root, target); err != nil {
		return fmt.Errorf("could not move the application files: %w", err)
	}
	if err := m.updateArchive(archiveID, func(x *AppArchive) { x.Native.Files = target }); err != nil {
		return err
	}
	m.archiveKept(archiveID, ArchiveItem{Kind: "directory", Name: target, Detail: "application files (" + root + "); the deployment account " + a.Native.Adopted.User + " and its home are unchanged"})
	return nil
}

func (m *Manager) archiveAdoptedState(ctx context.Context, archiveID string) error {
	id := m.archiveSnapshot(archiveID).AppID
	m.mu.Lock()
	for _, table := range []func(){
		func() { delete(m.data.NativeApps, id) }, func() { delete(m.data.NativeSettings, id) }, func() { delete(m.data.NativeReleases, id) },
		func() { delete(m.data.NativeBindings, id) }, func() { delete(m.data.NativeTransitions, id) }, func() { delete(m.data.NativeRuby, id) },
		func() { delete(m.data.BuildPeaks, id) }, func() { delete(m.data.NativeStopped, id) }, func() { delete(m.data.NativeIngress, id) },
	} {
		table()
	}
	for key, t := range m.data.Tasks {
		if t.AppID == id {
			delete(m.data.Tasks, key)
		}
	}
	dropped := m.dropRedirectsLocked(id)
	err := m.save()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if dropped {
		m.withdrawRedirects(ctx, id)
	}
	if m.serviceTLS != nil {
		if err := m.serviceTLS.retire(id); err != nil {
			return errors.New("copied service certificates could not be removed")
		}
	}
	return nil
}

// archiveAdoptedRecords removes the agent's records of the application's
// databases (their backups are deleted by then), keeping them in the
// archive: no backup runs for them while it is deleted.
func (m *Manager) archiveAdoptedRecords(archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	if a.Kind != archiveKindAdopted {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	archive := m.data.AppArchives[archiveID]
	previous := cloneJSON(archive)
	records := map[string]ArchivedDatabaseRecord{}
	maps.Copy(records, archive.Native.Adopted.Records)
	databases := maps.Clone(m.data.Databases)
	bindings := maps.Clone(m.data.NativeDatabases)
	for _, resource := range a.Databases {
		d, ok := m.data.Databases[resource]
		if !ok {
			continue
		}
		record := ArchivedDatabaseRecord{Database: d}
		if b, ok := m.data.NativeDatabases[resource]; ok {
			record.Binding = &b
		}
		records[resource] = record
		delete(m.data.Databases, resource)
		delete(m.data.NativeDatabases, resource)
	}
	archive.Native.Adopted.Records = records
	m.data.AppArchives[archiveID] = archive
	if err := m.save(); err != nil {
		m.data.AppArchives[archiveID] = previous
		m.data.Databases, m.data.NativeDatabases = databases, bindings
		return err
	}
	return nil
}

// ---- restore ---------------------------------------------------------------------

func (m *Manager) restoreAdopted(ctx context.Context, archiveID string, plan ArchiveRestoreResult) error {
	a := m.archiveSnapshot(archiveID)
	id, adopted := a.AppID, a.Native.Adopted
	running := adopted.Stopped == nil
	steps := []struct {
		name string
		run  func() error
	}{
		{"files", func() error {
			files := m.archiveSnapshot(archiveID).Native.Files
			if files == "" {
				return nil
			}
			if _, err := os.Lstat(adopted.Root); err == nil {
				return fmt.Errorf("%s exists; move it away before restoring", adopted.Root)
			}
			if err := m.moveTree(ctx, files, adopted.Root); err != nil {
				return fmt.Errorf("could not move the application files back: %w", err)
			}
			_ = os.Remove(filepath.Dir(files))
			return nil
		}},
		{"leftover-files", func() error {
			return m.restoreLeftoverFiles(ctx, m.archiveSnapshot(archiveID).Native.Leftovers)
		}},
		{"units", func() error {
			dir := m.adoptedUnitDir(adopted.Scope)
			for _, u := range adopted.Units {
				if u.Kept {
					continue
				}
				if filepath.Dir(u.Path) != dir || filepath.Base(u.Path) != u.Name || !nativeReleaseUnitPattern.MatchString(u.Name) {
					return fmt.Errorf("the unit file of %s is outside the unit directory", u.Name)
				}
				if err := os.WriteFile(u.Path, []byte(u.Content), os.FileMode(u.Mode)&0o777); err != nil {
					return fmt.Errorf("could not write the unit file of %s", u.Name)
				}
				if err := os.Chmod(u.Path, os.FileMode(u.Mode)&0o777); err != nil {
					return err
				}
				if err := os.Lchown(u.Path, u.UID, u.GID); err != nil && os.Geteuid() == 0 {
					return fmt.Errorf("could not hand the unit file of %s to its owner", u.Name)
				}
			}
			if len(adopted.Units) == 0 {
				return nil
			}
			if err := m.adoptedUnitCtl(ctx, adopted.Scope, "daemon-reload"); err != nil {
				return errors.New("systemd could not reload the application's units")
			}
			if !running {
				return nil // it was stopped: the stopped state is restored with it
			}
			for _, u := range adopted.Units {
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
				if err := m.adoptedUnitCtl(ctx, adopted.Scope, args...); err != nil {
					return fmt.Errorf("could not start %s: %w", u.Name, err)
				}
			}
			return nil
		}},
		{"schedule", func() error {
			if s := adopted.Schedule; s != nil && running {
				return resumeNativeSchedule(ctx, nativeReleaseCrontab{m: m, user: s.User, identifier: s.Identifier}, s.Entries)
			}
			return nil
		}},
		{"leftover-units", func() error {
			if !running {
				return nil
			}
			return m.restoreLeftoverStates(ctx, m.archiveSnapshot(archiveID).Native.Leftovers, func(scope, name string) bool {
				// Units of the application's own inventory were started above.
				return scope == cmpOr(adopted.Scope, "user") && slices.ContainsFunc(adopted.Units, func(u ArchivedUnit) bool { return u.Name == name })
			})
		}},
		{"logins", func() error { return m.restoreAdoptedLogins(ctx, adopted) }},
		{"routes", func() error {
			caddy, ok := m.nativeCaddy(ctx)
			if !ok {
				return nil
			}
			for i := len(adopted.Routes) - 1; i >= 0; i-- {
				route := adopted.Routes[i]
				if !nativeCaddyServerName.MatchString(route.Server) {
					continue
				}
				if _, err := caddy.call(ctx, http.MethodPut, "/config/apps/http/servers/"+route.Server+"/routes/0", route.Route); err != nil {
					return err
				}
			}
			hosts := plan.Hostnames
			if len(adopted.Routes) > 0 {
				if _, err := caddy.ensureTLSSubjects(ctx, hosts); err != nil {
					return fmt.Errorf("tls automation: %w", err)
				}
			}
			if !running && adopted.Stopped.Route {
				return caddy.publishStoppedRoute(ctx, id, hosts)
			}
			return nil
		}},
		{"error-routes", func() error {
			return m.restoreErrorRoutes(ctx, m.archiveSnapshot(archiveID).Native.Leftovers, plan.Hostnames)
		}},
		{"logger-names", func() error {
			return m.restoreLoggerNames(ctx, m.archiveSnapshot(archiveID).Native.Leftovers, plan.Hostnames)
		}},
		{"reservations", func() error { return m.restoreArchiveReservations(archiveID, plan) }},
		{"state", func() error { return m.restoreAdoptedState(ctx, archiveID, plan) }},
	}
	m.mu.Lock()
	if m.nativeOperations[id] {
		m.mu.Unlock()
		return errors.New("an operation on this application is running")
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[id] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.nativeOperations, id); m.mu.Unlock() }()
	for _, step := range steps {
		if err := m.archiveStep(archiveID, "restore-"+step.name, step.run); err != nil {
			return err
		}
	}
	m.finishArchiveRestore(ctx, archiveID, plan)
	return nil
}

func (m *Manager) restoreAdoptedState(ctx context.Context, archiveID string, plan ArchiveRestoreResult) error {
	m.mu.Lock()
	a := m.data.AppArchives[archiveID]
	native := cloneJSON(*a.Native)
	id := a.AppID
	before := m.resourceSnapshotLocked()
	keep := struct {
		apps      map[string]NativeApplication
		settings  map[string]NativeSettingsOverride
		releases  map[string]NativeReleaseHistory
		bindings  map[string]map[string]string
		stopped   map[string]NativeStoppedState
		databases map[string]Database
		native    map[string]NativeDatabaseBinding
		archives  map[string]AppArchive
	}{maps.Clone(m.data.NativeApps), maps.Clone(m.data.NativeSettings), maps.Clone(m.data.NativeReleases), maps.Clone(m.data.NativeBindings),
		maps.Clone(m.data.NativeStopped), maps.Clone(m.data.Databases), maps.Clone(m.data.NativeDatabases), maps.Clone(m.data.AppArchives)}
	if m.data.NativeApps == nil {
		m.data.NativeApps = map[string]NativeApplication{}
	}
	m.data.NativeApps[id] = native.App
	if native.Settings != nil {
		if m.data.NativeSettings == nil {
			m.data.NativeSettings = map[string]NativeSettingsOverride{}
		}
		m.data.NativeSettings[id] = *native.Settings
	}
	if native.Releases != nil {
		if m.data.NativeReleases == nil {
			m.data.NativeReleases = map[string]NativeReleaseHistory{}
		}
		m.data.NativeReleases[id] = *native.Releases
	}
	if native.Bindings != nil {
		if m.data.NativeBindings == nil {
			m.data.NativeBindings = map[string]map[string]string{}
		}
		m.data.NativeBindings[id] = native.Bindings
	}
	if s := native.Adopted.Stopped; s != nil {
		if m.data.NativeStopped == nil {
			m.data.NativeStopped = map[string]NativeStoppedState{}
		}
		m.data.NativeStopped[id] = *s
	}
	now := time.Now().UTC()
	for resource, record := range native.Adopted.Records {
		if _, taken := m.data.Databases[resource]; taken {
			continue
		}
		d := record.Database
		d.Backups, d.NextBackup = nil, time.Time{}
		if d.Backup.Cron != "" {
			d.NextBackup = d.Backup.NextBackupAfter(now)
		} else if d.Backup.Hours > 0 {
			d.NextBackup = now
		}
		if m.data.Databases == nil {
			m.data.Databases = map[string]Database{}
		}
		m.data.Databases[resource] = d
		if record.Binding != nil {
			if m.data.NativeDatabases == nil {
				m.data.NativeDatabases = map[string]NativeDatabaseBinding{}
			}
			m.data.NativeDatabases[resource] = *record.Binding
		}
	}
	for _, t := range a.Tasks {
		if m.data.Tasks == nil {
			m.data.Tasks = map[string]Task{}
		}
		t.Container, t.Release, t.Running, t.Launching, t.SeenKeys = "", "", false, false, nil
		m.data.Tasks[t.ID] = t
	}
	redirects := restoredRedirects(a, plan)
	if len(redirects) > 0 {
		if m.data.Redirects == nil {
			m.data.Redirects = map[string][]RedirectDomain{}
		}
		m.data.Redirects[id] = redirects
	}
	delete(m.data.AppArchives, archiveID)
	err := m.save()
	if err != nil {
		m.data = before
		m.data.NativeApps, m.data.NativeSettings, m.data.NativeReleases, m.data.NativeBindings = keep.apps, keep.settings, keep.releases, keep.bindings
		m.data.NativeStopped, m.data.Databases, m.data.NativeDatabases, m.data.AppArchives = keep.stopped, keep.databases, keep.native, keep.archives
	}
	m.mu.Unlock()
	if err == nil && len(redirects) > 0 {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, syncErr := m.syncRedirectIngress(bounded, id); syncErr != nil {
			return fmt.Errorf("restored; the redirect domains are published by the next reconcile: %w", syncErr)
		}
	}
	return err
}

// adoptedSnapshotFor returns the snapshot of an adopted application about
// to be deleted; nil for every other application, and when its deletion
// already started (the archive holds the snapshot).
func (m *Manager) adoptedSnapshotFor(ctx context.Context, id string) (*ArchivedAdopted, error) {
	m.mu.Lock()
	_, native := m.data.NativeApps[id]
	_, created := m.data.NativeCreated[id]
	_, container := m.data.Apps[id]
	resumed := false
	for _, a := range m.data.AppArchives {
		if a.AppID == id && a.Phase == archivePhaseDeleting {
			resumed = true
		}
	}
	m.mu.Unlock()
	if !native || created || container || resumed {
		return nil, nil
	}
	return m.adoptedArchiveSnapshot(ctx, id)
}
