package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Before-deploy modes: what a release does about a database's backup before
// it changes anything. The operator chooses per database from the panel; the
// platform only keeps a primary database from choosing never.
const (
	// BeforeDeployAlways takes a fresh verified backup before every
	// release. It is the default.
	BeforeDeployAlways = "always"
	// BeforeDeployReuse accepts a verified successful backup (scheduled or
	// manual, with its off-site copy when the gate requires one) newer than
	// MaxAgeHours as the pre-deployment backup, and takes a fresh one
	// otherwise.
	BeforeDeployReuse = "reuse"
	// BeforeDeployNever leaves the database to its scheduled backups. Only
	// a secondary database may choose it.
	BeforeDeployNever = "never"
)

// beforeDeployMaxAgeLimitHours bounds the age a reused backup may have.
const beforeDeployMaxAgeLimitHours = 24 * 30

// BeforeDeployPolicy is one database's pre-deployment backup choice. The zero
// value means always.
type BeforeDeployPolicy struct {
	Mode        string `json:"mode"`
	MaxAgeHours int    `json:"max_age_hours,omitempty"`
}

// Validate accepts always, never, and reuse with a maximum age between one
// hour and thirty days.
func (p BeforeDeployPolicy) Validate() error {
	switch p.Mode {
	case BeforeDeployAlways, BeforeDeployNever:
		if p.MaxAgeHours != 0 {
			return errors.New("a maximum backup age applies to reusing backups only")
		}
	case BeforeDeployReuse:
		if p.MaxAgeHours < 1 || p.MaxAgeHours > beforeDeployMaxAgeLimitHours {
			return fmt.Errorf("reuse backups between 1 and %d hours old", beforeDeployMaxAgeLimitHours)
		}
	default:
		return errors.New("before deploys, choose always, reuse or never")
	}
	return nil
}

// Normalized resolves the zero value, and anything invalid, to always.
func (p BeforeDeployPolicy) Normalized() BeforeDeployPolicy {
	if p.Mode == "" || p.Validate() != nil {
		return BeforeDeployPolicy{Mode: BeforeDeployAlways}
	}
	return p
}

// MaxAge is the oldest a reused backup may be.
func (p BeforeDeployPolicy) MaxAge() time.Duration { return time.Duration(p.MaxAgeHours) * time.Hour }

// String names the policy for logs, proofs and move plans: "always",
// "reuse within 24h" or "never".
func (p BeforeDeployPolicy) String() string {
	p = p.Normalized()
	if p.Mode == BeforeDeployReuse {
		return fmt.Sprintf("reuse within %dh", p.MaxAgeHours)
	}
	return p.Mode
}

// databasePrimaryLocked reports whether a database must be backed up before
// every release: a native primary, analytics or standalone database, or the
// DATABASE_URL of a container application. Caller holds m.mu.
func (m *Manager) databasePrimaryLocked(id string) bool {
	if binding, ok := m.data.NativeDatabases[id]; ok && binding.Policy.alwaysProtected() {
		return true
	}
	for _, a := range m.data.Apps {
		if !a.Retiring && a.Bindings["DATABASE_URL"] == id {
			return true
		}
	}
	return false
}

// beforeDeployPolicyLocked is a database's effective policy: the one the
// panel saved, else the reviewed configuration's initial default (which may
// still use the legacy skip_release_backup flag), else always. A primary
// database never resolves to never, whatever state or configuration say.
// Caller holds m.mu.
func (m *Manager) beforeDeployPolicyLocked(id string) BeforeDeployPolicy {
	policy := m.data.Databases[id].BeforeDeploy
	if policy.Mode == "" {
		source := m.data.NativeDatabases[id].Policy
		if m.cfg.Native != nil {
			if approved, ok := m.cfg.Native.Databases[id]; ok {
				source = approved
			}
		}
		policy, _ = source.configuredBeforeDeploy()
	}
	policy = policy.Normalized()
	if policy.Mode == BeforeDeployNever && m.databasePrimaryLocked(id) {
		return BeforeDeployPolicy{Mode: BeforeDeployAlways}
	}
	return policy
}

// setBeforeDeployPolicy saves a database's policy from the panel.
func (m *Manager) setBeforeDeployPolicy(id string, p BeforeDeployPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data.Databases[id]
	if !ok {
		return errors.New("no database configured")
	}
	if p.Mode == BeforeDeployNever && m.databasePrimaryLocked(id) {
		return errors.New("a primary database is backed up before every deploy; choose always or reuse")
	}
	old := d
	d.BeforeDeploy = p
	m.data.Databases[id] = d
	if err := m.save(); err != nil {
		m.data.Databases[id] = old
		return err
	}
	return nil
}

// beforeDeployDecision is what one release does for one database.
type beforeDeployDecision struct {
	Policy BeforeDeployPolicy
	// Skip is set under never: the release takes no backup of this database.
	Skip bool
	// Reuse is the verified backup that counts under reuse (ID set), and Age
	// how old it was when the release checked.
	Reuse BackupRecord
	Age   time.Duration
	// Reason says why a fresh backup is taken under reuse.
	Reason string
}

// beforeDeployDecisionLocked applies a database's policy to its backup
// history at now. Under reuse a backup counts when it finished with its
// checksum and size recorded, without an upload error, with an off-site
// copy when offsite is required (otherwise its local file must still
// exist), and when it is newer than the policy's maximum age. A database
// whose last operation failed gets a fresh backup. Caller holds m.mu.
func (m *Manager) beforeDeployDecisionLocked(id string, offsite bool, now time.Time) beforeDeployDecision {
	decision := beforeDeployDecision{Policy: m.beforeDeployPolicyLocked(id)}
	switch decision.Policy.Mode {
	case BeforeDeployNever:
		decision.Skip = true
	case BeforeDeployReuse:
		d := m.data.Databases[id]
		if d.Error != "" {
			decision.Reason = "its last database operation failed"
			return decision
		}
		var newest, usable BackupRecord
		for _, record := range d.Backups {
			if !verifiedBackup(record, offsite) {
				continue
			}
			if record.Created.After(newest.Created) {
				newest = record
			}
			if now.Sub(record.Created) >= decision.Policy.MaxAge() || !record.Offsite && !m.localBackupExists(id, record) {
				continue
			}
			if record.Created.After(usable.Created) {
				usable = record
			}
		}
		switch {
		case usable.ID != "":
			decision.Reuse, decision.Age = usable, now.Sub(usable.Created)
		case newest.ID == "" && offsite:
			decision.Reason = "no verified backup with an off-site copy yet"
		case newest.ID == "":
			decision.Reason = "no verified backup yet"
		case now.Sub(newest.Created) >= decision.Policy.MaxAge():
			decision.Reason = fmt.Sprintf("the newest verified backup is %s old", ageText(now.Sub(newest.Created)))
		default:
			decision.Reason = fmt.Sprintf("the newest verified backup (%s) is no longer on this server", newest.ID)
		}
	}
	return decision
}

// verifiedBackup reports whether a record is a finished, checksummed backup
// that uploaded when it had to, with an off-site copy when one is required.
func verifiedBackup(record BackupRecord, offsite bool) bool {
	return record.ID != "" && record.Size > 0 && nativeHexDigest(record.SHA256, 64) && record.UploadError == "" && (!offsite || record.Offsite)
}

// localBackupExists reports whether a backup's dump is still a nonempty
// regular file on this server.
func (m *Manager) localBackupExists(id string, record BackupRecord) bool {
	if record.ID == "" || filepath.Base(record.ID) != record.ID {
		return false
	}
	info, err := os.Lstat(filepath.Join(m.backupDir(id), record.ID+".dump"))
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// ageText says how old a backup is, as the deploy log shows it: 45m, 2h, 3d.
func ageText(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// logBeforeDeployDecision tells the deploy log what the policy decided for
// a database that is not skipped: which backup is reused, or why a fresh
// one is taken.
func logBeforeDeployDecision(ctx context.Context, id string, decision beforeDeployDecision) {
	switch {
	case decision.Skip:
		commandDiagnostic(ctx, "backup", fmt.Sprintf("Skipping the pre-release backup of %s by its database policy (scheduled backups only); its scheduled backups continue.", id))
	case decision.Reuse.ID != "":
		commandDiagnostic(ctx, "backup", fmt.Sprintf("Reusing backup %s from %s ago for %s (policy: %s).", decision.Reuse.ID, ageText(decision.Age), id, decision.Policy))
	case decision.Reason != "":
		commandDiagnostic(ctx, "backup", fmt.Sprintf("Taking a fresh backup of %s: %s (policy: %s).", id, decision.Reason, decision.Policy))
	default:
		commandDiagnostic(ctx, "backup", fmt.Sprintf("Taking a fresh backup of %s (policy: %s).", id, decision.Policy))
	}
}
