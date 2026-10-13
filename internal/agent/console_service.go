package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const consoleRootAction = "console-root"

// migrateConsoleService bridges hosts installed before the server console was
// introduced. The release updater is already a fixed root-owned helper; once
// that helper advertises the migration marker, queue its fixed systemd change.
// No command, path, or browser input is passed to the privileged helper.
func (m *Manager) migrateConsoleService() {
	if os.Geteuid() == 0 || runtime.GOOS != "linux" || m.cfg.DataDir != "/var/lib/lidza-agent" {
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				queued, err := queueConsoleServiceMigration(m.cfg.DataDir, "/usr/local/libexec/lidza-agent-upgrade")
				if err != nil || queued {
					return
				}
			}
		}
	}()
}

func queueConsoleServiceMigration(dataDir, helperPath string) (bool, error) {
	helper, err := os.ReadFile(helperPath)
	if err != nil || !bytes.Contains(helper, []byte("lidza-console-root-v1")) {
		return false, err
	}
	dir := filepath.Join(dataDir, "upgrade")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	request := filepath.Join(dir, "request")
	if _, err := os.Stat(request); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	file, err := os.CreateTemp(dir, ".console-root-")
	if err != nil {
		return false, err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.WriteString(consoleRootAction + "\n"); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, request)
	}
	return err == nil, err
}
