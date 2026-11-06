package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/movetunnel"
)

func TestNetworkSelectionSkipsTheAnchorAndContainerBridges(t *testing.T) {
	cidr := func(text string) *net.IPNet {
		ip, network, err := net.ParseCIDR(text)
		if err != nil {
			t.Fatal(err)
		}
		network.IP = ip
		return network
	}
	droplet := []interfaceAddresses{
		{Name: "lo", Up: true, Loopback: true, Addrs: []net.Addr{cidr("127.0.0.1/8")}},
		{Name: "eth0", Up: true, Addrs: []net.Addr{cidr("203.0.113.25/20"), cidr("10.10.0.20/16"), cidr("2001:db8:4:1d0::5/64")}},
		{Name: "eth1", Up: true, Addrs: []net.Addr{cidr("10.30.0.2/16")}},
		{Name: "docker0", Up: true, Addrs: []net.Addr{cidr("172.17.0.1/16")}},
		{Name: "br-1a2b3c", Up: true, Addrs: []net.Addr{cidr("172.18.0.1/16")}},
	}
	info := selectAddresses(droplet)
	if info.IPv4 != "203.0.113.25" || info.IPv6 != "2001:db8:4:1d0::5" || info.PrivateIPv4 != "10.30.0.2" || info.PrivateNetwork != "10.30.0.0/16" || !slices.Equal(info.PrivateNetworks, []string{"10.30.0.2/16"}) {
		t.Fatalf("the VPC address on eth1 wins over the anchor on eth0 and the bridges: %+v", info)
	}
	// Without a VPC the anchor is the only private address: reported, and
	// the panel's reachability probe decides whether it carries anything.
	alone := []interfaceAddresses{{Name: "eth0", Up: true, Addrs: []net.Addr{cidr("203.0.113.25/20"), cidr("10.10.0.20/16")}}}
	if info := selectAddresses(alone); info.PrivateIPv4 != "10.10.0.20" || !slices.Equal(info.PrivateNetworks, []string{"10.10.0.20/16"}) {
		t.Fatalf("%+v", info)
	}
	// A private-only host on two private networks reports both, in order;
	// 10.10/16 without a public address on its interface is no anchor.
	private := []interfaceAddresses{
		{Name: "ens3", Up: true, Addrs: []net.Addr{cidr("10.10.4.7/16")}},
		{Name: "ens4", Up: true, Addrs: []net.Addr{cidr("192.168.10.7/24")}},
		{Name: "ens5", Up: false, Addrs: []net.Addr{cidr("192.168.20.7/24")}},
	}
	if info := selectAddresses(private); info.PrivateIPv4 != "10.10.4.7" || !slices.Equal(info.PrivateNetworks, []string{"10.10.4.7/16", "192.168.10.7/24"}) || info.IPv4 != "" {
		t.Fatalf("%+v", info)
	}
}

func TestNetworkDialErrorNamesTheTargetAndTheFailure(t *testing.T) {
	if err := moveDialError("203.0.113.25:41237", 5*time.Second, context.DeadlineExceeded); err.Error() != "connection to 203.0.113.25:41237 timed out after 5s" {
		t.Fatal(err)
	}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	if err := moveDialError("10.30.0.2:41237", 5*time.Second, refused); err.Error() != "connection to 10.30.0.2:41237 was refused (nothing listens there, or a firewall rejects it)" {
		t.Fatal(err)
	}
	other := errors.New("dial tcp 10.30.0.2:41237: some other failure")
	if err := moveDialError("10.30.0.2:41237", 5*time.Second, other); err.Error() != "connection to 10.30.0.2:41237 failed: some other failure" {
		t.Fatal(err)
	}
	gateway, peers, err := parseMoveTunnelNetwork("fd00::/64 fd00::1;172.19.0.0/16 172.19.0.1;", "lidza-db-shop")
	if err != nil || gateway != "172.19.0.1" || !slices.Equal(peers, []string{"172.19.0.0/16"}) {
		t.Fatalf("%s %v %v", gateway, peers, err)
	}
	if gateway, _, _ := parseMoveTunnelNetwork("172.20.0.0/16 ;", "n"); gateway != "172.20.0.1" {
		t.Fatalf("a network without a reported gateway uses its first address: %s", gateway)
	}
	if _, _, err := parseMoveTunnelNetwork("fd00::/64 fd00::1;", "n"); err == nil {
		t.Fatal("an IPv6-only network has no IPv4 gateway")
	}
}

// tunnelTestServer serves the agent's routes on a real listener (upgrades
// need a connection to take over) with the test manager's database as both
// the source and the destination.
func tunnelTestServer(t *testing.T) (*Manager, *httptest.Server) {
	t.Helper()
	m, _, _ := replicationTestManager(t)
	previousBind := moveTunnelBind
	moveTunnelBind = func(context.Context, moveDatabaseTarget) (string, []string, error) {
		return "127.0.0.1", []string{"127.0.0.1/32"}, nil
	}
	t.Cleanup(func() { moveTunnelBind = previousBind })
	srv := httptest.NewServer(Handler(m))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		set := m.moveTunnels()
		set.mu.Lock()
		for _, tunnel := range set.tunnels {
			tunnel.Close()
		}
		set.mu.Unlock()
	})
	return m, srv
}

const (
	tunnelTestSourceToken = "source-token-0123456789abcdef0123456789"
	tunnelTestTargetToken = "target-token-0123456789abcdef0123456789"
)

func postJSON(t *testing.T, srv *httptest.Server, path string, in, out any) int {
	t.Helper()
	body, _ := json.Marshal(in)
	req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

// panelLeg plays the control panel's part of one leg: an idle stream to
// the destination that, once claimed, is piped to a stream to the source.
func panelLeg(t *testing.T, srv *httptest.Server) chan error {
	t.Helper()
	client := movetunnel.Client(nil)
	leg, err := movetunnel.Dial(context.Background(), client, srv.URL+"/v1/move/databases/shopdb/subscription/tunnel?move=m1", strings.Repeat("a", 32), tunnelTestTargetToken)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		if err := movetunnel.AwaitClient(context.Background(), leg); err != nil {
			done <- err
			return
		}
		source, err := movetunnel.Dial(context.Background(), client, srv.URL+"/v1/move/databases/shopdb/replication/stream?move=m1", strings.Repeat("a", 32), tunnelTestSourceToken)
		if err != nil {
			movetunnel.Fail(leg)
			leg.Close()
			done <- err
			return
		}
		movetunnel.Ack(leg)
		movetunnel.Pipe(context.Background(), leg, source)
		done <- nil
	}()
	return done
}

func waitIdle(t *testing.T, m *Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		set := m.moveTunnels()
		set.mu.Lock()
		tunnel := set.tunnels["shopdb"]
		set.mu.Unlock()
		if tunnel != nil && tunnel.pool.Idle() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the destination never had %d idle legs", want)
}

func TestMoveReplicationTunnelCarriesBytesBothWaysThroughThePanel(t *testing.T) {
	m, srv := tunnelTestServer(t)
	heartbeat, idle := movetunnel.Heartbeat, movetunnel.IdleTimeout
	movetunnel.Heartbeat, movetunnel.IdleTimeout = 30*time.Millisecond, 250*time.Millisecond
	t.Cleanup(func() { movetunnel.Heartbeat, movetunnel.IdleTimeout = heartbeat, idle })

	// The source publishes on the panel path: no relay, no firewall rule.
	var endpoint MoveReplicationEndpoint
	if code := postJSON(t, srv, "/v1/move/databases/shopdb/replication", MoveReplicationRequest{Move: "m1", Path: "panel", StreamToken: tunnelTestSourceToken}, &endpoint); code != 200 {
		t.Fatalf("publish: %d", code)
	}
	if endpoint.Host != "" || endpoint.Port != 0 || endpoint.Firewall != "not needed" || endpoint.SSLMode != "disable" || len(endpoint.Password) != 64 {
		t.Fatalf("%+v", endpoint)
	}
	if record, _ := m.moveReplicationRecord("shopdb"); record.Path != "panel" || record.StreamToken == tunnelTestSourceToken || record.Port != 0 {
		t.Fatalf("the record keeps the path and the token's digest only: %+v", record)
	}
	if code := postJSON(t, srv, "/v1/move/databases/shopdb/replication", MoveReplicationRequest{Move: "m1", Path: "panel", StreamToken: "short"}, nil); code != 409 {
		t.Fatalf("a short stream token is refused: %d", code)
	}

	// The destination's listener.
	var tunnel MoveTunnelEndpoint
	if code := postJSON(t, srv, "/v1/move/databases/shopdb/subscription/tunnel", MoveTunnelRequest{Move: "m1", Token: tunnelTestTargetToken}, &tunnel); code != 200 {
		t.Fatalf("tunnel: %d", code)
	}
	if tunnel.Host != "127.0.0.1" || tunnel.Port == 0 || tunnel.Firewall != "not needed" {
		t.Fatalf("%+v", tunnel)
	}
	var again MoveTunnelEndpoint
	if postJSON(t, srv, "/v1/move/databases/shopdb/subscription/tunnel", MoveTunnelRequest{Move: "m1", Token: tunnelTestTargetToken}, &again); again.Port != tunnel.Port {
		t.Fatal("a repeated request keeps the port the subscription names")
	}
	if code := postJSON(t, srv, "/v1/move/databases/shopdb/subscription/tunnel", MoveTunnelRequest{Move: "m2", Token: tunnelTestTargetToken}, nil); code != 409 {
		t.Fatalf("another move cannot take the tunnel over: %d", code)
	}

	// Legs are bound to the move and its token.
	client := movetunnel.Client(nil)
	for _, tc := range []struct {
		path, token string
		status      int
	}{
		{"/v1/move/databases/shopdb/subscription/tunnel?move=m1", "wrong-token", 403},
		{"/v1/move/databases/shopdb/subscription/tunnel?move=m9", tunnelTestTargetToken, 404},
		{"/v1/move/databases/shopdb/replication/stream?move=m1", tunnelTestTargetToken, 403},
		{"/v1/move/databases/shopdb/replication/stream?move=m9", tunnelTestSourceToken, 404},
	} {
		_, err := movetunnel.Dial(context.Background(), client, srv.URL+tc.path, strings.Repeat("a", 32), tc.token)
		var refused *movetunnel.RefusedError
		if !errors.As(err, &refused) || refused.Status != tc.status {
			t.Fatalf("%s with %s: %v", tc.path, tc.token, err)
		}
	}
	if _, err := movetunnel.Dial(context.Background(), client, srv.URL+"/v1/move/databases/shopdb/replication/stream?move=m1", "wrong-agent-key-wrong-agent-key!", tunnelTestSourceToken); err == nil {
		t.Fatal("streams sit behind the agent key")
	}

	// One leg, kept alive by heartbeats past the idle timeout.
	done := panelLeg(t, srv)
	waitIdle(t, m, 1)
	time.Sleep(3 * movetunnel.IdleTimeout)
	waitIdle(t, m, 1)

	// The destination's PostgreSQL connects: the bytes reach the source's
	// database (an echo) and come back, both ways at once.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(tunnel.Host, strconv.Itoa(tunnel.Port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 4<<20)
	rand.Read(payload)
	written := make(chan error, 1)
	go func() { _, err := conn.Write(payload); written <- err }()
	echo := make([]byte, len(payload))
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, echo); err != nil || !bytes.Equal(echo, payload) {
		t.Fatalf("the echo through the tunnel differs: %v", err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	waitIdle(t, m, 0)
	conn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the panel's pipe ends with the client's connection")
	}

	// The source cannot reach its database: the stream is refused with the
	// address and the error, and the waiting client is closed.
	previousDial := moveStreamDial
	moveStreamDial = func(context.Context, string) (net.Conn, error) {
		return nil, context.DeadlineExceeded
	}
	done = panelLeg(t, srv)
	waitIdle(t, m, 1)
	conn, err = net.DialTimeout("tcp", net.JoinHostPort(tunnel.Host, strconv.Itoa(tunnel.Port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = <-done
	moveStreamDial = previousDial
	var refused *movetunnel.RefusedError
	if !errors.As(err, &refused) || refused.Status != 502 || !strings.Contains(refused.Message, "the source database is unreachable from its agent: connection to 127.0.0.1:") || !strings.Contains(refused.Message, "timed out after 10s") {
		t.Fatalf("%v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := conn.Read(make([]byte, 1)); n != 0 || err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the destination closes a client whose source stream failed: %d %v", n, err)
	}
	conn.Close()

	// After an agent restart the record brings the listener back on its
	// port when the panel's next leg arrives.
	set := m.moveTunnels()
	set.mu.Lock()
	set.tunnels["shopdb"].Close()
	delete(set.tunnels, "shopdb")
	set.mu.Unlock()
	done = panelLeg(t, srv)
	waitIdle(t, m, 1)
	set.mu.Lock()
	port := set.tunnels["shopdb"].Port()
	set.mu.Unlock()
	if port != tunnel.Port {
		t.Fatalf("the listener came back on %d, not %d", port, tunnel.Port)
	}

	// Teardown: the subscription's removal closes the listener and the leg;
	// the publication's removal ends the source's streams.
	w := moveRequest(t, Handler(m), "DELETE", "/v1/move/databases/shopdb/subscription?move=m1", nil, nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if _, found := m.moveTunnelRecord("shopdb"); found {
		t.Fatal("the tunnel record goes with the subscription")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the idle leg ends with an error when the destination closes it")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the idle leg is closed at the teardown")
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort(tunnel.Host, strconv.Itoa(tunnel.Port)), 300*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("the listener stops")
	}
	if w := moveRequest(t, Handler(m), "DELETE", "/v1/move/databases/shopdb/replication", nil, nil); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if _, found := m.moveReplicationRecord("shopdb"); found {
		t.Fatal("the publication record goes")
	}
}

func TestMoveTunnelSweepClosesExpiredListeners(t *testing.T) {
	m, srv := tunnelTestServer(t)
	var tunnel MoveTunnelEndpoint
	if code := postJSON(t, srv, "/v1/move/databases/shopdb/subscription/tunnel", MoveTunnelRequest{Move: "m1", Token: tunnelTestTargetToken}, &tunnel); code != 200 {
		t.Fatal(code)
	}
	m.moveReplicationSweep(time.Now())
	if _, found := m.moveTunnelRecord("shopdb"); !found {
		t.Fatal("a live tunnel stays")
	}
	m.moveReplicationSweep(time.Now().Add(moveReplicationLifetime + time.Hour))
	if _, found := m.moveTunnelRecord("shopdb"); found {
		t.Fatal("an expired tunnel goes")
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort(tunnel.Host, strconv.Itoa(tunnel.Port)), 300*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("its listener stops")
	}
}

func TestMoveTunnelReachProbesAPortWithAShortTimeout(t *testing.T) {
	m, _, _ := replicationTestManager(t)
	h := Handler(m)
	body, _ := json.Marshal(MoveReachRequest{Host: "192.0.2.9", Port: 41237})
	w := moveRequest(t, h, "POST", "/v1/move/reach", bytes.NewReader(body), nil)
	var result MoveReachResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || result.Reachable || result.Error != "connection to 192.0.2.9:41237 timed out after 5s" {
		t.Fatalf("status %d: %+v", w.Code, result)
	}
	body, _ = json.Marshal(MoveReachRequest{Host: "10.30.0.2", Port: 41237})
	w = moveRequest(t, h, "POST", "/v1/move/reach", bytes.NewReader(body), nil)
	result = MoveReachResult{}
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || !result.Reachable || result.Error != "" {
		t.Fatalf("status %d: %+v", w.Code, result)
	}
	body, _ = json.Marshal(MoveReachRequest{Host: "example.com", Port: 41237})
	if w := moveRequest(t, h, "POST", "/v1/move/reach", bytes.NewReader(body), nil); w.Code != 400 {
		t.Fatalf("a hostname is refused: %d", w.Code)
	}
	previous := moveDetectAddresses
	moveDetectAddresses = func() NetworkInfo {
		return NetworkInfo{IPv4: "203.0.113.7", PrivateIPv4: "10.30.0.2", PrivateNetwork: "10.30.0.0/16", PrivateNetworks: []string{"10.30.0.2/16"}}
	}
	t.Cleanup(func() { moveDetectAddresses = previous })
	w = moveRequest(t, h, "GET", "/v1/move/host", nil, nil)
	var host MoveHost
	json.Unmarshal(w.Body.Bytes(), &host)
	if !host.Tunnel || !slices.Equal(host.PrivateNetworks, []string{"10.30.0.2/16"}) {
		t.Fatalf("%+v", host)
	}
}
