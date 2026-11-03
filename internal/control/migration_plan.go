package control

import (
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
)

func (c *Control) migrationPlan(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var plan agent.MigrationPlan
	if err := c.agentCall(r, a.ServerID, "GET", "/v1/apps/"+a.ID+"/migration-plan", nil, &plan); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, plan)
}
