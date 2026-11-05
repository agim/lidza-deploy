package updates

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Staged bundle layout on a host: <dir>/manifest.json, <dir>/manifest.sig
// and <dir>/bundle.tar.gz. The control panel uploads the three files after
// verifying them; the agent verifies them again before queueing the upgrade;
// the root upgrade helper verifies them a third time with the installed
// binary before replacing anything.
const (
	StagedManifest  = ManifestName
	StagedSignature = SignatureName
	StagedArchive   = "bundle.tar.gz"
)

// ReadStaged verifies a staged bundle directory and returns its manifest.
// The directory name must equal the manifest version.
func ReadStaged(dir, arch string, trust Trust) (Manifest, error) {
	var m Manifest
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return m, errors.New("staged bundle directory must be an absolute clean path")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return m, errors.New("staged bundle directory is missing")
	}
	manifest, err := readSmall(filepath.Join(dir, StagedManifest))
	if err != nil {
		return m, err
	}
	signature, err := readSmall(filepath.Join(dir, StagedSignature))
	if err != nil {
		return m, err
	}
	m, _, err = trust.Verify(manifest, signature)
	if err != nil {
		return m, err
	}
	if filepath.Base(dir) != m.Version {
		return m, fmt.Errorf("staged bundle directory %s does not match version %s", filepath.Base(dir), m.Version)
	}
	if _, err := VerifyArchive(filepath.Join(dir, StagedArchive), m, arch); err != nil {
		return m, err
	}
	return m, nil
}

func readSmall(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxManifestSize {
		return nil, fmt.Errorf("%s is missing or not a small regular file", filepath.Base(path))
	}
	return os.ReadFile(path)
}

// StagedDir returns the staging directory for a version under an agent's
// upgrade directory, refusing versions that are not private release versions
// so the name can never escape the directory.
func StagedDir(upgradeDir, version string) (string, error) {
	if !ValidVersion(version) || strings.ContainsAny(version, "/\\") {
		return "", errors.New("staged bundle version is invalid")
	}
	return filepath.Join(upgradeDir, "staged", version), nil
}
