package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/updates"
	"net/http"
	"time"
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
	case "state":
		var value agent.ApplicationState
		if err := agent.Decode(w, r, &value); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		in = value
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
		failAgent(w, err)
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
	if c.channelConfigured() {
		c.upgradeServerSigned(w, r)
		return
	}
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
			failAgent(w, err)
			return
		}
		agent.JSON(w, 202, map[string]string{"status": "queued"})
		return
	}
	if err := c.agentCall(r, r.PathValue("server"), r.Method, path, nil, &out); err != nil {
		failAgent(w, err)
		return
	}
	agent.JSON(w, 200, out)
}

// upgradeServerSigned upgrades one host from the signed channel: the
// newest verified signed bundle is transferred to the agent, which verifies
// it again, and its root helper installs it with rollback.
func (c *Control) upgradeServerSigned(w http.ResponseWriter, r *http.Request) {
	c.updateMu.Lock()
	defer c.updateMu.Unlock()
	server := c.serverByID(r.PathValue("server"))
	if server.ID == "" {
		agent.Fail(w, 404, errors.New("server not found"))
		return
	}
	var status agent.UpgradeStatus
	if err := c.agentCall(r, server.ID, "GET", "/v1/upgrade", nil, &status); err != nil {
		failAgent(w, err)
		return
	}
	settings := c.updateSettingsSnapshot()
	if r.Method == "GET" {
		if settings.Bundle == nil || time.Since(settings.Checked) > time.Hour {
			if err := c.checkUpdateRelease(r); err == nil {
				settings = c.updateSettingsSnapshot()
			}
		}
		latest := ""
		if settings.Bundle != nil {
			latest = settings.Bundle.Version
		}
		agent.JSON(w, 200, map[string]any{"current": status.Current, "latest": latest, "channel": "private", "state": status.State,
			"message": status.Message, "supported": status.Supported && latest != "" && updates.Newer(latest, status.Current)})
		return
	}
	if err := c.checkUpdateRelease(r); err != nil {
		failAgent(w, err)
		return
	}
	settings = c.updateSettingsSnapshot()
	if settings.Bundle == nil || !updates.ValidVersion(settings.Bundle.Version) {
		agent.Fail(w, 409, errors.New("no verified signed release is available"))
		return
	}
	if !status.Supported {
		agent.Fail(w, 409, errors.New(status.Message))
		return
	}
	if !updates.Newer(settings.Bundle.Version, status.Current) {
		agent.Fail(w, 409, fmt.Errorf("%s already runs %s", server.Name, status.Current))
		return
	}
	if err := c.transferBundle(r, server, *settings.Bundle, agentArch(status)); err != nil {
		failAgent(w, err)
		return
	}
	if err := c.agentCall(r, server.ID, "POST", "/v1/upgrade", map[string]string{"version": settings.Bundle.Version}, nil); err != nil {
		failAgent(w, err)
		return
	}
	agent.JSON(w, 202, map[string]string{"status": "queued", "version": settings.Bundle.Version})
}
