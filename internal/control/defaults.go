package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/agim/lidza-deploy/internal/agent"
)

// defaultsView is one runtime's workspace defaults. The top level of the
// API response is the Līdza (Go) set, as before; Runtimes adds every set,
// including Go, keyed by runtime.
type defaultsView struct {
	Values   map[string]string       `json:"values"`
	Revision string                  `json:"revision"`
	Runtimes map[string]defaultsView `json:"runtimes,omitempty"`
}

// runtimeDefaultSeeds are offered until an administrator saves a runtime's
// set; a deliberately empty saved set stays empty.
var runtimeDefaultSeeds = map[string]map[string]string{
	"rails": {"RAILS_ENV": "production", "RACK_ENV": "production", "RAILS_LOG_TO_STDOUT": "1", "RAILS_SERVE_STATIC_FILES": "1"},
	"astro": {"NODE_ENV": "production", "ASTRO_TELEMETRY_DISABLED": "1"},
	"node":  {"NODE_ENV": "production"},
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

// runtimeDefaultsLocked returns the saved (or seeded) set of one runtime.
func (c *Control) runtimeDefaultsLocked(runtime string) map[string]string {
	if runtime == "go" || runtime == "" {
		return c.defaultsLocked()
	}
	if values, saved := c.data.RuntimeDefaults[runtime]; saved && values != nil {
		return maps.Clone(values)
	}
	return maps.Clone(runtimeDefaultSeeds[runtime])
}

func defaultsResponse(values map[string]string) defaultsView {
	raw, _ := json.Marshal(values)
	digest := sha256.Sum256(raw)
	return defaultsView{Values: values, Revision: hex.EncodeToString(digest[:])}
}

// runtimeDefaultsResponse binds the revision to the runtime as well, so a
// review of one runtime's set never confirms another with equal values.
func runtimeDefaultsResponse(runtime string, values map[string]string) defaultsView {
	if runtime == "go" || runtime == "" {
		return defaultsResponse(values)
	}
	raw, _ := json.Marshal(values)
	digest := sha256.Sum256(append([]byte("runtime:"+runtime+"\n"), raw...))
	return defaultsView{Values: values, Revision: hex.EncodeToString(digest[:])}
}

func (c *Control) defaultsViewLocked() defaultsView {
	view := defaultsResponse(c.defaultsLocked())
	view.Runtimes = map[string]defaultsView{}
	for _, runtime := range agent.DefaultsRuntimes {
		view.Runtimes[runtime] = runtimeDefaultsResponse(runtime, c.runtimeDefaultsLocked(runtime))
	}
	return view
}

// applicationDefaultsRuntime names the defaults set that applies to an
// application: its container runtime, or for a native application its
// registered runtime, with release-managed Node.js applications reported by
// the agent's release policy.
func applicationDefaultsRuntime(a Application, releaseRuntime string) string {
	if a.Native != nil {
		if releaseRuntime == "node" {
			return "node"
		}
		if a.Native.Runtime == "astro" {
			return "astro"
		}
		return "rails"
	}
	switch a.Runtime {
	case "rails", "astro", "node":
		return a.Runtime
	}
	return "go"
}

// deployDefaults selects the first-deployment defaults sent with a deploy
// request. Native applications keep receiving the Go set, which their
// publishers ignore; container applications receive their runtime's set.
func (c *Control) deployDefaults(a Application) (map[string]string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	runtime := "go"
	if a.Native == nil {
		runtime = applicationDefaultsRuntime(a, "")
	}
	if runtime == "go" {
		return c.defaultsLocked(), ""
	}
	return c.runtimeDefaultsLocked(runtime), runtime
}

func (c *Control) applicationDefaults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "GET" {
		c.mu.Lock()
		view := c.defaultsViewLocked()
		c.mu.Unlock()
		agent.JSON(w, 200, view)
		return
	}
	var input struct {
		Runtime string            `json:"runtime"`
		Values  map[string]string `json:"values"`
	}
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	runtime := input.Runtime
	if runtime == "" {
		runtime = "go"
	}
	if !slices.Contains(agent.DefaultsRuntimes, runtime) {
		agent.Fail(w, 400, errors.New("unsupported application runtime: "+runtime))
		return
	}
	if input.Values == nil {
		agent.Fail(w, 400, errors.New("values object is required"))
		return
	}
	if err := agent.ValidateRuntimeDefaults(runtime, input.Values); err != nil {
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
	if runtime == "go" {
		old := c.data.ApplicationDefaults
		c.data.ApplicationDefaults = maps.Clone(input.Values)
		if err := c.save(); err != nil {
			c.data.ApplicationDefaults = old
			agent.Fail(w, 500, err)
			return
		}
	} else {
		old := c.data.RuntimeDefaults
		next := maps.Clone(old)
		if next == nil {
			next = map[string]map[string]string{}
		}
		next[runtime] = maps.Clone(input.Values)
		c.data.RuntimeDefaults = next
		if err := c.save(); err != nil {
			c.data.RuntimeDefaults = old
			agent.Fail(w, 500, err)
			return
		}
	}
	agent.JSON(w, 200, c.defaultsViewLocked())
}

// The revision binds confirmation to the exact profile the operator reviewed:
// the set of the application's runtime, as it was when it was shown.
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
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	path, releaseRuntime := "/v1/apps/"+a.ID+"/settings", ""
	if a.Native != nil {
		// The agent decides whether this native application's environment is
		// editable (static publication or release policy) and which release
		// runtime it follows.
		path = "/v1/native/apps/" + url.PathEscape(a.ID) + "/settings"
		var current agent.Settings
		if err := c.agentCall(r, a.ServerID, "GET", path, nil, &current); err != nil || current.Native == nil || !sameNativeApplication(*a.Native, *current.Native) || current.Capabilities == nil || !current.Capabilities.Configure || !current.Capabilities.Environment {
			agent.Fail(w, 409, errors.New("native settings require approved configuration ownership and unchanged registration"))
			return
		}
		releaseRuntime = current.ReleaseRuntime
	}
	runtime := applicationDefaultsRuntime(a, releaseRuntime)
	c.mu.Lock()
	latest, ok := c.data.Apps[a.ID]
	profile := runtimeDefaultsResponse(runtime, c.runtimeDefaultsLocked(runtime))
	c.mu.Unlock()
	if !ok || latest.Retiring || latest.Generation != a.Generation {
		agent.Fail(w, 409, errors.New("application registration changed; review defaults again"))
		return
	}
	if input.Revision != profile.Revision {
		agent.Fail(w, 409, errors.New("workspace defaults changed; review them again"))
		return
	}
	limit := 6
	if runtime != "go" {
		limit = agent.MaxRuntimeDefaults
	}
	if len(input.Keys) == 0 || len(input.Keys) > limit {
		agent.Fail(w, 400, errors.New("select defaults to apply"))
		return
	}
	patch := agent.SettingsPatch{FromDefaults: true, OnlyMissing: !input.Replace, EnvChanges: map[string]*string{}}
	if a.Native == nil {
		patch.Branch, patch.Domain = latest.Branch, latest.Domain
	}
	for _, k := range input.Keys {
		v, ok := profile.Values[k]
		if !ok || v == "" {
			agent.Fail(w, 400, errors.New("unknown or empty default: "+k))
			return
		}
		patch.EnvChanges[k] = &v
	}
	if err := c.agentCall(r, a.ServerID, "PATCH", path, patch, nil); err != nil {
		code := 502
		if a.Native != nil {
			code = 409
		}
		agent.Fail(w, code, err)
		return
	}
	agent.JSON(w, 200, map[string]any{"saved": true, "runtime": runtime})
}
