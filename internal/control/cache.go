package control

import (
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
)

func (c *Control) cacheSettings(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	if r.Method == "GET" {
		var view agent.CacheView
		if err := c.agentCall(r, a.ServerID, "GET", "/v1/apps/"+a.ID+"/cache", nil, &view); err != nil {
			agent.Fail(w, 502, err)
			return
		}
		agent.JSON(w, 200, view)
		return
	}
	var in agent.CacheRequest
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if err := in.Validate(); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if err := c.agentCall(r, a.ServerID, "POST", "/v1/apps/"+a.ID+"/cache", in, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 202, map[string]string{"status": "provisioning"})
}
