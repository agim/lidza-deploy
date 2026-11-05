package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Keep fingerprints only in encrypted state, never in the public app view.
func (m *Manager) nativeRevisions(units []string) (map[string]string, error) {
	root, err := os.OpenRoot(filepath.Join(m.cfg.Native.Root, ".config", "systemd", "user"))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	result := map[string]string{}
	for _, unit := range units {
		info, err := root.Stat(unit)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
			return nil, errors.New("native service definition unavailable")
		}
		data, err := root.ReadFile(unit)
		if err != nil || len(data) > 64<<10 {
			return nil, errors.New("native service definition unavailable")
		}
		sum := sha256.Sum256(data)
		result[unit] = hex.EncodeToString(sum[:])
	}
	return result, nil
}

func (m *Manager) nativeConfiguredPort(name string, units []string) string {
	expected := filepath.Join(m.cfg.Native.Root, name, ".asdf-vars")
	for _, unit := range units {
		if !strings.HasSuffix(unit, "-server.service") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.cfg.Native.Root, ".config", "systemd", "user", unit))
		if err != nil || len(data) > 64<<10 {
			return ""
		}
		found := false
		for _, line := range strings.Split(string(data), "\n") {
			key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			if key == "EnvironmentFile" {
				if strings.TrimPrefix(val, "-") != expected {
					return ""
				}
				found = true
			}
			if key == "Environment" && strings.Contains(val, "PORT") {
				return ""
			}
		}
		if found {
			return nativePort(m.nativeEnvironment(name))
		}
	}
	return ""
}
