package control

import (
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
)

func (c *Control) retire(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Confirm string `json:"confirm"`
	}
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	id := r.PathValue("id")
	if input.Confirm != id {
		agent.Fail(w, 400, errors.New("confirm the application ID to remove it"))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.data.Apps[id]
	if !ok {
		agent.JSON(w, 200, map[string]string{"status": "removed"})
		return
	}
	if !a.Retiring {
		old := a
		a.Retiring = true
		a.AutoDeploy = false
		c.data.Apps[id] = a
		if err := c.save(); err != nil {
			c.data.Apps[id] = old
			agent.Fail(w, 500, err)
			return
		}
	}
	for _, child := range c.data.Apps {
		if child.PreviewParent == id {
			if err := c.agentCall(r, child.ServerID, "DELETE", "/v1/previews/"+child.ID, nil, nil); err != nil {
				agent.Fail(w, 502, err)
				return
			}
			delete(c.data.Apps, child.ID)
		}
	}
	path := "/v1/apps/" + id
	if a.PreviewParent != "" {
		path = "/v1/previews/" + id
	}
	if err := c.agentCall(r, a.ServerID, "DELETE", path, nil, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	delete(c.data.Apps, id)
	for _, kind := range []string{"database:", "health:", "deploy:"} {
		delete(c.data.Incidents, kind+a.ServerID+":"+id)
	}
	if err := c.save(); err != nil {
		c.data.Apps[id] = a
		agent.Fail(w, 500, errors.New("agent removed application; retry removal to finish saving control metadata"))
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "removed", "note": "Remove the old repository webhook in GitHub if it is no longer needed."})
}
