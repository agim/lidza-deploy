// Package agentlink lets a control panel reach an agent that it cannot
// connect to: the agent connects out to the panel's public HTTPS address
// and the panel sends its agent calls back through those connections.
//
// A server whose agent listens on loopback behind a firewall that admits
// only 80 and 443 has no URL another server reaches. Its agent keeps a few
// idle "links" open to the panel instead: an HTTP/1.1 request to Path
// upgraded with "Upgrade: lidza-agent-link", authenticated with the agent's
// own API token (the panel's token for that server) and the server id. The
// panel holds the links per server; an agent call takes one, and from then
// on the link is a plain HTTP/1.1 connection from the panel (client) to the
// agent (server). The agent dials a replacement for every link taken and
// reconnects forever.
//
// A link is idle or claimed:
//
//   - idle: both ends write a ping byte (0x00) every Heartbeat and drop the
//     link when nothing arrives for IdleTimeout;
//   - claimed: the panel stops pinging and writes claim (0x01); the agent
//     stops pinging and answers ready (0x01). The first byte after it is the
//     panel's HTTP request. A link that does not answer within ReadyTimeout
//     is dropped and the call takes another, so a call is never sent into a
//     dead link.
package agentlink

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// Protocol is the Upgrade token of a link.
	Protocol = "lidza-agent-link"
	// Path is the panel route agents connect to.
	Path = "/api/agent/link"
	// ProbePath asks the panel to call the agent through a link.
	ProbePath = "/api/agent/link/probe"
	// ServerHeader names the server a link belongs to.
	ServerHeader = "X-Lidza-Server"
	// HostSuffix marks the URL of a server reached through links:
	// http://<server id>.agent-link.invalid. The .invalid name never
	// resolves; the panel's transport dials a link instead.
	HostSuffix = ".agent-link.invalid"

	framePing  byte = 0x00
	frameClaim byte = 0x01
	frameReady byte = 0x01
)

// Timing, replaced in tests.
var (
	Heartbeat    = 15 * time.Second
	IdleTimeout  = 45 * time.Second
	ReadyTimeout = 10 * time.Second
	// WaitForLink bounds how long a call waits for an idle link.
	WaitForLink = 15 * time.Second
)

// URL is the agent URL of a server reached through links.
func URL(server string) string { return "http://" + server + HostSuffix }

// ServerOf returns the server id of a link host ("old.agent-link.invalid"
// or with a port); ok is false for any other host.
func ServerOf(host string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	id, ok := strings.CutSuffix(strings.ToLower(host), HostSuffix)
	return id, ok && id != ""
}

// Requested reports whether r asks for a link.
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

// Authorized compares the request's bearer token with want in constant
// time.
func Authorized(r *http.Request, want string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && len(want) >= 32 && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// ---- panel side ---------------------------------------------------------------------

// Hub holds the idle links of every server.
type Hub struct {
	mu     sync.Mutex
	idle   map[string][]*link
	wake   chan struct{}
	closed bool
}

type link struct {
	conn    net.Conn
	writeMu sync.Mutex
	claimed bool
	ready   chan error
	gone    chan struct{}
	once    sync.Once
}

func (l *link) drop() {
	l.once.Do(func() { close(l.gone); l.conn.Close() })
}

// NewHub makes an empty hub.
func NewHub() *Hub { return &Hub{idle: map[string][]*link{}, wake: make(chan struct{})} }

// Idle counts the idle links of server.
func (h *Hub) Idle(server string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.idle[server])
}

// Serve answers a link request of server whose token the caller verified:
// it upgrades the connection and keeps the link until a call claims it.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, server string) {
	if !Requested(r) {
		http.Error(w, "upgrade to "+Protocol+" required", http.StatusUpgradeRequired)
		return
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "the connection cannot be taken over", http.StatusInternalServerError)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+Protocol+"\r\n\r\n"); err != nil {
		conn.Close()
		return
	}
	h.Accept(server, &bufferedConn{Conn: conn, r: rw.Reader})
}

// Accept adds an upgraded connection of server as an idle link.
func (h *Hub) Accept(server string, conn net.Conn) {
	l := &link{conn: conn, ready: make(chan error, 1), gone: make(chan struct{})}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		conn.Close()
		return
	}
	h.idle[server] = append(h.idle[server], l)
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
	go h.keep(server, l)
}

// keep pings an idle link and reads its pings until a claim's ready byte
// (handed to the claimer) or the link dies.
func (h *Hub) keep(server string, l *link) {
	alive := make(chan struct{}, 1)
	go func() {
		ticker := time.NewTicker(Heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-l.gone:
				return
			case <-ticker.C:
			}
			l.writeMu.Lock()
			if l.claimed {
				l.writeMu.Unlock()
				return
			}
			_, err := l.conn.Write([]byte{framePing})
			l.writeMu.Unlock()
			if err != nil {
				h.remove(server, l)
				l.drop()
				return
			}
		}
	}()
	go func() {
		timer := time.NewTimer(IdleTimeout)
		defer timer.Stop()
		for {
			select {
			case <-l.gone:
				return
			case <-alive:
				timer.Reset(IdleTimeout)
			case <-timer.C:
				l.writeMu.Lock()
				claimed := l.claimed
				l.writeMu.Unlock()
				if !claimed {
					h.remove(server, l)
					l.drop()
				}
				return
			}
		}
	}()
	b := make([]byte, 1)
	for {
		if _, err := io.ReadFull(l.conn, b); err != nil {
			h.remove(server, l)
			l.ready <- err
			l.drop()
			return
		}
		if b[0] == frameReady {
			l.writeMu.Lock()
			claimed := l.claimed
			l.writeMu.Unlock()
			if claimed {
				l.ready <- nil
				return
			}
		}
		select {
		case alive <- struct{}{}:
		default:
		}
	}
}

func (h *Hub) remove(server string, l *link) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.idle[server]
	for i, candidate := range list {
		if candidate == l {
			h.idle[server] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(h.idle[server]) == 0 {
		delete(h.idle, server)
	}
}

func (h *Hub) take(server string) (*link, chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.idle[server]
	if len(list) == 0 {
		return nil, h.wake
	}
	l := list[len(list)-1]
	h.idle[server] = list[:len(list)-1]
	return l, nil
}

// Dial claims an idle link of server and returns it as a connection to the
// agent's API. It waits up to WaitForLink for one.
func (h *Hub) Dial(ctx context.Context, server string) (net.Conn, error) {
	deadline := time.NewTimer(WaitForLink)
	defer deadline.Stop()
	for {
		l, wake := h.take(server)
		if l == nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-deadline.C:
				return nil, fmt.Errorf("the agent of %s has no open link to this panel; it connects out to the panel's address", server)
			case <-wake:
				continue
			}
		}
		l.writeMu.Lock()
		l.claimed = true
		_, err := l.conn.Write([]byte{frameClaim})
		l.writeMu.Unlock()
		if err != nil {
			l.drop()
			continue
		}
		timer := time.NewTimer(ReadyTimeout)
		select {
		case err = <-l.ready:
		case <-timer.C:
			err = errors.New("the link did not answer")
		case <-ctx.Done():
			err = ctx.Err()
		}
		timer.Stop()
		if err != nil {
			l.drop()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		return l.conn, nil
	}
}

// Close drops every idle link.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	all := h.idle
	h.idle = map[string][]*link{}
	h.mu.Unlock()
	for _, list := range all {
		for _, l := range list {
			l.drop()
		}
	}
}

// bufferedConn reads through the server's buffer, which may hold bytes the
// agent sent right after its request.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// ---- agent side ---------------------------------------------------------------------

// Agent keeps links of one server open to a panel.
type Agent struct {
	// URL is the panel's public URL (https://panel.example.com).
	URL string
	// Address, when set, is dialed instead of resolving the URL's host
	// (the TLS server name stays the host): during a panel move DNS still
	// points to the old panel. After repeated failures the host is resolved.
	Address string
	Server  string
	Token   string
	// Handler serves the panel's calls (the agent's API).
	Handler http.Handler
	// Idle is how many idle links to keep; default 3.
	Idle int
	// TLS is the client configuration (tests); nil verifies against the
	// system roots.
	TLS *tls.Config
}

// Run keeps the links until ctx ends.
func (a Agent) Run(ctx context.Context) {
	n := a.Idle
	if n <= 0 {
		n = 3
	}
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() { defer wg.Done(); a.worker(ctx) }()
	}
	wg.Wait()
}

func (a Agent) worker(ctx context.Context) {
	backoff, failures := time.Second, 0
	for ctx.Err() == nil {
		conn, err := a.dial(ctx, failures >= 3)
		if err != nil {
			failures++
			if failures == 1 || failures%30 == 0 {
				log.Printf("agent link to %s: %v; retrying", a.URL, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff, failures = time.Second, 0
		if a.idle(ctx, conn) {
			go a.serve(ctx, conn)
		}
	}
}

// dial opens and upgrades one link.
func (a Agent) dial(ctx context.Context, resolve bool) (net.Conn, error) {
	u, err := url.Parse(a.URL)
	if err != nil || u.Host == "" {
		return nil, errors.New("invalid panel URL")
	}
	port := u.Port()
	if port == "" {
		port = map[bool]string{true: "443", false: "80"}[u.Scheme == "https"]
	}
	address := net.JoinHostPort(u.Hostname(), port)
	if a.Address != "" && !resolve {
		address = net.JoinHostPort(a.Address, port)
	}
	handshake, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var conn net.Conn
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if u.Scheme == "https" {
		config := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
		if a.TLS != nil {
			config = a.TLS.Clone()
			config.ServerName = u.Hostname()
			// The link is an upgraded HTTP/1.1 stream; never offer HTTP/2.
			config.NextProtos = []string{"http/1.1"}
		}
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: config}).DialContext(handshake, "tcp", address)
	} else {
		conn, err = dialer.DialContext(handshake, "tcp", address)
	}
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(handshake, func() { conn.Close() })
	defer stop()
	req, _ := http.NewRequest(http.MethodGet, strings.TrimSuffix(a.URL, "/")+Path, nil)
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set(ServerHeader, a.Server)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", Protocol)
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	res, err := http.ReadResponse(reader, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
		res.Body.Close()
		conn.Close()
		if e.Error != "" {
			return nil, fmt.Errorf("the panel refused the link: HTTP %d: %s", res.StatusCode, e.Error)
		}
		return nil, fmt.Errorf("the panel refused the link: HTTP %d", res.StatusCode)
	}
	if !stop() {
		conn.Close()
		return nil, handshake.Err()
	}
	return &bufferedConn{Conn: conn, r: reader}, nil
}

// idle pings the link until the panel claims it (true, after the ready
// byte) or it dies.
func (a Agent) idle(ctx context.Context, conn net.Conn) bool {
	var writeMu sync.Mutex
	claimed := false
	done := make(chan struct{})
	defer close(done)
	stopCtx := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopCtx()
	go func() {
		ticker := time.NewTicker(Heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			writeMu.Lock()
			if claimed {
				writeMu.Unlock()
				return
			}
			_, err := conn.Write([]byte{framePing})
			writeMu.Unlock()
			if err != nil {
				conn.Close()
				return
			}
		}
	}()
	b := make([]byte, 1)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(IdleTimeout))
		if _, err := io.ReadFull(conn, b); err != nil {
			conn.Close()
			return false
		}
		if b[0] != frameClaim {
			continue
		}
		_ = conn.SetReadDeadline(time.Time{})
		writeMu.Lock()
		claimed = true
		_, err := conn.Write([]byte{frameReady})
		writeMu.Unlock()
		if err != nil {
			conn.Close()
			return false
		}
		return ctx.Err() == nil
	}
}

// serve answers the panel's HTTP requests on a claimed link until the
// panel closes it.
func (a Agent) serve(ctx context.Context, conn net.Conn) {
	server := &http.Server{Handler: a.Handler, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	listener := newOneConn(conn)
	stop := context.AfterFunc(ctx, func() { server.Close() })
	defer stop()
	_ = server.Serve(listener)
}

// oneConn is a listener of one connection: Accept returns it once, then
// blocks until it closes.
type oneConn struct {
	conn   net.Conn
	once   sync.Once
	closed chan struct{}
	taken  bool
	mu     sync.Mutex
}

type trackedConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

func newOneConn(conn net.Conn) *oneConn {
	closed := make(chan struct{})
	return &oneConn{conn: &trackedConn{Conn: conn, closed: closed}, closed: closed}
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.taken {
		l.taken = true
		l.mu.Unlock()
		return l.conn, nil
	}
	l.mu.Unlock()
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	l.once.Do(func() {
		l.mu.Lock()
		taken := l.taken
		l.mu.Unlock()
		if !taken {
			l.conn.Close()
		}
	})
	return nil
}

func (l *oneConn) Addr() net.Addr { return l.conn.LocalAddr() }
