package control

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Unit tests never look hostnames up on the network.
func init() { moveResolveHost = func(string) []string { return nil } }

func TestMoveTargetAddressesFallBackToTheServersHostname(t *testing.T) {
	saved := moveResolveHost
	t.Cleanup(func() { moveResolveHost = saved })
	asked := []string{}
	moveResolveHost = func(host string) []string {
		asked = append(asked, host)
		return []string{"192.168.60.192", "203.0.113.7", "2001:db8::7", "203.0.113.7"}
	}
	server := Server{ID: "home", URL: "https://server1.example.com"}
	// Behind a router the agent reports only a private address: the public
	// addresses of the name the panel reaches it by are used.
	got := moveTargetAddresses(agent.MoveHost{IPv4: "192.168.60.192"}, server, nil)
	if !slices.Equal(got, []string{"203.0.113.7", "2001:db8::7"}) || !slices.Equal(asked, []string{"server1.example.com"}) {
		t.Fatalf("addresses %v (asked %v)", got, asked)
	}
	// A detected public address, or addresses the operator entered, need no lookup.
	asked = nil
	if got := moveTargetAddresses(agent.MoveHost{IPv4: "198.51.100.4"}, server, nil); !slices.Equal(got, []string{"198.51.100.4"}) || len(asked) != 0 {
		t.Fatalf("detected address %v (asked %v)", got, asked)
	}
	if got := moveTargetAddresses(agent.MoveHost{}, server, []string{"198.51.100.9"}); !slices.Equal(got, []string{"198.51.100.9"}) || len(asked) != 0 {
		t.Fatalf("entered address %v (asked %v)", got, asked)
	}
	// A server addressed by IP is not looked up either.
	if got := moveTargetAddresses(agent.MoveHost{}, Server{URL: "https://198.51.100.20"}, nil); !slices.Equal(got, []string{"198.51.100.20"}) || len(asked) != 0 {
		t.Fatalf("literal address %v (asked %v)", got, asked)
	}
}

// A destination's own readiness problems are in the review: what the agent
// cannot decide is a blocker before anything starts; what it repairs itself
// is announced, not blocking; problems of a service this application does
// not use there are not listed.
func TestMovePlanReportsDestinationProblemsBeforeStarting(t *testing.T) {
	stopped := `PostgreSQL 17 cluster "main" (port 5432) is stopped and its service did not fail, so it was stopped on purpose; it is left unchanged.`
	docker := "Docker is installed but its daemon is not running; containers cannot start."
	plan := func(t *testing.T, problems ...agent.MoveHostProblem) MovePlan {
		t.Helper()
		fx := newMoveFixture(t)
		fx.containerSource()
		fx.b.host.Problems = problems
		plan, status, err := fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta"})
		if err != nil || status != 200 || plan.TargetRuntime != moveRuntimeContainer {
			t.Fatal(status, err, plan.TargetRuntime)
		}
		return plan
	}
	has := func(list []string, text string) bool {
		return slices.ContainsFunc(list, func(item string) bool { return strings.Contains(item, text) })
	}
	// A container application brings its own database: the host's PostgreSQL is not its concern.
	p := plan(t, agent.MoveHostProblem{ID: "postgres", Detail: stopped}, agent.MoveHostProblem{ID: "docker", Detail: docker})
	if has(p.Blockers, "PostgreSQL") || has(p.Notes, "PostgreSQL") || !has(p.Blockers, "On the destination: "+docker) {
		t.Fatalf("blockers %v notes %v", p.Blockers, p.Notes)
	}
	p = plan(t, agent.MoveHostProblem{ID: "docker", Detail: docker, Repairable: true})
	if has(p.Blockers, "Docker") || !has(p.Notes, docker+" The destination repairs this itself") {
		t.Fatalf("repairable problem: blockers %v notes %v", p.Blockers, p.Notes)
	}
	if p = plan(t); has(p.Blockers, "On the destination") || has(p.Notes, "On the destination") {
		t.Fatalf("a ready destination was reported: %v %v", p.Blockers, p.Notes)
	}
}
