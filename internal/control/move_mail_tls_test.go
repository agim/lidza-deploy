package control

import (
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestMovePlanWarnsAboutMailPortsWithoutPublicTLS(t *testing.T) {
	fx := newMoveFixture(t)
	fx.mailSource()
	want := agent.MailTLSWarning(fx.a.source.PublicPorts)
	if want != `Ports 25/tcp and 587/tcp receive mail, but no service declares public_tls: senders that require STARTTLS (Gmail, Microsoft) fail and defer mail. Declare "public_tls": {"hostname": "primary", "uid": <the mail server's uid>} on the service that terminates SMTP.` {
		t.Fatalf("warning text: %s", want)
	}
	declared := false
	fx.a.source.PublicTLS = &declared
	plan := mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if !containing(plan.Warnings, want) {
		t.Fatalf("warnings %v", plan.Warnings)
	}
	// A release whose mail server declares public_tls, and an agent that
	// does not report it, are not warned about.
	declared = true
	if plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"}); containing(plan.Warnings, "no service declares public_tls") {
		t.Fatalf("warnings %v", plan.Warnings)
	}
	fx.a.source.PublicTLS = nil
	if plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"}); containing(plan.Warnings, "no service declares public_tls") {
		t.Fatalf("warnings %v", plan.Warnings)
	}
	// Without a mail port there is nothing to warn about.
	declared = false
	fx.a.source.PublicTLS = &declared
	fx.a.source.PublicPorts = []agent.StackPort{{Port: 8443, Protocol: "tcp"}}
	if plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"}); containing(plan.Warnings, "no service declares public_tls") {
		t.Fatalf("warnings %v", plan.Warnings)
	}
}
