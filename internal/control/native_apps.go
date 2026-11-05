package control

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/agim/lidza-deploy/internal/agent"
)

// sameNativeApplication reports whether two registrations describe the same
// native application identity, so a refreshed agent view never re-binds a
// panel application to a different service set, hostname or repository.
func sameNativeApplication(a, b agent.NativeApplication) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Domain == b.Domain && a.Repository == b.Repository && a.Branch == b.Branch && a.Port == b.Port && a.HealthPath == b.HealthPath && a.Runtime == b.Runtime && a.DocumentRoot == b.DocumentRoot && slices.Equal(a.AdditionalDomains, b.AdditionalDomains) && slices.Equal(a.Services, b.Services)
}

func (c *Control) nativeContainerGuard(pattern string, next http.HandlerFunc) http.HandlerFunc {
	if !strings.Contains(pattern, " /api/control/apps/{id}") {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if app, found := c.app(r.PathValue("id")); found && app.Native != nil {
			// Moves handle both runtimes through their own agent endpoints.
			if pattern == "DELETE /api/control/apps/{id}/webhook" || strings.HasSuffix(pattern, "/{id}/move") || strings.HasSuffix(pattern, "/{id}/move-plan") {
				next(w, r)
				return
			}
			if app.Native.Runtime != "astro" && (pattern == "POST /api/control/apps/{id}/deploy" || pattern == "POST /api/control/apps/{id}/rollback" || pattern == "POST /api/control/apps/{id}/github-app" || pattern == "POST /api/control/apps/{id}/webhook" ||
				pattern == "PATCH /api/control/apps/{id}/settings" || pattern == "POST /api/control/apps/{id}/apply-defaults" || pattern == "POST /api/control/apps/{id}/environment/{key}/reveal") {
				// Release-managed native applications deploy and roll back through
				// the agent's release engine; the agent decides from its reviewed
				// host policy, never from the control panel alone.
				var status agent.NativeStatus
				if c.agentCall(r, app.ServerID, "GET", "/v1/native/apps/"+app.ID, nil, &status) != nil || !status.ReleaseManaged || !sameNativeApplication(*app.Native, status.Application) {
					agent.Fail(w, 409, errors.New("native deployment requires a reviewed release policy on the host"))
					return
				}
				if pattern == "POST /api/control/apps/{id}/webhook" && app.GitHubInstallation == 0 {
					agent.Fail(w, 409, errors.New("pair this native repository with the existing verified GitHub App before enabling auto-deploy"))
					return
				}
				next(w, r)
				return
			}
			if app.Native.Runtime == "astro" && (pattern == "POST /api/control/apps/{id}/github-app" || pattern == "POST /api/control/apps/{id}/webhook") {
				var status agent.NativeStatus
				if c.agentCall(r, app.ServerID, "GET", "/v1/native/apps/"+app.ID, nil, &status) != nil || !status.DeploymentSupported || !sameNativeApplication(*app.Native, status.Application) {
					agent.Fail(w, 409, errors.New("native auto-deploy requires approved static publication ownership"))
					return
				}
				if pattern == "POST /api/control/apps/{id}/webhook" && app.GitHubInstallation == 0 {
					agent.Fail(w, 409, errors.New("pair this native repository with the existing verified GitHub App before enabling auto-deploy"))
					return
				}
				next(w, r)
				return
			}
			if app.Native.Runtime == "astro" && (pattern == "PATCH /api/control/apps/{id}/settings" || pattern == "POST /api/control/apps/{id}/apply-defaults" || pattern == "POST /api/control/apps/{id}/environment/{key}/reveal") {
				next(w, r)
				return
			}
			if pattern == "GET /api/control/apps/{id}/settings" || pattern == "GET /api/control/apps/{id}/logs" || pattern == "GET /api/control/apps/{id}/processes" {
				next(w, r)
				return
			}
			if app.Native.Runtime == "astro" && (pattern == "POST /api/control/apps/{id}/deploy" || pattern == "POST /api/control/apps/{id}/rollback") {
				next(w, r)
				return
			}
			agent.Fail(w, 409, errors.New("this app uses existing native services; container deployment, configuration and routing actions require a separate approved native change plan"))
			return
		}
		next(w, r)
	}
}

func (c *Control) nativeDiscovery(w http.ResponseWriter, r *http.Request) {
	var apps []agent.NativeApplication
	if err := c.agentCall(r, r.PathValue("server"), "GET", "/v1/native/discovery", nil, &apps); err != nil {
		agent.Fail(w, 409, err)
		return
	}
	agent.JSON(w, 200, apps)
}

func (c *Control) registerNative(w http.ResponseWriter, r *http.Request) {
	var in agent.NativeApplication
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, found := c.data.Apps[in.ID]; found || len(c.data.Apps) >= 500 {
		agent.Fail(w, 409, errors.New("application ID already exists or the application limit was reached"))
		return
	}
	var registered agent.NativeApplication
	if err := c.agentCall(r, r.PathValue("server"), "POST", "/v1/native/apps", in, &registered); err != nil {
		agent.Fail(w, 409, err)
		return
	}
	if registered.ID != in.ID || registered.Name != in.Name || len(registered.Services) == 0 {
		agent.Fail(w, 502, errors.New("native registration response did not match the requested application"))
		return
	}
	runtime := registered.Runtime
	if runtime == "" {
		runtime = "rails"
	}
	a := Application{ID: registered.ID, Native: &registered, Runtime: runtime, Repository: registered.Repository, Branch: registered.Branch, Domain: registered.Domain, AdditionalDomains: registered.AdditionalDomains, ServerID: r.PathValue("server"), Generation: random()}
	c.data.Apps[a.ID] = a
	if err := c.save(); err != nil {
		delete(c.data.Apps, a.ID)
		agent.Fail(w, 500, err)
		return
	}
	agent.JSON(w, 201, a)
}

func (c *Control) nativeAppRoute(w http.ResponseWriter, r *http.Request) {
	a, found := c.app(r.PathValue("id"))
	if !found || a.Native == nil {
		http.NotFound(w, r)
		return
	}
	path := "/v1/native/apps/" + url.PathEscape(a.ID)
	var input any
	if r.Method == "POST" {
		var in struct {
			Confirm string `json:"confirm"`
		}
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		path += "/services/" + url.PathEscape(r.PathValue("unit")) + "/" + url.PathEscape(r.PathValue("action"))
		input = in
	} else if r.PathValue("action") == "logs" {
		path += "/logs?unit=" + url.QueryEscape(r.URL.Query().Get("unit"))
	} else if r.PathValue("action") != "" {
		http.NotFound(w, r)
		return
	}
	var out any
	if err := c.agentCall(r, a.ServerID, r.Method, path, input, &out); err != nil {
		agent.Fail(w, 409, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, out)
}
