package agent

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Concurrent commands of one release share the scratch directory: the first
// empties it, the others keep it, and nobody removes it while in use.
func TestNativeScratchSurvivesConcurrentCommands(t *testing.T) {
	root := t.TempDir()
	account := nativeReleaseAccount{UID: os.Getuid(), GID: os.Getgid()}
	dir := nativeReleaseScratchDir(root)
	release, err := nativeScratchUse(root, account)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "in-use")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done, err := nativeScratchUse(root, account)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := os.Stat(marker); err != nil {
				t.Errorf("a concurrent command emptied the scratch directory: %v", err)
			}
			done()
		}()
	}
	wg.Wait()
	release()
	// With nobody using it, the next command starts clean.
	done, err := nativeScratchUse(root, account)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the first command of a release must start from an empty scratch directory: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("scratch directory %v %v", info, err)
	}
}
