package testsupport

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/agim/lidza/pkg/credentials"
)

func TestFixtureKeyPreservesOperatorKeyFileIsolation(t *testing.T) {
	t.Setenv(credentials.EnvKeyOff, "1")
	t.Setenv(credentials.EnvMasterKey, "")
	dir := t.TempDir()
	Key(t)
	first, err := credentials.Key(dir)
	if err != nil || len(first) != 32 {
		t.Fatalf("fixture key unavailable: %v", err)
	}
	if _, err := credentials.Generate(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, credentials.MasterKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixture unexpectedly wrote a master-key file", err)
	}
	if _, err := credentials.Read(dir); err != nil {
		t.Fatal("fixture cannot decrypt its temporary credentials", err)
	}
	Key(t)
	second, err := credentials.Key(dir)
	if err != nil || bytes.Equal(first, second) {
		t.Fatal("fixture key was not independently regenerated", err)
	}
	t.Setenv(credentials.EnvMasterKey, "")
	if _, err := credentials.Key(dir); !errors.Is(err, credentials.ErrNoKey) {
		t.Fatal("operator key-file suppression was lost", err)
	}
}
