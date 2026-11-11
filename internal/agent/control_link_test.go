package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agentlink"
)

func TestControlLinkConnectsOutAndServesTheAPI(t *testing.T) {
	hub := agentlink.NewHub()
	defer hub.Close()
	token := strings.Repeat("a", 32)
	panel := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != agentlink.Path || r.Header.Get(agentlink.ServerHeader) != "old" || !agentlink.Authorized(r, token) {
			http.Error(w, "no", 401)
			return
		}
		hub.Serve(w, r, "old")
	}))
	defer panel.Close()
	pool := x509.NewCertPool()
	pool.AddCert(panel.Certificate())
	old := controlLinkTLS
	controlLinkTLS = &tls.Config{RootCAs: pool}
	t.Cleanup(func() { controlLinkTLS = old })
	m := testManager(t, &fakeRuntime{})
	if err := m.setControlLink(&ControlLink{URL: "http://insecure.example.com", Server: "old"}); err == nil {
		t.Fatal("a plain HTTP panel URL was accepted")
	}
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(panel.URL, "https://"))
	// The URL names the panel's host; the address pins where it is dialed.
	if err := m.setControlLink(&ControlLink{URL: "https://127.0.0.1:" + port, Address: "127.0.0.1", Server: "old"}); err != nil {
		t.Fatal(err)
	}
	if l := m.controlLink(); l == nil || l.Server != "old" || l.Address != "127.0.0.1" {
		t.Fatalf("stored %+v", l)
	}
	for deadline := time.Now().Add(5 * time.Second); hub.Idle("old") == 0 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		id, _ := agentlink.ServerOf(addr)
		return hub.Dial(ctx, id)
	}}}
	req, _ := http.NewRequest("GET", agentlink.URL("old")+"/v1/host/control", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	// Removing it stops the link.
	if err := m.setControlLink(nil); err != nil || m.controlLink() != nil {
		t.Fatal(err)
	}
}
