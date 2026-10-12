package control

import (
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
)

func (c *Control) resources(w http.ResponseWriter, r *http.Request) {
	server := r.PathValue("server")
	if r.Method == "GET" {
		var report agent.ResourceReport
		if err := c.agentCall(r, server, "GET", "/v1/resources", nil, &report); err != nil {
			agent.Fail(w, 502, err)
			return
		}
		agent.JSON(w, 200, report)
		return
	}
	var policy agent.ResourcePolicy
	if err := agent.Decode(w, r, &policy); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if err := policy.Validate(); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if err := c.agentCall(r, server, "PUT", "/v1/resources", policy, nil); err != nil {
		agent.Fail(w, 409, err)
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "applied"})
}
func (c *Control) appResources(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var limits agent.AppResources
	if err := agent.Decode(w, r, &limits); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if err := limits.Validate(); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if err := c.agentCall(r, a.ServerID, "PUT", "/v1/apps/"+a.ID+"/resources", limits, nil); err != nil {
		agent.Fail(w, 409, err)
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "applied"})
}
