package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const publicTLSMount = "/var/lib/lidza-public-tls"
const publicTLSPrefix = "DEPLOY_PUBLIC_TLS_"
const defaultCertificateDirectory = "/var/lib/caddy/.local/share/caddy/certificates"

type ServiceTLSConfig struct {
	CertificateDirectory string `json:"certificate_directory,omitempty"`
	TrustRoots           string `json:"trust_roots,omitempty"`
}
type StackPublicTLS struct {
	Hostname string `json:"hostname"`
	UID      int    `json:"uid,omitempty"`
}
type ServiceTLSBinding struct {
	ResourceID string `json:"resource_id"`
	Service    string `json:"service"`
	Hostname   string `json:"hostname"`
	UID        int    `json:"uid"`
}
type ServiceTLSStatus struct {
	Hostname    string     `json:"hostname"`
	Status      string     `json:"status"`
	NotAfter    *time.Time `json:"not_after,omitempty"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	Warning     string     `json:"warning,omitempty"`
}
type serviceTLSController struct {
	mu           sync.Mutex
	root, source string
	roots        *x509.CertPool
	states       map[string]ServiceTLSStatus
	owners       map[string][]string
	revoked      map[string]bool
}

func newServiceTLSController(cfg Config) (*serviceTLSController, error) {
	c := &serviceTLSController{root: filepath.Join(cfg.DataDir, "public-tls"), source: defaultCertificateDirectory, states: map[string]ServiceTLSStatus{}, owners: map[string][]string{}, revoked: map[string]bool{}}
	if cfg.ServiceTLS != nil {
		if cfg.ServiceTLS.CertificateDirectory != "" {
			c.source = cfg.ServiceTLS.CertificateDirectory
		}
		if cfg.ServiceTLS.TrustRoots != "" {
			data, err := readGatewayFile(cfg.ServiceTLS.TrustRoots, false)
			if err != nil {
				return nil, errors.New("service TLS trust_roots must be a bounded regular PEM file")
			}
			c.roots = x509.NewCertPool()
			if !c.roots.AppendCertsFromPEM(data) {
				return nil, errors.New("service TLS trust_roots contains no certificates")
			}
		}
	}
	if !filepath.IsAbs(c.source) || strings.ContainsAny(c.source, ",\x00\r\n") {
		return nil, errors.New("service TLS certificate_directory must be an absolute path")
	}
	return c, nil
}
func serviceTLSBinding(a App, s StackService) *ServiceTLSBinding {
	if s.PublicTLS == nil {
		return nil
	}
	return &ServiceTLSBinding{ResourceID: a.resourceID(), Service: s.Name, Hostname: a.Domain, UID: s.PublicTLS.UID}
}
func (b ServiceTLSBinding) valid() bool {
	return idPattern.MatchString(b.ResourceID) && idPattern.MatchString(b.Service) && validateHostname(b.Hostname) == nil && b.UID >= 0 && b.UID <= 65535
}
func (b ServiceTLSBinding) key() string {
	hash := sha256.Sum256([]byte(b.Hostname + "/" + strconv.Itoa(b.UID)))
	return filepath.Join(b.ResourceID, b.Service, hex.EncodeToString(hash[:]))
}
func (b ServiceTLSBinding) environment() map[string]string {
	return map[string]string{publicTLSPrefix + "HOSTNAME": b.Hostname, publicTLSPrefix + "CERT_FILE": publicTLSMount + "/current/fullchain.pem", publicTLSPrefix + "KEY_FILE": publicTLSMount + "/current/privkey.pem", publicTLSPrefix + "BUNDLE_FILE": publicTLSMount + "/current/bundle.pem"}
}
func (c *serviceTLSController) mount(b ServiceTLSBinding) string {
	return "type=bind,src=" + filepath.Join(c.root, b.key()) + ",dst=" + publicTLSMount + ",readonly,bind-propagation=rprivate"
}
func boundedCertificateFile(root *os.Root, path string) ([]byte, error) {
	// No source symlink, including intermediate components, may select another domain.
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := range parts {
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("certificate source symlink rejected")
		}
	}
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("certificate source is not regular")
	}
	data, err := io.ReadAll(io.LimitReader(f, 128*1024+1))
	if err != nil || len(data) > 128*1024 {
		return nil, errors.New("certificate source exceeds size limit")
	}
	return data, nil
}
func validatePublicCertificate(cert, key []byte, hostname string, roots *x509.CertPool) (*x509.Certificate, error) {
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, errors.New("certificate and private key do not match")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, errors.New("invalid public certificate")
	}
	// Never deliver a wildcard, CA or multi-domain private key to one app.
	if leaf.IsCA || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != hostname || len(leaf.IPAddresses) != 0 || len(leaf.EmailAddresses) != 0 || len(leaf.URIs) != 0 {
		return nil, errors.New("public service certificate must cover only its exact primary hostname")
	}
	intermediates := x509.NewCertPool()
	for _, raw := range pair.Certificate[1:] {
		v, e := x509.ParseCertificate(raw)
		if e != nil {
			return nil, errors.New("invalid public certificate chain")
		}
		intermediates.AddCert(v)
	}
	_, err = leaf.Verify(x509.VerifyOptions{DNSName: hostname, Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
		return nil, errors.New("public certificate chain, hostname or validity verification failed")
	}
	return leaf, nil
}
func (c *serviceTLSController) sourceCertificate(hostname string) ([]byte, []byte, *x509.Certificate, error) {
	root, err := os.OpenRoot(c.source)
	if err != nil {
		return nil, nil, nil, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, nil, nil, err
	}
	defer dir.Close()
	issuers, err := dir.ReadDir(65)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, nil, err
	}
	if len(issuers) > 64 {
		return nil, nil, nil, errors.New("too many Caddy certificate issuers")
	}
	var cert, key []byte
	var leaf *x509.Certificate
	var invalid bool
	for _, issuer := range issuers {
		if !issuer.IsDir() || issuer.Type()&os.ModeSymlink != 0 {
			continue
		}
		base := filepath.Join(issuer.Name(), hostname, hostname)
		crt, e := boundedCertificateFile(root, base+".crt")
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			invalid = true
			continue
		}
		k, e := boundedCertificateFile(root, base+".key")
		if e != nil {
			invalid = true
			continue
		}
		candidate, e := validatePublicCertificate(crt, k, hostname, c.roots)
		if e != nil {
			invalid = true
			continue
		}
		if leaf == nil || candidate.NotAfter.After(leaf.NotAfter) {
			cert, key, leaf = crt, k, candidate
		}
	}
	if leaf == nil {
		if invalid {
			return nil, nil, nil, errors.New("no valid matching public certificate")
		}
		return nil, nil, nil, os.ErrNotExist
	}
	return cert, key, leaf, nil
}
func certificateFingerprint(leaf *x509.Certificate) string {
	hash := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(hash[:])
}
func writeTLSFile(dir, name string, data []byte, mode os.FileMode, uid int) error {
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	if uid != os.Getuid() {
		if err = file.Chown(uid, -1); err != nil {
			return err
		}
	}
	if _, err = file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}
func syncTLSDirectory(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (c *serviceTLSController) publish(b ServiceTLSBinding, cert, key []byte) error {
	dir := filepath.Join(c.root, b.key())
	bundle := append(append(append([]byte{}, cert...), '\n'), key...)
	// The managed directory is read-only to its sole consumer, never writable by a service.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	old, readErr := boundedCurrentBundle(root)
	root.Close()
	if readErr == nil && bytes.Equal(old, bundle) {
		return nil
	}
	generation, err := os.MkdirTemp(dir, ".generation-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(generation)
		}
	}()
	if err = os.Chmod(generation, 0755); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{{"fullchain.pem", cert, 0444}, {"privkey.pem", key, 0400}, {"bundle.pem", bundle, 0400}} {
		if err = writeTLSFile(generation, f.name, f.data, f.mode, b.UID); err != nil {
			return err
		}
	}
	if err = syncTLSDirectory(generation); err != nil {
		return err
	}
	link := filepath.Join(dir, ".current-"+newID())
	if err = os.Symlink(filepath.Base(generation), link); err != nil {
		return err
	}
	defer os.Remove(link)
	if err = os.Rename(link, filepath.Join(dir, "current")); err != nil {
		return err
	}
	published = true
	if err = syncTLSDirectory(dir); err != nil {
		return err
	}
	// Retain one previous generation for consumers resolving current before reload.
	entries, _ := os.ReadDir(dir)
	var previous []os.DirEntry
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".generation-") && e.Name() != filepath.Base(generation) {
			previous = append(previous, e)
		}
	}
	slices.SortFunc(previous, func(a, b os.DirEntry) int {
		x, _ := a.Info()
		y, _ := b.Info()
		if x == nil || y == nil {
			return 0
		}
		return y.ModTime().Compare(x.ModTime())
	})
	for _, e := range previous[min(1, len(previous)):] {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return errors.New("public certificate generation cleanup failed")
		}
	}
	return nil
}
func boundedCurrentBundle(root *os.Root) ([]byte, error) {
	f, err := root.Open("current/bundle.pem")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 256*1024+1))
}
func (c *serviceTLSController) sync(b ServiceTLSBinding) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.ContainsAny(c.root, ",\x00\r\n") {
		return errors.New("public certificate volume path is unsafe")
	}
	if !b.valid() || c.revoked[b.ResourceID] || !slices.Contains(c.owners[b.ResourceID], b.Hostname) {
		return errors.New("public certificate binding is invalid or retired")
	}
	dir := filepath.Join(c.root, b.key())
	if err := os.MkdirAll(dir, 0755); err != nil {
		return errors.New("cannot prepare private service certificate volume")
	}
	// Guard the host root even when the selected service reader is unprivileged.
	if err := os.Chmod(c.root, 0700); err != nil {
		return errors.New("cannot secure public certificate root")
	}
	// systemd uses UMask=0077. The bind root must still be traversable by
	// the selected container uid; private files themselves stay owner-only.
	if err := os.Chmod(dir, 0755); err != nil {
		return errors.New("cannot prepare public certificate reader directory")
	}
	status := ServiceTLSStatus{Hostname: b.Hostname, Status: "pending"}
	cert, key, leaf, err := c.sourceCertificate(b.Hostname)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			status.Warning = "Certificate source is unavailable or failed chain, key or hostname validation."
		}
		root, e := os.OpenRoot(dir)
		if e == nil {
			bundle, readErr := boundedCurrentBundle(root)
			root.Close()
			if readErr == nil {
				retained, e := validatePublicCertificate(bundle, bundle, b.Hostname, c.roots)
				if e == nil {
					status.Status = "ready"
					status.NotAfter = &retained.NotAfter
					status.Fingerprint = certificateFingerprint(retained)
					status.Warning = "Certificate source unavailable; retaining the last valid certificate."
				} else {
					status.Status = "expired_or_invalid"
					status.Warning = "Retained public certificate is expired or invalid; service TLS must remain disabled."
				}
			}
		}
		c.states[b.key()] = status
		return nil // First-run issuance must not depend on a yet-unpublished application route.
	}
	if err = c.publish(b, cert, key); err != nil {
		return errors.New("could not atomically publish service certificate; inspect private volume permissions")
	}
	status.Status = "ready"
	status.NotAfter = &leaf.NotAfter
	status.Fingerprint = certificateFingerprint(leaf)
	c.states[b.key()] = status
	return nil
}
func (c *serviceTLSController) status(b ServiceTLSBinding) *ServiceTLSStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.states[b.key()]
	if !ok {
		s = ServiceTLSStatus{Hostname: b.Hostname, Status: "pending"}
	}
	if !slices.Contains(c.owners[b.ResourceID], b.Hostname) || c.revoked[b.ResourceID] {
		s.Status = "hostname_removed"
		s.Warning = "The certificate hostname is no longer assigned to this application; redeploy with its configured hostname."
	}
	if s.NotAfter != nil && time.Now().After(*s.NotAfter) {
		s.Status = "expired_or_invalid"
		s.Warning = "Public certificate expired; service TLS must remain disabled."
	}
	return &s
}
func (c *serviceTLSController) setOwner(a App) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.owners[a.resourceID()] = a.Hostnames()
}
func (c *serviceTLSController) retire(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !idPattern.MatchString(id) {
		return errors.New("invalid public certificate owner")
	}
	c.revoked[id] = true
	delete(c.owners, id)
	for key := range c.states {
		if strings.HasPrefix(key, id+string(filepath.Separator)) {
			delete(c.states, key)
		}
	}
	return os.RemoveAll(filepath.Join(c.root, id))
}
func (m *Manager) serviceTLSLoop() {
	defer m.wg.Done()
	m.runServiceTLS(m.ctx, time.Minute)
}
func (m *Manager) runServiceTLS(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	reconcile := func() {
		var bindings []ServiceTLSBinding
		m.mu.Lock()
		for _, a := range m.data.Apps {
			if a.Retiring || a.Stopped {
				continue
			}
			for _, r := range []*Release{a.Current, a.Previous} {
				if r != nil && r.Stack != nil {
					for _, s := range r.Stack.Containers {
						if s.PublicTLS != nil {
							bindings = append(bindings, *s.PublicTLS)
						}
					}
				}
			}
		}
		m.mu.Unlock()
		seen := map[string]bool{}
		for _, b := range bindings {
			if ctx.Err() != nil {
				return
			}
			if seen[b.key()] {
				continue
			}
			seen[b.key()] = true
			if err := m.serviceTLS.sync(b); err != nil { // Metadata only, no source output or key material.
				m.serviceTLS.mu.Lock()
				if !m.serviceTLS.revoked[b.ResourceID] && slices.Contains(m.serviceTLS.owners[b.ResourceID], b.Hostname) {
					m.serviceTLS.states[b.key()] = ServiceTLSStatus{Hostname: b.Hostname, Status: "error", Warning: "Service certificate synchronization failed; inspect agent filesystem permissions."}
				}
				m.serviceTLS.mu.Unlock()
			}
		}
	}
	reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}
