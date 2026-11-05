package control

import (
	"context"
	"errors"
	"net/http"

	"github.com/agim/lidza-deploy/internal/agent"
)

func init() {
	alternatePushDispatch = dispatchNativePush
}

// dispatchNativePush deploys a native application on push. Static sites go
// through the approved static adapter; every other native application
// deploys only while the agent reports a reviewed release policy for it.
// A general takeover never authorizes a service action on its own.
func dispatchNativePush(ctx context.Context, c *Control, a Application, push pushJob) (bool, error) {
	if a.Native == nil {
		return false, nil
	}
	r, _ := http.NewRequestWithContext(ctx, "GET", c.cfg.PublicURL, nil)
	var status agent.NativeStatus
	path := "/v1/native/apps/" + a.ID
	if err := c.agentCall(r, a.ServerID, "GET", path, nil, &status); err != nil {
		return true, err
	}
	if !sameNativeApplication(*a.Native, status.Application) {
		return true, errors.New("native publication binding changed; review ownership before dispatch")
	}
	if !status.DeploymentSupported || a.Native.Runtime != "astro" && !status.ReleaseManaged {
		return true, nil
	}
	issue := c.deploymentCredentials
	if c.credentialsFixture != nil {
		issue = c.credentialsFixture
	}
	credentials, err := issue(ctx, a, "github:"+push.Delivery)
	if err != nil {
		return true, err
	}
	var deployment agent.Deployment
	return true, c.agentCall(r, a.ServerID, "POST", path+"/deploy", credentials, &deployment)
}
