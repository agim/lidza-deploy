package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Per-application access logs.
//
// The host's Caddy writes every request's access entry to the system
// journal, which application accounts cannot read (and must not: it holds
// every application's requests). With the access log on (the default, for
// every application that runs as an account of its own), Caddy also writes
// the entries of the application's own hostnames to
//
//	/var/log/lidza/apps/<id>/access.log
//
// one JSON entry per line, the entry the journal used to show without the
// response headers and TLS details (they repeat on every request and were
// more than half of each entry) and with its own "logger" field
// (http.log.access.lidza-access-<id>). Each request is written once: a
// hostname with its own file no longer goes to the journal as well. The roller keeps
// rotated siblings in the same directory (access-<UTC time>[-<reason>].log.gz),
// at most 50 MiB per file, 30 files and 14 days; Caddy 2.11 and later also
// rotate daily. The directory is owned by Caddy's account with the
// application account's group, mode 2750, and the files are 0640: the
// application's account and root read them, no other application does. The
// application's environment file gets LIDZA_ACCESS_LOG with the path; it is
// removed when the log is switched off.
//
// How Caddy is changed (admin API, the publishing server):
//
//   - logging.logs.lidza-access-<id>: a file log that includes only the
//     logger http.log.access.lidza-access-<id>;
//   - every other log that would accept that logger (the default log, which
//     writes to the journal, first of all) excludes it, so the journal keeps
//     exactly what it has today;
//   - the server's logs.logger_names maps each hostname of the application
//     to lidza-access-<id>, plus any logger an operator named for it; the
//     default logger (the journal) is dropped while the file is on. A hostname that was not logged before gets only
//     the application's name. A server without access logging gets a logs
//     object whose default logger name (lidza_unlogged) no log accepts, so
//     unmapped hostnames stay unlogged.
//
// The agent reconciles all of it once a minute (and right after a change):
// directories and modes, Caddy's configuration, the environment variable,
// and the removal of everything of applications that are switched off,
// deleted or no longer have a hostname. Logger names need Caddy 2.8 and
// the file mode needs 2.9; an older Caddy is reported in the status and
// nothing is changed. Container applications are not covered: their
// processes run inside Docker with accounts the host does not know.

const (
	accessLogPrefix   = "lidza-access-"
	accessLogUnlogged = "lidza_unlogged" // not an application id: ids have no underscore
	accessLogEnv      = "LIDZA_ACCESS_LOG"
	accessLogFileName = "access.log"
	accessLogSizeMiB  = 50
	accessLogKeep     = 30
	accessLogKeepDays = 14
	accessLogFileMode = 0o640
	accessLogDirMode  = os.ModeSetgid | 0o750
	accessLogRootMode = 0o731
)

var (
	// accessLogRoot holds one directory per application; tests point it
	// elsewhere.
	accessLogRoot = "/var/log/lidza/apps"
	// accessLogAutomatic runs the reconcile loop and its wake-ups; tests turn
	// it off and reconcile explicitly.
	accessLogAutomatic = true
	accessLogStart     = 20 * time.Second
	accessLogInterval  = time.Minute
	// accessLogCaddyVersionProbe reads the installed Caddy's version
	// ("v2.10.2 h1:..."); "" when unknown.
	accessLogCaddyVersionProbe = func(ctx context.Context) string {
		if !hostToolAvailable("caddy") {
			return ""
		}
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := hostCommand(bounded, "caddy", "version")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(out)
	}
	// accessLogCaddyOwner is the account Caddy runs as.
	accessLogCaddyOwner = func() (uid, gid int, name string, err error) {
		name = caddyProcessUser()
		if name == "" {
			return 0, 0, "", errors.New("no running Caddy process was found on this server")
		}
		var u *user.User
		if rest, ok := strings.CutPrefix(name, "uid "); ok {
			u, err = user.LookupId(rest)
		} else {
			u, err = user.Lookup(name)
		}
		if err != nil {
			return 0, 0, name, fmt.Errorf("the account Caddy runs as (%s) could not be resolved", name)
		}
		uid, _ = strconv.Atoi(u.Uid)
		gid, _ = strconv.Atoi(u.Gid)
		return uid, gid, u.Username, nil
	}
	// accessLogCaddyDropIn widens a strictly sandboxed Caddy unit.
	accessLogCaddyDropIn = "/etc/systemd/system/caddy.service.d/50-lidza-access-log.conf"
)

var accessLogVersionPattern = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

func accessLogName(id string) string        { return accessLogPrefix + id }
func accessLogNamespace(name string) string { return "http.log.access." + name }
func accessLogDir(id string) string         { return filepath.Join(filepath.Clean(accessLogRoot), id) }
func accessLogPath(id string) string        { return filepath.Join(accessLogDir(id), accessLogFileName) }

// accessLogID returns the application id of one of our logger names.
func accessLogID(name string) (string, bool) {
	id, ok := strings.CutPrefix(name, accessLogPrefix)
	return id, ok && idPattern.MatchString(id)
}

func accessLogOurs(name string) bool {
	_, ok := accessLogID(name)
	return ok || name == accessLogUnlogged
}

// AppAccessLogStatus is an application's access log setting and state.
type AppAccessLogStatus struct {
	// Supported is false for applications without an account of their own
	// (container applications, adopted static publications); Reason says why.
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	Enabled   bool   `json:"enabled"`
	Path      string `json:"path"`
	Directory string `json:"directory"`
	// Env is the variable that names Path in the application's environment.
	Env string `json:"env"`
	// EnvSet reports that the application's environment file sets it.
	EnvSet bool `json:"env_set"`
	// Active reports that Caddy writes the application's entries now.
	Active bool     `json:"active"`
	Hosts  []string `json:"hosts,omitempty"`
	// Account and Group read the files; nobody else but root does.
	Account      string `json:"account,omitempty"`
	Group        string `json:"group,omitempty"`
	CaddyVersion string `json:"caddy_version,omitempty"`
	Files        int    `json:"files"`
	Bytes        int64  `json:"bytes"`
	// Error is the latest reconcile problem; Warning a condition that does
	// not stop the log (a sandbox drop-in waiting for a Caddy restart).
	Error   string     `json:"error,omitempty"`
	Warning string     `json:"warning,omitempty"`
	Checked *time.Time `json:"checked,omitempty"`
	// RestartNote says when running processes see Env.
	RestartNote string `json:"restart_note,omitempty"`
}

// AppAccessLogSettings is the panel's change of the setting.
type AppAccessLogSettings struct {
	Enabled *bool `json:"enabled"`
}

// accessLogResult is what the latest pass found for one application.
type accessLogResult struct {
	Active  bool
	Error   string
	Warning string
	Checked time.Time
	Files   int
	Bytes   int64
	Version string
}

// accessLogSubject is a registered application the pass looks at.
type accessLogSubject struct {
	ID        string
	Hosts     []string
	Enabled   bool
	Reason    string // unsupported when set
	Account   string
	GID       int
	Group     string
	EnvManage bool // release-managed: its environment file carries Env
}

// ---- Caddy configuration (pure) --------------------------------------------------

// caddyNameList reads a logger_names value: a string or an array of strings.
func caddyNameList(v any) []string {
	switch typed := v.(type) {
	case string:
		return []string{typed}
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return slices.Clone(typed)
	}
	return nil
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func anyList(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// caddyLogAccepts mirrors Caddy's CustomLog.loggerAllowed for an entry of
// logger name.
func caddyLogAccepts(include, exclude []string, name string) bool {
	if len(include) == 0 && len(exclude) == 0 {
		return true
	}
	name += "."
	longestAccept, longestReject := 0, 0
	if len(include) > 0 {
		for _, namespace := range include {
			if strings.HasPrefix(name, namespace+".") && len(namespace) > longestAccept {
				longestAccept = len(namespace)
			}
		}
		if longestAccept == 0 {
			return false
		}
	}
	for _, namespace := range exclude {
		if namespace == "*" {
			return false
		}
		if strings.HasPrefix(name, namespace+".") && len(namespace) > longestReject {
			longestReject = len(namespace)
		}
	}
	if longestReject > longestAccept {
		return false
	}
	return longestAccept > longestReject || len(include) == 0 && longestReject == 0
}

// caddyLogValid mirrors Caddy's check of a log's include and exclude lists.
func caddyLogValid(include, exclude []string) error {
	if len(include) == 0 || len(exclude) == 0 {
		return nil
	}
	for _, allow := range include {
		if slices.Contains(exclude, allow) {
			return fmt.Errorf("include and exclude must not intersect, but found %s in both lists", allow)
		}
	}
outer:
	for _, allow := range include {
		for _, deny := range exclude {
			if strings.HasPrefix(allow+".", deny+".") || strings.HasPrefix(deny+".", allow+".") {
				continue outer
			}
		}
		return fmt.Errorf("its include entry %q has no matching exclusion", allow)
	}
	return nil
}

func accessLogClone(v any) any {
	if v == nil {
		return nil
	}
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func childMap(parent map[string]any, keys ...string) map[string]any {
	node := parent
	for _, key := range keys {
		next, _ := node[key].(map[string]any)
		if next == nil {
			return nil
		}
		node = next
	}
	return node
}

// caddyVersionAtLeast compares a version string with major.minor.
func caddyVersionAtLeast(version string, major, minor int) (bool, bool) {
	match := accessLogVersionPattern.FindStringSubmatch(version)
	if match == nil {
		return false, false
	}
	x, _ := strconv.Atoi(match[1])
	y, _ := strconv.Atoi(match[2])
	return x > major || x == major && y >= minor, true
}

// renderAccessLog is the file log of one application. encoder is the
// default log's encoder when it has one, so the entries are encoded as the
// journal's are.
func renderAccessLog(id, version string, encoder any) map[string]any {
	writer := map[string]any{"output": "file", "filename": accessLogPath(id), "mode": fmt.Sprintf("%04o", accessLogFileMode),
		"roll_size_mb": accessLogSizeMiB, "roll_keep": accessLogKeep, "roll_keep_days": accessLogKeepDays, "roll_gzip": true}
	if daily, known := caddyVersionAtLeast(version, 2, 11); known && daily {
		// Nanoseconds: Caddy 2.11.0 reads a plain duration number only.
		writer["roll_interval"] = int64(24 * time.Hour)
	}
	return map[string]any{"writer": writer, "include": []any{accessLogNamespace(accessLogName(id))}, "encoder": accessLogSlimEncoder(encoder)}
}

// accessLogSlimFields are left out of an application's file: the response
// headers and the TLS details repeat on every request and were measured at
// more than half of each entry. What identifies and measures a request
// stays: time, host, method, URI, client address, request headers, status,
// size and duration.
var accessLogSlimFields = []string{"resp_headers", "request>tls"}

// accessLogSlimEncoder wraps the journal's encoder (JSON when it has none) in
// a filter that deletes accessLogSlimFields, keeping the fields a filter
// encoder there already handles.
func accessLogSlimEncoder(encoder any) map[string]any {
	wrap := map[string]any{"format": "json"}
	fields := map[string]any{}
	if e, ok := accessLogClone(encoder).(map[string]any); ok {
		if format, _ := e["format"].(string); format == "filter" {
			if inner, ok := e["wrap"].(map[string]any); ok {
				wrap = inner
			}
			if existing, ok := e["fields"].(map[string]any); ok {
				fields = existing
			}
		} else {
			wrap = e
		}
	}
	for _, name := range accessLogSlimFields {
		fields[name] = map[string]any{"filter": "delete"}
	}
	return map[string]any{"format": "filter", "wrap": wrap, "fields": fields}
}

// accessLogEncoder is the default log's JSON encoder, nil when it has none
// (Caddy then encodes JSON the same way for the journal and the file).
func accessLogEncoder(logging map[string]any) any {
	def := childMap(logging, "logs", "default")
	encoder, _ := def["encoder"].(map[string]any)
	if format, _ := encoder["format"].(string); format == "json" || format == "filter" {
		return encoder
	}
	return nil
}

// accessLogLogging returns logging with exactly the logs of defs (by
// application id) and the exclusions every other log needs so that it
// keeps receiving what it receives without them. unlogged keeps the
// exclusion of the default logger name the agent set on a server that had
// no access logging. It never changes anything else.
func accessLogLogging(current map[string]any, defs map[string]map[string]any, unlogged bool) (map[string]any, bool, error) {
	next, _ := accessLogClone(current).(map[string]any)
	if next == nil {
		next = map[string]any{}
	}
	logs, _ := next["logs"].(map[string]any)
	if logs == nil {
		logs = map[string]any{}
	}
	for name := range logs {
		if id, ok := accessLogID(name); ok && defs[id] == nil {
			delete(logs, name)
		}
	}
	names := []string{}
	for _, id := range slices.Sorted(maps.Keys(defs)) {
		logs[accessLogName(id)] = defs[id]
		names = append(names, accessLogNamespace(accessLogName(id)))
	}
	if unlogged {
		names = append(names, accessLogNamespace(accessLogUnlogged))
	}
	if _, found := logs["default"]; !found && len(names) > 0 {
		logs["default"] = map[string]any{}
	}
	var problems []error
	for _, key := range slices.Sorted(maps.Keys(logs)) {
		if _, ours := accessLogID(key); ours {
			continue
		}
		entry, _ := logs[key].(map[string]any)
		if entry == nil {
			continue
		}
		include := stringList(entry["include"])
		exclude := slices.DeleteFunc(stringList(entry["exclude"]), func(namespace string) bool {
			name, ok := strings.CutPrefix(namespace, "http.log.access.")
			return ok && accessLogOurs(name)
		})
		base := slices.Clone(exclude)
		for _, name := range names {
			if caddyLogAccepts(include, base, name) {
				exclude = append(exclude, name)
			}
		}
		if err := caddyLogValid(include, exclude); err != nil {
			problems = append(problems, fmt.Errorf("the Caddy log %q cannot exclude the application logs: %v", key, err))
			continue
		}
		before := stringList(entry["exclude"])
		if slices.Equal(before, exclude) {
			continue
		}
		if len(exclude) == 0 {
			delete(entry, "exclude")
		} else {
			entry["exclude"] = anyList(exclude)
		}
		if key == "default" && len(entry) == 0 {
			delete(logs, key)
		}
	}
	if err := errors.Join(problems...); err != nil {
		return current, false, err
	}
	if len(logs) == 0 {
		delete(next, "logs")
	} else {
		next["logs"] = logs
	}
	if current == nil && len(next) == 0 {
		return nil, false, nil
	}
	return next, !jsonEqual(mustJSON(current), mustJSON(next)), nil
}

// caddyHostLogged reports whether Caddy logs requests for host under logs
// (Server.shouldLogRequest) and with which logger names
// (ServerLogConfig.getLoggerHosts).
func caddyHostLogged(logs map[string]any, host string) (bool, []string) {
	if logs == nil {
		return false, nil
	}
	names, _ := logs["logger_names"].(map[string]any)
	if value, found := names[host]; found {
		return true, caddyNameList(value)
	}
	mapped := false
	for key := range names {
		if caddySubjectsCover([]string{key}, host) {
			mapped = true
		}
	}
	if !mapped {
		for _, skip := range stringList(logs["skip_hosts"]) {
			if caddySubjectsCover([]string{skip}, host) {
				return false, nil
			}
		}
		if skip, _ := logs["skip_unmapped_hosts"].(bool); skip {
			return false, nil
		}
	}
	labels := strings.Split(host, ".")
	for i := range labels {
		if labels[i] == "" {
			continue
		}
		labels[i] = "*"
		if value, found := names[strings.Join(labels, ".")]; found {
			return true, caddyNameList(value)
		}
	}
	name, _ := logs["default_logger_name"].(string)
	return true, []string{name}
}

// accessLogServerLogs returns a server's logs object with every hostname of
// hosts (hostname to application id) also logged to the application's
// logger and our names removed from every other hostname. Entries are
// otherwise kept exactly; a hostname left with no name loses its entry.
func accessLogServerLogs(current map[string]any, hosts map[string]string) (map[string]any, bool) {
	if current == nil && len(hosts) == 0 {
		return nil, false
	}
	next, _ := accessLogClone(current).(map[string]any)
	if next == nil {
		// Nothing was logged on this server; unmapped hostnames go to a
		// logger no log accepts, so they stay unlogged.
		next = map[string]any{"default_logger_name": accessLogUnlogged}
	}
	names, _ := next["logger_names"].(map[string]any)
	if names == nil {
		names = map[string]any{}
	}
	for host, value := range names {
		list := caddyNameList(value)
		kept := slices.DeleteFunc(slices.Clone(list), accessLogOurs)
		if id, wanted := hosts[host]; wanted {
			kept = append(accessLogWithoutJournal(kept), accessLogName(id))
		}
		switch {
		case slices.Equal(kept, list):
		case len(kept) == 0:
			delete(names, host)
		default:
			names[host] = anyList(kept)
		}
	}
	for _, host := range slices.Sorted(maps.Keys(hosts)) {
		if _, found := names[host]; found {
			continue
		}
		_, before := caddyHostLogged(current, host)
		kept := accessLogWithoutJournal(slices.DeleteFunc(slices.Clone(before), accessLogOurs))
		names[host] = anyList(append(kept, accessLogName(hosts[host])))
	}
	if len(names) == 0 {
		delete(next, "logger_names")
	} else {
		next["logger_names"] = names
	}
	return next, !jsonEqual(mustJSON(current), mustJSON(next))
}

// accessLogWithoutJournal drops the default logger (the journal) from the
// loggers of a hostname that has its own file: each request is written once.
// Loggers an operator named stay. With the application's log switched off
// its hostname is unmapped again and logs to the journal as before.
func accessLogWithoutJournal(names []string) []string {
	return slices.DeleteFunc(names, func(name string) bool { return name == "" || name == "default" })
}

// accessLogMapped lists, by application id, the hostnames a server's logs
// map to our loggers.
func accessLogMapped(logs map[string]any) map[string][]string {
	out := map[string][]string{}
	names, _ := logs["logger_names"].(map[string]any)
	for _, host := range slices.Sorted(maps.Keys(names)) {
		for _, name := range caddyNameList(names[host]) {
			if id, ok := accessLogID(name); ok && !slices.Contains(out[id], host) {
				out[id] = append(out[id], host)
			}
		}
	}
	return out
}

// accessLogDefined lists the application logs of a logging object.
func accessLogDefined(logging map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for name, value := range childMap(logging, "logs") {
		if id, ok := accessLogID(name); ok {
			if def, _ := value.(map[string]any); def != nil {
				out[id] = def
			}
		}
	}
	return out
}

// ---- files -----------------------------------------------------------------

// ensureAccessLogDir creates or repairs an application's directory: the
// parents traversable (the apps directory writable by Caddy's group so a
// Caddy restarting from its saved configuration can recreate a missing
// directory), the directory Caddy's with the application's group, setgid,
// and every file in it 0640 with that group. It returns the file count and
// size.
func ensureAccessLogDir(id string, caddyUID, caddyGID, appGID int) (int, int64, error) {
	if !idPattern.MatchString(id) {
		return 0, 0, errors.New("invalid application id")
	}
	root := filepath.Clean(accessLogRoot)
	self := os.Geteuid()
	if err := ensureAccessLogNode(filepath.Dir(root), self, -1, 0o755); err != nil {
		return 0, 0, err
	}
	if err := ensureAccessLogNode(root, self, caddyGID, accessLogRootMode); err != nil {
		return 0, 0, err
	}
	dir := accessLogDir(id)
	if err := ensureAccessLogNode(dir, caddyUID, appGID, accessLogDirMode); err != nil {
		return 0, 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	files, size := 0, int64(0)
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files++
		size += info.Size()
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Gid) != appGID {
			if err := os.Lchown(path, -1, appGID); err != nil {
				return files, size, fmt.Errorf("%s: %w", path, err)
			}
		}
		if info.Mode().Perm() != accessLogFileMode {
			if err := os.Chmod(path, accessLogFileMode); err != nil {
				return files, size, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	return files, size, nil
}

// ensureAccessLogNode makes path a real directory (never a symlink) with
// owner uid, group gid (-1 leaves it) and mode.
func ensureAccessLogNode(path string, uid, gid int, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a directory", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && (int(stat.Uid) != uid || gid >= 0 && int(stat.Gid) != gid) {
		if err := os.Lchown(path, uid, gid); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if info.Mode()&(os.ModePerm|os.ModeSetgid|os.ModeSetuid|os.ModeSticky) != mode {
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

// removeAccessLogDir deletes an application's directory and its files.
func removeAccessLogDir(id string) (bool, error) {
	if !idPattern.MatchString(id) {
		return false, nil
	}
	dir := accessLogDir(id)
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%s is not a directory", dir)
	}
	return true, os.RemoveAll(dir)
}

// accessLogDirs lists the application directories under the root.
func accessLogDirs() []string {
	entries, err := os.ReadDir(filepath.Clean(accessLogRoot))
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() && idPattern.MatchString(entry.Name()) {
			out = append(out, entry.Name())
		}
	}
	return out
}

// ---- manager -----------------------------------------------------------------------

// accessLogSubjects lists the registered applications the pass considers.
func (m *Manager) accessLogSubjects() map[string]accessLogSubject {
	m.mu.Lock()
	apps := map[string]NativeApplication{}
	for id, a := range m.data.NativeApps {
		apps[id] = a
	}
	off := maps.Clone(m.data.AccessLogOff)
	m.mu.Unlock()
	out := map[string]accessLogSubject{}
	for id, a := range apps {
		s := accessLogSubject{ID: id, Enabled: !off[id], Hosts: accessLogHosts(a)}
		s.Reason, s.Account, s.GID, s.Group, s.EnvManage = m.accessLogAccount(id)
		if s.Reason == "" && len(s.Hosts) == 0 {
			s.Reason = "This application has no hostname."
		}
		out[id] = s
	}
	return out
}

func accessLogHosts(a NativeApplication) []string {
	var hosts []string
	for _, host := range append([]string{a.Domain}, a.AdditionalDomains...) {
		host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
		if host != "" && validLoggerHost(host) && !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// accessLogAccount resolves the account that reads an application's log;
// reason is set when it has none of its own.
func (m *Manager) accessLogAccount(id string) (reason, account string, gid int, group string, release bool) {
	_, policy, err := m.nativeReleasePolicy(id)
	if err != nil {
		return "This application is not deployed by the release engine, so no account of its own runs it on the server.", "", 0, "", false
	}
	owner, err := m.nativeReleaseAccount(policy)
	if err != nil {
		return "The application's account could not be resolved: " + err.Error(), "", 0, "", false
	}
	group = strconv.Itoa(owner.GID)
	if g, err := user.LookupGroupId(group); err == nil {
		group = g.Name
	}
	return "", owner.Name, owner.GID, group, true
}

// accessLogCaddyVersion is the installed Caddy's version, checked at most
// every ten minutes.
func (m *Manager) accessLogCaddyVersion(ctx context.Context) string {
	m.mu.Lock()
	version, checked := m.accessLogVersion, m.accessLogVersionChecked
	m.mu.Unlock()
	if !checked.IsZero() && time.Since(checked) < 10*time.Minute {
		return version
	}
	version = accessLogCaddyVersionProbe(ctx)
	if fields := strings.Fields(version); len(fields) > 0 {
		version = fields[0]
	}
	m.mu.Lock()
	m.accessLogVersion, m.accessLogVersionChecked = version, time.Now()
	m.mu.Unlock()
	return version
}

// accessLogSandbox makes sure a Caddy unit that mounts the system read-only
// (ProtectSystem=strict) may write the logs: it adds a drop-in with
// ReadWritePaths for the root, effective from Caddy's next start. The agent
// never restarts Caddy for it; the warning says so.
func (m *Manager) accessLogSandbox(ctx context.Context) string {
	out, err := m.nativeRootExec(ctx, "systemctl", "show", "caddy.service", "--property=LoadState,ProtectSystem,ReadWritePaths,ReadOnlyPaths")
	if err != nil {
		return ""
	}
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[key] = value
		}
	}
	if props["LoadState"] != "loaded" {
		return ""
	}
	root := filepath.Clean(accessLogRoot)
	covers := func(list string) bool {
		for _, path := range strings.Fields(list) {
			path = filepath.Clean(strings.TrimLeft(path, "-+"))
			if root == path || strings.HasPrefix(root, path+"/") {
				return true
			}
		}
		return false
	}
	if props["ProtectSystem"] != "strict" && !covers(props["ReadOnlyPaths"]) || covers(props["ReadWritePaths"]) {
		return ""
	}
	body := "# Managed by Līdza Deploy: per-application access logs (access_log.go).\n[Service]\nReadWritePaths=-" + root + "\n"
	if current, err := os.ReadFile(accessLogCaddyDropIn); err != nil || string(current) != body {
		if err := os.MkdirAll(filepath.Dir(accessLogCaddyDropIn), 0o755); err != nil {
			return "Caddy's service sandbox keeps " + root + " read-only, and the agent could not add a drop-in: " + err.Error()
		}
		if _, err := writeReadableFile(accessLogCaddyDropIn, []byte(body)); err != nil {
			return "Caddy's service sandbox keeps " + root + " read-only, and the agent could not add a drop-in: " + err.Error()
		}
		_, _ = m.nativeRootExec(ctx, "systemctl", "daemon-reload")
	}
	return "Caddy's service sandbox keeps " + root + " read-only until Caddy restarts: the agent added " + accessLogCaddyDropIn + " (ReadWritePaths), which Caddy reads when it next starts. The agent does not restart Caddy."
}

// reconcileAccessLogs is the access log pass over every application.
func (m *Manager) reconcileAccessLogs(ctx context.Context) {
	m.accessLogMu.Lock()
	defer m.accessLogMu.Unlock()
	subjects := m.accessLogSubjects()
	results := map[string]accessLogResult{}
	now := time.Now().UTC()
	fail := func(ids []string, message string) {
		for _, id := range ids {
			r := results[id]
			r.Error, r.Checked = boundedErrorText(message, 400), now
			results[id] = r
		}
	}
	defer func() { m.storeAccessLogResults(results, now) }()
	var wanted []string
	for _, id := range slices.Sorted(maps.Keys(subjects)) {
		s := subjects[id]
		results[id] = accessLogResult{Checked: now}
		if s.Reason == "" && s.Enabled {
			wanted = append(wanted, id)
		}
	}
	if len(subjects) == 0 && len(accessLogDirs()) == 0 {
		return // a host without native applications: nothing to read or write
	}
	caddy, _, ok := m.gatewayCaddy(ctx)
	if !ok {
		fail(wanted, "No Caddy admin API fronts this server, so the agent cannot direct access entries to the application.")
		m.syncAccessLogEnvs(subjects, results)
		return
	}
	version := m.accessLogCaddyVersion(ctx)
	for _, id := range wanted {
		r := results[id]
		r.Version = version
		results[id] = r
	}
	config, err := caddy.readConfig(ctx)
	if err != nil {
		fail(wanted, "The Caddy configuration could not be read: "+err.Error())
		return
	}
	server := childMap(config, "apps", "http", "servers", caddy.cfg.Server)
	if server == nil {
		fail(wanted, "Caddy has no server "+caddy.cfg.Server+".")
		return
	}
	logging, _ := config["logging"].(map[string]any)
	serverLogs, _ := server["logs"].(map[string]any)
	desired := map[string][]string{}
	defs := map[string]map[string]any{}
	tooOld := false
	if supported, known := caddyVersionAtLeast(version, 2, 9); known && !supported {
		tooOld = true
		fail(wanted, "Caddy "+version+" is too old for per-application access logs: they need Caddy 2.9 or later (logger names for several logs came in 2.8, the log file mode in 2.9). Routing is unchanged.")
	} else if uid, gid, _, err := accessLogCaddyOwner(); err != nil {
		fail(wanted, err.Error())
	} else {
		warning := m.accessLogSandbox(ctx)
		encoder := accessLogEncoder(logging)
		for _, id := range wanted {
			s := subjects[id]
			files, size, err := ensureAccessLogDir(id, uid, gid, s.GID)
			r := results[id]
			r.Files, r.Bytes, r.Warning = files, size, warning
			results[id] = r
			if err != nil {
				fail([]string{id}, "The log directory could not be prepared: "+err.Error())
				continue
			}
			desired[id] = s.Hosts
			defs[id] = renderAccessLog(id, version, encoder)
		}
	}
	// A wanted log that failed for a passing reason (Caddy restarting, a
	// directory repair) keeps what Caddy holds for it rather than flapping.
	if !tooOld {
		mapped, defined := accessLogMapped(serverLogs), accessLogDefined(logging)
		for _, id := range wanted {
			if desired[id] == nil && defined[id] != nil && len(mapped[id]) > 0 {
				desired[id], defs[id] = mapped[id], defined[id]
			}
		}
	}
	// What Caddy holds now beyond the desired applications stays until the
	// mappings are gone, so no entry reaches a log without its exclusions.
	if err := m.applyAccessLogs(ctx, caddy, logging, serverLogs, desired, defs); err != nil {
		fail(slices.Sorted(maps.Keys(desired)), err.Error())
		m.syncAccessLogEnvs(subjects, results)
		return
	}
	for id := range desired {
		r := results[id]
		r.Active = true
		results[id] = r
	}
	// Directories of applications that are switched off or gone; Caddy no
	// longer names them.
	for _, id := range accessLogDirs() {
		s, registered := subjects[id]
		if registered && (s.Enabled || s.Reason != "") {
			continue
		}
		if _, err := removeAccessLogDir(id); err != nil {
			log.Printf("access log %s: %v", id, err)
			if registered {
				fail([]string{id}, "The log directory could not be removed: "+err.Error())
			}
		}
	}
	m.syncAccessLogEnvs(subjects, results)
}

// applyAccessLogs writes Caddy's configuration for desired (application id
// to hostnames) in three steps, each only when it changes something: the
// logs and exclusions of desired and of everything still mapped, then the
// server's hostname mappings, then the logs and exclusions of desired only.
func (m *Manager) applyAccessLogs(ctx context.Context, caddy nativeCaddy, logging, serverLogs map[string]any, desired map[string][]string, defs map[string]map[string]any) error {
	hosts := map[string]string{}
	for _, id := range slices.Sorted(maps.Keys(desired)) {
		for _, host := range desired[id] {
			if _, taken := hosts[host]; !taken {
				hosts[host] = id
			}
		}
	}
	nextLogs, logsChanged := accessLogServerLogs(serverLogs, hosts)
	unlogged := func(logs map[string]any) bool {
		name, _ := logs["default_logger_name"].(string)
		return name == accessLogUnlogged
	}
	// Step 1: the union, so a mapping still in place keeps its exclusions.
	union := maps.Clone(defs)
	defined := accessLogDefined(logging)
	for id := range accessLogMapped(serverLogs) {
		if union[id] == nil && defined[id] != nil {
			union[id] = defined[id]
		}
	}
	first, changed, err := accessLogLogging(logging, union, unlogged(serverLogs) || unlogged(nextLogs))
	if err != nil {
		return err
	}
	if changed {
		if _, err := caddy.call(ctx, http.MethodPost, "/config/logging", mustJSON(first)); err != nil {
			return fmt.Errorf("Caddy refused the log configuration: %w", err)
		}
	}
	if logsChanged {
		if _, err := caddy.call(ctx, http.MethodPost, "/config/apps/http/servers/"+caddy.cfg.Server+"/logs", mustJSON(nextLogs)); err != nil {
			return fmt.Errorf("Caddy refused the hostname log mapping: %w", err)
		}
	}
	final, changed, err := accessLogLogging(first, defs, unlogged(nextLogs))
	if err != nil {
		return err
	}
	if changed {
		if _, err := caddy.call(ctx, http.MethodPost, "/config/logging", mustJSON(final)); err != nil {
			return fmt.Errorf("Caddy refused the log configuration: %w", err)
		}
	}
	return nil
}

// withdrawAccessLog removes a deleted application's access log: its
// hostname mappings, its log and exclusions, then its directory. Other
// applications' logs are left exactly as Caddy holds them.
func (m *Manager) withdrawAccessLog(ctx context.Context, archiveID, id string) error {
	m.accessLogMu.Lock()
	defer m.accessLogMu.Unlock()
	m.mu.Lock()
	_, registered := m.data.NativeApps[id]
	if !registered {
		delete(m.accessLogResults, id)
	}
	m.mu.Unlock()
	if registered || !idPattern.MatchString(id) {
		return nil
	}
	caddy, _, ok := m.gatewayCaddy(ctx)
	if !ok {
		// Without the admin API nothing proves Caddy no longer writes there.
		return nil
	}
	{
		config, err := caddy.readConfig(ctx)
		if err != nil {
			return err
		}
		logging, _ := config["logging"].(map[string]any)
		serverLogs, _ := childMap(config, "apps", "http", "servers", caddy.cfg.Server)["logs"].(map[string]any)
		desired := accessLogMapped(serverLogs)
		defs := accessLogDefined(logging)
		delete(desired, id)
		delete(defs, id)
		for other := range desired {
			if defs[other] == nil {
				delete(desired, other)
			}
		}
		if err := m.applyAccessLogs(ctx, caddy, logging, serverLogs, desired, defs); err != nil {
			return err
		}
	}
	removed, err := removeAccessLogDir(id)
	if err != nil {
		return err
	}
	if removed && archiveID != "" {
		m.archiveDeleted(archiveID, ArchiveItem{Kind: "access-log", Name: accessLogDir(id), Detail: "the application's access log files and Caddy's log for its hostnames"})
	}
	return nil
}

// withdrawAccessLogLater is withdrawAccessLog for a deletion: a failure never
// stops the deletion; the reconcile pass removes what is left.
func (m *Manager) withdrawAccessLogLater(ctx context.Context, archiveID, id string) error {
	if err := m.withdrawAccessLog(ctx, archiveID, id); err != nil {
		log.Printf("access log %s: %v (the next pass removes it)", id, err)
		m.wakeAccessLogs()
	}
	return nil
}

// syncAccessLogEnvs keeps LIDZA_ACCESS_LOG in release-managed applications'
// environment files: set while their log is active, removed when switched
// off. A failing pass leaves an existing value alone.
func (m *Manager) syncAccessLogEnvs(subjects map[string]accessLogSubject, results map[string]accessLogResult) {
	for _, id := range slices.Sorted(maps.Keys(subjects)) {
		s := subjects[id]
		if !s.EnvManage {
			continue
		}
		var want *bool
		switch {
		case !s.Enabled:
			no := false
			want = &no
		case s.Reason == "" && results[id].Active:
			yes := true
			want = &yes
		}
		if want == nil {
			continue
		}
		if err := m.syncAccessLogEnv(id, *want); err != nil {
			r := results[id]
			r.Error = boundedErrorText("The application's environment file could not be updated: "+err.Error(), 400)
			results[id] = r
		}
	}
}

func (m *Manager) syncAccessLogEnv(id string, want bool) error {
	m.mu.Lock()
	busy := m.nativeOperations[id]
	m.mu.Unlock()
	if busy {
		return nil // the next pass, after the operation
	}
	_, policy, err := m.nativeReleasePolicy(id)
	if err != nil {
		return nil
	}
	values, err := nativeReleaseEnvValues(policy)
	if err != nil {
		return err
	}
	current, has := values[accessLogEnv]
	path := accessLogPath(id)
	switch {
	case want && current != path:
		return rewriteNativeReleaseEnvFiles(policy.EnvFiles, map[string]*string{accessLogEnv: &path})
	case !want && has:
		return rewriteNativeReleaseEnvFiles(policy.EnvFiles, map[string]*string{accessLogEnv: nil})
	}
	return nil
}

func (m *Manager) storeAccessLogResults(results map[string]accessLogResult, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.accessLogResults == nil {
		m.accessLogResults = map[string]accessLogResult{}
	}
	for id := range m.accessLogResults {
		if _, found := results[id]; !found {
			delete(m.accessLogResults, id)
		}
	}
	for id, r := range results {
		if r.Checked.IsZero() {
			r.Checked = now
		}
		m.accessLogResults[id] = r
	}
}

// accessLogEnvSet reports whether the application's environment names its log.
func (m *Manager) accessLogEnvSet(id string) bool {
	_, policy, err := m.nativeReleasePolicy(id)
	if err != nil {
		return false
	}
	values, err := nativeReleaseEnvValues(policy)
	return err == nil && values[accessLogEnv] == accessLogPath(id)
}

// accessLogStatus is an application's access log as the panel shows it.
func (m *Manager) accessLogStatus(id string) (AppAccessLogStatus, error) {
	status := AppAccessLogStatus{Path: accessLogPath(id), Directory: accessLogDir(id), Env: accessLogEnv,
		RestartNote: "Running processes read " + accessLogEnv + " when they start: after the next restart or deploy. Scheduled jobs read it from their next run."}
	m.mu.Lock()
	_, container := m.data.Apps[id]
	a, native := m.data.NativeApps[id]
	off := m.data.AccessLogOff[id]
	result, checked := m.accessLogResults[id]
	m.mu.Unlock()
	switch {
	case container:
		status.Reason = "Container applications are not covered yet: their processes run inside Docker under accounts the server does not know. Their requests stay in Caddy's system journal."
		return status, nil
	case !native:
		return status, errors.New("unknown application")
	}
	status.Enabled = !off
	status.Hosts = accessLogHosts(a)
	reason, account, _, group, _ := m.accessLogAccount(id)
	if reason == "" && len(status.Hosts) == 0 {
		reason = "This application has no hostname."
	}
	status.Supported, status.Reason, status.Account, status.Group = reason == "", reason, account, group
	if checked {
		status.Active, status.Error, status.Warning, status.Files, status.Bytes, status.CaddyVersion = result.Active, result.Error, result.Warning, result.Files, result.Bytes, result.Version
		at := result.Checked
		status.Checked = &at
	}
	if !status.Supported || !status.Enabled {
		status.Active, status.Error, status.Warning = false, "", ""
		if !status.Enabled {
			status.Error = result.Error
		}
	}
	status.EnvSet = m.accessLogEnvSet(id)
	return status, nil
}

// setAccessLog stores the panel's setting and applies it at once.
func (m *Manager) setAccessLog(ctx context.Context, id string, s AppAccessLogSettings) error {
	if s.Enabled == nil {
		return errors.New("enabled is required")
	}
	m.mu.Lock()
	if _, native := m.data.NativeApps[id]; !native {
		_, container := m.data.Apps[id]
		m.mu.Unlock()
		if container {
			return errors.New("access logs are not available for container applications")
		}
		return errors.New("unknown application")
	}
	previous := m.data.AccessLogOff[id]
	if m.data.AccessLogOff == nil {
		m.data.AccessLogOff = map[string]bool{}
	}
	if *s.Enabled {
		delete(m.data.AccessLogOff, id)
	} else {
		m.data.AccessLogOff[id] = true
	}
	if err := m.save(); err != nil {
		if previous {
			m.data.AccessLogOff[id] = true
		} else {
			delete(m.data.AccessLogOff, id)
		}
		m.mu.Unlock()
		return errors.New("could not save the access log setting")
	}
	m.mu.Unlock()
	m.reconcileAccessLogs(ctx)
	return nil
}

// accessLogOffLocked reports the stored setting, for moves and archives.
func (m *Manager) accessLogOffLocked(id string) bool { return m.data.AccessLogOff[id] }

// setAccessLogOffLocked records a created, restored or moved application's
// setting; the caller saves.
func (m *Manager) setAccessLogOffLocked(id string, off bool) {
	if !off {
		delete(m.data.AccessLogOff, id)
		return
	}
	if m.data.AccessLogOff == nil {
		m.data.AccessLogOff = map[string]bool{}
	}
	m.data.AccessLogOff[id] = true
}

// wakeAccessLogs asks the loop for a pass soon (a hostname or application
// changed).
func (m *Manager) wakeAccessLogs() {
	if m.accessLogWake == nil {
		return
	}
	select {
	case m.accessLogWake <- struct{}{}:
	default:
	}
}

func (m *Manager) accessLogLoop() {
	defer m.wg.Done()
	timer := time.NewTimer(accessLogStart)
	defer timer.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-timer.C:
		case <-m.accessLogWake:
			// Let the change that woke the loop finish first.
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		if accessLogAutomatic && m.hostOperationsAllowed() == nil {
			ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
			m.reconcileAccessLogs(ctx)
			cancel()
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(accessLogInterval)
	}
}

func (m *Manager) accessLogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/apps/{id}/access-log", func(w http.ResponseWriter, r *http.Request) {
		status, err := m.accessLogStatus(r.PathValue("id"))
		if err != nil {
			Fail(w, 404, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, status)
	})
	mux.HandleFunc("PUT /v1/apps/{id}/access-log", func(w http.ResponseWriter, r *http.Request) {
		var s AppAccessLogSettings
		if err := Decode(w, r, &s); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		if err := m.setAccessLog(ctx, r.PathValue("id"), s); err != nil {
			Fail(w, 409, err)
			return
		}
		status, err := m.accessLogStatus(r.PathValue("id"))
		if err != nil {
			Fail(w, 404, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, status)
	})
}
