package control

// Authoritative DNS for the cutover check. A move waits at "Switch DNS"
// until every hostname resolves to the destination. The host's resolver and
// public resolvers answer from their caches for up to the record's TTL, so
// a record the operator has already changed can look unchanged for an hour.
// The cutover check therefore asks the zone's own nameservers, which answer
// with the current record the moment it is saved, and falls back to public
// resolvers and then the host when the zone cannot be queried (for example
// a CNAME into another zone, or a nameserver that does not answer).

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// movePublicNameservers answer NS and fallback lookups; they are tried in
// turn so one unreachable resolver does not delay a check.
var movePublicNameservers = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}

// moveAuthoritativeLookupTimeout bounds one query to one nameserver.
const moveAuthoritativeLookupTimeout = 2 * time.Second

// moveResolverSource is implemented by resolvers that can say which server
// answered a hostname lookup; the cutover view shows it.
type moveResolverSource interface {
	LookupIPAddrFrom(ctx context.Context, host string) ([]net.IPAddr, string, error)
}

// authoritativeResolver is the default move resolver.
type authoritativeResolver struct {
	public *net.Resolver
	system *net.Resolver
	next   atomic.Uint32
}

func newAuthoritativeResolver() *authoritativeResolver {
	r := &authoritativeResolver{system: net.DefaultResolver}
	r.public = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		server := movePublicNameservers[int(r.next.Add(1)-1)%len(movePublicNameservers)]
		return (&net.Dialer{Timeout: moveAuthoritativeLookupTimeout}).DialContext(ctx, network, server)
	}}
	return r
}

// resolverFor queries one nameserver directly.
func resolverFor(server string) *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: moveAuthoritativeLookupTimeout}).DialContext(ctx, network, server)
	}}
}

func (r *authoritativeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	addresses, _, err := r.LookupIPAddrFrom(ctx, host)
	return addresses, err
}

// LookupIPAddrFrom resolves host at its zone's nameservers and names the
// one that answered; "public resolver" or "host resolver" when it fell back.
func (r *authoritativeResolver) LookupIPAddrFrom(ctx context.Context, host string) ([]net.IPAddr, string, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, ns := range r.zoneNameservers(ctx, host) {
		lookup, cancel := context.WithTimeout(ctx, moveAuthoritativeLookupTimeout)
		addresses, err := resolverFor(net.JoinHostPort(ns.address, "53")).LookupIPAddr(lookup, host)
		cancel()
		if err == nil && len(addresses) > 0 {
			return addresses, ns.name, nil
		}
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		// A CNAME into another zone or an empty answer: the public
		// resolvers follow it.
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			break
		}
	}
	if addresses, err := r.public.LookupIPAddr(ctx, host); err == nil {
		return addresses, "public resolver", nil
	} else if ctx.Err() != nil {
		return nil, "", err
	}
	addresses, err := r.system.LookupIPAddr(ctx, host)
	return addresses, "host resolver", err
}

type moveNameserver struct{ name, address string }

// zoneNameservers finds the nameservers of the closest enclosing zone of
// host (host itself, then its parents down to the registrable domain) and
// resolves up to three of them. Each lookup goes to a public resolver; NS
// records change rarely, so their caching does not matter here.
func (r *authoritativeResolver) zoneNameservers(ctx context.Context, host string) []moveNameserver {
	labels := strings.Split(host, ".")
	for i := 0; i < len(labels)-1; i++ {
		zone := strings.Join(labels[i:], ".")
		lookup, cancel := context.WithTimeout(ctx, moveAuthoritativeLookupTimeout)
		records, err := r.public.LookupNS(lookup, zone)
		cancel()
		if err != nil || len(records) == 0 {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		out := []moveNameserver{}
		for _, record := range records {
			if len(out) == 3 {
				break
			}
			name := strings.TrimSuffix(record.Host, ".")
			lookup, cancel := context.WithTimeout(ctx, moveAuthoritativeLookupTimeout)
			addresses, err := r.public.LookupIPAddr(lookup, name)
			cancel()
			if err != nil || len(addresses) == 0 {
				continue
			}
			out = append(out, moveNameserver{name: name, address: addresses[0].IP.String()})
		}
		return out
	}
	return nil
}

func (r *authoritativeResolver) LookupAddr(ctx context.Context, address string) ([]string, error) {
	if names, err := r.public.LookupAddr(ctx, address); err == nil {
		return names, nil
	}
	return r.system.LookupAddr(ctx, address)
}

func (r *authoritativeResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	if records, err := r.public.LookupTXT(ctx, name); err == nil {
		return records, nil
	}
	return r.system.LookupTXT(ctx, name)
}

func (r *authoritativeResolver) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	if records, err := r.public.LookupMX(ctx, name); err == nil {
		return records, nil
	}
	return r.system.LookupMX(ctx, name)
}
