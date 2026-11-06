package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Continuous replication of a PostgreSQL database during a move. Instead of
// dumping the source twice (once while it serves, once more with the
// application locked), the destination subscribes to the source with
// logical replication: the schema is restored from a schema-only dump, a
// publication on the source and a subscription on the destination copy the
// tables once and then stream every change, and the locked final sync only
// waits for the last transactions to arrive. The source agent's part:
//
//   - readiness in MoveSourceDatabase.Replication: server version, wal_level,
//     free slots and WAL senders, tables without a replica identity;
//   - POST   /v1/move/databases/{id}/replication          role, publication, relay, firewall
//   - GET    /v1/move/databases/{id}/replication          publisher LSN, slot, sequences
//   - DELETE /v1/move/databases/{id}/replication          tear everything down
//   - POST   /v1/move/databases/{id}/replication/restart  wal_level=logical and restart
//   - POST   /v1/move/databases/{id}/schema               schema-only dump (downloaded as table data)
//
// and the destination agent's part:
//
//   - POST   /v1/move/databases/{id}/subscription           create or repoint the subscription
//   - GET    /v1/move/databases/{id}/subscription           table states, LSNs, errors
//   - POST   /v1/move/databases/{id}/subscription/sequences setval from the source's values
//   - DELETE /v1/move/databases/{id}/subscription           drop it (slot kept for the source to drop)
//
// Tables without a primary key or REPLICA IDENTITY cannot replicate UPDATE
// and DELETE (PostgreSQL refuses the statement on the publisher), so they are
// left out of the publication and the move copies them with the dump-based
// table copy instead, at the bulk copy and again at the final sync. Every
// replication slot the agent's moves create is recorded with an expiry: a
// slot left behind would retain WAL until the disk is full, so the
// maintenance tick drops what no move tore down in time.

const (
	moveReplicationLifetime = 7 * 24 * time.Hour
	moveReplicationPrefix   = "lidza_move_"
	moveReplicationDir      = "lidza-move"
)

var moveReplicationNamePattern = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// MoveReplicationReadiness is what the source reports about logical
// replication of one database.
type MoveReplicationReadiness struct {
	// Supported reports that this agent runs the replication endpoints.
	Supported bool `json:"supported"`
	// ServerVersion is the PostgreSQL major version.
	ServerVersion int    `json:"server_version,omitempty"`
	WALLevel      string `json:"wal_level,omitempty"`
	FreeSlots     int    `json:"free_slots"`
	FreeSenders   int    `json:"free_senders"`
	// NoIdentity lists tables without a primary key or replica identity;
	// they are copied with the dump-based table copy.
	NoIdentity []string `json:"no_identity,omitempty"`
	// RestartNeeded reports a wal_level below logical: Prepare sets it and
	// restarts the database once.
	RestartNeeded bool `json:"restart_needed"`
	// Reason says why the database cannot be replicated at all.
	Reason string `json:"reason,omitempty"`
}

// MoveReplicationRequest asks the source to publish a database for a move.
type MoveReplicationRequest struct {
	Move string `json:"move"`
	// Listen is the address of this host the relay binds; Peers the
	// addresses allowed to connect; TLS whether the relay terminates TLS
	// (the public path).
	Listen string   `json:"listen"`
	Peers  []string `json:"peers"`
	TLS    bool     `json:"tls"`
	// Exclude are tables left out of the publication (schema.table).
	Exclude []string `json:"exclude,omitempty"`
	// Path "panel" carries the subscription's connections through the
	// control panel (move_replication_tunnel.go): no relay listens and no
	// firewall rule opens; Listen, Peers and TLS are ignored and
	// StreamToken (32+ characters) authorises the panel's streams to the
	// database. Empty is the direct relay.
	Path        string `json:"path,omitempty"`
	StreamToken string `json:"stream_token,omitempty"`
}

// MoveReplicationEndpoint is how the destination subscribes. The password
// is freshly set on every request and never stored by the control panel.
type MoveReplicationEndpoint struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Database    string `json:"database"`
	User        string `json:"user"`
	Password    string `json:"password"`
	SSLMode     string `json:"sslmode"`
	CA          string `json:"ca,omitempty"` // PEM, on the public path
	Publication string `json:"publication"`
	Slot        string `json:"slot"`
	// Tables counts the published tables; Firewall says what the host
	// firewall did: opened, inactive or unmanaged.
	Tables   int    `json:"tables"`
	Firewall string `json:"firewall"`
}

// MoveReplicationStatus is the publisher's view.
type MoveReplicationStatus struct {
	CurrentLSN string `json:"current_lsn"`
	// SlotExists and SlotActive describe the subscription's slot;
	// RetainedBytes is the WAL the slot holds back.
	SlotExists    bool           `json:"slot_exists"`
	SlotActive    bool           `json:"slot_active"`
	ConfirmedLSN  string         `json:"confirmed_lsn,omitempty"`
	RetainedBytes int64          `json:"retained_bytes"`
	WALLevel      string         `json:"wal_level"`
	Relay         bool           `json:"relay"`
	Sequences     []MoveSequence `json:"sequences,omitempty"`
}

// MoveSequence is one sequence's position.
type MoveSequence struct {
	Name   string `json:"name"`
	Value  int64  `json:"value"`
	Called bool   `json:"called"`
}

// MoveSubscriptionRequest asks the destination to subscribe.
type MoveSubscriptionRequest struct {
	Move     string                  `json:"move"`
	Endpoint MoveReplicationEndpoint `json:"endpoint"`
}

// MoveSubscriptionStatus is the subscriber's view.
type MoveSubscriptionStatus struct {
	Name    string `json:"name"`
	Exists  bool   `json:"exists"`
	Enabled bool   `json:"enabled"`
	// Tables counts tables by pg_subscription_rel state (i initialize, d
	// data copy, f finished copy, s synchronized, r ready); Total and
	// Ready sum them.
	Tables       map[string]int `json:"tables"`
	Total        int            `json:"total"`
	Ready        int            `json:"ready"`
	ReceivedLSN  string         `json:"received_lsn,omitempty"`
	LatestEndLSN string         `json:"latest_end_lsn,omitempty"`
	Workers      int            `json:"workers"`
	ApplyErrors  int64          `json:"apply_errors"`
	SyncErrors   int64          `json:"sync_errors"`
	// LastError is the newest replication error in the server log.
	LastError string `json:"last_error,omitempty"`
}

// MoveSequenceRequest sets sequences on the destination.
type MoveSequenceRequest struct {
	Sequences []MoveSequence `json:"sequences"`
}

// MoveReplicationRecord is the source's durable record of one published
// database, so a restart tears it down the same way and nothing outlives
// its move. Expires moves on with every status read by the control panel
// (which follows a replicating move at least once a minute); a publication
// nobody asked about for the lifetime is torn down by the maintenance tick.
type MoveReplicationRecord struct {
	Move        string    `json:"move"`
	Role        string    `json:"role"`
	Publication string    `json:"publication"`
	Slot        string    `json:"slot"`
	Listen      string    `json:"listen"`
	Port        int       `json:"port"`
	Peers       []string  `json:"peers"`
	TLS         bool      `json:"tls"`
	Firewall    bool      `json:"firewall"`
	Created     time.Time `json:"created"`
	Expires     time.Time `json:"expires"`
	// Path is "panel" when the subscription connects through the control
	// panel; StreamToken is the SHA-256 of the token its streams carry.
	Path        string `json:"path,omitempty"`
	StreamToken string `json:"stream_token,omitempty"`
}

// moveReplicationName derives the role, publication, slot and subscription
// name of a move.
func moveReplicationName(move string) (string, error) {
	cleaned := strings.ToLower(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, move))
	if len(cleaned) > 40 {
		cleaned = cleaned[:40]
	}
	if !moveReplicationNamePattern.MatchString(cleaned) {
		return "", errors.New("invalid move id")
	}
	return moveReplicationPrefix + cleaned, nil
}

// ---- SQL ------------------------------------------------------------------------

const moveReplicationReadinessSQL = `SELECT 'version', current_setting('server_version_num')
UNION ALL SELECT 'wal_level', current_setting('wal_level')
UNION ALL SELECT 'max_slots', current_setting('max_replication_slots')
UNION ALL SELECT 'slots', count(*)::text FROM pg_catalog.pg_replication_slots
UNION ALL SELECT 'max_senders', current_setting('max_wal_senders')
UNION ALL SELECT 'senders', count(*)::text FROM pg_catalog.pg_stat_replication
UNION ALL SELECT 'noidentity', n.nspname || '.' || c.relname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  WHERE c.relkind = 'r' AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
  AND (c.relreplident = 'n' OR c.relreplident = 'd' AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid = c.oid AND i.indisprimary))`

// parseReplicationReadiness reads the readiness query's key/value rows.
func parseReplicationReadiness(text string) MoveReplicationReadiness {
	out := MoveReplicationReadiness{Supported: true}
	maxSlots, slots, maxSenders, senders := 0, 0, 0, 0
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(value)
		switch key {
		case "version":
			out.ServerVersion = n / 10000
		case "wal_level":
			out.WALLevel = value
		case "max_slots":
			maxSlots = n
		case "slots":
			slots = n
		case "max_senders":
			maxSenders = n
		case "senders":
			senders = n
		case "noidentity":
			if moveTableName.MatchString(value) {
				out.NoIdentity = append(out.NoIdentity, value)
			}
		}
	}
	slices.Sort(out.NoIdentity)
	out.FreeSlots, out.FreeSenders = max(maxSlots-slots, 0), max(maxSenders-senders, 0)
	out.RestartNeeded = out.WALLevel != "logical"
	return out
}

const moveReplicationTablesSQL = "SELECT n.nspname || '.' || c.relname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%' ORDER BY 1"

// renderMovePublicationSQL creates the move's replication role (REPLICATION
// LOGIN, read access to every table) and its publication: FOR ALL TABLES
// when nothing is excluded, else FOR TABLE with every table but the
// excluded ones. The password is a SCRAM verifier, never the plaintext.
func renderMovePublicationSQL(role, publication, database, verifier string, tables, exclude []string) (string, error) {
	roleIdent, err := pgQuoteIdent(role)
	if err != nil {
		return "", err
	}
	pubIdent, err := pgQuoteIdent(publication)
	if err != nil {
		return "", err
	}
	dbIdent, err := pgQuoteIdent(database)
	if err != nil {
		return "", err
	}
	roleLiteral, _ := pgQuoteLiteral(role)
	pubLiteral, _ := pgQuoteLiteral(publication)
	if !strings.HasPrefix(verifier, "SCRAM-SHA-256$") {
		return "", errors.New("the replication password must be a SCRAM-SHA-256 verifier")
	}
	verifierLiteral, _ := pgQuoteLiteral(verifier)
	createRole, _ := pgQuoteLiteral("CREATE ROLE " + roleIdent)
	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = %s)\\gexec\n", createRole, roleLiteral)
	fmt.Fprintf(&b, "ALTER ROLE %s WITH LOGIN REPLICATION NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD %s;\n", roleIdent, verifierLiteral)
	fmt.Fprintf(&b, "GRANT pg_read_all_data TO %s;\n", roleIdent)
	fmt.Fprintf(&b, "GRANT CONNECT ON DATABASE %s TO %s;\n", dbIdent, roleIdent)
	published := []string{}
	for _, table := range tables {
		if !slices.Contains(exclude, table) {
			published = append(published, moveTableIdent(table))
		}
	}
	create := "CREATE PUBLICATION " + pubIdent + " FOR ALL TABLES"
	if len(exclude) > 0 {
		if len(published) == 0 {
			return "", errors.New("every table is excluded from the publication")
		}
		create = "CREATE PUBLICATION " + pubIdent + " FOR TABLE " + strings.Join(published, ", ")
	}
	createLiteral, _ := pgQuoteLiteral(create)
	fmt.Fprintf(&b, "SELECT %s WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_publication WHERE pubname = %s)\\gexec\n", createLiteral, pubLiteral)
	return b.String(), nil
}

// renderMoveUnpublishSQL drops the slot (ending its WAL sender first), the
// publication and the role; every statement tolerates an absent object.
func renderMoveUnpublishSQL(role, publication, slot string) (string, error) {
	roleIdent, err := pgQuoteIdent(role)
	if err != nil {
		return "", err
	}
	pubIdent, err := pgQuoteIdent(publication)
	if err != nil {
		return "", err
	}
	roleLiteral, _ := pgQuoteLiteral(role)
	slotLiteral, _ := pgQuoteLiteral(slot)
	var b strings.Builder
	fmt.Fprintf(&b, "SELECT count(pg_catalog.pg_terminate_backend(active_pid)) FROM pg_catalog.pg_replication_slots WHERE slot_name = %s AND active_pid IS NOT NULL;\n", slotLiteral)
	b.WriteString("SELECT pg_catalog.pg_sleep(0.5);\n")
	fmt.Fprintf(&b, "SELECT pg_catalog.pg_drop_replication_slot(slot_name) FROM pg_catalog.pg_replication_slots WHERE slot_name = %s AND NOT active;\n", slotLiteral)
	fmt.Fprintf(&b, "DROP PUBLICATION IF EXISTS %s;\n", pubIdent)
	fmt.Fprintf(&b, "SELECT count(pg_catalog.pg_terminate_backend(pid)) FROM pg_catalog.pg_stat_activity WHERE usename = %s AND pid <> pg_catalog.pg_backend_pid();\n", roleLiteral)
	fmt.Fprintf(&b, "DROP OWNED BY %s;\n", roleIdent)
	fmt.Fprintf(&b, "DROP ROLE IF EXISTS %s;\n", roleIdent)
	return b.String(), nil
}

// renderMoveReplicationStatusSQL reads the publisher's position and the
// slot; with sequences it lists every sequence's position too.
func renderMoveReplicationStatusSQL(slot string, sequences bool) string {
	slotLiteral, _ := pgQuoteLiteral(slot)
	var b strings.Builder
	b.WriteString("SELECT 'current_lsn', pg_catalog.pg_current_wal_lsn()::text\n")
	b.WriteString("UNION ALL SELECT 'wal_level', current_setting('wal_level')\n")
	fmt.Fprintf(&b, "UNION ALL SELECT 'slot', active::text || E'\\t' || coalesce(confirmed_flush_lsn::text, '') || E'\\t' || coalesce(pg_catalog.pg_wal_lsn_diff(pg_catalog.pg_current_wal_lsn(), restart_lsn), 0)::text FROM pg_catalog.pg_replication_slots WHERE slot_name = %s\n", slotLiteral)
	if sequences {
		b.WriteString("UNION ALL SELECT 'sequence', schemaname || '.' || sequencename || E'\\t' || coalesce(last_value, start_value)::text || E'\\t' || (last_value IS NOT NULL)::text FROM pg_catalog.pg_sequences\n")
	}
	return b.String()
}

func parseMoveReplicationStatus(text string) MoveReplicationStatus {
	out := MoveReplicationStatus{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		switch fields[0] {
		case "current_lsn":
			if len(fields) > 1 {
				out.CurrentLSN = fields[1]
			}
		case "wal_level":
			if len(fields) > 1 {
				out.WALLevel = fields[1]
			}
		case "slot":
			if len(fields) >= 4 {
				out.SlotExists, out.SlotActive, out.ConfirmedLSN = true, fields[1] == "t" || fields[1] == "true", fields[2]
				out.RetainedBytes, _ = strconv.ParseInt(fields[3], 10, 64)
			}
		case "sequence":
			if len(fields) >= 4 && moveTableName.MatchString(fields[1]) {
				value, err := strconv.ParseInt(fields[2], 10, 64)
				if err == nil {
					out.Sequences = append(out.Sequences, MoveSequence{Name: fields[1], Value: value, Called: fields[3] == "t" || fields[3] == "true"})
				}
			}
		}
	}
	return out
}

// renderMoveSubscriptionSQL creates the subscription, or repoints an
// existing one at a fresh connection string, and enables it.
func renderMoveSubscriptionSQL(name, publication, slot, conninfo string) (string, error) {
	ident, err := pgQuoteIdent(name)
	if err != nil {
		return "", err
	}
	pubIdent, err := pgQuoteIdent(publication)
	if err != nil {
		return "", err
	}
	nameLiteral, _ := pgQuoteLiteral(name)
	slotLiteral, _ := pgQuoteLiteral(slot)
	connLiteral, _ := pgQuoteLiteral(conninfo)
	create, _ := pgQuoteLiteral("CREATE SUBSCRIPTION " + ident + " CONNECTION " + connLiteral + " PUBLICATION " + pubIdent + " WITH (copy_data = true, streaming = on, two_phase = false, slot_name = " + slotLiteral + ", create_slot = true)")
	alter, _ := pgQuoteLiteral("ALTER SUBSCRIPTION " + ident + " CONNECTION " + connLiteral)
	enable, _ := pgQuoteLiteral("ALTER SUBSCRIPTION " + ident + " ENABLE")
	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s WHERE EXISTS (SELECT 1 FROM pg_catalog.pg_subscription WHERE subname = %s)\\gexec\n", alter, nameLiteral)
	fmt.Fprintf(&b, "SELECT %s WHERE EXISTS (SELECT 1 FROM pg_catalog.pg_subscription WHERE subname = %s AND NOT subenabled)\\gexec\n", enable, nameLiteral)
	fmt.Fprintf(&b, "SELECT %s WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_subscription WHERE subname = %s)\\gexec\n", create, nameLiteral)
	return b.String(), nil
}

// renderMoveUnsubscribeSQL disables the subscription, detaches it from its
// slot (the source drops the slot itself) and drops it.
func renderMoveUnsubscribeSQL(name string) (string, error) {
	ident, err := pgQuoteIdent(name)
	if err != nil {
		return "", err
	}
	nameLiteral, _ := pgQuoteLiteral(name)
	disable, _ := pgQuoteLiteral("ALTER SUBSCRIPTION " + ident + " DISABLE")
	detach, _ := pgQuoteLiteral("ALTER SUBSCRIPTION " + ident + " SET (slot_name = NONE)")
	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s WHERE EXISTS (SELECT 1 FROM pg_catalog.pg_subscription WHERE subname = %s AND subenabled)\\gexec\n", disable, nameLiteral)
	fmt.Fprintf(&b, "SELECT %s WHERE EXISTS (SELECT 1 FROM pg_catalog.pg_subscription WHERE subname = %s AND subslotname IS NOT NULL)\\gexec\n", detach, nameLiteral)
	fmt.Fprintf(&b, "DROP SUBSCRIPTION IF EXISTS %s;\n", ident)
	return b.String(), nil
}

func renderMoveSubscriptionStatusSQL(name string) string {
	literal, _ := pgQuoteLiteral(name)
	var b strings.Builder
	fmt.Fprintf(&b, "SELECT 'exists', count(*)::text FROM pg_catalog.pg_subscription WHERE subname = %s\n", literal)
	fmt.Fprintf(&b, "UNION ALL SELECT 'enabled', subenabled::text FROM pg_catalog.pg_subscription WHERE subname = %s\n", literal)
	fmt.Fprintf(&b, "UNION ALL SELECT 'rel:' || r.srsubstate, count(*)::text FROM pg_catalog.pg_subscription_rel r JOIN pg_catalog.pg_subscription s ON s.oid = r.srsubid WHERE s.subname = %s GROUP BY r.srsubstate\n", literal)
	fmt.Fprintf(&b, "UNION ALL SELECT 'received_lsn', coalesce(received_lsn::text, '') FROM pg_catalog.pg_stat_subscription WHERE subname = %s AND relid IS NULL\n", literal)
	fmt.Fprintf(&b, "UNION ALL SELECT 'latest_end_lsn', coalesce(latest_end_lsn::text, '') FROM pg_catalog.pg_stat_subscription WHERE subname = %s AND relid IS NULL\n", literal)
	fmt.Fprintf(&b, "UNION ALL SELECT 'workers', count(*)::text FROM pg_catalog.pg_stat_subscription WHERE subname = %s AND pid IS NOT NULL\n", literal)
	fmt.Fprintf(&b, "UNION ALL SELECT 'apply_errors', apply_error_count::text FROM pg_catalog.pg_stat_subscription_stats WHERE subname = %s\n", literal)
	fmt.Fprintf(&b, "UNION ALL SELECT 'sync_errors', sync_error_count::text FROM pg_catalog.pg_stat_subscription_stats WHERE subname = %s\n", literal)
	return b.String()
}

func parseMoveSubscriptionStatus(name, text string) MoveSubscriptionStatus {
	out := MoveSubscriptionStatus{Name: name, Tables: map[string]int{}}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(value, 10, 64)
		switch {
		case key == "exists":
			out.Exists = n > 0
		case key == "enabled":
			out.Enabled = value == "t" || value == "true"
		case strings.HasPrefix(key, "rel:"):
			state := strings.TrimPrefix(key, "rel:")
			out.Tables[state] += int(n)
			out.Total += int(n)
			if state == "r" {
				out.Ready += int(n)
			}
		case key == "received_lsn":
			out.ReceivedLSN = value
		case key == "latest_end_lsn":
			out.LatestEndLSN = value
		case key == "workers":
			out.Workers = int(n)
		case key == "apply_errors":
			out.ApplyErrors = n
		case key == "sync_errors":
			out.SyncErrors = n
		}
	}
	return out
}

// renderMoveSequencesSQL sets every sequence to the source's position.
func renderMoveSequencesSQL(sequences []MoveSequence) (string, error) {
	var b strings.Builder
	for _, s := range sequences {
		if !moveTableName.MatchString(s.Name) {
			return "", errors.New("sequences must be exact lowercase schema.name identifiers")
		}
		literal, _ := pgQuoteLiteral(moveTableIdent(s.Name))
		fmt.Fprintf(&b, "SELECT pg_catalog.setval(%s::regclass, %d, %t) WHERE pg_catalog.to_regclass(%s) IS NOT NULL;\n", literal, s.Value, s.Called, literal)
	}
	return b.String(), nil
}

// moveReplicationConninfo is the subscription's connection string; the
// password lives in a passfile the server reads, never in the statement.
func moveReplicationConninfo(e MoveReplicationEndpoint, passfile, ca string) string {
	parts := []string{"host=" + e.Host, "port=" + strconv.Itoa(e.Port), "dbname=" + e.Database, "user=" + e.User, "passfile=" + passfile, "connect_timeout=15", "application_name=lidza_move"}
	switch {
	case e.SSLMode == "verify-ca" && ca != "":
		parts = append(parts, "sslmode=verify-ca", "sslrootcert="+ca)
	case e.SSLMode == "disable":
		parts = append(parts, "sslmode=disable")
	default:
		parts = append(parts, "sslmode=require")
	}
	return strings.Join(parts, " ")
}

// ---- source side ------------------------------------------------------------------

// moveRelays are the running relays of a manager, by database id.
type moveRelaySet struct {
	mu     sync.Mutex
	relays map[string]*replicationRelay
}

var moveRelaySets sync.Map // *Manager -> *moveRelaySet

func (m *Manager) moveRelays() *moveRelaySet {
	value, _ := moveRelaySets.LoadOrStore(m, &moveRelaySet{relays: map[string]*replicationRelay{}})
	return value.(*moveRelaySet)
}

// moveReplicationSQL runs a script as the superuser in a database this
// agent manages (the application's database, not "postgres"); tests
// replace it.
var moveReplicationSQL = func(ctx context.Context, m *Manager, target moveDatabaseTarget, script string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if target.Host != nil {
		h := nativeHostDataHost(m)
		out, err := h.run(ctx, script, "/usr/sbin/runuser", "-u", "postgres", "--", "/usr/bin/psql", "-X", "-q", "-A", "-t", "-F", "\t", "-v", "ON_ERROR_STOP=1", "-h", nativePGSocketDir, "-p", strconv.Itoa(target.Host.Port), "-d", target.Host.Name)
		if err != nil {
			return out, fmt.Errorf("psql failed: %s", boundedErrorText(pgRestoreCause(out, err), 300))
		}
		return out, nil
	}
	database, _, err := moveContainerDatabase(ctx, target)
	if err != nil {
		return "", err
	}
	var buffer limitedBuffer
	if err := dockerStream(ctx, strings.NewReader(script), &buffer, nil, "exec", "-i", target.Database.Network, "psql", "-X", "-q", "-A", "-t", "-F", "\t", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", database); err != nil {
		return buffer.String(), err
	}
	return buffer.String(), nil
}

// moveContainerDatabase names a container database and its address on the
// Docker network; tests replace it.
var moveContainerDatabase = func(ctx context.Context, target moveDatabaseTarget) (database, address string, err error) {
	u, err := url.Parse(target.Database.URL)
	if err != nil || strings.TrimPrefix(u.Path, "/") == "" {
		return "", "", errors.New("the database connection is invalid")
	}
	ip, err := command(ctx, "", nil, "docker", "inspect", "-f", `{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}`, target.Database.Network)
	if err != nil {
		return "", "", errors.New("the database container's address could not be read")
	}
	fields := strings.Fields(ip)
	if len(fields) == 0 {
		return "", "", errors.New("the database container has no network address")
	}
	return strings.TrimPrefix(u.Path, "/"), fields[0] + ":5432", nil
}

// moveDatabaseAddress is where the relay forwards to, with the database
// name the subscription names.
func moveDatabaseAddress(ctx context.Context, target moveDatabaseTarget) (database, address string, err error) {
	if target.Host != nil {
		return target.Host.Name, "127.0.0.1:" + strconv.Itoa(target.Host.Port), nil
	}
	return moveContainerDatabase(ctx, target)
}

// moveReplicationReadiness probes a source database.
func (m *Manager) moveReplicationReadiness(ctx context.Context, id string) *MoveReplicationReadiness {
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	text, err := moveReplicationSQL(ctx, m, target, moveReplicationReadinessSQL)
	if err != nil {
		return &MoveReplicationReadiness{Supported: true, Reason: "the database could not be inspected: " + boundedErrorText(err.Error(), 200)}
	}
	readiness := parseReplicationReadiness(text)
	if readiness.ServerVersion < 14 {
		readiness.Reason = fmt.Sprintf("PostgreSQL %d is too old (14 or newer)", readiness.ServerVersion)
	}
	return &readiness
}

func (m *Manager) moveReplicationRecord(id string) (MoveReplicationRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.data.MoveReplications[id]
	return record, ok
}

func (m *Manager) setMoveReplicationRecord(id string, record *MoveReplicationRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.MoveReplications == nil {
		m.data.MoveReplications = map[string]MoveReplicationRecord{}
	}
	if record == nil {
		delete(m.data.MoveReplications, id)
	} else {
		m.data.MoveReplications[id] = *record
	}
	return m.save()
}

// moveReplicationHBAEntriesLocked admits the replication roles of the
// moves published on the cluster at port. Caller holds m.mu.
func (m *Manager) moveReplicationHBAEntriesLocked(port int) []nativeHBAEntry {
	entries := []nativeHBAEntry{}
	for id, record := range m.data.MoveReplications {
		for appID, host := range m.data.NativeHostData {
			if host.Postgres != nil && host.Postgres.Resource == id && host.Postgres.Port == port && appID != "" {
				entries = append(entries, nativeHBAEntry{Role: record.Role, Database: host.Postgres.Name})
			}
		}
	}
	return entries
}

// movePublish creates or refreshes everything the destination needs to
// subscribe to a database: the role (with a new password), the publication,
// the relay and the firewall rule. It is idempotent: a retried request
// reuses the publication and relay and only rotates the password.
func (m *Manager) movePublish(ctx context.Context, id string, in MoveReplicationRequest) (MoveReplicationEndpoint, error) {
	name, err := moveReplicationName(in.Move)
	if err != nil {
		return MoveReplicationEndpoint{}, err
	}
	panel := in.Path == moveTunnelPath
	switch {
	case in.Path != "" && !panel:
		return MoveReplicationEndpoint{}, errors.New("path must be panel or empty")
	case panel && len(in.StreamToken) < 32:
		return MoveReplicationEndpoint{}, errors.New("the panel path needs a stream token of 32 or more characters")
	case !panel:
		if _, err := netip.ParseAddr(in.Listen); err != nil {
			return MoveReplicationEndpoint{}, errors.New("listen must be an IP address of this host")
		}
		if _, err := relayPeers(in.Peers); err != nil {
			return MoveReplicationEndpoint{}, err
		}
	}
	for _, table := range in.Exclude {
		if !moveTableName.MatchString(table) {
			return MoveReplicationEndpoint{}, errors.New("excluded tables must be exact lowercase schema.table names")
		}
	}
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		return MoveReplicationEndpoint{}, err
	}
	database, address, err := moveDatabaseAddress(ctx, target)
	if err != nil {
		return MoveReplicationEndpoint{}, err
	}
	existing, found := m.moveReplicationRecord(id)
	if found && existing.Move != in.Move {
		return MoveReplicationEndpoint{}, fmt.Errorf("database %s is already published for move %s; tear that down first", id, existing.Move)
	}
	// Role and publication.
	password := nativeRandomSecret()
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	verifier, err := nativeSCRAMVerifier(password, salt, nativeSCRAMIterates)
	if err != nil {
		return MoveReplicationEndpoint{}, err
	}
	listing, err := moveReplicationSQL(ctx, m, target, moveReplicationTablesSQL)
	if err != nil {
		return MoveReplicationEndpoint{}, fmt.Errorf("the tables could not be listed: %w", err)
	}
	tables := []string{}
	for _, line := range strings.Split(listing, "\n") {
		if line = strings.TrimSpace(line); moveTableName.MatchString(line) {
			tables = append(tables, line)
		}
	}
	script, err := renderMovePublicationSQL(name, name, database, verifier, tables, in.Exclude)
	if err != nil {
		return MoveReplicationEndpoint{}, err
	}
	if _, err := moveReplicationSQL(ctx, m, target, script); err != nil {
		return MoveReplicationEndpoint{}, fmt.Errorf("the publication could not be created: %w", err)
	}
	published := len(tables)
	for _, table := range in.Exclude {
		if slices.Contains(tables, table) {
			published--
		}
	}
	now := time.Now().UTC()
	record := MoveReplicationRecord{Move: in.Move, Role: name, Publication: name, Slot: name, Listen: in.Listen, Peers: slices.Clone(in.Peers), TLS: in.TLS, Created: now, Expires: now.Add(moveReplicationLifetime)}
	if found {
		record.Created, record.Firewall, record.Port = existing.Created, existing.Firewall, existing.Port
	}
	if panel {
		record.Listen, record.Peers, record.TLS, record.Port, record.Firewall = "", nil, false, 0, false
		record.Path, record.StreamToken = moveTunnelPath, moveTunnelTokenHash(in.StreamToken)
	}
	// The record exists before anything listens, so a crash still tears
	// the role and publication down.
	if err := m.setMoveReplicationRecord(id, &record); err != nil {
		return MoveReplicationEndpoint{}, err
	}
	if target.Host != nil {
		h := nativeHostDataHost(m)
		nativeHostDataMu.Lock()
		err = h.ensureHBA(ctx, target.Host.Port, m.hostDataHBAEntries(target.Host.Port, ""))
		nativeHostDataMu.Unlock()
		if err != nil {
			return MoveReplicationEndpoint{}, fmt.Errorf("the database access rules could not admit the replication role: %w", err)
		}
	}
	if panel {
		// An earlier direct path's relay and firewall rule go: the panel's
		// streams reach the database from this agent's own process.
		m.moveCloseRelay(id)
		if found && existing.Firewall {
			_ = m.moveFirewallClose(ctx, existing.Port, existing.Peers)
		}
		if err := m.setMoveReplicationRecord(id, &record); err != nil {
			return MoveReplicationEndpoint{}, err
		}
		return MoveReplicationEndpoint{Database: database, User: name, Password: password, SSLMode: "disable", Publication: name, Slot: name, Tables: published, Firewall: "not needed"}, nil
	}
	// Relay: reuse one that listens where asked, else start it anew.
	relays := m.moveRelays()
	relays.mu.Lock()
	relay := relays.relays[id]
	if relay != nil && (relay.Address() != in.Listen || relay.TLS() != in.TLS || !slices.Equal(existing.Peers, in.Peers)) {
		relay.Close()
		delete(relays.relays, id)
		relay = nil
	}
	if relay == nil {
		port := 0
		if found && existing.Listen == in.Listen {
			port = existing.Port // keep the port the firewall rule names
		}
		relay, err = newReplicationRelay(in.Listen, port, in.Peers, address, in.TLS)
		if err != nil && port != 0 {
			relay, err = newReplicationRelay(in.Listen, 0, in.Peers, address, in.TLS)
		}
		if err != nil {
			relays.mu.Unlock()
			return MoveReplicationEndpoint{}, err
		}
		relays.relays[id] = relay
	}
	relays.mu.Unlock()
	record.Port = relay.Port()
	// Firewall: one owned rule per peer, for the move's duration.
	firewall := "unmanaged"
	if record.Firewall && existing.Port != record.Port || record.Firewall && !slices.Equal(existing.Peers, in.Peers) {
		_ = m.moveFirewallClose(ctx, existing.Port, existing.Peers)
		record.Firewall = false
	}
	if !record.Firewall {
		opened, state, err := m.moveFirewallOpen(ctx, record.Port, in.Peers, in.Move)
		if err != nil {
			return MoveReplicationEndpoint{}, err
		}
		record.Firewall, firewall = opened, state
	} else {
		firewall = "opened"
	}
	if err := m.setMoveReplicationRecord(id, &record); err != nil {
		return MoveReplicationEndpoint{}, err
	}
	endpoint := MoveReplicationEndpoint{Host: in.Listen, Port: record.Port, Database: database, User: name, Password: password, SSLMode: "disable", Publication: name, Slot: name, Tables: published, Firewall: firewall}
	if in.TLS {
		endpoint.SSLMode, endpoint.CA = "verify-ca", relay.certPEM
	}
	return endpoint, nil
}

// moveUnpublish tears a published database down: relay, firewall rule,
// slot, publication, role and access rule. Every part tolerates being gone
// already; the first error is reported after the rest ran.
func (m *Manager) moveUnpublish(ctx context.Context, id string) error {
	m.moveCloseRelay(id)
	m.moveStreams().closeAll(id)
	record, found := m.moveReplicationRecord(id)
	if !found {
		return nil
	}
	var failures []error
	if record.Firewall {
		if err := m.moveFirewallClose(ctx, record.Port, record.Peers); err != nil {
			failures = append(failures, err)
		} else {
			record.Firewall = false
			_ = m.setMoveReplicationRecord(id, &record)
		}
	}
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err == nil {
		script, err := renderMoveUnpublishSQL(record.Role, record.Publication, record.Slot)
		if err == nil {
			// An active slot ends its sender first; a second pass catches a
			// sender that reconnected in between.
			for attempt := 0; attempt < 3; attempt++ {
				if _, err = moveReplicationSQL(ctx, m, target, script); err == nil {
					break
				}
			}
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("the publication of %s could not be removed: %w", id, err))
		}
		if target.Host != nil && err == nil {
			h := nativeHostDataHost(m)
			nativeHostDataMu.Lock()
			if err := m.setMoveReplicationRecord(id, nil); err != nil {
				failures = append(failures, err)
			}
			if err := h.ensureHBA(ctx, target.Host.Port, m.hostDataHBAEntries(target.Host.Port, "")); err != nil {
				failures = append(failures, err)
			}
			nativeHostDataMu.Unlock()
		}
	}
	// A database that is gone (an aborted or finished move removed it)
	// holds no slot any more; only the host-side parts could still fail.
	if len(failures) == 0 {
		if err := m.setMoveReplicationRecord(id, nil); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

// moveReplicationStatus reads the publisher's position.
func (m *Manager) moveReplicationStatus(ctx context.Context, id string, sequences bool) (MoveReplicationStatus, error) {
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		return MoveReplicationStatus{}, err
	}
	record, found := m.moveReplicationRecord(id)
	if !found {
		return MoveReplicationStatus{}, errors.New("the database is not published for a move")
	}
	text, err := moveReplicationSQL(ctx, m, target, renderMoveReplicationStatusSQL(record.Slot, sequences))
	if err != nil {
		return MoveReplicationStatus{}, err
	}
	// The control panel asks while its move lives: each answer renews the
	// publication's lifetime, so only a move nobody follows any more expires.
	if time.Until(record.Expires) < moveReplicationLifetime-time.Hour {
		record.Expires = time.Now().UTC().Add(moveReplicationLifetime)
		_ = m.setMoveReplicationRecord(id, &record)
	}
	status := parseMoveReplicationStatus(text)
	relays := m.moveRelays()
	relays.mu.Lock()
	// On the panel path the source's part of the relay is the stream route,
	// which answers whenever the record exists.
	status.Relay = relays.relays[id] != nil || record.Path == moveTunnelPath
	relays.mu.Unlock()
	return status, nil
}

// moveReplicationRestart sets wal_level=logical and restarts the database
// once; tests replace it. Containers: ALTER SYSTEM writes
// postgresql.auto.conf in the data volume, then docker restart. Native:
// ALTER SYSTEM, then the cluster's service restarts.
var moveReplicationRestart = func(ctx context.Context, m *Manager, target moveDatabaseTarget) error {
	if _, err := moveReplicationSQL(ctx, m, target, "ALTER SYSTEM SET wal_level = 'logical';\n"); err != nil {
		return fmt.Errorf("wal_level could not be set: %w", err)
	}
	if target.Host != nil {
		h := nativeHostDataHost(m)
		major, cluster, _ := strings.Cut(target.Host.Cluster, "/")
		if out, err := h.run(ctx, "", "/usr/bin/pg_ctlcluster", major, cluster, "restart"); err != nil {
			return fmt.Errorf("the PostgreSQL cluster did not restart: %s", nativeTrimOutput(out))
		}
	} else {
		if _, err := command(ctx, "", nil, "docker", "restart", target.Database.Network); err != nil {
			return errors.New("the database container did not restart")
		}
	}
	for range 60 {
		if text, err := moveReplicationSQL(ctx, m, target, "SHOW wal_level;\n"); err == nil {
			if strings.TrimSpace(text) != "logical" {
				return errors.New("wal_level is still " + strings.TrimSpace(text) + " after the restart")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("the database did not come back after the restart")
}

// moveReplicationSweep tears down publications whose move outlived its
// lifetime, so no slot retains WAL forever.
func (m *Manager) moveReplicationSweep(now time.Time) {
	m.moveTunnelSweep(now)
	m.mu.Lock()
	expired := []string{}
	for id, record := range m.data.MoveReplications {
		if now.After(record.Expires) {
			expired = append(expired, id)
		}
	}
	m.mu.Unlock()
	for _, id := range expired {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Minute)
		if err := m.moveUnpublish(ctx, id); err != nil {
			log.Printf("move replication of %s: expired publication not fully removed: %v", id, err)
		} else {
			log.Printf("move replication of %s: removed the publication and slot of a move that ended %s ago without tearing them down", id, moveReplicationLifetime)
		}
		cancel()
	}
}

// ---- schema dump --------------------------------------------------------------------

// moveDumpSchema writes a schema-only custom-format dump; tests replace it.
var moveDumpSchema = func(ctx context.Context, m *Manager, id string, output io.Writer) error {
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	options := []string{"--format=custom", "--no-owner", "--no-acl", "--schema-only", "--lock-wait-timeout=30s"}
	if target.Host != nil {
		return m.moveHostPGTool(ctx, *target.Host, "pg_dump", options, nil, output)
	}
	environment, flags := postgresEnvironment(target.Database.URL)
	args := []string{"run", "--rm", "--network", target.Database.Network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128"}
	args = append(args, flags...)
	args = append(args, databaseImage, "pg_dump")
	args = append(args, options...)
	if err := dockerStream(ctx, nil, output, environment, args...); err != nil {
		return errors.New("the schema-only dump failed")
	}
	return nil
}

// moveSchemaExportRoute dumps the schema into the database's table-data
// directory, where the existing download route serves it.
func (m *Manager) moveSchemaExportRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		Fail(w, 400, errors.New("invalid database"))
		return
	}
	if m.upgradePending() {
		Fail(w, 409, errors.New("agent upgrade in progress"))
		return
	}
	m.mu.Lock()
	_, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		Fail(w, 404, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	ctx, endProgress := m.trackDatabase(ctx, id, "Dumping the schema")
	defer endProgress()
	dir := m.moveTableDataDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		Fail(w, 500, errors.New("cannot prepare the dump directory"))
		return
	}
	file, err := os.CreateTemp(dir, ".dump-*")
	if err != nil {
		Fail(w, 500, errors.New("cannot store the dump"))
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	counter := &progressWriter{}
	if err := moveDumpSchema(ctx, m, id, io.MultiWriter(file, hash, counter)); err != nil {
		Fail(w, 409, err)
		return
	}
	if err := file.Sync(); err != nil || counter.n.Load() == 0 {
		Fail(w, 500, errors.New("the dump could not be stored"))
		return
	}
	export := MoveTableDataExport{ID: nativeRandomSecret(), Size: counter.n.Load(), SHA256: hex.EncodeToString(hash.Sum(nil)), Tables: []string{}, Rows: map[string]int64{}}
	if err := os.Rename(file.Name(), filepath.Join(dir, export.ID+".dump")); err != nil {
		Fail(w, 500, errors.New("the dump could not be finalized"))
		return
	}
	raw, _ := json.Marshal(export)
	if err := os.WriteFile(filepath.Join(dir, export.ID+".json"), raw, 0o600); err != nil {
		Fail(w, 500, errors.New("the dump record could not be stored"))
		return
	}
	JSON(w, 200, export)
}

// ---- destination side ---------------------------------------------------------------

// moveSubscriptionFiles writes the passfile and CA the subscription's
// connection reads, where the database server can read them, and returns
// their paths as the server sees them; tests replace it. Native: a directory
// under the postgres user's home. Container: inside the container's data
// volume, written through docker exec.
var moveSubscriptionFiles = func(ctx context.Context, m *Manager, target moveDatabaseTarget, passfile, ca []byte) (passPath, caPath string, err error) {
	if target.Host != nil {
		h := nativeHostDataHost(m)
		dir := filepath.Join("/var/lib/postgresql", moveReplicationDir, target.ID)
		if err := os.MkdirAll(h.path(dir), 0o700); err != nil {
			return "", "", err
		}
		passPath, caPath = filepath.Join(dir, "pgpass"), filepath.Join(dir, "ca.pem")
		if err := os.WriteFile(h.path(passPath), passfile, 0o600); err != nil {
			return "", "", err
		}
		if len(ca) > 0 {
			if err := os.WriteFile(h.path(caPath), ca, 0o600); err != nil {
				return "", "", err
			}
		} else {
			caPath = ""
		}
		if _, err := h.run(ctx, "", "/bin/chown", "-R", "postgres:postgres", h.path(dir)); err != nil {
			return "", "", errors.New("the subscription's credential files could not be handed to postgres")
		}
		return passPath, caPath, nil
	}
	dir := "/var/lib/postgresql/data/" + moveReplicationDir
	write := func(name string, content []byte) error {
		script := `umask 077; mkdir -p "$1" && cat > "$1/$2" && chown -R postgres:postgres "$1"`
		return dockerStream(ctx, strings.NewReader(string(content)), io.Discard, nil, "exec", "-i", "-u", "root", target.Database.Network, "sh", "-c", script, "sh", dir, name)
	}
	if err := write("pgpass", passfile); err != nil {
		return "", "", errors.New("the subscription's passfile could not be written into the database container")
	}
	passPath = dir + "/pgpass"
	if len(ca) > 0 {
		if err := write("ca.pem", ca); err != nil {
			return "", "", errors.New("the subscription's CA could not be written into the database container")
		}
		caPath = dir + "/ca.pem"
	}
	return passPath, caPath, nil
}

// moveSubscriptionFilesRemove removes the files again; tests replace it.
var moveSubscriptionFilesRemove = func(ctx context.Context, m *Manager, target moveDatabaseTarget) {
	if target.Host != nil {
		h := nativeHostDataHost(m)
		_ = os.RemoveAll(h.path(filepath.Join("/var/lib/postgresql", moveReplicationDir, target.ID)))
		return
	}
	_ = dockerStream(ctx, nil, io.Discard, nil, "exec", "-u", "root", target.Database.Network, "rm", "-rf", "/var/lib/postgresql/data/"+moveReplicationDir)
}

// moveSubscriptionProbe connects to the endpoint the way the server will;
// tests replace it. It turns an unreachable relay into a clear error before
// a subscription is created.
var moveSubscriptionProbe = func(ctx context.Context, m *Manager, target moveDatabaseTarget, conninfo string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out string
	var err error
	if target.Host != nil {
		h := nativeHostDataHost(m)
		out, err = h.run(ctx, "", "/usr/sbin/runuser", "-u", "postgres", "--", "/usr/bin/psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", conninfo, "-c", "SELECT 1")
	} else {
		// psql's own message names the address and the failure (it carries
		// no password: that is in the passfile).
		out, err = moveDockerOutput(ctx, "exec", "-u", "postgres", target.Database.Network, "psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", conninfo, "-c", "SELECT 1")
	}
	if err != nil {
		return fmt.Errorf("the destination cannot reach the source's replication endpoint: %s", boundedErrorText(pgRestoreCause(out, err), 300))
	}
	return nil
}

// moveDockerOutput runs a docker command and returns its combined output,
// also on failure; tests replace it.
var moveDockerOutput = func(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var buffer limitedBuffer
	cmd.Stdout, cmd.Stderr = &buffer, &buffer
	cmd.WaitDelay = 5 * time.Second
	for _, name := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT"} {
		if v := os.Getenv(name); v != "" {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		return buffer.String(), fmt.Errorf("timed out: %w", ctx.Err())
	}
	return buffer.String(), err
}

// moveSubscribe creates the subscription, or repoints an existing one.
func (m *Manager) moveSubscribe(ctx context.Context, id string, in MoveSubscriptionRequest) (MoveSubscriptionStatus, error) {
	name, err := moveReplicationName(in.Move)
	if err != nil {
		return MoveSubscriptionStatus{}, err
	}
	e := in.Endpoint
	if _, err := netip.ParseAddr(e.Host); err != nil || e.Port < 1 || e.Port > 65535 || e.Database == "" || e.User == "" || e.Password == "" || e.Publication == "" || e.Slot == "" {
		return MoveSubscriptionStatus{}, errors.New("the replication endpoint is incomplete")
	}
	if strings.ContainsAny(e.Database+e.User+e.Publication+e.Slot, " '\\\n") {
		return MoveSubscriptionStatus{}, errors.New("the replication endpoint has invalid names")
	}
	if e.SSLMode == "verify-ca" && !strings.Contains(e.CA, "BEGIN CERTIFICATE") {
		return MoveSubscriptionStatus{}, errors.New("a verify-ca endpoint needs its CA")
	}
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		return MoveSubscriptionStatus{}, err
	}
	// pgpass: host and port as wildcards, so an IPv6 host needs no escaping.
	escape := func(s string) string { return strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(s) }
	passfile := "*:*:" + escape(e.Database) + ":" + escape(e.User) + ":" + escape(e.Password) + "\n"
	passPath, caPath, err := moveSubscriptionFiles(ctx, m, target, []byte(passfile), []byte(e.CA))
	if err != nil {
		return MoveSubscriptionStatus{}, err
	}
	conninfo := moveReplicationConninfo(e, passPath, caPath)
	// A TCP dial first: an unreachable endpoint is reported with its
	// address and the network error instead of psql's.
	if err := moveReachTCP(ctx, e.Host, e.Port, moveReachTimeout); err != nil {
		return MoveSubscriptionStatus{}, fmt.Errorf("the destination cannot reach the source's replication endpoint: %w", err)
	}
	if err := moveSubscriptionProbe(ctx, m, target, conninfo); err != nil {
		return MoveSubscriptionStatus{}, err
	}
	script, err := renderMoveSubscriptionSQL(name, e.Publication, e.Slot, conninfo)
	if err != nil {
		return MoveSubscriptionStatus{}, err
	}
	if _, err := moveReplicationSQL(ctx, m, target, script); err != nil {
		return MoveSubscriptionStatus{}, fmt.Errorf("the subscription could not be created: %w", err)
	}
	return m.moveSubscriptionStatus(ctx, id, in.Move)
}

// moveSubscriptionStatus reads the subscriber's view.
func (m *Manager) moveSubscriptionStatus(ctx context.Context, id, move string) (MoveSubscriptionStatus, error) {
	name, err := moveReplicationName(move)
	if err != nil {
		return MoveSubscriptionStatus{}, err
	}
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		return MoveSubscriptionStatus{}, err
	}
	text, err := moveReplicationSQL(ctx, m, target, renderMoveSubscriptionStatusSQL(name))
	if err != nil {
		return MoveSubscriptionStatus{}, err
	}
	status := parseMoveSubscriptionStatus(name, text)
	if status.ApplyErrors > 0 || status.SyncErrors > 0 || status.Exists && status.Workers == 0 {
		status.LastError = moveSubscriptionLogError(ctx, target, name)
	}
	return status, nil
}

// moveSubscriptionLogError finds the newest replication error for the
// subscription in the server log; tests replace it.
var moveSubscriptionLogError = func(ctx context.Context, target moveDatabaseTarget, name string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var text string
	if target.Host != nil {
		major, cluster, _ := strings.Cut(target.Host.Cluster, "/")
		out, err := nativeHostRun(ctx, "", "/usr/bin/tail", "-n", "400", filepath.Join("/var/log/postgresql", "postgresql-"+major+"-"+cluster+".log"))
		if err != nil {
			return ""
		}
		text = out
	} else {
		out, err := command(ctx, "", nil, "docker", "logs", "--tail", "400", target.Database.Network)
		if err != nil {
			return ""
		}
		text = out
	}
	return lastSubscriptionError(text, name)
}

// lastSubscriptionError is the newest ERROR line of the log that names the
// subscription (or logical replication), bounded.
func lastSubscriptionError(text, name string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.Contains(line, "ERROR") || !(strings.Contains(line, name) || strings.Contains(line, "logical replication")) {
			continue
		}
		if at := strings.Index(line, "ERROR:"); at >= 0 {
			line = strings.TrimSpace(line[at+len("ERROR:"):])
		}
		return boundedErrorText(line, 300)
	}
	return ""
}

// moveUnsubscribe drops the subscription and its credential files.
func (m *Manager) moveUnsubscribe(ctx context.Context, id, move string) error {
	name, err := moveReplicationName(move)
	if err != nil {
		return err
	}
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		// The database is gone with the application; its tunnel goes too.
		return m.moveTunnelClose(ctx, id, move)
	}
	script, err := renderMoveUnsubscribeSQL(name)
	if err != nil {
		return err
	}
	if _, err := moveReplicationSQL(ctx, m, target, script); err != nil {
		return fmt.Errorf("the subscription could not be dropped: %w", err)
	}
	moveSubscriptionFilesRemove(ctx, m, target)
	return m.moveTunnelClose(ctx, id, move)
}

// moveSetSequences sets the destination's sequences.
func (m *Manager) moveSetSequences(ctx context.Context, id string, sequences []MoveSequence) error {
	if len(sequences) > 10000 {
		return errors.New("too many sequences")
	}
	m.mu.Lock()
	target, err := m.moveDatabaseTargetLocked(id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	script, err := renderMoveSequencesSQL(sequences)
	if err != nil {
		return err
	}
	if script == "" {
		return nil
	}
	if _, err := moveReplicationSQL(ctx, m, target, script); err != nil {
		return fmt.Errorf("the sequences could not be set: %w", err)
	}
	return nil
}

// ---- routes -------------------------------------------------------------------------

func (m *Manager) moveReplicationRoutes(mux *http.ServeMux) {
	validID := func(w http.ResponseWriter, id string) bool {
		if !idPattern.MatchString(id) {
			Fail(w, 400, errors.New("invalid database"))
			return false
		}
		return true
	}
	mux.HandleFunc("POST /v1/move/databases/{id}/replication", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var in MoveReplicationRequest
		if !validID(w, id) {
			return
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		endpoint, err := m.movePublish(ctx, id, in)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, endpoint)
	})
	mux.HandleFunc("GET /v1/move/databases/{id}/replication", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validID(w, id) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		status, err := m.moveReplicationStatus(ctx, id, r.URL.Query().Get("sequences") == "1")
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, status)
	})
	mux.HandleFunc("DELETE /v1/move/databases/{id}/replication", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validID(w, id) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		if err := m.moveUnpublish(ctx, id); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "removed"})
	})
	mux.HandleFunc("POST /v1/move/databases/{id}/replication/restart", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validID(w, id) {
			return
		}
		if m.upgradePending() {
			Fail(w, 409, errors.New("agent upgrade in progress"))
			return
		}
		m.mu.Lock()
		target, err := m.moveDatabaseTargetLocked(id)
		busy := err == nil && target.Database.Operation != ""
		m.mu.Unlock()
		if err != nil {
			Fail(w, 404, err)
			return
		}
		if busy {
			Fail(w, 409, errors.New("a database operation is already running"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		if err := moveReplicationRestart(ctx, m, target); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, m.moveReplicationReadiness(ctx, id))
	})
	mux.HandleFunc("POST /v1/move/databases/{id}/schema", m.moveTransfer(m.moveSchemaExportRoute))
	mux.HandleFunc("POST /v1/move/databases/{id}/subscription", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var in MoveSubscriptionRequest
		if !validID(w, id) {
			return
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		status, err := m.moveSubscribe(ctx, id, in)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, status)
	})
	mux.HandleFunc("GET /v1/move/databases/{id}/subscription", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validID(w, id) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		status, err := m.moveSubscriptionStatus(ctx, id, r.URL.Query().Get("move"))
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, status)
	})
	mux.HandleFunc("DELETE /v1/move/databases/{id}/subscription", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validID(w, id) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		if err := m.moveUnsubscribe(ctx, id, r.URL.Query().Get("move")); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "removed"})
	})
	m.moveTunnelRoutes(mux)
	mux.HandleFunc("POST /v1/move/databases/{id}/subscription/sequences", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var in MoveSequenceRequest
		if !validID(w, id) {
			return
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		if err := m.moveSetSequences(ctx, id, in.Sequences); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]int{"sequences": len(in.Sequences)})
	})
}
