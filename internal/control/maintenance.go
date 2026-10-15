package control

import (
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
)

func (c *Control) serverMaintenance(w http.ResponseWriter, r *http.Request) {
	var input any
	path := "/v1/maintenance"
	switch r.Method {
	case http.MethodPatch:
		var policy agent.MaintenancePolicy
		if err := agent.Decode(w, r, &policy); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		if err := policy.Validate(); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		input = policy
	case http.MethodPost:
		var request agent.CleanupRequest
		if err := agent.Decode(w, r, &request); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		input = request
		path += "/run"
	}
	var out agent.MaintenanceState
	if err := c.agentCall(r, r.PathValue("server"), r.Method, path, input, &out); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusAccepted
	}
	agent.JSON(w, status, out)
}
