package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const domainProbePath = "/.well-known/lidza-deploy-host"

type DomainStatus struct {
	Domain  string    `json:"domain"`
	State   string    `json:"state"`
	Checked time.Time `json:"checked,omitempty"`
	Message string    `json:"message,omitempty"`
}

func (m *Manager) domainProof(domain string) string {
	hash := hmac.New(sha256.New, m.key)
	hash.Write([]byte("domain-proof:" + domain))
	return hex.EncodeToString(hash.Sum(nil))
}

// Resolve each address, then prove it serves this agent. This also works with
// public NAT addresses that are not present on the server's network interfaces.
func checkDomainRoute(ctx context.Context, domain, proof string, lookup func(context.Context, string, string) ([]net.IP, error), port string) error {
	ips, err := lookup(ctx, "ip", domain)
	if err != nil || len(ips) == 0 {
		return errors.New("waiting for DNS A/AAAA records")
	}
	nonce := newID()
	expected := challengeProof(proof, nonce)
	hasA := false
	for _, ip := range ips {
		if ip.To4() != nil {
			hasA = true
		}
		addr := net.JoinHostPort(ip.String(), port)
		transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
		}}
		client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+domain+domainProbePath+"?nonce="+nonce, nil)
		res, err := client.Do(req)
		if err != nil {
			transport.CloseIdleConnections()
			return errors.New("waiting for all DNS addresses to reach this server on port 80")
		}
		body, readErr := io.ReadAll(io.LimitReader(res.Body, 1024))
		res.Body.Close()
		transport.CloseIdleConnections()
		if readErr != nil || res.StatusCode != 200 || !hmac.Equal(body, []byte(expected)) {
			return errors.New("DNS points elsewhere or the server's HTTP verification route is unavailable")
		}
	}
	if !hasA {
		return errors.New("waiting for a DNS A record pointing to this server")
	}
	return nil
}

func requestDomainCertificate(ctx context.Context, domain, address string) error {
	return verifyDomainCertificate(ctx, domain, address, &tls.Config{ServerName: domain, MinVersion: tls.VersionTLS12})
}

func verifyDomainCertificate(ctx context.Context, domain, address string, config *tls.Config) error {
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 60 * time.Second}, Config: config}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return errors.New("certificate pending; check Caddy logs and public ports 80/443")
	}
	conn.Close()
	return nil
}

func (m *Manager) domainLoop() {
	defer m.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-timer.C:
		case <-m.domainWake:
		}
		var pending sync.WaitGroup
		slots := make(chan struct{}, 8)
		for _, a := range m.Apps() {
			if a.Current == nil || a.Retiring {
				continue
			}
			select {
			case slots <- struct{}{}:
			case <-m.ctx.Done():
				pending.Wait()
				return
			}
			pending.Add(1)
			go func(a App) {
				defer pending.Done()
				defer func() { <-slots }()
				m.checkAppDomain(a, "80", net.DefaultResolver.LookupIP, func(ctx context.Context, domain string) error {
					return requestDomainCertificate(ctx, domain, m.cfg.TLSListen)
				})
			}(a)
		}
		pending.Wait()
		timer.Reset(time.Minute)
	}
}

func (m *Manager) checkAppDomain(a App, port string, lookup func(context.Context, string, string) ([]net.IP, error), issue func(context.Context, string) error) {
	ctx, cancel := context.WithTimeout(m.ctx, 90*time.Second)
	defer cancel()
	status := DomainStatus{Domain: a.Domain, State: "waiting_dns", Checked: time.Now().UTC()}
	if err := checkDomainRoute(ctx, a.Domain, m.domainProof(a.Domain), lookup, port); err != nil {
		status.Message = err.Error()
	} else {
		status.State = "requesting_ssl"
		m.setDomainStatus(a, status)
		if err := issue(ctx, a.Domain); err != nil {
			status.Message = err.Error()
		} else {
			status.State = "ready"
		}
	}
	m.setDomainStatus(a, status)
}
func (m *Manager) setDomainStatus(a App, status DomainStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.data.Apps[a.ID]
	if ok && current.Domain == a.Domain && !current.Retiring {
		m.domains[a.ID] = status
	}
}
func (m *Manager) serveDomainProof(w http.ResponseWriter, r *http.Request, domain string) {
	nonce := r.URL.Query().Get("nonce")
	if decoded, err := hex.DecodeString(nonce); err != nil || len(decoded) != 12 {
		http.NotFound(w, r)
		return
	}
	for _, a := range m.Apps() {
		if a.Domain == domain && !a.Retiring {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, challengeProof(m.domainProof(domain), nonce))
			return
		}
	}
	http.NotFound(w, r)
}

// Certificate authorization uses a fresh route check, so an old successful
// DNS result cannot authorize issuance after DNS is moved away from this host.
func (m *Manager) domainAllowed(ctx context.Context, domain string) bool {
	for _, a := range m.Apps() {
		if a.Domain == strings.ToLower(domain) && !a.Retiring {
			if m.cfg.TLSListen == "" {
				return true
			} // Isolated agents without a TLS terminator.
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return checkDomainRoute(ctx, a.Domain, m.domainProof(a.Domain), net.DefaultResolver.LookupIP, "80") == nil
		}
	}
	return false
}

func (m *Manager) wakeDomains() {
	select {
	case m.domainWake <- struct{}{}:
	default:
	}
}

func challengeProof(proof, nonce string) string {
	hash := hmac.New(sha256.New, []byte(proof))
	hash.Write([]byte(nonce))
	return hex.EncodeToString(hash.Sum(nil))
}
