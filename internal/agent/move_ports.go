package agent

import (
	"context"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Published ports in a move. The source reports the ports its current release
// publishes (the stack's app services, as public_ports.go opens them in the
// Docker firewall chain); a destination reports the ports already bound on
// its public addresses, so the control panel can tell before the move starts
// that a published port would collide there.

// movePublicPorts lists the public ports of a release, deduplicated and
// sorted; never nil, so the control panel can tell an agent that reports
// ports from one that predates the field.
func movePublicPorts(r *Release) []StackPort {
	out := []StackPort{}
	for _, entry := range releasePortRegistrations(r) {
		for _, p := range entry.Ports {
			port := StackPort{Port: p.Port, Protocol: strings.ToLower(p.Protocol)}
			if port.Protocol == "" {
				port.Protocol = "tcp"
			}
			if port.Port > 0 && !slices.Contains(out, port) {
				out = append(out, port)
			}
		}
	}
	sortStackPorts(out)
	return out
}

func sortStackPorts(ports []StackPort) {
	slices.SortFunc(ports, func(a, b StackPort) int {
		if a.Port != b.Port {
			return a.Port - b.Port
		}
		return strings.Compare(a.Protocol, b.Protocol)
	})
}

// ListeningPort is a port bound on the host's public addresses with the
// name of the process that holds it ("" when ss did not name one). Port and
// protocol read as a StackPort, so older control panels still decode it.
type ListeningPort struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Process  string `json:"process,omitempty"`
}

// Stack is the port without its process.
func (p ListeningPort) Stack() StackPort { return StackPort{Port: p.Port, Protocol: p.Protocol} }

// moveListeningPorts lists the host's listening sockets bound to a wildcard
// or a non-loopback address, as port/protocol with the owning process. It
// returns nil when the sockets cannot be listed, so the control panel knows
// the answer is unknown rather than empty.
func moveListeningPorts(ctx context.Context) []ListeningPort {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tcp, err := hostCommand(ctx, "ss", "-H", "-l", "-t", "-n", "-p")
	if err != nil {
		return nil
	}
	udp, err := hostCommand(ctx, "ss", "-H", "-l", "-u", "-n", "-p")
	if err != nil {
		return nil
	}
	return parseListeningPorts(tcp, udp)
}

// ssProcess is the first process name of ss -p's users:(("name",pid=..)).
var ssProcess = regexp.MustCompile(`users:\(\("([^"]*)"`)

// parseListeningPorts reads the output of ss -Hltnp and ss -Hlunp (State,
// Recv-Q, Send-Q, Local Address:Port, Peer Address:Port, Process). Loopback
// and link-local binds are left out; wildcard and every other address
// count, since a port published for a container binds every address. The
// process column is optional (ss without -p, or without the privilege to
// see other users' sockets).
func parseListeningPorts(tcp, udp string) []ListeningPort {
	out := []ListeningPort{}
	for _, proto := range []string{"tcp", "udp"} {
		text := tcp
		if proto == "udp" {
			text = udp
		}
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			local := fields[3]
			i := strings.LastIndex(local, ":")
			if i < 0 {
				continue
			}
			address := strings.Trim(strings.Split(local[:i], "%")[0], "[]")
			if ip := net.ParseIP(address); ip != nil && (ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
				continue
			}
			port, err := strconv.Atoi(local[i+1:])
			if err != nil || port <= 0 {
				continue
			}
			process := ""
			if match := ssProcess.FindStringSubmatch(line); match != nil {
				process = match[1]
			}
			j := slices.IndexFunc(out, func(p ListeningPort) bool { return p.Port == port && p.Protocol == proto })
			if j < 0 {
				out = append(out, ListeningPort{Port: port, Protocol: proto, Process: process})
			} else if out[j].Process == "" {
				out[j].Process = process
			}
		}
	}
	slices.SortFunc(out, func(a, b ListeningPort) int {
		if a.Port != b.Port {
			return a.Port - b.Port
		}
		return strings.Compare(a.Protocol, b.Protocol)
	})
	return out
}

// moveHostFirewall reports the managed host firewall's state for the move
// plan: "active", "inactive" or "" when unknown. Container ports bypass it
// (public_ports.go); it matters for processes that bind host ports directly.
func moveHostFirewall(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	host, err := readUfwHost(ctx)
	if err != nil || !host.Installed {
		if err == nil {
			return "inactive"
		}
		return ""
	}
	if host.Active {
		return "active"
	}
	return "inactive"
}
