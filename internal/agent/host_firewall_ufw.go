package agent

// Pure translation between the host firewall model and ufw. ufw is driven
// with argument vectors (never a shell) and read back through
// `ufw show added`, whose lines use the same syntax.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ufwAtom is one ufw allow rule: one port (or range), one protocol and one
// source. A model rule with several sources becomes several atoms.
type ufwAtom struct {
	Port    string `json:"port"`
	Proto   string `json:"proto"`          // tcp, udp or any
	From    string `json:"from,omitempty"` // empty = anywhere
	Comment string `json:"comment,omitempty"`
}

func (a ufwAtom) key() string { return a.Proto + "|" + a.Port + "|" + a.From }

// args renders `ufw allow …`; withComment is false for `ufw delete`, which
// matches rules regardless of comment.
func (a ufwAtom) args(withComment bool) []string {
	var out []string
	if a.From == "" {
		spec := a.Port
		if a.Proto != "any" {
			spec += "/" + a.Proto
		}
		out = []string{"allow", spec}
	} else {
		out = []string{"allow", "from", a.From, "to", "any", "port", a.Port}
		if a.Proto != "any" {
			out = append(out, "proto", a.Proto)
		}
	}
	if withComment && a.Comment != "" {
		out = append(out, "comment", a.Comment)
	}
	return out
}

func (a ufwAtom) command(withComment bool, prefix ...string) string {
	parts := append([]string{"ufw"}, prefix...)
	for _, arg := range a.args(withComment) {
		if strings.ContainsAny(arg, " \t'\"") {
			arg = "'" + arg + "'"
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}

var ufwPortPattern = regexp.MustCompile(`^([0-9]{1,5})(?::([0-9]{1,5}))?$`)

func validFirewallPort(port string) error {
	match := ufwPortPattern.FindStringSubmatch(port)
	if match == nil {
		return fmt.Errorf("port %q must be a number or a range such as 60000:61000", port)
	}
	low, _ := strconv.Atoi(match[1])
	if low < 1 || low > 65535 {
		return fmt.Errorf("port %q is out of range", port)
	}
	if match[2] != "" {
		high, _ := strconv.Atoi(match[2])
		if high <= low || high > 65535 {
			return fmt.Errorf("port range %q is invalid", port)
		}
	}
	return nil
}

// normalizeSource returns the canonical form ufw displays: a bare address for
// single hosts and a network address for CIDRs.
func normalizeSource(source string) (string, error) {
	source = strings.TrimSpace(source)
	if ip := net.ParseIP(source); ip != nil {
		return ip.String(), nil
	}
	ip, network, err := net.ParseCIDR(source)
	if err != nil {
		return "", fmt.Errorf("%q is not an IP address or CIDR network", source)
	}
	ones, bits := network.Mask.Size()
	if ones == bits {
		return ip.String(), nil
	}
	if ones == 0 {
		return "", fmt.Errorf("%q matches every address; leave the source empty for anywhere", source)
	}
	return network.String(), nil
}

// tokenizeUfw splits a `ufw show added` line, honouring quotes.
func tokenizeUfw(line string) ([]string, error) {
	var out []string
	var current strings.Builder
	quote := rune(0)
	inToken := false
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inToken = true
		case r == ' ' || r == '\t':
			if inToken {
				out = append(out, current.String())
				current.Reset()
				inToken = false
			}
		default:
			current.WriteRune(r)
			inToken = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inToken {
		out = append(out, current.String())
	}
	return out, nil
}

// parseUfwAdded reads `ufw show added`. Plain allow rules for a port become
// atoms; anything else (deny/limit/reject rules, interface or destination
// rules, application profiles, all-port allows) is returned verbatim so the
// agent leaves it exactly as it is.
func parseUfwAdded(out string) (atoms []ufwAtom, kept []string) {
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "ufw ") {
			continue
		}
		atom, ok := parseUfwRule(line)
		if ok {
			atoms = append(atoms, atom)
		} else {
			kept = append(kept, line)
		}
	}
	return atoms, kept
}

func parseUfwRule(line string) (ufwAtom, bool) {
	tokens, err := tokenizeUfw(line)
	if err != nil || len(tokens) < 3 || tokens[0] != "ufw" || tokens[1] != "allow" {
		return ufwAtom{}, false
	}
	tokens = tokens[2:]
	atom := ufwAtom{Proto: "any"}
	if i := slices.Index(tokens, "comment"); i >= 0 {
		if i != len(tokens)-2 {
			return ufwAtom{}, false
		}
		atom.Comment = tokens[i+1]
		tokens = tokens[:i]
	}
	if len(tokens) == 1 {
		spec, proto, hasProto := strings.Cut(tokens[0], "/")
		if hasProto {
			atom.Proto = proto
		}
		atom.Port = spec
	} else {
		destination := ""
		for i := 0; i < len(tokens); i += 2 {
			if i+1 >= len(tokens) {
				return ufwAtom{}, false
			}
			value := tokens[i+1]
			switch tokens[i] {
			case "proto":
				atom.Proto = value
			case "from":
				if value != "any" {
					atom.From = value
				}
			case "to":
				destination = value
			case "port":
				atom.Port = value
			default:
				return ufwAtom{}, false
			}
		}
		if destination != "any" {
			return ufwAtom{}, false
		}
	}
	if atom.Proto != "tcp" && atom.Proto != "udp" && atom.Proto != "any" {
		return ufwAtom{}, false
	}
	if validFirewallPort(atom.Port) != nil || strings.Contains(atom.Port, ":") && atom.Proto == "any" {
		return ufwAtom{}, false
	}
	if atom.From != "" {
		normalized, err := normalizeSource(atom.From)
		if err != nil {
			return ufwAtom{}, false
		}
		atom.From = normalized
	}
	return atom, true
}

// ufwHostStatus is what ufw reports about the running firewall.
type ufwHostStatus struct {
	Installed       bool
	Active          bool
	DefaultIncoming string // deny, allow or reject
	Atoms           []ufwAtom
	Kept            []string
}

// parseUfwStatus reads `ufw status verbose`. The default policy is only
// printed while ufw is active; parseUfwDefaults covers the inactive case.
func parseUfwStatus(out string) (active bool, incoming string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "Status:"); ok {
			active = strings.TrimSpace(value) == "active"
		}
		if value, ok := strings.CutPrefix(line, "Default:"); ok {
			for _, part := range strings.Split(value, ",") {
				fields := strings.Fields(part)
				if len(fields) == 2 && fields[1] == "(incoming)" {
					incoming = fields[0]
				}
			}
		}
	}
	return active, incoming
}

// parseUfwDefaults reads DEFAULT_INPUT_POLICY from /etc/default/ufw.
func parseUfwDefaults(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "DEFAULT_INPUT_POLICY="); ok {
			switch strings.Trim(value, `"'`) {
			case "DROP":
				return "deny"
			case "ACCEPT":
				return "allow"
			case "REJECT":
				return "reject"
			}
		}
	}
	return ""
}

// firewallStep is one command of a plan.
type firewallStep struct {
	Action string // add, delete, default, enable, disable
	Atom   *ufwAtom
	Args   []string
}

func (s firewallStep) command() string {
	if s.Atom != nil {
		if s.Action == "delete" {
			return s.Atom.command(false, "--force", "delete")
		}
		return s.Atom.command(true)
	}
	return "ufw " + strings.Join(s.Args, " ")
}

// planUfw computes the commands that move the host from its current rules to
// the desired ones. Only atoms the agent owns (imported or applied before) are
// ever deleted; host rules it does not own are kept and reported. Rules are
// added before the firewall is enabled and removed last, so a new ruleset never
// starts out narrower than both the old and new rule sets.
func planUfw(host ufwHostStatus, owned, desired []ufwAtom, enable bool, incoming string) (steps []firewallStep, unmanaged []ufwAtom) {
	present := map[string]bool{}
	for _, a := range host.Atoms {
		present[a.key()] = true
	}
	wanted := map[string]bool{}
	for _, a := range desired {
		wanted[a.key()] = true
	}
	ownedKeys := map[string]bool{}
	for _, a := range owned {
		ownedKeys[a.key()] = true
	}
	added := map[string]bool{}
	for _, a := range desired {
		if !present[a.key()] && !added[a.key()] {
			atom := a
			steps = append(steps, firewallStep{Action: "add", Atom: &atom})
			added[a.key()] = true
		}
	}
	if enable {
		if incoming != "" && host.DefaultIncoming != incoming {
			steps = append(steps, firewallStep{Action: "default", Args: []string{"default", incoming, "incoming"}})
		}
		if !host.Active {
			steps = append(steps, firewallStep{Action: "enable", Args: []string{"--force", "enable"}})
		}
	} else {
		if host.Active {
			steps = append(steps, firewallStep{Action: "disable", Args: []string{"disable"}})
		}
		if incoming != "" && host.DefaultIncoming != "" && host.DefaultIncoming != incoming {
			steps = append(steps, firewallStep{Action: "default", Args: []string{"default", incoming, "incoming"}})
		}
	}
	for _, a := range host.Atoms {
		if wanted[a.key()] {
			continue
		}
		if ownedKeys[a.key()] {
			atom := a
			steps = append(steps, firewallStep{Action: "delete", Atom: &atom})
		} else {
			unmanaged = append(unmanaged, a)
		}
	}
	return steps, unmanaged
}

// expandRules turns model rules into atoms with the agent's comment marker.
func expandRules(rules []HostFirewallRule) []ufwAtom {
	var out []ufwAtom
	for _, r := range rules {
		if r.Enforced == "docker" {
			continue
		}
		// Imported and user rules keep their own comment; system rules are
		// marked so an operator reading `ufw status` knows who added them.
		comment := r.Comment
		if r.Source == "system" {
			comment = "lidza: " + r.Comment
		}
		if comment == "" {
			comment = "lidza: " + r.Source
		}
		if len(comment) > 100 {
			comment = comment[:100]
		}
		sources := r.From
		if len(sources) == 0 {
			sources = []string{""}
		}
		for _, from := range sources {
			out = append(out, ufwAtom{Port: r.Port, Proto: r.Proto, From: from, Comment: comment})
		}
	}
	return out
}

func planFingerprint(steps []firewallStep) string {
	var commands []string
	for _, s := range steps {
		commands = append(commands, s.command())
	}
	raw, _ := json.Marshal(commands)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}
