package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/agim/lidza-deploy/internal/agent"
)

func (c *Control) deployment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, result := range c.readFleet(r, "/v1/deployments/"+url.PathEscape(id)) {
		if result.Err != nil {
			continue
		}
		var row agent.Deployment
		if err := json.Unmarshal(result.Body, &row); err != nil || row.ID != id {
			continue
		}
		agent.JSON(w, 200, struct {
			agent.Deployment
			ServerID string `json:"server_id"`
		}{row, result.Server.ID})
		return
	}
	agent.Fail(w, 502, errors.New("deployment detail unavailable; check hosting-agent connectivity and upgrade the agent to support individual build logs"))
}
