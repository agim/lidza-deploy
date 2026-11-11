package agent

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Backup tool re-approval.
//
// Native backups run a host binary (/usr/bin/sqlite3, or pg_dump of the
// cluster's major version) pinned by its SHA-256: the reviewed configuration's
// dump_sha256, or the digest the agent recorded when it provisioned the
// database. A distribution update replaces that binary, and the pin then
// refuses every backup, including the one each deployment takes first.
//
// The agent re-approves the new binary on its own only when the package
// manager vouches for it, all of:
//
//   - the file is a regular, root-owned file that neither group nor others
//     can write;
//   - exactly one package owns the path (dpkg-query -S), with no diversion,
//     and that package is an expected provider of the tool (sqlite3;
//     postgresql-client-<major> or postgresql-<major>);
//   - the file's MD5 equals the package's md5sums record and dpkg --verify
//     reports nothing for the file;
//   - the installed version is offered by a configured apt repository whose
//     Release origin is the distribution archive (Ubuntu or Debian; for
//     PostgreSQL packages also apt.postgresql.org), not only by
//     /var/lib/dpkg/status as a locally installed package is.
//
// The approval is agent state (diskState.BackupToolApprovals), with the hash
// it replaces, the package and version and when; it takes precedence over the
// pin it supersedes. Anything else keeps failing closed, naming the check
// that failed, and the refusal stays visible as a server and database
// warning until the binary matches a pin again.

// BackupToolApproval is one re-approved backup tool binary, by path.
type BackupToolApproval struct {
	SHA256          string    `json:"sha256"`
	PreviousSHA256  string    `json:"previous_sha256"`
	Package         string    `json:"package"`
	Version         string    `json:"version"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	Approved        time.Time `json:"approved"`
	// Supersedes are the pins this approval replaces: the configured or
	// provisioned digest and every earlier approval of the path. A pin
	// outside this list (a reviewed configuration re-pinned by hand to a
	// different binary) is used as it is.
	Supersedes []string `json:"supersedes"`
}

const backupToolApprovalHistory = 32

// nativeBackupToolRootUID is the owner a trusted tool must have; tests use
// their own uid for fixture files.
var nativeBackupToolRootUID uint32

// nativeBackupToolMu serializes checks, so a backup and the maintenance hook
// never approve the same binary twice.
var nativeBackupToolMu sync.Mutex

// backupToolProblem is a pinned tool that could not be re-approved: the
// pins it fails and why. Kept in memory; every check recomputes it.
type backupToolProblem struct {
	Message string
	Pins    map[string]bool
}

// nativeBackupTool returns the tool path a native database policy pins and
// whether a package name is an expected provider of it. ok is false for
// policies without a tool (ClickHouse backs itself up).
func nativeBackupTool(p NativeDatabasePolicy) (path string, provider func(string) bool, ok bool) {
	if p.ClickHouseDatabase != "" || !nativeHexDigest(p.DumpSHA256, 64) {
		return "", nil, false
	}
	if p.SQLitePath != "" {
		return nativeSQLiteTool, func(name string) bool { return name == "sqlite3" }, true
	}
	if p.PostgresMajor < nativePGMinMajor || p.PostgresMajor > nativePGMaxMajor {
		return "", nil, false
	}
	major := strconv.Itoa(p.PostgresMajor)
	return nativePGDumpPath(p.PostgresMajor), func(name string) bool {
		return name == "postgresql-client-"+major || name == "postgresql-"+major
	}, true
}

func backupToolProviders(path string) string {
	if path == nativeSQLiteTool {
		return "sqlite3"
	}
	parts := strings.Split(path, "/")
	if len(parts) > 4 {
		return "postgresql-client-" + parts[4] + " or postgresql-" + parts[4]
	}
	return "its expected package"
}

// backupToolPinLocked is the digest a tool at path is verified against for a
// policy pinned to configured. Caller holds m.mu.
func (m *Manager) backupToolPinLocked(path, configured string) string {
	a, ok := m.data.BackupToolApprovals[path]
	if ok && nativeHexDigest(a.SHA256, 64) && (a.SHA256 == configured || slices.Contains(a.Supersedes, configured)) {
		return a.SHA256
	}
	return configured
}

// nativeBackupToolPin returns the digest a backup of p must verify its tool
// against, re-approving a package update of the tool when every check
// passes. note is the re-approval message, empty when nothing changed.
func (m *Manager) nativeBackupToolPin(ctx context.Context, p NativeDatabasePolicy) (pin, note string, err error) {
	path, provider, ok := nativeBackupTool(p)
	if !ok {
		return p.DumpSHA256, "", nil
	}
	nativeBackupToolMu.Lock()
	defer nativeBackupToolMu.Unlock()
	m.mu.Lock()
	pin = m.backupToolPinLocked(path, p.DumpSHA256)
	m.mu.Unlock()
	digest, sum, err := inspectNativeBackupTool(path)
	if errors.Is(err, os.ErrNotExist) {
		// A missing tool is refused by the backup itself.
		return pin, "", nil
	}
	if err != nil {
		return "", "", m.refuseBackupTool(path, pin, fmt.Sprintf("backup tool %s is not trusted: %v", path, err))
	}
	if digest == pin {
		m.clearBackupToolProblem(path, pin)
		m.clearStaleBackupToolErrors(path, pin)
		return pin, "", nil
	}
	pkg, version, err := verifyNativeBackupToolPackage(ctx, path, sum, provider)
	if err != nil {
		return "", "", m.refuseBackupTool(path, pin, fmt.Sprintf("backup tool %s changed and was not re-approved: %v; review the binary before backups resume", path, err))
	}
	m.mu.Lock()
	previous, had := m.data.BackupToolApprovals[path]
	approval := BackupToolApproval{SHA256: digest, PreviousSHA256: pin, Package: pkg, Version: version, Approved: time.Now().UTC(),
		PreviousVersion: dpkgLogPreviousVersion(pkg, version)}
	if approval.PreviousVersion == "" && had && previous.SHA256 == pin {
		approval.PreviousVersion = previous.Version
	}
	if had {
		approval.Supersedes = slices.Clone(previous.Supersedes)
		approval.Supersedes = append(approval.Supersedes, previous.SHA256)
	}
	approval.Supersedes = append(approval.Supersedes, p.DumpSHA256, pin)
	approval.Supersedes = compactBackupToolPins(approval.Supersedes, digest)
	if m.data.BackupToolApprovals == nil {
		m.data.BackupToolApprovals = map[string]BackupToolApproval{}
	}
	m.data.BackupToolApprovals[path] = approval
	if err = m.save(); err != nil {
		if had {
			m.data.BackupToolApprovals[path] = previous
		} else {
			delete(m.data.BackupToolApprovals, path)
		}
		m.mu.Unlock()
		return "", "", fmt.Errorf("backup tool %s re-approval could not be saved: %w", path, err)
	}
	delete(m.backupToolProblems, path)
	m.clearStaleBackupToolErrorsLocked(path, digest)
	m.mu.Unlock()
	from := approval.PreviousVersion
	if from == "" {
		from = "earlier"
	}
	note = fmt.Sprintf("backup tool %s updated by %s %s→%s: re-approved (sha256 %s→%s)", path, pkg, from, version, shortDigest(pin), shortDigest(digest))
	log.Print(note)
	return digest, note, nil
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// compactBackupToolPins keeps unique valid pins other than current, newest
// last, bounded.
func compactBackupToolPins(pins []string, current string) []string {
	out := []string{}
	for i := len(pins) - 1; i >= 0; i-- {
		if pin := pins[i]; nativeHexDigest(pin, 64) && pin != current && !slices.Contains(out, pin) {
			out = append(out, pin)
		}
	}
	if len(out) > backupToolApprovalHistory {
		out = out[:backupToolApprovalHistory]
	}
	slices.Reverse(out)
	return out
}

func (m *Manager) refuseBackupTool(path, pin, message string) error {
	m.mu.Lock()
	if m.backupToolProblems == nil {
		m.backupToolProblems = map[string]backupToolProblem{}
	}
	problem := m.backupToolProblems[path]
	if problem.Pins == nil {
		problem.Pins = map[string]bool{}
	}
	problem.Pins[pin] = true
	problem.Message = message
	m.backupToolProblems[path] = problem
	m.mu.Unlock()
	return errors.New(message)
}

func (m *Manager) clearBackupToolProblem(path, pin string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	problem, ok := m.backupToolProblems[path]
	if !ok {
		return
	}
	delete(problem.Pins, pin)
	if len(problem.Pins) == 0 {
		delete(m.backupToolProblems, path)
	}
}

// backupToolErrorMarkers are the refusals a backup records when its tool
// check fails (openNativeBackupTool and refuseBackupTool); an error naming
// one of them came from the tool check and nothing else.
var backupToolErrorMarkers = []string{
	"native backup tool changed; review its binary before backup",
	"native backup tool requires an approved binary hash",
}

// staleBackupToolError reports whether a database error came from the check
// of the tool at path.
func staleBackupToolError(message, path string) bool {
	if message == "" {
		return false
	}
	for _, marker := range backupToolErrorMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return strings.Contains(message, "backup tool "+path+" is not trusted:") ||
		strings.Contains(message, "backup tool "+path+" changed and was not re-approved:")
}

func (m *Manager) clearStaleBackupToolErrors(path, digest string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearStaleBackupToolErrorsLocked(path, digest)
}

// clearStaleBackupToolErrorsLocked clears the backup errors the tool check
// left on databases whose tool at path now verifies against digest (it
// matched its pin or was re-approved). Errors of any other kind stay.
// Caller holds m.mu.
func (m *Manager) clearStaleBackupToolErrorsLocked(path, digest string) {
	changed := map[string]Database{}
	for id, binding := range m.data.NativeDatabases {
		toolPath, _, ok := nativeBackupTool(binding.Policy)
		if !ok || toolPath != path || m.backupToolPinLocked(path, binding.Policy.DumpSHA256) != digest {
			continue
		}
		d, found := m.data.Databases[id]
		if !found || !staleBackupToolError(d.Error, path) {
			continue
		}
		changed[id] = d
		d.Error, d.Event = "", newID()
		m.data.Databases[id] = d
	}
	if len(changed) == 0 {
		return
	}
	if err := m.save(); err != nil {
		for id, d := range changed {
			m.data.Databases[id] = d
		}
		log.Printf("backup tool %s verified; clearing its stale database errors could not be saved: %v", path, err)
	}
}

// backupToolWarningLocked is the refusal affecting a database's backups, if
// any. Caller holds m.mu.
func (m *Manager) backupToolWarningLocked(p NativeDatabasePolicy) string {
	path, _, ok := nativeBackupTool(p)
	if !ok {
		return ""
	}
	problem, found := m.backupToolProblems[path]
	if !found || !problem.Pins[m.backupToolPinLocked(path, p.DumpSHA256)] {
		return ""
	}
	return problem.Message
}

// backupToolHealth lists each refused tool with the databases whose backups
// it stops, for the server warnings.
func (m *Manager) backupToolHealth() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	affected := map[string][]string{}
	for id, binding := range m.data.NativeDatabases {
		if !m.nativeDatabaseInUseLocked(id) {
			continue
		}
		if path, _, ok := nativeBackupTool(binding.Policy); ok && m.backupToolWarningLocked(binding.Policy) != "" {
			affected[path] = append(affected[path], id)
		}
	}
	paths := make([]string, 0, len(affected))
	for path := range affected {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var out []string
	for _, path := range paths {
		ids := affected[path]
		sort.Strings(ids)
		out = append(out, fmt.Sprintf("%s. Backups of %s fail, and so do deployments that back them up first.", strings.TrimSuffix(m.backupToolProblems[path].Message, "."), strings.Join(ids, ", ")))
	}
	return out
}

// checkNativeBackupTools re-checks every pinned tool of the databases in use
// against the same rules a backup applies, so the first backup after a
// package update finds the tool approved. It returns one line per
// re-approval or refusal.
func (m *Manager) checkNativeBackupTools(ctx context.Context) []string {
	m.mu.Lock()
	type check struct{ path, pin string }
	seen := map[check]bool{}
	var policies []NativeDatabasePolicy
	ids := make([]string, 0, len(m.data.NativeDatabases))
	for id := range m.data.NativeDatabases {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		binding := m.data.NativeDatabases[id]
		path, _, ok := nativeBackupTool(binding.Policy)
		if !ok || !m.nativeDatabaseInUseLocked(id) {
			continue
		}
		key := check{path, binding.Policy.DumpSHA256}
		if !seen[key] {
			seen[key] = true
			policies = append(policies, binding.Policy)
		}
	}
	m.mu.Unlock()
	var lines []string
	for _, p := range policies {
		_, note, err := m.nativeBackupToolPin(ctx, p)
		if err != nil {
			lines = append(lines, err.Error())
		} else if note != "" {
			lines = append(lines, note)
		}
	}
	return lines
}

// backupToolTick re-checks the tools when the dpkg database changed (an
// unattended upgrade, an operator's apt run) and hourly otherwise.
func (m *Manager) backupToolTick(now time.Time) {
	var changed time.Time
	if info, err := os.Stat(hostPath("/var/lib/dpkg/status")); err == nil {
		changed = info.ModTime()
	}
	m.mu.Lock()
	due := m.backupToolChecked.IsZero() || now.Sub(m.backupToolChecked) >= time.Hour || !changed.Equal(m.backupToolDpkg)
	if due {
		m.backupToolChecked, m.backupToolDpkg = now, changed
	}
	m.mu.Unlock()
	if !due {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
	defer cancel()
	for _, line := range m.checkNativeBackupTools(ctx) {
		if !strings.Contains(line, ": re-approved") { // re-approvals log themselves
			log.Print(line)
		}
	}
}

// inspectNativeBackupTool returns the SHA-256 and MD5 of a tool that is a
// regular root-owned file nobody else can write.
func inspectNativeBackupTool(path string) (sha, sum string, err error) {
	file, err := os.OpenFile(hostPath(path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	if err != nil {
		return "", "", errors.New("the file cannot be opened without following a link")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return "", "", errors.New("it is not a regular file of a reviewable size")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != nativeBackupToolRootUID {
		return "", "", errors.New("it is not owned by root")
	}
	if info.Mode()&0022 != 0 {
		return "", "", errors.New("it is group or world writable")
	}
	shaHash, md5Hash := sha256.New(), md5.New()
	n, err := io.Copy(io.MultiWriter(shaHash, md5Hash), io.LimitReader(file, (64<<20)+1))
	if err != nil || n != info.Size() {
		return "", "", errors.New("it could not be read completely")
	}
	return hex.EncodeToString(shaHash.Sum(nil)), hex.EncodeToString(md5Hash.Sum(nil)), nil
}

// verifyNativeBackupToolPackage applies the package checks to a changed
// tool and returns the owning package and its installed version. The error
// names the check that failed.
func verifyNativeBackupToolPackage(ctx context.Context, path, sum string, provider func(string) bool) (pkg, version string, err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := hostCommand(ctx, "dpkg-query", "-S", path)
	if err != nil {
		return "", "", errors.New("dpkg lists no package that owns it")
	}
	var owners []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "diversion ") {
			return "", "", errors.New("dpkg diverts it to another file")
		}
		names, file, found := strings.Cut(line, ": ")
		if !found || file != path {
			continue
		}
		for _, name := range strings.Split(names, ", ") {
			owners = append(owners, strings.TrimSpace(name))
		}
	}
	if len(owners) != 1 {
		return "", "", fmt.Errorf("dpkg lists %d packages owning it, not exactly one", len(owners))
	}
	pkg = owners[0]
	name, _, _ := strings.Cut(pkg, ":")
	if !provider(name) {
		return "", "", fmt.Errorf("its package %s is not an expected provider (%s)", name, backupToolProviders(path))
	}
	recorded := ""
	for _, control := range []string{pkg, name} {
		raw, readErr := os.ReadFile(hostPath("/var/lib/dpkg/info/" + control + ".md5sums"))
		if readErr != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if fields := strings.Fields(line); len(fields) == 2 && "/"+fields[1] == path {
				recorded = strings.ToLower(fields[0])
			}
		}
		break
	}
	if recorded == "" {
		return "", "", fmt.Errorf("package %s records no checksum for it", name)
	}
	if recorded != sum {
		return "", "", fmt.Errorf("its MD5 differs from package %s's md5sums record", name)
	}
	out, err = hostCommand(ctx, "dpkg", "--verify", pkg)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), " "+path) {
			return "", "", fmt.Errorf("dpkg --verify %s reports it modified (%s)", name, strings.TrimSpace(line))
		}
	}
	if err != nil && strings.TrimSpace(out) == "" {
		return "", "", fmt.Errorf("dpkg --verify %s could not run", name)
	}
	out, err = hostCommand(ctx, "apt-cache", "policy", pkg)
	if err != nil {
		return "", "", fmt.Errorf("apt-cache policy %s failed", name)
	}
	policies := parseAptPolicy(out)
	policy, found := policies[pkg]
	if !found {
		policy, found = policies[name]
	}
	if !found || policy.Installed == "" {
		return "", "", fmt.Errorf("apt reports package %s as not installed", name)
	}
	version = policy.Installed
	if len(policy.InstalledSources) == 0 {
		return "", "", fmt.Errorf("installed version %s of %s is not offered by any configured apt repository (a locally installed package)", version, name)
	}
	out, err = hostCommand(ctx, "apt-cache", "policy")
	if err != nil {
		return "", "", errors.New("apt-cache policy failed")
	}
	releases := map[aptSource]aptRelease{}
	for _, r := range parseAptReleases(out) {
		releases[aptSource{URL: r.URL, Suite: r.Suite}] = r
	}
	var origins []string
	for _, source := range policy.InstalledSources {
		r, known := releases[source]
		if known && trustedBackupToolOrigin(name, r.Origin) {
			return pkg, version, nil
		}
		origin := "unknown origin"
		if known && r.Origin != "" {
			origin = "origin " + r.Origin
		}
		origins = append(origins, source.URL+" "+source.Suite+" ("+origin+")")
	}
	return "", "", fmt.Errorf("installed version %s of %s comes from %s, not the distribution archive", version, name, strings.Join(origins, ", "))
}

// trustedBackupToolOrigin reports whether a repository's Release origin is
// the distribution archive (any mirror serves the signed Release with the
// same origin), or the PostgreSQL project's repository for its packages.
func trustedBackupToolOrigin(pkg, origin string) bool {
	switch origin {
	case "Ubuntu", "Debian":
		return true
	case "apt.postgresql.org":
		return strings.HasPrefix(pkg, "postgresql-")
	}
	return false
}

// dpkgLogPreviousVersion finds the version an upgrade to version replaced,
// from dpkg's log; empty when the log does not say.
func dpkgLogPreviousVersion(pkg, version string) string {
	raw, err := os.ReadFile(hostPath("/var/log/dpkg.log"))
	if err != nil {
		return ""
	}
	name, _, _ := strings.Cut(pkg, ":")
	previous := ""
	for _, line := range strings.Split(string(raw), "\n") {
		// 2026-11-06 06:12:01 upgrade sqlite3:amd64 3.45.1-1ubuntu2.8 3.45.1-1ubuntu2.9
		fields := strings.Fields(line)
		if len(fields) == 6 && fields[2] == "upgrade" && fields[5] == version {
			if owner, _, _ := strings.Cut(fields[3], ":"); owner == name {
				previous = fields[4]
			}
		}
	}
	return previous
}
