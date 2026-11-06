// Package movetunnel carries the TCP connections of a move's logical
// replication through the control panel, over the HTTP channels the panel
// already uses to reach every agent.
//
// Neither agent needs to reach the other, and neither needs to reach the
// panel: the panel opens every connection. A stream is an HTTP/1.1 request
// upgraded with "Upgrade: lidza-tcp" (net/http's Hijacker on the agent, the
// writable 101 response body on the panel), so it passes the agent's own
// listener directly or a reverse proxy that forwards upgrades (Caddy does).
//
// The destination's PostgreSQL connects to a local listener of the
// destination agent. That agent cannot call the panel, so the panel keeps a
// few idle "legs" open to it. A leg is an upgraded stream in one of two
// phases:
//
//   - idle: both ends write a ping byte (0x00) every Heartbeat and drop the
//     leg when nothing arrives for IdleTimeout;
//   - claimed: the destination hands an accepted connection to an idle leg
//     and writes go (0x01); the panel stops pinging, opens the source leg
//     (an upgraded stream the source agent connects to its database) and
//     answers ack (0x01) or fail (0x02). After ack every byte in both
//     directions belongs to PostgreSQL.
//
// Because each end stops pinging before it writes its go or ack byte, the
// first raw byte on either side of a claimed leg always follows the last
// control byte.
package movetunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// Protocol is the Upgrade token of a tunnel stream.
	Protocol = "lidza-tcp"
	// TokenHeader carries the move-bound token of a stream, besides the
	// agent's bearer token.
	TokenHeader = "X-Lidza-Move-Token"

	framePing byte = 0x00
	frameGo   byte = 0x01 // destination → panel: a client waits on this leg
	frameAck  byte = 0x01 // panel → destination: the source leg is open
	frameFail byte = 0x02 // panel → destination: the source leg failed
)

// Timing, replaced in tests; a pool and an idle leg read them when they
// start.
var (
	Heartbeat    = 15 * time.Second
	IdleTimeout  = 45 * time.Second
	ClaimTimeout = 20 * time.Second
)

// ErrNotUpgrade marks a request that does not ask for a tunnel stream.
var ErrNotUpgrade = errors.New("the request does not upgrade to " + Protocol)

// Requested reports whether r asks for a tunnel stream.
func Requested(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), Protocol) {
		return false
	}
	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// Upgrade answers 101 and takes the connection over from net/http. The
// server's read and write deadlines are cleared: the stream lives as long as
// its two ends want.
func Upgrade(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	if !Requested(r) {
		return nil, ErrNotUpgrade
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, fmt.Errorf("the connection cannot be taken over: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+Protocol+"\r\n\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	return &bufferedConn{Conn: conn, r: rw.Reader}, nil
}

// bufferedConn reads through the server's buffer, which may hold bytes the
// client sent right after its request.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// RefusedError is an agent's answer other than 101.
type RefusedError struct {
	Status  int
	Message string
}

func (e *RefusedError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// Client returns an HTTP client for tunnel streams built on base: HTTP/1.1
// only (an upgrade cannot ride HTTP/2) and without an overall timeout.
func Client(base http.RoundTripper) *http.Client {
	var transport *http.Transport
	switch t := base.(type) {
	case *http.Transport:
		transport = t.Clone()
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	default:
		return &http.Client{Transport: base, CheckRedirect: noRedirect}
	}
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: transport, CheckRedirect: noRedirect}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Dial opens a stream to an agent route. The handshake is bounded by ctx;
// the stream itself is not.
func Dial(ctx context.Context, client *http.Client, url, bearer, token string) (io.ReadWriteCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(TokenHeader, token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", Protocol)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		defer res.Body.Close()
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
		return nil, &RefusedError{Status: res.StatusCode, Message: e.Error}
	}
	stream, ok := res.Body.(io.ReadWriteCloser)
	if !ok || !strings.EqualFold(res.Header.Get("Upgrade"), Protocol) {
		res.Body.Close()
		return nil, errors.New("the agent's answer is not a " + Protocol + " stream (a proxy in between may not forward upgrades)")
	}
	return stream, nil
}

// Pipe copies both ways until either side ends or ctx is done, then closes
// both.
func Pipe(ctx context.Context, a, b io.ReadWriteCloser) {
	stop := context.AfterFunc(ctx, func() { a.Close(); b.Close() })
	defer stop()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

// ---- panel side ---------------------------------------------------------------------

// AwaitClient keeps an idle leg alive until the destination announces a
// client (nil), or the leg dies, stays silent for IdleTimeout or ctx ends
// (an error; the leg is closed). After nil the caller answers with Ack or
// Fail.
func AwaitClient(ctx context.Context, leg io.ReadWriteCloser) error {
	heartbeat, idleTimeout := Heartbeat, IdleTimeout
	var mu sync.Mutex
	stopped := false
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				mu.Lock()
				if !stopped {
					if _, err := leg.Write([]byte{framePing}); err != nil {
						leg.Close()
					}
				}
				mu.Unlock()
			}
		}
	}()
	silent := time.AfterFunc(idleTimeout, func() { leg.Close() })
	defer silent.Stop()
	cancelled := context.AfterFunc(ctx, func() { leg.Close() })
	defer cancelled()
	frame := make([]byte, 1)
	for {
		if _, err := io.ReadFull(leg, frame); err != nil {
			leg.Close()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("the idle stream ended: %w", err)
		}
		silent.Reset(idleTimeout)
		switch frame[0] {
		case framePing:
		case frameGo:
			mu.Lock()
			stopped = true
			mu.Unlock()
			if !cancelled() {
				leg.Close()
				return ctx.Err()
			}
			if !silent.Stop() {
				leg.Close()
				return errors.New("the idle stream timed out")
			}
			return nil
		default:
			leg.Close()
			return fmt.Errorf("unexpected frame 0x%02x on an idle stream", frame[0])
		}
	}
}

// Ack tells the destination the source leg is open; raw bytes follow.
func Ack(leg io.Writer) error { _, err := leg.Write([]byte{frameAck}); return err }

// Fail tells the destination the source leg could not be opened.
func Fail(leg io.Writer) error { _, err := leg.Write([]byte{frameFail}); return err }

// ---- destination side ---------------------------------------------------------------

// Pool holds the idle legs the panel keeps open to the destination and
// hands accepted connections to them.
type Pool struct {
	heartbeat, idleTimeout, claimTimeout time.Duration

	mu      sync.Mutex
	idle    []*leg
	active  map[*leg]struct{}
	closed  bool
	arrived chan struct{}
}

type leg struct {
	pool    *Pool
	conn    net.Conn
	mu      sync.Mutex
	claimed bool
	dead    bool
	client  net.Conn
}

// NewPool makes an empty pool.
func NewPool() *Pool {
	return &Pool{heartbeat: Heartbeat, idleTimeout: IdleTimeout, claimTimeout: ClaimTimeout, active: map[*leg]struct{}{}, arrived: make(chan struct{})}
}

// Idle counts the legs waiting for a client.
func (p *Pool) Idle() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle)
}

// Active counts the legs that carry or are about to carry a client.
func (p *Pool) Active() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active)
}

// ServeLeg runs one leg the panel opened until it dies or its client's
// connection ends.
func (p *Pool) ServeLeg(conn net.Conn) {
	l := &leg{pool: p, conn: conn}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		conn.Close()
		return
	}
	p.idle = append(p.idle, l)
	close(p.arrived)
	p.arrived = make(chan struct{})
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		for i, other := range p.idle {
			if other == l {
				p.idle = append(p.idle[:i], p.idle[i+1:]...)
				break
			}
		}
		delete(p.active, l)
		p.mu.Unlock()
		l.mu.Lock()
		l.dead = true
		client := l.client
		l.mu.Unlock()
		conn.Close()
		if client != nil {
			client.Close()
		}
	}()
	stop := make(chan struct{})
	defer close(stop)
	go l.ping(stop)
	frame := make([]byte, 1)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(p.idleTimeout))
		if _, err := io.ReadFull(conn, frame); err != nil {
			return
		}
		l.mu.Lock()
		client := l.client
		l.mu.Unlock()
		switch {
		case frame[0] == framePing:
		case frame[0] == frameAck && client != nil:
			_ = conn.SetDeadline(time.Time{})
			Pipe(context.Background(), conn, client)
			return
		default: // fail, or a frame out of turn
			return
		}
	}
}

func (l *leg) ping(stop chan struct{}) {
	ticker := time.NewTicker(l.pool.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			l.mu.Lock()
			if l.claimed || l.dead {
				l.mu.Unlock()
				return
			}
			_ = l.conn.SetWriteDeadline(time.Now().Add(l.pool.idleTimeout))
			if _, err := l.conn.Write([]byte{framePing}); err != nil {
				l.dead = true
				l.conn.Close()
			}
			l.mu.Unlock()
		}
	}
}

// claim hands client to the leg and announces it to the panel.
func (l *leg) claim(client net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.claimed || l.dead {
		return false
	}
	l.claimed = true
	_ = l.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := l.conn.Write([]byte{frameGo}); err != nil {
		l.dead = true
		l.conn.Close()
		return false
	}
	_ = l.conn.SetWriteDeadline(time.Time{})
	l.client = client
	return true
}

// Handle gives an accepted connection to an idle leg, waiting up to
// ClaimTimeout for one; without a leg the connection is closed.
func (p *Pool) Handle(client net.Conn) {
	deadline := time.NewTimer(p.claimTimeout)
	defer deadline.Stop()
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			client.Close()
			return
		}
		var candidate *leg
		if len(p.idle) > 0 {
			candidate = p.idle[0]
			p.idle = p.idle[1:]
			p.active[candidate] = struct{}{}
		}
		arrived := p.arrived
		p.mu.Unlock()
		if candidate != nil {
			if candidate.claim(client) {
				return
			}
			p.mu.Lock()
			delete(p.active, candidate)
			p.mu.Unlock()
			continue
		}
		select {
		case <-arrived:
		case <-deadline.C:
			client.Close()
			return
		}
	}
}

// Close ends every leg and the connections they carry.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	legs := append([]*leg{}, p.idle...)
	for l := range p.active {
		legs = append(legs, l)
	}
	p.idle = nil
	p.mu.Unlock()
	for _, l := range legs {
		l.mu.Lock()
		l.dead = true
		client := l.client
		l.mu.Unlock()
		l.conn.Close()
		if client != nil {
			client.Close()
		}
	}
}
