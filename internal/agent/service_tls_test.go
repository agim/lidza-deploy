package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type publicTLSFixture struct {
	c     *serviceTLSController
	a     App
	b     ServiceTLSBinding
	ca    *x509.Certificate
	caKey ed25519.PrivateKey
	caPEM []byte
}

func newPublicTLSFixture(t *testing.T) *publicTLSFixture {
	t.Helper()
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated public TLS test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	raw, e := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if e != nil {
		t.Fatal(e)
	}
	ca, e := x509.ParseCertificate(raw)
	if e != nil {
		t.Fatal(e)
	}
	dir, source := t.TempDir(), t.TempDir()
	rootPath := filepath.Join(dir, "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw})
	if e = os.WriteFile(rootPath, caPEM, 0600); e != nil {
		t.Fatal(e)
	}
	c, e := newServiceTLSController(Config{DataDir: dir, ServiceTLS: &ServiceTLSConfig{CertificateDirectory: source, TrustRoots: rootPath}})
	if e != nil {
		t.Fatal(e)
	}
	a := testApp("tls-one")
	c.setOwner(a)
	return &publicTLSFixture{c: c, a: a, b: ServiceTLSBinding{ResourceID: a.resourceID(), Service: "mail", Hostname: a.Domain, UID: os.Getuid()}, ca: ca, caKey: key, caPEM: caPEM}
}
func (f *publicTLSFixture) issue(t *testing.T, names []string, expiry time.Time) ([]byte, []byte) {
	t.Helper()
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if e != nil {
		t.Fatal(e)
	}
	template := &x509.Certificate{SerialNumber: serial, DNSNames: names, NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: expiry, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	raw, e := x509.CreateCertificate(rand.Reader, template, f.ca, pub, f.caKey)
	if e != nil {
		t.Fatal(e)
	}
	private, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
}
func (f *publicTLSFixture) store(t *testing.T, cert, key []byte) {
	t.Helper()
	dir := filepath.Join(f.c.source, "acme-test", f.b.Hostname)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	for suffix, data := range map[string][]byte{".crt": cert, ".key": key} {
		if e := os.WriteFile(filepath.Join(dir, f.b.Hostname+suffix), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
}
func (f *publicTLSFixture) sync(t *testing.T) {
	t.Helper()
	if e := f.c.sync(f.b); e != nil {
		t.Fatal(e)
	}
}
func TestPublicTLSColdStartRenewalAndIsolation(t *testing.T) {
	f := newPublicTLSFixture(t)
	f.sync(t)
	if f.c.status(f.b).Status != "pending" {
		t.Fatal("cold start must wait for Caddy")
	}
	cert, key := f.issue(t, []string{f.b.Hostname}, time.Now().Add(time.Hour))
	f.store(t, cert, key)
	f.sync(t)
	status := f.c.status(f.b)
	if status.Status != "ready" || status.NotAfter == nil {
		t.Fatal(status)
	}
	dir := filepath.Join(f.c.root, f.b.key())
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	f.sync(t)
	if info, e := os.Stat(dir); e != nil || info.Mode().Perm() != 0755 {
		t.Fatal("systemd umask leaves reader unable to traverse bind root", e)
	}
	oldGeneration, e := os.Readlink(filepath.Join(dir, "current"))
	if e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(dir, "current", "bundle.pem"))
	if e != nil || !bytes.Contains(data, key) {
		t.Fatal("private bundle missing", e)
	}
	info, e := os.Stat(filepath.Join(dir, "current", "bundle.pem"))
	if e != nil || info.Mode().Perm() != 0400 {
		t.Fatal("private key permissions", e)
	}
	if !strings.Contains(f.c.mount(f.b), ",readonly,") || strings.Contains(f.c.mount(f.b), f.c.source) {
		t.Fatal("source store exposed")
	}
	raw, _ := json.Marshal(status)
	if bytes.Contains(raw, key) || bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Fatal("key in status")
	}
	cert2, key2 := f.issue(t, []string{f.b.Hostname}, time.Now().Add(2*time.Hour))
	f.store(t, cert2, key2)
	f.sync(t)
	if f.c.status(f.b).Fingerprint == status.Fingerprint {
		t.Fatal("renewal did not change fingerprint")
	}
	old, e := os.ReadFile(filepath.Join(dir, oldGeneration, "bundle.pem"))
	if e != nil || !bytes.Equal(old, data) {
		t.Fatal("reader generation changed", e)
	}
	f.store(t, cert2, key) // mismatched renewal must retain last valid pair
	f.sync(t)
	if f.c.status(f.b).Fingerprint == status.Fingerprint || f.c.status(f.b).Warning == "" {
		t.Fatal("bad renewal replaced previous pair")
	}
	if !bytes.Equal(mustReadTLS(t, filepath.Join(dir, "current", "privkey.pem")), key2) {
		t.Fatal("bad key installed")
	}
	other := f.b
	other.ResourceID = "other-app"
	if f.c.sync(other) == nil {
		t.Fatal("another app's binding accepted")
	}
	f.c.setOwner(App{ID: f.a.ID, Domain: "new.example.com"})
	if f.c.sync(f.b) == nil {
		t.Fatal("removed hostname renewed")
	}
	if f.c.status(f.b).Status != "hostname_removed" {
		t.Fatal("removed hostname reported ready")
	}
	f.c.setOwner(f.a)
	if e = f.c.retire(f.b.ResourceID); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(dir); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("retired private key retained", e)
	}
	if f.c.sync(f.b) == nil {
		t.Fatal("stale reconciliation resurrected removed keys")
	}
}
func mustReadTLS(t *testing.T, path string) []byte {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestPublicTLSRejectsWrongIdentityAndUnsafeSources(t *testing.T) {
	for _, which := range []string{"wrong-domain", "wildcard", "extra-domain", "expired", "untrusted", "symlink", "oversize"} {
		t.Run(which, func(t *testing.T) {
			f := newPublicTLSFixture(t)
			names := []string{f.b.Hostname}
			expiry := time.Now().Add(time.Hour)
			switch which {
			case "wrong-domain":
				names = []string{"someone.example.com"}
			case "wildcard":
				names = []string{"*.example.com"}
			case "extra-domain":
				names = append(names, "someone.example.com")
			case "expired":
				expiry = time.Now().Add(-time.Hour)
			}
			cert, key := f.issue(t, names, expiry)
			if which == "untrusted" {
				other := newPublicTLSFixture(t)
				cert, key = other.issue(t, names, expiry)
			}
			f.store(t, cert, key)
			p := filepath.Join(f.c.source, "acme-test", f.b.Hostname, f.b.Hostname+".key")
			if which == "symlink" {
				if e := os.Rename(p, p+".saved"); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(p+".saved", p); e != nil {
					t.Fatal(e)
				}
			}
			if which == "oversize" {
				if e := os.WriteFile(p, bytes.Repeat([]byte("a"), 129*1024), 0600); e != nil {
					t.Fatal(e)
				}
			}
			f.sync(t)
			if f.c.status(f.b).Status != "pending" || f.c.status(f.b).Warning == "" {
				t.Fatal("unsafe source not rejected", f.c.status(f.b))
			}
			if _, e := os.Stat(filepath.Join(f.c.root, f.b.key(), "current")); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("unsafe source published", e)
			}
		})
	}
}
func TestPublicTLSConcurrentSyncAndRestart(t *testing.T) {
	f := newPublicTLSFixture(t)
	cert, key := f.issue(t, []string{f.b.Hostname}, time.Now().Add(time.Hour))
	f.store(t, cert, key)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := f.c.sync(f.b); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	old := f.c.status(f.b).Fingerprint
	replacement, e := newServiceTLSController(Config{DataDir: filepath.Dir(f.c.root), ServiceTLS: &ServiceTLSConfig{CertificateDirectory: f.c.source, TrustRoots: filepath.Join(filepath.Dir(f.c.root), "ca.pem")}})
	if e != nil {
		t.Fatal(e)
	}
	replacement.setOwner(f.a)
	if e = replacement.sync(f.b); e != nil {
		t.Fatal(e)
	}
	if replacement.status(f.b).Fingerprint != old {
		t.Fatal("restart rotated certificate")
	}
}
func TestPublicTLSManifestContract(t *testing.T) {
	valid := func() StackManifest {
		m := fixtureStackV2()
		m.Services[1].PublicTLS = &StackPublicTLS{Hostname: "primary"}
		return m
	}
	if e := valid().validate(); e != nil {
		t.Fatal(e)
	}
	for name, mutate := range map[string]func(*StackManifest){"v1": func(m *StackManifest) { m.Version = 1 }, "hostname": func(m *StackManifest) { m.Services[1].PublicTLS.Hostname = "other.example.com" }, "uid": func(m *StackManifest) { m.Services[1].PublicTLS.UID = -1 }, "hardened": func(m *StackManifest) { m.Services[1].Policy = "hardened" }, "mount": func(m *StackManifest) { m.Services[1].Volumes[0].Target = "/var/lib" }, "managed-env": func(m *StackManifest) { m.Services[1].Environment[publicTLSPrefix+"HOSTNAME"] = "other.example.com" }} {
		t.Run(name, func(t *testing.T) {
			m := valid()
			mutate(&m)
			if m.validate() == nil {
				t.Fatal("unsafe contract accepted")
			}
		})
	}
}

func TestPublicTLSPeriodicReconciliation(t *testing.T) {
	f := newPublicTLSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := f.a
	a.Current = &Release{Stack: &StackRelease{Containers: []StackContainer{{PublicTLS: &f.b}}}}
	m := &Manager{ctx: ctx, serviceTLS: f.c, data: diskState{Apps: map[string]App{a.ID: a}}}
	done := make(chan struct{})
	go func() { defer close(done); m.runServiceTLS(ctx, 5*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })
	wait := func(previous string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			s := f.c.status(f.b)
			if s.Status == "ready" && s.Fingerprint != previous {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("periodic public certificate synchronization did not finish")
	}
	cert, key := f.issue(t, []string{f.b.Hostname}, time.Now().Add(time.Hour))
	f.store(t, cert, key)
	wait("")
	old := f.c.status(f.b).Fingerprint
	cert, key = f.issue(t, []string{f.b.Hostname}, time.Now().Add(2*time.Hour))
	f.store(t, cert, key)
	wait(old)
}
func TestPublicTLSExpiredRetainedMaterialIsNotHealthy(t *testing.T) {
	f := newPublicTLSFixture(t)
	f.sync(t)
	cert, key := f.issue(t, []string{f.b.Hostname}, time.Now().Add(-time.Minute))
	// Simulate material that expired after it was installed, with no renewed source.
	if e := f.c.publish(f.b, cert, key); e != nil {
		t.Fatal(e)
	}
	f.sync(t)
	if f.c.status(f.b).Status != "expired_or_invalid" {
		t.Fatal("expired retained material reported healthy")
	}
}
