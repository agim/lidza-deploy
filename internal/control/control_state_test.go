package control

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func init() {
	saveState = saveStateWithoutSync
	stateWritesAsync = false
}

// saveStateWithoutSync replaces the state file atomically like state.Save,
// without fsync.
func saveStateWithoutSync(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err == nil {
		_, err = f.Write(b)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	} else {
		f.Close()
		return err
	}
	return os.Rename(f.Name(), path)
}
