package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/agim/lidza/packs/storage"
)

// Off-site storage calls of a deletion are retried: a listing or deletion
// that fails with a network error, a timeout or a 5xx answer is tried again
// after archiveStorageRetryDelays (three attempts over about 20 seconds)
// before the deletion stops. Only an access-denied answer (401/403,
// AccessDenied and its relatives) is reported as a permission problem, and
// it is not retried: it does not heal on its own.

var archiveStorageRetryDelays = []time.Duration{5 * time.Second, 15 * time.Second}

var offsiteDeniedPattern = regexp.MustCompile(`: 40[13]( |$)|\b(AccessDenied|InvalidAccessKeyId|SignatureDoesNotMatch|AllAccessDisabled|AccountProblem|InvalidToken)\b`)

// offsiteAccessDenied reports a storage error that is the bucket refusing
// access, as opposed to a network, timeout or server error.
func offsiteAccessDenied(err error) bool {
	return err != nil && offsiteDeniedPattern.MatchString(err.Error())
}

// withStorageRetry runs op until it succeeds, fails with access denied or
// not found, or the attempts are spent.
func withStorageRetry(ctx context.Context, op func() error) error {
	for attempt := 0; ; attempt++ {
		err := op()
		if err == nil || offsiteAccessDenied(err) || errors.Is(err, storage.ErrNotFound) || attempt >= len(archiveStorageRetryDelays) || ctx.Err() != nil {
			return err
		}
		timer := time.NewTimer(archiveStorageRetryDelays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

// offsiteFailure words a failed off-site call: a permission to check only
// for an access-denied answer, otherwise a temporary storage error.
func offsiteFailure(err error, what, permission string) error {
	if offsiteAccessDenied(err) {
		return fmt.Errorf("could not %s: the bucket denied access; check its %s permission", what, permission)
	}
	return fmt.Errorf("could not %s: temporary off-site storage error after %d attempts (%s); it is retried when the deletion runs again", what, len(archiveStorageRetryDelays)+1, strings.TrimPrefix(err.Error(), "storage: "))
}

// ---- backup locks ---------------------------------------------------------------

func (m *Manager) nativeBackupLockPath(resource string) string {
	return filepath.Join(m.cfg.DataDir, ".lidza-native-pg-locks", resource+".lock")
}

// removeNativeBackupLock deletes a deleted database's backup lock file
// while holding the lock, so a backup that holds it is never disturbed.
// removed is false when there is no file or another operation holds it
// (the sweep tries again). A dump recreates the file when it runs again.
func (m *Manager) removeNativeBackupLock(resource string) (removed bool, err error) {
	if m.cfg.DataDir == "" || !idPattern.MatchString(resource) {
		return false, nil
	}
	path := m.nativeBackupLockPath(resource)
	lock, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("could not open the backup lock of %s", resource)
	}
	defer lock.Close()
	if info, err := lock.Stat(); err != nil || !info.Mode().IsRegular() {
		return false, nil
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false, nil // a backup holds it
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("could not remove the backup lock of %s", resource)
	}
	return true, nil
}

// archiveRemoveBackupLocks removes the backup lock files of an archive's
// databases that no live application's database record uses.
func (m *Manager) archiveRemoveBackupLocks(archiveID string) error {
	a := m.archiveSnapshot(archiveID)
	resources := append([]string{}, a.Databases...)
	if a.Native != nil && a.Native.Adopted != nil {
		for resource := range a.Native.Adopted.Records {
			resources = append(resources, resource)
		}
	}
	for _, resource := range resources {
		m.mu.Lock()
		binding, bound := m.data.NativeDatabases[resource]
		_, liveNative := m.data.NativeApps[binding.Policy.AppID]
		_, liveCreated := m.data.NativeCreated[binding.Policy.AppID]
		m.mu.Unlock()
		if bound && binding.Policy.AppID != a.AppID && (liveNative || liveCreated) {
			continue
		}
		removed, err := m.removeNativeBackupLock(resource)
		if err != nil {
			return err
		}
		if removed {
			m.archiveDeleted(archiveID, ArchiveItem{Kind: "backup lock", Name: resource, Detail: "backup lock file removed"})
		}
	}
	return nil
}
