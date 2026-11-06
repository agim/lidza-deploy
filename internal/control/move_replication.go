package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Continuous replication of PostgreSQL databases during a move.
//
// With the dump path the locked final sync copies the whole database a
// second time. With continuous replication the bulk copy restores the
// schema from a schema-only dump, publishes the database on the source and
// subscribes the destination to it: the subscription copies every table
// once (the bulk copy's progress is the tables in i/d/f/s/r state) and then
// streams each transaction as it commits, so the locked final sync waits
// seconds for the last transactions, copies the sequences, verifies and
// drops the subscription. The plan chooses the mode per database from the
// source's readiness report and names the reason whenever it falls back to
// the dump path; the operator can force the dump path for a move.
//
// The panel decides how the subscription's connections reach the source:
//
//   - "private": a relay the source agent runs on a private network both
//     hosts share (agent/move_replication_relay.go; the firewall admits the
//     destination's private address only, no TLS needed), chosen only when
//     both hosts report addresses within each other's network and the
//     destination agent opens a TCP connection to the relay's port;
//   - "panel", the default: through the control panel, which already
//     reaches both agents (move_tunnel.go). Cloud firewalls in front of the
//     hosts that admit only HTTPS and SSH do not block it, and no port opens;
//   - "public": the relay on the source's public address with TLS
//     (sslmode=verify-ca, the firewall admits the destination's public
//     addresses), only when the move's plan asks for it
//     (replication_network=public).
//
// A private path the destination cannot reach falls back to the panel.
//
// Tables without a primary key or REPLICA IDENTITY cannot replicate UPDATE
// and DELETE: they stay out of the publication and are copied with the
// dump-based table copy at the bulk copy and again at the final sync. A
// subscription that errors (a schema change on the source during the move,
// a conflict) makes the bulk copy fall back to a fresh dump; at the final
// sync it fails the step with what to do, and the retry uses the dump path.
// Nothing outlives the move on the source: Cleanup drops the subscription,
// the slot, the publication, the role, the relay and the firewall rule, and
// the source agent drops what a dead move left after seven days.

const (
	moveReplicationAuto       = "auto"
	moveReplicationDump       = "dump"
	moveReplicationContinuous = "continuous"

	moveReplicationLabelContinuous = "continuous replication"
	moveReplicationLabelDump       = "dump and restore"
	moveReplicationFinalBasis      = "replication lag"

	moveReplicationNeededSlots = 3 // the subscription's slot and the table-sync workers'

	moveReplicationPathPanel   = "panel"
	moveReplicationPathPrivate = "private"
	moveReplicationPathPublic  = "public"
	// moveReplicationNetworkAuto lets the panel choose (private when
	// reachable, else panel); the plan option may name panel or public.
	moveReplicationNetworkAuto   = "auto"
	moveReplicationNeededSenders = 3
	moveReplicationBulkLimit     = 24 * time.Hour
	moveReplicationFinalLimit    = 30 * time.Minute
)

// MoveReplicationLink is the replication path of one database, as recorded
// with the job: addresses and names, never a credential.
type MoveReplicationLink struct {
	Path  string   `json:"path"` // panel, private or public
	Host  string   `json:"host"`
	Port  int      `json:"port,omitempty"`
	Peers []string `json:"peers"`
	TLS   bool     `json:"tls"`
	// Name is the publication, slot and subscription name.
	Name string `json:"name,omitempty"`
	// Exclude are the tables left out of the publication.
	Exclude []string `json:"exclude,omitempty"`
	// Published and Subscribed record what exists on each end.
	Published  bool `json:"published,omitempty"`
	Subscribed bool `json:"subscribed,omitempty"`
	// Firewall is what the source's host firewall did: opened, inactive or
	// unmanaged.
	Firewall string `json:"firewall,omitempty"`
	// RetainedBytes is the WAL the source keeps for the slot, as last seen
	// while the move waits; Broken marks a subscription that stopped then.
	RetainedBytes int64 `json:"retained_bytes,omitempty"`
	Broken        bool  `json:"broken,omitempty"`
}

// moveReplicationSummary words a database's mode for logs and the plan;
// via names the network path of a replicated database ("through the
// control panel").
func moveReplicationSummary(mode, reason, via string) string {
	if mode == moveReplicationContinuous {
		if via != "" {
			return moveReplicationLabelContinuous + " " + via
		}
		return moveReplicationLabelContinuous
	}
	if reason != "" {
		return moveReplicationLabelDump + " (" + reason + ")"
	}
	return moveReplicationLabelDump
}

// moveReplicationRoute is one way the subscription can reach the source.
type moveReplicationRoute struct {
	path   string
	listen string   // the relay's address on the source (direct paths)
	peers  []string // who may connect to the relay
	tls    bool
	// label names the path in sentences ("the control panel"); via in the
	// plan's per-database text ("through the control panel").
	label, via string
}

// moveHostPrivate lists a host's private addresses with their networks:
// PrivateNetworks, or the single PrivateIPv4/PrivateNetwork of an older
// agent.
func moveHostPrivate(h agent.MoveHost) []netip.Prefix {
	out := []netip.Prefix{}
	for _, text := range h.PrivateNetworks {
		if prefix, err := netip.ParsePrefix(text); err == nil && prefix.Addr().Is4() {
			out = append(out, prefix)
		}
	}
	if len(out) == 0 && h.PrivateIPv4 != "" {
		ip, err1 := netip.ParseAddr(h.PrivateIPv4)
		network, err2 := netip.ParsePrefix(h.PrivateNetwork)
		if err1 == nil && err2 == nil && network.Contains(ip) {
			out = append(out, netip.PrefixFrom(ip, network.Bits()))
		}
	}
	return out
}

// moveSharedPrivateNetwork finds a private network both hosts are on: the
// source's address there, the destination's, and the network.
func moveSharedPrivateNetwork(source, target agent.MoveHost) (netip.Addr, netip.Addr, netip.Prefix, bool) {
	for _, s := range moveHostPrivate(source) {
		for _, t := range moveHostPrivate(target) {
			if s.Addr() != t.Addr() && s.Contains(t.Addr()) && t.Contains(s.Addr()) {
				return s.Addr(), t.Addr(), s.Masked(), true
			}
		}
	}
	return netip.Addr{}, netip.Addr{}, netip.Prefix{}, false
}

// moveReplicationPublicRoute is the relay on the source's public address.
func moveReplicationPublicRoute(source, target agent.MoveHost) (moveReplicationRoute, bool) {
	if source.IPv4 == "" {
		return moveReplicationRoute{}, false
	}
	peers := []string{}
	for _, address := range []string{target.IPv4, target.IPv6} {
		if ip, err := netip.ParseAddr(address); err == nil {
			peers = append(peers, ip.String())
		}
	}
	if len(peers) == 0 {
		return moveReplicationRoute{}, false
	}
	label := "the public internet through a TLS relay on " + source.IPv4 + " that accepts " + strings.Join(peers, " and ") + " only"
	return moveReplicationRoute{path: moveReplicationPathPublic, listen: source.IPv4, peers: peers, tls: true, label: label, via: "over " + label}, true
}

// moveReplicationRoutes lists the paths to try, in order: a shared private
// network first (the move probes it before using it), then the control
// panel; the public relay only when the plan asks for it.
func moveReplicationRoutes(source, target agent.MoveHost, network string) []moveReplicationRoute {
	if network == moveReplicationPathPublic {
		if route, ok := moveReplicationPublicRoute(source, target); ok {
			return []moveReplicationRoute{route}
		}
		return nil
	}
	routes := []moveReplicationRoute{}
	if network != moveReplicationPathPanel {
		if listen, peer, shared, ok := moveSharedPrivateNetwork(source, target); ok {
			label := "the private network " + shared.String()
			routes = append(routes, moveReplicationRoute{path: moveReplicationPathPrivate, listen: listen.String(), peers: []string{peer.String()}, label: label, via: "over " + label})
		}
	}
	if source.Tunnel && target.Tunnel {
		routes = append(routes, moveReplicationRoute{path: moveReplicationPathPanel, label: "the control panel", via: "through the control panel"})
	}
	return routes
}

// moveReplicationDecision chooses a database's copy mode from the source's
// readiness, the destination's abilities and the operator's options.
func moveReplicationDecision(d agent.MoveSourceDatabase, source, target agent.MoveHost, option, network string) (mode, reason string, restart bool) {
	if option == moveReplicationDump {
		return moveReplicationDump, "dump and restore chosen for this move", false
	}
	r := d.Replication
	switch {
	case r == nil || !r.Supported:
		return moveReplicationDump, "the source agent does not report logical replication; upgrade it", false
	case !target.Subscribe:
		return moveReplicationDump, "the destination agent cannot subscribe; upgrade it", false
	case r.Reason != "":
		return moveReplicationDump, r.Reason, false
	case d.Tables > 0 && len(r.NoIdentity) >= d.Tables:
		return moveReplicationDump, "no table has a primary key or replica identity", false
	case r.FreeSlots < moveReplicationNeededSlots:
		return moveReplicationDump, fmt.Sprintf("only %d free replication slots on the source (%d needed; raise max_replication_slots)", r.FreeSlots, moveReplicationNeededSlots), false
	case r.FreeSenders < moveReplicationNeededSenders:
		return moveReplicationDump, fmt.Sprintf("only %d free WAL senders on the source (%d needed; raise max_wal_senders)", r.FreeSenders, moveReplicationNeededSenders), false
	}
	if len(moveReplicationRoutes(source, target, network)) == 0 {
		switch {
		case network == moveReplicationPathPublic:
			return moveReplicationDump, "no public path from the destination to the source is known (the source reports no public IPv4 address)", false
		case !source.Tunnel || !target.Tunnel:
			return moveReplicationDump, "the agents cannot replicate through the control panel and share no private network; upgrade them", false
		}
		return moveReplicationDump, "no network path from the destination to the source is known", false
	}
	return moveReplicationContinuous, "", r.RestartNeeded
}

// planMoveReplication records a database's mode in the plan with its
// warnings and notes.
func planMoveReplication(plan *MovePlan, entry *MovePlanDatabase, d agent.MoveSourceDatabase, source, target agent.MoveHost, option, network, sourceName, targetName string) {
	entry.Replication, entry.ReplicationReason, entry.ReplicationRestart = moveReplicationDecision(d, source, target, option, network)
	if entry.Replication != moveReplicationContinuous {
		return
	}
	routes := moveReplicationRoutes(source, target, network)
	first := routes[0]
	entry.ReplicationPath, entry.ReplicationVia = first.path, first.via
	// The plan lists the database's mode and then its note: the note
	// carries the path ("continuous replication · through the control
	// panel").
	entry.Note = strings.Join(slices.DeleteFunc([]string{first.via, entry.Note}, func(s string) bool { return s == "" }), "; ")
	switch first.path {
	case moveReplicationPathPanel:
		plan.Notes = append(plan.Notes, fmt.Sprintf("Database %s: %s. The destination's PostgreSQL connects to a local port of the agent on %s, and the control panel carries each connection over its own channels to both agents; no firewall port opens on either host.", d.ID, moveReplicationSummary(entry.Replication, "", first.via), targetName))
	case moveReplicationPathPrivate:
		fallback := "."
		if len(routes) > 1 {
			fallback = "; if the destination cannot reach it there, the move replicates through the control panel instead."
		}
		plan.Notes = append(plan.Notes, fmt.Sprintf("Database %s: %s. The source's host firewall admits the destination's address to the relay port for the move only%s", d.ID, moveReplicationSummary(entry.Replication, "", first.via), fallback))
	default:
		plan.Notes = append(plan.Notes, fmt.Sprintf("Database %s replicates continuously to the destination over %s; the source's host firewall admits the destination's address to the relay port for the move only.", d.ID, first.label))
	}
	if entry.ReplicationRestart {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("Prepare restarts the PostgreSQL database %s on %s once to set wal_level=logical (a few seconds of refused connections; the setting is written with ALTER SYSTEM and the database container or cluster service restarts). Databases created from now on start with wal_level=logical.", d.ID, sourceName))
	}
	if r := d.Replication; r != nil && len(r.NoIdentity) > 0 {
		plan.Notes = append(plan.Notes, fmt.Sprintf("Tables of %s without a primary key or replica identity (%s) cannot replicate updates and deletes; they are copied with a data dump at the bulk copy and again at the final sync.", d.ID, strings.Join(r.NoIdentity, ", ")))
	}
	if !slices.ContainsFunc(plan.Warnings, func(w string) bool { return strings.HasPrefix(w, "Schema changes on the source") }) {
		plan.Warnings = append(plan.Warnings, "Schema changes on the source (migrations of a deploy, manual DDL) while the move runs break its replication; the move then copies that database with a fresh dump instead, and a break during the locked final sync fails it with what to do. Deploy the same commit on both sides.")
		plan.Notes = append(plan.Notes, fmt.Sprintf("While the move waits (for DNS, or paused after a failure) %s keeps the WAL the replication slot has not sent; a long pause can fill its disk. Finish or abort the move promptly; a slot left behind is dropped after 7 days.", sourceName))
	}
}

// moveFinalSyncNote words the plan's final-sync note for its database modes.
func moveFinalSyncNote(plan MovePlan) string {
	replicated := false
	for _, d := range plan.Databases {
		replicated = replicated || d.Replication == moveReplicationContinuous
	}
	if replicated {
		return "Replicated databases follow the source continuously; dumped databases and files are copied again by the final sync during the cutover, with the source in maintenance."
	}
	return "Writes made on the source after the data copy are not on the destination. Use the final sync during the cutover to copy them with the source in maintenance."
}

// ---- LSNs ---------------------------------------------------------------------------

// parsePGLSN reads a PostgreSQL LSN ("16/B374D848") as a number.
func parsePGLSN(text string) (uint64, bool) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(text), "/")
	if !ok {
		return 0, false
	}
	high, err1 := strconv.ParseUint(hi, 16, 32)
	low, err2 := strconv.ParseUint(lo, 16, 32)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return high<<32 | low, true
}

// moveReplicationCaughtUp reports whether a subscription has every table
// ready and applied everything the publisher had written at the moment its
// position was read. detail says what is still missing.
func moveReplicationCaughtUp(publisher agent.MoveReplicationStatus, subscriber agent.MoveSubscriptionStatus) (bool, string) {
	if !subscriber.Exists {
		return false, "the subscription does not exist on the destination"
	}
	if subscriber.Total == 0 {
		return false, "the subscription has no tables yet"
	}
	if subscriber.Ready < subscriber.Total {
		return false, fmt.Sprintf("%d of %d tables synchronised", subscriber.Ready, subscriber.Total)
	}
	want, ok := parsePGLSN(publisher.CurrentLSN)
	if !ok {
		return false, "the source's position is unknown"
	}
	got, ok := parsePGLSN(subscriber.LatestEndLSN)
	if !ok {
		return false, "the destination has not applied anything yet"
	}
	if got < want {
		return false, fmt.Sprintf("%s behind the source (%s applied of %s)", moveSize(int64(want-got)), subscriber.LatestEndLSN, publisher.CurrentLSN)
	}
	return true, ""
}

// moveReplicationBroken reports a subscription that stopped applying.
func moveReplicationBroken(status agent.MoveSubscriptionStatus, idlePolls int) string {
	switch {
	case status.ApplyErrors > 0 || status.SyncErrors > 0:
		if status.LastError != "" {
			return "the subscription reports errors: " + status.LastError
		}
		return fmt.Sprintf("the subscription reports %d apply and %d table-sync errors", status.ApplyErrors, status.SyncErrors)
	case status.Exists && !status.Enabled:
		return "the subscription is disabled on the destination"
	case status.Exists && status.Workers == 0 && idlePolls > 20:
		if status.LastError != "" {
			return "no replication worker runs on the destination: " + status.LastError
		}
		return "no replication worker runs on the destination (the source's relay may be unreachable)"
	}
	return ""
}

// ---- prepare --------------------------------------------------------------------------

// movePrepareReplication restarts the source databases whose plan asked
// for wal_level=logical, once; a database that still cannot replicate
// afterwards takes the dump path with the reason logged.
func (c *Control) movePrepareReplication(ctx context.Context, job MoveJob) error {
	for i, d := range job.Databases {
		if !d.Copyable || d.Replication != moveReplicationContinuous || !d.ReplicationRestart {
			continue
		}
		c.moveLog(job.ID, moveStepPrepare, "Setting wal_level=logical on %s and restarting it once on %s, as acknowledged in the plan.", d.SourceID, job.SourceServer)
		var readiness agent.MoveReplicationReadiness
		err := c.moveCall(ctx, 6*time.Minute, job.SourceServer, http.MethodPost, "/v1/move/databases/"+url.PathEscape(d.SourceID)+"/replication/restart", nil, &readiness)
		reason := ""
		switch {
		case err != nil:
			class, _, _ := classifyMoveError(err)
			if class == moveClassTransient {
				return moveOn(job.SourceServer, fmt.Errorf("the restart of %s for wal_level=logical did not finish: %w", d.SourceID, err))
			}
			reason = "the source could not be restarted for wal_level=logical: " + moveAgentError(err)
		case readiness.RestartNeeded:
			reason = "wal_level is still " + readiness.WALLevel + " after the restart"
		case readiness.Reason != "":
			reason = readiness.Reason
		}
		if _, err := c.updateMove(job.ID, func(job *MoveJob) error {
			job.Databases[i].ReplicationRestart = false
			if reason != "" {
				job.Databases[i].Replication, job.Databases[i].ReplicationReason = moveReplicationDump, reason
				job.logf(moveStepPrepare, "Database %s is copied with %s instead: %s.", d.SourceID, moveReplicationLabelDump, reason)
			} else {
				job.logf(moveStepPrepare, "Database %s restarted with wal_level=logical.", d.SourceID)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// ---- engine -----------------------------------------------------------------------------

// moveReplicationExclude lists the tables kept out of the publication: the
// ones left behind and the ones without a replica identity.
func moveReplicationExclude(d MoveDatabase) []string {
	out := slices.Clone(d.ReplicationExcluded)
	for _, table := range d.Excluded {
		if table.Choice == moveTableLeave && !slices.Contains(out, table.Name) {
			out = append(out, table.Name)
		}
	}
	slices.Sort(out)
	return out
}

// moveReplicationDumpTables lists the tables the dump-based table copy
// carries for a replicated database: without a replica identity, and not
// left behind.
func moveReplicationDumpTables(d MoveDatabase) []string {
	out := []string{}
	for _, table := range d.ReplicationExcluded {
		if slices.Contains(moveExcludedNames(d, moveTableLeave), table) {
			continue
		}
		out = append(out, table)
	}
	slices.Sort(out)
	return out
}

// moveReplicationHosts reads both hosts' addresses for the path decision.
func (c *Control) moveReplicationHosts(ctx context.Context, job MoveJob) (agent.MoveHost, agent.MoveHost, error) {
	var source, target agent.MoveHost
	if err := c.moveCall(ctx, 30*time.Second, job.SourceServer, http.MethodGet, "/v1/move/host", nil, &source); err != nil {
		return source, target, moveOn(job.SourceServer, fmt.Errorf("the source's addresses could not be read: %w", err))
	}
	if err := c.moveCall(ctx, 30*time.Second, job.TargetServer, http.MethodGet, "/v1/move/host", nil, &target); err != nil {
		return source, target, moveOn(job.TargetServer, fmt.Errorf("the destination's addresses could not be read: %w", err))
	}
	return source, target, nil
}

// moveReplicationFallbackError marks a replication problem the dump path
// can work around.
type moveReplicationFallbackError struct{ reason string }

func (e *moveReplicationFallbackError) Error() string { return e.reason }

func moveFallback(format string, args ...any) error {
	return &moveReplicationFallbackError{reason: fmt.Sprintf(format, args...)}
}

// moveReplicationEnsure publishes the database on the source and
// subscribes the destination, idempotently: a retried attempt rotates the
// credential and keeps the publication and subscription. The private path
// is tried first when both hosts share a network; a destination that cannot
// reach the source there replicates through the control panel instead.
func (c *Control) moveReplicationEnsure(ctx context.Context, run moveRun, i int) (MoveReplicationLink, error) {
	job := run.job()
	d := job.Databases[i]
	sourceHost, targetHost, err := c.moveReplicationHosts(ctx, job)
	if err != nil {
		return MoveReplicationLink{}, err
	}
	exclude := moveReplicationExclude(d)
	if d.Link != nil && d.Link.Published && !slices.Equal(d.Link.Exclude, exclude) {
		return MoveReplicationLink{}, moveFallback("the tables kept out of the publication changed after it was created (%s → %s)", moveKeyList(d.Link.Exclude), moveKeyList(exclude))
	}
	routes := moveReplicationRoutes(sourceHost, targetHost, job.Plan.ReplicationNetwork)
	if len(routes) == 0 {
		return MoveReplicationLink{}, moveFallback("no network path from %s to %s is known", job.TargetServer, job.SourceServer)
	}
	if d.Link != nil && d.Link.Path == moveReplicationPathPanel {
		// An earlier attempt replicated through the panel: the private path
		// was unreachable then.
		routes = slices.DeleteFunc(routes, func(r moveReplicationRoute) bool { return r.path == moveReplicationPathPrivate })
		if len(routes) == 0 {
			return MoveReplicationLink{}, moveFallback("the destination cannot reach the source over the private network and the agents cannot replicate through the control panel")
		}
	}
	for k, route := range routes {
		link, err := c.moveReplicationLink(ctx, run, i, route, exclude)
		var unreachable *moveReplicationUnreachable
		if errors.As(err, &unreachable) && k+1 < len(routes) {
			run.log("The destination cannot reach the source over %s (%s); replicating through %s instead.", route.label, unreachable.detail, routes[k+1].label)
			continue
		}
		if errors.As(err, &unreachable) {
			return MoveReplicationLink{}, moveFallback("the destination cannot reach the source's replication endpoint over %s: %s", route.label, unreachable.detail)
		}
		return link, err
	}
	return MoveReplicationLink{}, moveFallback("no network path from %s to %s worked", job.TargetServer, job.SourceServer)
}

type moveReplicationUnreachable struct{ detail string }

func (e *moveReplicationUnreachable) Error() string { return e.detail }

// moveReplicationLink sets one path up on both ends and records it.
func (c *Control) moveReplicationLink(ctx context.Context, run moveRun, i int, route moveReplicationRoute, exclude []string) (MoveReplicationLink, error) {
	job := run.job()
	d := job.Databases[i]
	if d.Link == nil || d.Link.Path != route.path {
		run.log("Replication of %s connects over %s.", d.SourceID, route.label)
	}
	panel := route.path == moveReplicationPathPanel
	request := agent.MoveReplicationRequest{Move: job.ID, Listen: route.listen, Peers: route.peers, TLS: route.tls, Exclude: exclude}
	if panel {
		request = agent.MoveReplicationRequest{Move: job.ID, Exclude: exclude, Path: moveReplicationPathPanel, StreamToken: c.moveTunnelToken(job.ID, "source", d.SourceID)}
	}
	var endpoint agent.MoveReplicationEndpoint
	if err := c.moveCall(ctx, 5*time.Minute, job.SourceServer, http.MethodPost, "/v1/move/databases/"+url.PathEscape(d.SourceID)+"/replication", request, &endpoint); err != nil {
		class, _, _ := classifyMoveError(err)
		if class == moveClassTransient {
			return MoveReplicationLink{}, moveOn(job.SourceServer, fmt.Errorf("the source could not publish %s: %w", d.SourceID, err))
		}
		return MoveReplicationLink{}, moveFallback("the source could not publish %s: %s", d.SourceID, moveAgentError(err))
	}
	link := MoveReplicationLink{Path: route.path, Host: endpoint.Host, Port: endpoint.Port, Peers: route.peers, TLS: route.tls, Name: endpoint.Publication, Exclude: exclude, Published: true, Firewall: endpoint.Firewall}
	if panel {
		link.Peers = []string{}
	}
	if _, err := c.updateMove(run.id, func(job *MoveJob) error {
		recorded := link
		job.Databases[i].Link = &recorded
		if d.Link == nil || !d.Link.Published || d.Link.Path != route.path {
			if panel {
				job.logf(run.step, "Published %s on %s as %s (%d tables%s); the control panel connects its streams to the database there (no relay port, no firewall rule).", d.SourceID, job.SourceServer, endpoint.Publication, endpoint.Tables, moveExcludeNote(exclude))
			} else {
				job.logf(run.step, "Published %s on %s as %s (%d tables%s); the relay listens on %s:%d for %s (host firewall: %s).", d.SourceID, job.SourceServer, endpoint.Publication, endpoint.Tables, moveExcludeNote(exclude), endpoint.Host, endpoint.Port, strings.Join(route.peers, ", "), endpoint.Firewall)
			}
		}
		return nil
	}); err != nil {
		return link, err
	}
	switch route.path {
	case moveReplicationPathPrivate:
		// The destination opens a TCP connection to the relay first: a
		// network or a cloud firewall that does not carry the private path
		// shows here within seconds.
		var reach agent.MoveReachResult
		err := c.moveCall(ctx, 30*time.Second, job.TargetServer, http.MethodPost, "/v1/move/reach", agent.MoveReachRequest{Host: endpoint.Host, Port: endpoint.Port, TimeoutSeconds: 5}, &reach)
		switch {
		case err == nil && !reach.Reachable:
			return link, &moveReplicationUnreachable{detail: reach.Error}
		case err != nil && !moveReplicationGone(err):
			return link, moveOn(job.TargetServer, fmt.Errorf("the destination could not probe the source's relay: %w", err))
		}
	case moveReplicationPathPanel:
		var tunnel agent.MoveTunnelEndpoint
		if err := c.moveCall(ctx, time.Minute, job.TargetServer, http.MethodPost, "/v1/move/databases/"+url.PathEscape(d.TargetID)+"/subscription/tunnel", agent.MoveTunnelRequest{Move: job.ID, Token: c.moveTunnelToken(job.ID, "destination", d.TargetID)}, &tunnel); err != nil {
			class, _, _ := classifyMoveError(err)
			if class == moveClassTransient {
				return link, moveOn(job.TargetServer, fmt.Errorf("the destination could not open the replication tunnel of %s: %w", d.TargetID, err))
			}
			return link, moveFallback("the destination could not open the replication tunnel of %s: %s", d.TargetID, moveAgentError(err))
		}
		link.Host, link.Port, link.Firewall = tunnel.Host, tunnel.Port, tunnel.Firewall
		if _, err := c.updateMove(run.id, func(job *MoveJob) error {
			recorded := link
			job.Databases[i].Link = &recorded
			if d.Link == nil || d.Link.Path != route.path || d.Link.Host != tunnel.Host || d.Link.Port != tunnel.Port {
				job.logf(run.step, "The destination's PostgreSQL connects to %s:%d on %s; the control panel keeps %d idle streams open to it and opens a stream to the source's database for each connection.", tunnel.Host, tunnel.Port, job.TargetServer, moveTunnelIdleLegs)
			}
			return nil
		}); err != nil {
			return link, err
		}
		t := c.moveTunnelStart(job.ID, job.SourceServer, job.TargetServer, d.SourceID, d.TargetID)
		if err := t.waitReady(ctx, moveTunnelReadyWait); err != nil {
			return link, moveOn(job.TargetServer, moveTransientError("tunnel", err))
		}
		// The local hop needs no TLS: the bytes travel the panel's own
		// authenticated agent channels, never an open port.
		endpoint.Host, endpoint.Port, endpoint.SSLMode, endpoint.CA = tunnel.Host, tunnel.Port, "disable", ""
	}
	var status agent.MoveSubscriptionStatus
	subscribe := agent.MoveSubscriptionRequest{Move: job.ID, Endpoint: endpoint}
	err := c.moveCall(ctx, 5*time.Minute, job.TargetServer, http.MethodPost, "/v1/move/databases/"+url.PathEscape(d.TargetID)+"/subscription", subscribe, &status)
	endpoint.Password = ""
	if err != nil {
		if strings.Contains(moveAgentError(err), "cannot reach the source") {
			detail := moveAgentError(err)
			if panel {
				if tunnelErr := c.moveTunnelError(job.ID, d.SourceID); tunnelErr != "" {
					detail += " (" + tunnelErr + ")"
				}
			}
			return link, &moveReplicationUnreachable{detail: detail}
		}
		class, _, _ := classifyMoveError(err)
		if class == moveClassTransient {
			return link, moveOn(job.TargetServer, fmt.Errorf("the destination could not subscribe %s: %w", d.TargetID, err))
		}
		return link, moveFallback("the destination could not subscribe %s: %s", d.TargetID, moveAgentError(err))
	}
	link.Subscribed = true
	_, err = c.updateMove(run.id, func(job *MoveJob) error {
		recorded := link
		job.Databases[i].Link = &recorded
		if d.Link == nil || !d.Link.Subscribed {
			job.logf(run.step, "%s subscribed %s to the source's publication; the initial copy of %d tables runs on the destination.", job.TargetServer, d.TargetID, status.Total)
		}
		return nil
	})
	return link, err
}

func moveExcludeNote(exclude []string) string {
	if len(exclude) == 0 {
		return ""
	}
	return "; kept out: " + strings.Join(exclude, ", ")
}

// moveReplicationTeardown drops the subscription on the destination and
// the publication, slot, role, relay and firewall rule on the source. It
// is idempotent and keeps going past a failing end, reporting the first
// error.
func (c *Control) moveReplicationTeardown(ctx context.Context, run moveRun, i int) error {
	job := run.job()
	d := job.Databases[i]
	var failures []error
	if d.TargetID != "" {
		if err := c.moveCall(ctx, 5*time.Minute, job.TargetServer, http.MethodDelete, "/v1/move/databases/"+url.PathEscape(d.TargetID)+"/subscription?move="+url.QueryEscape(job.ID), nil, nil); err != nil && !moveReplicationGone(err) {
			failures = append(failures, moveOn(job.TargetServer, fmt.Errorf("the subscription of %s could not be dropped: %w", d.TargetID, err)))
		}
	}
	// The panel's streams end once the subscription is gone (the
	// destination closed its listener with it).
	c.moveTunnelStop(job.ID, d.SourceID)
	if err := c.moveCall(ctx, 5*time.Minute, job.SourceServer, http.MethodDelete, "/v1/move/databases/"+url.PathEscape(d.SourceID)+"/replication", nil, nil); err != nil && !moveReplicationGone(err) {
		failures = append(failures, moveOn(job.SourceServer, fmt.Errorf("the publication of %s could not be removed: %w", d.SourceID, err)))
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	_, err := c.updateMove(run.id, func(job *MoveJob) error {
		if job.Databases[i].Link != nil {
			job.Databases[i].Link = nil
			job.logf(run.step, "Removed the replication of %s: subscription on %s; publication, slot, role, relay and firewall rule on %s.", d.SourceID, job.TargetServer, job.SourceServer)
		}
		return nil
	})
	return err
}

// moveReplicationGone reports an agent that has no such route or database:
// nothing to tear down there.
func moveReplicationGone(err error) bool {
	var remote *agentHTTPError
	return errors.As(err, &remote) && remote.status == http.StatusNotFound
}

// moveReplicationFallback switches a database to the dump path for the
// rest of the move and copies it that way now.
func (c *Control) moveReplicationFallback(ctx context.Context, run moveRun, i int, cp moveCheckpointRef, reason string) error {
	job := run.job()
	d := job.Databases[i]
	run.log("Database %s cannot replicate continuously: %s. Copying it with %s instead, for this and every later copy.", d.SourceID, reason, moveReplicationLabelDump)
	if err := c.moveReplicationTeardown(ctx, run, i); err != nil {
		return err
	}
	if _, err := c.updateMove(run.id, func(job *MoveJob) error {
		job.Databases[i].Replication, job.Databases[i].ReplicationReason = moveReplicationDump, reason
		return job.setCheckpointMeta(cp.key, movePostgresCheckpoint{})
	}); err != nil {
		return err
	}
	return c.moveCopyDatabase(ctx, run, i, cp)
}

// moveReplicateDatabase is the bulk copy of a replicated database: the
// schema, the publication and subscription, the initial table copy and
// the dump copy of the tables without a replica identity.
func (c *Control) moveReplicateDatabase(ctx context.Context, run moveRun, i int, cp moveCheckpointRef) error {
	job := run.job()
	d := job.Databases[i]
	var count struct {
		Tables int `json:"tables"`
	}
	if err := c.moveCall(ctx, time.Minute, job.SourceServer, http.MethodGet, "/v1/move/databases/"+url.PathEscape(d.SourceID)+"/tables", nil, &count); err != nil {
		return moveOn(job.SourceServer, fmt.Errorf("database %s could not be measured: %w", d.SourceID, err))
	}
	if count.Tables == 0 {
		_, err := c.updateMove(run.id, func(job *MoveJob) error {
			job.Databases[i].Skipped = "no tables"
			job.logf(run.step, "Database %s has no tables; nothing to copy.", d.SourceID)
			return nil
		})
		return err
	}
	if d.TargetID == "" {
		return moveOperatorError("destination_unknown", fmt.Sprintf("the destination database for %s is unknown", d.SourceID), "Retry the move from Prepare destination, or abort it.")
	}
	var saved movePostgresCheckpoint
	cp.Meta(&saved)
	if exclude := moveReplicationExclude(d); d.Link != nil && d.Link.Published && !slices.Equal(d.Link.Exclude, exclude) {
		// The operator's choices changed after the publication was made:
		// the tables must be copied anew, so the replication and the schema
		// start over.
		run.log("The tables kept out of the publication of %s changed (%s → %s); setting the replication up again from the schema.", d.SourceID, moveKeyList(d.Link.Exclude), moveKeyList(exclude))
		if err := c.moveReplicationTeardown(ctx, run, i); err != nil {
			return err
		}
		saved = movePostgresCheckpoint{}
		if _, err := c.updateMove(run.id, func(job *MoveJob) error { return job.setCheckpointMeta(cp.key, saved) }); err != nil {
			return err
		}
	}
	if saved.Schema == "" {
		digest, err := c.moveCopySchema(ctx, run, i)
		if err != nil {
			var fallback *moveReplicationFallbackError
			if errors.As(err, &fallback) {
				return c.moveReplicationFallback(ctx, run, i, cp, fallback.reason)
			}
			return err
		}
		saved.Schema = digest
		if _, err := c.updateMove(run.id, func(job *MoveJob) error { return job.setCheckpointMeta(cp.key, saved) }); err != nil {
			return err
		}
	} else {
		run.log("The schema of %s was restored into %s before this attempt (dump %s…); not restoring it again.", d.SourceID, d.TargetID, saved.Schema[:12])
	}
	if !saved.Replicated {
		if _, err := c.moveReplicationEnsure(ctx, run, i); err != nil {
			var fallback *moveReplicationFallbackError
			if errors.As(err, &fallback) {
				return c.moveReplicationFallback(ctx, run, i, cp, fallback.reason)
			}
			return err
		}
		if err := c.moveReplicationWatch(ctx, run, i, moveReplicationBulkLimit, false); err != nil {
			var fallback *moveReplicationFallbackError
			if errors.As(err, &fallback) {
				return c.moveReplicationFallback(ctx, run, i, cp, fallback.reason)
			}
			return err
		}
		saved.Replicated = true
		if _, err := c.updateMove(run.id, func(job *MoveJob) error {
			job.Databases[i].Copied, job.Databases[i].Skipped = "replication", ""
			job.logf(run.step, "The initial copy of %s into %s on %s finished; every table is synchronised and the destination now follows the source continuously.", d.SourceID, d.TargetID, job.TargetServer)
			return job.setCheckpointMeta(cp.key, saved)
		}); err != nil {
			return err
		}
	} else {
		run.log("The initial copy of %s finished before this attempt; the destination follows the source.", d.SourceID)
	}
	return c.moveCopyNoIdentityTables(ctx, run, i, cp, &saved)
}

// moveReplicationFinalSync waits, with the source frozen, until the
// destination applied everything, copies the sequences and the tables
// without a replica identity, then drops the subscription.
func (c *Control) moveReplicationFinalSync(ctx context.Context, run moveRun, i int, cp moveCheckpointRef) error {
	job := run.job()
	d := job.Databases[i]
	if d.TargetID == "" {
		return moveOperatorError("destination_unknown", fmt.Sprintf("the destination database for %s is unknown", d.SourceID), "Retry the move from Prepare destination, or abort it.")
	}
	var saved movePostgresCheckpoint
	cp.Meta(&saved)
	if job.FinalSynced && d.Link == nil && d.Copied == "replication" {
		// A final sync ran to its end before: the replication was torn down
		// after it and the frozen source cannot have changed since.
		run.log("%s already has every transaction of the frozen source from the earlier final sync; nothing to copy again.", d.TargetID)
		return nil
	}
	if !saved.Replicated {
		var status agent.MoveSubscriptionStatus
		if err := c.moveCall(ctx, time.Minute, job.TargetServer, http.MethodGet, "/v1/move/databases/"+url.PathEscape(d.TargetID)+"/subscription?move="+url.QueryEscape(job.ID), nil, &status); err != nil {
			return moveOn(job.TargetServer, fmt.Errorf("the subscription of %s could not be read: %w", d.TargetID, err))
		}
		if !status.Exists || d.Link == nil {
			return c.moveReplicationFinalFallback(ctx, run, i, cp, "the subscription no longer exists on the destination")
		}
		if status.Workers == 0 {
			if _, err := c.moveReplicationEnsure(ctx, run, i); err != nil {
				var fallback *moveReplicationFallbackError
				if errors.As(err, &fallback) {
					return c.moveReplicationFinalFallback(ctx, run, i, cp, fallback.reason)
				}
				return err
			}
		}
		if err := c.moveReplicationWatch(ctx, run, i, moveReplicationFinalLimit, true); err != nil {
			var fallback *moveReplicationFallbackError
			if errors.As(err, &fallback) {
				return c.moveReplicationFinalFallback(ctx, run, i, cp, fallback.reason)
			}
			return err
		}
		if err := c.moveCopySequences(ctx, run, i); err != nil {
			return err
		}
		saved.Replicated = true
		if _, err := c.updateMove(run.id, func(job *MoveJob) error {
			job.Databases[i].Copied = "replication"
			return job.setCheckpointMeta(cp.key, saved)
		}); err != nil {
			return err
		}
	}
	if err := c.moveCopyNoIdentityTables(ctx, run, i, cp, &saved); err != nil {
		return err
	}
	// The destination is complete. Once the framework verified it, the
	// engine's Cleanup drops the subscription and the source's publication,
	// slot, role, relay and firewall rule: the deploy's migrations and the
	// application own the database from there.
	run.log("%s on %s has every transaction of the frozen source; the replication is removed once the verification passed.", d.TargetID, job.TargetServer)
	return nil
}

// moveReplicationFinalFallback fails the locked final sync with what to do
// and makes the retry use the dump path.
func (c *Control) moveReplicationFinalFallback(ctx context.Context, run moveRun, i int, cp moveCheckpointRef, reason string) error {
	job := run.job()
	d := job.Databases[i]
	_ = c.moveReplicationTeardown(ctx, run, i)
	_, _ = c.updateMove(run.id, func(job *MoveJob) error {
		job.Databases[i].Replication, job.Databases[i].ReplicationReason = moveReplicationDump, reason
		return job.setCheckpointMeta(cp.key, movePostgresCheckpoint{})
	})
	return moveOperatorError("replication_broken", fmt.Sprintf("the replication of %s broke during the final sync: %s", d.SourceID, reason),
		"The replication was removed on both ends. Retry the final sync: it copies "+d.SourceID+" with a fresh dump (the application stays locked meanwhile), or abort the move. If a schema change on the source caused it, deploy the same commit on both sides first.")
}

// moveReplicationWatch follows the subscription until the initial copy is
// done (bulk) or the destination caught up with the frozen source (final),
// with the step's live progress.
func (c *Control) moveReplicationWatch(ctx context.Context, run moveRun, i int, limit time.Duration, caughtUp bool) error {
	job := run.job()
	d := job.Databases[i]
	interval := c.movePollInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	deadline := time.Now().Add(limit)
	idle := 0
	for {
		if link := run.job().Databases[i].Link; link != nil && link.Path == moveReplicationPathPanel {
			// After a panel restart the runner resumes here: the tunnel
			// resumes with it.
			c.moveTunnelStart(job.ID, job.SourceServer, job.TargetServer, d.SourceID, d.TargetID)
		}
		var publisher agent.MoveReplicationStatus
		if err := c.moveCall(ctx, time.Minute, job.SourceServer, http.MethodGet, "/v1/move/databases/"+url.PathEscape(d.SourceID)+"/replication", nil, &publisher); err != nil {
			return moveOn(job.SourceServer, fmt.Errorf("the source's replication state of %s could not be read: %w", d.SourceID, err))
		}
		var subscriber agent.MoveSubscriptionStatus
		if err := c.moveCall(ctx, time.Minute, job.TargetServer, http.MethodGet, "/v1/move/databases/"+url.PathEscape(d.TargetID)+"/subscription?move="+url.QueryEscape(job.ID), nil, &subscriber); err != nil {
			return moveOn(job.TargetServer, fmt.Errorf("the subscription of %s could not be read: %w", d.TargetID, err))
		}
		if subscriber.Workers == 0 {
			idle++
		} else {
			idle = 0
		}
		if broken := moveReplicationBroken(subscriber, idle); broken != "" {
			if tunnelErr := c.moveTunnelError(job.ID, d.SourceID); tunnelErr != "" {
				broken += " (replication stream through the control panel: " + tunnelErr + ")"
			}
			return moveFallback("%s", broken)
		}
		if !publisher.Relay && idle > 2 {
			// The source agent restarted: its relay is gone; set it up again.
			if _, err := c.moveReplicationEnsure(ctx, run, i); err != nil {
				return err
			}
		}
		done, detail := moveReplicationCaughtUp(publisher, subscriber)
		if !caughtUp {
			done = subscriber.Total > 0 && subscriber.Ready == subscriber.Total
			if !done && subscriber.Total > 0 {
				detail = fmt.Sprintf("%d of %d tables synchronised", subscriber.Ready, subscriber.Total)
			}
		}
		if done {
			return nil
		}
		phase := fmt.Sprintf("initial copy of %s on %s: %s", d.SourceID, job.TargetServer, detail)
		if caughtUp {
			phase = fmt.Sprintf("waiting for %s on %s to catch up: %s", d.SourceID, job.TargetServer, detail)
		}
		run.stage(phase, 0, nil)
		if time.Now().After(deadline) {
			return moveTransientError("timeout", fmt.Errorf("the replication of %s did not %s within %s (%s)", d.SourceID, map[bool]string{false: "finish its initial copy", true: "catch up"}[caughtUp], moveDuration(limit), detail))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// moveCopySchema restores a schema-only dump of the source into the
// destination database (replacing it) and returns the dump's digest.
func (c *Control) moveCopySchema(ctx context.Context, run moveRun, i int) (string, error) {
	job := run.job()
	d := job.Databases[i]
	run.log("Dumping the schema of %s on %s (tables, sequences, extensions; no rows).", d.SourceID, job.SourceServer)
	run.stage(fmt.Sprintf("dumping the schema of %s on %s", d.SourceID, job.SourceServer), 0, nil)
	var export agent.MoveTableDataExport
	if err := c.moveCall(ctx, 30*time.Minute, job.SourceServer, http.MethodPost, "/v1/move/databases/"+url.PathEscape(d.SourceID)+"/schema", nil, &export); err != nil {
		if moveReplicationGone(err) {
			return "", moveFallback("the source agent cannot dump a schema on its own; upgrade it")
		}
		return "", moveOn(job.SourceServer, fmt.Errorf("the schema of %s could not be dumped: %w", d.SourceID, err))
	}
	if export.Size <= 0 || export.Size > moveMaxDumpBytes || !moveDigestPattern.MatchString(export.SHA256) || !moveDigestPattern.MatchString(export.ID) {
		return "", fmt.Errorf("the schema dump of %s has no usable size or digest", d.SourceID)
	}
	err := c.moveRelay(ctx, job, moveRelayStream{
		download: "/v1/move/databases/" + url.PathEscape(d.SourceID) + "/table-data/" + url.PathEscape(export.ID),
		upload:   "/v1/move/databases/" + url.PathEscape(d.TargetID) + "/restore?replace=1",
		size:     export.Size, digest: export.SHA256, mismatch: errMoveChecksum, what: "schema dump", resume: moveUploadID(job, "schema/"+d.SourceID, export.SHA256),
		phase: fmt.Sprintf("streaming the schema of %s to %s", d.SourceID, job.TargetServer),
		after: fmt.Sprintf("restoring the schema of %s into %s on %s", d.SourceID, d.TargetID, job.TargetServer),
	}, nil)
	if err != nil {
		return "", fmt.Errorf("restoring the schema of %s failed: %w", d.SourceID, err)
	}
	run.log("Restored the schema of %s into %s on %s (%s, SHA-256 %s… matched on both ends).", d.SourceID, d.TargetID, job.TargetServer, moveSize(export.Size), export.SHA256[:12])
	return export.SHA256, nil
}

// moveCopySequences sets every sequence on the destination to the frozen
// source's position.
func (c *Control) moveCopySequences(ctx context.Context, run moveRun, i int) error {
	job := run.job()
	d := job.Databases[i]
	var publisher agent.MoveReplicationStatus
	if err := c.moveCall(ctx, time.Minute, job.SourceServer, http.MethodGet, "/v1/move/databases/"+url.PathEscape(d.SourceID)+"/replication?sequences=1", nil, &publisher); err != nil {
		return moveOn(job.SourceServer, fmt.Errorf("the sequences of %s could not be read: %w", d.SourceID, err))
	}
	if len(publisher.Sequences) == 0 {
		run.log("Database %s has no sequences to carry over.", d.SourceID)
		return nil
	}
	if err := c.moveCall(ctx, 5*time.Minute, job.TargetServer, http.MethodPost, "/v1/move/databases/"+url.PathEscape(d.TargetID)+"/subscription/sequences", agent.MoveSequenceRequest{Sequences: publisher.Sequences}, nil); err != nil {
		return moveOn(job.TargetServer, fmt.Errorf("the sequences of %s could not be set: %w", d.TargetID, err))
	}
	run.log("Set %d sequences of %s on %s to the source's positions.", len(publisher.Sequences), d.TargetID, job.TargetServer)
	return nil
}

// moveCopyNoIdentityTables copies the tables kept out of the publication
// for lack of a replica identity with a data-only dump, once per phase,
// verified by row brackets and checkpointed.
func (c *Control) moveCopyNoIdentityTables(ctx context.Context, run moveRun, i int, cp moveCheckpointRef, saved *movePostgresCheckpoint) error {
	job := run.job()
	d := job.Databases[i]
	tables := moveReplicationDumpTables(d)
	if len(tables) == 0 {
		return nil
	}
	if loaded := saved.NoIdentity; loaded != nil && slices.Equal(loaded.Tables, tables) && loaded.SHA256 != "" {
		run.log("The data of %s (no replica identity) was loaded into %s before this attempt (%s verified); not loading it again.", moveKeyList(tables), d.TargetID, moveTableRowsList(loaded.Rows))
		return nil
	}
	run.log("Copying %s with a data dump: without a primary key or replica identity these tables cannot replicate updates and deletes.", moveKeyList(tables))
	run.stage(fmt.Sprintf("dumping the tables without a replica identity of %s on %s", d.SourceID, job.SourceServer), 0, nil)
	source := "/v1/move/databases/" + url.PathEscape(d.SourceID)
	stop := c.moveWatchDatabase(ctx, job.SourceServer, d.SourceID, d.SourceID+" (tables without a replica identity)", run.id)
	var export agent.MoveTableDataExport
	err := c.moveCall(ctx, 6*time.Hour, job.SourceServer, http.MethodPost, source+"/table-data", agent.MoveTableDataRequest{Tables: tables}, &export)
	stop()
	if err != nil {
		return moveOn(job.SourceServer, fmt.Errorf("the tables without a replica identity of %s could not be dumped: %w", d.SourceID, err))
	}
	if export.Size <= 0 || export.Size > moveMaxDumpBytes || !moveDigestPattern.MatchString(export.SHA256) || !moveDigestPattern.MatchString(export.ID) {
		return fmt.Errorf("the table dump of %s has no usable size or digest", d.SourceID)
	}
	brackets := moveRowBrackets(export, saved.Schema)
	var restored agent.MoveTableRows
	err = c.moveRelay(ctx, job, moveRelayStream{
		download: source + "/table-data/" + url.PathEscape(export.ID),
		upload:   "/v1/move/databases/" + url.PathEscape(d.TargetID) + "/table-data/restore?tables=" + url.QueryEscape(strings.Join(tables, ",")),
		size:     export.Size, digest: export.SHA256, mismatch: errMoveChecksum, what: "table dump", resume: moveUploadID(job, cp.key+"/no-identity", export.SHA256),
		phase: fmt.Sprintf("streaming the tables without a replica identity of %s to %s", d.SourceID, job.TargetServer),
		after: fmt.Sprintf("loading the tables without a replica identity of %s on %s", d.SourceID, job.TargetServer),
	}, &restored)
	if err != nil {
		return fmt.Errorf("copying the tables without a replica identity of %s failed: %w", d.SourceID, err)
	}
	if err := moveCheckRowBrackets(d.SourceID, tables, brackets, restored.Rows); err != nil {
		return err
	}
	saved.NoIdentity = &moveTablesCheckpoint{Dump: saved.Schema, Export: export.ID, SHA256: export.SHA256, Tables: tables, Rows: restored.Rows}
	_, err = c.updateMove(run.id, func(job *MoveJob) error {
		job.logf(run.step, "Copied %s into %s on %s with a data dump; the row counts match.", moveTableRowsList(restored.Rows), d.TargetID, job.TargetServer)
		return job.setCheckpointMeta(cp.key, *saved)
	})
	return err
}

// moveReplicatedSavings is the measured bulk time of the databases that
// replicate continuously, which the locked sync does not repeat.
func moveReplicatedSavings(job MoveJob) float64 {
	saved := 0.0
	for _, d := range job.Databases {
		if d.Replication != moveReplicationContinuous {
			continue
		}
		if cp, ok := job.Checkpoints[moveCheckpointKey(movePhaseBulk, moveEnginePostgres, d.SourceID)]; ok && cp.Done != nil && cp.Seconds > moveEngineOverhead {
			saved += cp.Seconds - moveEngineOverhead
		}
	}
	return saved
}

// moveReplicationKeepalive follows the replication of a move that waits at
// the cutover: each status read renews the publication's lifetime on the
// source, and a subscription that broke meanwhile is logged once, so the
// operator learns before the final sync that it will copy a dump instead.
func (c *Control) moveReplicationKeepalive(ctx context.Context, id string) {
	job, ok := c.moveJob(id)
	if !ok || job.State != moveStateCutover && job.State != moveStateReady {
		return
	}
	for i, d := range job.Databases {
		if d.Link == nil || !d.Link.Subscribed || d.Replication != moveReplicationContinuous {
			continue
		}
		if d.Link.Path == moveReplicationPathPanel {
			c.moveTunnelStart(job.ID, job.SourceServer, job.TargetServer, d.SourceID, d.TargetID)
		}
		var publisher agent.MoveReplicationStatus
		if err := c.moveCall(ctx, 30*time.Second, job.SourceServer, http.MethodGet, "/v1/move/databases/"+url.PathEscape(d.SourceID)+"/replication", nil, &publisher); err != nil {
			continue
		}
		var subscriber agent.MoveSubscriptionStatus
		if err := c.moveCall(ctx, 30*time.Second, job.TargetServer, http.MethodGet, "/v1/move/databases/"+url.PathEscape(d.TargetID)+"/subscription?move="+url.QueryEscape(job.ID), nil, &subscriber); err != nil {
			continue
		}
		broken := moveReplicationBroken(subscriber, 0)
		if !subscriber.Exists {
			broken = "the subscription no longer exists on the destination"
		}
		_, _ = c.updateMove(id, func(job *MoveJob) error {
			link := job.Databases[i].Link
			if link == nil {
				return errMoveUnchanged
			}
			link.RetainedBytes = publisher.RetainedBytes
			if broken != "" && !link.Broken {
				link.Broken = true
				job.logf(moveStepCutover, "The replication of %s stopped while the move waits (%s); the final sync will copy it with a fresh dump instead.", d.SourceID, broken)
			}
			return nil
		})
	}
}
