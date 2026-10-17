package control

import (
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/pkg/router"
	"net/http"
	"net/url"
	"slices"
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
	if out.ID == "" {
		out.ID = a.ID
	}
	if out.Branch == "" {
		out.Branch = a.Branch
	}
	if out.Domain == "" {
		out.Domain = a.Domain
	}
	agent.JSON(w, 200, struct {
		agent.Settings
		ServerID           string `json:"server_id"`
		Repository         string `json:"repository"`
		GitHubInstallation int64  `json:"github_installation,omitempty"`
	}{Settings: out, ServerID: a.ServerID, Repository: a.Repository, GitHubInstallation: a.GitHubInstallation})
}

func (c *Control) revealCredential(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	key := r.PathValue("key")
	if key == "" {
		agent.Fail(w, http.StatusBadRequest, errors.New("environment variable is required"))
		return
	}
	target := a.ID + "/" + key
	if err := auth.AuthorizeStepUp(r.Context(), r, "credential.reveal", target); err != nil {
		router.WriteError(w, r, err)
		return
	}
	var out struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	path := "/v1/apps/" + url.PathEscape(a.ID) + "/environment/" + url.PathEscape(key)
	if err := c.agentCall(r, a.ServerID, "GET", path, nil, &out); err != nil {
		agent.Fail(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, http.StatusOK, out)
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
	spec := agent.App{ID: old.ID, Repository: old.Repository, Branch: input.Branch, Domain: input.Domain, AdditionalDomains: old.AdditionalDomains}
	if input.AdditionalDomains != nil {
		spec.AdditionalDomains = slices.Clone(*input.AdditionalDomains)
	}
	if err := spec.Validate(); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	for id, a := range c.data.Apps {
		if id != old.ID && spec.DomainsOverlap(agent.App{Domain: a.Domain, AdditionalDomains: a.AdditionalDomains}) {
			agent.Fail(w, 409, errors.New("domain already registered"))
			return
		}
	}
	if len(input.EnvKindChanges) > 0 {
		var current agent.Settings
		if err := c.agentCall(r, old.ServerID, "GET", "/v1/apps/"+old.ID+"/settings", nil, &current); err != nil {
			agent.Fail(w, 502, errors.New("current environment classifications unavailable"))
			return
		}
		for _, entry := range current.Environment {
			if entry.Kind == agent.EnvironmentCredential && input.EnvKindChanges[entry.Key] == agent.EnvironmentSetting {
				if err := auth.AuthorizeStepUp(r.Context(), r, "credential.reveal", old.ID+"/"+entry.Key); err != nil {
					router.WriteError(w, r, err)
					return
				}
			}
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
	if input.CustomerHosts != nil {
		next.CustomerHosts = *input.CustomerHosts
	}
	next.Branch = input.Branch
	next.Domain = input.Domain
	next.AdditionalDomains = spec.AdditionalDomains
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

// Disabling does not depend on a working GitHub connection. GitHub may continue to
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
