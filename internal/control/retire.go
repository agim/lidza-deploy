package control

import (
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
	"net/url"
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
	if _, moving := c.activeMoveLocked(id); moving {
		agent.Fail(w, 409, errors.New("finish or abort the application's move before removing it"))
		return
	}
	// A native application stays as it is until the agent removed it: the
	// agent refuses applications it did not create, and those must keep
	// working in the panel.
	if !a.Retiring && a.Native == nil {
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
				failAgent(w, err)
				return
			}
			delete(c.data.Apps, child.ID)
		}
	}
	path := "/v1/apps/" + id
	if a.PreviewParent != "" {
		path = "/v1/previews/" + id
	}
	if a.Native != nil {
		// The agent removes applications it created: their units, account
		// and route. Their data services become a retained resource and
		// files stay on the server. Applications it adopted are refused.
		path = "/v1/native/apps/" + url.PathEscape(id)
	}
	// The retained resource records the move that brought the application
	// here, so the data is never mistaken for an unfinished move's copy.
	remove := agent.AppRemoveRequest{Origin: c.movedHereLocked(id, a.ServerID)}
	if err := c.agentCall(r, a.ServerID, "DELETE", path, remove, nil); err != nil {
		failAgent(w, err)
		return
	}
	delete(c.data.Apps, id)
	for _, kind := range []string{"database:", "health:", "deploy:", "security-burst:", "security-review:"} {
		delete(c.data.Incidents, kind+a.ServerID+":"+id)
	}
	if err := c.save(); err != nil {
		c.data.Apps[id] = a
		agent.Fail(w, 500, errors.New("agent removed application; retry removal to finish saving control metadata"))
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "removed", "note": "Remove the old repository webhook in GitHub if it is no longer needed."})
}

// movedHereLocked is the origin of an application that a switched move
// brought to serverID, or nil when no move did.
func (c *Control) movedHereLocked(appID, serverID string) *agent.RetainedOrigin {
	var latest *MoveJob
	for _, job := range c.data.Moves {
		if job.AppID == appID && job.TargetServer == serverID && job.Switched && (latest == nil || job.Updated.After(latest.Updated)) {
			job := job
			latest = &job
		}
	}
	if latest == nil {
		return nil
	}
	return moveRetainedOrigin(*latest, true)
}
