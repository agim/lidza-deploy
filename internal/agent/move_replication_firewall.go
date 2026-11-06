package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// The replication relay's port in the managed host firewall. While a move
// replicates, the source allows the destination's addresses (and nothing
// else) to the relay's port with rules the agent owns under the source
// "move:<id>": they are applied at once (an allow rule for one peer cannot
// lock the operator out, so the confirmation window is not needed), listed
// with the firewall's rules until the move removes them, and never written
// to the operator's model. An inactive firewall needs nothing; a host whose
// firewall the agent does not manage is reported as such, so the plan can
// say the port must be reachable.

// moveFirewallAtoms are the rules of one relay.
func moveFirewallAtoms(port int, peers []string, move string) []ufwAtom {
	out := []ufwAtom{}
	for _, peer := range peers {
		out = append(out, ufwAtom{Port: strconv.Itoa(port), Proto: "tcp", From: peer, Comment: "lidza: move " + move + " replication"})
	}
	return out
}

// moveFirewallRules lists the relay rules of the published databases, for
// the firewall's effective rule list.
func (m *Manager) moveFirewallRules() []HostFirewallRule {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range m.data.MoveReplications {
			if !yield(id) {
				return
			}
		}
	})
	var rules []HostFirewallRule
	for _, id := range ids {
		record := m.data.MoveReplications[id]
		if !record.Firewall {
			continue
		}
		rules = append(rules, HostFirewallRule{Port: strconv.Itoa(record.Port), Proto: "tcp", From: slices.Clone(record.Peers), Comment: "move " + record.Move + " replication", Source: "move:" + record.Move, Enforced: "ufw"})
	}
	// The destination's tunnel listeners on a Docker network's gateway
	// admit that network for the move (move_replication_tunnel.go).
	for _, id := range slices.Sorted(func(yield func(string) bool) {
		for id := range m.data.MoveTunnels {
			if !yield(id) {
				return
			}
		}
	}) {
		record := m.data.MoveTunnels[id]
		if !record.Firewall {
			continue
		}
		rules = append(rules, HostFirewallRule{Port: strconv.Itoa(record.Port), Proto: "tcp", From: slices.Clone(record.Peers), Comment: "move " + record.Move + " replication tunnel", Source: "move:" + record.Move, Enforced: "ufw"})
	}
	return rules
}

// moveFirewallOpen allows peers to port. It reports whether rules were
// added and the firewall's state: opened, inactive or unmanaged.
func (m *Manager) moveFirewallOpen(ctx context.Context, port int, peers []string, move string) (bool, string, error) {
	if m.firewallSupported() != nil {
		return false, "unmanaged", nil
	}
	hc := m.host()
	hc.fwMu.Lock()
	defer hc.fwMu.Unlock()
	fc, err := m.loadFirewall(ctx)
	if err != nil {
		return false, "unmanaged", nil
	}
	if !fc.host.Active {
		return false, "inactive", nil
	}
	present := map[string]bool{}
	for _, a := range fc.host.Atoms {
		present[a.key()] = true
	}
	atoms := moveFirewallAtoms(port, peers, move)
	for _, a := range atoms {
		if present[a.key()] {
			continue
		}
		if out, err := hostCommand(ctx, "ufw", a.args(true)...); err != nil {
			return false, "", fmt.Errorf("the host firewall did not open the relay port: %s", boundedErrorText(out, 200))
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.data.HostFirewall; st != nil {
		for _, a := range atoms {
			if !slices.ContainsFunc(st.Owned, func(o ufwAtom) bool { return o.key() == a.key() }) {
				st.Owned = append(st.Owned, a)
			}
		}
		if err := m.save(); err != nil {
			return true, "opened", err
		}
	}
	return true, "opened", nil
}

// moveFirewallClose removes the rules of one relay.
func (m *Manager) moveFirewallClose(ctx context.Context, port int, peers []string) error {
	if m.firewallSupported() != nil {
		return nil
	}
	hc := m.host()
	hc.fwMu.Lock()
	defer hc.fwMu.Unlock()
	host, err := readUfwHost(ctx)
	if err != nil {
		return err
	}
	var failures []error
	atoms := moveFirewallAtoms(port, peers, "")
	for _, a := range atoms {
		if !slices.ContainsFunc(host.Atoms, func(o ufwAtom) bool { return o.key() == a.key() }) {
			continue
		}
		if out, err := hostCommand(ctx, "ufw", append([]string{"--force", "delete"}, a.args(false)...)...); err != nil {
			failures = append(failures, fmt.Errorf("the host firewall kept the relay rule for %s: %s", a.From, boundedErrorText(out, 200)))
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.data.HostFirewall; st != nil {
		st.Owned = slices.DeleteFunc(st.Owned, func(o ufwAtom) bool {
			return slices.ContainsFunc(atoms, func(a ufwAtom) bool { return a.key() == o.key() })
		})
		if err := m.save(); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
