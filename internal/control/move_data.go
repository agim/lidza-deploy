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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Database dumps and file archives stream from the source agent through the
// control panel to the destination agent; the control panel never stores
// them. The source serves a finished backup or archive with its recorded
// SHA-256 and length. The control panel hashes the stream while forwarding
// it and fails the upload before its end when the bytes do not match, so the
// destination never receives a complete mismatching stream; the destination
// verifies the same digest again before it restores anything.

const moveMaxDumpBytes int64 = 64 << 30

var (
	moveCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	moveDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errMoveChecksum   = errors.New("the dump does not match its recorded SHA-256; nothing was restored")
	errMoveArchiveSum = errors.New("the file archive does not match its recorded SHA-256; nothing was restored")
)

type agentTimeoutKey struct{}

// withAgentTimeout gives agent calls made with ctx a longer client timeout
// for operations that are known to be slow.
func withAgentTimeout(ctx context.Context, timeout time.Duration) context.Context {
	return context.WithValue(ctx, agentTimeoutKey{}, timeout)
}

// moveVerifyingReader forwards a stream of a known size and digest. It holds
// back the final byte until it has read the whole source stream and checked
// its length and SHA-256; on a mismatch it fails instead, so the HTTP client
// aborts the upload and the destination never receives a complete stream.
//
// A resumed stream starts at sent (its offset) and shares hash and hashed
// with the earlier tries of the same transfer: bytes below hashed are
// already in the digest and are forwarded without hashing them again. When
// an offset lies beyond what this process hashed (the destination kept more
// than this process ever saw, for example after a restart of the control
// panel), trust is set: the stream is forwarded without the hold-back and
// the destination's verification of the whole artifact decides.
type moveVerifyingReader struct {
	r        io.Reader
	hash     hash.Hash
	expected string
	size     int64
	sent     int64
	last     byte
	err      error
	verified bool
	mismatch error // errMoveChecksum when nil
	hashed   *int64
	trust    bool
	// done counts the bytes forwarded, for live progress; onSent runs once
	// when the last byte was handed to the upload.
	done   atomic.Int64
	onSent func()
}

func (v *moveVerifyingReader) mismatchError() error {
	if v.mismatch != nil {
		return v.mismatch
	}
	return errMoveChecksum
}

// absorb adds bytes read at offset at to the digest, once.
func (v *moveVerifyingReader) absorb(p []byte, at int64) {
	if v.hashed == nil {
		v.hash.Write(p)
		return
	}
	if end := at + int64(len(p)); end > *v.hashed {
		v.hash.Write(p[*v.hashed-at:])
		*v.hashed = end
	}
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
	if v.trust {
		if remaining := v.size - v.sent; int64(len(p)) > remaining {
			p = p[:remaining]
		}
		n, err := v.r.Read(p)
		v.sent += int64(n)
		v.done.Store(v.sent)
		switch {
		case errors.Is(err, io.EOF) && v.sent < v.size:
			v.err = errMoveStreamShort
			return n, v.err
		case err != nil && !errors.Is(err, io.EOF):
			v.err = err
			return n, err
		}
		if v.sent == v.size && v.onSent != nil {
			v.onSent()
		}
		return n, nil
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
		v.done.Store(v.sent)
		if v.onSent != nil {
			v.onSent()
		}
		return 1, nil
	}
	if remaining := v.size - 1 - v.sent; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := v.r.Read(p)
	v.absorb(p[:n], v.sent)
	v.sent += int64(n)
	v.done.Store(v.sent)
	switch {
	case errors.Is(err, io.EOF):
		if v.sent < v.size-1 {
			v.err = errMoveStreamShort
			if v.hashed == nil {
				v.err = v.mismatchError()
			}
			return n, v.err
		}
		return n, nil
	case err != nil:
		v.err = err
		return n, err
	}
	return n, nil
}

// errMoveStreamShort is a download that ended early: a connection that
// dropped, not a mismatch.
var errMoveStreamShort = errors.New("the download ended early")

// verify reads the final byte and requires the end of the stream after it.
func (v *moveVerifyingReader) verify() error {
	tail := make([]byte, 2)
	n, err := io.ReadFull(v.r, tail)
	if n != 1 || !errors.Is(err, io.ErrUnexpectedEOF) {
		if n == 0 && v.hashed != nil {
			return errMoveStreamShort
		}
		return v.mismatchError()
	}
	v.absorb(tail[:1], v.size-1)
	if hex.EncodeToString(v.hash.Sum(nil)) != v.expected {
		return v.mismatchError()
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
func (c *Control) moveTransferDump(ctx context.Context, job MoveJob, d MoveDatabase, record agent.BackupRecord, upload string, keep []string) error {
	if err := c.moveWaitTargetIdle(ctx, job, d.TargetID); err != nil {
		return err
	}
	if record.Size <= 0 || record.Size > moveMaxDumpBytes || !moveDigestPattern.MatchString(record.SHA256) {
		return errors.New("the backup record has no usable size or digest")
	}
	restore := "/v1/move/databases/" + url.PathEscape(d.TargetID) + "/restore?replace=1"
	if len(keep) > 0 {
		restore += "&keep=" + url.QueryEscape(strings.Join(keep, ","))
	}
	return c.moveRelay(ctx, job, moveRelayStream{
		download: "/v1/move/databases/" + url.PathEscape(d.SourceID) + "/backups/" + url.PathEscape(record.ID),
		upload:   restore,
		size:     record.Size, digest: record.SHA256, mismatch: errMoveChecksum, what: "dump", resume: upload,
		phase: fmt.Sprintf("streaming the dump of %s to %s", d.SourceID, job.TargetServer),
		after: fmt.Sprintf("restoring %s into %s on %s", d.SourceID, d.TargetID, job.TargetServer),
	}, nil)
}

// moveUploadID names the resumable upload of one artifact of a move; the
// same artifact gets the same id after a restart of the control panel.
func moveUploadID(job MoveJob, key, digest string) string {
	sum := sha256.Sum256([]byte(job.ID + "\x00" + key + "\x00" + digest))
	return hex.EncodeToString(sum[:16])
}

// moveRelayStream is one verified stream from the source to the destination.
type moveRelayStream struct {
	download, upload string
	size             int64
	digest           string
	mismatch         error
	what             string // dump or archive, for messages
	// phase is the step's progress phase while the stream flows and after
	// once the destination has every byte and restores; watch, when set,
	// follows the destination's own progress from then on.
	phase, after string
	watch        func() (stop func())
	// resume names the transfer's resumable upload; empty, or a
	// destination that cannot resume, sends the stream in one request.
	resume string
}

// moveRelayResumes bounds the resumed requests of one relay; each must have
// moved the transfer forward.
const moveRelayResumes = 8

// moveRelay forwards stream from the source agent to the destination agent
// and decodes the destination's answer into out. When both agents support
// it, a dropped connection resumes from the destination's staged offset
// with a ranged download, and the digest is verified over the whole
// artifact (move_resume.go in the agent).
func (c *Control) moveRelay(ctx context.Context, job MoveJob, stream moveRelayStream, out any) error {
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
	resumable := stream.resume != "" && c.moveUploadSupported(ctx, job.TargetServer, stream.resume)
	hashState := sha256.New()
	hashed := int64(0)
	var lastOffset int64 = -1
	for try := 0; ; try++ {
		offset := int64(0)
		if resumable {
			if state, err := c.moveUploadState(ctx, job.TargetServer, stream.resume); err == nil && state.Size == stream.size && state.SHA256 == stream.digest {
				offset = state.Received
				if state.Complete {
					offset = stream.size
				}
			}
		}
		err := c.moveRelayOnce(ctx, client, source, target, job, stream, out, resumable, offset, hashState, &hashed)
		if err == nil || !resumable || ctx.Err() != nil || !moveRelayRetryable(err) || try >= moveRelayResumes {
			return err
		}
		// Resume only while the transfer moves forward.
		state, stateErr := c.moveUploadState(ctx, job.TargetServer, stream.resume)
		if stateErr != nil || state.Received <= lastOffset && try > 0 {
			return err
		}
		lastOffset = state.Received
		c.moveLog(job.ID, c.moveLiveStepKey(job.ID), "The %s transfer was interrupted at %s of %s (%s); resuming from the destination's offset.", stream.what, moveSize(state.Received), moveSize(stream.size), moveAgentError(err))
		select {
		case <-ctx.Done():
			return err
		case <-time.After(c.moveResumePause()):
		}
	}
}

func (c *Control) moveResumePause() time.Duration {
	if c.movePollInterval > 0 && c.movePollInterval < time.Second {
		return c.movePollInterval
	}
	return 2 * time.Second
}

// moveRelayRetryable reports an interrupted transfer worth resuming: the
// connection, not the content or a refusal.
func moveRelayRetryable(err error) bool {
	if errors.Is(err, errMoveChecksum) || errors.Is(err, errMoveArchiveSum) || errors.Is(err, errMoveClickHouseSum) {
		return false
	}
	class, code, _ := classifyMoveError(err)
	return class == moveClassTransient && code != "checksum"
}

func (c *Control) moveUploadSupported(ctx context.Context, server, upload string) bool {
	_, err := c.moveUploadState(ctx, server, upload)
	return err == nil
}

func (c *Control) moveUploadState(ctx context.Context, server, upload string) (agent.MoveUploadState, error) {
	var state agent.MoveUploadState
	err := c.moveCall(ctx, 15*time.Second, server, http.MethodGet, "/v1/move/uploads/"+url.PathEscape(upload), nil, &state)
	if err == nil && state.ID != upload {
		err = errors.New("the destination does not resume uploads")
	}
	return state, err
}

// moveRelayOnce sends the stream from offset in one request.
func (c *Control) moveRelayOnce(ctx context.Context, client *http.Client, source, target Server, job MoveJob, stream moveRelayStream, out any, resumable bool, offset int64, hashState hash.Hash, hashed *int64) error {
	body := &moveVerifyingReader{hash: hashState, expected: stream.digest, size: stream.size, mismatch: stream.mismatch, sent: offset}
	if resumable {
		body.hashed = hashed
		body.trust = offset > *hashed
	}
	if offset < stream.size {
		download, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL+stream.download, nil)
		if err != nil {
			return err
		}
		download.Header.Set("Authorization", "Bearer "+source.Token)
		if offset > 0 {
			download.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
		}
		res, err := client.Do(download)
		if err != nil {
			return moveOn(job.SourceServer, moveTransientError("agent_unavailable", errors.New("the source agent is unavailable")))
		}
		defer res.Body.Close()
		switch {
		case offset > 0 && res.StatusCode == http.StatusOK:
			// The source cannot serve ranges: start the upload over.
			body.sent, body.trust = 0, false
			offset = 0
		case offset > 0 && res.StatusCode != http.StatusPartialContent, offset == 0 && res.StatusCode != http.StatusOK:
			return moveOn(job.SourceServer, fmt.Errorf("the source refused the download: %w", moveResponseError(res)))
		}
		if strings.ToLower(res.Header.Get(agent.MoveSHA256Header)) != stream.digest || res.ContentLength != stream.size-offset {
			return moveOn(job.SourceServer, fmt.Errorf("the source's download does not match the recorded %s", stream.what))
		}
		body.r = res.Body
	} else {
		body.r = http.NoBody
		if !body.trust && *hashed == stream.size {
			body.verified = hex.EncodeToString(hashState.Sum(nil)) == stream.digest
		}
	}
	body.done.Store(offset)
	if stream.phase != "" {
		c.moveStage(job.ID, stream.phase, stream.size, body.done.Load)
	}
	// The upload reads the body on the transport's goroutine, which can
	// outlive the request; a watch never starts after the relay returned.
	var watching sync.Mutex
	var stopWatch func()
	relayed := false
	body.onSent = func() {
		watching.Lock()
		defer watching.Unlock()
		if relayed {
			return
		}
		if stream.after != "" {
			c.moveStage(job.ID, stream.after, 0, nil)
		}
		if stream.watch != nil {
			stopWatch = stream.watch()
		}
	}
	defer func() {
		watching.Lock()
		defer watching.Unlock()
		relayed = true
		if stopWatch != nil {
			stopWatch()
		}
	}()
	var reader io.Reader = body
	if offset >= stream.size {
		reader = http.NoBody
		body.onSent()
	}
	upload, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL+stream.upload, reader)
	if err != nil {
		return err
	}
	upload.ContentLength = stream.size - offset
	upload.Header.Set("Authorization", "Bearer "+target.Token)
	upload.Header.Set("Content-Type", "application/octet-stream")
	upload.Header.Set(agent.MoveSHA256Header, stream.digest)
	if resumable {
		upload.Header.Set(agent.MoveUploadHeader, stream.resume)
		upload.Header.Set(agent.MoveSizeHeader, strconv.FormatInt(stream.size, 10))
		upload.Header.Set(agent.MoveOffsetHeader, strconv.FormatInt(offset, 10))
	}
	response, err := client.Do(upload)
	if body.err != nil {
		if response != nil {
			response.Body.Close()
		}
		if errors.Is(body.err, errMoveStreamShort) {
			return moveOn(job.SourceServer, moveTransientError("agent_unavailable", errors.New("the source's download was interrupted")))
		}
		return body.err
	}
	if err != nil {
		return moveOn(job.TargetServer, moveTransientError("agent_unavailable", errors.New("the destination agent is unavailable")))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		failure := fmt.Errorf("the destination did not restore the %s: %w", stream.what, moveResponseError(response))
		if response.StatusCode == http.StatusInsufficientStorage {
			return moveOn(job.TargetServer, moveOperatorError("disk_space", failure.Error(), "Free disk space on "+c.serverName(job.TargetServer)+" (or add a disk), then retry the move."))
		}
		if response.StatusCode >= 500 {
			return moveOn(job.TargetServer, moveTransientError("agent_"+strconv.Itoa(response.StatusCode), failure))
		}
		return moveOn(job.TargetServer, failure)
	}
	switch {
	case body.verified:
	case resumable && strings.ToLower(response.Header.Get(agent.MoveVerifiedHeader)) == stream.digest:
		// The destination verified the whole artifact this process did not
		// see in full.
	default:
		return body.mismatchError()
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(out); err != nil {
			return fmt.Errorf("the destination's answer could not be read: %w", err)
		}
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
	statuses, all := c.resolveMoveHostnames(ctx, job)
	// Mail-sending applications: the reverse DNS is checked and logged too,
	// but never holds the cutover.
	ptr := c.checkMovePTR(ctx, job)
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
		if ptr != nil && (explicit || movePTRChanged(job.PTR, ptr)) {
			for _, line := range movePTRLogLines(ptr) {
				job.logf(moveStepCutover, "%s", line)
			}
		}
		if ptr != nil {
			job.PTR = ptr
		}
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
	if err == nil && updated.State == moveStateReady && updated.AutoCutover {
		c.moveAutoCutover(ctx, id)
	}
}

// resolveMoveHostnames resolves every hostname of a move; all reports that
// each one resolves only to the destination's addresses.
func (c *Control) resolveMoveHostnames(ctx context.Context, job MoveJob) (statuses []MoveDNSStatus, all bool) {
	resolver := c.resolver()
	statuses = []MoveDNSStatus{}
	all = len(job.Hostnames) > 0
	for _, host := range job.Hostnames {
		status := MoveDNSStatus{Hostname: host, Addresses: []string{}}
		lookup, cancel := context.WithTimeout(ctx, 8*time.Second)
		var addresses []net.IPAddr
		var err error
		if sourced, ok := resolver.(moveResolverSource); ok {
			addresses, status.Source, err = sourced.LookupIPAddrFrom(lookup, host)
		} else {
			addresses, err = resolver.LookupIPAddr(lookup, host)
		}
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
	return statuses, all
}

// moveDNSAhead reports a hostname that already resolves to one of the
// destination's addresses.
func moveDNSAhead(job MoveJob, statuses []MoveDNSStatus) string {
	for _, status := range statuses {
		for _, address := range status.Addresses {
			if slices.Contains(job.Addresses, address) {
				return status.Hostname
			}
		}
	}
	return ""
}

// checkMoveEarlyDNS watches DNS before the cutover: when a hostname already
// points to the destination while the application is not deployed there,
// visitors reach a server that cannot serve it yet. The move records it so
// the operator sees the outage, and logs it once.
func (c *Control) checkMoveEarlyDNS(ctx context.Context, id string) {
	job, ok := c.moveJob(id)
	if !ok || job.State != moveStateRunning && job.State != moveStateFailed {
		return
	}
	if s := job.step(moveStepDeploy); s == nil || s.Status == "done" || s.Status == "skipped" {
		return
	}
	statuses, _ := c.resolveMoveHostnames(ctx, job)
	host := moveDNSAhead(job, statuses)
	now := time.Now().UTC()
	_, _ = c.updateMove(id, func(job *MoveJob) error {
		if job.State != moveStateRunning && job.State != moveStateFailed {
			return errors.New("state changed")
		}
		if host != "" && !job.DNSAhead {
			step := nextMoveStep(*job)
			if step == "" {
				step = moveStepDeploy
			}
			job.logf(step, "DNS for %s already points to the destination, but the application is not deployed there yet: the site is down for visitors that reach the destination until the deploy finishes. Change DNS only when the move reaches Switch DNS.", host)
		}
		job.DNS, job.DNSChecked, job.DNSAhead = statuses, &now, host != ""
		return nil
	})
}

// moveWaitTargetIdle waits until the destination runs no deployment of the
// application and no operation on database id. The final sync reconciles
// the destination first, which may start a redeploy with its pre-deploy
// backup; a restore started meanwhile is refused ("a database operation is
// already running", "wait for the application's active deployment"), and
// every retry would reconcile and collide again.
func (c *Control) moveWaitTargetIdle(ctx context.Context, job MoveJob, id string) error {
	logged := false
	return c.movePoll(ctx, 20*time.Minute, func() (bool, error) {
		busy := ""
		var deployments []agent.Deployment
		if err := c.moveCall(ctx, time.Minute, job.TargetServer, http.MethodGet, "/v1/deployments?logs=0", nil, &deployments); err == nil {
			for _, d := range deployments {
				if d.AppID == job.AppID && (d.Status == "queued" || d.Status == "building") {
					busy = "deployment " + d.ID + " of " + job.AppID
				}
			}
		}
		var views []agent.DatabaseView
		if err := c.moveCall(ctx, time.Minute, job.TargetServer, http.MethodGet, "/v1/databases", nil, &views); err == nil {
			for _, v := range views {
				if v.AppID == id && v.Operation != "" {
					busy = "the " + v.Operation + " of database " + id
				}
			}
		}
		if busy == "" {
			return true, nil
		}
		if !logged {
			logged = true
			step := moveStepData
			if job.State == moveStateSyncing {
				step = moveStepFinalSync
			}
			c.moveLog(job.ID, step, "Waiting for %s on %s to finish before restoring.", busy, job.TargetServer)
		}
		return false, nil
	})
}
