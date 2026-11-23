package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// database.yml re-approval.
//
// An adopted Rails database is pinned to the SHA-256 of the application's
// config/database.yml (the policy's config_sha256): the file decides which
// variable each database reads, and so which database the backup copies.
// Any deployment that touches the file (a pool size, a timeout) breaks the
// pin, and the next backup, including the one each deployment takes first,
// is refused.
//
// The agent re-approves a changed file on its own when the database
// identity it declares is unchanged. Both the approved file and the new one
// are read as data (rails_database_yml.go; Ruby never runs) and compared,
// for every production database, on url (the variable it reads),
// adapter, host, port, database name or SQLite path and user name. When
// they match, the new file's hash and content are recorded with the time,
// release and changed settings (diskState.DatabaseConfigApprovals) and take
// precedence over the pin they supersede; the backup goes ahead, which still
// requires the policy's variable to hold the adopted connection. When they
// differ, or the file computes one of them with Ruby, the backup keeps being
// refused with what changed, shown as a database and server warning until
// the file matches an approval again.
//
// The approved file is the copy recorded at adoption or at the last
// re-approval; for a database adopted before copies were kept, the newest
// release whose database.yml has the pinned hash.

// DatabaseConfigApproval is the approved database.yml of one application.
type DatabaseConfigApproval struct {
	SHA256         string    `json:"sha256"`
	PreviousSHA256 string    `json:"previous_sha256,omitempty"`
	Config         string    `json:"config"`
	Release        string    `json:"release,omitempty"`
	Changed        []string  `json:"changed,omitempty"` // settings that changed at re-approval
	Approved       time.Time `json:"approved"`
	// Supersedes are the pins this approval replaces. A configured pin
	// outside it (a configuration re-pinned by hand) is used as it is.
	Supersedes []string `json:"supersedes,omitempty"`
}

// databaseConfigProblem is a changed database.yml that was not re-approved,
// with the configured pins it fails. Kept in memory; checks recompute it.
type databaseConfigProblem struct {
	Message string
	Pins    map[string]bool
}

// databaseConfigPinLocked is the hash an application's database.yml must
// have for a policy pinned to configured. Caller holds m.mu.
func (m *Manager) databaseConfigPinLocked(appID, configured string) string {
	a, ok := m.data.DatabaseConfigApprovals[appID]
	if ok && nativeHexDigest(a.SHA256, 64) && (a.SHA256 == configured || slices.Contains(a.Supersedes, configured)) {
		return a.SHA256
	}
	return configured
}

// nativeRailsDatabaseConfig checks the current database.yml of a against
// p's pin. It returns the approval to record once the rest of the source
// checks pass: a re-approval, or the first copy of the approved file.
// Caller holds m.databaseConfigMu.
func (m *Manager) nativeRailsDatabaseConfig(a NativeApplication, p NativeDatabasePolicy) (*DatabaseConfigApproval, error) {
	data := m.nativeMetadataFile(filepath.Join(a.Name, "current/config/database.yml"))
	if len(data) == 0 {
		return nil, m.refuseDatabaseConfig(p, a.Name+": database.yml of the current release is missing or unreadable; review needed")
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	m.mu.Lock()
	pin := m.databaseConfigPinLocked(p.AppID, p.ConfigSHA256)
	stored, has := m.data.DatabaseConfigApprovals[p.AppID]
	m.mu.Unlock()
	now := time.Now().UTC()
	if digest == pin {
		if has && stored.SHA256 == pin && stored.Config != "" {
			return nil, nil
		}
		return &DatabaseConfigApproval{SHA256: pin, Config: string(data), Release: m.nativeCurrentRelease(a), Approved: now}, nil
	}
	var reference []byte
	if has && stored.SHA256 == pin && stored.Config != "" {
		reference = []byte(stored.Config)
	} else {
		reference = m.nativeReleaseDatabaseConfig(a, pin)
	}
	if reference == nil {
		return nil, m.refuseDatabaseConfig(p, a.Name+": database.yml changed and no copy of the approved file remains to compare it with; review needed")
	}
	changed, err := compareRailsDatabaseConfig(reference, data)
	if err != nil {
		var change *railsConfigChange
		if errors.As(err, &change) {
			message := a.Name + ": " + change.Message + "; review needed"
			if change.Detail != "" {
				message += " (" + change.Detail + ")"
			}
			return nil, m.refuseDatabaseConfig(p, message)
		}
		return nil, m.refuseDatabaseConfig(p, a.Name+": "+err.Error()+"; review needed")
	}
	approval := &DatabaseConfigApproval{SHA256: digest, PreviousSHA256: pin, Config: string(data), Release: m.nativeCurrentRelease(a), Changed: changed, Approved: now}
	if has {
		approval.Supersedes = append(slices.Clone(stored.Supersedes), stored.SHA256)
	}
	approval.Supersedes = compactBackupToolPins(append(approval.Supersedes, p.ConfigSHA256, pin), digest)
	return approval, nil
}

// recordDatabaseConfig saves an approval nativeRailsDatabaseConfig returned
// and logs a re-approval.
func (m *Manager) recordDatabaseConfig(a NativeApplication, p NativeDatabasePolicy, approval *DatabaseConfigApproval) error {
	m.mu.Lock()
	previous, had := m.data.DatabaseConfigApprovals[p.AppID]
	if m.data.DatabaseConfigApprovals == nil {
		m.data.DatabaseConfigApprovals = map[string]DatabaseConfigApproval{}
	}
	m.data.DatabaseConfigApprovals[p.AppID] = *approval
	if err := m.save(); err != nil {
		if had {
			m.data.DatabaseConfigApprovals[p.AppID] = previous
		} else {
			delete(m.data.DatabaseConfigApprovals, p.AppID)
		}
		m.mu.Unlock()
		if approval.PreviousSHA256 == "" {
			return nil // only the copy of an already approved file
		}
		return fmt.Errorf("%s: database.yml re-approval could not be saved: %w", a.Name, err)
	}
	m.mu.Unlock()
	if approval.PreviousSHA256 != "" {
		release := ""
		if approval.Release != "" {
			release = ", release " + approval.Release
		}
		log.Printf("database.yml for %s changed only %s: re-approved (sha256 %s→%s%s)", a.Name, strings.Join(approval.Changed, ", "), shortDigest(approval.PreviousSHA256), shortDigest(approval.SHA256), release)
	}
	return nil
}

// nativeCurrentRelease is the release directory current points at.
func (m *Manager) nativeCurrentRelease(a NativeApplication) string {
	current, err := filepath.EvalSymlinks(filepath.Join(m.cfg.Native.Root, a.Name, "current"))
	if err != nil {
		return ""
	}
	return filepath.Base(current)
}

// nativeReleaseDatabaseConfig is the database.yml with the given hash in the
// newest release that has one.
func (m *Manager) nativeReleaseDatabaseConfig(a NativeApplication, digest string) []byte {
	entries, err := os.ReadDir(filepath.Join(m.cfg.Native.Root, a.Name, "releases"))
	if err != nil {
		return nil
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for i, name := range names {
		if i >= 100 {
			break
		}
		data := m.nativeMetadataFile(filepath.Join(a.Name, "releases", name, "config/database.yml"))
		if sum := sha256.Sum256(data); len(data) > 0 && hex.EncodeToString(sum[:]) == digest {
			return data
		}
	}
	return nil
}

func (m *Manager) refuseDatabaseConfig(p NativeDatabasePolicy, message string) error {
	m.mu.Lock()
	if m.databaseConfigProblems == nil {
		m.databaseConfigProblems = map[string]databaseConfigProblem{}
	}
	problem := m.databaseConfigProblems[p.AppID]
	if problem.Pins == nil {
		problem.Pins = map[string]bool{}
	}
	problem.Pins[p.ConfigSHA256] = true
	problem.Message = message
	m.databaseConfigProblems[p.AppID] = problem
	m.mu.Unlock()
	return errors.New(message)
}

func (m *Manager) clearDatabaseConfigProblem(p NativeDatabasePolicy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	problem, ok := m.databaseConfigProblems[p.AppID]
	if !ok {
		return
	}
	delete(problem.Pins, p.ConfigSHA256)
	if len(problem.Pins) == 0 {
		delete(m.databaseConfigProblems, p.AppID)
	}
}

// databaseConfigWarningLocked is the refused database.yml affecting a
// database's backups, if any. Caller holds m.mu.
func (m *Manager) databaseConfigWarningLocked(p NativeDatabasePolicy) string {
	problem, ok := m.databaseConfigProblems[p.AppID]
	if !ok || !problem.Pins[p.ConfigSHA256] {
		return ""
	}
	return problem.Message
}

// databaseConfigHealth lists each refused database.yml with the databases
// whose backups it stops, for the server warnings.
func (m *Manager) databaseConfigHealth() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	affected := map[string][]string{}
	for id, binding := range m.data.NativeDatabases {
		if m.nativeDatabaseInUseLocked(id) && m.databaseConfigWarningLocked(binding.Policy) != "" {
			affected[binding.Policy.AppID] = append(affected[binding.Policy.AppID], id)
		}
	}
	var out []string
	for _, app := range railsMapKeys(affected) {
		ids := affected[app]
		sort.Strings(ids)
		out = append(out, fmt.Sprintf("%s. Backups of %s fail, and so do deployments that back them up first.", strings.TrimSuffix(m.databaseConfigProblems[app].Message, "."), strings.Join(ids, ", ")))
	}
	return out
}

// staleDatabaseConfigError reports whether a database error came from the
// database.yml check, in this form or the earlier one.
func staleDatabaseConfigError(message string) bool {
	return strings.Contains(message, ": database.yml ") || strings.Contains(message, "Rails database configuration changed") ||
		strings.Contains(message, "native database source changed")
}

// checkNativeDatabaseConfigs re-checks the database.yml of the adopted Rails
// databases in use (of one application when appID is set) with the rules a
// backup applies, so the first backup after a deployment finds the file
// approved. It clears the backup errors the check left once the source is
// the adopted one again, and returns one line per refusal.
func (m *Manager) checkNativeDatabaseConfigs(appID string) []string {
	m.mu.Lock()
	type candidate struct {
		id     string
		policy NativeDatabasePolicy
	}
	var candidates []candidate
	for _, id := range railsMapKeys(m.data.NativeDatabases) {
		p := m.data.NativeDatabases[id].Policy
		if appID != "" && p.AppID != appID || p.ArchiveDatabase != "" || p.ClickHouseDatabase != "" || p.PluginService != "" ||
			!m.nativeDatabaseInUseLocked(id) || m.nativeHostDataBoundLocked(id, p) {
			continue
		}
		candidates = append(candidates, candidate{id, p})
	}
	m.mu.Unlock()
	var lines []string
	for _, c := range candidates {
		raw, err := m.nativeDatabaseSource(c.policy)
		if err != nil {
			if staleDatabaseConfigError(err.Error()) {
				lines = append(lines, err.Error())
			}
			continue
		}
		m.mu.Lock()
		d, ok := m.data.Databases[c.id]
		if ok && d.URL == raw && staleDatabaseConfigError(d.Error) {
			before := d
			d.Error, d.Event = "", newID()
			m.data.Databases[c.id] = d
			if err := m.save(); err != nil {
				m.data.Databases[c.id] = before
				log.Printf("database.yml of %s verified; clearing its stale backup error could not be saved: %v", c.id, err)
			}
		}
		m.mu.Unlock()
	}
	return lines
}

// databaseConfigTick re-checks hourly, and at start.
func (m *Manager) databaseConfigTick(now time.Time) {
	if m.cfg.Native == nil {
		return
	}
	m.mu.Lock()
	due := m.databaseConfigChecked.IsZero() || now.Sub(m.databaseConfigChecked) >= time.Hour
	if due {
		m.databaseConfigChecked = now
	}
	m.mu.Unlock()
	if !due {
		return
	}
	for _, line := range m.checkNativeDatabaseConfigs("") {
		log.Print(line)
	}
}

// recheckDatabaseConfigsAfterRelease runs the check for an application whose
// deployment published a release, so its next deployment's backup does not
// find the change first.
func (m *Manager) recheckDatabaseConfigsAfterRelease(appID string) {
	if m.cfg.Native == nil {
		return
	}
	for _, line := range m.checkNativeDatabaseConfigs(appID) {
		log.Print(line)
	}
}
