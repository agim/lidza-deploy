package control

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

// mailSource turns the fixture's container application into one that
// publishes mail ports, with the source and destination agents reporting
// ports.
func (fx *moveFixture) mailSource() {
	fx.containerSource()
	fx.a.source.PublicPorts = []agent.StackPort{{Port: 25, Protocol: "tcp"}, {Port: 587, Protocol: "tcp"}, {Port: 8443, Protocol: "tcp"}}
	fx.a.host = agent.MoveHost{IPv4: moveTestSourceIP}
	fx.b.host = agent.MoveHost{IPv4: moveTestTargetIP, IPv6: moveTestTargetIP6, Native: true, HostData: true, ListeningPorts: []agent.ListeningPort{{Port: 22, Protocol: "tcp"}, {Port: 443, Protocol: "tcp"}}, HostFirewall: "active"}
}

func mailPlanFor(t *testing.T, fx *moveFixture, in moveStartInput) MovePlan {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	plan, status, err := fx.c.computeMovePlan(req, "shop", in)
	if err != nil || status != 200 {
		t.Fatal(status, err)
	}
	return plan
}

func containing(items []string, want string) bool {
	return slices.ContainsFunc(items, func(item string) bool { return strings.Contains(item, want) })
}

func TestMovePlanChecksPublishedPortsOnTheDestination(t *testing.T) {
	fx := newMoveFixture(t)
	fx.mailSource()
	fx.resolver.setPTR(moveTestTargetIP, "shop.example.com")
	fx.resolver.setPTR(moveTestTargetIP6, "shop.example.com")
	plan := mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if len(plan.PublicPorts) != 3 || containing(plan.Blockers, "in use on the destination") {
		t.Fatalf("free ports block: %+v %v", plan.PublicPorts, plan.Blockers)
	}
	if !containing(plan.Notes, "The destination publishes port 25/tcp, 587/tcp, 8443/tcp; its agent opens them in the Docker firewall chain") {
		t.Fatalf("notes %v", plan.Notes)
	}
	// A port already bound on the destination blocks the move.
	fx.b.host.ListeningPorts = append(fx.b.host.ListeningPorts, agent.ListeningPort{Port: 25, Protocol: "tcp"})
	plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if !containing(plan.Blockers, "Port 25/tcp is in use on the destination") || !containing(plan.Notes, "publishes port 587/tcp, 8443/tcp") {
		t.Fatalf("blockers %v notes %v", plan.Blockers, plan.Notes)
	}
	if status, _ := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true}); status != 409 {
		t.Fatalf("a port collision must stop the move: %d", status)
	}
	// Older agents: the plan says what it cannot check.
	fx.b.host.ListeningPorts = nil
	plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if containing(plan.Blockers, "in use") || !containing(plan.Notes, "its agent does not report bound ports") {
		t.Fatalf("an old destination agent is a note, not a blocker: %v %v", plan.Blockers, plan.Notes)
	}
	fx.a.source.PublicPorts = nil
	plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if plan.PublicPorts != nil || plan.Mail != nil || !containing(plan.Notes, "The source agent does not report published ports") {
		t.Fatalf("an old source agent is a note: %+v", plan.Notes)
	}
	// Applications without published ports get no port note.
	fx.a.source.PublicPorts = []agent.StackPort{}
	plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if containing(plan.Notes, "port") || plan.Mail != nil {
		t.Fatalf("no ports, no note: %v", plan.Notes)
	}
}

func TestMovePlanPreparesMailRecordsAndWarnsAboutReverseDNS(t *testing.T) {
	fx := newMoveFixture(t)
	fx.mailSource()
	fx.resolver.setPTR(moveTestSourceIP, "shop.example.com")
	fx.resolver.setPTR(moveTestTargetIP, "ubuntu-s-1vcpu-2gb-01")
	fx.resolver.setTXT("example.com", "v=spf1 ip4:"+moveTestSourceIP+" include:_spf.example.net -all", "google-site-verification=abc")
	fx.resolver.setTXT("shop.example.com", "v=spf1 a ip6:2001:db8::7/128 ~all")
	fx.resolver.setMX("example.com", "mx.mailhost.example.net.")
	fx.resolver.setMX("shop.example.com", "shop.example.com.")
	plan := mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	mail := plan.Mail
	if mail == nil || len(mail.Ports) != 2 || !slices.Equal(mail.SourceAddresses, []string{moveTestSourceIP}) {
		t.Fatalf("mail plan %+v", mail)
	}
	// The destination IPv4 names the droplet, the IPv6 has none; the wanted
	// name is the source's reverse DNS, one of the application's hostnames.
	if len(mail.PTR) != 2 || mail.PTR[0].OK || mail.PTR[1].OK || mail.PTR[0].Want != "shop.example.com" || mail.PTR[1].Error != "no reverse DNS" {
		t.Fatalf("ptr %+v", mail.PTR)
	}
	for _, want := range []string{
		"Reverse DNS of " + moveTestTargetIP + " is ubuntu-s-1vcpu-2gb-01; set it to shop.example.com at the server provider before the cutover, or outgoing mail is rejected. (At DigitalOcean the reverse DNS is the droplet's name.)",
		"Reverse DNS of " + moveTestTargetIP6 + " is none; set it to shop.example.com at the server provider",
		"MX of example.com points at mx.mailhost.example.net, which is not one of the moved hostnames",
	} {
		if !containing(plan.Warnings, want) {
			t.Fatalf("missing warning %q in %v", want, plan.Warnings)
		}
	}
	if containing(plan.Warnings, "MX of shop.example.com") {
		t.Fatalf("an MX at a moved hostname is fine: %v", plan.Warnings)
	}
	if !containing(plan.Notes, "The application sends mail (port 25/tcp, 587/tcp)") || !containing(plan.Notes, "DKIM is unaffected") {
		t.Fatalf("notes %v", plan.Notes)
	}
	var kinds []string
	for _, r := range mail.Records {
		kinds = append(kinds, r.Type+" "+r.Hostname+" "+r.Value+" | "+r.Note)
	}
	want := []string{
		"PTR " + moveTestTargetIP + " shop.example.com | " + moveMailPTRNote,
		"PTR " + moveTestTargetIP6 + " shop.example.com | " + moveMailPTRNote,
		"TXT example.com v=spf1 ip4:" + moveTestTargetIP + " include:_spf.example.net -all | " + moveMailSPFNote,
	}
	if !slices.Equal(kinds, want) {
		t.Fatalf("records\n%s\nwant\n%s", strings.Join(kinds, "\n"), strings.Join(want, "\n"))
	}
	// A destination whose reverse DNS already names the application needs
	// no change; the record says so.
	fx.resolver.setPTR(moveTestTargetIP, "www.shop.example.com.")
	fx.resolver.setPTR(moveTestTargetIP6, "shop.example.com")
	plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if containing(plan.Warnings, "Reverse DNS") || !plan.Mail.PTR[0].OK || !strings.HasPrefix(plan.Mail.Records[0].Note, "Already set.") {
		t.Fatalf("ptr %+v warnings %v", plan.Mail.PTR, plan.Warnings)
	}
	// Without a usable source reverse DNS the primary hostname is wanted.
	fx.resolver.setPTR(moveTestSourceIP, "old-droplet")
	fx.resolver.setPTR(moveTestTargetIP6)
	plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if plan.Mail.PTR[1].Want != "shop.example.com" {
		t.Fatalf("ptr %+v", plan.Mail.PTR)
	}
}

func TestMoveCutoverListsMailRecordsAndChecksReverseDNSWithoutBlocking(t *testing.T) {
	fx := newMoveFixture(t)
	fx.mailSource()
	fx.resolver.setPTR(moveTestSourceIP, "shop.example.com")
	fx.resolver.setTXT("example.com", "v=spf1 ip4:"+moveTestSourceIP+" -all")
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatalf("start: %d %+v", status, view)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	records := moveRecords(job)
	var lines []string
	for _, r := range records {
		lines = append(lines, r.Hostname+" "+r.Type+" "+r.Value)
	}
	for _, want := range []string{moveTestTargetIP + " PTR shop.example.com", moveTestTargetIP6 + " PTR shop.example.com", "example.com TXT v=spf1 ip4:" + moveTestTargetIP + " -all", "shop.example.com A " + moveTestTargetIP} {
		if !slices.Contains(lines, want) {
			t.Fatalf("missing record %q in %v", want, lines)
		}
	}
	log := strings.Join(moveStepLogs(job, moveStepCutover), "\n")
	for _, want := range []string{
		moveTestTargetIP + " PTR shop.example.com (" + strings.TrimSuffix(moveMailPTRNote, ".") + ")",
		"example.com TXT v=spf1 ip4:" + moveTestTargetIP + " -all (" + strings.TrimSuffix(moveMailSPFNote, ".") + ")",
		"Reverse DNS of " + moveTestTargetIP + " is none; set it to shop.example.com at the server provider",
		"DKIM is unaffected",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("cutover log lacks %q:\n%s", want, log)
		}
	}
	// DNS points to the destination and HTTPS works, but the reverse DNS is
	// still missing: the move is ready anyway, and says so again on an
	// explicit recheck.
	fx.resolver.set("shop.example.com", moveTestTargetIP, moveTestTargetIP6)
	fx.resolver.set("www.shop.example.com", moveTestTargetIP, moveTestTargetIP6)
	fx.probe.set(true)
	if status := moveAction(t, fx.c, job.ID, "recheck-dns", nil); status != 202 {
		t.Fatalf("recheck: %d", status)
	}
	job = waitMove(t, fx.c, job.ID, moveStateReady)
	if len(job.PTR) != 2 || job.PTR[0].OK || job.PTR[0].Want != "shop.example.com" {
		t.Fatalf("ptr status %+v", job.PTR)
	}
	// Once set at the provider, the next check records it.
	fx.resolver.setPTR(moveTestTargetIP, "shop.example.com")
	fx.resolver.setPTR(moveTestTargetIP6, "shop.example.com")
	if status := moveAction(t, fx.c, job.ID, "recheck-dns", nil); status != 202 {
		t.Fatalf("recheck: %d", status)
	}
	job = waitMove(t, fx.c, job.ID, moveStateReady)
	if !job.PTR[0].OK || !job.PTR[1].OK {
		t.Fatalf("ptr status %+v", job.PTR)
	}
	if log := strings.Join(moveStepLogs(job, moveStepCutover), "\n"); !strings.Contains(log, "Reverse DNS of "+moveTestTargetIP+" is shop.example.com, one of the application's hostnames; outgoing mail is accepted.") {
		t.Fatalf("cutover log:\n%s", log)
	}
	// The browser's view carries the notes with the records.
	view = moveJobView(job)
	if i := slices.IndexFunc(view.Records, func(r MoveDNSRecord) bool { return r.Type == "PTR" }); i < 0 || view.Records[i].Note == "" {
		t.Fatalf("records %+v", view.Records)
	}
}

func moveStepLogs(job MoveJob, key string) []string {
	var out []string
	if s := job.step(key); s != nil {
		for _, line := range s.Log {
			out = append(out, line.Message)
		}
	}
	return out
}

func TestMoveRewriteSPFReplacesOnlySourceAddresses(t *testing.T) {
	source, destination := []string{"198.51.100.7", "2001:db8::7"}, []string{"203.0.113.9", "2001:db8::9"}
	cases := []struct {
		in, want string
		changed  bool
	}{
		{"v=spf1 ip4:198.51.100.7 -all", "v=spf1 ip4:203.0.113.9 -all", true},
		{"v=spf1 +ip4:198.51.100.7/32 ip6:2001:db8::7 ~all", "v=spf1 +ip4:203.0.113.9 ip6:2001:db8::9 ~all", true},
		{"v=spf1 ip4:198.51.100.0/24 -all", "v=spf1 ip4:198.51.100.0/24 -all", false},
		{"v=spf1 ip4:198.51.100.70 a mx -all", "v=spf1 ip4:198.51.100.70 a mx -all", false},
		{"v=spf1 a mx -all", "v=spf1 a mx -all", false},
		{"google-site-verification=ip4:198.51.100.7", "google-site-verification=ip4:198.51.100.7", false},
		{"v=spf1 ip4:198.51.100.7 ip4:203.0.113.9 -all", "v=spf1 ip4:203.0.113.9 -all", true},
	}
	for _, tc := range cases {
		got, changed := moveRewriteSPF(tc.in, source, destination)
		if got != tc.want || changed != tc.changed {
			t.Errorf("%q → %q (%v), want %q (%v)", tc.in, got, changed, tc.want, tc.changed)
		}
	}
	// A source IPv6 with no destination IPv6 is dropped rather than kept.
	if got, _ := moveRewriteSPF("v=spf1 ip4:198.51.100.7 ip6:2001:db8::7 -all", source, []string{"203.0.113.9"}); got != "v=spf1 ip4:203.0.113.9 -all" {
		t.Fatalf("got %q", got)
	}
	for host, want := range map[string]string{"shop.example.com": "example.com", "www.shop.example.co.uk": "example.co.uk", "example.com": "example.com", "localhost": "localhost"} {
		if got := moveApex(host); got != want {
			t.Errorf("apex of %s = %s, want %s", host, got, want)
		}
	}
	if names := moveMailNames([]string{"Shop.example.com", "www.shop.example.com"}); !slices.Equal(names, []string{"shop.example.com", "example.com", "www.shop.example.com"}) {
		t.Fatalf("names %v", names)
	}
}

func TestMovePlanSkipsReverseDNSForMailSentThroughAProvider(t *testing.T) {
	fx := newMoveFixture(t)
	fx.mailSource()
	declared := false
	fx.a.source.PublicTLS = &declared
	fx.a.source.OutgoingMail = "Mailgun"
	fx.resolver.setPTR(moveTestTargetIP, "ubuntu-s-1vcpu-2gb-01")
	plan := mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if containing(plan.Warnings, "Reverse DNS") || plan.Mail == nil || len(plan.Mail.PTR) != 0 || plan.Mail.OutgoingProvider != "Mailgun" {
		t.Fatalf("warnings %v mail %+v", plan.Warnings, plan.Mail)
	}
	if !containing(plan.Notes, "Outgoing mail goes through Mailgun; reverse DNS of the destination does not affect it.") {
		t.Fatalf("notes %v", plan.Notes)
	}
	for _, record := range plan.Mail.Records {
		if record.Type == "PTR" {
			t.Fatalf("a PTR record for provider mail: %+v", record)
		}
	}
	// Receiving is separate: the inbound STARTTLS certificate warning stays.
	if !containing(plan.Warnings, "public_tls") {
		t.Fatalf("the inbound public_tls warning was dropped: %v", plan.Warnings)
	}
	if statuses := fx.c.checkMovePTR(context.Background(), MoveJob{Plan: plan, Addresses: plan.Addresses, Hostnames: plan.Hostnames}); statuses != nil {
		t.Fatalf("the cutover checks reverse DNS for provider mail: %+v", statuses)
	}
}

func TestOutgoingMailProviderDetection(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"MAILGUN_API_KEY": "key"}, "Mailgun"},
		{map[string]string{"SENDGRID_API_KEY": "key"}, "SendGrid"},
		{map[string]string{"POSTMARK_API_TOKEN": "t"}, "Postmark"},
		{map[string]string{"AWS_SES_REGION": "eu-west-1"}, "Amazon SES"},
		{map[string]string{"RESEND_API_KEY": "re_x"}, "Resend"},
		{map[string]string{"SMTP_ADDRESS": "smtp.mailgun.org"}, "Mailgun"},
		{map[string]string{"SMTP_HOST": "email-smtp.eu-west-1.amazonaws.com"}, "Amazon SES"},
		{map[string]string{"MAIL_SMTP_HOST": "mail.example.net"}, "the SMTP server mail.example.net"},
		{map[string]string{"MAIL_PROVIDER": "mailgun"}, "Mailgun"},
		{map[string]string{"MAIL_PROVIDER": "smtp", "SMTP_HOST": "smtp.postmarkapp.com"}, "Postmark"},
		{map[string]string{"MAIL_PROVIDER": "smtp", "SMTP_HOST": "localhost"}, ""},
		{map[string]string{"SMTP_ADDRESS": "127.0.0.1"}, ""},
		{map[string]string{"SMTP_HOST": "mailserver"}, ""},
		{map[string]string{"SMTP_HOST": "10.0.0.5"}, ""},
		{map[string]string{"MAILGUN_API_KEY": ""}, ""},
		{map[string]string{"DATABASE_URL": "postgres://x"}, ""},
	} {
		if got := agent.OutgoingMailProvider(tc.env); got != tc.want {
			t.Fatalf("%v: got %q want %q", tc.env, got, tc.want)
		}
	}
}
