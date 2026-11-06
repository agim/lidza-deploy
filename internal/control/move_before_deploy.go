package control

import (
	"context"
	"net/http"
	"net/url"

	"github.com/agim/lidza-deploy/internal/agent"
)

// moveCarryBeforeDeploy sets the destination database's pre-deployment
// backup policy to the source's. A new database starts at always, the safe
// default, so a policy that cannot be carried is logged, not fatal.
func (c *Control) moveCarryBeforeDeploy(ctx context.Context, job MoveJob, d MoveDatabase, target string) {
	policy := d.BeforeDeploy.Normalized()
	if target == "" || policy.Mode == agent.BeforeDeployAlways {
		return
	}
	r := moveRequestFor(ctx)
	if err := c.agentCall(r, job.TargetServer, http.MethodPatch, "/v1/databases/"+url.PathEscape(target)+"/backups/before-deploy", policy, nil); err != nil {
		c.moveLog(job.ID, moveStepPrepare, "The before-deploy backup policy of %s (%s) could not be set on %s; it stays at always: %s", target, policy, job.TargetServer, moveAgentError(err))
		return
	}
	c.moveLog(job.ID, moveStepPrepare, "Database %s keeps its before-deploy backup policy on the destination: %s.", target, policy)
}
