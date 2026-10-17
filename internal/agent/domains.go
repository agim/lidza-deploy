package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
)

const domainProbePath = "/.well-known/lidza-deploy-host"
const domainPendingInterval = 10 * time.Second
const domainReadyInterval = 5 * time.Minute

type DomainStatus struct {
	RecheckSeconds int       `json:"recheck_seconds,omitempty"`
	Domain         string    `json:"domain"`
	State          string    `json:"state"`
	Checked        time.Time `json:"checked,omitempty"`
	Message        string    `json:"message,omitempty"`
}

func (m *Manager) domainProof(domain string) string {
	hash := hmac.New(sha256.New, m.key)
	hash.Write([]byte("domain-proof:" + domain))
	return hex.EncodeToString(hash.Sum(nil))
}

// Resolve each address, then prove it serves this agent. This also works with
// public NAT addresses that are not present on the server's network interfaces.
func checkDomainRoute(ctx context.Context, domain, proof string, lookup func(context.Context, string, string) ([]net.IP, error), port string) error {
	resolve, stop := context.WithTimeout(ctx, 5*time.Second)
	ips, err := lookup(resolve, "ip", domain)
	stop()
	if err != nil || len(ips) == 0 {
		var networkError net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
			return errors.New("DNS lookup timed out; check the hosting server's resolver")
		}
		return errors.New("waiting for DNS A/AAAA records")
	}
	nonce := newID()
	expected := challengeProof(proof, nonce)
	hasA := false
	unique := map[string]bool{}
	checks, probeContext := errgroup.WithContext(ctx)
	checks.SetLimit(4)
	for _, ip := range ips {
		if ip.To4() != nil {
			hasA = true
		}
		if unique[ip.String()] {
			continue
		}
		unique[ip.String()] = true
		checks.Go(func() error { return checkDomainAddress(probeContext, domain, ip, port, nonce, expected) })
	}
	if err := checks.Wait(); err != nil {
		return err
	}
	if !hasA {
		return errors.New("waiting for a DNS A record pointing to this server")
	}
	return nil
}

func checkDomainAddress(ctx context.Context, domain string, ip net.IP, port, nonce, expected string) error {
	addr := net.JoinHostPort(ip.String(), port)
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}}
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+domain+domainProbePath+"?nonce="+nonce, nil)
	res, err := client.Do(req)
	if err != nil {
		transport.CloseIdleConnections()
		return fmt.Errorf("HTTP verification failed for %s: %s; check port %s access from the hosting server", addr, domainConnectionError(err), port)
	}
	body, readErr := io.ReadAll(io.LimitReader(res.Body, 1024))
	res.Body.Close()
	transport.CloseIdleConnections()
	if readErr != nil || res.StatusCode != 200 || !hmac.Equal(body, []byte(expected)) {
		return fmt.Errorf("HTTP verification for %s did not match this host (HTTP %d); check DNS and Caddy's verification route", addr, res.StatusCode)
	}
	return nil
}

func domainConnectionError(err error) string {
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "connection timed out"
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "address unreachable"
	case errors.Is(err, io.EOF):
		return "connection closed"
	default:
		return "connection failed"
	}
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
	m.runDomainChecks(m.ctx, time.Second, func(ctx context.Context, a App) {
		m.checkAppDomainContext(ctx, a, "80", net.DefaultResolver.LookupIP, func(ctx context.Context, domain string) error {
			return requestDomainCertificate(ctx, domain, m.cfg.TLSListen)
		})
	})
}

// Each host has its own retry clock. An unreachable domain cannot stall the
// entire fleet; no domain runs overlapping checks, and work remains bounded.
func (m *Manager) runDomainChecks(ctx context.Context, interval time.Duration, check func(context.Context, App)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	type runningCheck struct {
		domain string
		cancel context.CancelFunc
	}
	running := map[string]runningCheck{}
	finished := make(chan string, 8)
	var pending sync.WaitGroup
	defer func() {
		for _, item := range running {
			item.cancel()
		}
		pending.Wait()
	}()
	scan := func() {
		if ctx.Err() != nil {
			return
		}
		apps := []App{}
		for _, a := range m.Apps() {
			for _, domain := range a.Hostnames() {
				item := a
				item.Domain = domain
				item.domainCheckKey = domainStatusKey(a, domain)
				apps = append(apps, item)
			}
		}
		live := map[string]string{}
		for _, a := range apps {
			if a.Current != nil && !a.Retiring {
				live[checkKey(a)] = a.Domain
			}
		}
		for id, item := range running {
			if live[id] != item.domain {
				item.cancel()
			}
		}
		for _, a := range apps {
			if a.Current == nil || a.Retiring {
				continue
			}
			if _, active := running[checkKey(a)]; active {
				continue
			}
			if len(running) >= 8 {
				break
			}
			m.mu.Lock()
			status := m.domains[checkKey(a)]
			m.mu.Unlock()
			delay := domainPendingInterval
			if status.State == "ready" {
				delay = domainReadyInterval
			}
			if status.Domain == a.Domain && !status.Checked.IsZero() && time.Since(status.Checked) < delay {
				continue
			}
			work, cancel := context.WithCancel(ctx)
			running[checkKey(a)] = runningCheck{domain: a.Domain, cancel: cancel}
			pending.Add(1)
			go func(a App) { defer pending.Done(); check(work, a); finished <- checkKey(a) }(a)
		}
	}
	scan()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-finished:
			running[id].cancel()
			delete(running, id)
		case <-ticker.C:
		case <-m.domainWake:
		}
		scan()
	}
}

func (m *Manager) checkAppDomain(a App, port string, lookup func(context.Context, string, string) ([]net.IP, error), issue func(context.Context, string) error) {
	m.checkAppDomainContext(m.ctx, a, port, lookup, issue)
}
func (m *Manager) checkAppDomainContext(parent context.Context, a App, port string, lookup func(context.Context, string, string) ([]net.IP, error), issue func(context.Context, string) error) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	status := DomainStatus{Domain: a.Domain, State: "waiting_dns", Checked: time.Now().UTC(), RecheckSeconds: int(domainPendingInterval / time.Second)}
	if err := checkDomainRoute(ctx, a.Domain, m.domainProof(a.Domain), lookup, port); err != nil {
		status.Message = err.Error()
	} else {
		status.State = "requesting_ssl"
		m.setDomainStatus(a, status)
		if err := issue(ctx, a.Domain); err != nil {
			status.Message = err.Error()
		} else {
			status.State = "ready"
			status.RecheckSeconds = int(domainReadyInterval / time.Second)
		}
	}
	if parent.Err() != nil {
		return
	}
	status.Checked = time.Now().UTC()
	m.setDomainStatus(a, status)
}
func (m *Manager) setDomainStatus(a App, status DomainStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.data.Apps[a.ID]
	if ok && current.HasDomain(a.Domain) && domainStatusKey(current, a.Domain) == checkKey(a) && !current.Retiring {
		m.domains[checkKey(a)] = status
	}
}
func (m *Manager) serveDomainProof(w http.ResponseWriter, r *http.Request, domain string) {
	nonce := r.URL.Query().Get("nonce")
	if decoded, err := hex.DecodeString(nonce); err != nil || len(decoded) != 12 {
		http.NotFound(w, r)
		return
	}
	for _, a := range m.Apps() {
		if a.HasDomain(domain) && !a.Retiring {
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
		if a.HasDomain(strings.ToLower(domain)) && !a.Retiring {
			if m.cfg.TLSListen == "" {
				return true
			} // Isolated agents without a TLS terminator.
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return checkDomainRoute(ctx, strings.ToLower(domain), m.domainProof(strings.ToLower(domain)), net.DefaultResolver.LookupIP, "80") == nil
		}
	}
	a, ok := m.hostApplication(ctx, domain)
	return ok && a.CustomerHosts && !a.Stopped && !a.Retiring && a.Current != nil
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

func domainStatusKey(a App, domain string) string {
	if domain == a.Domain {
		return a.ID
	}
	return a.ID + "/" + domain
}
func checkKey(a App) string {
	if a.domainCheckKey != "" {
		return a.domainCheckKey
	}
	return a.ID
}
func (m *Manager) domainStatusesLocked(a App) []DomainStatus {
	if m.cfg.TLSListen == "" {
		return nil
	}
	out := []DomainStatus{}
	for _, domain := range a.Hostnames() {
		status, ok := m.domains[domainStatusKey(a, domain)]
		if !ok || status.Domain != domain {
			status = DomainStatus{Domain: domain, State: "waiting_dns", RecheckSeconds: int(domainPendingInterval / time.Second)}
		}
		out = append(out, status)
	}
	return out
}
