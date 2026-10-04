package control

import (
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
)

func (c *Control) settings(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var out agent.Settings
	if err := c.agentCall(r, a.ServerID, "GET", "/v1/apps/"+a.ID+"/settings", nil, &out); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 200, out)
}
func (c *Control) updateSettings(w http.ResponseWriter, r *http.Request) {
	var input agent.SettingsPatch
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.data.Apps[r.PathValue("id")]
	if !ok || old.Retiring {
		http.NotFound(w, r)
		return
	}
	spec := agent.App{ID: old.ID, Repository: old.Repository, Branch: input.Branch, Domain: input.Domain}
	if err := spec.Validate(); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	for id, a := range c.data.Apps {
		if id != old.ID && a.Domain == input.Domain {
			agent.Fail(w, 409, errors.New("domain already registered"))
			return
		}
	}
	if c.data.BackupStorage != nil {
		if err := c.agentCall(r, old.ServerID, "PUT", "/v1/backup-storage", c.data.BackupStorage, nil); err != nil {
			agent.Fail(w, 502, err)
			return
		}
	}
	if err := c.agentCall(r, old.ServerID, "PATCH", "/v1/apps/"+old.ID+"/settings", input, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	next := old
	next.Branch = input.Branch
	next.Domain = input.Domain
	c.data.Apps[old.ID] = next
	if err := c.save(); err != nil {
		c.data.Apps[old.ID] = old
		agent.Fail(w, 500, errors.New("agent settings saved but control metadata could not be saved; retry these settings to synchronize"))
		return
	}
	next.Secret = ""
	next.Previews.Env = nil
	agent.JSON(w, 200, next)
}

// Disabling does not depend on a working OAuth grant. GitHub may continue to
// deliver signed events; we acknowledge them without scheduling deployments.
func (c *Control) disableHook(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.data.Apps[r.PathValue("id")]
	if !ok || old.Retiring {
		http.NotFound(w, r)
		return
	}
	next := old
	next.AutoDeploy = false
	c.data.Apps[old.ID] = next
	if err := c.save(); err != nil {
		c.data.Apps[old.ID] = old
		agent.Fail(w, 500, err)
		return
	}
	next.Secret = ""
	next.Previews.Env = nil
	agent.JSON(w, 200, next)
}
