package control

import (
	"net/http"

	"github.com/agim/lidza-deploy/internal/agent"
)

func (c *Control) appErrors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var out agent.AppErrors
	if err := c.agentCall(r, a.ServerID, "GET", "/v1/apps/"+a.ID+"/errors", nil, &out); err != nil {
		agent.Fail(w, http.StatusBadGateway, err)
		return
	}
	agent.JSON(w, http.StatusOK, out)
}
