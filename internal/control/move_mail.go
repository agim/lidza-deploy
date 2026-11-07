package control

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"golang.org/x/net/publicsuffix"
)

// Mail-sending applications in a move. An application that publishes a mail
// port (25, 465 or 587/tcp) delivers mail from the server's address, and
// receiving servers judge that address: its reverse DNS must name the
// application and the domain's SPF record must list it. The plan looks both
// up and lists, next to the A/AAAA records, the SPF records to rewrite and
// the PTR to set at the server provider; the cutover checks the PTR again
// and warns, never blocks, since mail is accepted again as soon as it is set.

// MovePlanMail is the mail part of a plan, copied to the job with the plan.
type MovePlanMail struct {
	// Ports are the mail ports the application publishes.
	Ports []agent.StackPort `json:"ports"`
	// SourceAddresses are the source server's public addresses, the ones the
	// SPF records name today.
	SourceAddresses []string `json:"source_addresses"`
	// PTR is the reverse DNS of each destination address at plan time.
	PTR []MovePTRStatus `json:"ptr"`
	// Records are the SPF (TXT) and PTR records to set at the cutover, in
	// addition to the A/AAAA records.
	Records []MoveDNSRecord `json:"records"`
	// OutgoingProvider names the provider the application sends mail
	// through: the destination's reverse DNS does not affect it, so it is
	// neither checked nor listed. Receiving mail is unaffected by this.
	OutgoingProvider string `json:"outgoing_provider,omitempty"`
}

// MovePTRStatus is the reverse DNS of one address.
type MovePTRStatus struct {
	Address string   `json:"address"`
	Names   []string `json:"names"`
	// Want is the name the reverse DNS should carry: the source's reverse
	// DNS when it is one of the application's hostnames, else the primary
	// hostname.
	Want  string `json:"want"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

var moveMailPorts = []int{25, 465, 587}

const (
	moveMailProviderHint = "(At DigitalOcean the reverse DNS is the droplet's name.)"
	moveMailPTRNote      = "Set at the server provider, not at the DNS provider."
	moveMailSPFNote      = "Replaces the SPF record that names the source address."
	moveMailDKIMNote     = "DKIM is unaffected by the address change: its keys live in the application's files, which the move copies, and its DNS records stay as they are."
)

// moveMailPortsOf returns the mail ports among published ports.
func moveMailPortsOf(ports []agent.StackPort) []agent.StackPort {
	var out []agent.StackPort
	for _, p := range ports {
		if slices.Contains(moveMailPorts, p.Port) && (p.Protocol == "" || strings.EqualFold(p.Protocol, "tcp")) {
			out = append(out, p)
		}
	}
	return out
}

// moveApex is the registrable domain of a hostname (shop.example.co.uk →
// example.co.uk), where SPF and MX records usually live.
func moveApex(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if apex, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return apex
	}
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// moveMailNames lists the hostnames and their apexes, each once.
func moveMailNames(hostnames []string) []string {
	out := []string{}
	for _, host := range hostnames {
		for _, name := range []string{strings.ToLower(host), moveApex(host)} {
			if name != "" && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

func (c *Control) resolver() moveResolver {
	if c.moveResolver != nil {
		return c.moveResolver
	}
	return defaultMoveResolver
}

// moveReverseDNS looks up the reverse DNS of each address; want names the
// expected hostname of one.
func (c *Control) moveReverseDNS(ctx context.Context, addresses []string, hostnames []string, want func(address string) string) []MovePTRStatus {
	out := []MovePTRStatus{}
	for _, address := range addresses {
		status := MovePTRStatus{Address: address, Names: []string{}, Want: want(address)}
		lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
		names, err := c.resolver().LookupAddr(lookup, address)
		cancel()
		if err != nil && len(names) == 0 {
			status.Error = "no reverse DNS"
		}
		for _, name := range names {
			name = strings.ToLower(strings.TrimSuffix(name, "."))
			if name == "" || slices.Contains(status.Names, name) {
				continue
			}
			status.Names = append(status.Names, name)
			if moveHostnameOf(name, hostnames) {
				status.OK = true
			}
		}
		out = append(out, status)
	}
	return out
}

func moveHostnameOf(name string, hostnames []string) bool {
	return slices.ContainsFunc(hostnames, func(host string) bool { return strings.EqualFold(strings.TrimSuffix(host, "."), name) })
}

func movePTRWarning(status MovePTRStatus) string {
	current := "none"
	if len(status.Names) > 0 {
		current = strings.Join(status.Names, ", ")
	}
	return fmt.Sprintf("Reverse DNS of %s is %s; set it to %s at the server provider before the cutover, or outgoing mail is rejected. %s", status.Address, current, status.Want, moveMailProviderHint)
}

// planMoveMail adds the mail checks to a plan of an application that
// publishes a mail port: reverse DNS of the destination, SPF and MX of the
// hostnames, and the records to set at the cutover.
func (c *Control) planMoveMail(ctx context.Context, plan *MovePlan, sourceAddresses []string, provider string) {
	ports := moveMailPortsOf(plan.PublicPorts)
	if len(ports) == 0 || len(plan.Hostnames) == 0 {
		return
	}
	mail := &MovePlanMail{Ports: ports, SourceAddresses: slices.Clone(sourceAddresses), PTR: []MovePTRStatus{}, Records: []MoveDNSRecord{}, OutgoingProvider: provider}
	if provider != "" {
		plan.Notes = append(plan.Notes, moveOutgoingProviderNote(provider))
	}
	// The source's reverse DNS is the name to carry over when it is one of
	// the application's hostnames; else the primary hostname.
	want := plan.Hostnames[0]
	for _, status := range c.moveReverseDNS(ctx, sourceAddresses, plan.Hostnames, func(string) string { return want }) {
		if status.OK {
			for _, name := range status.Names {
				if moveHostnameOf(name, plan.Hostnames) {
					want = name
					break
				}
			}
			break
		}
	}
	if provider == "" {
		mail.PTR = c.moveReverseDNS(ctx, plan.Addresses, plan.Hostnames, func(string) string { return want })
	}
	for _, status := range mail.PTR {
		record := MoveDNSRecord{Hostname: status.Address, Type: "PTR", Value: status.Want, Note: moveMailPTRNote}
		if status.OK {
			record.Note = "Already set. " + moveMailPTRNote
		} else {
			plan.Warnings = append(plan.Warnings, movePTRWarning(status))
		}
		mail.Records = append(mail.Records, record)
	}
	for _, name := range moveMailNames(plan.Hostnames) {
		lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
		records, _ := c.resolver().LookupTXT(lookup, name)
		cancel()
		for _, txt := range records {
			if rewritten, changed := moveRewriteSPF(txt, sourceAddresses, plan.Addresses); changed {
				mail.Records = append(mail.Records, MoveDNSRecord{Hostname: name, Type: "TXT", Value: rewritten, Note: moveMailSPFNote})
			}
		}
		lookup, cancel = context.WithTimeout(ctx, 5*time.Second)
		exchangers, _ := c.resolver().LookupMX(lookup, name)
		cancel()
		for _, mx := range exchangers {
			if mx == nil {
				continue
			}
			target := strings.ToLower(strings.TrimSuffix(mx.Host, "."))
			if target == "" || target == "." || moveHostnameOf(target, plan.Hostnames) {
				continue
			}
			warning := fmt.Sprintf("MX of %s points at %s, which is not one of the moved hostnames; it keeps pointing where it does today, so check that incoming mail does not depend on the source server.", name, target)
			if !slices.Contains(plan.Warnings, warning) {
				plan.Warnings = append(plan.Warnings, warning)
			}
		}
	}
	plan.Notes = append(plan.Notes, fmt.Sprintf("The application sends mail (port %s). The cutover lists the SPF and reverse DNS records next to the A/AAAA records. %s", movePortLabels(ports), moveMailDKIMNote))
	plan.Mail = mail
}

// moveOutgoingProviderNote is the plan's note for mail sent through a provider.
func moveOutgoingProviderNote(provider string) string {
	return fmt.Sprintf("Outgoing mail goes through %s; reverse DNS of the destination does not affect it.", provider)
}

// moveRewriteSPF replaces, in an SPF record, every ip4:/ip6: mechanism that
// names a source address with the destination addresses of the same family.
// changed is false for records that are not SPF or name no source address.
func moveRewriteSPF(txt string, sourceAddresses, destinationAddresses []string) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(txt)), "v=spf1") {
		return txt, false
	}
	parse := func(raw []string) []netip.Addr {
		var out []netip.Addr
		for _, a := range raw {
			if ip, err := netip.ParseAddr(a); err == nil {
				out = append(out, ip.Unmap())
			}
		}
		return out
	}
	sources, destinations := parse(sourceAddresses), parse(destinationAddresses)
	out := []string{}
	changed := false
	for _, token := range strings.Fields(txt) {
		qualifier, rest := "", token
		if len(rest) > 0 && strings.ContainsRune("+-~?", rune(rest[0])) {
			qualifier, rest = rest[:1], rest[1:]
		}
		mechanism, value, ok := strings.Cut(rest, ":")
		mechanism = strings.ToLower(mechanism)
		if !ok || mechanism != "ip4" && mechanism != "ip6" {
			out = append(out, token)
			continue
		}
		address, prefix, _ := strings.Cut(value, "/")
		ip, err := netip.ParseAddr(address)
		single := prefix == "" || (ip.Is4() && prefix == "32") || (ip.Is6() && prefix == "128")
		if err != nil || !single || !slices.Contains(sources, ip.Unmap()) {
			// A destination address the record names already is kept once.
			if !slices.Contains(out, token) {
				out = append(out, token)
			}
			continue
		}
		changed = true
		for _, destination := range destinations {
			if destination.Is4() != ip.Is4() {
				continue
			}
			replacement := qualifier + mechanism + ":" + destination.String()
			if !slices.Contains(out, replacement) {
				out = append(out, replacement)
			}
		}
	}
	rewritten := strings.Join(out, " ")
	if rewritten == txt {
		return txt, false
	}
	return rewritten, changed
}

// moveMailRecords are the mail records of a job's plan, after the A/AAAA
// records; none for applications that send no mail.
func moveMailRecords(job MoveJob) []MoveDNSRecord {
	if job.Plan.Mail == nil {
		return nil
	}
	return slices.Clone(job.Plan.Mail.Records)
}

// checkMovePTR looks up the reverse DNS of the destination again during the
// cutover of a mail-sending application and records it on the job. It
// never holds the cutover: the operator sets it at the provider, and mail
// is accepted again as soon as it is there.
func (c *Control) checkMovePTR(ctx context.Context, job MoveJob) []MovePTRStatus {
	if job.Plan.Mail == nil || job.Plan.Mail.OutgoingProvider != "" {
		return nil
	}
	want := func(address string) string {
		for _, status := range job.Plan.Mail.PTR {
			if status.Address == address && status.Want != "" {
				return status.Want
			}
		}
		if len(job.Hostnames) > 0 {
			return job.Hostnames[0]
		}
		return ""
	}
	return c.moveReverseDNS(ctx, job.Addresses, job.Hostnames, want)
}

// movePTRLogLines describes reverse DNS statuses for the cutover log.
func movePTRLogLines(statuses []MovePTRStatus) []string {
	var lines []string
	for _, status := range statuses {
		if status.OK {
			lines = append(lines, fmt.Sprintf("Reverse DNS of %s is %s, one of the application's hostnames; outgoing mail is accepted.", status.Address, strings.Join(status.Names, ", ")))
			continue
		}
		lines = append(lines, movePTRWarning(status))
	}
	return lines
}

// movePTRChanged reports a reverse DNS result that differs from the last
// recorded one, so the cutover log notes changes without repeating itself.
func movePTRChanged(previous, current []MovePTRStatus) bool {
	if len(previous) != len(current) {
		return true
	}
	for i := range current {
		if previous[i].Address != current[i].Address || previous[i].OK != current[i].OK || !slices.Equal(previous[i].Names, current[i].Names) {
			return true
		}
	}
	return false
}
