package control

import (
	"encoding/json"
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
)

func (c *Control) appFeature(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	action := r.PathValue("feature")
	var in any
	switch action {
	case "maintenance":
		var value agent.Maintenance
		if err := agent.Decode(w, r, &value); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		in = value
	case "restore":
		var value agent.RestoreRequest
		if err := agent.Decode(w, r, &value); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		in = value
	default:
		http.NotFound(w, r)
		return
	}
	if err := c.agentCall(r, a.ServerID, r.Method, "/v1/apps/"+a.ID+"/"+action, in, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 202, map[string]string{"status": "accepted"})
}
func (c *Control) serverHealth(w http.ResponseWriter, r *http.Request) {
	type view struct {
		ServerID string `json:"server_id"`
		agent.ServerHealth
	}
	list := []view{}
	unavailable := []string{}
	for _, result := range c.readFleet(r, "/v1/server-health") {
		var h agent.ServerHealth
		if result.Err != nil || json.Unmarshal(result.Body, &h) != nil {
			unavailable = append(unavailable, result.Server.Name)
			continue
		}
		list = append(list, view{result.Server.ID, h})
	}
	agent.JSON(w, 200, map[string]any{"servers": list, "unavailable_servers": unavailable})
}

func (c *Control) upgradeServer(w http.ResponseWriter, r *http.Request) {
	var in any
	if r.Method == "POST" {
		var value struct {
			Version string `json:"version"`
		}
		if err := agent.Decode(w, r, &value); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		in = value
	}
	path := "/v1/upgrade"
	if r.Method == "GET" {
		path += "?check=1"
	}
	var out agent.UpgradeStatus
	if r.Method == "POST" {
		if err := c.agentCall(r, r.PathValue("server"), r.Method, path, in, nil); err != nil {
			agent.Fail(w, 502, err)
			return
		}
		agent.JSON(w, 202, map[string]string{"status": "queued"})
		return
	}
	if err := c.agentCall(r, r.PathValue("server"), r.Method, path, nil, &out); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 200, out)
}
