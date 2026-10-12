package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDomainDNSGateRetriesAndInvalidatesEditedDomain(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("dns")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(Proxy(m))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	pointsHere := false
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		if !pointsHere {
			return nil, errors.New("not yet")
		}
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	calls := 0
	issue := func(ctx context.Context, domain string) error {
		calls++
		if m.domains[a.ID].State != "requesting_ssl" {
			t.Fatal("no requesting state")
		}
		if calls == 1 {
			return errors.New("CA unavailable")
		}
		return nil
	}
	m.checkAppDomain(a, port, lookup, issue)
	if calls != 0 || m.domains[a.ID].State != "waiting_dns" {
		t.Fatal("issued before DNS")
	}
	pointsHere = true
	m.checkAppDomain(a, port, lookup, issue)
	if calls != 1 || m.domains[a.ID].State != "requesting_ssl" {
		t.Fatal("failed issuance not pending")
	}
	m.checkAppDomain(a, port, lookup, issue)
	if calls != 2 || m.domains[a.ID].State != "ready" {
		t.Fatal("issuance did not retry")
	}
	m.mu.Lock()
	edited := m.data.Apps[a.ID]
	edited.Domain = "edited.example.com"
	m.data.Apps[a.ID] = edited
	m.cfg.TLSListen = "127.0.0.1:443"
	m.mu.Unlock()
	if status := m.Apps()[0].DomainStatus; status.Domain != edited.Domain || status.State != "waiting_dns" {
		t.Fatal("stale domain status", status)
	}
	m.setDomainStatus(a, DomainStatus{Domain: a.Domain, State: "ready"})
	if m.Apps()[0].DomainStatus.State == "ready" {
		t.Fatal("stale asynchronous result applied")
	}
	pointsHere = false
	m.checkAppDomain(edited, port, lookup, issue)
	if calls != 2 {
		t.Fatal("edited FQDN skipped DNS gate")
	}
	pointsHere = true
	m.checkAppDomain(edited, port, lookup, issue)
	if calls != 3 || m.domains[a.ID].Domain != edited.Domain || m.domains[a.ID].State != "ready" {
		t.Fatal("edited FQDN did not obtain SSL after DNS changed")
	}
	req := httptest.NewRequest("GET", "http://"+a.Domain+domainProbePath+"?nonce="+newID(), nil)
	w := httptest.NewRecorder()
	Proxy(m).ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatal("old domain still verified")
	}
}

func TestDomainAddressProofsRunConcurrently(t *testing.T) {
	barrier := make(chan struct{})
	var arrived atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) == 2 {
			close(barrier)
		}
		select {
		case <-barrier:
			w.Write([]byte(challengeProof("proof", r.URL.Query().Get("nonce"))))
		case <-r.Context().Done():
		}
	}))
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.2")}, nil
	}
	if err := checkDomainRoute(ctx, "app.example.com", "proof", lookup, port); err != nil {
		t.Fatal("multiple addresses were not checked concurrently", err)
	}
	if arrived.Load() != 2 {
		t.Fatal("an address was skipped")
	}
}

func TestDomainRetryDoesNotWaitForSlowDomains(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	for _, id := range []string{"a-slow", "z-pending", "ready"} {
		if err := m.Upsert(testApp(id)); err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		a := m.data.Apps[id]
		a.Current = &Release{ID: "fixture"}
		m.data.Apps[id] = a
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.domains["ready"] = DomainStatus{Domain: "ready.example.com", State: "ready", Checked: time.Now().Add(-2 * time.Minute)}
	m.mu.Unlock()
	type event struct {
		id string
		at time.Time
	}
	events := make(chan event, 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		m.runDomainChecks(ctx, 5*time.Millisecond, func(work context.Context, a App) {
			events <- event{a.ID, time.Now()}
			if a.ID == "a-slow" {
				<-work.Done()
				return
			}
			m.setDomainStatus(a, DomainStatus{Domain: a.Domain, State: "waiting_dns", Checked: time.Now()})
		})
	}()
	var first time.Time
	deadline := time.After(12 * time.Second)
	for {
		select {
		case event := <-events:
			if event.id == "ready" {
				t.Fatal("healthy domain checked before its five-minute interval")
			}
			if event.id != "z-pending" {
				continue
			}
			if first.IsZero() {
				first = event.at
				continue
			}
			elapsed := event.at.Sub(first)
			if elapsed < 10*time.Second || elapsed > 11*time.Second {
				t.Fatal("pending retry interval", elapsed)
			}
			cancel()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("domain scheduler did not stop")
			}
			return
		case <-deadline:
			t.Fatal("slow domain blocked another domain's retry")
		}
	}
}

func TestDomainChecksBoundConcurrencyCancelEditsAndAvoidOverlap(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("domain-%02d", i)
		if err := m.Upsert(testApp(id)); err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		a := m.data.Apps[id]
		a.Current = &Release{ID: "fixture"}
		m.data.Apps[id] = a
		m.mu.Unlock()
	}
	type event struct{ id, domain string }
	started := make(chan event, 30)
	var active, maxActive atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		m.runDomainChecks(ctx, time.Millisecond, func(work context.Context, a App) {
			count := active.Add(1)
			defer active.Add(-1)
			for {
				old := maxActive.Load()
				if count <= old || maxActive.CompareAndSwap(old, count) {
					break
				}
			}
			started <- event{a.ID, a.Domain}
			<-work.Done()
		})
	}()
	for i := 0; i < 8; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("checks did not start")
		}
	}
	m.wakeDomains()
	select {
	case extra := <-started:
		t.Fatal("overlapping or unbounded check", extra)
	case <-time.After(30 * time.Millisecond):
	}
	m.mu.Lock()
	a := m.data.Apps["domain-00"]
	a.Domain = "edited.example.com"
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	m.wakeDomains()
	select {
	case updated := <-started:
		if updated.id != a.ID || updated.domain != a.Domain {
			t.Fatal("edited domain did not immediately replace canceled check", updated)
		}
	case <-time.After(time.Second):
		t.Fatal("edited domain check was not canceled")
	}
	if maxActive.Load() > 8 {
		t.Fatal("domain work exceeded concurrency limit")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("pending checks did not stop")
	}
}

func TestDomainProofRejectsWrongHostRedirectAndMixedAddresses(t *testing.T) {
	for _, mode := range []string{"wrong", "redirect", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, "https://elsewhere.example", http.StatusFound)
					return
				}
				if mode == "mixed" {
					w.Write([]byte(challengeProof("proof", r.URL.Query().Get("nonce"))))
					return
				}
				w.Write([]byte("someone else"))
			}))
			defer server.Close()
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			lookup := func(context.Context, string, string) ([]net.IP, error) {
				ips := []net.IP{net.ParseIP("127.0.0.1")}
				if mode == "mixed" {
					ips = append(ips, net.ParseIP("127.0.0.2"))
				}
				return ips, nil
			}
			if err := checkDomainRoute(context.Background(), "app.example.com", "proof", lookup, port); err == nil {
				t.Fatal("wrong server accepted")
			} else if mode == "mixed" && !strings.Contains(err.Error(), net.JoinHostPort("127.0.0.2", port)) {
				t.Fatal("failing address was not identified", err)
			}
		})
	}
}
