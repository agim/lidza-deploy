package control

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Published ports in a move plan. The source agent reports the host ports its
// current release publishes; the destination agent reports the ports already
// bound there. A collision is a blocker: the destination's deploy would fail
// to publish the port. Otherwise the plan says which ports the destination
// publishes and that its agent opens them itself (the agent's Docker firewall
// chain; the host firewall does not filter container ports).

func movePortLabel(p agent.StackPort) string {
	proto := p.Protocol
	if proto == "" {
		proto = "tcp"
	}
	return strconv.Itoa(p.Port) + "/" + proto
}

func movePortLabels(ports []agent.StackPort) string {
	labels := make([]string, 0, len(ports))
	for _, p := range ports {
		labels = append(labels, movePortLabel(p))
	}
	return strings.Join(labels, ", ")
}

func movePortsEqual(a, b agent.StackPort) bool {
	return a.Port == b.Port && movePortLabel(a) == movePortLabel(b)
}

// MovePlanHostMail is the destination's host mail server that Prepare binds
// to loopback so the application can publish its mail ports there.
type MovePlanHostMail struct {
	MTA      string            `json:"mta"`
	Ports    []agent.StackPort `json:"ports"`
	Loopback bool              `json:"loopback"`
}

// planMovePorts adds the port checks to the plan: a blocker per published
// port already bound on the destination, else a note listing the ports the
// destination publishes. A port held by the destination's own Postfix that
// served nobody outside the host in the last 7 days is a warning instead:
// Prepare binds that Postfix to loopback. Agents that predate port
// reporting get a note.
func planMovePorts(plan *MovePlan, source agent.MoveSource, host agent.MoveHost) {
	if source.PublicPorts == nil {
		plan.Notes = append(plan.Notes, "The source agent does not report published ports; upgrade it so the move checks the destination's ports and the application's mail settings.")
		return
	}
	if len(source.PublicPorts) == 0 {
		return
	}
	ports := slices.Clone(source.PublicPorts)
	plan.PublicPorts = ports
	if plan.TargetRuntime == moveRuntimeNative {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("The application publishes port %s on the source; native applications listen on a loopback port behind the proxy, so the destination will not publish them.", movePortLabels(ports)))
		if host.HostFirewall == "active" {
			plan.Notes = append(plan.Notes, "The destination's host firewall is active; it allows only the system ports and the rules reviewed in the panel.")
		}
		return
	}
	// A mail port published without public_tls answers STARTTLS without
	// the primary hostname's certificate; the move carries that over.
	if source.PublicTLS != nil && !*source.PublicTLS {
		if warning := agent.MailTLSWarning(ports); warning != "" {
			plan.Warnings = append(plan.Warnings, warning)
		}
	}
	if host.ListeningPorts == nil {
		plan.Notes = append(plan.Notes, fmt.Sprintf("The destination publishes port %s; its agent does not report bound ports, so a collision would only show at the deploy. Upgrade the destination agent to check beforehand.", movePortLabels(ports)))
		return
	}
	var free, loopback []agent.StackPort
	for _, p := range ports {
		i := slices.IndexFunc(host.ListeningPorts, func(bound agent.ListeningPort) bool { return movePortsEqual(bound.Stack(), p) })
		if i < 0 {
			free = append(free, p)
			continue
		}
		bound := host.ListeningPorts[i]
		if blocker := moveHostMailBlocker(p, bound, host.HostMail); blocker != "" {
			plan.Blockers = append(plan.Blockers, blocker)
			continue
		}
		loopback = append(loopback, p)
		free = append(free, p)
	}
	if len(loopback) > 0 {
		plan.HostMail = &MovePlanHostMail{MTA: "postfix", Ports: loopback, Loopback: true}
		subject := "Port " + movePortLabels(loopback) + " is"
		if len(loopback) > 1 {
			subject = "Ports " + movePortLabels(loopback) + " are"
		}
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s held by the destination's host mail server (Postfix), which only delivers local system mail. Prepare binds it to loopback (inet_interfaces = loopback-only), so local system mail keeps working and the port is free for %s.", subject, plan.AppID))
	}
	if len(free) > 0 {
		note := fmt.Sprintf("The destination publishes port %s; its agent opens %s in the Docker firewall chain at the deploy. The host firewall does not filter container ports, so it needs no change.", movePortLabels(free), movePortPlural(free))
		plan.Notes = append(plan.Notes, note)
	}
}

// moveHostMailBlocker is the blocker for a published port that is bound on
// the destination, or "" when the holder is the destination's Postfix that
// only delivers local system mail (Prepare binds it to loopback).
func moveHostMailBlocker(p agent.StackPort, bound agent.ListeningPort, mail *agent.MoveHostMail) string {
	label := movePortLabel(p)
	held := mail != nil && bound.Process != "" && mail.Process == bound.Process && slices.ContainsFunc(mail.Ports, func(m agent.StackPort) bool { return movePortsEqual(m, p) })
	if !held || mail.MTA != "postfix" {
		if bound.Process == "" {
			return fmt.Sprintf("Port %s is in use on the destination; free it there or move the application elsewhere.", label)
		}
		holder := "process " + bound.Process
		switch {
		case held && mail.MTA == "exim":
			holder = "the host mail server Exim (" + bound.Process + ")"
		case held && mail.MTA == "sendmail":
			holder = "the host mail server Sendmail (" + bound.Process + ")"
		}
		return fmt.Sprintf("Port %s is in use on the destination by %s; free it there or move the application elsewhere.", label, holder)
	}
	var evidence []string
	if mail.ExternalConnections7d > 0 {
		evidence = append(evidence, fmt.Sprintf("accepted %d outside %s", mail.ExternalConnections7d, movePlural(mail.ExternalConnections7d, "connection", "connections")))
	}
	if mail.RelayedOut7d > 0 {
		relay := fmt.Sprintf("sent %d %s to other mail servers", mail.RelayedOut7d, movePlural(mail.RelayedOut7d, "message", "messages"))
		if mail.Relayhost != "" {
			relay += " (relayhost " + mail.Relayhost + ")"
		}
		evidence = append(evidence, relay)
	}
	if len(evidence) > 0 {
		return fmt.Sprintf("Port %s is in use on the destination by its host mail server: Postfix on the destination %s in the last 7 days, so it serves mail beyond the host. Free the port there or move the application elsewhere.", label, strings.Join(evidence, " and "))
	}
	if !mail.LogsRead {
		return fmt.Sprintf("Port %s is in use on the destination by its host mail server (Postfix); its mail log of the last 7 days could not be read, so the panel cannot tell whether it serves mail beyond the host. Free the port there or move the application elsewhere.", label)
	}
	return ""
}

func movePlural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func movePortPlural(ports []agent.StackPort) string {
	if len(ports) == 1 {
		return "it"
	}
	return "them"
}
