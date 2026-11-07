package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/agim/lidza-deploy/internal/agent"
)

// hostAdminRoutes proxies per-server host updates, reboots, the managed
// firewall, the web ingress setting, the removal of unused Bundler
// versions and resource tuning (settings, overrides, Redis moves) to the
// server's agent. Mutations pass the panel's origin check,
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
		"GET /api/control/servers/{server}/host/ingress",
		"PUT /api/control/servers/{server}/host/ingress",
		"GET /api/control/servers/{server}/host/ruby/bundlers",
		"POST /api/control/servers/{server}/host/ruby/bundlers/remove",
		"GET /api/control/servers/{server}/host/resources",
		"PUT /api/control/servers/{server}/host/resources/overrides",
		"POST /api/control/servers/{server}/host/resources/apply",
		"POST /api/control/servers/{server}/host/resources/redis/{app}/migrate",
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
	if r.Method == http.MethodGet && (rest == "/host/updates" || rest == "/host/ingress" || rest == "/host/resources") && r.URL.Query().Get("refresh") == "1" {
		path += "?refresh=1"
	}
	var input any
	accepted := false
	switch {
	case rest == "/host/resources/overrides":
		var overrides agent.TuningOverrides
		if err := agent.Decode(w, r, &overrides); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		if err := overrides.Validate(); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		input = overrides
	case rest == "/host/resources/apply":
		var apply agent.ResourceTuningApply
		if err := agent.Decode(w, r, &apply); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		input, accepted = apply, !apply.DryRun
	case strings.HasPrefix(rest, "/host/resources/redis/"):
		if !databaseResourceID.MatchString(r.PathValue("app")) {
			agent.Fail(w, 400, errors.New("invalid application id"))
			return
		}
		var migration agent.RedisMigrationRequest
		if err := agent.Decode(w, r, &migration); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		if migration.Confirm != r.PathValue("app") {
			agent.Fail(w, 400, errors.New("confirm with the application id"))
			return
		}
		input, accepted = migration, true
	case rest == "/host/ingress" && r.Method == http.MethodPut:
		var settings agent.HostIngressSettings
		if err := agent.Decode(w, r, &settings); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		if err := settings.Validate(); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		input = settings
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
	case rest == "/host/ruby/bundlers/remove":
		var removal agent.BundlerRemoval
		if err := agent.Decode(w, r, &removal); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		if err := removal.Validate(); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		input = removal
	}
	var out json.RawMessage
	if err := c.agentCall(r, r.PathValue("server"), r.Method, path, input, &out); err != nil {
		failAgent(w, err)
		return
	}
	status := http.StatusOK
	if accepted || rest == "/host/updates/run" || rest == "/host/reboot" || rest == "/host/ruby/bundlers/remove" {
		status = http.StatusAccepted
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}
