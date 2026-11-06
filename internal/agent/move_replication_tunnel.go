package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agim/lidza-deploy/internal/movetunnel"
)

// Replication through the control panel. A cloud firewall in front of a
// host (outside it, invisible to its own firewall) may admit nothing but
// HTTPS and SSH, so the destination's PostgreSQL cannot always reach a relay
// on the source. The panel reaches every agent already, so the
// subscription's connections ride the panel's agent channels instead
// (internal/movetunnel):
//
//   - destination: POST /v1/move/databases/{id}/subscription/tunnel starts a
//     listener the destination's PostgreSQL can reach (127.0.0.1 for a host
//     cluster, the database network's gateway for a container) and the
//     subscription's connection string names it; GET on the same route,
//     upgraded, is one idle leg the panel keeps open, and each connection
//     the listener accepts is handed to an idle leg;
//   - source: GET /v1/move/databases/{id}/replication/stream, upgraded,
//     connects one stream to the published database (the container's
//     address on its Docker network, or 127.0.0.1:port of a host cluster).
//
// Both routes sit behind the agent's bearer token like every /v1 route and
// are bound to the move: the record must name the move, and the stream
// carries a token derived for this move and database by the panel, whose
// SHA-256 the record keeps. The bytes travel the panel's existing channels
// to the agents (HTTPS through the agent's Caddy site, or the SSH tunnel the
// panel maintains), so no port opens anywhere and the subscription uses
// sslmode=disable on its local hop; the password is the move role's own.
// POST /v1/move/reach lets the panel probe a direct path before it chooses
// one.

const (
	moveTunnelPath       = "panel"
	moveReachTimeout     = 5 * time.Second
	moveTunnelMaxClients = 32
	moveStreamDialLimit  = 10 * time.Second
)

// MoveTunnelRequest asks the destination for the local listener of a
// subscription that replicates through the control panel.
type MoveTunnelRequest struct {
	Move  string `json:"move"`
	Token string `json:"token"`
}

// MoveTunnelEndpoint is where the destination's PostgreSQL connects, with
// what the host firewall did (opened, inactive, unmanaged or not needed)
// and the legs the panel keeps open.
type MoveTunnelEndpoint struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Firewall string `json:"firewall"`
	Idle     int    `json:"idle"`
	Active   int    `json:"active"`
}

// MoveTunnelRecord is the destination's durable record of one listener, so
// a restarted agent listens on the port the subscription names again.
type MoveTunnelRecord struct {
	Move     string    `json:"move"`
	Token    string    `json:"token"` // SHA-256 of the panel's token
	Listen   string    `json:"listen"`
	Port     int       `json:"port"`
	Peers    []string  `json:"peers"`
	Firewall bool      `json:"firewall"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
}

// MoveReachRequest asks the agent to open a TCP connection.
type MoveReachRequest struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// MoveReachResult says whether the connection opened, or why not.
type MoveReachResult struct {
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

func moveTunnelTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func moveTunnelTokenMatches(hash, token string) bool {
	return hash != "" && token != "" && subtle.ConstantTimeCompare([]byte(hash), []byte(moveTunnelTokenHash(token))) == 1
}

// moveReachTCP opens and closes a TCP connection to host:port; tests
// replace it.
var moveReachTCP = func(ctx context.Context, host string, port int, timeout time.Duration) error {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", target)
	if err != nil {
		return moveDialError(target, timeout, err)
	}
	conn.Close()
	return nil
}

// moveDialError words a failed dial with its target and the network error,
// for example "connection to 203.0.113.25:41237 timed out after 5s".
func moveDialError(target string, timeout time.Duration, err error) error {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout():
		return fmt.Errorf("connection to %s timed out after %s", target, timeout)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("connection to %s was refused (nothing listens there, or a firewall rejects it)", target)
	case errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH):
		return fmt.Errorf("connection to %s failed: no route to the host", target)
	}
	text := err.Error()
	if _, after, ok := strings.Cut(text, target+": "); ok {
		text = after
	}
	return fmt.Errorf("connection to %s failed: %s", target, boundedErrorText(text, 200))
}

// ---- source side ----------------------------------------------------------------------

// moveStreamSet tracks the panel's open streams to each published database,
// so a teardown ends them.
type moveStreamSet struct {
	mu    sync.Mutex
	conns map[string]map[io.Closer]struct{}
}

var moveStreamSets sync.Map // *Manager -> *moveStreamSet

func (m *Manager) moveStreams() *moveStreamSet {
	value, _ := moveStreamSets.LoadOrStore(m, &moveStreamSet{conns: map[string]map[io.Closer]struct{}{}})
	return value.(*moveStreamSet)
}

func (s *moveStreamSet) add(id string, closers ...io.Closer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[id] == nil {
		s.conns[id] = map[io.Closer]struct{}{}
	}
	for _, c := range closers {
		s.conns[id][c] = struct{}{}
	}
}

func (s *moveStreamSet) remove(id string, closers ...io.Closer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range closers {
		delete(s.conns[id], c)
	}
	if len(s.conns[id]) == 0 {
		delete(s.conns, id)
	}
}

func (s *moveStreamSet) closeAll(id string) {
	s.mu.Lock()
	conns := s.conns[id]
	delete(s.conns, id)
	s.mu.Unlock()
	for c := range conns {
		c.Close()
	}
}

// moveCloseRelay stops a database's direct relay, if one runs.
func (m *Manager) moveCloseRelay(id string) {
	relays := m.moveRelays()
	relays.mu.Lock()
	defer relays.mu.Unlock()
	if relay := relays.relays[id]; relay != nil {
		relay.Close()
		delete(relays.relays, id)
	}
}

// moveStreamDial connects to the database behind a stream; tests replace it.
var moveStreamDial = func(ctx context.Context, address string) (net.Conn, error) {
	return (&net.Dialer{Timeout: moveStreamDialLimit}).DialContext(ctx, "tcp", address)
}

// moveReplicationStreamRoute connects one upgraded stream from the panel to
// a database published for a move on the panel path. The database is dialed
// before the upgrade, so a failure is an ordinary error answer naming the
// address.
func (m *Manager) moveReplicationStreamRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		Fail(w, 400, errors.New("invalid database"))
		return
	}
	record, found := m.moveReplicationRecord(id)
	if !found || record.Path != moveTunnelPath || record.Move != r.URL.Query().Get("move") {
		Fail(w, 404, errors.New("the database is not published through the control panel for this move"))
		return
	}
	if !moveTunnelTokenMatches(record.StreamToken, r.Header.Get(movetunnel.TokenHeader)) {
		Fail(w, 403, errors.New("the stream token does not match the move"))
		return
	}
	if !movetunnel.Requested(r) {
		Fail(w, 400, movetunnel.ErrNotUpgrade)
		return
	}
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		Fail(w, 404, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	_, address, err := moveDatabaseAddress(ctx, target)
	if err != nil {
		cancel()
		Fail(w, 409, err)
		return
	}
	backend, err := moveStreamDial(ctx, address)
	cancel()
	if err != nil {
		Fail(w, 502, fmt.Errorf("the source database is unreachable from its agent: %w", moveDialError(address, moveStreamDialLimit, err)))
		return
	}
	conn, err := movetunnel.Upgrade(w, r)
	if err != nil {
		backend.Close()
		return
	}
	streams := m.moveStreams()
	streams.add(id, conn, backend)
	defer streams.remove(id, conn, backend)
	movetunnel.Pipe(m.ctx, conn, backend)
}

// ---- destination side -------------------------------------------------------------------

// moveTunnelListener is one running listener with the panel's legs.
type moveTunnelListener struct {
	listener net.Listener
	allow    []netip.Prefix
	pool     *movetunnel.Pool
	wg       sync.WaitGroup
}

type moveTunnelSet struct {
	mu      sync.Mutex
	tunnels map[string]*moveTunnelListener
}

var moveTunnelSets sync.Map // *Manager -> *moveTunnelSet

func (m *Manager) moveTunnels() *moveTunnelSet {
	value, _ := moveTunnelSets.LoadOrStore(m, &moveTunnelSet{tunnels: map[string]*moveTunnelListener{}})
	return value.(*moveTunnelSet)
}

func startMoveTunnel(listen string, port int, peers []string) (*moveTunnelListener, error) {
	ip, err := netip.ParseAddr(listen)
	if err != nil {
		return nil, errors.New("the tunnel address must be an IP address of this host")
	}
	allow, err := relayPeers(peers)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("the replication tunnel could not listen on %s: %w", net.JoinHostPort(ip.String(), strconv.Itoa(port)), err)
	}
	t := &moveTunnelListener{listener: listener, allow: allow, pool: movetunnel.NewPool()}
	t.wg.Add(1)
	go t.serve()
	return t, nil
}

func (t *moveTunnelListener) Port() int       { return t.listener.Addr().(*net.TCPAddr).Port }
func (t *moveTunnelListener) Address() string { return t.listener.Addr().(*net.TCPAddr).IP.String() }

func (t *moveTunnelListener) serve() {
	defer t.wg.Done()
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			return
		}
		relay := replicationRelay{allow: t.allow}
		if !relay.allowed(conn.RemoteAddr()) || t.pool.Active() >= moveTunnelMaxClients {
			conn.Close()
			continue
		}
		go t.pool.Handle(conn)
	}
}

func (t *moveTunnelListener) Close() {
	t.listener.Close()
	t.pool.Close()
	t.wg.Wait()
}

// moveTunnelBind chooses where a destination database's listener binds and
// who may connect; tests replace it. A host cluster connects over loopback;
// a container database over its network's gateway, the host's address on
// that Docker network.
var moveTunnelBind = func(ctx context.Context, target moveDatabaseTarget) (string, []string, error) {
	if target.Host != nil {
		return "127.0.0.1", []string{"127.0.0.1/32"}, nil
	}
	out, err := command(ctx, "", nil, "docker", "network", "inspect", "-f", `{{range .IPAM.Config}}{{.Subnet}} {{.Gateway}};{{end}}`, target.Database.Network)
	if err != nil {
		return "", nil, fmt.Errorf("the database network %s could not be inspected", target.Database.Network)
	}
	return parseMoveTunnelNetwork(out, target.Database.Network)
}

// parseMoveTunnelNetwork reads "subnet gateway;" pairs and returns the
// first IPv4 gateway (the subnet's first address when Docker reports none)
// with its subnet.
func parseMoveTunnelNetwork(out, network string) (string, []string, error) {
	for _, entry := range strings.Split(out, ";") {
		fields := strings.Fields(entry)
		if len(fields) == 0 {
			continue
		}
		subnet, err := netip.ParsePrefix(fields[0])
		if err != nil || !subnet.Addr().Is4() {
			continue
		}
		subnet = subnet.Masked()
		gateway := subnet.Addr().Next()
		if len(fields) > 1 {
			if ip, err := netip.ParseAddr(fields[1]); err == nil && subnet.Contains(ip) {
				gateway = ip
			}
		}
		return gateway.String(), []string{subnet.String()}, nil
	}
	return "", nil, fmt.Errorf("the database network %s has no IPv4 subnet", network)
}

func (m *Manager) moveTunnelRecord(id string) (MoveTunnelRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.data.MoveTunnels[id]
	return record, ok
}

func (m *Manager) setMoveTunnelRecord(id string, record *MoveTunnelRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.MoveTunnels == nil {
		m.data.MoveTunnels = map[string]MoveTunnelRecord{}
	}
	if record == nil {
		delete(m.data.MoveTunnels, id)
	} else {
		m.data.MoveTunnels[id] = *record
	}
	return m.save()
}

// moveTunnelEnsure starts, or keeps, the listener of a destination
// database's subscription for a move. It is idempotent: a repeated request
// keeps the port the subscription names.
func (m *Manager) moveTunnelEnsure(ctx context.Context, id string, in MoveTunnelRequest) (MoveTunnelEndpoint, error) {
	if _, err := moveReplicationName(in.Move); err != nil {
		return MoveTunnelEndpoint{}, err
	}
	if len(in.Token) < 32 {
		return MoveTunnelEndpoint{}, errors.New("the tunnel needs a token of 32 or more characters")
	}
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		return MoveTunnelEndpoint{}, err
	}
	existing, found := m.moveTunnelRecord(id)
	if found && existing.Move != in.Move {
		return MoveTunnelEndpoint{}, fmt.Errorf("database %s already replicates through the control panel for move %s; tear that down first", id, existing.Move)
	}
	listen, peers, err := moveTunnelBind(ctx, target)
	if err != nil {
		return MoveTunnelEndpoint{}, err
	}
	set := m.moveTunnels()
	set.mu.Lock()
	tunnel := set.tunnels[id]
	if tunnel != nil && tunnel.Address() != listen {
		tunnel.Close()
		delete(set.tunnels, id)
		tunnel = nil
	}
	if tunnel == nil {
		port := 0
		if found && existing.Listen == listen {
			port = existing.Port
		}
		tunnel, err = startMoveTunnel(listen, port, peers)
		if err != nil && port != 0 {
			tunnel, err = startMoveTunnel(listen, 0, peers)
		}
		if err != nil {
			set.mu.Unlock()
			return MoveTunnelEndpoint{}, err
		}
		set.tunnels[id] = tunnel
	}
	set.mu.Unlock()
	now := time.Now().UTC()
	record := MoveTunnelRecord{Move: in.Move, Token: moveTunnelTokenHash(in.Token), Listen: listen, Port: tunnel.Port(), Peers: peers, Created: now, Expires: now.Add(moveReplicationLifetime)}
	if found {
		record.Created = existing.Created
		if existing.Firewall && existing.Port == record.Port && slices.Equal(existing.Peers, peers) {
			record.Firewall = true
		} else if existing.Firewall {
			_ = m.moveFirewallClose(ctx, existing.Port, existing.Peers)
		}
	}
	firewall := "not needed"
	if ip, _ := netip.ParseAddr(listen); !ip.IsLoopback() {
		// The container reaches the host through its network's gateway,
		// which the host firewall's input rules govern.
		if record.Firewall {
			firewall = "opened"
		} else {
			opened, state, err := m.moveFirewallOpen(ctx, record.Port, peers, in.Move)
			if err != nil {
				return MoveTunnelEndpoint{}, err
			}
			record.Firewall, firewall = opened, state
		}
	}
	if err := m.setMoveTunnelRecord(id, &record); err != nil {
		return MoveTunnelEndpoint{}, err
	}
	return MoveTunnelEndpoint{Host: listen, Port: record.Port, Firewall: firewall, Idle: tunnel.pool.Idle(), Active: tunnel.pool.Active()}, nil
}

// moveTunnelForLeg returns the running listener a leg of the panel joins,
// starting it again from its record after an agent restart.
func (m *Manager) moveTunnelForLeg(id, move, token string) (*moveTunnelListener, int, error) {
	record, found := m.moveTunnelRecord(id)
	if !found || record.Move != move {
		return nil, 404, errors.New("the database has no replication tunnel for this move")
	}
	if !moveTunnelTokenMatches(record.Token, token) {
		return nil, 403, errors.New("the tunnel token does not match the move")
	}
	set := m.moveTunnels()
	set.mu.Lock()
	tunnel := set.tunnels[id]
	if tunnel == nil {
		var err error
		tunnel, err = startMoveTunnel(record.Listen, record.Port, record.Peers)
		if err != nil {
			set.mu.Unlock()
			return nil, 409, err
		}
		set.tunnels[id] = tunnel
	}
	set.mu.Unlock()
	// Each leg renews the record's lifetime, so only a move nobody follows
	// any more expires.
	if time.Until(record.Expires) < moveReplicationLifetime-time.Hour {
		record.Expires = time.Now().UTC().Add(moveReplicationLifetime)
		_ = m.setMoveTunnelRecord(id, &record)
	}
	return tunnel, 0, nil
}

// moveTunnelClose stops a database's listener and its legs and removes the
// firewall rule and the record. move "" closes whatever runs.
func (m *Manager) moveTunnelClose(ctx context.Context, id, move string) error {
	record, found := m.moveTunnelRecord(id)
	if found && move != "" && record.Move != move {
		return nil
	}
	set := m.moveTunnels()
	set.mu.Lock()
	if tunnel := set.tunnels[id]; tunnel != nil {
		tunnel.Close()
		delete(set.tunnels, id)
	}
	set.mu.Unlock()
	if !found {
		return nil
	}
	if record.Firewall {
		if err := m.moveFirewallClose(ctx, record.Port, record.Peers); err != nil {
			return err
		}
	}
	return m.setMoveTunnelRecord(id, nil)
}

// moveTunnelSweep closes listeners whose move outlived its lifetime.
func (m *Manager) moveTunnelSweep(now time.Time) {
	m.mu.Lock()
	expired := []string{}
	for id, record := range m.data.MoveTunnels {
		if now.After(record.Expires) {
			expired = append(expired, id)
		}
	}
	m.mu.Unlock()
	for _, id := range expired {
		ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
		if err := m.moveTunnelClose(ctx, id, ""); err != nil {
			log.Printf("move replication of %s: expired tunnel not fully removed: %v", id, err)
		}
		cancel()
	}
}

// ---- routes -------------------------------------------------------------------------

func (m *Manager) moveTunnelRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/move/databases/{id}/replication/stream", m.moveReplicationStreamRoute)
	mux.HandleFunc("POST /v1/move/databases/{id}/subscription/tunnel", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !idPattern.MatchString(id) {
			Fail(w, 400, errors.New("invalid database"))
			return
		}
		var in MoveTunnelRequest
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		endpoint, err := m.moveTunnelEnsure(ctx, id, in)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, endpoint)
	})
	mux.HandleFunc("GET /v1/move/databases/{id}/subscription/tunnel", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !idPattern.MatchString(id) {
			Fail(w, 400, errors.New("invalid database"))
			return
		}
		tunnel, status, err := m.moveTunnelForLeg(id, r.URL.Query().Get("move"), r.Header.Get(movetunnel.TokenHeader))
		if err != nil {
			Fail(w, status, err)
			return
		}
		if !movetunnel.Requested(r) {
			Fail(w, 400, movetunnel.ErrNotUpgrade)
			return
		}
		conn, err := movetunnel.Upgrade(w, r)
		if err != nil {
			return
		}
		tunnel.pool.ServeLeg(conn)
	})
	mux.HandleFunc("DELETE /v1/move/databases/{id}/subscription/tunnel", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !idPattern.MatchString(id) {
			Fail(w, 400, errors.New("invalid database"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		if err := m.moveTunnelClose(ctx, id, r.URL.Query().Get("move")); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "removed"})
	})
	mux.HandleFunc("POST /v1/move/reach", func(w http.ResponseWriter, r *http.Request) {
		var in MoveReachRequest
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		ip, err := netip.ParseAddr(in.Host)
		if err != nil || in.Port < 1 || in.Port > 65535 {
			Fail(w, 400, errors.New("host must be an IP address and port 1-65535"))
			return
		}
		timeout := moveReachTimeout
		if in.TimeoutSeconds > 0 {
			timeout = time.Duration(min(in.TimeoutSeconds, 30)) * time.Second
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout+time.Second)
		defer cancel()
		if err := moveReachTCP(ctx, ip.String(), in.Port, timeout); err != nil {
			JSON(w, 200, MoveReachResult{Error: err.Error()})
			return
		}
		JSON(w, 200, MoveReachResult{Reachable: true})
	})
}
