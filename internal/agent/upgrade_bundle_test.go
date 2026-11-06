package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/updates"
)

type bundleFixture struct {
	key      ed25519.PrivateKey
	manifest []byte
	archive  []byte
	version  string
}

func newBundleFixture(t *testing.T, version string) bundleFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	archive := []byte("tar.gz bytes for " + version)
	sum := sha256.Sum256(archive)
	m := updates.Manifest{Schema: 1, Channel: "private", Repository: "acme/deploy-private", Version: version, Commit: strings.Repeat("b", 40), BuiltAt: time.Now().UTC(),
		Assets: []updates.Asset{{Name: updates.ArchiveName(runtime.GOARCH), Arch: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive))}}}
	raw, _ := json.Marshal(m)
	saved := upgradeTrust
	upgradeTrust = func() updates.Trust { return updates.Trust{Keys: []ed25519.PublicKey{pub}} }
	t.Cleanup(func() { upgradeTrust = saved })
	return bundleFixture{key: priv, manifest: raw, archive: archive, version: version}
}

// channelHost makes the manager look like a host with the helper installed.
func channelHost(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "var/lib/lidza-agent")
	os.MkdirAll(dataDir, 0o700)
	helper := filepath.Join(root, "helper")
	os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755)
	savedHelper, savedDirs := upgradeHelperPath, upgradeDataDirs
	upgradeHelperPath, upgradeDataDirs = helper, []string{dataDir}
	t.Cleanup(func() { upgradeHelperPath, upgradeDataDirs = savedHelper, savedDirs })
	m, err := NewManager(context.Background(), Config{DataDir: dataDir, APIKey: strings.Repeat("a", 32)}, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func installedVersion(t *testing.T, version string) {
	t.Helper()
	saved := upgradeInstalled
	upgradeInstalled = func() string { return version }
	t.Cleanup(func() { upgradeInstalled = saved })
}

func call(t *testing.T, h http.Handler, token, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUpgradeBundleStagingVerifiesEveryStep(t *testing.T) {
	m := channelHost(t)
	f := newBundleFixture(t, "v0.9.0-private.1")
	h := Handler(m)
	token := m.cfg.APIKey
	status := m.upgradeStatus()
	if !status.Supported || status.Channel != "private" || len(status.Keys) != 1 || status.Arch != runtime.GOARCH {
		t.Fatalf("channel status %+v", status)
	}
	manifestBody := func(sig updates.Signature, manifest []byte) []byte {
		raw, _ := json.Marshal(upgradeManifestRequest{Manifest: string(manifest), Signature: sig})
		return raw
	}
	// Archive before manifest, bad signature, then a valid manifest.
	if w := call(t, h, token, "PUT", "/v1/upgrade/bundle/"+f.version, "application/octet-stream", f.archive); w.Code != 409 {
		t.Fatal("archive accepted before its manifest", w.Code, w.Body)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if w := call(t, h, token, "POST", "/v1/upgrade/manifest", "application/json", manifestBody(updates.Sign(other, f.manifest), f.manifest)); w.Code != 409 {
		t.Fatal("untrusted signature accepted", w.Code)
	}
	if w := call(t, h, token, "POST", "/v1/upgrade/manifest", "application/json", manifestBody(updates.Sign(f.key, f.manifest), f.manifest)); w.Code != 200 {
		t.Fatal("valid manifest refused", w.Code, w.Body)
	}
	// Queueing before the archive arrives, a wrong archive, then the right one.
	if w := call(t, h, token, "POST", "/v1/upgrade", "application/json", []byte(`{"version":"`+f.version+`"}`)); w.Code != 409 {
		t.Fatal("upgrade queued without a staged archive", w.Code, w.Body)
	}
	if w := call(t, h, token, "PUT", "/v1/upgrade/bundle/"+f.version, "application/octet-stream", append(f.archive, '!')); w.Code != 409 {
		t.Fatal("wrong archive accepted", w.Code)
	}
	if w := call(t, h, token, "PUT", "/v1/upgrade/bundle/"+f.version, "text/plain", f.archive); w.Code != 415 {
		t.Fatal("non-binary upload accepted", w.Code)
	}
	if w := call(t, h, token, "PUT", "/v1/upgrade/bundle/"+f.version, "application/octet-stream", f.archive); w.Code != 200 {
		t.Fatal("archive refused", w.Code, w.Body)
	}
	staged := filepath.Join(m.cfg.DataDir, "upgrade", "staged", f.version)
	if _, err := updates.ReadStaged(staged, runtime.GOARCH, upgradeTrust()); err != nil {
		t.Fatal("staged bundle does not verify", err)
	}
	// A native application's release (not in Apps) must block the restart.
	m.mu.Lock()
	m.data.Deployments = append(m.data.Deployments, Deployment{ID: "native-release", AppID: "rails-shop", Status: "building"})
	m.mu.Unlock()
	if w := call(t, h, token, "POST", "/v1/upgrade", "application/json", []byte(`{"version":"`+f.version+`"}`)); w.Code != 409 || !strings.Contains(w.Body.String(), "rails-shop") {
		t.Fatal("upgrade queued during a native release", w.Code, w.Body)
	}
	m.mu.Lock()
	m.data.Deployments = m.data.Deployments[:len(m.data.Deployments)-1]
	m.nativeOperations = map[string]bool{"rails-shop": true}
	m.mu.Unlock()
	if w := call(t, h, token, "POST", "/v1/upgrade", "application/json", []byte(`{"version":"`+f.version+`"}`)); w.Code != 409 {
		t.Fatal("upgrade queued during a native operation", w.Code, w.Body)
	}
	m.mu.Lock()
	m.nativeOperations = nil
	m.mu.Unlock()
	if w := call(t, h, token, "POST", "/v1/upgrade", "application/json", []byte(`{"version":"`+f.version+`"}`)); w.Code != 202 {
		t.Fatal("verified bundle not queued", w.Code, w.Body)
	}
	request, err := os.ReadFile(filepath.Join(m.cfg.DataDir, "upgrade", "request"))
	if err != nil || strings.TrimSpace(string(request)) != f.version {
		t.Fatal("helper request not written", string(request), err)
	}
	if m.upgradeStatus().State != "queued" {
		t.Fatal("status should be queued")
	}
	// A tampered staged archive is refused by the final check too.
	os.Remove(filepath.Join(m.cfg.DataDir, "upgrade", "request"))
	os.WriteFile(filepath.Join(staged, updates.StagedArchive), []byte("swapped"), 0o600)
	if w := call(t, h, token, "POST", "/v1/upgrade", "application/json", []byte(`{"version":"`+f.version+`"}`)); w.Code != 409 {
		t.Fatal("tampered staged archive queued", w.Code)
	}
}

func TestUpgradeBundleRefusesDowngradesAndUnsupportedHosts(t *testing.T) {
	installedVersion(t, "v0.9.0-private.5")
	m := channelHost(t)
	f := newBundleFixture(t, "v0.9.0-private.4")
	_, _, err := m.stageUpgradeManifest(upgradeManifestRequest{Manifest: string(f.manifest), Signature: updates.Sign(f.key, f.manifest)})
	if err == nil || !strings.Contains(err.Error(), "not newer") {
		t.Fatal("downgrade accepted", err)
	}
	// A preview build accepts any valid version, but not on an unsupported host.
	installedVersion(t, "preview-abc1234")
	other := testManager(t, &fakeRuntime{}) // data directory not served by the helper
	if other.upgradeStatus().Supported {
		t.Fatal("unsupported data directory reported as supported")
	}
	if _, _, err := other.stageUpgradeManifest(upgradeManifestRequest{Manifest: string(f.manifest), Signature: updates.Sign(f.key, f.manifest)}); err == nil {
		t.Fatal("manifest staged on an unsupported host")
	}
	upgradeTrust = func() updates.Trust { return updates.Trust{} }
	status := m.upgradeStatus()
	if status.Supported || status.Channel != "" {
		t.Fatal("channel reported without pinned keys", status)
	}
}

func TestVerifyStagedBundleUsesPinnedKeysOnly(t *testing.T) {
	f := newBundleFixture(t, "v0.9.0-private.1")
	dir := filepath.Join(t.TempDir(), f.version)
	os.Mkdir(dir, 0o700)
	sig, _ := json.Marshal(updates.Sign(f.key, f.manifest))
	os.WriteFile(filepath.Join(dir, updates.StagedManifest), f.manifest, 0o600)
	os.WriteFile(filepath.Join(dir, updates.StagedSignature), sig, 0o600)
	os.WriteFile(filepath.Join(dir, updates.StagedArchive), f.archive, 0o600)
	// The CLI path uses the compiled-in keys, not the test override.
	if _, err := VerifyStagedBundle(dir); err == nil {
		t.Fatal("bundle verified without a pinned key")
	}
	pub := f.key.Public().(ed25519.PublicKey)
	savedKeys := updates.TrustedPublicKeys
	updates.TrustedPublicKeys = []string{hex.EncodeToString(pub)}
	defer func() { updates.TrustedPublicKeys = savedKeys }()
	if got, err := VerifyStagedBundle(dir); err != nil || got != f.version {
		t.Fatal(got, err)
	}
}
