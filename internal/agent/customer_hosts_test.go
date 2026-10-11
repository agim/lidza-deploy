package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func customerApp(t *testing.T, m *Manager, id string, h http.HandlerFunc) App {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	a := testApp(id)
	a.CustomerHosts = true
	a.Current = &Release{ID: id + "-release", Port: port}
	m.mu.Lock()
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	return a
}
func expireCustomerHost(m *Manager, host string) {
	m.hostMu.Lock()
	v := m.hostCache[host]
	v.expires = time.Now().Add(-time.Second)
	m.hostCache[host] = v
	m.hostMu.Unlock()
}
func TestCustomerHostsApprovalRoutingAndInvalidation(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	var calls atomic.Int32
	var allowed atomic.Bool
	allowed.Store(true)
	a := customerApp(t, m, "customer", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tls-allow" {
			calls.Add(1)
			if r.Host != "customer.example.com" || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
				t.Error("approval forwarded visitor headers or wrong Host")
			}
			if !allowed.Load() || r.URL.Query().Get("domain") != "shop.customer.example" {
				http.NotFound(w, r)
			}
			return
		}
		fmt.Fprint(w, r.Host)
	})
	if !m.domainAllowed(context.Background(), "shop.customer.example") || m.Target("shop.customer.example") == nil {
		t.Fatal("approved host unavailable")
	}
	if calls.Load() != 1 {
		t.Fatal("approval not shared by issuance and routing")
	}
	rec := httptest.NewRecorder()
	Proxy(m).ServeHTTP(rec, httptest.NewRequest("GET", "http://shop.customer.example/", nil))
	if rec.Code != 200 || rec.Body.String() != "shop.customer.example" {
		t.Fatal("Host was not routed intact", rec.Code, rec.Body.String())
	}
	if m.domainAllowed(context.Background(), "evil.customer.example") {
		t.Fatal("refused hostname authorized")
	}
	count := calls.Load()
	if m.Target("evil.customer.example") != nil || calls.Load() != count {
		t.Fatal("negative cache not used")
	}
	allowed.Store(false)
	expireCustomerHost(m, "shop.customer.example")
	if m.Target("shop.customer.example") != nil {
		t.Fatal("revocation ignored after TTL")
	}
	allowed.Store(true)
	expireCustomerHost(m, "shop.customer.example")
	if m.Target("shop.customer.example") == nil {
		t.Fatal("approval not rechecked")
	}
	m.mu.Lock()
	a.CustomerHosts = false
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	if m.Target("shop.customer.example") != nil {
		t.Fatal("opt-out did not invalidate cache")
	}
	m.mu.Lock()
	a.CustomerHosts = true
	a.Stopped = true
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	if m.Target("shop.customer.example") != nil || m.domainAllowed(context.Background(), "shop.customer.example") {
		t.Fatal("stopped app authorized")
	}
	m.mu.Lock()
	a.Stopped = false
	a.Current = &Release{ID: "next", Port: a.Current.Port}
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	count = calls.Load()
	if m.Target("shop.customer.example") == nil || calls.Load() != count+1 {
		t.Fatal("release change retained old verdict")
	}
	if err := m.setMaintenance(a.ID, Maintenance{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	Proxy(m).ServeHTTP(rec, httptest.NewRequest("GET", "http://shop.customer.example/", nil))
	if rec.Code != 503 {
		t.Fatal("customer hostname bypassed maintenance")
	}
	for _, p := range []string{"/tls-allow?domain=shop.customer.example", "/tls-allow/", "/x/../tls-allow", "/metrics", "/readyz", "/healthz"} {
		rec = httptest.NewRecorder()
		Proxy(m).ServeHTTP(rec, httptest.NewRequest("GET", "http://shop.customer.example"+p, nil))
		if rec.Code != 404 {
			t.Fatalf("operator path %s exposed: %d", p, rec.Code)
		}
	}
}
func TestCustomerHostsReservationsAmbiguityAndValidation(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	var calls atomic.Int32
	hook := func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }
	a := customerApp(t, m, "owner", hook)
	reserved := testApp("reserved")
	reserved.Domain = "shop.customer.example"
	reserved.Stopped = true
	m.mu.Lock()
	m.data.Apps[reserved.ID] = reserved
	m.mu.Unlock()
	if m.Target(reserved.Domain) != nil || m.domainAllowed(context.Background(), reserved.Domain) != true || calls.Load() != 0 {
		t.Fatal("registered domain fell through to customer's hook")
	}
	// Removing the registry entry invalidates its reservation; ambiguity fails closed.
	m.mu.Lock()
	delete(m.data.Apps, reserved.ID)
	m.mu.Unlock()
	customerApp(t, m, "other", hook)
	if m.Target("shop.customer.example") != nil || m.domainAllowed(context.Background(), "shop.customer.example") {
		t.Fatal("ambiguous ownership accepted")
	}
	count := calls.Load()
	for _, host := range []string{"", "127.0.0.1", "[::1]", "*.example.com", "host.example:443", "bad..example", "-bad.example", "a.example/secret", "a.example\n", "example.com."} {
		if m.Target(host) != nil || m.domainAllowed(context.Background(), host) {
			t.Fatalf("invalid host accepted: %q", host)
		}
	}
	if calls.Load() != count {
		t.Fatal("malformed name reached hook")
	}
	m.mu.Lock()
	a.Preview = true
	m.data.Apps[a.ID] = a
	other := m.data.Apps["other"]
	other.CustomerHosts = false
	m.data.Apps[other.ID] = other
	m.mu.Unlock()
	if m.Target("new.customer.example") != nil {
		t.Fatal("PR preview authorized customer host")
	}
}
func TestCustomerHostsProtocolFailureAndBoundedConcurrency(t *testing.T) {
	for _, mode := range []string{"html", "redirect", "error"} {
		t.Run(mode, func(t *testing.T) {
			m := testManager(t, &fakeRuntime{})
			customerApp(t, m, "protocol", func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "html":
					fmt.Fprint(w, "<html>SPA fallback</html>")
				case "redirect":
					http.Redirect(w, r, "http://127.0.0.1:1/", 302)
				case "error":
					w.WriteHeader(500)
				}
			})
			if m.Target("shop.customer.example") != nil {
				t.Fatal("nonapproval accepted")
			}
		})
	}
	m := testManager(t, &fakeRuntime{})
	var calls atomic.Int32
	entered := make(chan struct{})
	gate := make(chan struct{})
	customerApp(t, m, "shared", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-gate
	})
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { m.Target("shop.customer.example") })
	}
	<-entered
	time.Sleep(30 * time.Millisecond)
	close(gate)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("parallel checks did not coalesce", calls.Load())
	}
	// A settings/release change while a probe is in flight cannot authorize it.
	m2 := testManager(t, &fakeRuntime{})
	entered = make(chan struct{})
	gate = make(chan struct{})
	a := customerApp(t, m2, "changed", func(w http.ResponseWriter, r *http.Request) { close(entered); <-gate })
	done := make(chan *Release)
	go func() { done <- m2.Target("shop.customer.example") }()
	<-entered
	m2.mu.Lock()
	a.CustomerHosts = false
	m2.data.Apps[a.ID] = a
	m2.mu.Unlock()
	close(gate)
	if <-done != nil {
		t.Fatal("in-flight opt-out ignored")
	}
	// Bounded queue: saturating approval work denies immediately without a probe.
	for range cap(m.hostSlots) {
		m.hostSlots <- struct{}{}
	}
	if m.Target("busy.customer.example") != nil {
		t.Fatal("saturated gate approved")
	}
	for range cap(m.hostSlots) {
		<-m.hostSlots
	}
	m.hostMu.Lock()
	m.hostCache = map[string]hostVerdict{}
	for i := range customerHostCacheLimit {
		m.hostCache[fmt.Sprint(i)] = hostVerdict{expires: time.Now().Add(time.Hour)}
	}
	m.hostMu.Unlock()
	m.Target("bounded.customer.example")
	if len(m.hostCache) > customerHostCacheLimit {
		t.Fatal("cache unbounded", len(m.hostCache))
	}
}
func TestCustomerHostsSettingPersistsAndOmissionKeepsIt(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("settings-hosts")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	enabled := true
	if err := m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, CustomerHosts: &enabled}); err != nil {
		t.Fatal(err)
	}
	if err := m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain}); err != nil {
		t.Fatal(err)
	}
	s, err := m.Settings(a.ID)
	if err != nil || !s.CustomerHosts {
		t.Fatal("opt-in lost", s, err)
	}
	restarted, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	s, _ = restarted.Settings(a.ID)
	if !s.CustomerHosts {
		t.Fatal("opt-in not persisted")
	}
}

func TestCustomerHostApprovalDeadline(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	customerApp(t, m, "slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	start := time.Now()
	if m.Target("slow.customer.example") != nil {
		t.Fatal("timed-out hook approved a host")
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatal("approval exceeded five-second deadline", elapsed)
	}
}
