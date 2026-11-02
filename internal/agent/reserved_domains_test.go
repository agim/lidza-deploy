package agent

import (
	"context"
	"testing"
)

func TestReservedDomainsProtectAssignmentsCustomerHostsAndCertificates(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.cfg.ReservedDomains = []string{"native.example.com", "*.tenants.example.com"}
	a := testApp("go-app")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"native.example.com", "shop.tenants.example.com", "deep.shop.tenants.example.com"} {
		b := testApp("candidate")
		b.Domain = domain
		if err := m.Upsert(b); err == nil {
			t.Fatal("reserved primary accepted", domain)
		}
		b.Domain = "candidate.example.com"
		b.AdditionalDomains = []string{domain}
		if err := m.Upsert(b); err == nil {
			t.Fatal("reserved alias accepted", domain)
		}
		aliases := []string{domain}
		if err := m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, AdditionalDomains: &aliases}); err == nil {
			t.Fatal("settings bypassed reservation", domain)
		}
		if _, ok := m.hostApplication(context.Background(), domain); ok {
			t.Fatal("reserved customer host routed", domain)
		}
		if m.domainAllowed(context.Background(), domain) {
			t.Fatal("reserved certificate authorized", domain)
		}
	}
	if m.cfg.reservedHostname("unrelated.example.com") || m.cfg.reservedHostname("nottenants.example.com") {
		t.Fatal("reservation matched outside subtree")
	}
	// Even stale inventory cannot bypass the routing/certificate deny rule.
	m.mu.Lock()
	b := a
	b.Domain = "native.example.com"
	m.data.Apps[a.ID] = b
	m.mu.Unlock()
	if _, ok := m.hostApplication(context.Background(), b.Domain); ok {
		t.Fatal("stale route bypassed reservation")
	}
	if m.domainAllowed(context.Background(), b.Domain) {
		t.Fatal("stale record bypassed certificate reservation")
	}
}

func TestReservedDomainValidation(t *testing.T) {
	for _, names := range [][]string{{"UPPER.example.com"}, {"*.127.0.0.1"}, {"x.example.com", "x.example.com"}, {"*.*.example.com"}, {"https://example.com"}} {
		if err := validateReservedDomains(names); err == nil {
			t.Fatal("invalid reservation accepted", names)
		}
	}
	if err := validateReservedDomains([]string{"native.example.com", "*.tenant.example.com"}); err != nil {
		t.Fatal(err)
	}
}
