package migrate

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Probe checks the application on a specific host before DNS points there.
type Probe struct {
	Domain  string // name the application expects in the Host header
	Address string // target host:port to connect to, bypassing DNS
	Path    string // default /up
	TLS     bool
	// InsecureTLS accepts a certificate that does not match Domain. Before
	// DNS moves, the target cannot obtain a public certificate for the name,
	// so it serves an internal one; this check still verifies the
	// application, not the certificate.
	InsecureTLS bool
	Timeout     time.Duration
	Expect      int // default 200
}

// Verify performs the probe and returns the status and a body excerpt.
func Verify(ctx context.Context, p Probe) (int, string, error) {
	if !hostnamePattern.MatchString(p.Domain) {
		return 0, "", fmt.Errorf("migrate: invalid domain %q", p.Domain)
	}
	if _, _, err := net.SplitHostPort(p.Address); err != nil {
		return 0, "", errors.New("migrate: probe address must be host:port")
	}
	if p.Path == "" {
		p.Path = "/up"
	}
	if p.Timeout <= 0 {
		p.Timeout = 10 * time.Second
	}
	if p.Expect == 0 {
		p.Expect = http.StatusOK
	}
	dialer := &net.Dialer{Timeout: p.Timeout}
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, p.Address)
		},
		TLSClientConfig: &tls.Config{ServerName: p.Domain, InsecureSkipVerify: p.InsecureTLS, MinVersion: tls.VersionTLS12},
	}
	client := &http.Client{Timeout: p.Timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	scheme := "http"
	if p.TLS {
		scheme = "https"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+p.Domain+p.Path, nil)
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("User-Agent", "lidza-migrate-verify")
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	excerpt := strings.TrimSpace(string(body))
	if response.StatusCode != p.Expect {
		return response.StatusCode, excerpt, fmt.Errorf("migrate: %s%s on %s answered HTTP %d, want %d",
			p.Domain, p.Path, p.Address, response.StatusCode, p.Expect)
	}
	return response.StatusCode, excerpt, nil
}
