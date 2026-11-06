package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/movetunnel"
)

// moveTunnelFake plays both agents' tunnel routes with the real
// destination pool (internal/movetunnel) and an echo database behind the
// source.
type moveTunnelFake struct {
	mu         sync.Mutex
	listener   net.Listener
	pool       *movetunnel.Pool
	legs       []net.Conn
	streams    int
	sourceDown bool
	reached    []agent.MoveReachRequest
	backend    string
	// unreachable answers the reachability probe; nil reaches everything.
	unreachable func(agent.MoveReachRequest) string
}

func (tf *moveTunnelFake) with(fn func()) {
	tf.mu.Lock()
	defer tf.mu.Unlock()
	fn()
}

func (tf *moveTunnelFake) idle() int {
	tf.mu.Lock()
	pool := tf.pool
	tf.mu.Unlock()
	if pool == nil {
		return 0
	}
	return pool.Idle()
}

func (tf *moveTunnelFake) address() string {
	tf.mu.Lock()
	defer tf.mu.Unlock()
	if tf.listener == nil {
		return ""
	}
	return tf.listener.Addr().String()
}

func (tf *moveTunnelFake) close() {
	tf.mu.Lock()
	defer tf.mu.Unlock()
	if tf.listener != nil {
		tf.listener.Close()
		tf.pool.Close()
		tf.listener, tf.pool = nil, nil
	}
}

func moveEchoBackend(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	return listener.Addr().String()
}

// tunnels gives both fake agents the routes of replication through the
// panel and the reachability probe.
func (fx *moveFixture) tunnels(t *testing.T) *moveTunnelFake {
	tf := &moveTunnelFake{backend: moveEchoBackend(t)}
	t.Cleanup(tf.close)
	fx.a.host.Tunnel, fx.b.host.Tunnel = true, true
	fx.b.mux.HandleFunc("POST /v1/move/reach", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveReachRequest
		json.NewDecoder(r.Body).Decode(&in)
		tf.mu.Lock()
		tf.reached = append(tf.reached, in)
		unreachable := tf.unreachable
		tf.mu.Unlock()
		if unreachable != nil {
			if detail := unreachable(in); detail != "" {
				agent.JSON(w, 200, agent.MoveReachResult{Error: detail})
				return
			}
		}
		agent.JSON(w, 200, agent.MoveReachResult{Reachable: true})
	})
	fx.b.mux.HandleFunc("POST /v1/move/databases/{id}/subscription/tunnel", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveTunnelRequest
		json.NewDecoder(r.Body).Decode(&in)
		if in.Token != fx.c.moveTunnelToken(in.Move, "destination", r.PathValue("id")) {
			agent.JSON(w, 409, map[string]string{"error": "token"})
			return
		}
		tf.mu.Lock()
		defer tf.mu.Unlock()
		if tf.listener == nil {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				agent.JSON(w, 409, map[string]string{"error": err.Error()})
				return
			}
			tf.listener, tf.pool = listener, movetunnel.NewPool()
			pool := tf.pool
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					go pool.Handle(conn)
				}
			}()
		}
		port := tf.listener.Addr().(*net.TCPAddr).Port
		agent.JSON(w, 200, agent.MoveTunnelEndpoint{Host: "127.0.0.1", Port: port, Firewall: "not needed"})
	})
	fx.b.streams.HandleFunc("GET /v1/move/databases/{id}/subscription/tunnel", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(movetunnel.TokenHeader) != fx.c.moveTunnelToken(r.URL.Query().Get("move"), "destination", r.PathValue("id")) {
			agent.JSON(w, 403, map[string]string{"error": "the tunnel token does not match the move"})
			return
		}
		tf.mu.Lock()
		pool := tf.pool
		tf.mu.Unlock()
		if pool == nil {
			agent.JSON(w, 404, map[string]string{"error": "the database has no replication tunnel for this move"})
			return
		}
		conn, err := movetunnel.Upgrade(w, r)
		if err != nil {
			return
		}
		tf.mu.Lock()
		tf.legs = append(tf.legs, conn)
		tf.mu.Unlock()
		pool.ServeLeg(conn)
	})
	fx.a.streams.HandleFunc("GET /v1/move/databases/{id}/replication/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(movetunnel.TokenHeader) != fx.c.moveTunnelToken(r.URL.Query().Get("move"), "source", r.PathValue("id")) {
			agent.JSON(w, 403, map[string]string{"error": "the stream token does not match the move"})
			return
		}
		tf.mu.Lock()
		down := tf.sourceDown
		tf.streams++
		tf.mu.Unlock()
		if down {
			agent.JSON(w, 502, map[string]string{"error": "the source database is unreachable from its agent: connection to 172.18.0.2:5432 timed out after 10s"})
			return
		}
		backend, err := net.Dial("tcp", tf.backend)
		if err != nil {
			agent.JSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		conn, err := movetunnel.Upgrade(w, r)
		if err != nil {
			backend.Close()
			return
		}
		movetunnel.Pipe(context.Background(), conn, backend)
	})
	return tf
}

// moveTunnelEcho connects like the destination's PostgreSQL would and
// checks that a payload comes back from the source's database unchanged.
func moveTunnelEcho(address string, size int) error {
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	payload := make([]byte, size)
	rand.Read(payload)
	written := make(chan error, 1)
	go func() { _, err := conn.Write(payload); written <- err }()
	echo := make([]byte, size)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, echo); err != nil {
		return err
	}
	if !bytes.Equal(echo, payload) {
		return io.ErrUnexpectedEOF
	}
	return <-written
}

func waitTunnel(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(what)
}

func TestMoveTunnelCopiesBothWaysRefillsThePoolAndTearsDown(t *testing.T) {
	fx := newMoveFixture(t)
	tf := fx.tunnels(t)
	heartbeat, idle := movetunnel.Heartbeat, movetunnel.IdleTimeout
	movetunnel.Heartbeat, movetunnel.IdleTimeout = 20*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { movetunnel.Heartbeat, movetunnel.IdleTimeout = heartbeat, idle })
	var endpoint agent.MoveTunnelEndpoint
	if err := fx.c.moveCall(context.Background(), time.Minute, "beta", http.MethodPost, "/v1/move/databases/shop-db0001/subscription/tunnel", agent.MoveTunnelRequest{Move: "m1", Token: fx.c.moveTunnelToken("m1", "destination", "shop-db0001")}, &endpoint); err != nil {
		t.Fatal(err)
	}
	tunnel := fx.c.moveTunnelStart("m1", "alpha", "beta", "shop", "shop-db0001")
	if again := fx.c.moveTunnelStart("m1", "alpha", "beta", "shop", "shop-db0001"); again != tunnel {
		t.Fatal("starting a running tunnel keeps it")
	}
	if err := tunnel.waitReady(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	waitTunnel(t, "the panel keeps two idle legs", func() bool { return tf.idle() == moveTunnelIdleLegs })
	// Heartbeats keep idle legs past the idle timeout.
	time.Sleep(3 * movetunnel.IdleTimeout)
	if tf.idle() != moveTunnelIdleLegs {
		t.Fatalf("idle legs %d after the idle timeout", tf.idle())
	}
	address := net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.Port))
	// Bytes both ways at once, on two connections at the same time; the
	// pool refills behind each claimed leg.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- moveTunnelEcho(address, 2<<20) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("the echo through the panel differs: %v", err)
		}
	}
	waitTunnel(t, "the pool refills to two idle legs", func() bool { return tf.idle() == moveTunnelIdleLegs })
	tf.with(func() {
		if tf.streams != 2 {
			t.Fatalf("one source stream per connection, not %d", tf.streams)
		}
	})
	// Dead legs (the destination agent restarted, a proxy dropped them)
	// are replaced.
	tf.with(func() {
		for _, leg := range tf.legs {
			leg.Close()
		}
	})
	waitTunnel(t, "dead legs are replaced", func() bool { return tf.idle() == moveTunnelIdleLegs })
	if err := moveTunnelEcho(address, 1024); err != nil {
		t.Fatal(err)
	}
	// A source that cannot reach its database: the client is closed and
	// the failure is kept for the move's messages.
	tf.with(func() { tf.sourceDown = true })
	if err := moveTunnelEcho(address, 16); err == nil {
		t.Fatal("the client of a failed source stream is closed")
	}
	waitTunnel(t, "the failure is recorded", func() bool {
		return strings.Contains(fx.c.moveTunnelError("m1", "shop"), "connection to 172.18.0.2:5432 timed out after 10s")
	})
	tf.with(func() { tf.sourceDown = false })
	if err := moveTunnelEcho(address, 16); err != nil {
		t.Fatal(err)
	}
	if fx.c.moveTunnelError("m1", "shop") != "" {
		t.Fatal("a working stream clears the failure")
	}
	// Teardown: the legs close and nothing reconnects.
	fx.c.moveTunnelStop("m1", "shop")
	waitTunnel(t, "the teardown closes the idle legs", func() bool { return tf.idle() == 0 })
	time.Sleep(50 * time.Millisecond)
	if tf.idle() != 0 {
		t.Fatal("a stopped tunnel opens no legs")
	}
	if fx.c.moveTunnelError("m1", "shop") != "" {
		t.Fatal("no tunnel, no error")
	}
}

func TestMoveTunnelWaitsForTheDestinationAndNamesWhy(t *testing.T) {
	fx := newMoveFixture(t)
	fx.tunnels(t)
	// No listener on the destination: every leg is refused.
	tunnel := fx.c.moveTunnelStart("m1", "alpha", "beta", "shop", "shop-db0001")
	err := tunnel.waitReady(context.Background(), 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "the control panel opened no replication stream to beta within") || !strings.Contains(err.Error(), "the destination beta did not accept a stream: HTTP 404: the database has no replication tunnel for this move") {
		t.Fatal(err)
	}
}

func TestMoveReplicationRoutesPreferAReachablePrivateNetworkThenThePanel(t *testing.T) {
	source := agent.MoveHost{IPv4: moveTestSourceIP, PrivateIPv4: "10.10.0.20", PrivateNetwork: "10.10.0.0/16", PrivateNetworks: []string{"10.10.0.20/16", "10.30.0.2/16"}, Tunnel: true}
	target := agent.MoveHost{IPv4: moveTestTargetIP, IPv6: moveTestTargetIP6, PrivateNetworks: []string{"10.30.0.3/16"}, Tunnel: true}
	routes := moveReplicationRoutes(source, target, "")
	paths := []string{}
	for _, r := range routes {
		paths = append(paths, r.path)
	}
	if !slices.Equal(paths, []string{moveReplicationPathPrivate, moveReplicationPathPanel}) || routes[0].listen != "10.30.0.2" || !slices.Equal(routes[0].peers, []string{"10.30.0.3"}) || routes[0].via != "over the private network 10.30.0.0/16" || routes[1].via != "through the control panel" {
		t.Fatalf("the shared network among several is found: %+v", routes)
	}
	// Different VPCs: only the panel; never the public relay by default.
	other := target
	other.PrivateNetworks = []string{"10.20.0.3/16"}
	if routes := moveReplicationRoutes(source, other, moveReplicationNetworkAuto); len(routes) != 1 || routes[0].path != moveReplicationPathPanel {
		t.Fatalf("%+v", routes)
	}
	if routes := moveReplicationRoutes(source, target, moveReplicationPathPanel); len(routes) != 1 || routes[0].path != moveReplicationPathPanel {
		t.Fatalf("panel on request skips the private network: %+v", routes)
	}
	public := moveReplicationRoutes(source, target, moveReplicationPathPublic)
	if len(public) != 1 || public[0].path != moveReplicationPathPublic || !public[0].tls || public[0].listen != moveTestSourceIP || !slices.Equal(public[0].peers, []string{moveTestTargetIP, moveTestTargetIP6}) {
		t.Fatalf("the public relay only on request: %+v", public)
	}
	// An older agent without PrivateNetworks still matches by its single
	// address; without the tunnel only the private path remains.
	old := agent.MoveHost{PrivateIPv4: "10.30.0.4", PrivateNetwork: "10.30.0.0/16"}
	if routes := moveReplicationRoutes(source, old, ""); len(routes) != 1 || routes[0].path != moveReplicationPathPrivate || routes[0].peers[0] != "10.30.0.4" {
		t.Fatalf("%+v", routes)
	}
	if routes := moveReplicationRoutes(agent.MoveHost{}, agent.MoveHost{}, ""); len(routes) != 0 {
		t.Fatalf("%+v", routes)
	}
}

func TestMoveTunnelSyncResumesTunnelsFromTheMoveState(t *testing.T) {
	fx := newMoveFixture(t)
	rep := fx.replicationSource()
	tf := rep.tunnel
	rep.with(func() { rep.unreachablePrivate = true })
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true, "auto_cutover": false})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if job.Databases[0].Link == nil || job.Databases[0].Link.Path != moveReplicationPathPanel {
		t.Fatalf("%+v", job.Databases[0].Link)
	}
	waitTunnel(t, "the waiting move keeps its tunnel", func() bool { return tf.idle() == moveTunnelIdleLegs })
	// A panel restart: no tunnel runs until the move state brings it back.
	fx.c.moveTunnelStop(view.ID, "shop")
	waitTunnel(t, "the legs close", func() bool { return tf.idle() == 0 })
	fx.c.moveTick(context.Background())
	waitTunnel(t, "the tick resumes the tunnel", func() bool { return tf.idle() == moveTunnelIdleLegs })
	if err := moveTunnelEcho(tf.address(), 4096); err != nil {
		t.Fatal(err)
	}
	// The abort tears the tunnel down with the replication.
	if code := moveAction(t, fx.c, view.ID, "abort", nil); code != 202 {
		t.Fatal(code)
	}
	waitMove(t, fx.c, view.ID, moveStateAborted)
	waitTunnel(t, "the abort ends the tunnel", func() bool {
		fx.c.moveTunnelMu.Lock()
		defer fx.c.moveTunnelMu.Unlock()
		return len(fx.c.moveTunnels) == 0
	})
	fx.c.moveTick(context.Background())
	fx.c.moveTunnelMu.Lock()
	running := len(fx.c.moveTunnels)
	fx.c.moveTunnelMu.Unlock()
	if running != 0 {
		t.Fatal("an ended move gets no tunnel back")
	}
}
