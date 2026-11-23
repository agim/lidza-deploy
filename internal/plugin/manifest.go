// Package plugin is the agent's side of the plugin contract
// (docs/plugins-proposal.md): a plugin is one signed program that knows one
// piece of application-specific software. The agent verifies and installs
// it, runs its hooks with limits and knows nothing about what is behind
// them.
package plugin

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/agim/lidza-deploy/pluginapi"
)

// Contract is the version of the hook contract this agent speaks.
const Contract = pluginapi.Contract

const (
	// MaxManifestSize bounds a manifest or signature document.
	MaxManifestSize = 64 << 10
	// MaxArchiveSize bounds a plugin archive.
	MaxArchiveSize = 512 << 20
	// signaturePrefix separates plugin signatures from every other signature
	// made with the same key: an update bundle's signature is not a plugin's.
	signaturePrefix = "lidza-deploy plugin manifest v1\n"
)

// Hooks are the hooks of contract 1.
var Hooks = []string{"describe", "check", "provision", "release", "status", "backup", "restore", "export", "import", "tune", "maintain", "repair", "remove"}

var (
	namePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	versionPattern = regexp.MustCompile(`^[0-9]{1,5}(\.[0-9]{1,5}){0,2}$`)
	envPattern     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	digestPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	keyIDPattern   = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// Archive is the plugin's program for one architecture.
type Archive struct {
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest describes a plugin release. It is the signed document.
type Manifest struct {
	Contract int    `json:"contract"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	// Provides names the service applications ask for, such as "clickhouse".
	Provides string `json:"provides"`
	// Summary is one sentence for people.
	Summary string `json:"summary,omitempty"`
	// Hooks are the hooks the plugin implements.
	Hooks []string `json:"hooks"`
	// Environment lists the only variables the plugin may set for an application.
	Environment []string  `json:"environment,omitempty"`
	Archives    []Archive `json:"archives"`
}

// Validate reports whether a manifest is complete and well formed.
func (m Manifest) Validate() error {
	switch {
	case m.Contract != Contract:
		return fmt.Errorf("plugin contract %d is not supported by this agent (it speaks %d)", m.Contract, Contract)
	case !namePattern.MatchString(m.Name):
		return errors.New("plugin name must be lowercase letters, digits and dashes")
	case !versionPattern.MatchString(m.Version):
		return errors.New("plugin version must look like 1, 1.2 or 1.2.3")
	case !namePattern.MatchString(m.Provides):
		return errors.New("plugin must name the service it provides")
	case len(m.Summary) > 200:
		return errors.New("plugin summary is too long")
	case len(m.Hooks) == 0 || !slices.Contains(m.Hooks, "describe"):
		return errors.New("plugin must implement the describe hook")
	case len(m.Archives) == 0:
		return errors.New("plugin manifest names no archive")
	}
	for _, hook := range m.Hooks {
		if !slices.Contains(Hooks, hook) {
			return fmt.Errorf("plugin names an unknown hook %q", hook)
		}
	}
	for _, key := range m.Environment {
		if !envPattern.MatchString(key) {
			return fmt.Errorf("plugin names an invalid environment variable %q", key)
		}
	}
	seen := map[string]bool{}
	for _, a := range m.Archives {
		if (a.Arch != "amd64" && a.Arch != "arm64") || seen[a.Arch] || !digestPattern.MatchString(a.SHA256) || a.Size <= 0 || a.Size > MaxArchiveSize {
			return fmt.Errorf("plugin archive for %q is invalid", a.Arch)
		}
		seen[a.Arch] = true
	}
	return nil
}

// Implements reports whether the plugin has a hook.
func (m Manifest) Implements(hook string) bool { return slices.Contains(m.Hooks, hook) }

// Archive returns the archive for an architecture.
func (m Manifest) Archive(arch string) (Archive, error) {
	for _, a := range m.Archives {
		if a.Arch == arch {
			return a, nil
		}
	}
	return Archive{}, fmt.Errorf("plugin %s has no build for %s", m.Name, arch)
}

// ParseManifest decodes and validates a manifest document.
func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	if len(raw) == 0 || len(raw) > MaxManifestSize {
		return m, errors.New("plugin manifest size is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, errors.New("plugin manifest is not valid JSON of the expected shape")
	}
	return m, m.Validate()
}

// Signature is the detached signature published beside a manifest.
type Signature struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Signature string `json:"signature"`
}

// KeyID identifies a public key: the first eight bytes of its SHA-256.
func KeyID(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// ParsePublicKey decodes a hex Ed25519 public key.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("a signing key is 64 hexadecimal characters")
	}
	return ed25519.PublicKey(raw), nil
}

// Sign signs manifest bytes for publication.
func Sign(key ed25519.PrivateKey, manifest []byte) Signature {
	return Signature{KeyID: KeyID(key.Public().(ed25519.PublicKey)), Algorithm: "ed25519",
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte(signaturePrefix), manifest...)))}
}

// Verify checks a manifest against its signature with the trusted keys and
// returns the manifest and the id of the key that signed it.
func Verify(manifest, signature []byte, trusted []ed25519.PublicKey) (Manifest, string, error) {
	if len(trusted) == 0 {
		return Manifest{}, "", errors.New("this server trusts no plugin signing key; add the plugin's key first")
	}
	var sig Signature
	if len(signature) == 0 || len(signature) > MaxManifestSize || json.Unmarshal(signature, &sig) != nil || sig.Algorithm != "ed25519" || !keyIDPattern.MatchString(sig.KeyID) {
		return Manifest{}, "", errors.New("plugin signature document is invalid")
	}
	raw, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return Manifest{}, "", errors.New("plugin signature is malformed")
	}
	for _, key := range trusted {
		if KeyID(key) != sig.KeyID {
			continue
		}
		if !ed25519.Verify(key, append([]byte(signaturePrefix), manifest...), raw) {
			return Manifest{}, "", errors.New("plugin signature does not verify")
		}
		m, err := ParseManifest(manifest)
		return m, sig.KeyID, err
	}
	return Manifest{}, "", errors.New("the plugin was signed by a key this server does not trust")
}
