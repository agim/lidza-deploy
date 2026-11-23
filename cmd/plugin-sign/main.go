// Command plugin-sign prepares a plugin release (docs/plugins-proposal.md):
//
//	plugin-sign keygen -out plugin-signing.key        print the public key servers must trust
//	plugin-sign manifest -name clickhouse -version 1.0 -provides clickhouse \
//	    -hooks describe,check,status -env CLICKHOUSE_URL -archive amd64=plugin-amd64.tar.gz -out plugin.json
//	plugin-sign sign -key plugin-signing.key -manifest plugin.json -out plugin.sig
//
// The signing key never leaves the file it is written to (mode 0600).
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/agim/lidza-deploy/internal/plugin"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: plugin-sign keygen|manifest|sign ..."))
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "manifest":
		err = manifest(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	default:
		err = errors.New("usage: plugin-sign keygen|manifest|sign ...")
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "new private key file (created 0600; refused when it exists)")
	fs.Parse(args)
	if *out == "" {
		return errors.New("keygen needs -out")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
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
	fmt.Printf("public key: %s\nkey id:     %s\n", hex.EncodeToString(public), plugin.KeyID(public))
	fmt.Println("Servers install this plugin only after an operator adds the public key as a trusted plugin key.")
	return nil
}

func list(value string) []string {
	out := []string{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

type archives []string

func (a *archives) String() string     { return strings.Join(*a, ",") }
func (a *archives) Set(v string) error { *a = append(*a, v); return nil }

func manifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	name := fs.String("name", "", "plugin name")
	version := fs.String("version", "", "plugin version (1, 1.2 or 1.2.3)")
	provides := fs.String("provides", "", "service the plugin provides")
	summary := fs.String("summary", "", "one sentence for people")
	hooks := fs.String("hooks", "describe", "comma-separated hooks the plugin implements")
	env := fs.String("env", "", "comma-separated environment variables the plugin may set")
	out := fs.String("out", "plugin.json", "manifest file to write")
	var files archives
	fs.Var(&files, "archive", "ARCH=FILE, once per architecture (amd64, arm64)")
	fs.Parse(args)
	m := plugin.Manifest{Contract: plugin.Contract, Name: *name, Version: *version, Provides: *provides, Summary: *summary, Hooks: list(*hooks), Environment: list(*env)}
	for _, entry := range files {
		arch, path, ok := strings.Cut(entry, "=")
		if !ok {
			return fmt.Errorf("-archive wants ARCH=FILE, got %q", entry)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		digest := sha256.New()
		size, err := io.Copy(digest, file)
		file.Close()
		if err != nil {
			return err
		}
		m.Archives = append(m.Archives, plugin.Archive{Arch: arch, SHA256: hex.EncodeToString(digest.Sum(nil)), Size: size})
	}
	if err := m.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// No trailing newline: the signature covers the file's exact bytes, and
	// the server verifies exactly the text it is sent.
	return os.WriteFile(*out, raw, 0o644)
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("key", "", "private key file")
	manifestPath := fs.String("manifest", "plugin.json", "manifest to sign")
	out := fs.String("out", "plugin.sig", "signature file to write")
	fs.Parse(args)
	raw, err := os.ReadFile(*keyPath)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" {
		return errors.New("the signing key must be a PEM PKCS#8 private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return err
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return errors.New("the signing key must be an Ed25519 key")
	}
	// The signature covers the file's exact bytes: servers verify what they receive.
	document, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	if _, err := plugin.ParseManifest(document); err != nil {
		return err
	}
	signature, err := json.Marshal(plugin.Sign(key, document))
	if err != nil {
		return err
	}
	if _, _, err := plugin.Verify(document, signature, []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}); err != nil {
		return err
	}
	return os.WriteFile(*out, append(signature, '\n'), 0o644)
}
