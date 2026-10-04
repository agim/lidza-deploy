package agent

import (
	"fmt"
	"net"
)

// NetworkInfo holds the detected public IPv4 and IPv6 addresses.
type NetworkInfo struct {
	IPv4 string
	IPv6 string
}

// DetectAddresses inspects all non-loopback network interfaces and returns
// the first usable global-unicast IPv4 and IPv6 addresses found.
func DetectAddresses() NetworkInfo {
	info := NetworkInfo{}

	ifaces, err := net.Interfaces()
	if err != nil {
		return info
	}

	for _, iface := range ifaces {
		// Skip loopback and interfaces that are down.
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}

			if ip4 := ip.To4(); ip4 != nil {
				if info.IPv4 == "" {
					info.IPv4 = ip4.String()
				}
			} else if ip.To16() != nil && ip.IsGlobalUnicast() {
				if info.IPv6 == "" {
					info.IPv6 = ip.String()
				}
			}
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
