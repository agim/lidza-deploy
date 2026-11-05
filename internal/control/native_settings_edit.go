package control

import (
	"errors"
	"net/http"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/pkg/router"
)

func (c *Control) updateNativeSettings(w http.ResponseWriter, r *http.Request, a Application, p agent.SettingsPatch) {
	var current agent.Settings
	path := "/v1/native/apps/" + a.ID + "/settings"
	if err := c.agentCall(r, a.ServerID, "GET", path, nil, &current); err != nil || current.Native == nil || !sameNativeApplication(*a.Native, *current.Native) || current.Capabilities == nil || !current.Capabilities.Configure {
		agent.Fail(w, 409, errors.New("native settings require approved configuration ownership and unchanged registration"))
		return
	}
	for _, entry := range current.Environment {
		if entry.Kind == agent.EnvironmentCredential && p.EnvKindChanges[entry.Key] == agent.EnvironmentSetting {
			if err := auth.AuthorizeStepUp(r.Context(), r, "credential.reveal", a.ID+"/"+entry.Key); err != nil {
				router.WriteError(w, r, err)
				return
			}
		}
	}
	var saved agent.Settings
	if err := c.agentCall(r, a.ServerID, "PATCH", path, p, &saved); err != nil {
		agent.Fail(w, 409, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.data.Apps[a.ID]
	if !ok || old.Retiring || old.Generation != a.Generation {
		agent.Fail(w, 409, errors.New("application registration changed; refresh settings"))
		return
	}
	next := old
	next.Branch = saved.Branch
	next.Astro = saved.Astro
	c.data.Apps[a.ID] = next
	if err := c.save(); err != nil {
		c.data.Apps[a.ID] = old
		agent.Fail(w, 500, errors.New("agent settings saved; retry to synchronize dashboard metadata"))
		return
	}
	next.Secret = ""
	next.Previews.Env = nil
	agent.JSON(w, 200, next)
}
