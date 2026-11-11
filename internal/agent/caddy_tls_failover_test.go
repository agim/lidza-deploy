package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const policiesPath = "/config/apps/tls/automation/policies"

// dnsimpleIssuer is an operator's DNS-challenge ACME issuer.
func dnsimpleIssuer() map[string]any {
	return map[string]any{"module": "acme", "ca": "https://acme.example.test/directory", "email": "ops@example.com",
		"external_account": map[string]any{"key_id": "kid", "mac_key": "mac"},
		"challenges": map[string]any{
			"dns":  map[string]any{"provider": map[string]any{"name": "dnsimple", "api_access_token": "secret"}},
			"http": map[string]any{"disabled": true, "alternate_port": 8080},
		}}
}

// incidentCaddyConfig mirrors the host of the incident: a policy without
// issuers (Caddy's HTTP-validating defaults) and a DNS-only policy for a
// wildcard and hostnames whose zones the DNS provider hosts.
func incidentCaddyConfig(policies ...any) map[string]any {
	config := operatorCaddyConfig()
	if len(policies) == 0 {
		policies = []any{
			map[string]any{"subjects": []any{"ops.example.com", "blog.example.com"}},
			map[string]any{"subjects": []any{"*.tenant.example.net", "zone.example.org"}, "issuers": []any{dnsimpleIssuer()}},
		}
	}
	config["apps"].(map[string]any)["tls"] = map[string]any{"automation": map[string]any{"policies": policies}}
	return config
}

func policyIssuers(admin *fakeCaddyAdmin, index string) []map[string]any {
	list, _ := admin.at(policiesPath + "/" + index + "/issuers").([]any)
	out := []map[string]any{}
	for _, item := range list {
		out = append(out, item.(map[string]any))
	}
	return out
}

func TestCaddyTLSNewHostGoesToTheHTTPPolicyNotTheDNSOne(t *testing.T) {
	admin := newFakeCaddyAdmin(t, incidentCaddyConfig())
	m := caddyManager(admin, "https")
	if _, err := m.publishNativeCaddyRoute(context.Background(), "cars", "cars.example.com", []string{"www.cars.example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := admin.strings(policiesPath + "/0/subjects"); !slices.Equal(got, []string{"ops.example.com", "blog.example.com", "cars.example.com", "www.cars.example.com"}) {
		t.Fatalf("HTTP policy subjects: %v", got)
	}
	if got := admin.strings(policiesPath + "/1/subjects"); !slices.Equal(got, []string{"*.tenant.example.net", "zone.example.org"}) {
		t.Fatalf("the DNS policy changed: %v", got)
	}
	if issuers := policyIssuers(admin, "1"); len(issuers) != 2 {
		t.Fatalf("publishing did not give the DNS policy its HTTP fallback: %v", issuers)
	}
}

func TestCaddyTLSWithoutAnHTTPPolicyCreatesAPlainACMEPolicy(t *testing.T) {
	admin := newFakeCaddyAdmin(t, incidentCaddyConfig(
		map[string]any{"subjects": []any{"zone.example.org"}, "issuers": []any{dnsimpleIssuer()}},
	))
	m := caddyManager(admin, "https")
	if _, err := m.publishNativeCaddyRoute(context.Background(), "cars", "cars.example.com", nil); err != nil {
		t.Fatal(err)
	}
	if got := admin.strings(policiesPath + "/1/subjects"); !slices.Equal(got, []string{"cars.example.com"}) {
		t.Fatalf("new policy: %v", admin.at(policiesPath))
	}
	if issuers := policyIssuers(admin, "1"); len(issuers) != 1 || issuers[0]["module"] != "acme" || issuers[0]["challenges"] != nil {
		t.Fatalf("new policy issuers: %v", issuers)
	}
	// The DNS policy with its fallback is still not a target: its
	// hostnames try DNS first, which a new hostname's zone may not have.
	if got := admin.strings(policiesPath + "/0/subjects"); !slices.Equal(got, []string{"zone.example.org"}) {
		t.Fatalf("the DNS policy received the hostname: %v", got)
	}
}

func TestCaddyTLSFallbackCopiesTheDNSIssuerOnce(t *testing.T) {
	admin := newFakeCaddyAdmin(t, incidentCaddyConfig())
	m := caddyManager(admin, "https")
	caddy, _ := m.nativeCaddy(context.Background())
	ctx := context.Background()
	policies, err := caddy.tlsPolicies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := caddy.applyTLSFallbacks(ctx, policies)
	if err != nil || !slices.Equal(changed, []int{1}) {
		t.Fatalf("changed %v %v", changed, err)
	}
	issuers := policyIssuers(admin, "1")
	if len(issuers) != 2 || issuers[0]["challenges"].(map[string]any)["dns"] == nil {
		t.Fatalf("the DNS issuer must stay first: %v", issuers)
	}
	fallback := issuers[1]
	challenges, _ := fallback["challenges"].(map[string]any)
	if fallback["module"] != "acme" || fallback["ca"] != "https://acme.example.test/directory" || fallback["email"] != "ops@example.com" ||
		fallback["external_account"].(map[string]any)["key_id"] != "kid" || challenges["dns"] != nil {
		t.Fatalf("fallback issuer: %v", fallback)
	}
	if http, _ := challenges["http"].(map[string]any); http["disabled"] != nil || http["alternate_port"] != float64(8080) {
		t.Fatalf("fallback HTTP challenge: %v", challenges)
	}
	// Subjects, the wildcard among them, are not touched.
	if got := admin.strings(policiesPath + "/1/subjects"); !slices.Equal(got, []string{"*.tenant.example.net", "zone.example.org"}) {
		t.Fatalf("subjects: %v", got)
	}
	if issuers := policyIssuers(admin, "0"); len(issuers) != 0 {
		t.Fatalf("the HTTP policy got issuers: %v", issuers)
	}

	// Again: nothing is written.
	admin.reset()
	policies, _ = caddy.tlsPolicies(ctx)
	if changed, err := caddy.applyTLSFallbacks(ctx, policies); err != nil || len(changed) != 0 || admin.called("PATCH ") != 0 {
		t.Fatalf("second run: %v %v %v", changed, err, admin.calls)
	}

	// A Caddyfile reload drops it; the upkeep restores it and reports it.
	admin.mu.Lock()
	admin.config = incidentCaddyConfig()
	admin.mu.Unlock()
	stubCaddyTLSStorage(t, "")
	m.reconcileCaddyTLS(ctx)
	if issuers := policyIssuers(admin, "1"); len(issuers) != 2 {
		t.Fatalf("the upkeep did not restore the fallback: %v", issuers)
	}
	report := m.caddyTLSReport()
	if report == nil || len(report.Fallbacks) != 1 || report.Fallbacks[0].Policy != 1 || report.Fallbacks[0].Provider != "dnsimple" || !report.Fallbacks[0].Added || report.Fallbacks[0].Subjects != 2 {
		t.Fatalf("report: %+v", report)
	}
	if status := m.hostIngressReport(ctx, false); status.TLS == nil || len(status.TLS.Fallbacks) != 1 {
		t.Fatalf("ingress view: %+v", status.TLS)
	}
}

func TestCaddyTLSRepairMovesDNSOnlyHostsWithoutCertificate(t *testing.T) {
	// The fallback could not be applied (an older configuration, a refused
	// write): the policy still validates only through DNS.
	admin := newFakeCaddyAdmin(t, incidentCaddyConfig(
		map[string]any{"subjects": []any{"ops.example.com"}},
		map[string]any{"subjects": []any{"*.tenant.example.net", "stuck.example.com", "issued.example.com"}, "issuers": []any{dnsimpleIssuer()}},
		map[string]any{"subjects": []any{"alone.example.com"}, "issuers": []any{dnsimpleIssuer()}},
	))
	m := caddyManager(admin, "https")
	caddy, _ := m.nativeCaddy(context.Background())
	ctx := context.Background()
	policies, _ := caddy.tlsPolicies(ctx)

	// Caddy refuses a subject in two policies, so adding first fails.
	if _, err := caddy.call(ctx, http.MethodPatch, policiesPath+"/0/subjects", mustJSON([]string{"ops.example.com", "stuck.example.com"})); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("the fake accepted a duplicate subject: %v", err)
	}
	admin.reset()
	hasCertificate := func(host string) bool { return host == "issued.example.com" }
	moved, err := caddy.repairTLSHosts(ctx, policies, []string{"stuck.example.com", "issued.example.com", "app.tenant.example.net", "alone.example.com"}, hasCertificate)
	if err != nil || !slices.Equal(moved, []string{"stuck.example.com", "alone.example.com"}) {
		t.Fatalf("moved %v %v", moved, err)
	}
	if got := admin.strings(policiesPath + "/0/subjects"); !slices.Equal(got, []string{"ops.example.com", "stuck.example.com", "alone.example.com"}) {
		t.Fatalf("HTTP policy: %v", got)
	}
	if got := admin.strings(policiesPath + "/1/subjects"); !slices.Equal(got, []string{"*.tenant.example.net", "issued.example.com"}) {
		t.Fatalf("DNS policy keeps the wildcard and the certified hostname: %v", got)
	}
	// The policy left empty is deleted, not turned into a catch-all.
	if list, _ := admin.at(policiesPath).([]any); len(list) != 2 {
		t.Fatalf("policies: %v", list)
	}
	// Remove, then add.
	removeAt, deleteAt, addAt := -1, -1, -1
	for i, call := range admin.calls {
		switch call {
		case "PATCH " + policiesPath + "/1/subjects":
			removeAt = i
		case "DELETE " + policiesPath + "/2":
			deleteAt = i
		case "PATCH " + policiesPath + "/0/subjects":
			addAt = i
		}
	}
	if removeAt < 0 || deleteAt < 0 || addAt < removeAt || addAt < deleteAt {
		t.Fatalf("calls out of order: %v", admin.calls)
	}

	// Nothing more to move.
	admin.reset()
	policies, _ = caddy.tlsPolicies(ctx)
	if moved, err := caddy.repairTLSHosts(ctx, policies, []string{"stuck.example.com", "issued.example.com"}, hasCertificate); err != nil || len(moved) != 0 || admin.called("PATCH ") != 0 {
		t.Fatalf("second repair: %v %v %v", moved, err, admin.calls)
	}
}

func TestCaddyTLSWithdrawRemovesSubjectsFromAnyPolicy(t *testing.T) {
	admin := newFakeCaddyAdmin(t, incidentCaddyConfig(
		map[string]any{"subjects": []any{"ops.example.com", "cars.example.com"}},
		map[string]any{"subjects": []any{"zone.example.org", "old.example.com"}, "issuers": []any{dnsimpleIssuer()}},
		map[string]any{"subjects": []any{"only.example.com"}},
	))
	caddy, _ := caddyManager(admin, "https").nativeCaddy(context.Background())
	if err := caddy.removeTLSSubjects(context.Background(), []string{"cars.example.com", "old.example.com", "only.example.com", "*.tenant.example.net"}); err != nil {
		t.Fatal(err)
	}
	if got := admin.strings(policiesPath + "/0/subjects"); !slices.Equal(got, []string{"ops.example.com"}) {
		t.Fatalf("policy 0: %v", got)
	}
	if got := admin.strings(policiesPath + "/1/subjects"); !slices.Equal(got, []string{"zone.example.org"}) {
		t.Fatalf("policy 1: %v", got)
	}
	if list, _ := admin.at(policiesPath).([]any); len(list) != 2 {
		t.Fatalf("an emptied policy must be deleted: %v", list)
	}
}

// stubCaddyTLSStorage points the upkeep at a Caddy storage root ("" for
// none) and makes DNS resolve.
func stubCaddyTLSStorage(t *testing.T, root string) {
	t.Helper()
	oldRoot, oldLookup := caddyTLSStorageRoot, caddyTLSLookup
	caddyTLSStorageRoot = func(context.Context, *Manager) string { return root }
	caddyTLSLookup = func(_ context.Context, _, host string) ([]net.IP, error) {
		if strings.HasPrefix(host, "nodns.") {
			return nil, errors.New("no such host")
		}
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	t.Cleanup(func() { caddyTLSStorageRoot, caddyTLSLookup = oldRoot, oldLookup })
}

func storeTestCertificate(t *testing.T, root, name string, notAfter time.Time) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{strings.Replace(name, "wildcard_.", "*.", 1)}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "certificates", "acme-v02.api.letsencrypt.org-directory", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCaddyTLSUpkeepMovesManagedHostsAndReportsCertificates(t *testing.T) {
	// Policy 1 validates only through DNS and lists one managed hostname.
	admin := newFakeCaddyAdmin(t, incidentCaddyConfig(
		map[string]any{"subjects": []any{"ops.example.com", "cars.example.com", "nodns.example.com"}},
		map[string]any{"subjects": []any{"zone.example.org", "www.cars.example.com"}, "issuers": []any{dnsimpleIssuer()}},
	))
	m := caddyManager(admin, "https")
	m.cfg.DataDir = t.TempDir()
	m.data.NativeApps = map[string]NativeApplication{"cars": {ID: "cars", Name: "cars", Domain: "cars.example.com", AdditionalDomains: []string{"www.cars.example.com", "nodns.example.com"}}}
	m.data.NativeCreated = map[string]NativeReleasePolicy{"cars": {Enabled: true, Runtime: "rails"}}
	m.data.Redirects = map[string][]RedirectDomain{"cars": {{Host: "old-cars.example.com"}}}
	root := t.TempDir()
	stubCaddyTLSStorage(t, root)
	storeTestCertificate(t, root, "cars.example.com", time.Now().Add(30*24*time.Hour))
	storeTestCertificate(t, root, "old-cars.example.com", time.Now().Add(-time.Hour)) // expired
	ctx := context.Background()

	m.reconcileCaddyTLS(ctx)
	report := m.caddyTLSReport()
	if report == nil || report.Error != "" {
		t.Fatalf("report: %+v", report)
	}
	// The fallback made the DNS policy HTTP-capable, so www stays there.
	if issuers := policyIssuers(admin, "1"); len(issuers) != 2 || len(report.Moved) != 0 {
		t.Fatalf("fallback %v moved %v", issuers, report.Moved)
	}
	// The redirect host had no policy: it is added to the HTTP one.
	if got := admin.strings(policiesPath + "/0/subjects"); !slices.Contains(got, "old-cars.example.com") {
		t.Fatalf("uncovered managed host not added: %v", got)
	}
	statuses := m.caddyDomainStatuses([]string{"cars.example.com", "www.cars.example.com", "nodns.example.com", "old-cars.example.com"})
	if len(statuses) != 4 || statuses[0].State != "ready" || statuses[1].State != "requesting_ssl" || statuses[2].State != "requesting_ssl" || !strings.Contains(statuses[2].Message, "no A or AAAA record") || statuses[3].State != "requesting_ssl" {
		t.Fatalf("statuses: %+v", statuses)
	}

	// After the grace period: not issued, with the reason, in the
	// application's view and settings.
	m.caddyTLSMu.Lock()
	for host, state := range m.caddyCerts {
		if !state.Missing.IsZero() {
			state.Missing = state.Missing.Add(-caddyCertificateGrace)
			m.caddyCerts[host] = state
		}
	}
	m.caddyTLSMu.Unlock()
	m.reconcileCaddyTLS(ctx)
	views := m.nativeViews()
	if len(views) != 1 || views[0].DomainStatus == nil || views[0].DomainStatus.State != "ready" || len(views[0].DomainStatuses) != 3 {
		t.Fatalf("view: %+v", views)
	}
	www := views[0].DomainStatuses[1]
	if www.Domain != "www.cars.example.com" || www.State != "certificate_not_issued" || !strings.HasPrefix(www.Message, "Certificate not issued: ") {
		t.Fatalf("www status: %+v", www)
	}
	if report := m.caddyTLSReport(); !slices.Equal(report.NotIssued, []string{"nodns.example.com", "old-cars.example.com", "www.cars.example.com"}) {
		t.Fatalf("not issued: %v", report.NotIssued)
	}
}

func TestCaddyTLSUpkeepMovesAStuckManagedHostWhenTheFallbackIsRefused(t *testing.T) {
	admin := newFakeCaddyAdmin(t, incidentCaddyConfig(
		map[string]any{"subjects": []any{"ops.example.com"}},
		map[string]any{"subjects": []any{"zone.example.org", "cars.example.com", "issued.example.com"}, "issuers": []any{dnsimpleIssuer()}},
	))
	admin.refuse = map[string]bool{"PATCH " + policiesPath + "/1/issuers": true}
	m := caddyManager(admin, "https")
	m.cfg.DataDir = t.TempDir()
	m.data.NativeApps = map[string]NativeApplication{"cars": {ID: "cars", Name: "cars", Domain: "cars.example.com", AdditionalDomains: []string{"issued.example.com"}}}
	m.data.NativeCreated = map[string]NativeReleasePolicy{"cars": {Enabled: true, Runtime: "rails"}}
	root := t.TempDir()
	stubCaddyTLSStorage(t, root)
	storeTestCertificate(t, root, "issued.example.com", time.Now().Add(30*24*time.Hour))

	m.reconcileCaddyTLS(context.Background())
	if got := admin.strings(policiesPath + "/0/subjects"); !slices.Equal(got, []string{"ops.example.com", "cars.example.com"}) {
		t.Fatalf("HTTP policy: %v", got)
	}
	if got := admin.strings(policiesPath + "/1/subjects"); !slices.Equal(got, []string{"zone.example.org", "issued.example.com"}) {
		t.Fatalf("DNS policy keeps the operator's and the certified hostname: %v", got)
	}
	report := m.caddyTLSReport()
	if !slices.Equal(report.Moved, []string{"cars.example.com"}) || !strings.Contains(report.Error, "HTTP fallback for TLS policy 1") || len(report.Fallbacks) != 0 {
		t.Fatalf("report: %+v", report)
	}
	statuses := m.caddyDomainStatuses([]string{"cars.example.com", "issued.example.com"})
	if len(statuses) != 2 || statuses[0].State != "requesting_ssl" || statuses[1].State != "ready" {
		t.Fatalf("statuses: %+v", statuses)
	}
	// The moved hostname stays moved.
	m.reconcileCaddyTLS(context.Background())
	if got := m.caddyTLSReport().Moved; !slices.Equal(got, []string{"cars.example.com"}) {
		t.Fatalf("moved after a second run: %v", got)
	}
}

func TestCaddyTLSReasonForADNSOnlyPolicyIsReportedAtOnce(t *testing.T) {
	stubCaddyTLSStorage(t, "")
	policies := []caddyTLSPolicy{{Subjects: &[]string{"cars.example.com"}, Issuers: []json.RawMessage{mustJSON(dnsimpleIssuer())}}}
	reason, structural := caddyCertificateReason(context.Background(), policies, "cars.example.com")
	if !structural || !strings.Contains(reason, "dnsimple DNS provider") {
		t.Fatalf("%q %v", reason, structural)
	}
	if reason, structural := caddyCertificateReason(context.Background(), policies, "other.example.com"); !structural || !strings.Contains(reason, "no Caddy TLS automation policy") {
		t.Fatalf("%q %v", reason, structural)
	}
}
