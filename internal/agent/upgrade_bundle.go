package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/agim/lidza-deploy/internal/buildinfo"
	"github.com/agim/lidza-deploy/internal/updates"
)

// Signed distribution channel on a host. It is active only in builds that
// pin bundle signing keys (see internal/updates/keys.go). The control panel
// verifies a signed bundle, uploads its manifest, signature and archive here, and the agent
// verifies all three again with the keys compiled into this binary before
// it queues the upgrade for the root helper, which verifies once more.
var (
	upgradeTrust      = updates.Pinned
	upgradeHelperPath = "/usr/local/libexec/lidza-agent-upgrade"
	upgradeDataDirs   = []string{"/var/lib/lidza-agent"}
	upgradeInstalled  = func() string { return buildinfo.Version }
)

func (m *Manager) upgradeDir() string { return filepath.Join(m.cfg.DataDir, "upgrade") }

// upgradeChannelSupported reports whether this host can install signed
// bundles: the root helper is installed, the data directory is one the helper
// serves, and a signing key is pinned in this build.
func (m *Manager) upgradeChannelSupported() bool {
	if !upgradeTrust().Configured() || !slices.Contains(upgradeDataDirs, m.cfg.DataDir) || runtime.GOOS != "linux" {
		return false
	}
	info, err := os.Stat(upgradeHelperPath)
	return err == nil && info.Mode().IsRegular()
}

func (m *Manager) upgradeChannelStatus(status *UpgradeStatus) {
	status.Arch = runtime.GOARCH
	trust := upgradeTrust()
	status.Keys = trust.Fingerprints()
	if !trust.Configured() {
		return
	}
	status.Channel = "private"
	status.Supported = m.upgradeChannelSupported()
	switch {
	case status.Supported:
		status.Message = "Signed channel: bundles are verified on this host before installation."
	case !slices.Contains(upgradeDataDirs, m.cfg.DataDir):
		status.Message = "This agent's data directory is not served by the upgrade helper."
	default:
		status.Message = "Install the upgrade helper and its path unit on this server to enable signed bundle updates."
	}
}

type upgradeManifestRequest struct {
	Manifest  string            `json:"manifest"`
	Signature updates.Signature `json:"signature"`
}

// stageUpgradeManifest verifies and stores a manifest and signature for a
// version newer than the running build. Earlier staged versions are removed.
func (m *Manager) stageUpgradeManifest(in upgradeManifestRequest) (updates.Manifest, updates.Asset, error) {
	var empty updates.Manifest
	raw, err := json.Marshal(in.Signature)
	if err != nil {
		return empty, updates.Asset{}, err
	}
	manifest, _, err := upgradeTrust().Verify([]byte(in.Manifest), raw)
	if err != nil {
		return empty, updates.Asset{}, err
	}
	if !updates.Newer(manifest.Version, upgradeInstalled()) {
		return empty, updates.Asset{}, errors.New("bundle version is not newer than the installed release")
	}
	asset, err := manifest.Asset(runtime.GOARCH)
	if err != nil {
		return empty, updates.Asset{}, err
	}
	if !m.upgradeChannelSupported() {
		return empty, updates.Asset{}, errors.New("signed bundle updates are not supported on this host")
	}
	dir, err := updates.StagedDir(m.upgradeDir(), manifest.Version)
	if err != nil {
		return empty, updates.Asset{}, err
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return empty, updates.Asset{}, err
	}
	entries, _ := os.ReadDir(parent)
	for _, entry := range entries {
		if entry.Name() != manifest.Version {
			os.RemoveAll(filepath.Join(parent, entry.Name()))
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return empty, updates.Asset{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, updates.StagedManifest), []byte(in.Manifest), 0o600); err != nil {
		return empty, updates.Asset{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, updates.StagedSignature), raw, 0o600); err != nil {
		return empty, updates.Asset{}, err
	}
	os.Remove(filepath.Join(dir, updates.StagedArchive))
	return manifest, asset, nil
}

// receiveUpgradeArchive stores an uploaded archive beside its staged
// manifest after checking the digest and size the manifest names.
func (m *Manager) receiveUpgradeArchive(version string, body io.Reader) error {
	dir, err := updates.StagedDir(m.upgradeDir(), version)
	if err != nil {
		return err
	}
	manifestRaw, err := os.ReadFile(filepath.Join(dir, updates.StagedManifest))
	if err != nil {
		return errors.New("stage the bundle manifest before its archive")
	}
	signature, err := os.ReadFile(filepath.Join(dir, updates.StagedSignature))
	if err != nil {
		return errors.New("stage the bundle manifest before its archive")
	}
	manifest, _, err := upgradeTrust().Verify(manifestRaw, signature)
	if err != nil || manifest.Version != version {
		return errors.New("staged bundle manifest is not valid for this version")
	}
	asset, err := manifest.Asset(runtime.GOARCH)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".archive-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(body, asset.Size+1))
	if err != nil {
		return err
	}
	if size != asset.Size || hex.EncodeToString(hash.Sum(nil)) != asset.SHA256 {
		return errors.New("uploaded archive does not match the signed manifest")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := os.Chmod(file.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(dir, updates.StagedArchive))
}

// verifyStagedUpgrade re-checks a staged bundle right before it is queued.
func (m *Manager) verifyStagedUpgrade(version string) error {
	dir, err := updates.StagedDir(m.upgradeDir(), version)
	if err != nil {
		return err
	}
	manifest, err := updates.ReadStaged(dir, runtime.GOARCH, upgradeTrust())
	if err != nil {
		return err
	}
	if !updates.Newer(manifest.Version, upgradeInstalled()) {
		return errors.New("bundle version is not newer than the installed release")
	}
	return nil
}

func (m *Manager) upgradeBundleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/upgrade/manifest", func(w http.ResponseWriter, r *http.Request) {
		var in upgradeManifestRequest
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		manifest, asset, err := m.stageUpgradeManifest(in)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]any{"version": manifest.Version, "arch": runtime.GOARCH, "asset": asset})
	})
	mux.HandleFunc("PUT /v1/upgrade/bundle/{version}", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(20 * time.Minute))
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(20 * time.Minute))
		if r.Header.Get("Content-Type") != "application/octet-stream" {
			Fail(w, 415, errors.New("bundle archives are uploaded as application/octet-stream"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, updates.MaxArchiveSize+1)
		if err := m.receiveUpgradeArchive(r.PathValue("version"), r.Body); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]any{"version": r.PathValue("version"), "verified": true})
	})
}

// VerifyStagedBundle verifies a staged bundle directory with the keys pinned
// in this binary and returns its version. The root upgrade helper runs it
// through the installed binary before replacing anything.
func VerifyStagedBundle(dir string) (string, error) {
	manifest, err := updates.ReadStaged(filepath.Clean(dir), runtime.GOARCH, updates.Pinned())
	if err != nil {
		return "", err
	}
	return manifest.Version, nil
}
