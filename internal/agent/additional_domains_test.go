package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAdditionalDomainsValidationAndOwnership(t *testing.T) {
	a := testApp("domains")
	a.AdditionalDomains = []string{"www.example.com", "other.example.org"}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, names := range [][]string{{a.Domain}, {"same.example.com", "same.example.com"}, {"https://example.com"}, {"*.example.com"}, {"127.0.0.1"}, {"example.com:443"}, {"Bad.example.com"}, {strings.Repeat("a", 64) + ".example.com"}} {
		invalid := a
		invalid.AdditionalDomains = names
		if err := invalid.Validate(); err == nil {
			t.Fatal("invalid aliases accepted", names)
		}
	}
	invalid := a
	invalid.AdditionalDomains = make([]string, 21)
	if err := invalid.Validate(); err == nil {
		t.Fatal("unbounded domains accepted")
	}
	m := testManager(t, &fakeRuntime{})
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	b := testApp("other")
	b.Domain = a.AdditionalDomains[0]
	if err := m.Upsert(b); err == nil {
		t.Fatal("alias stolen as primary")
	}
	b.Domain = "different.example.net"
	b.AdditionalDomains = []string{a.Domain}
	if err := m.Upsert(b); err == nil {
		t.Fatal("primary stolen as alias")
	}
	b.AdditionalDomains = []string{a.AdditionalDomains[1]}
	if err := m.Upsert(b); err == nil {
		t.Fatal("alias stolen as alias")
	}
	// Older clients omitting aliases must preserve them; [] explicitly removes them.
	a.AdditionalDomains = nil
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if len(m.Apps()[0].AdditionalDomains) != 2 {
		t.Fatal("legacy update lost aliases")
	}
	empty := []string{}
	if err := m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, AdditionalDomains: &empty}); err != nil {
		t.Fatal(err)
	}
	if err := m.Upsert(b); err != nil {
		t.Fatal("removed alias remained reserved", err)
	}
}

func TestAdditionalDomainsRoutePersistAndRetireWithoutReload(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("routing")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.Host)) }))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	a.Current = &Release{ID: "live", Port: port}
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	aliases := []string{"alias.example.com", "second.example.org"}
	if err := m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, AdditionalDomains: &aliases}); err != nil {
		t.Fatal(err)
	}
	if len(m.Deployments()) != 0 {
		t.Fatal("alias edit queued an unnecessary app reload")
	}
	proxy := Proxy(m)
	request := func(host string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://"+host+"/", nil)
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, r)
		return w
	}
	for _, host := range append([]string{a.Domain}, aliases...) {
		w := request(host)
		if w.Code != 200 || w.Body.String() != host {
			t.Fatal("alias route/Host lost", host, w.Code, w.Body.String())
		}
		if !m.domainAllowed(context.Background(), host) {
			t.Fatal("registered alias denied certificate", host)
		}
	}
	if m.domainAllowed(context.Background(), "unknown.example.com") {
		t.Fatal("unknown hostname authorized")
	}
	m.mu.Lock()
	stopped := m.data.Apps[a.ID]
	stopped.Stopped = true
	m.data.Apps[a.ID] = stopped
	m.mu.Unlock()
	if w := request(aliases[0]); w.Code != 503 {
		t.Fatal("alias bypassed stopped state", w.Code)
	}
	m.mu.Lock()
	stopped.Stopped = false
	m.data.Apps[a.ID] = stopped
	m.mu.Unlock()
	if err := m.setMaintenance(a.ID, Maintenance{Enabled: true, Message: "maintenance"}); err != nil {
		t.Fatal(err)
	}
	if w := request(aliases[0]); w.Code != 503 {
		t.Fatal("alias bypassed maintenance", w.Code)
	}
	m.Close()
	reopened, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !slices.Equal(reopened.Apps()[0].AdditionalDomains, aliases) {
		t.Fatal("restart lost aliases")
	}
	if err := reopened.Retire(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if reopened.domainAllowed(context.Background(), aliases[0]) {
		t.Fatal("removed app still authorized alias")
	}
}

func TestAliasDomainMonitorDoesNotWaitForBlockedPrimary(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("independent")
	a.AdditionalDomains = []string{"alias.example.org"}
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	a.Current = &Release{ID: "current"}
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := make(chan string, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runDomainChecks(ctx, time.Millisecond, func(ctx context.Context, item App) {
			if item.Domain == a.Domain {
				<-ctx.Done()
				return
			}
			m.setDomainStatus(item, DomainStatus{Domain: item.Domain, State: "ready", Checked: time.Now()})
			seen <- item.Domain
		})
	}()
	select {
	case host := <-seen:
		if host != a.AdditionalDomains[0] {
			t.Fatal(host)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("alias blocked behind primary DNS check")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("domain monitor did not stop")
	}
	m.mu.Lock()
	status := m.domains[domainStatusKey(a, a.AdditionalDomains[0])]
	m.mu.Unlock()
	if status.State != "ready" {
		t.Fatal("alias status missing", status)
	}
}

func TestAdditionalDomainDNSChecksAreIndependentAndRemovedChecksIgnored(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("dns-alias")
	a.AdditionalDomains = []string{"alias.example.com"}
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(Proxy(m))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	alias := a
	alias.Domain = a.AdditionalDomains[0]
	alias.domainCheckKey = domainStatusKey(a, alias.Domain)
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	issued := []string{}
	m.checkAppDomain(alias, port, lookup, func(ctx context.Context, host string) error { issued = append(issued, host); return nil })
	m.mu.Lock()
	m.cfg.TLSListen = ":443"
	statuses := m.domainStatusesLocked(a)
	m.mu.Unlock()
	if len(statuses) != 2 || statuses[0].State != "waiting_dns" || statuses[1].State != "ready" || !slices.Equal(issued, a.AdditionalDomains) {
		t.Fatal("alias status overwrote primary or TLS failed", statuses, issued)
	}
	empty := []string{}
	if err := m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, AdditionalDomains: &empty}); err != nil {
		t.Fatal(err)
	}
	m.setDomainStatus(alias, DomainStatus{Domain: alias.Domain, State: "ready", Checked: time.Now()})
	if _, ok := registeredHost(m.Apps(), alias.Domain); ok {
		t.Fatal("removed alias remains routed")
	}
	r := httptest.NewRequest("GET", "http://"+alias.Domain+domainProbePath+"?nonce="+newID(), nil)
	w := httptest.NewRecorder()
	Proxy(m).ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("removed alias still proves DNS ownership", w.Code)
	}
}
