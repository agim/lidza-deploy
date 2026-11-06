package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestMoveReplicationDecisionNamesEveryFallback(t *testing.T) {
	ready := &agent.MoveReplicationReadiness{Supported: true, ServerVersion: 17, WALLevel: "logical", FreeSlots: 10, FreeSenders: 10}
	source := agent.MoveHost{IPv4: moveTestSourceIP, PrivateIPv4: "10.40.0.2", PrivateNetwork: "10.40.0.0/20", Tunnel: true}
	target := agent.MoveHost{IPv4: moveTestTargetIP, IPv6: moveTestTargetIP6, PrivateIPv4: "10.40.0.3", PrivateNetwork: "10.40.0.0/20", Subscribe: true, Tunnel: true}
	db := func(r *agent.MoveReplicationReadiness, tables int) agent.MoveSourceDatabase {
		return agent.MoveSourceDatabase{ID: "shop", Copyable: true, Tables: tables, Replication: r}
	}
	cases := []struct {
		name    string
		d       agent.MoveSourceDatabase
		target  agent.MoveHost
		option  string
		mode    string
		reason  string
		restart bool
	}{
		{"continuous", db(ready, 12), target, moveReplicationAuto, moveReplicationContinuous, "", false},
		{"forced dump", db(ready, 12), target, moveReplicationDump, moveReplicationDump, "dump and restore chosen for this move", false},
		{"old source agent", db(nil, 12), target, "", moveReplicationDump, "the source agent does not report logical replication; upgrade it", false},
		{"old destination agent", db(ready, 12), agent.MoveHost{IPv4: moveTestTargetIP}, "", moveReplicationDump, "the destination agent cannot subscribe; upgrade it", false},
		{"source reason", db(&agent.MoveReplicationReadiness{Supported: true, Reason: "PostgreSQL 13 is too old (14 or newer)"}, 12), target, "", moveReplicationDump, "PostgreSQL 13 is too old (14 or newer)", false},
		{"no identity anywhere", db(&agent.MoveReplicationReadiness{Supported: true, WALLevel: "logical", FreeSlots: 10, FreeSenders: 10, NoIdentity: []string{"public.a", "public.b"}}, 2), target, "", moveReplicationDump, "no table has a primary key or replica identity", false},
		{"slots", db(&agent.MoveReplicationReadiness{Supported: true, WALLevel: "logical", FreeSlots: 1, FreeSenders: 10}, 12), target, "", moveReplicationDump, "only 1 free replication slots on the source (3 needed; raise max_replication_slots)", false},
		{"senders", db(&agent.MoveReplicationReadiness{Supported: true, WALLevel: "logical", FreeSlots: 10, FreeSenders: 2}, 12), target, "", moveReplicationDump, "only 2 free WAL senders on the source (3 needed; raise max_wal_senders)", false},
		{"restart", db(&agent.MoveReplicationReadiness{Supported: true, WALLevel: "replica", RestartNeeded: true, FreeSlots: 10, FreeSenders: 10}, 12), target, "", moveReplicationContinuous, "", true},
	}
	for _, tc := range cases {
		mode, reason, restart := moveReplicationDecision(tc.d, source, tc.target, tc.option, "")
		if mode != tc.mode || reason != tc.reason || restart != tc.restart {
			t.Errorf("%s: %s %q %t", tc.name, mode, reason, restart)
		}
	}
	// Agents that cannot tunnel and share no network; the public relay
	// requested without a public address.
	if mode, reason, _ := moveReplicationDecision(db(ready, 12), agent.MoveHost{}, target, "", ""); mode != moveReplicationDump || reason != "the agents cannot replicate through the control panel and share no private network; upgrade them" {
		t.Fatalf("%s %q", mode, reason)
	}
	if mode, reason, _ := moveReplicationDecision(db(ready, 12), agent.MoveHost{Tunnel: true}, target, "", moveReplicationPathPublic); mode != moveReplicationDump || !strings.HasPrefix(reason, "no public path") {
		t.Fatalf("%s %q", mode, reason)
	}
	// Different networks: through the panel.
	if mode, reason, _ := moveReplicationDecision(db(ready, 12), agent.MoveHost{Tunnel: true}, target, "", ""); mode != moveReplicationContinuous || reason != "" {
		t.Fatalf("%s %q", mode, reason)
	}
}

func TestMoveReplicationCaughtUpComparesLSNs(t *testing.T) {
	if n, ok := parsePGLSN("16/B374D848"); !ok || n != 0x16B374D848 {
		t.Fatalf("%x %t", n, ok)
	}
	if _, ok := parsePGLSN("nope"); ok {
		t.Fatal("garbage parses")
	}
	publisher := agent.MoveReplicationStatus{CurrentLSN: "0/5000100"}
	cases := []struct {
		sub    agent.MoveSubscriptionStatus
		done   bool
		detail string
	}{
		{agent.MoveSubscriptionStatus{}, false, "the subscription does not exist on the destination"},
		{agent.MoveSubscriptionStatus{Exists: true}, false, "the subscription has no tables yet"},
		{agent.MoveSubscriptionStatus{Exists: true, Total: 4, Ready: 3}, false, "3 of 4 tables synchronised"},
		{agent.MoveSubscriptionStatus{Exists: true, Total: 4, Ready: 4}, false, "the destination has not applied anything yet"},
		{agent.MoveSubscriptionStatus{Exists: true, Total: 4, Ready: 4, LatestEndLSN: "0/5000000"}, false, "0.2 KiB behind the source (0/5000000 applied of 0/5000100)"},
		{agent.MoveSubscriptionStatus{Exists: true, Total: 4, Ready: 4, LatestEndLSN: "0/5000100"}, true, ""},
		{agent.MoveSubscriptionStatus{Exists: true, Total: 4, Ready: 4, LatestEndLSN: "1/0"}, true, ""},
	}
	for _, tc := range cases {
		if done, detail := moveReplicationCaughtUp(publisher, tc.sub); done != tc.done || detail != tc.detail {
			t.Errorf("%+v: %t %q", tc.sub, done, detail)
		}
	}
	if broken := moveReplicationBroken(agent.MoveSubscriptionStatus{Exists: true, Enabled: true, ApplyErrors: 2, LastError: "column missing"}, 0); broken != "the subscription reports errors: column missing" {
		t.Fatal(broken)
	}
	if broken := moveReplicationBroken(agent.MoveSubscriptionStatus{Exists: true, Enabled: true, Workers: 0}, 5); broken != "" {
		t.Fatal("a short idle is not broken")
	}
	if broken := moveReplicationBroken(agent.MoveSubscriptionStatus{Exists: true, Enabled: true, Workers: 0}, 30); !strings.HasPrefix(broken, "no replication worker runs") {
		t.Fatal(broken)
	}
	d := MoveDatabase{ReplicationExcluded: []string{"public.sessions", "public.audit_log"}, Excluded: []MoveExcludedTable{{Name: "public.audit_log", Choice: moveTableLeave}, {Name: "public.events", Choice: moveTableCopy}}}
	if !slices.Equal(moveReplicationExclude(d), []string{"public.audit_log", "public.sessions"}) || !slices.Equal(moveReplicationDumpTables(d), []string{"public.sessions"}) {
		t.Fatalf("%v %v", moveReplicationExclude(d), moveReplicationDumpTables(d))
	}
}

// moveReplicationFake is the replication state of both fake agents.
type moveReplicationFake struct {
	mu                 sync.Mutex
	publishes          []agent.MoveReplicationRequest
	unpublished        int
	subscribes         []agent.MoveSubscriptionRequest
	subscribed         bool
	unsubscribed       int
	total, ready       int
	sourceLSN, applied string
	applyErrors        int64
	lastError          string
	unreachablePrivate bool
	sequences          []agent.MoveSequence
	set                []agent.MoveSequence
	restarts           int
	tunnel             *moveTunnelFake
}

func (f *moveReplicationFake) with(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// replicationSource makes the container source's database replicable over
// a private network both hosts share, with both agents' replication routes.
func (fx *moveFixture) replicationSource() *moveReplicationFake {
	fx.containerSource()
	fake := &moveReplicationFake{total: 3, ready: 3, sourceLSN: "0/5000000", applied: "0/5000000", sequences: []agent.MoveSequence{{Name: "public.orders_id_seq", Value: 120, Called: true}}}
	fx.a.source.Databases[0].Replication = &agent.MoveReplicationReadiness{Supported: true, ServerVersion: 17, WALLevel: "logical", FreeSlots: 10, FreeSenders: 10}
	fx.a.host = agent.MoveHost{IPv4: moveTestSourceIP, PrivateIPv4: "10.40.0.2", PrivateNetwork: "10.40.0.0/20"}
	fx.b.host.Subscribe, fx.b.host.TableData, fx.b.host.PrivateIPv4, fx.b.host.PrivateNetwork = true, true, "10.40.0.3", "10.40.0.0/20"
	fake.tunnel = fx.tunnels(fx.a.t)
	fake.tunnel.unreachable = func(in agent.MoveReachRequest) string {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.unreachablePrivate {
			return "connection to " + in.Host + ":" + strconv.Itoa(in.Port) + " timed out after 5s"
		}
		return ""
	}
	schema := []byte("PGDMP schema-only archive")
	sum := sha256.Sum256(schema)
	digest := hex.EncodeToString(sum[:])
	export := strings.Repeat("5", 64)
	fx.a.mux.HandleFunc("POST /v1/move/databases/{id}/schema", func(w http.ResponseWriter, r *http.Request) {
		agent.JSON(w, 200, agent.MoveTableDataExport{ID: export, Size: int64(len(schema)), SHA256: digest, Tables: []string{}, Rows: map[string]int64{}})
	})
	fx.a.mux.HandleFunc("GET /v1/move/databases/{id}/table-data/{export}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(agent.MoveSHA256Header, digest)
		w.Header().Set("Content-Length", strconv.Itoa(len(schema)))
		w.Write(schema)
	})
	fx.a.mux.HandleFunc("POST /v1/move/databases/{id}/replication", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveReplicationRequest
		json.NewDecoder(r.Body).Decode(&in)
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.publishes = append(fake.publishes, in)
		endpoint := agent.MoveReplicationEndpoint{Host: in.Listen, Port: 45000 + len(fake.publishes), Database: "app", User: "lidza_move_" + in.Move, Password: "secret-" + strconv.Itoa(len(fake.publishes)), SSLMode: "disable", Publication: "lidza_move_" + in.Move, Slot: "lidza_move_" + in.Move, Tables: 3 - len(in.Exclude), Firewall: "opened"}
		if in.TLS {
			endpoint.SSLMode, endpoint.CA = "verify-ca", "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
		}
		agent.JSON(w, 200, endpoint)
	})
	fx.a.mux.HandleFunc("GET /v1/move/databases/{id}/replication", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		status := agent.MoveReplicationStatus{CurrentLSN: fake.sourceLSN, SlotExists: fake.subscribed, SlotActive: fake.subscribed, WALLevel: "logical", Relay: len(fake.publishes) > fake.unpublished}
		if r.URL.Query().Get("sequences") == "1" {
			status.Sequences = fake.sequences
		}
		agent.JSON(w, 200, status)
	})
	fx.a.mux.HandleFunc("DELETE /v1/move/databases/{id}/replication", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.unpublished++
		fake.mu.Unlock()
		agent.JSON(w, 200, map[string]string{"status": "removed"})
	})
	fx.a.mux.HandleFunc("POST /v1/move/databases/{id}/replication/restart", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.restarts++
		fake.mu.Unlock()
		agent.JSON(w, 200, agent.MoveReplicationReadiness{Supported: true, ServerVersion: 17, WALLevel: "logical", FreeSlots: 10, FreeSenders: 10})
	})
	fx.a.mux.HandleFunc("DELETE /v1/move/databases/{id}/table-data", func(w http.ResponseWriter, r *http.Request) {
		agent.JSON(w, 200, map[string]string{"status": "removed"})
	})
	fx.b.mux.HandleFunc("POST /v1/move/databases/{id}/subscription", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveSubscriptionRequest
		json.NewDecoder(r.Body).Decode(&in)
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.unreachablePrivate && strings.HasPrefix(in.Endpoint.Host, "10.") {
			agent.JSON(w, 409, map[string]string{"error": "the destination cannot reach the source's replication endpoint: connection timed out"})
			return
		}
		if in.Endpoint.SSLMode == "verify-ca" && !strings.Contains(in.Endpoint.CA, "BEGIN CERTIFICATE") {
			agent.JSON(w, 409, map[string]string{"error": "a verify-ca endpoint needs its CA"})
			return
		}
		if in.Endpoint.Host == "127.0.0.1" {
			// The panel path: connect through the tunnel like PostgreSQL's
			// probe would.
			if err := moveTunnelEcho(net.JoinHostPort(in.Endpoint.Host, strconv.Itoa(in.Endpoint.Port)), 64); err != nil {
				agent.JSON(w, 409, map[string]string{"error": "the destination cannot reach the source's replication endpoint: " + err.Error()})
				return
			}
		}
		fake.subscribes = append(fake.subscribes, in)
		fake.subscribed = true
		agent.JSON(w, 200, agent.MoveSubscriptionStatus{Name: "lidza_move_" + in.Move, Exists: true, Enabled: true, Total: fake.total, Ready: fake.ready, Workers: 1})
	})
	fx.b.mux.HandleFunc("GET /v1/move/databases/{id}/subscription", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		status := agent.MoveSubscriptionStatus{Name: "lidza_move_" + r.URL.Query().Get("move"), Exists: fake.subscribed, Enabled: fake.subscribed, Tables: map[string]int{"r": fake.ready, "d": fake.total - fake.ready}, Total: fake.total, Ready: fake.ready, LatestEndLSN: fake.applied, ReceivedLSN: fake.applied, Workers: 1, ApplyErrors: fake.applyErrors, LastError: fake.lastError}
		if !fake.subscribed {
			status = agent.MoveSubscriptionStatus{Name: status.Name, Tables: map[string]int{}}
		}
		// The destination catches up one step per poll.
		if fake.applied != fake.sourceLSN {
			fake.applied = fake.sourceLSN
		}
		agent.JSON(w, 200, status)
	})
	fx.b.mux.HandleFunc("DELETE /v1/move/databases/{id}/subscription", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.subscribed = false
		fake.unsubscribed++
		fake.mu.Unlock()
		fake.tunnel.close()
		agent.JSON(w, 200, map[string]string{"status": "removed"})
	})
	fx.b.mux.HandleFunc("POST /v1/move/databases/{id}/subscription/sequences", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveSequenceRequest
		json.NewDecoder(r.Body).Decode(&in)
		fake.mu.Lock()
		fake.set = append(fake.set, in.Sequences...)
		fake.mu.Unlock()
		agent.JSON(w, 200, map[string]int{"sequences": len(in.Sequences)})
	})
	return fake
}

func TestMovePlanShowsTheReplicationModePerDatabase(t *testing.T) {
	fx := newMoveFixture(t)
	fx.replicationSource()
	plan, _, err := fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	db := plan.Databases[0]
	if db.Replication != moveReplicationContinuous || db.ReplicationReason != "" || db.ReplicationRestart || plan.Replication != moveReplicationAuto {
		t.Fatalf("%+v %q", db, plan.Replication)
	}
	wantNote := "Database shop: continuous replication over the private network 10.40.0.0/20. The source's host firewall admits the destination's address to the relay port for the move only; if the destination cannot reach it there, the move replicates through the control panel instead."
	if !slices.Contains(plan.Notes, wantNote) || db.Note != "over the private network 10.40.0.0/20" || db.ReplicationPath != moveReplicationPathPrivate {
		t.Fatalf("notes %q, %+v", plan.Notes, db)
	}
	// Hosts on different networks replicate through the panel.
	fx.b.with(func() { fx.b.host.PrivateIPv4, fx.b.host.PrivateNetwork = "10.20.0.3", "10.20.0.0/16" })
	plan, _, _ = fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta"})
	if db := plan.Databases[0]; db.Replication != moveReplicationContinuous || db.Note != "through the control panel" || db.ReplicationVia != "through the control panel" || !slices.Contains(plan.Notes, "Database shop: continuous replication through the control panel. The destination's PostgreSQL connects to a local port of the agent on Beta, and the control panel carries each connection over its own channels to both agents; no firewall port opens on either host.") {
		t.Fatalf("%+v %q", db, plan.Notes)
	}
	if _, status, _ := fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta", ReplicationNetwork: "carrier-pigeon"}); status != 400 {
		t.Fatal("the network option is validated")
	}
	fx.b.with(func() { fx.b.host.PrivateIPv4, fx.b.host.PrivateNetwork = "10.40.0.3", "10.40.0.0/20" })
	if !slices.ContainsFunc(plan.Warnings, func(w string) bool { return strings.HasPrefix(w, "Schema changes on the source") }) {
		t.Fatalf("warnings %q", plan.Warnings)
	}
	if !slices.ContainsFunc(plan.Notes, func(n string) bool { return strings.Contains(n, "keeps the WAL the replication slot has not sent") }) {
		t.Fatalf("notes %q", plan.Notes)
	}
	if !slices.Contains(plan.Notes, "Replicated databases follow the source continuously; dumped databases and files are copied again by the final sync during the cutover, with the source in maintenance.") {
		t.Fatalf("notes %q", plan.Notes)
	}
	var pg *MoveEngineEstimate
	for i := range plan.Estimate.Engines {
		if plan.Estimate.Engines[i].Kind == moveEnginePostgres {
			pg = &plan.Estimate.Engines[i]
		}
	}
	if pg == nil || pg.FinalBasis != moveReplicationFinalBasis || pg.FinalSeconds != moveEngineOverhead || !slices.Contains(plan.Estimate.Notes, "Final sync of shop: seconds (replication lag); the destination follows the source continuously.") {
		t.Fatalf("estimate %+v %q", pg, plan.Estimate.Notes)
	}

	// A source below wal_level=logical: a restart to acknowledge.
	fx.a.with(func() {
		fx.a.source.Databases[0].Replication.WALLevel, fx.a.source.Databases[0].Replication.RestartNeeded = "replica", true
		fx.a.source.Databases[0].Replication.NoIdentity = []string{"public.sessions"}
	})
	plan, _, _ = fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta"})
	if !plan.Databases[0].ReplicationRestart || !slices.ContainsFunc(plan.Warnings, func(w string) bool {
		return strings.HasPrefix(w, "Prepare restarts the PostgreSQL database shop on Alpha once to set wal_level=logical")
	}) {
		t.Fatalf("%+v %q", plan.Databases[0], plan.Warnings)
	}
	if !slices.ContainsFunc(plan.Notes, func(n string) bool {
		return n == "Tables of shop without a primary key or replica identity (public.sessions) cannot replicate updates and deletes; they are copied with a data dump at the bulk copy and again at the final sync."
	}) {
		t.Fatalf("notes %q", plan.Notes)
	}

	// The operator forces the dump path.
	plan, _, _ = fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta", Replication: moveReplicationDump})
	if plan.Databases[0].Replication != moveReplicationDump || plan.Databases[0].ReplicationReason != "dump and restore chosen for this move" || plan.Replication != moveReplicationDump {
		t.Fatalf("%+v", plan.Databases[0])
	}
	if slices.ContainsFunc(plan.Warnings, func(w string) bool {
		return strings.HasPrefix(w, "Schema changes") || strings.HasPrefix(w, "Prepare restarts")
	}) {
		t.Fatalf("the dump path carries no replication warnings: %q", plan.Warnings)
	}
	if _, status, err := fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta", Replication: "sometimes"}); status != 400 || err == nil {
		t.Fatal("the option is validated")
	}

	// A source agent without readiness falls back with the reason.
	fx.a.with(func() { fx.a.source.Databases[0].Replication = nil })
	plan, _, _ = fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta"})
	if plan.Databases[0].Replication != moveReplicationDump || plan.Databases[0].ReplicationReason != "the source agent does not report logical replication; upgrade it" {
		t.Fatalf("%+v", plan.Databases[0])
	}
	if !slices.Contains(plan.Notes, "Writes made on the source after the data copy are not on the destination. Use the final sync during the cutover to copy them with the source in maintenance.") {
		t.Fatalf("notes %q", plan.Notes)
	}
}

func TestMoveReplicatesTheDatabaseAndCatchesUpAtTheFinalSync(t *testing.T) {
	fx := newMoveFixture(t)
	rep := fx.replicationSource()
	fx.a.with(func() {
		fx.a.source.Databases[0].Replication.WALLevel, fx.a.source.Databases[0].Replication.RestartNeeded = "replica", true
	})
	// The manual cutover: the automatic one is move_auto_test.go's.
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true, "auto_cutover": false})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	d := job.Databases[0]
	if d.Replication != moveReplicationContinuous || d.Copied != "replication" || d.Link == nil || d.Link.Path != "private" || d.Link.Host != "10.40.0.2" || !d.Link.Subscribed || !d.Link.Published || d.Link.TLS || d.ReplicationRestart {
		t.Fatalf("%+v %+v", d, d.Link)
	}
	rep.with(func() {
		if rep.restarts != 1 {
			t.Fatalf("restarts %d", rep.restarts)
		}
		if len(rep.publishes) != 1 || rep.publishes[0].Listen != "10.40.0.2" || !slices.Equal(rep.publishes[0].Peers, []string{"10.40.0.3"}) || rep.publishes[0].TLS || rep.publishes[0].Move != view.ID {
			t.Fatalf("publishes %+v", rep.publishes)
		}
		if len(rep.subscribes) != 1 || rep.subscribes[0].Endpoint.Host != "10.40.0.2" || rep.subscribes[0].Endpoint.SSLMode != "disable" || rep.subscribes[0].Endpoint.Password != "secret-1" {
			t.Fatalf("subscribes %+v", rep.subscribes)
		}
		if rep.unpublished != 0 || rep.unsubscribed != 0 {
			t.Fatal("the bulk phase keeps the replication")
		}
	})
	fx.b.with(func() {
		if len(fx.b.restores["shop-db0001"])+len(fx.b.restores[job.Databases[0].TargetID]) != 1 || string(fx.b.restores[job.Databases[0].TargetID][0]) != "PGDMP schema-only archive" {
			t.Fatalf("restores %v", fx.b.restores)
		}
	})
	for _, want := range []string{
		"Database shop restarted with wal_level=logical.",
	} {
		if !moveLogged(job, moveStepPrepare, want) {
			t.Fatalf("%q not logged: %+v", want, job.step(moveStepPrepare).Log)
		}
	}
	for _, want := range []string{
		"Dumping the schema of shop on alpha (tables, sequences, extensions; no rows).",
		"Replication of shop connects over the private network 10.40.0.0/20.",
		"Published shop on alpha as lidza_move_" + view.ID + " (3 tables); the relay listens on 10.40.0.2:45001 for 10.40.0.3 (host firewall: opened).",
		"beta subscribed " + d.TargetID + " to the source's publication; the initial copy of 3 tables runs on the destination.",
		"The initial copy of shop into " + d.TargetID + " on beta finished; every table is synchronised and the destination now follows the source continuously.",
	} {
		if !moveLogged(job, moveStepData, want) {
			t.Fatalf("%q not logged: %+v", want, job.step(moveStepData).Log)
		}
	}
	if estimate := moveLockEstimate(job); !strings.Contains(estimate.Basis, "less the databases that replicate continuously") && estimate.Seconds > moveDeployDefault+moveFreezeOverhead+moveEngineOverhead+5 {
		t.Fatalf("lock estimate %+v", estimate)
	}
	// The source keeps writing until the final sync freezes it.
	rep.with(func() { rep.sourceLSN = "0/5000900" })
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	if !job.FinalSynced || job.Error != "" {
		t.Fatalf("final sync: %q %+v", job.Error, job.step(moveStepFinalSync).Log)
	}
	rep.with(func() {
		if len(rep.set) != 1 || rep.set[0].Name != "public.orders_id_seq" || rep.set[0].Value != 120 || rep.unsubscribed != 1 || rep.unpublished != 1 {
			t.Fatalf("sequences %+v, unsubscribed %d, unpublished %d", rep.set, rep.unsubscribed, rep.unpublished)
		}
	})
	fx.b.with(func() {
		if len(fx.b.restores[d.TargetID]) != 1 {
			t.Fatalf("the final sync restores nothing: %d restores", len(fx.b.restores[d.TargetID]))
		}
	})
	for _, want := range []string{
		"Set 1 sequences of " + d.TargetID + " on beta to the source's positions.",
		d.TargetID + " on beta has every transaction of the frozen source; the replication is removed once the verification passed.",
		"Removed the replication of shop: subscription on beta; publication, slot, role, relay and firewall rule on alpha.",
	} {
		if !moveLogged(job, moveStepFinalSync, want) {
			t.Fatalf("%q not logged: %+v", want, job.step(moveStepFinalSync).Log)
		}
	}
	if job.Databases[0].Link != nil {
		t.Fatalf("the link is gone after the teardown: %+v", job.Databases[0].Link)
	}
	fx.resolver.set("shop.example.com", moveTestTargetIP)
	fx.resolver.set("www.shop.example.com", moveTestTargetIP)
	fx.probe.set(true)
	fx.c.moveTick(context.Background())
	waitMove(t, fx.c, view.ID, moveStateReady)
	if code := moveAction(t, fx.c, view.ID, "finish", nil); code != 202 {
		t.Fatal(code)
	}
	waitMove(t, fx.c, view.ID, moveStateFinished)
	// A second final sync after the teardown has nothing to copy: the
	// frozen source cannot have changed.
	rep.with(func() {
		if rep.unpublished < 1 {
			t.Fatal("the finish tears the source down")
		}
	})
}

func TestMoveFinalSyncAfterAReplicatedOneCopiesNothingAgain(t *testing.T) {
	fx := newMoveFixture(t)
	rep := fx.replicationSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatal(status)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	for range 2 {
		if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
			t.Fatal(code)
		}
		if job := waitMove(t, fx.c, view.ID, moveStateCutover); !job.FinalSynced || job.Error != "" {
			t.Fatalf("%q %+v", job.Error, job.step(moveStepFinalSync).Log)
		}
	}
	job, _ := fx.c.moveJob(view.ID)
	if !moveLogged(job, moveStepFinalSync, job.Databases[0].TargetID+" already has every transaction of the frozen source from the earlier final sync; nothing to copy again.") {
		t.Fatalf("%+v", job.step(moveStepFinalSync).Log)
	}
	rep.with(func() {
		if len(rep.publishes) != 1 || rep.unsubscribed != 1 {
			t.Fatalf("publishes %d unsubscribed %d", len(rep.publishes), rep.unsubscribed)
		}
	})
	fx.b.with(func() {
		if len(fx.b.restores[job.Databases[0].TargetID]) != 1 {
			t.Fatal("nothing is restored again")
		}
	})
}

func TestMoveReplicationFallsBackToThePanelAndThenToADump(t *testing.T) {
	fx := newMoveFixture(t)
	rep := fx.replicationSource()
	rep.with(func() { rep.unreachablePrivate = true })
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	d := job.Databases[0]
	if d.Link == nil || d.Link.Path != moveReplicationPathPanel || d.Link.Host != "127.0.0.1" || d.Link.Port == 0 || d.Link.TLS || !d.Link.Subscribed || d.Link.Firewall != "not needed" {
		t.Fatalf("%+v", d.Link)
	}
	rep.with(func() {
		if len(rep.publishes) != 2 || rep.publishes[0].Listen != "10.40.0.2" || rep.publishes[1].Path != moveReplicationPathPanel || rep.publishes[1].StreamToken != fx.c.moveTunnelToken(view.ID, "source", "shop") || rep.publishes[1].Listen != "" || rep.publishes[1].TLS {
			t.Fatalf("publishes %+v", rep.publishes)
		}
		if len(rep.subscribes) != 1 || rep.subscribes[0].Endpoint.Host != "127.0.0.1" || rep.subscribes[0].Endpoint.Port != d.Link.Port || rep.subscribes[0].Endpoint.SSLMode != "disable" || rep.subscribes[0].Endpoint.CA != "" || rep.subscribes[0].Endpoint.Password != "secret-2" {
			t.Fatalf("subscribes %+v", rep.subscribes)
		}
	})
	rep.tunnel.with(func() {
		if len(rep.tunnel.reached) != 1 || rep.tunnel.reached[0].Host != "10.40.0.2" || rep.tunnel.reached[0].Port != 45001 || rep.tunnel.reached[0].TimeoutSeconds != 5 {
			t.Fatalf("the private relay is probed first: %+v", rep.tunnel.reached)
		}
		if rep.tunnel.streams == 0 {
			t.Fatal("the subscription's probe went through the panel to the source")
		}
	})
	for _, want := range []string{
		"The destination cannot reach the source over the private network 10.40.0.0/20 (connection to 10.40.0.2:45001 timed out after 5s); replicating through the control panel instead.",
		"Replication of shop connects over the control panel.",
		"Published shop on alpha as lidza_move_" + view.ID + " (3 tables); the control panel connects its streams to the database there (no relay port, no firewall rule).",
		"The destination's PostgreSQL connects to 127.0.0.1:" + strconv.Itoa(d.Link.Port) + " on beta; the control panel keeps 2 idle streams open to it and opens a stream to the source's database for each connection.",
	} {
		if !moveLogged(job, moveStepData, want) {
			t.Fatalf("%q not logged: %+v", want, job.step(moveStepData).Log)
		}
	}
	// A schema change on the source breaks the subscription during the
	// locked final sync: the step fails with what to do, and the retry
	// copies the database with a dump.
	rep.with(func() {
		rep.applyErrors, rep.lastError = 1, `logical replication target relation "public.orders" is missing replicated column: "note"`
	})
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	if job.Failure == nil || job.Failure.Code != "replication_broken" || job.Failure.Class != moveClassOperator || !strings.Contains(job.Failure.Message, "missing replicated column") || !strings.Contains(job.Failure.Remediation, "Retry the final sync: it copies shop with a fresh dump") {
		t.Fatalf("failure %+v", job.Failure)
	}
	if job.Databases[0].Replication != moveReplicationDump || !strings.Contains(job.Databases[0].ReplicationReason, "missing replicated column") || job.Databases[0].Link != nil {
		t.Fatalf("%+v", job.Databases[0])
	}
	rep.with(func() {
		if rep.unsubscribed != 1 || rep.unpublished != 1 {
			t.Fatalf("teardown: unsubscribed %d unpublished %d", rep.unsubscribed, rep.unpublished)
		}
	})
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	if !job.FinalSynced || job.Error != "" {
		t.Fatalf("retry: %q %+v", job.Error, job.step(moveStepFinalSync).Log)
	}
	fx.b.with(func() {
		restores := fx.b.restores[d.TargetID]
		if len(restores) != 2 || string(restores[1]) != "PGDMP container archive with rows" {
			t.Fatalf("the retry restores a full dump: %q", restores)
		}
	})
}

func TestMoveReplicationFallsBackToADumpDuringTheBulkCopy(t *testing.T) {
	fx := newMoveFixture(t)
	rep := fx.replicationSource()
	rep.with(func() { rep.applyErrors, rep.lastError = 1, "duplicate key value violates unique constraint" })
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	d := job.Databases[0]
	if d.Replication != moveReplicationDump || d.ReplicationReason != "the subscription reports errors: duplicate key value violates unique constraint" || d.Link != nil || d.Copied == "replication" {
		t.Fatalf("%+v", d)
	}
	if !moveLogged(job, moveStepData, "Database shop cannot replicate continuously: the subscription reports errors: duplicate key value violates unique constraint. Copying it with dump and restore instead, for this and every later copy.") {
		t.Fatalf("%+v", job.step(moveStepData).Log)
	}
	fx.b.with(func() {
		restores := fx.b.restores[d.TargetID]
		if len(restores) != 2 || string(restores[1]) != "PGDMP container archive with rows" {
			t.Fatalf("restores %q", restores)
		}
	})
	rep.with(func() {
		if rep.unsubscribed != 1 || rep.unpublished != 1 {
			t.Fatalf("teardown: unsubscribed %d unpublished %d", rep.unsubscribed, rep.unpublished)
		}
	})
	// Every later copy dumps: the final sync does not touch replication.
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	if !job.FinalSynced {
		t.Fatalf("%+v", job.step(moveStepFinalSync).Log)
	}
	rep.with(func() {
		if len(rep.publishes) != 1 {
			t.Fatalf("the dump path publishes nothing more: %d", len(rep.publishes))
		}
	})
}

func TestMoveAbortTearsReplicationDownOnBothEnds(t *testing.T) {
	fx := newMoveFixture(t)
	rep := fx.replicationSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatal(status)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	if code := moveAction(t, fx.c, view.ID, "abort", nil); code != 202 {
		t.Fatal(code)
	}
	job := waitMove(t, fx.c, view.ID, moveStateAborted)
	rep.with(func() {
		if rep.unsubscribed != 1 || rep.unpublished != 1 {
			t.Fatalf("abort: unsubscribed %d unpublished %d", rep.unsubscribed, rep.unpublished)
		}
	})
	if job.Databases[0].Link != nil {
		t.Fatalf("link kept: %+v", job.Databases[0].Link)
	}
	if !moveLogged(job, moveStepPlan, "Removed the replication of shop: subscription on beta; publication, slot, role, relay and firewall rule on alpha.") {
		t.Fatalf("%+v", job.step(moveStepPlan).Log)
	}
}
