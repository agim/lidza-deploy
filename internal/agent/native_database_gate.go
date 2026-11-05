package agent

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"time"
)

type NativeDatabaseBackupProof struct {
	Resource string    `json:"resource"`
	Backup   string    `json:"backup"`
	SHA256   string    `json:"sha256"`
	Created  time.Time `json:"created"`
	Verified time.Time `json:"verified"`
}

// A live coordinator must reserve the app operation before this gate and retain
// these proofs with its release intent. No restored database, URL rewrite or
// migration is part of a native release. All declared roles must be protected.
func (m *Manager) backupBeforeNativeRailsRelease(ctx context.Context, appID string, roles []string) ([]NativeDatabaseBackupProof, error) {
	if len(roles) == 0 || len(roles) > 5 || !slices.Contains(roles, "primary") {
		return nil, errors.New("native release requires its reviewed database roles")
	}
	required := map[string]bool{}
	for _, role := range roles {
		if !validNativeDatabaseRole(role) || required[role] {
			return nil, errors.New("native release database roles are invalid")
		}
		required[role] = true
	}
	m.mu.Lock()
	if _, registered := m.data.NativeApps[appID]; !registered || !m.nativeOperations[appID] {
		m.mu.Unlock()
		return nil, errors.New("native release must reserve its application before backups")
	}
	type backupResource struct {
		id          string
		archiveSize int64
	}
	resources := []backupResource{}
	// Databases the agent provisioned for an application it created follow
	// that application's backup choice: a verified local backup suffices when
	// off-site copies were not requested. Other databases require off-site
	// protection.
	_, provisioned := m.data.NativeHostData[appID]
	offsite := map[string]bool{}
	for id, binding := range m.data.NativeDatabases {
		if binding.Policy.AppID != appID || !m.nativeDatabaseInUseLocked(id) {
			continue
		}
		d, exists := m.data.Databases[id]
		offsite[id] = !provisioned || d.Backup.Offsite
		if !exists || !d.Ready || d.Mode != "external" || offsite[id] && (!d.Backup.Offsite || m.data.BackupStorage == nil) {
			m.mu.Unlock()
			return nil, errors.New("native release requires ready resources and verified off-site protection")
		}
		var latest BackupRecord
		for _, backup := range d.Backups {
			if backup.Offsite && backup.Created.After(latest.Created) {
				latest = backup
			}
		}
		resources = append(resources, backupResource{id: id, archiveSize: latest.Size})
		for _, role := range binding.Policy.Roles {
			delete(required, role)
		}
	}
	m.mu.Unlock()
	if len(resources) == 0 || len(resources) > 4 || len(required) > 0 {
		return nil, errors.New("native release has unprotected database roles; current release retained")
	}
	// Use the latest verified archive sizes to run likely slower backups
	// first. A quick archive taken first could age out during a large dump.
	slices.SortFunc(resources, func(a, b backupResource) int {
		return cmp.Or(cmp.Compare(b.archiveSize, a.archiveSize), cmp.Compare(a.id, b.id))
	})
	proofs := make([]NativeDatabaseBackupProof, 0, len(resources))
	for _, resource := range resources {
		id := resource.id
		started := time.Now().UTC()
		if err := m.backupBeforeRelease(ctx, []string{id}); err != nil {
			return nil, err
		}
		m.mu.Lock()
		d, found := m.data.Databases[id]
		valid := found && m.nativeDatabaseInUseLocked(id) && d.Operation == "" && d.Error == ""
		var latest BackupRecord
		for _, record := range d.Backups {
			if record.Kind == predeploymentBackup && record.Created.After(latest.Created) {
				latest = record
			}
		}
		m.mu.Unlock()
		if !valid || latest.ID == "" || latest.Created.Before(started) || offsite[id] && (!latest.Offsite || latest.UploadError != "") || !nativeHexDigest(latest.SHA256, 64) {
			return nil, errors.New("native backup proof changed or is incomplete; current release retained")
		}
		proofs = append(proofs, NativeDatabaseBackupProof{Resource: id, Backup: latest.ID, SHA256: latest.SHA256, Created: latest.Created, Verified: time.Now().UTC()})
	}
	return proofs, nil
}

// nativeHexDigest reports whether value is exactly length lowercase hex digits.
func nativeHexDigest(value string, length int) bool {
	if len(value) != length {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}
