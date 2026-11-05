// bundle-sign builds, signs and verifies signed distribution bundle manifests.
//
//	bundle-sign keygen -out signing.key            print the public key to pin
//	bundle-sign manifest -repository o/n -version v0.3.0-private.1 -commit <sha> -out manifest.json ARCHIVE...
//	bundle-sign sign -key signing.key -manifest manifest.json -out manifest.sig
//	bundle-sign verify -manifest manifest.json -signature manifest.sig [-public-key HEX] [ARCHIVE...]
//
// The signing key never leaves the file it is written to (mode 0600) or the
// LIDZA_BUNDLE_SIGNING_KEY environment variable used by CI.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/updates"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: bundle-sign keygen|manifest|sign|verify ..."))
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "manifest":
		err = manifest(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "bundle-sign:", err)
	os.Exit(1)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "new private key file (created 0600; refused when it exists)")
	fs.Parse(args)
	if *out == "" {
		return errors.New("-out is required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := pem.Encode(file, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		return err
	}
	fmt.Printf("public key: %s\nkey id:     %s\n", hex.EncodeToString(pub), updates.KeyID(pub))
	fmt.Println("Pin the public key in internal/updates/keys.go or LIDZA_BUNDLE_TRUSTED_KEYS; keep the private key file out of the repository.")
	return nil
}

func loadKey(path string) (ed25519.PrivateKey, error) {
	var raw []byte
	if path != "" {
		var err error
		if raw, err = os.ReadFile(path); err != nil {
			return nil, err
		}
	} else if env := os.Getenv("LIDZA_BUNDLE_SIGNING_KEY"); env != "" {
		raw = []byte(env)
	} else {
		return nil, errors.New("provide -key or LIDZA_BUNDLE_SIGNING_KEY")
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("signing key must be a PEM PKCS#8 private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("signing key must be Ed25519")
	}
	return key, nil
}

var archPattern = regexp.MustCompile(`^lidza-agent-linux-(amd64|arm64)\.tar\.gz$`)

func manifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	repository := fs.String("repository", "", "owner/name of the repository publishing the bundle")
	version := fs.String("version", "", "channel release version, for example v0.3.0-private.1")
	commit := fs.String("commit", "", "source commit")
	out := fs.String("out", updates.ManifestName, "manifest output path")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New("name at least one archive")
	}
	m := updates.Manifest{Schema: 1, Channel: "private", Repository: *repository, Version: *version, Commit: *commit, BuiltAt: time.Now().UTC().Truncate(time.Second)}
	for _, path := range fs.Args() {
		match := archPattern.FindStringSubmatch(filepath.Base(path))
		if match == nil {
			return fmt.Errorf("archive %s is not named lidza-agent-linux-<arch>.tar.gz", path)
		}
		digest, size, err := updates.Digest(path)
		if err != nil {
			return err
		}
		m.Assets = append(m.Assets, updates.Asset{Name: filepath.Base(path), Arch: match[1], SHA256: digest, Size: size})
	}
	if err := m.Validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*out, append(raw, '\n'), 0o644)
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("key", "", "private key file (default: LIDZA_BUNDLE_SIGNING_KEY)")
	manifestPath := fs.String("manifest", updates.ManifestName, "manifest to sign")
	out := fs.String("out", updates.SignatureName, "signature output path")
	fs.Parse(args)
	key, err := loadKey(*keyPath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	if _, err := updates.ParseManifest(raw); err != nil {
		return err
	}
	sig, err := json.Marshal(updates.Sign(key, raw))
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, append(sig, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s with key %s\n", *manifestPath, updates.KeyID(key.Public().(ed25519.PublicKey)))
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	manifestPath := fs.String("manifest", updates.ManifestName, "manifest")
	signaturePath := fs.String("signature", updates.SignatureName, "signature")
	publicKey := fs.String("public-key", "", "hex public key to trust instead of the pinned keys")
	fs.Parse(args)
	trust := updates.Pinned()
	if *publicKey != "" {
		key, err := updates.ParsePublicKey(*publicKey)
		if err != nil {
			return err
		}
		trust = updates.Trust{Keys: []ed25519.PublicKey{key}}
	}
	raw, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(*signaturePath)
	if err != nil {
		return err
	}
	m, keyID, err := trust.Verify(raw, sig)
	if err != nil {
		return err
	}
	for _, path := range fs.Args() {
		match := archPattern.FindStringSubmatch(filepath.Base(path))
		if match == nil {
			return fmt.Errorf("archive %s is not named lidza-agent-linux-<arch>.tar.gz", path)
		}
		if _, err := updates.VerifyArchive(path, m, match[1]); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	fmt.Printf("verified %s %s (%s) signed by %s; archives: %s\n", m.Repository, m.Version, m.Commit[:12], keyID, strings.Join(fs.Args(), " "))
	return nil
}
