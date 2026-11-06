package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// echoBackend is a database stand-in that echoes what it receives.
func echoBackend(t *testing.T) string {
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

func pgRequest(code uint32) []byte {
	head := make([]byte, 8)
	binary.BigEndian.PutUint32(head[0:4], 8)
	binary.BigEndian.PutUint32(head[4:8], code)
	return head
}

func pgStartup() []byte {
	body := []byte("user\x00app\x00database\x00app\x00\x00")
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(body)))
	binary.BigEndian.PutUint32(out[4:8], 196608)
	copy(out[8:], body)
	return out
}

func TestReplicationRelayTerminatesTLSOnThePublicPath(t *testing.T) {
	backend := echoBackend(t)
	relay, err := newReplicationRelay("127.0.0.1", 0, []string{"127.0.0.1"}, backend, true)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	if !relay.TLS() || !strings.Contains(relay.certPEM, "BEGIN CERTIFICATE") {
		t.Fatal("a public relay terminates TLS with its own certificate")
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(relay.Port()))
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(pgRequest(pgSSLRequestCode)); err != nil {
		t.Fatal(err)
	}
	answer := make([]byte, 1)
	if _, err := io.ReadFull(conn, answer); err != nil || answer[0] != 'S' {
		t.Fatalf("SSLRequest answered %q, %v", answer, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(relay.certPEM)) {
		t.Fatal("the CA PEM is not a certificate")
	}
	session := tls.Client(conn, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err := session.Handshake(); err != nil {
		t.Fatalf("the destination verifies the relay against the handed CA: %v", err)
	}
	startup := pgStartup()
	if _, err := session.Write(startup); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len(startup))
	if _, err := io.ReadFull(session, echo); err != nil || !bytes.Equal(echo, startup) {
		t.Fatalf("the startup message reaches the database in clear text behind the relay: %v", err)
	}

	// Clear text on the public path is refused.
	plain, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	plain.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := plain.Write(pgStartup()); err != nil {
		t.Fatal(err)
	}
	if n, err := plain.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatal("a clear-text client must be dropped on the public path")
	}
}

func TestReplicationRelayAcceptsOnlyThePeersAndForwardsPlainOnAPrivatePath(t *testing.T) {
	backend := echoBackend(t)
	relay, err := newReplicationRelay("127.0.0.1", 0, []string{"203.0.113.5", "10.20.0.0/16"}, backend, false)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(relay.Port()))
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write(pgStartup())
	if n, err := conn.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatal("a connection from an address outside the peer list is dropped")
	}
	conn.Close()
	if !relay.allowed(&net.TCPAddr{IP: net.ParseIP("10.20.7.9"), Port: 5}) || relay.allowed(&net.TCPAddr{IP: net.ParseIP("10.21.0.1"), Port: 5}) || !relay.allowed(&net.TCPAddr{IP: net.ParseIP("::ffff:203.0.113.5"), Port: 5}) {
		t.Fatal("peers are matched as addresses or networks, IPv4-mapped included")
	}

	// A peer on a private path: an SSLRequest is answered N and the clear
	// text continues to the database.
	open, err := newReplicationRelay("127.0.0.1", 0, []string{"127.0.0.1"}, backend, false)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	peer, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(open.Port())), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peer.SetDeadline(time.Now().Add(5 * time.Second))
	peer.Write(pgRequest(pgSSLRequestCode))
	answer := make([]byte, 1)
	if _, err := io.ReadFull(peer, answer); err != nil || answer[0] != 'N' {
		t.Fatalf("SSLRequest on a private relay answered %q, %v", answer, err)
	}
	startup := pgStartup()
	peer.Write(startup)
	echo := make([]byte, len(startup))
	if _, err := io.ReadFull(peer, echo); err != nil || !bytes.Equal(echo, startup) {
		t.Fatalf("clear text forwarded on the private path: %v", err)
	}
	if _, err := newReplicationRelay("127.0.0.1", 0, nil, backend, false); err == nil {
		t.Fatal("a relay without peers is refused")
	}
	if _, err := newReplicationRelay("127.0.0.1", 0, []string{"not-an-address"}, backend, false); err == nil {
		t.Fatal("peers must be addresses or networks")
	}
}

func TestReplicationReadinessParsesTheProbe(t *testing.T) {
	text := "version\t170004\nwal_level\treplica\nmax_slots\t10\nslots\t1\nmax_senders\t10\nsenders\t0\nnoidentity\tpublic.sessions\nnoidentity\tpublic.audit_log\n"
	r := parseReplicationReadiness(text)
	if r.ServerVersion != 17 || r.WALLevel != "replica" || !r.RestartNeeded || r.FreeSlots != 9 || r.FreeSenders != 10 || strings.Join(r.NoIdentity, ",") != "public.audit_log,public.sessions" || !r.Supported {
		t.Fatalf("%+v", r)
	}
	if r := parseReplicationReadiness("version\t180001\nwal_level\tlogical\nmax_slots\t10\nslots\t0\nmax_senders\t10\nsenders\t0\n"); r.RestartNeeded || r.ServerVersion != 18 || len(r.NoIdentity) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestReplicationSQLBuilders(t *testing.T) {
	verifier := "SCRAM-SHA-256$4096:c2FsdHNhbHRzYWx0c2FsdA==$x:y"
	script, err := renderMovePublicationSQL("lidza_move_abc", "lidza_move_abc", "app", verifier, []string{"public.orders", "public.sessions", "public.events"}, []string{"public.sessions"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`ALTER ROLE "lidza_move_abc" WITH LOGIN REPLICATION NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD 'SCRAM-SHA-256$4096`,
		`GRANT pg_read_all_data TO "lidza_move_abc"`,
		`GRANT CONNECT ON DATABASE "app" TO "lidza_move_abc"`,
		`CREATE PUBLICATION "lidza_move_abc" FOR TABLE "public"."orders", "public"."events"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("missing %q in\n%s", want, script)
		}
	}
	if strings.Contains(script, `"public"."sessions"`) || strings.Contains(script, "FOR ALL TABLES") {
		t.Fatalf("excluded tables stay out of the publication:\n%s", script)
	}
	all, _ := renderMovePublicationSQL("lidza_move_abc", "lidza_move_abc", "app", verifier, []string{"public.orders"}, nil)
	if !strings.Contains(all, "FOR ALL TABLES") {
		t.Fatalf("without exclusions the publication covers every table:\n%s", all)
	}
	if _, err := renderMovePublicationSQL("r", "p", "app", "plain-password", nil, nil); err == nil {
		t.Fatal("plaintext passwords never reach the server")
	}
	if _, err := renderMovePublicationSQL("r", "p", "app", verifier, []string{"public.a"}, []string{"public.a"}); err == nil {
		t.Fatal("a publication without tables is refused")
	}
	down, _ := renderMoveUnpublishSQL("lidza_move_abc", "lidza_move_abc", "lidza_move_abc")
	for _, want := range []string{"pg_terminate_backend(active_pid)", "pg_drop_replication_slot(slot_name)", `DROP PUBLICATION IF EXISTS "lidza_move_abc"`, `DROP OWNED BY "lidza_move_abc"`, `DROP ROLE IF EXISTS "lidza_move_abc"`} {
		if !strings.Contains(down, want) {
			t.Fatalf("missing %q in\n%s", want, down)
		}
	}
	sub, _ := renderMoveSubscriptionSQL("lidza_move_abc", "lidza_move_abc", "lidza_move_abc", "host=10.0.0.1 port=5433 passfile=/x/pgpass sslmode=disable")
	for _, want := range []string{"copy_data = true, streaming = on, two_phase = false, slot_name = ''lidza_move_abc'', create_slot = true", "ALTER SUBSCRIPTION \"lidza_move_abc\" CONNECTION", "ALTER SUBSCRIPTION \"lidza_move_abc\" ENABLE"} {
		if !strings.Contains(sub, want) {
			t.Fatalf("missing %q in\n%s", want, sub)
		}
	}
	unsub, _ := renderMoveUnsubscribeSQL("lidza_move_abc")
	if !strings.Contains(unsub, "SET (slot_name = NONE)") || !strings.Contains(unsub, `DROP SUBSCRIPTION IF EXISTS "lidza_move_abc"`) || strings.Index(unsub, "DISABLE") > strings.Index(unsub, "slot_name = NONE") {
		t.Fatalf("the subscription is disabled, detached from its slot and dropped, in that order:\n%s", unsub)
	}
	seq, err := renderMoveSequencesSQL([]MoveSequence{{Name: "public.orders_id_seq", Value: 42, Called: true}})
	if err != nil || !strings.Contains(seq, `setval('"public"."orders_id_seq"'::regclass, 42, true)`) {
		t.Fatalf("%v %s", err, seq)
	}
	if _, err := renderMoveSequencesSQL([]MoveSequence{{Name: "public.x; DROP", Value: 1}}); err == nil {
		t.Fatal("sequence names are validated")
	}
	conninfo := moveReplicationConninfo(MoveReplicationEndpoint{Host: "203.0.113.7", Port: 40123, Database: "app", User: "lidza_move_abc", SSLMode: "verify-ca"}, "/var/lib/postgresql/lidza-move/d/pgpass", "/var/lib/postgresql/lidza-move/d/ca.pem")
	if !strings.Contains(conninfo, "sslmode=verify-ca sslrootcert=/var/lib/postgresql/lidza-move/d/ca.pem") || strings.Contains(conninfo, "password=") {
		t.Fatal(conninfo)
	}
	if name, _ := moveReplicationName("Ab-12_cd"); name != "lidza_move_ab12cd" {
		t.Fatal(name)
	}
}

func TestReplicationStatusParsers(t *testing.T) {
	status := parseMoveSubscriptionStatus("lidza_move_x", "exists\t1\nenabled\tt\nrel:r\t11\nrel:d\t1\nreceived_lsn\t0/3000060\nlatest_end_lsn\t0/3000060\nworkers\t2\napply_errors\t0\nsync_errors\t1\n")
	if !status.Exists || !status.Enabled || status.Total != 12 || status.Ready != 11 || status.Tables["d"] != 1 || status.LatestEndLSN != "0/3000060" || status.Workers != 2 || status.SyncErrors != 1 {
		t.Fatalf("%+v", status)
	}
	pub := parseMoveReplicationStatus("current_lsn\t0/3000F00\nwal_level\tlogical\nslot\tt\t0/3000060\t3744\nsequence\tpublic.orders_id_seq\t42\tt\nsequence\tpublic.fresh_id_seq\t1\tf\n")
	if pub.CurrentLSN != "0/3000F00" || !pub.SlotExists || !pub.SlotActive || pub.ConfirmedLSN != "0/3000060" || pub.RetainedBytes != 3744 || len(pub.Sequences) != 2 || pub.Sequences[0].Value != 42 || !pub.Sequences[0].Called || pub.Sequences[1].Called {
		t.Fatalf("%+v", pub)
	}
	if pub := parseMoveReplicationStatus("current_lsn\t0/10\nwal_level\treplica\n"); pub.SlotExists {
		t.Fatal("no slot row means no slot")
	}
	log := "2026-11-06 10:00:01 UTC [1] LOG: ok\n2026-11-06 10:00:02 UTC [9] ERROR: logical replication target relation \"public.orders\" is missing replicated column: \"note\"\n2026-11-06 10:00:03 UTC [9] CONTEXT: processing remote data for replication origin\n"
	if got := lastSubscriptionError(log, "lidza_move_x"); got != `logical replication target relation "public.orders" is missing replicated column: "note"` {
		t.Fatal(got)
	}
	if lastSubscriptionError("nothing here\n", "lidza_move_x") != "" {
		t.Fatal("no error")
	}
}

// fakeReplicationSQL answers the replication scripts of a test database.
type fakeReplicationSQL struct {
	scripts []string
	answers map[string]string // substring of the script → output
	fail    string            // substring that fails
}

func (f *fakeReplicationSQL) run(_ context.Context, _ *Manager, target moveDatabaseTarget, script string) (string, error) {
	f.scripts = append(f.scripts, script)
	if f.fail != "" && strings.Contains(script, f.fail) {
		return "", errors.New("psql failed: simulated")
	}
	for key, answer := range f.answers {
		if strings.Contains(script, key) {
			return answer, nil
		}
	}
	return "", nil
}

func replicationTestManager(t *testing.T) (*Manager, *fakeReplicationSQL, string) {
	t.Helper()
	m := testManager(t, &fakeRuntime{})
	m.mu.Lock()
	m.data.Databases["shopdb"] = Database{AppID: "shopdb", Mode: "local", Network: "lidza-db-shopdb", Ready: true, URL: "postgres://app:pw@lidza-db-shopdb:5432/app", AdminPassword: "admin"}
	m.mu.Unlock()
	backend := echoBackend(t)
	fake := &fakeReplicationSQL{answers: map[string]string{
		"relkind IN ('r', 'p')":                         "public.orders\npublic.sessions\n",
		"pg_current_wal_lsn()::text":                    "current_lsn\t0/5000000\nwal_level\tlogical\nslot\tt\t0/4FFFF00\t256\n",
		"server_version_num":                            "version\t170004\nwal_level\tlogical\nmax_slots\t10\nslots\t0\nmax_senders\t10\nsenders\t0\n",
		"FROM pg_catalog.pg_subscription WHERE subname": "exists\t1\nenabled\tt\nrel:r\t2\nworkers\t1\napply_errors\t0\nsync_errors\t0\n",
	}}
	previousSQL, previousContainer, previousFiles, previousRemove, previousProbe := moveReplicationSQL, moveContainerDatabase, moveSubscriptionFiles, moveSubscriptionFilesRemove, moveSubscriptionProbe
	previousReach := moveReachTCP
	moveReachTCP = func(_ context.Context, host string, port int, timeout time.Duration) error {
		if host == "192.0.2.9" {
			return moveDialError(net.JoinHostPort(host, strconv.Itoa(port)), timeout, context.DeadlineExceeded)
		}
		return nil
	}
	moveReplicationSQL = fake.run
	moveContainerDatabase = func(context.Context, moveDatabaseTarget) (string, string, error) { return "app", backend, nil }
	moveSubscriptionFiles = func(_ context.Context, _ *Manager, target moveDatabaseTarget, passfile, ca []byte) (string, string, error) {
		if !strings.HasPrefix(string(passfile), "*:*:app:lidza_move_") {
			t.Errorf("passfile %q", passfile)
		}
		if len(ca) > 0 {
			return "/data/lidza-move/pgpass", "/data/lidza-move/ca.pem", nil
		}
		return "/data/lidza-move/pgpass", "", nil
	}
	moveSubscriptionFilesRemove = func(context.Context, *Manager, moveDatabaseTarget) {}
	moveSubscriptionProbe = func(_ context.Context, _ *Manager, _ moveDatabaseTarget, conninfo string) error {
		if strings.Contains(conninfo, "host=192.0.2.1") {
			return errors.New("the destination cannot reach the source's replication endpoint: connection timed out")
		}
		return nil
	}
	t.Cleanup(func() {
		moveReplicationSQL, moveContainerDatabase, moveSubscriptionFiles, moveSubscriptionFilesRemove, moveSubscriptionProbe = previousSQL, previousContainer, previousFiles, previousRemove, previousProbe
		moveReachTCP = previousReach
		relays := m.moveRelays()
		relays.mu.Lock()
		for _, relay := range relays.relays {
			relay.Close()
		}
		relays.mu.Unlock()
	})
	return m, fake, backend
}

func TestMoveReplicationPublishesRelaysAndTearsDown(t *testing.T) {
	m, fake, _ := replicationTestManager(t)
	h := Handler(m)
	body, _ := json.Marshal(MoveReplicationRequest{Move: "m0123456789abcdef", Listen: "127.0.0.1", Peers: []string{"127.0.0.1"}, TLS: true, Exclude: []string{"public.sessions"}})
	w := moveRequest(t, h, "POST", "/v1/move/databases/shopdb/replication", bytes.NewReader(body), nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var endpoint MoveReplicationEndpoint
	json.Unmarshal(w.Body.Bytes(), &endpoint)
	if endpoint.Host != "127.0.0.1" || endpoint.Port == 0 || endpoint.User != "lidza_move_m0123456789abcdef" || endpoint.Database != "app" || endpoint.SSLMode != "verify-ca" || !strings.Contains(endpoint.CA, "BEGIN CERTIFICATE") || len(endpoint.Password) != 64 || endpoint.Tables != 1 || endpoint.Firewall != "unmanaged" {
		t.Fatalf("%+v", endpoint)
	}
	if !strings.Contains(fake.scripts[len(fake.scripts)-1], `FOR TABLE "public"."orders"`) {
		t.Fatalf("the publication leaves the excluded table out: %s", fake.scripts[len(fake.scripts)-1])
	}
	record, found := m.moveReplicationRecord("shopdb")
	if !found || record.Move != "m0123456789abcdef" || record.Port != endpoint.Port || !record.TLS || record.Expires.Before(time.Now().Add(6*24*time.Hour)) {
		t.Fatalf("record %+v", record)
	}
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(endpoint.Port)), time.Second); err != nil {
		t.Fatal("the relay listens:", err)
	} else {
		conn.Close()
	}
	// A second request rotates the password and keeps the relay's port.
	w = moveRequest(t, h, "POST", "/v1/move/databases/shopdb/replication", bytes.NewReader(body), nil)
	var again MoveReplicationEndpoint
	json.Unmarshal(w.Body.Bytes(), &again)
	if w.Code != 200 || again.Port != endpoint.Port || again.Password == endpoint.Password {
		t.Fatalf("status %d: %+v", w.Code, again)
	}
	// Another move cannot take the database over.
	other, _ := json.Marshal(MoveReplicationRequest{Move: "other", Listen: "127.0.0.1", Peers: []string{"127.0.0.1"}})
	if w := moveRequest(t, h, "POST", "/v1/move/databases/shopdb/replication", bytes.NewReader(other), nil); w.Code != 409 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	w = moveRequest(t, h, "GET", "/v1/move/databases/shopdb/replication?sequences=1", nil, nil)
	var status MoveReplicationStatus
	json.Unmarshal(w.Body.Bytes(), &status)
	if w.Code != 200 || status.CurrentLSN != "0/5000000" || !status.SlotActive || !status.Relay {
		t.Fatalf("status %d: %+v", w.Code, status)
	}
	if w := moveRequest(t, h, "DELETE", "/v1/move/databases/shopdb/replication", nil, nil); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if _, found := m.moveReplicationRecord("shopdb"); found {
		t.Fatal("the record goes with the teardown")
	}
	if !strings.Contains(fake.scripts[len(fake.scripts)-1], "pg_drop_replication_slot") {
		t.Fatal("the teardown drops the slot")
	}
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(endpoint.Port)), 300*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("the relay stops listening")
	}
	if w := moveRequest(t, h, "DELETE", "/v1/move/databases/shopdb/replication", nil, nil); w.Code != 200 {
		t.Fatal("teardown is idempotent")
	}
}

func TestMoveReplicationSweepDropsExpiredPublications(t *testing.T) {
	m, fake, _ := replicationTestManager(t)
	body, _ := json.Marshal(MoveReplicationRequest{Move: "m1", Listen: "127.0.0.1", Peers: []string{"127.0.0.1"}})
	if w := moveRequest(t, Handler(m), "POST", "/v1/move/databases/shopdb/replication", bytes.NewReader(body), nil); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	m.moveReplicationSweep(time.Now())
	if _, found := m.moveReplicationRecord("shopdb"); !found {
		t.Fatal("a live move keeps its publication")
	}
	m.moveReplicationSweep(time.Now().Add(moveReplicationLifetime + time.Hour))
	if _, found := m.moveReplicationRecord("shopdb"); found {
		t.Fatal("an expired publication is torn down")
	}
	if !strings.Contains(fake.scripts[len(fake.scripts)-1], "pg_drop_replication_slot") {
		t.Fatal("the sweep drops the slot")
	}
}

func TestMoveSubscriptionCreatesRepointsAndDrops(t *testing.T) {
	m, fake, _ := replicationTestManager(t)
	h := Handler(m)
	endpoint := MoveReplicationEndpoint{Host: "10.0.0.2", Port: 40000, Database: "app", User: "lidza_move_m1", Password: strings.Repeat("p", 64), SSLMode: "disable", Publication: "lidza_move_m1", Slot: "lidza_move_m1"}
	body, _ := json.Marshal(MoveSubscriptionRequest{Move: "m1", Endpoint: endpoint})
	w := moveRequest(t, h, "POST", "/v1/move/databases/shopdb/subscription", bytes.NewReader(body), nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var status MoveSubscriptionStatus
	json.Unmarshal(w.Body.Bytes(), &status)
	if !status.Exists || status.Ready != 2 || status.Total != 2 || status.Name != "lidza_move_m1" {
		t.Fatalf("%+v", status)
	}
	script := fake.scripts[len(fake.scripts)-2]
	if !strings.Contains(script, "passfile=/data/lidza-move/pgpass") || !strings.Contains(script, "sslmode=disable") || strings.Contains(script, strings.Repeat("p", 64)) {
		t.Fatalf("the password stays in the passfile:\n%s", script)
	}
	if w := moveRequest(t, h, "GET", "/v1/move/databases/shopdb/subscription?move=m1", nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"ready":2`) {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	// A verify-ca endpoint needs its CA; an unreachable one is a clear error.
	tlsEndpoint := endpoint
	tlsEndpoint.SSLMode = "verify-ca"
	body, _ = json.Marshal(MoveSubscriptionRequest{Move: "m1", Endpoint: tlsEndpoint})
	if w := moveRequest(t, h, "POST", "/v1/move/databases/shopdb/subscription", bytes.NewReader(body), nil); w.Code != 409 || !strings.Contains(w.Body.String(), "needs its CA") {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	unreachable := endpoint
	unreachable.Host = "192.0.2.1"
	body, _ = json.Marshal(MoveSubscriptionRequest{Move: "m1", Endpoint: unreachable})
	if w := moveRequest(t, h, "POST", "/v1/move/databases/shopdb/subscription", bytes.NewReader(body), nil); w.Code != 409 || !strings.Contains(w.Body.String(), "cannot reach the source's replication endpoint") {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	// The dial preflight names the address and the network error.
	unreachable.Host, unreachable.Port = "192.0.2.9", 41237
	body, _ = json.Marshal(MoveSubscriptionRequest{Move: "m1", Endpoint: unreachable})
	if w := moveRequest(t, h, "POST", "/v1/move/databases/shopdb/subscription", bytes.NewReader(body), nil); w.Code != 409 || !strings.Contains(w.Body.String(), "cannot reach the source's replication endpoint: connection to 192.0.2.9:41237 timed out after 5s") {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	seq, _ := json.Marshal(MoveSequenceRequest{Sequences: []MoveSequence{{Name: "public.orders_id_seq", Value: 77, Called: true}}})
	if w := moveRequest(t, h, "POST", "/v1/move/databases/shopdb/subscription/sequences", bytes.NewReader(seq), nil); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(fake.scripts[len(fake.scripts)-1], "setval") {
		t.Fatal("sequences are set")
	}
	if w := moveRequest(t, h, "DELETE", "/v1/move/databases/shopdb/subscription?move=m1", nil, nil); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(fake.scripts[len(fake.scripts)-1], "DROP SUBSCRIPTION IF EXISTS") {
		t.Fatal("the subscription is dropped")
	}
	for _, route := range []string{"/v1/move/databases/shopdb/replication", "/v1/move/databases/shopdb/subscription"} {
		if w := moveRequest(t, h, "POST", route, bytes.NewReader([]byte("{}")), nil); w.Code != 400 && w.Code != 409 {
			t.Fatalf("%s with an empty body: %d", route, w.Code)
		}
	}
}

func TestMoveSourceReportsReplicationReadinessAndHostPrivateAddress(t *testing.T) {
	m, _, _ := replicationTestManager(t)
	previous := moveTableCount
	moveTableCount = func(context.Context, *Manager, string) (int, error) { return 2, nil }
	t.Cleanup(func() { moveTableCount = previous })
	a := testApp("source")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	stored := m.data.Apps["source"]
	stored.Current = &Release{Commit: "0123456789abcdef0123456789abcdef01234567"}
	stored.Bindings = map[string]string{"DATABASE_URL": "shopdb"}
	stored.Env["DATABASE_URL"] = "postgres://app:pw@lidza-db-shopdb:5432/app"
	m.data.Apps["source"] = stored
	m.mu.Unlock()
	w := moveRequest(t, Handler(m), "GET", "/v1/move/apps/source", nil, nil)
	var source MoveSource
	if err := json.Unmarshal(w.Body.Bytes(), &source); err != nil || w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if len(source.Databases) != 1 || source.Databases[0].Replication == nil || !source.Databases[0].Replication.Supported || source.Databases[0].Replication.WALLevel != "logical" || source.Databases[0].Replication.RestartNeeded {
		t.Fatalf("%+v", source.Databases)
	}
	previousDetect := moveDetectAddresses
	moveDetectAddresses = func() NetworkInfo {
		return NetworkInfo{IPv4: "203.0.113.7", PrivateIPv4: "10.40.0.3", PrivateNetwork: "10.40.0.0/20"}
	}
	t.Cleanup(func() { moveDetectAddresses = previousDetect })
	w = moveRequest(t, Handler(m), "GET", "/v1/move/host", nil, nil)
	var host MoveHost
	json.Unmarshal(w.Body.Bytes(), &host)
	if host.PrivateIPv4 != "10.40.0.3" || host.PrivateNetwork != "10.40.0.0/20" || !host.Subscribe || host.IPv4 != "203.0.113.7" {
		t.Fatalf("%+v", host)
	}
	previousRestart := moveReplicationRestart
	restarted := 0
	moveReplicationRestart = func(context.Context, *Manager, moveDatabaseTarget) error { restarted++; return nil }
	t.Cleanup(func() { moveReplicationRestart = previousRestart })
	w = moveRequest(t, Handler(m), "POST", "/v1/move/databases/shopdb/replication/restart", nil, nil)
	if w.Code != 200 || restarted != 1 || !strings.Contains(w.Body.String(), `"wal_level":"logical"`) {
		t.Fatalf("restart route: %d %s", w.Code, w.Body)
	}
}
