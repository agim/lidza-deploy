package updates

import (
	"crypto/ed25519"
	"strings"
)

// TrustedPublicKeys pins the Ed25519 public keys allowed to sign bundles for
// this distribution, hex encoded. The list is compiled into every binary that
// verifies updates; nothing read from a host, a database or the network can
// add to it.
//
// The public source tree pins no key, so the signed bundle channel is off and
// hosts update from the project's published releases. A distribution that
// publishes its own signed bundles either adds its keys here in its own source
// tree, or supplies them at build time without changing the source:
//
//	go build -ldflags "-X github.com/agim/lidza-deploy/internal/updates.BuildTrustedKeys=<hex>[,<hex>]" ./cmd/...
//
// scripts/bundle-build.sh passes LIDZA_BUNDLE_TRUSTED_KEYS this way.
//
// Rotation: add the new key, release through the channel so every host
// trusts both keys, sign later bundles with the new key, then remove the old
// key in a following release. An empty trust set disables the channel.
//
// Generate a key pair with: go run ./cmd/bundle-sign keygen -out <file>
var TrustedPublicKeys = []string{}

// BuildTrustedKeys is a comma-separated list of hex keys set with -ldflags -X.
var BuildTrustedKeys string

// Pinned returns the compiled-in trust set. Malformed entries are ignored
// rather than trusted.
func Pinned() Trust {
	var keys []ed25519.PublicKey
	encoded := append([]string{}, TrustedPublicKeys...)
	for _, value := range strings.Split(BuildTrustedKeys, ",") {
		if value = strings.TrimSpace(value); value != "" {
			encoded = append(encoded, value)
		}
	}
	for _, value := range encoded {
		if key, err := ParsePublicKey(value); err == nil {
			keys = append(keys, key)
		}
	}
	return Trust{Keys: keys}
}
