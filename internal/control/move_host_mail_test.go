package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

// postfixDestination makes the destination's Postfix hold port 25 on every
// address, as Ubuntu's default installation does.
func (fx *moveFixture) postfixDestination(external, relayed int) {
	fx.b.host.ListeningPorts = append(fx.b.host.ListeningPorts, agent.ListeningPort{Port: 25, Protocol: "tcp", Process: "master"})
	fx.b.host.HostMail = &agent.MoveHostMail{Ports: []agent.StackPort{{Port: 25, Protocol: "tcp"}}, Process: "master", MTA: "postfix", InetInterfaces: "all",
		ExternalConnections7d: external, RelayedOut7d: relayed, LogsRead: true, LogSource: "/var/log/mail.log"}
}

const moveHostMailWarning = "Port 25/tcp is held by the destination's host mail server (Postfix), which only delivers local system mail. Prepare binds it to loopback (inet_interfaces = loopback-only), so local system mail keeps working and the port is free for shop."

func TestMovePlanBindsALocalOnlyHostPostfixToLoopback(t *testing.T) {
	fx := newMoveFixture(t)
	fx.mailSource()
	fx.postfixDestination(0, 0)
	plan := mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if containing(plan.Blockers, "25/tcp") || !containing(plan.Warnings, moveHostMailWarning) {
		t.Fatalf("blockers %v warnings %v", plan.Blockers, plan.Warnings)
	}
	if plan.HostMail == nil || !plan.HostMail.Loopback || movePortLabels(plan.HostMail.Ports) != "25/tcp" {
		t.Fatalf("host mail %+v", plan.HostMail)
	}
	if !containing(plan.Notes, "The destination publishes port 25/tcp, 587/tcp, 8443/tcp") {
		t.Fatalf("the freed port is published: %v", plan.Notes)
	}
	// Without readable mail logs the panel cannot tell: a blocker.
	fx.b.host.HostMail.LogsRead = false
	plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if plan.HostMail != nil || !containing(plan.Blockers, "Port 25/tcp is in use on the destination by its host mail server (Postfix); its mail log of the last 7 days could not be read") {
		t.Fatalf("blockers %v", plan.Blockers)
	}
}

func TestMovePlanKeepsTheBlockerForAHostPostfixThatServesMail(t *testing.T) {
	fx := newMoveFixture(t)
	fx.mailSource()
	fx.postfixDestination(3, 0)
	plan := mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if plan.HostMail != nil || containing(plan.Warnings, "Prepare binds it to loopback") ||
		!containing(plan.Blockers, "Port 25/tcp is in use on the destination by its host mail server: Postfix on the destination accepted 3 outside connections in the last 7 days") {
		t.Fatalf("blockers %v warnings %v", plan.Blockers, plan.Warnings)
	}
	fx.b.host.HostMail.ExternalConnections7d, fx.b.host.HostMail.RelayedOut7d, fx.b.host.HostMail.Relayhost = 0, 1, "[smtp.example.net]:587"
	plan = mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	if !containing(plan.Blockers, "Postfix on the destination sent 1 message to other mail servers (relayhost [smtp.example.net]:587) in the last 7 days") {
		t.Fatalf("blockers %v", plan.Blockers)
	}
	if status, _ := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true}); status != 409 {
		t.Fatalf("a Postfix that serves mail must stop the move: %d", status)
	}
}

func TestMovePlanNamesOtherPortHolders(t *testing.T) {
	fx := newMoveFixture(t)
	fx.mailSource()
	fx.b.host.ListeningPorts = append(fx.b.host.ListeningPorts, agent.ListeningPort{Port: 25, Protocol: "tcp", Process: "exim4"}, agent.ListeningPort{Port: 8443, Protocol: "tcp", Process: "caddy"})
	fx.b.host.HostMail = &agent.MoveHostMail{Ports: []agent.StackPort{{Port: 25, Protocol: "tcp"}}, Process: "exim4", MTA: "exim"}
	plan := mailPlanFor(t, fx, moveStartInput{TargetServer: "beta"})
	for _, want := range []string{
		"Port 25/tcp is in use on the destination by the host mail server Exim (exim4); free it there or move the application elsewhere.",
		"Port 8443/tcp is in use on the destination by process caddy; free it there or move the application elsewhere.",
	} {
		if !containing(plan.Blockers, want) {
			t.Fatalf("missing blocker %q in %v", want, plan.Blockers)
		}
	}
	if plan.HostMail != nil {
		t.Fatalf("only Postfix is bound to loopback: %+v", plan.HostMail)
	}
}

func TestMovePrepareBindsTheHostPostfixToLoopback(t *testing.T) {
	fx := newMoveFixture(t)
	fx.mailSource()
	fx.postfixDestination(0, 0)
	var requests []agent.HostMailLoopbackRequest
	fx.b.mux.HandleFunc("POST /v1/host/mail/loopback", func(w http.ResponseWriter, r *http.Request) {
		var in agent.HostMailLoopbackRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		requests = append(requests, in)
		agent.JSON(w, 200, agent.HostMailLoopbackResult{Changed: true, Backup: "/etc/postfix/main.cf.lidza-20261106T120000Z", Summary: "Postfix listens on loopback only (inet_interfaces = loopback-only); local system mail keeps working and port 25/tcp is free."})
	})
	if status, _ := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"}); status != 409 {
		t.Fatalf("the loopback warning needs an acknowledgement: %d", status)
	}
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 && status != 200 {
		t.Fatalf("start %d", status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if len(requests) != 1 || requests[0].Trigger != "move" || movePortLabels(requests[0].Ports) != "25/tcp" || !strings.Contains(requests[0].Reference, view.ID) {
		t.Fatalf("loopback requests %+v", requests)
	}
	var lines []string
	for _, step := range job.Steps {
		if step.Key == moveStepPrepare {
			for _, line := range step.Log {
				lines = append(lines, line.Message)
			}
		}
	}
	if !containing(lines, "Binding the destination's host mail server (Postfix) to loopback to free port 25/tcp.") || !containing(lines, "local system mail keeps working") {
		t.Fatalf("prepare log %+v", job.Steps)
	}
}

func TestMovePlanNamesThePlaceholderServerByItsRole(t *testing.T) {
	if got := moveServerLabel(Server{ID: "local", Name: "This server"}, "source"); got != "the source server (This server)" {
		t.Fatal(got)
	}
	if got := moveServerLabel(Server{ID: "alpha", Name: "Alpha"}, "source"); got != "Alpha" {
		t.Fatal(got)
	}
	if got := moveServerLabel(Server{ID: "alpha"}, "destination"); got != "alpha" {
		t.Fatal(got)
	}
}
