package control

import (
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
	"net/url"
)

func (c *Control) appPorts(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var out agent.PortInventory
	if err := c.agentCall(r, a.ServerID, "GET", "/v1/apps/"+url.PathEscape(a.ID)+"/ports", nil, &out); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 200, out)
}
