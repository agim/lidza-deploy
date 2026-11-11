package agent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TLS automation upkeep on a host fronted by an admin-API Caddy.
//
// Caddy obtains a certificate with the issuers of the first automation
// policy whose subjects match the hostname, trying them in order. A policy
// whose only issuers solve the ACME DNS challenge through a DNS provider
// (DNSimple, Cloudflare, ...) can only certify hostnames whose zone that
// provider hosts; any other hostname in it never gets a certificate and its
// visitors see a TLS error. Hence:
//
//   - Hostnames the agent publishes go to a policy that validates over HTTP
//     (HTTP-01 or TLS-ALPN-01): one without issuers (Caddy's defaults), or
//     with an ACME issuer that has no DNS challenge; a plain ACME policy is
//     created when there is none (caddyTargetPolicy).
//   - Every policy whose issuers are all DNS-challenge ACME issuers gets an
//     HTTP fallback: a copy of each such issuer (CA, account, external
//     account binding) without the DNS challenge, after them. Caddy tries
//     DNS first and HTTP when DNS fails. Wildcard subjects still need DNS;
//     the fallback cannot certify them and changes nothing for them.
//   - A managed hostname (created application, additional or redirect
//     domain, the panel's hostname) listed in a policy that still validates
//     only through DNS, with no certificate in Caddy's storage, is moved to
//     the HTTP policy: removed from the old policy first, since Caddy
//     rejects a subject that two policies list, then added.
//   - The certificate status of each application and redirect hostname
//     comes from Caddy's storage, with the reason when there is none.
//
// The loop re-applies all of this every minute, so a Caddyfile reload that
// resets the runtime configuration is corrected without an operator.

const (
	caddyTLSInterval = time.Minute
	// caddyCertificateGrace is how long a hostname without a certificate is
	// reported as being requested before it is reported as not issued.
	caddyCertificateGrace = 10 * time.Minute
)

// Probes replaced in tests.
var (
	caddyTLSStorageRoot = func(ctx context.Context, m *Manager) string { return m.caddyStorage(ctx) }
	caddyTLSLookup      = net.DefaultResolver.LookupIP
)

// ---- issuers ------------------------------------------------------------------

type caddyIssuer struct {
	Module     string `json:"module"`
	Challenges *struct {
		DNS *struct {
			Provider *struct {
				Name string `json:"name"`
			} `json:"provider,omitempty"`
		} `json:"dns,omitempty"`
		HTTP *struct {
			Disabled bool `json:"disabled"`
		} `json:"http,omitempty"`
		TLSALPN *struct {
			Disabled bool `json:"disabled"`
		} `json:"tls-alpn,omitempty"`
	} `json:"challenges,omitempty"`
}

func parseCaddyIssuer(raw json.RawMessage) caddyIssuer {
	var issuer caddyIssuer
	_ = json.Unmarshal(raw, &issuer)
	return issuer
}

// dnsChallenge reports an ACME issuer that solves the DNS challenge; Caddy
// then uses no other challenge type for it.
func (i caddyIssuer) dnsChallenge() bool {
	return i.Module == "acme" && i.Challenges != nil && i.Challenges.DNS != nil
}

// httpValidation reports an ACME issuer that solves HTTP-01 or TLS-ALPN-01.
func (i caddyIssuer) httpValidation() bool {
	if i.Module != "acme" || i.dnsChallenge() {
		return false
	}
	if i.Challenges == nil {
		return true
	}
	httpOff := i.Challenges.HTTP != nil && i.Challenges.HTTP.Disabled
	alpnOff := i.Challenges.TLSALPN != nil && i.Challenges.TLSALPN.Disabled
	return !httpOff || !alpnOff
}

func (i caddyIssuer) dnsProvider() string {
	if i.Challenges != nil && i.Challenges.DNS != nil && i.Challenges.DNS.Provider != nil {
		return i.Challenges.DNS.Provider.Name
	}
	return ""
}

// httpCapable reports a policy that can certify a hostname over HTTP:
// without issuers Caddy uses its defaults (ACME with HTTP-01/TLS-ALPN-01).
func (p caddyTLSPolicy) httpCapable() bool {
	if len(p.Issuers) == 0 {
		return true
	}
	return slices.ContainsFunc(p.Issuers, func(raw json.RawMessage) bool { return parseCaddyIssuer(raw).httpValidation() })
}

func (p caddyTLSPolicy) hasDNSIssuer() bool {
	return slices.ContainsFunc(p.Issuers, func(raw json.RawMessage) bool { return parseCaddyIssuer(raw).dnsChallenge() })
}

// dnsOnly reports a policy whose issuers all validate through DNS.
func (p caddyTLSPolicy) dnsOnly() bool {
	return len(p.Issuers) > 0 && !slices.ContainsFunc(p.Issuers, func(raw json.RawMessage) bool { return !parseCaddyIssuer(raw).dnsChallenge() })
}

// dnsProvider names the DNS provider of the policy's first DNS issuer.
func (p caddyTLSPolicy) dnsProvider() string {
	for _, raw := range p.Issuers {
		if issuer := parseCaddyIssuer(raw); issuer.dnsChallenge() {
			return issuer.dnsProvider()
		}
	}
	return ""
}

// caddyHTTPFallback is the DNS issuer's settings without the DNS challenge
// (and without disabled HTTP or TLS-ALPN challenges, which only made sense
// next to it): the same CA, email and external account, validating over
// HTTP.
func caddyHTTPFallback(raw json.RawMessage) (json.RawMessage, error) {
	var issuer map[string]any
	if err := json.Unmarshal(raw, &issuer); err != nil {
		return nil, err
	}
	if challenges, ok := issuer["challenges"].(map[string]any); ok {
		delete(challenges, "dns")
		for _, name := range []string{"http", "tls-alpn"} {
			if challenge, ok := challenges[name].(map[string]any); ok {
				delete(challenge, "disabled")
				if len(challenge) == 0 {
					delete(challenges, name)
				}
			}
		}
		if len(challenges) == 0 {
			delete(issuer, "challenges")
		}
	}
	return json.Marshal(issuer)
}

// applyTLSFallbacks gives every DNS-only policy its HTTP fallback issuers
// and updates policies to match. It returns the policies changed; a policy
// that already has an HTTP-validating issuer is left alone, so repeating it
// writes nothing. A refused policy does not stop the others.
func (c nativeCaddy) applyTLSFallbacks(ctx context.Context, policies []caddyTLSPolicy) ([]int, error) {
	var changed []int
	var problems []error
	for i, policy := range policies {
		if policy.OnDemand || !policy.dnsOnly() {
			continue
		}
		issuers := slices.Clone(policy.Issuers)
		for _, raw := range policy.Issuers {
			fallback, err := caddyHTTPFallback(raw)
			if err != nil {
				continue
			}
			if !slices.ContainsFunc(issuers, func(existing json.RawMessage) bool { return jsonEqual(existing, fallback) }) {
				issuers = append(issuers, fallback)
			}
		}
		if len(issuers) == len(policy.Issuers) {
			continue
		}
		if _, err := c.call(ctx, http.MethodPatch, caddyTLSPath+"/automation/policies/"+strconv.Itoa(i)+"/issuers", mustJSON(issuers)); err != nil {
			problems = append(problems, fmt.Errorf("HTTP fallback for TLS policy %d: %w", i, err))
			continue
		}
		policies[i].Issuers = issuers
		changed = append(changed, i)
	}
	return changed, errors.Join(problems...)
}

// tlsPolicies reads the automation policies; nil when there are none.
func (c nativeCaddy) tlsPolicies(ctx context.Context) ([]caddyTLSPolicy, error) {
	var tlsApp caddyTLSApp
	found, err := c.get(ctx, caddyTLSPath, &tlsApp)
	if err != nil || !found || tlsApp.Automation == nil || tlsApp.Automation.Policies == nil {
		return nil, err
	}
	return *tlsApp.Automation.Policies, nil
}

// caddyPolicyFor is the index of the first policy that matches host; -1
// when none does.
func caddyPolicyFor(policies []caddyTLSPolicy, host string) int {
	for i, policy := range policies {
		if policy.matches(host) {
			return i
		}
	}
	return -1
}

// repairTLSHosts moves managed hostnames that sit as exact subjects in a
// DNS-only policy and have no certificate to the HTTP policy, and adds
// managed hostnames no policy covers. Wildcard subjects and the hostnames
// they cover are left alone. It returns the hostnames moved.
func (c nativeCaddy) repairTLSHosts(ctx context.Context, policies []caddyTLSPolicy, hosts []string, hasCertificate func(string) bool) ([]string, error) {
	move := map[int][]string{}
	var moved, missing []string
	for _, host := range hosts {
		i := caddyPolicyFor(policies, host)
		if i < 0 {
			missing = append(missing, host)
			continue
		}
		policy := policies[i]
		exact := policy.Subjects != nil && slices.ContainsFunc(*policy.Subjects, func(subject string) bool { return strings.EqualFold(subject, host) })
		if exact && !policy.OnDemand && policy.dnsOnly() && !hasCertificate(host) {
			move[i] = append(move[i], host)
			moved = append(moved, host)
		}
	}
	if len(moved) == 0 && len(missing) == 0 {
		return nil, nil
	}
	// Remove first: Caddy refuses a configuration in which two policies
	// list the same subject. From the last policy down, so a deleted
	// policy shifts no index still to be visited.
	indices := make([]int, 0, len(move))
	for i := range move {
		indices = append(indices, i)
	}
	slices.Sort(indices)
	for _, i := range slices.Backward(indices) {
		if err := c.removePolicySubjects(ctx, i, *policies[i].Subjects, move[i]); err != nil {
			return nil, err
		}
	}
	if _, err := c.ensureTLSSubjects(ctx, append(moved, missing...)); err != nil {
		return moved, err
	}
	return moved, nil
}

// ---- certificates in Caddy's storage ------------------------------------------

// caddyCertificateStored reports whether Caddy's file storage under root
// holds an unexpired certificate for host: its own, or a wildcard of its
// parent.
func caddyCertificateStored(root, host string, now time.Time) bool {
	if root == "" || !controlHostnamePattern.MatchString(host) {
		return false
	}
	names := []string{host}
	if _, parent, ok := strings.Cut(host, "."); ok && strings.Contains(parent, ".") {
		names = append(names, "wildcard_."+parent)
	}
	issuers, err := os.ReadDir(filepath.Join(root, "certificates"))
	if err != nil {
		return false
	}
	for _, name := range names {
		for _, issuer := range issuers {
			if !issuer.IsDir() || !controlIssuerPattern.MatchString(issuer.Name()) {
				continue
			}
			raw, err := readRegular(filepath.Join(root, "certificates", issuer.Name(), name, name+".crt"), 1<<20)
			if err != nil {
				continue
			}
			block, _ := pem.Decode(raw)
			if block == nil {
				continue
			}
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil && now.Before(cert.NotAfter) {
				return true
			}
		}
	}
	return false
}

// caddyCertState is the certificate status of a managed hostname and since
// when it has had none.
type caddyCertState struct {
	Status  DomainStatus
	Missing time.Time
}

// caddyCertificateReason explains why host has no certificate, and whether
// the reason is structural (reported at once) rather than issuance that may
// still be in progress.
func caddyCertificateReason(ctx context.Context, policies []caddyTLSPolicy, host string) (reason string, structural bool) {
	i := caddyPolicyFor(policies, host)
	switch {
	case i < 0:
		return "no Caddy TLS automation policy lists this hostname", true
	case policies[i].dnsOnly():
		provider := policies[i].dnsProvider()
		if provider == "" {
			provider = "configured"
		}
		return "Caddy validates this hostname only through the " + provider + " DNS provider, which must host its DNS zone", true
	}
	lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
	ips, err := caddyTLSLookup(lookup, "ip", host)
	cancel()
	if err != nil || len(ips) == 0 {
		return "its DNS has no A or AAAA record yet", false
	}
	return "Caddy has not obtained one yet; check that DNS points to this server and ports 80 and 443 are reachable", false
}

// ---- report -------------------------------------------------------------------

// CaddyTLSReport is the latest TLS automation upkeep, shown in the server's
// web ingress view.
type CaddyTLSReport struct {
	Checked time.Time `json:"checked"`
	// Fallbacks are the policies that validate through a DNS provider and
	// fall back to HTTP validation.
	Fallbacks []CaddyTLSFallback `json:"fallbacks,omitempty"`
	// Moved are managed hostnames moved out of a DNS-only policy since the
	// agent started.
	Moved []string `json:"moved,omitempty"`
	// NotIssued are managed hostnames without a certificate.
	NotIssued []string `json:"not_issued,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// CaddyTLSFallback is one policy with DNS validation and an HTTP fallback.
type CaddyTLSFallback struct {
	Policy   int      `json:"policy"`
	Provider string   `json:"provider,omitempty"`
	Subjects int      `json:"subjects"`
	Examples []string `json:"examples,omitempty"`
	// Added reports that this agent added the fallback (since it started).
	Added bool `json:"added,omitempty"`
}

func caddyTLSFallbacks(policies []caddyTLSPolicy, added []int) []CaddyTLSFallback {
	var out []CaddyTLSFallback
	for i, policy := range policies {
		if !policy.hasDNSIssuer() || !policy.httpCapable() {
			continue
		}
		fallback := CaddyTLSFallback{Policy: i, Provider: policy.dnsProvider(), Added: slices.Contains(added, i)}
		if policy.Subjects != nil {
			fallback.Subjects = len(*policy.Subjects)
			fallback.Examples = slices.Clone((*policy.Subjects)[:min(len(*policy.Subjects), 5)])
		}
		out = append(out, fallback)
	}
	return out
}

func (m *Manager) caddyTLSReport() *CaddyTLSReport {
	m.caddyTLSMu.Lock()
	defer m.caddyTLSMu.Unlock()
	if m.caddyTLS == nil {
		return nil
	}
	report := *m.caddyTLS
	report.Fallbacks = slices.Clone(report.Fallbacks)
	report.Moved = slices.Clone(report.Moved)
	report.NotIssued = slices.Clone(report.NotIssued)
	return &report
}

// caddyDomainStatuses returns the certificate status of each of hosts that
// the upkeep checked; nil when it checked none.
func (m *Manager) caddyDomainStatuses(hosts []string) []DomainStatus {
	m.caddyTLSMu.Lock()
	defer m.caddyTLSMu.Unlock()
	var out []DomainStatus
	for _, host := range hosts {
		if state, ok := m.caddyCerts[strings.ToLower(host)]; ok {
			out = append(out, state.Status)
		}
	}
	return out
}

// ---- upkeep -------------------------------------------------------------------

// caddyManagedHosts lists the hostnames this agent publishes to Caddy: the
// created applications' (with their additional domains) and the redirect
// domains, which get a certificate status, and the panel's hostname.
func (m *Manager) caddyManagedHosts() (statusHosts, all []string) {
	add := func(list *[]string, host string) {
		host = strings.ToLower(host)
		if host != "" && !slices.Contains(*list, host) {
			*list = append(*list, host)
		}
	}
	m.mu.Lock()
	for id := range m.data.NativeCreated {
		a, found := m.data.NativeApps[id]
		if !found || a.Domain == "" {
			continue
		}
		for _, host := range append([]string{a.Domain}, a.AdditionalDomains...) {
			add(&statusHosts, host)
		}
	}
	for id, list := range m.data.Redirects {
		if _, ok := m.appHostnamesLocked(id); !ok {
			continue
		}
		for _, d := range list {
			add(&statusHosts, d.Host)
		}
	}
	m.mu.Unlock()
	slices.Sort(statusHosts)
	all = slices.Clone(statusHosts)
	if s := m.controlState(); s != nil && s.Route == "caddy" && s.TLSSubject && s.Phase != "" {
		add(&all, s.Hostname)
	}
	return statusHosts, all
}

// reconcileCaddyTLS applies the HTTP fallbacks, repairs managed hostnames
// and refreshes their certificate status.
func (m *Manager) reconcileCaddyTLS(ctx context.Context) {
	caddy, ok := m.nativeCaddy(ctx)
	if !ok {
		m.caddyTLSMu.Lock()
		m.caddyTLS, m.caddyCerts = nil, nil
		m.caddyTLSMu.Unlock()
		return
	}
	now := time.Now().UTC()
	report := &CaddyTLSReport{Checked: now}
	statusHosts, managed := m.caddyManagedHosts()
	root := caddyTLSStorageRoot(ctx, m)
	hasCertificate := func(host string) bool { return caddyCertificateStored(root, host, now) }
	policies, err := caddy.tlsPolicies(ctx)
	var problems []error
	var added []int
	if err == nil {
		// A refused fallback leaves the repair below to move the managed
		// hostnames out of that policy.
		var fallbackErr error
		added, fallbackErr = caddy.applyTLSFallbacks(ctx, policies)
		problems = append(problems, fallbackErr)
		// Without the storage nothing is known about certificates: no
		// hostname is moved and no status is claimed.
		if root != "" {
			var moved []string
			var repairErr error
			moved, repairErr = caddy.repairTLSHosts(ctx, policies, managed, hasCertificate)
			problems = append(problems, repairErr)
			for _, host := range moved {
				log.Printf("caddy tls: moved %s from a DNS-only policy to HTTP validation", host)
			}
			report.Moved = moved
			policies, err = caddy.tlsPolicies(ctx)
		}
	}
	problems = append(problems, err)
	if joined := errors.Join(problems...); joined != nil {
		report.Error = boundedErrorText(joined.Error(), 300)
		log.Printf("caddy tls: %s", report.Error)
	}
	report.Fallbacks = caddyTLSFallbacks(policies, added)

	m.caddyTLSMu.Lock()
	previous := m.caddyCerts
	if m.caddyTLS != nil {
		for _, host := range m.caddyTLS.Moved {
			if !slices.Contains(report.Moved, host) && len(report.Moved) < 50 {
				report.Moved = append(report.Moved, host)
			}
		}
		for _, fallback := range m.caddyTLS.Fallbacks {
			for i := range report.Fallbacks {
				if report.Fallbacks[i].Policy == fallback.Policy && fallback.Added {
					report.Fallbacks[i].Added = true
				}
			}
		}
	}
	m.caddyTLSMu.Unlock()

	certs := map[string]caddyCertState{}
	if root != "" && err == nil {
		for _, host := range statusHosts {
			status := DomainStatus{Domain: host, State: "ready", Checked: now, RecheckSeconds: int(caddyTLSInterval / time.Second)}
			state := caddyCertState{}
			if !hasCertificate(host) {
				state.Missing = now
				if before, ok := previous[host]; ok && !before.Missing.IsZero() {
					state.Missing = before.Missing
				}
				reason, structural := caddyCertificateReason(ctx, policies, host)
				if i := caddyPolicyFor(policies, host); i >= 0 && policies[i].OnDemand {
					// Issued at the first HTTPS request, which may not have come.
					status.State, status.Message = "requesting_ssl", "Caddy issues the certificate on demand at the first HTTPS request."
				} else if structural || now.Sub(state.Missing) >= caddyCertificateGrace {
					status.State, status.Message = "certificate_not_issued", "Certificate not issued: "+reason+"."
					report.NotIssued = append(report.NotIssued, host)
				} else {
					status.State, status.Message = "requesting_ssl", "Requesting HTTPS certificate; "+reason+"."
				}
			}
			state.Status = status
			certs[host] = state
		}
	}
	m.caddyTLSMu.Lock()
	m.caddyTLS, m.caddyCerts = report, certs
	m.caddyTLSMu.Unlock()
}

func (m *Manager) caddyTLSLoop() {
	defer m.wg.Done()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-timer.C:
		}
		ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
		m.reconcileCaddyTLS(ctx)
		cancel()
		timer.Reset(caddyTLSInterval)
	}
}
