package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
			}
		})
	}
}
