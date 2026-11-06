package control

import (
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestMovePlanChecksTheDestinationGatewayForProtectedApps(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.a.source.Protected = true
	fx.b.host = agent.MoveHost{IPv4: moveTestTargetIP, Native: true, HostData: true, Gateway: "auto"}
	plan := mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if !containing(plan.Notes, "The destination sets up the protected gateway for shop at the first deploy") || containing(plan.Blockers, "protected gateway") {
		t.Fatalf("auto: notes %v blockers %v", plan.Notes, plan.Blockers)
	}
	fx.b.host.Gateway = "configured"
	plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if containing(plan.Notes, "protected gateway") || containing(plan.Blockers, "protected gateway") {
		t.Fatalf("configured: notes %v blockers %v", plan.Notes, plan.Blockers)
	}
	fx.b.host.Gateway = "unavailable"
	plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if !containing(plan.Blockers, "neither a host gateway nor a Caddy admin API") {
		t.Fatalf("unavailable: blockers %v", plan.Blockers)
	}
	fx.b.host.Gateway = ""
	plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if !containing(plan.Blockers, "cannot set up; upgrade its agent") {
		t.Fatalf("old agent: blockers %v", plan.Blockers)
	}
	// Unprotected applications need nothing.
	fx.a.source.Protected = false
	plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if containing(plan.Blockers, "protected gateway") || containing(plan.Notes, "protected gateway") {
		t.Fatalf("unprotected: notes %v blockers %v", plan.Notes, plan.Blockers)
	}
}
