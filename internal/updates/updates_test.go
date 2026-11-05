package updates

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (ed25519.PrivateKey, Trust, Manifest, []byte, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, StagedArchive)
	if err := os.WriteFile(archive, []byte("archive bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, size, err := Digest(archive)
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Schema: 1, Channel: "private", Repository: "acme/deploy-private", Version: "v0.3.0-private.2",
		Commit: strings.Repeat("a", 40), BuiltAt: time.Now().UTC(),
		Assets: []Asset{{Name: ArchiveName("amd64"), Arch: "amd64", SHA256: digest, Size: size}}}
	raw, _ := json.Marshal(m)
	return priv, Trust{Keys: []ed25519.PublicKey{pub}}, m, raw, dir
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSignVerifyAndRefusals(t *testing.T) {
	priv, trust, m, raw, _ := fixture(t)
	sig := encode(t, Sign(priv, raw))
	got, keyID, err := trust.Verify(raw, sig)
	if err != nil || got.Version != m.Version || keyID != KeyID(priv.Public().(ed25519.PublicKey)) {
		t.Fatal("valid signature refused", err)
	}
	tampered := []byte(strings.Replace(string(raw), "private.2", "private.3", 1))
	if _, _, err := trust.Verify(tampered, sig); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, err := trust.Verify(raw, encode(t, Sign(other, raw))); err == nil {
		t.Fatal("untrusted key accepted")
	}
	forged := Sign(other, raw)
	forged.KeyID = keyID
	if _, _, err := trust.Verify(raw, encode(t, forged)); err == nil {
		t.Fatal("forged key id accepted")
	}
	if _, _, err := (Trust{}).Verify(raw, sig); err == nil {
		t.Fatal("empty trust accepted a signature")
	}
	if _, _, err := trust.Verify(raw, []byte(`{"key_id":"`+keyID+`","algorithm":"none","signature":""}`)); err == nil {
		t.Fatal("unsigned document accepted")
	}
	public := Manifest{Schema: 1, Channel: "private", Repository: m.Repository, Version: "v0.3.0", Commit: m.Commit, Assets: m.Assets}
	publicRaw := encode(t, public)
	if _, _, err := trust.Verify(publicRaw, encode(t, Sign(priv, publicRaw))); err == nil {
		t.Fatal("public version accepted on the private channel")
	}
}

func TestVersionsAndDowngrades(t *testing.T) {
	for v, ok := range map[string]bool{"v0.3.0-private.1": true, "v10.2.3-private.12": true, "v0.3.0": false, "v0.3.0-private": false,
		"0.3.0-private.1": false, "v0.3.0-private.1/..": false, "preview-abc": false} {
		if ValidVersion(v) != ok {
			t.Fatal("version validity", v)
		}
	}
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"v0.3.0-private.2", "v0.3.0-private.1", true},
		{"v0.3.0-private.1", "v0.3.0-private.1", false},
		{"v0.3.0-private.1", "v0.3.0-private.2", false},
		{"v0.3.0-private.1", "v0.2.105", true},
		{"v0.2.105-private.1", "v0.2.105", false},
		{"v0.3.0-private.1", "preview-fd846a6", true},
		{"v0.3.0-private.1", "development", true},
		{"v0.3.0", "development", false},
	}
	for _, c := range cases {
		if Newer(c.candidate, c.current) != c.want {
			t.Fatalf("Newer(%s, %s) != %t", c.candidate, c.current, c.want)
		}
	}
}

func TestStagedBundleVerification(t *testing.T) {
	priv, trust, m, raw, dir := fixture(t)
	staged := filepath.Join(dir, m.Version)
	if err := os.Mkdir(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Rename(filepath.Join(dir, StagedArchive), filepath.Join(staged, StagedArchive))
	os.WriteFile(filepath.Join(staged, StagedManifest), raw, 0o600)
	os.WriteFile(filepath.Join(staged, StagedSignature), encode(t, Sign(priv, raw)), 0o600)
	if got, err := ReadStaged(staged, "amd64", trust); err != nil || got.Version != m.Version {
		t.Fatal("staged bundle refused", err)
	}
	if _, err := ReadStaged(staged, "arm64", trust); err == nil {
		t.Fatal("missing architecture accepted")
	}
	os.WriteFile(filepath.Join(staged, StagedArchive), []byte("archive bytes!"), 0o600)
	if _, err := ReadStaged(staged, "amd64", trust); err == nil {
		t.Fatal("modified archive accepted")
	}
	wrong := filepath.Join(dir, "v0.3.0-private.9")
	os.Rename(staged, wrong)
	if _, err := ReadStaged(wrong, "amd64", trust); err == nil || !strings.Contains(err.Error(), "does not match version") {
		t.Fatal("directory/version mismatch accepted", err)
	}
	if _, err := StagedDir("/var/lib/x/upgrade", "../etc"); err == nil {
		t.Fatal("unsafe version accepted")
	}
	if path, err := StagedDir("/var/lib/x/upgrade", m.Version); err != nil || path != "/var/lib/x/upgrade/staged/"+m.Version {
		t.Fatal(path, err)
	}
}

func TestPinnedIgnoresMalformedKeys(t *testing.T) {
	saved := TrustedPublicKeys
	defer func() { TrustedPublicKeys = saved }()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	TrustedPublicKeys = []string{"not-hex", strings.Repeat("ab", 32)}
	if got := Pinned(); len(got.Keys) != 1 {
		t.Fatal("expected one parsed key", len(got.Keys))
	}
	if _, err := ParsePublicKey(Fingerprint(pub)[17:]); err != nil {
		t.Fatal("fingerprint does not round-trip", err)
	}
}

func TestSourceTreePinsNoKeyAndBuildKeysAreAdded(t *testing.T) {
	if len(TrustedPublicKeys) != 0 || BuildTrustedKeys != "" {
		t.Fatal("the source tree must not pin a distribution signing key")
	}
	if Pinned().Configured() {
		t.Fatal("the signed channel must be off without a pinned key")
	}
	defer func() { BuildTrustedKeys = "" }()
	BuildTrustedKeys = " " + strings.Repeat("ab", 32) + ",not-hex, " + strings.Repeat("cd", 32)
	if got := Pinned(); len(got.Keys) != 2 || !got.Configured() {
		t.Fatal("build-time keys were not trusted", len(got.Keys))
	}
}
