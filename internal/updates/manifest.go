// Package updates verifies signed release bundles for a distribution update
// channel. A bundle is a GitHub release on the distribution's own repository
// that carries the built archives, a manifest naming every archive with its
// SHA-256 digest, and a detached Ed25519 signature over the exact manifest
// bytes. Verification needs only the public keys pinned in this package:
// neither the control panel nor an agent trusts a repository, a network
// path or each other for the content of an update.
package updates

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	// ManifestName and SignatureName are the release asset names.
	ManifestName  = "manifest.json"
	SignatureName = "manifest.sig"
	// ArchiveName returns the archive asset name for an architecture.
	archivePrefix = "lidza-agent-linux-"
	// MaxArchiveSize bounds a downloaded or uploaded archive.
	MaxArchiveSize = 512 << 20
	// MaxManifestSize bounds the manifest and signature documents.
	MaxManifestSize = 64 << 10
	signaturePrefix = "lidza-bundle-v1\n"
)

var (
	versionPattern    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-private\.[0-9]+$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	archPattern       = regexp.MustCompile(`^(amd64|arm64)$`)
)

// ArchiveName is the archive asset for an architecture.
func ArchiveName(arch string) string { return archivePrefix + arch + ".tar.gz" }

// ValidVersion reports whether v is a private channel version such as
// v0.3.0-private.1. Public stable versions (v0.3.0) never match, so a public
// release can never be mistaken for a private bundle or the reverse.
func ValidVersion(v string) bool { return versionPattern.MatchString(v) }

// Newer reports whether candidate may replace current. A build whose version
// is not a release version (a development or preview build) accepts any valid
// candidate; otherwise the candidate must be strictly newer by semantic
// version, so a bundle can never roll a host back silently.
func Newer(candidate, current string) bool {
	if !ValidVersion(candidate) {
		return false
	}
	if !semver.IsValid(current) {
		return true
	}
	return semver.Compare(candidate, current) > 0
}

// Asset is one archive named by the manifest.
type Asset struct {
	Name   string `json:"name"`
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest describes one signed bundle.
type Manifest struct {
	Schema     int       `json:"schema"`
	Channel    string    `json:"channel"`
	Repository string    `json:"repository"`
	Version    string    `json:"version"`
	Commit     string    `json:"commit"`
	BuiltAt    time.Time `json:"built_at"`
	Assets     []Asset   `json:"assets"`
}

// Validate reports whether the manifest is complete and well formed.
func (m Manifest) Validate() error {
	switch {
	case m.Schema != 1:
		return errors.New("bundle manifest schema is not supported")
	case m.Channel != "private":
		return errors.New("bundle manifest is not for the private channel")
	case !repositoryPattern.MatchString(m.Repository):
		return errors.New("bundle manifest repository is invalid")
	case !ValidVersion(m.Version):
		return errors.New("bundle manifest version is not a private release version")
	case !commitPattern.MatchString(m.Commit):
		return errors.New("bundle manifest commit is invalid")
	case len(m.Assets) == 0:
		return errors.New("bundle manifest names no archives")
	}
	seen := map[string]bool{}
	for _, a := range m.Assets {
		if !archPattern.MatchString(a.Arch) || a.Name != ArchiveName(a.Arch) || !digestPattern.MatchString(a.SHA256) || a.Size <= 0 || a.Size > MaxArchiveSize || seen[a.Arch] {
			return fmt.Errorf("bundle manifest asset %q is invalid", a.Name)
		}
		seen[a.Arch] = true
	}
	return nil
}

// Asset returns the archive for an architecture.
func (m Manifest) Asset(arch string) (Asset, error) {
	for _, a := range m.Assets {
		if a.Arch == arch {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("bundle has no archive for %s", arch)
}

// ParseManifest decodes and validates a manifest document.
func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	if len(raw) == 0 || len(raw) > MaxManifestSize {
		return m, errors.New("bundle manifest size is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, errors.New("bundle manifest is not valid JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return m, errors.New("bundle manifest must be one JSON object")
	}
	return m, m.Validate()
}

// Signature is the detached signature document published beside a manifest.
type Signature struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Signature string `json:"signature"`
}

// ParseSignature decodes a signature document.
func ParseSignature(raw []byte) (Signature, error) {
	var s Signature
	if len(raw) == 0 || len(raw) > MaxManifestSize {
		return s, errors.New("bundle signature size is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil || s.Algorithm != "ed25519" || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(s.KeyID) {
		return s, errors.New("bundle signature document is invalid")
	}
	return s, nil
}

// KeyID identifies a public key: the first eight bytes of its SHA-256.
func KeyID(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// Fingerprint renders a public key for operators: its key id followed by
// the hex public key.
func Fingerprint(key ed25519.PublicKey) string {
	return KeyID(key) + " " + hex.EncodeToString(key)
}

// Sign signs manifest bytes. The signed message is domain separated so a
// manifest signature can never be replayed as a signature over other data.
func Sign(key ed25519.PrivateKey, manifest []byte) Signature {
	return Signature{
		KeyID:     KeyID(key.Public().(ed25519.PublicKey)),
		Algorithm: "ed25519",
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte(signaturePrefix), manifest...))),
	}
}

// Trust is the set of public keys that may sign bundles.
type Trust struct {
	Keys []ed25519.PublicKey
}

// Configured reports whether any signing key is trusted.
func (t Trust) Configured() bool { return len(t.Keys) > 0 }

// Fingerprints lists the trusted keys for display.
func (t Trust) Fingerprints() []string {
	out := make([]string, 0, len(t.Keys))
	for _, key := range t.Keys {
		out = append(out, Fingerprint(key))
	}
	sort.Strings(out)
	return out
}

// Verify checks a manifest document against its signature and returns the
// parsed manifest and the id of the key that signed it.
func (t Trust) Verify(manifest, signature []byte) (Manifest, string, error) {
	if !t.Configured() {
		return Manifest{}, "", errors.New("no trusted bundle signing key is configured")
	}
	sig, err := ParseSignature(signature)
	if err != nil {
		return Manifest{}, "", err
	}
	raw, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return Manifest{}, "", errors.New("bundle signature is malformed")
	}
	if len(manifest) == 0 || len(manifest) > MaxManifestSize {
		return Manifest{}, "", errors.New("bundle manifest size is invalid")
	}
	for _, key := range t.Keys {
		if KeyID(key) != sig.KeyID {
			continue
		}
		if !ed25519.Verify(key, append([]byte(signaturePrefix), manifest...), raw) {
			return Manifest{}, "", errors.New("bundle signature does not verify")
		}
		m, err := ParseManifest(manifest)
		if err != nil {
			return Manifest{}, "", err
		}
		return m, sig.KeyID, nil
	}
	return Manifest{}, "", errors.New("bundle was signed by an untrusted key")
}

// Digest hashes a file, refusing files above MaxArchiveSize.
func Digest(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, MaxArchiveSize+1))
	if err != nil {
		return "", 0, err
	}
	if size > MaxArchiveSize {
		return "", 0, errors.New("bundle archive exceeds the size limit")
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// VerifyArchive checks that the file at path is the manifest's archive for
// arch, byte for byte.
func VerifyArchive(path string, m Manifest, arch string) (Asset, error) {
	asset, err := m.Asset(arch)
	if err != nil {
		return asset, err
	}
	digest, size, err := Digest(path)
	if err != nil {
		return asset, err
	}
	if digest != asset.SHA256 || size != asset.Size {
		return asset, errors.New("bundle archive does not match its signed manifest")
	}
	return asset, nil
}

// ParsePublicKey decodes a hex encoded Ed25519 public key.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("bundle signing public key must be 32 hex-encoded bytes")
	}
	return ed25519.PublicKey(raw), nil
}
