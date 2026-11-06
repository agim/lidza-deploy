package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// Replication relay. A logical replication subscription needs a libpq
// connection from the destination's PostgreSQL to the source's, but the
// source database listens on a Docker network or on loopback only. For the
// move's duration the source agent runs a small TCP relay in its own
// process: it listens on one of the host's addresses, accepts connections
// from the destination's addresses alone and forwards them to the database.
//
// On a private network shared by both hosts the relay forwards bytes as they
// are. On the public internet it terminates TLS itself with a certificate
// generated for the move, whose PEM the control panel hands to the
// destination (sslmode=verify-ca): the PostgreSQL protocol starts in clear
// text with an SSLRequest, which the relay answers before the TLS handshake,
// and a client that tries to start in clear text on the public path is
// refused. The database behind the relay sees an ordinary local connection.

const (
	pgSSLRequestCode    = 80877103
	pgGSSENCRequestCode = 80877104
	pgCancelRequestCode = 80877102
	relayCertLifetime   = 30 * 24 * time.Hour
	relayHandshakeLimit = 15 * time.Second
	relayMaxConnections = 64
)

var errRelayPlaintext = errors.New("the replication relay on a public address accepts TLS connections only")

// replicationRelay is one listening relay.
type replicationRelay struct {
	listener net.Listener
	allow    []netip.Prefix
	target   string
	config   *tls.Config
	certPEM  string
	dial     func(ctx context.Context, address string) (net.Conn, error)

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// relayPeers parses the addresses allowed to connect: single addresses or
// CIDR networks.
func relayPeers(peers []string) ([]netip.Prefix, error) {
	out := []netip.Prefix{}
	for _, peer := range peers {
		if prefix, err := netip.ParsePrefix(peer); err == nil {
			out = append(out, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(peer)
		if err != nil {
			return nil, errors.New("peer addresses must be IP addresses or CIDR networks")
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	if len(out) == 0 {
		return nil, errors.New("the relay needs at least one peer address")
	}
	return out, nil
}

// relayCertificate makes a self-signed certificate for the relay's address;
// the certificate itself is the CA the destination verifies against.
func relayCertificate(address netip.Addr) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "lidza-move-relay"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(relayCertLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{address.AsSlice()},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, string(certPEM), nil
}

// newReplicationRelay listens on address (port 0 picks a free port) for the
// peers and forwards to target (host:port of the database). With useTLS the
// relay terminates TLS with a fresh certificate, returned as PEM.
func newReplicationRelay(address string, port int, peers []string, target string, useTLS bool) (*replicationRelay, error) {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return nil, errors.New("the relay address must be an IP address of this host")
	}
	allow, err := relayPeers(peers)
	if err != nil {
		return nil, err
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		return nil, errors.New("the relay target must be host:port")
	}
	r := &replicationRelay{allow: allow, target: target, conns: map[net.Conn]struct{}{}}
	if useTLS {
		cert, certPEM, err := relayCertificate(ip)
		if err != nil {
			return nil, err
		}
		r.config = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		r.certPEM = certPEM
	}
	r.dial = func(ctx context.Context, address string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	if err != nil {
		return nil, errors.New("the replication relay could not listen on " + ip.String() + ": " + err.Error())
	}
	r.listener = listener
	r.wg.Add(1)
	go r.serve()
	return r, nil
}

// Port is the port the relay listens on.
func (r *replicationRelay) Port() int {
	return r.listener.Addr().(*net.TCPAddr).Port
}

// Address is the address the relay listens on.
func (r *replicationRelay) Address() string {
	return r.listener.Addr().(*net.TCPAddr).IP.String()
}

// TLS reports whether the relay terminates TLS.
func (r *replicationRelay) TLS() bool { return r.config != nil }

func (r *replicationRelay) allowed(remote net.Addr) bool {
	addrPort, err := netip.ParseAddrPort(remote.String())
	if err != nil {
		return false
	}
	addr := addrPort.Addr().Unmap()
	for _, prefix := range r.allow {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (r *replicationRelay) serve() {
	defer r.wg.Done()
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			return
		}
		if !r.allowed(conn.RemoteAddr()) || !r.track(conn) {
			conn.Close()
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer r.untrack(conn)
			r.handle(conn)
		}()
	}
}

func (r *replicationRelay) track(conn net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || len(r.conns) >= relayMaxConnections {
		return false
	}
	r.conns[conn] = struct{}{}
	return true
}

func (r *replicationRelay) untrack(conn net.Conn) {
	r.mu.Lock()
	delete(r.conns, conn)
	r.mu.Unlock()
	conn.Close()
}

// handle answers the protocol's opening request and pipes the rest.
func (r *replicationRelay) handle(client net.Conn) {
	_ = client.SetDeadline(time.Now().Add(relayHandshakeLimit))
	stream, prefix, err := r.negotiate(client)
	if err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	backend, err := r.dial(ctx, r.target)
	cancel()
	if err != nil {
		return
	}
	defer backend.Close()
	if len(prefix) > 0 {
		if _, err := backend.Write(prefix); err != nil {
			return
		}
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, stream); done <- struct{}{} }()
	go func() { _, _ = io.Copy(stream, backend); done <- struct{}{} }()
	<-done
}

// negotiate reads the client's first request. It returns the stream to pipe
// (the TLS session on the public path) and any bytes already read that
// belong to the database.
func (r *replicationRelay) negotiate(client net.Conn) (io.ReadWriter, []byte, error) {
	var stream io.ReadWriter = client
	for range 3 {
		head := make([]byte, 8)
		if _, err := io.ReadFull(stream, head); err != nil {
			return nil, nil, err
		}
		code := binary.BigEndian.Uint32(head[4:8])
		switch {
		case binary.BigEndian.Uint32(head[0:4]) == 8 && code == pgSSLRequestCode:
			if r.config == nil {
				// No TLS on this path: the client continues in clear text.
				if _, err := stream.Write([]byte{'N'}); err != nil {
					return nil, nil, err
				}
				continue
			}
			if _, err := stream.Write([]byte{'S'}); err != nil {
				return nil, nil, err
			}
			session := tls.Server(client, r.config)
			if err := session.Handshake(); err != nil {
				return nil, nil, err
			}
			stream = session
			continue
		case binary.BigEndian.Uint32(head[0:4]) == 8 && code == pgGSSENCRequestCode:
			if _, err := stream.Write([]byte{'N'}); err != nil {
				return nil, nil, err
			}
			continue
		}
		// A startup or cancel message: it belongs to the database.
		if r.config != nil && stream == io.ReadWriter(client) {
			return nil, nil, errRelayPlaintext
		}
		return stream, head, nil
	}
	return nil, nil, errors.New("the client did not start the PostgreSQL protocol")
}

// Close stops the listener and every connection.
func (r *replicationRelay) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	conns := make([]net.Conn, 0, len(r.conns))
	for conn := range r.conns {
		conns = append(conns, conn)
	}
	r.mu.Unlock()
	r.listener.Close()
	for _, conn := range conns {
		conn.Close()
	}
	r.wg.Wait()
}
