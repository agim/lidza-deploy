package agent

import (
	"reflect"
	"strings"
	"testing"
)

const handMadeUfw = `Added user rules (see 'ufw status' for running firewall):
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443
ufw allow from 203.0.113.10 to any port 5432 proto tcp
ufw allow from 198.51.100.0/24 to any port 5432 proto tcp comment 'office VPN'
ufw allow from 203.0.113.10 to any port 6379 proto tcp
ufw limit 2222/tcp
ufw allow OpenSSH
ufw allow in on eth1 to any port 9000
`

func TestParseUfwAddedKeepsUnrepresentableRules(t *testing.T) {
	atoms, kept := parseUfwAdded(handMadeUfw)
	want := []ufwAtom{
		{Port: "22", Proto: "tcp"},
		{Port: "80", Proto: "tcp"},
		{Port: "443", Proto: "any"},
		{Port: "5432", Proto: "tcp", From: "203.0.113.10"},
		{Port: "5432", Proto: "tcp", From: "198.51.100.0/24", Comment: "office VPN"},
		{Port: "6379", Proto: "tcp", From: "203.0.113.10"},
	}
	if !reflect.DeepEqual(atoms, want) {
		t.Fatalf("atoms:\n got %#v\nwant %#v", atoms, want)
	}
	if !reflect.DeepEqual(kept, []string{"ufw limit 2222/tcp", "ufw allow OpenSSH", "ufw allow in on eth1 to any port 9000"}) {
		t.Fatalf("kept = %#v", kept)
	}
}

func TestUfwAtomRendering(t *testing.T) {
	cases := map[string]ufwAtom{
		"allow 80/tcp": {Port: "80", Proto: "tcp"},
		"allow 443":    {Port: "443", Proto: "any"},
		"allow from 203.0.113.10 to any port 5432 proto tcp comment office VPN": {Port: "5432", Proto: "tcp", From: "203.0.113.10", Comment: "office VPN"},
		"allow from 2001:db8::/32 to any port 60000:61000 proto udp":            {Port: "60000:61000", Proto: "udp", From: "2001:db8::/32"},
	}
	for want, atom := range cases {
		if got := strings.Join(atom.args(true), " "); got != want {
			t.Fatalf("args = %q, want %q", got, want)
		}
		// The rendered command parses back to the same rule.
		parsed, ok := parseUfwRule(atom.command(true))
		if !ok || parsed != atom {
			t.Fatalf("round trip of %q = %#v, %v", atom.command(true), parsed, ok)
		}
	}
	if got := (ufwAtom{Port: "80", Proto: "tcp", Comment: "x"}).command(false, "--force", "delete"); got != "ufw --force delete allow 80/tcp" {
		t.Fatalf("delete command = %q", got)
	}
}

func TestImportPreservesExistingAllowlistVerbatim(t *testing.T) {
	atoms, kept := parseUfwAdded(handMadeUfw)
	host := ufwHostStatus{Installed: true, Active: true, DefaultIncoming: "deny", Atoms: atoms, Kept: kept}
	model := importUfwModel(host, []string{"22"})
	if !model.Enabled || model.DefaultInbound != "deny" || model.SSHFrom != nil {
		t.Fatalf("model = %#v", model)
	}
	want := []HostFirewallRule{
		{Port: "443", Proto: "any", Source: "user", Enforced: "ufw"},
		{Port: "5432", Proto: "tcp", From: []string{"203.0.113.10"}, Source: "user", Enforced: "ufw"},
		{Port: "5432", Proto: "tcp", From: []string{"198.51.100.0/24"}, Comment: "office VPN", Source: "user", Enforced: "ufw"},
		{Port: "6379", Proto: "tcp", From: []string{"203.0.113.10"}, Source: "user", Enforced: "ufw"},
	}
	if !reflect.DeepEqual(model.Rules, want) {
		t.Fatalf("rules:\n got %#v\nwant %#v", model.Rules, want)
	}
	m := &Manager{cfg: Config{Listen: "127.0.0.1:9090"}, data: diskState{Apps: map[string]App{}}}
	rules := append(m.systemFirewallRules([]string{"22"}, model.SSHFrom), model.Rules...)
	steps, unmanaged := planUfw(host, atoms, expandRules(rules), true, "deny")
	var commands []string
	for _, s := range steps {
		commands = append(commands, s.command())
	}
	// Only the missing system rules are added; nothing imported is removed.
	wantCommands := []string{"ufw allow 443/tcp comment 'lidza: HTTPS'", "ufw allow 443/udp comment 'lidza: HTTP/3'"}
	if !reflect.DeepEqual(commands, wantCommands) || len(unmanaged) != 0 {
		t.Fatalf("commands = %#v, unmanaged = %#v", commands, unmanaged)
	}
}

func TestImportRestrictedSSHBecomesSSHSources(t *testing.T) {
	atoms, _ := parseUfwAdded("ufw allow from 192.0.2.4 to any port 22 proto tcp\nufw allow from 192.0.2.0/28 to any port 22 proto tcp\n")
	model := importUfwModel(ufwHostStatus{Installed: true, Active: true, Atoms: atoms}, []string{"22"})
	if !reflect.DeepEqual(model.SSHFrom, []string{"192.0.2.4", "192.0.2.0/28"}) || len(model.Rules) != 0 {
		t.Fatalf("model = %#v", model)
	}
}

func TestPlanEnablesAfterAddingAndDeletesOnlyOwnedRules(t *testing.T) {
	m := &Manager{cfg: Config{Listen: "0.0.0.0:9443"}, data: diskState{Apps: map[string]App{}}}
	system := m.systemFirewallRules([]string{"22"}, nil)
	if last := system[len(system)-1]; last.Port != "9443" || last.Comment != "Līdza agent" {
		t.Fatalf("public agent listener must stay reachable: %#v", system)
	}
	owned := ufwAtom{Port: "8080", Proto: "tcp"}
	foreign := ufwAtom{Port: "9100", Proto: "tcp", From: "10.0.0.5"}
	host := ufwHostStatus{Installed: true, Active: false, DefaultIncoming: "allow", Atoms: []ufwAtom{owned, foreign}}
	steps, unmanaged := planUfw(host, []ufwAtom{owned}, expandRules(system), true, "deny")
	var actions []string
	for _, s := range steps {
		actions = append(actions, s.Action)
	}
	want := []string{"add", "add", "add", "add", "add", "default", "enable", "delete"}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
	if steps[len(steps)-1].Atom.Port != "8080" || !reflect.DeepEqual(unmanaged, []ufwAtom{foreign}) {
		t.Fatalf("delete %#v, unmanaged %#v", steps[len(steps)-1].Atom, unmanaged)
	}
	if planFingerprint(steps) == planFingerprint(steps[:len(steps)-1]) {
		t.Fatal("fingerprint must change with the plan")
	}
}

func TestRevertPlanRestoresPreviousRules(t *testing.T) {
	previous := []ufwAtom{{Port: "22", Proto: "tcp"}}
	applied := []ufwAtom{{Port: "22", Proto: "tcp"}, {Port: "80", Proto: "tcp"}}
	host := ufwHostStatus{Installed: true, Active: true, DefaultIncoming: "deny", Atoms: applied}
	steps, _ := planUfw(host, append(applied, previous...), previous, false, "deny")
	var commands []string
	for _, s := range steps {
		commands = append(commands, s.command())
	}
	if !reflect.DeepEqual(commands, []string{"ufw disable", "ufw --force delete allow 80/tcp"}) {
		t.Fatalf("revert commands = %#v", commands)
	}
}

func TestNormalizeFirewallModel(t *testing.T) {
	model, err := normalizeFirewallModel(HostFirewallModel{Enabled: true, Rules: []HostFirewallRule{
		{Port: "5432", From: []string{"203.0.113.10/32", "198.51.100.7/24"}, Comment: "replica"},
		{Port: "22", Proto: "tcp", Source: "system"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []HostFirewallRule{{Port: "5432", Proto: "tcp", From: []string{"203.0.113.10", "198.51.100.0/24"}, Comment: "replica", Source: "user", Enforced: "ufw"}}
	if model.DefaultInbound != "deny" || !reflect.DeepEqual(model.Rules, want) {
		t.Fatalf("model = %#v", model)
	}
	for name, bad := range map[string]HostFirewallModel{
		"ssh loopback":  {SSHFrom: []string{"127.0.0.1"}},
		"default allow": {DefaultInbound: "allow"},
		"range any":     {Rules: []HostFirewallRule{{Port: "6000:6010", Proto: "any"}}},
		"port":          {Rules: []HostFirewallRule{{Port: "70000"}}},
		"everyone cidr": {Rules: []HostFirewallRule{{Port: "80", From: []string{"0.0.0.0/0"}}}},
		"duplicate":     {Rules: []HostFirewallRule{{Port: "80"}, {Port: "80", Proto: "tcp"}}},
		"comment quote": {Rules: []HostFirewallRule{{Port: "80", Comment: "it's"}}},
	} {
		if _, err := normalizeFirewallModel(bad); err == nil {
			t.Fatalf("%s: invalid model accepted", name)
		}
	}
}

func TestUfwStatusParsing(t *testing.T) {
	active, incoming := parseUfwStatus("Status: active\nLogging: on (low)\nDefault: deny (incoming), allow (outgoing), deny (routed)\nNew profiles: skip\n")
	if !active || incoming != "deny" {
		t.Fatalf("active=%v incoming=%q", active, incoming)
	}
	if active, incoming := parseUfwStatus("Status: inactive\n"); active || incoming != "" {
		t.Fatalf("inactive parse = %v %q", active, incoming)
	}
	if got := parseUfwDefaults("IPV6=yes\nDEFAULT_INPUT_POLICY=\"DROP\"\n"); got != "deny" {
		t.Fatalf("defaults = %q", got)
	}
	if !portCovered("5432", "tcp", []HostFirewallRule{{Port: "5000:6000", Proto: "tcp"}}) || portCovered("5432", "udp", []HostFirewallRule{{Port: "5432", Proto: "tcp"}}) {
		t.Fatal("port coverage")
	}
}
