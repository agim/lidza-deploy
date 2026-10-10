package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"

	"github.com/agim/lidza-deploy/internal/agent"
)

type defaultsView struct {
	Values   map[string]string `json:"values"`
	Revision string            `json:"revision"`
}

func (c *Control) applicationDefaultsSnapshot() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.defaultsLocked()
}
func (c *Control) defaultsLocked() map[string]string {
	if c.data.ApplicationDefaults == nil {
		return map[string]string{"DB_MIGRATE": "true", "LOG_LEVEL": "info"}
	}
	return maps.Clone(c.data.ApplicationDefaults)
}
func defaultsResponse(values map[string]string) defaultsView {
	raw, _ := json.Marshal(values)
	digest := sha256.Sum256(raw)
	return defaultsView{values, hex.EncodeToString(digest[:])}
}
func (c *Control) applicationDefaults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "GET" {
		agent.JSON(w, 200, defaultsResponse(c.applicationDefaultsSnapshot()))
		return
	}
	var input struct {
		Values map[string]string `json:"values"`
	}
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if input.Values == nil {
		agent.Fail(w, 400, errors.New("values object is required"))
		return
	}
	if err := agent.ValidateAppDefaults(input.Values); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	for k, v := range input.Values {
		v = strings.TrimSpace(v)
		if v == "" {
			delete(input.Values, k)
		} else {
			input.Values[k] = v
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.data.ApplicationDefaults
	c.data.ApplicationDefaults = maps.Clone(input.Values)
	if err := c.save(); err != nil {
		c.data.ApplicationDefaults = old
		agent.Fail(w, 500, err)
		return
	}
	agent.JSON(w, 200, defaultsResponse(c.defaultsLocked()))
}

// The revision binds confirmation to the exact profile the operator reviewed.
func (c *Control) applyDefaults(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Keys     []string `json:"keys"`
		Revision string   `json:"revision"`
		Replace  bool     `json:"replace"`
	}
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.data.Apps[r.PathValue("id")]
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	profile := defaultsResponse(c.defaultsLocked())
	if input.Revision != profile.Revision {
		agent.Fail(w, 409, errors.New("workspace defaults changed; review them again"))
		return
	}
	if len(input.Keys) == 0 || len(input.Keys) > 6 {
		agent.Fail(w, 400, errors.New("select defaults to apply"))
		return
	}
	patch := agent.SettingsPatch{Branch: a.Branch, Domain: a.Domain, FromDefaults: true, OnlyMissing: !input.Replace, EnvChanges: map[string]*string{}}
	for _, k := range input.Keys {
		v, ok := profile.Values[k]
		if !ok || v == "" {
			agent.Fail(w, 400, errors.New("unknown or empty default: "+k))
			return
		}
		patch.EnvChanges[k] = &v
	}
	if err := c.agentCall(r, a.ServerID, "PATCH", "/v1/apps/"+a.ID+"/settings", patch, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 200, map[string]bool{"saved": true})
}
