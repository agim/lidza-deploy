package control

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Application lifecycle: POST /api/control/apps/{id}/stop, /start and
// /restart work for every runtime; the agent does the work
// (agent/app_lifecycle.go). They follow the rules of deploys and moves: an
// application that is moving (any phase, not only the final sync) is
// refused, and the agent refuses while a deployment or another operation
// on the application runs, saying why.

var agentLifecycleActions = []string{"stop", "start", "restart"}

var appLifecyclePatterns = func() map[string]bool {
	out := map[string]bool{}
	for _, action := range agentLifecycleActions {
		out["POST /api/control/apps/{id}/"+action] = true
	}
	return out
}()

const appLifecycleAgentTimeout = 3 * time.Minute

func (c *Control) appLifecycle(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		a, ok := c.app(id)
		if !ok || a.Retiring {
			http.NotFound(w, r)
			return
		}
		if job, moving := c.activeMove(id); moving {
			agent.Fail(w, http.StatusConflict, fmt.Errorf("%s is moving to %s; finish or abort the move before you %s it", id, c.serverName(job.TargetServer), action))
			return
		}
		slow := r.WithContext(withAgentTimeout(r.Context(), appLifecycleAgentTimeout))
		var result agent.AppLifecycleResult
		err := c.agentCall(slow, a.ServerID, http.MethodPost, "/v1/apps/"+id+"/"+action, agent.AppLifecycleRequest{By: currentUserID(r.Context())}, &result)
		var refused *agentHTTPError
		if errors.As(err, &refused) && refused.status == http.StatusNotFound && refused.message == "" {
			if a.Native != nil {
				agent.Fail(w, http.StatusConflict, fmt.Errorf("the server's agent cannot %s native applications yet; update it first", action))
				return
			}
			// An agent from before these routes: container applications
			// have the state and reload routes.
			result, err = c.legacyContainerLifecycle(slow, a, action)
		}
		if err != nil {
			failAgent(w, err)
			return
		}
		recordArchiveAudit(r.Context(), "app."+action, id, map[string]string{"server": a.ServerID, "status": result.Status})
		agent.JSON(w, http.StatusOK, result)
	}
}

func (c *Control) legacyContainerLifecycle(r *http.Request, a Application, action string) (agent.AppLifecycleResult, error) {
	result := agent.AppLifecycleResult{AppID: a.ID}
	if action == "restart" {
		var d agent.Deployment
		if err := c.agentCall(r, a.ServerID, http.MethodPost, "/v1/apps/"+a.ID+"/reload", nil, &d); err != nil {
			return result, err
		}
		result.Status, result.Deployment = "restarting", &d
		return result, nil
	}
	stopped := action == "stop"
	if err := c.agentCall(r, a.ServerID, http.MethodPut, "/v1/apps/"+a.ID+"/state", agent.ApplicationState{Stopped: &stopped}, nil); err != nil {
		return result, err
	}
	result.Status = map[bool]string{true: "stopped", false: "starting"}[stopped]
	return result, nil
}
