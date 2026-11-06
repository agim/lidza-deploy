package agent

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// NetworkInfo holds the detected public IPv4 and IPv6 addresses.
type NetworkInfo struct {
	IPv4 string
	IPv6 string
	// PrivateIPv4 is the preferred private (RFC 1918) IPv4 address, with
	// PrivateNetwork its interface's network in CIDR notation: two hosts
	// whose private addresses lie in each other's network may share a VPC.
	PrivateIPv4    string
	PrivateNetwork string
	// PrivateNetworks are every private IPv4 address of the host's own
	// interfaces with its prefix length ("10.30.0.5/16"), in interface
	// order, without container bridges and cloud anchor addresses. The
	// control panel matches them against a peer's to find a network both
	// hosts share.
	PrivateNetworks []string
}

// interfaceAddresses is one interface's state as DetectAddresses reads it.
type interfaceAddresses struct {
	Name     string
	Up       bool
	Loopback bool
	Addrs    []net.Addr
}

// DetectAddresses inspects all non-loopback network interfaces and returns
// the first usable global-unicast IPv4 and IPv6 addresses found, and the
// host's private IPv4 networks.
func DetectAddresses() NetworkInfo {
	ifaces, err := net.Interfaces()
	if err != nil {
		return NetworkInfo{}
	}
	list := make([]interfaceAddresses, 0, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		list = append(list, interfaceAddresses{Name: iface.Name, Up: iface.Flags&net.FlagUp != 0, Loopback: iface.Flags&net.FlagLoopback != 0, Addrs: addrs})
	}
	return selectAddresses(list)
}

// virtualInterface reports bridges and links of container runtimes and
// hypervisors on this host: their private networks never reach another
// host.
func virtualInterface(name string) bool {
	for _, prefix := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "cali", "vxlan", "lxc", "lxd", "podman", "kube"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// doAnchorNetwork is DigitalOcean's anchor network: every droplet carries
// an address in it on its public interface (for reserved IPs), but it is
// never routed between droplets.
var doAnchorNetwork = netip.MustParsePrefix("10.10.0.0/16")

// selectAddresses picks the public addresses and the private networks from
// the interfaces. A private address in DigitalOcean's anchor network on an
// interface that also carries a public address is skipped whenever another
// private address exists: the VPC address (eth1) is the one peers reach.
func selectAddresses(ifaces []interfaceAddresses) NetworkInfo {
	info := NetworkInfo{}
	type private struct {
		addr   netip.Addr
		prefix netip.Prefix
		anchor bool
	}
	privates := []private{}
	for _, iface := range ifaces {
		// Skip loopback and interfaces that are down.
		if iface.Loopback || !iface.Up {
			continue
		}
		public := false
		local := []private{}
		for _, addr := range iface.Addrs {
			var ip net.IP
			var mask net.IPMask
			switch v := addr.(type) {
			case *net.IPNet:
				ip, mask = v.IP, v.Mask
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				if ip4.IsPrivate() {
					if mask == nil || virtualInterface(iface.Name) {
						continue
					}
					ones, bits := mask.Size()
					if bits != 32 {
						continue
					}
					a, _ := netip.AddrFromSlice(ip4)
					local = append(local, private{addr: a, prefix: netip.PrefixFrom(a, ones)})
					continue
				}
				public = true
				if info.IPv4 == "" {
					info.IPv4 = ip4.String()
				}
			} else if ip.To16() != nil && ip.IsGlobalUnicast() {
				if info.IPv6 == "" {
					info.IPv6 = ip.String()
				}
			}
		}
		for _, p := range local {
			p.anchor = public && doAnchorNetwork.Contains(p.addr) && p.prefix.Bits() >= doAnchorNetwork.Bits()
			privates = append(privates, p)
		}
	}
	routable := 0
	for _, p := range privates {
		if !p.anchor {
			routable++
		}
	}
	for _, p := range privates {
		if p.anchor && routable > 0 {
			continue
		}
		info.PrivateNetworks = append(info.PrivateNetworks, p.prefix.String())
		if info.PrivateIPv4 == "" {
			info.PrivateIPv4 = p.addr.String()
			info.PrivateNetwork = p.prefix.Masked().String()
		}
	}
	return info
}

// FormatAddrPort formats an IP address and port into a valid listen address
// string that works for both IPv4 and IPv6.
func FormatAddrPort(host string, port int) string {
	ip := net.ParseIP(host)
	if ip != nil && ip.To4() == nil && ip.To16() != nil {
		// IPv6 — brackets required.
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}
