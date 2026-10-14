package control

import (
	"net/http"

	"github.com/agim/lidza-deploy/internal/agent"
)

func (c *Control) serverContainers(w http.ResponseWriter, r *http.Request) {
	server := r.PathValue("server")
	if r.Method == http.MethodGet {
		var out agent.ContainerInventory
		if err := c.agentCall(r, server, http.MethodGet, "/v1/containers", nil, &out); err != nil {
			agent.Fail(w, http.StatusBadGateway, err)
			return
		}
		agent.JSON(w, http.StatusOK, out)
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, http.StatusBadRequest, err)
		return
	}
	var out map[string]string
	if err := c.agentCall(r, server, http.MethodPost, "/v1/containers/restart", input, &out); err != nil {
		agent.Fail(w, http.StatusBadGateway, err)
		return
	}
	agent.JSON(w, http.StatusOK, out)
}
