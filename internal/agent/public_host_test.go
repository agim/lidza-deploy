package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// closedPort is a loopback port nothing listens on: a probe of it fails
// like the probe of a public address the router does not loop back.
func closedPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	listener.Close()
	return port
}

func TestHostnameOnTheServersOwnPublicAddressPassesWithoutLoopback(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	previous := publicHostLookup
	t.Cleanup(func() { publicHostLookup = previous })
	publicHostLookup = func(_ context.Context, host string) ([]net.IP, error) {
		if host != "server.example.com" {
			t.Fatalf("looked up %q", host)
		}
		return []net.IP{net.ParseIP("192.0.2.10")}, nil
	}
	port := closedPort(t)
	// 192.0.2.10 (a documentation address) stands for the public address:
	// like one behind a router without loopback, it cannot be connected to.
	here := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("192.0.2.10")}, nil
	}
	elsewhere := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.2")}, nil
	}
	check := func(lookup func(context.Context, string, string) ([]net.IP, error)) error {
		return checkDomainRouteVia(context.Background(), "app.example.com", "proof", lookup, port, m.ownPublicAddress)
	}
	// Until the control panel has reached the agent by name, nothing is known.
	if err := check(here); err == nil {
		t.Fatal("an unproven hostname passed without a known public name")
	}
	// An unauthenticated request teaches nothing.
	h := Handler(m)
	r := httptest.NewRequest("GET", "/v1/apps", nil)
	r.Host = "server.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || m.publicHostKnown() != "" {
		t.Fatalf("unauthenticated request: %d, public name %q", w.Code, m.publicHostKnown())
	}
	// Neither does an authenticated request to an address or a loopback name.
	for _, host := range []string{"127.0.0.1:9090", "localhost", "203.0.113.4", "[2001:db8::1]:443"} {
		r = httptest.NewRequest("GET", "/v1/apps", nil)
		r.Host = host
		r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
		h.ServeHTTP(httptest.NewRecorder(), r)
		if m.publicHostKnown() != "" {
			t.Fatalf("%s was taken as the public name", host)
		}
	}
	r = httptest.NewRequest("GET", "/v1/apps", nil)
	r.Host = "Server.Example.com"
	r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusUnauthorized || m.publicHostKnown() != "server.example.com" {
		t.Fatalf("authenticated request: %d, public name %q", w.Code, m.publicHostKnown())
	}
	// The hostname resolves to the server's own public address: it leads
	// here although the probe from here cannot connect.
	if err := check(here); err != nil {
		t.Fatal(err)
	}
	// A hostname that resolves elsewhere still has to answer the probe.
	if err := check(elsewhere); err == nil {
		t.Fatal("a hostname on another address passed")
	}
	// The name survives a restart of the agent.
	m.publicHost.Store(nil)
	if m.publicHostKnown() != "server.example.com" {
		t.Fatalf("after a restart: %q", m.publicHostKnown())
	}
}
