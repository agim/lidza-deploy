package control

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Database dumps stream from the source agent through the control panel to
// the destination agent; the control panel never stores them. The source
// serves a finished backup with its recorded SHA-256 and length. The control
// panel hashes the stream while forwarding it and fails the upload before
// its end when the bytes do not match, so the destination never receives a
// complete mismatching dump; the destination verifies the same digest again
// before it restores anything.

const moveMaxDumpBytes int64 = 64 << 30

var (
	moveCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	moveDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errMoveChecksum   = errors.New("the dump does not match its recorded SHA-256; nothing was restored")
)

type agentTimeoutKey struct{}

// withAgentTimeout gives agent calls made with ctx a longer client timeout
// for operations that are known to be slow.
func withAgentTimeout(ctx context.Context, timeout time.Duration) context.Context {
	return context.WithValue(ctx, agentTimeoutKey{}, timeout)
}

// moveVerifyingReader forwards a dump of a known size and digest. It holds
// back the final byte until it has read the whole source stream and checked
// its length and SHA-256; on a mismatch it fails instead, so the HTTP client
// aborts the upload and the destination never receives a complete dump.
type moveVerifyingReader struct {
	r        io.Reader
	hash     hash.Hash
	expected string
	size     int64
	sent     int64
	last     byte
	err      error
	verified bool
}

func (v *moveVerifyingReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	if v.sent >= v.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if v.sent == v.size-1 {
		if !v.verified {
			if err := v.verify(); err != nil {
				v.err = err
				return 0, err
			}
		}
		p[0] = v.last
		v.sent++
		return 1, nil
	}
	if remaining := v.size - 1 - v.sent; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := v.r.Read(p)
	v.hash.Write(p[:n])
	v.sent += int64(n)
	switch {
	case errors.Is(err, io.EOF):
		if v.sent < v.size-1 {
			v.err = errMoveChecksum
			return n, v.err
		}
		return n, nil
	case err != nil:
		v.err = err
		return n, err
	}
	return n, nil
}

// verify reads the final byte and requires the end of the stream after it.
func (v *moveVerifyingReader) verify() error {
	tail := make([]byte, 2)
	n, err := io.ReadFull(v.r, tail)
	if n != 1 || !errors.Is(err, io.ErrUnexpectedEOF) {
		return errMoveChecksum
	}
	v.hash.Write(tail[:1])
	if hex.EncodeToString(v.hash.Sum(nil)) != v.expected {
		return errMoveChecksum
	}
	v.last, v.verified = tail[0], true
	return nil
}

func (c *Control) moveServer(id string) (Server, error) {
	for _, s := range c.servers() {
		if s.ID == id {
			return s, nil
		}
	}
	return Server{}, errors.New("unknown server")
}

func (c *Control) moveStreamClient() *http.Client {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if c.client != nil {
		client.Transport = c.client.Transport
	}
	return client
}

func moveResponseError(res *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
	if e.Error != "" {
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, e.Error)
	}
	return fmt.Errorf("HTTP %d", res.StatusCode)
}

// moveTransferDump streams one backup from the source into the destination
// database.
func (c *Control) moveTransferDump(ctx context.Context, job MoveJob, d MoveDatabase, record agent.BackupRecord) error {
	if record.Size <= 0 || record.Size > moveMaxDumpBytes || !moveDigestPattern.MatchString(record.SHA256) {
		return errors.New("the backup record has no usable size or digest")
	}
	source, err := c.moveServer(job.SourceServer)
	if err != nil {
		return err
	}
	target, err := c.moveServer(job.TargetServer)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	client := c.moveStreamClient()
	download, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL+"/v1/move/databases/"+url.PathEscape(d.SourceID)+"/backups/"+url.PathEscape(record.ID), nil)
	if err != nil {
		return err
	}
	download.Header.Set("Authorization", "Bearer "+source.Token)
	res, err := client.Do(download)
	if err != nil {
		return errors.New("the source agent is unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the source refused the download: %w", moveResponseError(res))
	}
	if strings.ToLower(res.Header.Get(agent.MoveSHA256Header)) != record.SHA256 || res.ContentLength != record.Size {
		return errors.New("the source's download does not match the backup record")
	}
	body := &moveVerifyingReader{r: res.Body, hash: sha256.New(), expected: record.SHA256, size: record.Size}
	upload, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL+"/v1/move/databases/"+url.PathEscape(d.TargetID)+"/restore?replace=1", body)
	if err != nil {
		return err
	}
	upload.ContentLength = record.Size
	upload.Header.Set("Authorization", "Bearer "+target.Token)
	upload.Header.Set("Content-Type", "application/octet-stream")
	upload.Header.Set(agent.MoveSHA256Header, record.SHA256)
	response, err := client.Do(upload)
	if body.err != nil {
		if response != nil {
			response.Body.Close()
		}
		return body.err
	}
	if err != nil {
		return errors.New("the destination agent is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the destination did not restore the dump: %w", moveResponseError(response))
	}
	if !body.verified {
		return errMoveChecksum
	}
	return nil
}

// ---- DNS and HTTPS --------------------------------------------------------------

func moveProbeAddress(addresses []string) string {
	for _, address := range addresses {
		if ip, err := netip.ParseAddr(address); err == nil && ip.Is4() {
			return address
		}
	}
	if len(addresses) > 0 {
		return addresses[0]
	}
	return ""
}

// moveHTTPSProbe connects to the destination's address with the hostname as
// SNI and Host and requires a certificate that is valid for it. Any answer
// below 500 counts: the destination's web server serves the application.
func (c *Control) moveHTTPSProbe(ctx context.Context, address, host string) error {
	if c.moveProbe != nil {
		return c.moveProbe(ctx, address, host)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(address, "443"))
		},
		TLSClientConfig: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "lidza-deploy-move-check")
	response, err := client.Do(request)
	if err != nil {
		var certificate *tls.CertificateVerificationError
		if errors.As(err, &certificate) {
			return errors.New("no valid certificate yet")
		}
		return errors.New("no HTTPS answer")
	}
	response.Body.Close()
	if response.StatusCode >= 500 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return nil
}

// checkMoveDNS resolves every hostname and records whether all of them
// point only at the destination. HTTPS on the destination is probed once
// DNS points there, or on the operator's explicit recheck, so certificate
// requests are not attempted while the names still point elsewhere.
func (c *Control) checkMoveDNS(ctx context.Context, id string, explicit bool) {
	job, ok := c.moveJob(id)
	if !ok || job.State != moveStateCutover && job.State != moveStateReady {
		return
	}
	var resolver moveResolver = net.DefaultResolver
	if c.moveResolver != nil {
		resolver = c.moveResolver
	}
	statuses := []MoveDNSStatus{}
	all := len(job.Hostnames) > 0
	for _, host := range job.Hostnames {
		status := MoveDNSStatus{Hostname: host, Addresses: []string{}}
		lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
		addresses, err := resolver.LookupIPAddr(lookup, host)
		cancel()
		if err != nil {
			status.Error = "does not resolve"
		} else {
			status.Ready = len(addresses) > 0
			for _, address := range addresses {
				ip, ok := netip.AddrFromSlice(address.IP)
				value := address.IP.String()
				if ok {
					value = ip.Unmap().String()
				}
				if !slices.Contains(status.Addresses, value) {
					status.Addresses = append(status.Addresses, value)
				}
				if !slices.Contains(job.Addresses, value) {
					status.Ready = false
				}
			}
			slices.Sort(status.Addresses)
		}
		all = all && status.Ready
		statuses = append(statuses, status)
	}
	tlsReady, tlsDetail := job.TLSReady, job.TLSDetail
	if (all || explicit) && (!tlsReady || explicit) {
		tlsReady, tlsDetail = true, "HTTPS works on the destination for every hostname."
		address := moveProbeAddress(job.Addresses)
		for _, host := range job.Hostnames {
			if err := c.moveHTTPSProbe(ctx, address, host); err != nil {
				tlsReady, tlsDetail = false, fmt.Sprintf("HTTPS for %s on the destination: %s.", host, err)
				break
			}
		}
	}
	now := time.Now().UTC()
	updated, err := c.updateMove(id, func(job *MoveJob) error {
		if job.State != moveStateCutover && job.State != moveStateReady {
			return errors.New("state changed")
		}
		wasReady := job.State == moveStateReady
		job.DNS, job.DNSChecked, job.TLSReady, job.TLSDetail = statuses, &now, tlsReady, tlsDetail
		switch {
		case all && tlsReady && !wasReady:
			job.State = moveStateReady
			job.setStep(moveStepCutover, "done")
			job.logf(moveStepCutover, "DNS for every hostname points to the destination and HTTPS works there. Finish the move to retire the source.")
		case wasReady && !(all && tlsReady):
			job.State = moveStateCutover
			job.setStep(moveStepCutover, "waiting")
			job.logf(moveStepCutover, "DNS or HTTPS no longer points to the destination; waiting again.")
		case all && !tlsReady && explicit:
			job.logf(moveStepCutover, "DNS points to the destination; %s", tlsDetail)
		}
		return nil
	})
	if err == nil && updated.State == moveStateReady && job.State != moveStateReady {
		moveAudit(ctx, updated, "dns_ready", false)
	}
}
