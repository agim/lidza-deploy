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
	var saved struct {
		agent.Settings
		Apply *agent.SettingsApplyResult `json:"apply"`
	}
	if err := c.agentCall(r, a.ServerID, "PATCH", path, p, &saved); err != nil {
		agent.Fail(w, 409, err)
		return
	}
	next, err := c.recordNativeSettings(a, saved.Settings)
	if err != nil {
		agent.Fail(w, 409, err)
		return
	}
	agent.JSON(w, 200, SettingsSaved{Application: next, Apply: c.finishSettingsApply(r, a.ID, saved.Apply)})
}

// recordNativeSettings keeps the panel's branch and Astro settings in step
// with the agent's saved ones.
func (c *Control) recordNativeSettings(a Application, saved agent.Settings) (Application, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.data.Apps[a.ID]
	if !ok || old.Retiring || old.Generation != a.Generation {
		return Application{}, errors.New("application registration changed; refresh settings")
	}
	next := old
	next.Branch = saved.Branch
	next.Astro = saved.Astro
	c.data.Apps[a.ID] = next
	if err := c.save(); err != nil {
		c.data.Apps[a.ID] = old
		return Application{}, errors.New("agent settings saved; retry to synchronize dashboard metadata")
	}
	next.Secret = ""
	next.Previews.Env = nil
	return next, nil
}
