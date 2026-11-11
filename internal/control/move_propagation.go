package control

// DNS propagation before the source stops serving. The cutover check asks
// the zone's own nameservers (move_dns.go), which return a changed record
// at once; resolvers that cached the old record keep returning the old
// address for up to its TTL. Retiring the source as soon as the
// authoritative answer changed sent every visitor behind such a resolver to
// a server that no longer served (the panel move's 502s). So the move
// records when the authoritative servers first returned the destination
// and the longest TTL the record had, and the source stops serving only
// once the public resolvers (1.1.1.1, 8.8.8.8, 9.9.9.9) all return the
// destination, or the old TTL plus a margin has passed since the change,
// whichever comes first.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// movePropagationMargin is added to the old TTL.
	movePropagationMargin = 5 * time.Minute
	// movePropagationDefaultTTL stands in for a TTL that could not be read.
	movePropagationDefaultTTL = time.Hour
)

// MoveDNSPropagation tracks cached DNS after the authoritative change.
type MoveDNSPropagation struct {
	// OldTTL is the longest TTL (seconds) the zone's nameservers gave the
	// hostnames' address records, from the cutover on.
	OldTTL int `json:"old_ttl,omitempty"`
	// Changed is when the zone's nameservers first returned only the
	// destination.
	Changed *time.Time `json:"changed,omitempty"`
	// Resolvers are the public resolvers' last answers.
	Resolvers []MoveResolverAnswer `json:"resolvers,omitempty"`
	Checked   *time.Time           `json:"checked,omitempty"`
	// Complete: cached DNS no longer sends visitors to the source.
	Complete bool   `json:"complete,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// Logged is when a move last logged the wait, so it is not logged at
	// every check.
	Logged *time.Time `json:"logged,omitempty"`
}

// MoveResolverAnswer is one public resolver's cached answer for a hostname.
type MoveResolverAnswer struct {
	Resolver  string   `json:"resolver"`
	Hostname  string   `json:"hostname"`
	Addresses []string `json:"addresses"`
	// TTL is the remaining cache lifetime in seconds.
	TTL   int    `json:"ttl"`
	Ready bool   `json:"ready"`
	Error string `json:"error,omitempty"`
}

// moveCacheResolver reads TTLs: the authoritative TTL of a hostname's
// address records and the public resolvers' cached answers. The default
// resolver implements it; tests replace it.
type moveCacheResolver interface {
	AuthoritativeTTL(ctx context.Context, host string) (time.Duration, error)
	PublicAnswers(ctx context.Context, host string) []MoveResolverAnswer
}

// cacheResolver is nil when the move resolver cannot read TTLs (a test
// fake); propagation then counts as complete with the authoritative change.
func (c *Control) cacheResolver() moveCacheResolver {
	if c.moveCaches != nil {
		return c.moveCaches
	}
	if caches, ok := c.resolver().(moveCacheResolver); ok {
		return caches
	}
	return nil
}

// trackPropagation updates the propagation of hostnames toward addresses;
// pointed reports that the zone's nameservers return only the destination.
// A record that points elsewhere again restarts the wait.
func (c *Control) trackPropagation(ctx context.Context, previous *MoveDNSPropagation, hostnames, addresses []string, pointed bool, now time.Time) *MoveDNSPropagation {
	p := &MoveDNSPropagation{}
	if previous != nil {
		*p = *previous
		p.Resolvers = slices.Clone(previous.Resolvers)
	}
	if !pointed {
		p.Changed, p.Complete, p.Detail, p.Resolvers = nil, false, "", nil
	}
	if pointed && p.Complete {
		return p
	}
	checked := now
	p.Checked = &checked
	caches := c.cacheResolver()
	if caches == nil {
		if pointed {
			if p.Changed == nil {
				p.Changed = &checked
			}
			p.Complete, p.Detail = true, "Cached DNS was not measured."
		}
		return p
	}
	// The record's TTL before the change counts; read until the change
	// shows and keep the longest.
	for _, host := range hostnames {
		lookup, cancel := context.WithTimeout(ctx, 6*time.Second)
		ttl, err := caches.AuthoritativeTTL(lookup, host)
		cancel()
		if err == nil && int(ttl.Seconds()) > p.OldTTL {
			p.OldTTL = int(ttl.Seconds())
		}
	}
	if !pointed {
		return p
	}
	if p.Changed == nil {
		p.Changed = &checked
	}
	p.Resolvers = []MoveResolverAnswer{}
	for _, host := range hostnames {
		lookup, cancel := context.WithTimeout(ctx, 8*time.Second)
		answers := caches.PublicAnswers(lookup, host)
		cancel()
		for _, answer := range answers {
			answer.Hostname = host
			answer.Ready = answer.Error == "" && len(answer.Addresses) > 0
			for _, address := range answer.Addresses {
				if !slices.Contains(addresses, address) {
					answer.Ready = false
				}
			}
			p.Resolvers = append(p.Resolvers, answer)
		}
	}
	p.Complete, p.Detail = evaluatePropagation(*p, now)
	return p
}

// evaluatePropagation decides whether cached DNS still sends visitors to
// the source, and says how long that is expected to last.
func evaluatePropagation(p MoveDNSPropagation, now time.Time) (bool, string) {
	if p.Changed == nil {
		return false, "Waiting for DNS to point to the destination."
	}
	ttl := time.Duration(p.OldTTL) * time.Second
	if ttl <= 0 {
		ttl = movePropagationDefaultTTL
	}
	deadline := p.Changed.Add(ttl + movePropagationMargin)
	answered := 0
	stale := []MoveResolverAnswer{}
	for _, answer := range p.Resolvers {
		if answer.Error != "" {
			continue
		}
		answered++
		if !answer.Ready {
			stale = append(stale, answer)
		}
	}
	if answered > 0 && len(stale) == 0 {
		return true, "Every public resolver that answered returns the destination."
	}
	if !now.Before(deadline) {
		return true, fmt.Sprintf("The old record's TTL (%s) and a %s margin have passed since DNS changed.", moveMinutes(ttl), moveMinutes(movePropagationMargin))
	}
	left := deadline.Sub(now)
	if len(stale) == 0 {
		return false, fmt.Sprintf("Waiting for cached DNS to expire: about %s left (no public resolver answered; waiting out the old record's TTL of %s).", moveMinutes(left), moveMinutes(ttl))
	}
	longest := 0
	names := []string{}
	for _, answer := range stale {
		longest = max(longest, answer.TTL)
		name := answer.Resolver
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if cached := time.Duration(longest)*time.Second + time.Minute; longest > 0 && cached < left {
		left = cached
	}
	which := "resolver " + names[0] + " still returns"
	if len(names) > 1 {
		which = "resolvers " + strings.Join(names, ", ") + " still return"
	}
	old := strings.Join(stale[0].Addresses, ", ")
	return false, fmt.Sprintf("Waiting for cached DNS to expire: about %s left (%s the old address %s for %s).", moveMinutes(left), which, old, stale[0].Hostname)
}

// moveMinutes renders a duration in whole minutes, rounded up.
func moveMinutes(d time.Duration) string {
	n := int(math.Ceil(d.Minutes()))
	if n <= 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", n)
}

// ---- TTLs from the wire ---------------------------------------------------------

// AuthoritativeTTL returns the longest TTL of host's A and AAAA records
// (and a CNAME leading to them) at the zone's own nameservers.
func (r *authoritativeResolver) AuthoritativeTTL(ctx context.Context, host string) (time.Duration, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	var lastErr error = errors.New("no nameserver of the zone answered")
	for _, ns := range r.zoneNameservers(ctx, host) {
		best, answered := uint32(0), false
		for _, qtype := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
			_, ttl, err := dnsQuery(ctx, net.JoinHostPort(ns.address, "53"), host, qtype, false)
			if err != nil {
				lastErr = err
				continue
			}
			answered = true
			best = max(best, ttl)
		}
		if answered {
			return time.Duration(best) * time.Second, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
	}
	return 0, lastErr
}

// PublicAnswers asks each public resolver for host's addresses and the
// time they stay cached there.
func (r *authoritativeResolver) PublicAnswers(ctx context.Context, host string) []MoveResolverAnswer {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	out := []MoveResolverAnswer{}
	for _, server := range movePublicNameservers {
		name, _, _ := net.SplitHostPort(server)
		answer := MoveResolverAnswer{Resolver: name, Addresses: []string{}}
		failures := 0
		for _, qtype := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
			addresses, ttl, err := dnsQuery(ctx, server, host, qtype, true)
			if err != nil {
				failures++
				answer.Error = err.Error()
				continue
			}
			answer.TTL = max(answer.TTL, int(ttl))
			for _, address := range addresses {
				if !slices.Contains(answer.Addresses, address) {
					answer.Addresses = append(answer.Addresses, address)
				}
			}
		}
		if failures < 2 {
			answer.Error = ""
		}
		slices.Sort(answer.Addresses)
		out = append(out, answer)
	}
	return out
}

// dnsQuery sends one question over UDP and returns the answer's addresses
// of the asked type and the longest TTL in the answer section.
func dnsQuery(ctx context.Context, server, host string, qtype dnsmessage.Type, recursive bool) ([]string, uint32, error) {
	name, err := dnsmessage.NewName(host + ".")
	if err != nil {
		return nil, 0, err
	}
	var idBytes [2]byte
	_, _ = rand.Read(idBytes[:])
	id := binary.BigEndian.Uint16(idBytes[:])
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: id, RecursionDesired: recursive},
		Questions: []dnsmessage.Question{{Name: name, Type: qtype, Class: dnsmessage.ClassINET}}}
	packet, err := query.Pack()
	if err != nil {
		return nil, 0, err
	}
	lookup, cancel := context.WithTimeout(ctx, moveAuthoritativeLookupTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(lookup, "udp", server)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	if deadline, ok := lookup.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(packet); err != nil {
		return nil, 0, err
	}
	buffer := make([]byte, 4096)
	for {
		n, err := conn.Read(buffer)
		if err != nil {
			return nil, 0, err
		}
		var reply dnsmessage.Message
		if reply.Unpack(buffer[:n]) != nil || reply.Header.ID != id || !reply.Header.Response {
			continue
		}
		if reply.Header.RCode != dnsmessage.RCodeSuccess && reply.Header.RCode != dnsmessage.RCodeNameError {
			return nil, 0, fmt.Errorf("%s answered %s", server, reply.Header.RCode)
		}
		addresses := []string{}
		ttl := uint32(0)
		for _, answer := range reply.Answers {
			ttl = max(ttl, answer.Header.TTL)
			var raw []byte
			switch body := answer.Body.(type) {
			case *dnsmessage.AResource:
				if qtype == dnsmessage.TypeA {
					raw = body.A[:]
				}
			case *dnsmessage.AAAAResource:
				if qtype == dnsmessage.TypeAAAA {
					raw = body.AAAA[:]
				}
			}
			if ip, ok := netip.AddrFromSlice(raw); ok {
				addresses = append(addresses, ip.Unmap().String())
			}
		}
		return addresses, ttl, nil
	}
}
