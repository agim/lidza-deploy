package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const consoleRootAction = "console-root"
const controlBootAction = "control-boot"
const controlBootMarker = "lidza-control-boot-v1"

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

// migrateControlBootService repairs the startup policy on control-panel hosts
// upgraded from a release that did not order the GUI after Docker. Unlike the
// older console-root bridge, this also runs after the agent has become root.
// The helper marker prevents an old privileged helper from seeing a request it
// does not understand, and the unit/drop-in marker makes the migration one-shot.
func (m *Manager) migrateControlBootService() {
	if runtime.GOOS != "linux" || m.cfg.DataDir != "/var/lib/lidza-agent" {
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
				done, err := queueControlBootMigration(
					m.cfg.DataDir,
					"/usr/local/libexec/lidza-agent-upgrade",
					"/etc/systemd/system/lidza-control.service",
					"/etc/systemd/system/lidza-control.service.d/20-boot.conf",
				)
				if err != nil || done {
					return
				}
			}
		}
	}()
}

func queueControlBootMigration(dataDir, helperPath, unitPath, overridePath string) (bool, error) {
	unit, err := os.ReadFile(unitPath)
	if os.IsNotExist(err) {
		return true, nil // Hosting-only agent: there is no local GUI to migrate.
	}
	if err != nil {
		return false, err
	}
	if bytes.Contains(unit, []byte(controlBootMarker)) {
		return true, nil
	}
	if override, readErr := os.ReadFile(overridePath); readErr == nil {
		if bytes.Contains(override, []byte(controlBootMarker)) {
			return true, nil
		}
	} else if !os.IsNotExist(readErr) {
		return false, readErr
	}
	help, err := os.ReadFile(helperPath)
	if err != nil || !bytes.Contains(help, []byte(controlBootMarker)) {
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
	file, err := os.CreateTemp(dir, ".control-boot-")
	if err != nil {
		return false, err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.WriteString(controlBootAction + "\n"); err == nil {
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
