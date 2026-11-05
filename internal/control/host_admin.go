package control

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/agim/lidza-deploy/internal/agent"
)

// hostAdminRoutes proxies per-server host updates, reboots and the managed
// firewall to the server's agent. Mutations pass the panel's origin check,
// role check and audit trail like every other server mutation.
func (c *Control) hostAdminRoutes(handle func(string, http.HandlerFunc)) {
	for _, pattern := range []string{
		"GET /api/control/servers/{server}/host/updates",
		"PATCH /api/control/servers/{server}/host/updates",
		"POST /api/control/servers/{server}/host/updates/run",
		"POST /api/control/servers/{server}/host/reboot",
		"GET /api/control/servers/{server}/host/firewall",
		"PUT /api/control/servers/{server}/host/firewall",
		"GET /api/control/servers/{server}/host/firewall/plan",
		"POST /api/control/servers/{server}/host/firewall/apply",
		"POST /api/control/servers/{server}/host/firewall/confirm",
	} {
		handle(pattern, c.hostAdmin)
	}
}

func (c *Control) hostAdmin(w http.ResponseWriter, r *http.Request) {
	// Firewall rules name trusted networks; reading them needs the same
	// permission as changing them, not just fleet read access.
	if strings.Contains(r.URL.Path, "/host/firewall") && r.Method == http.MethodGet {
		if err := fleetRoles.Check(r.Context(), fleetScope, "infrastructure.manage"); err != nil {
			permissionError(w, err)
			return
		}
	}
	_, rest, _ := strings.Cut(r.URL.Path, "/servers/"+r.PathValue("server"))
	path := "/v1" + rest
	if r.Method == http.MethodGet && rest == "/host/updates" && r.URL.Query().Get("refresh") == "1" {
		path += "?refresh=1"
	}
	var input any
	switch {
	case r.Method == http.MethodPatch:
		var policy agent.HostUpdatesPolicy
		if err := agent.Decode(w, r, &policy); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		if err := policy.Validate(); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		input = policy
	case rest == "/host/updates/run":
		var run agent.HostUpdatesRun
		if err := agent.Decode(w, r, &run); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		input = run
	case rest == "/host/reboot":
		var reboot agent.HostRebootRequest
		if err := agent.Decode(w, r, &reboot); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		input = reboot
	case r.Method == http.MethodPut:
		var model agent.HostFirewallModel
		if err := agent.Decode(w, r, &model); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		input = model
	case rest == "/host/firewall/apply":
		var apply agent.HostFirewallApply
		if err := agent.Decode(w, r, &apply); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		input = apply
	case rest == "/host/firewall/confirm":
		input = struct{}{}
	}
	var out json.RawMessage
	if err := c.agentCall(r, r.PathValue("server"), r.Method, path, input, &out); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	status := http.StatusOK
	if rest == "/host/updates/run" || rest == "/host/reboot" {
		status = http.StatusAccepted
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}
