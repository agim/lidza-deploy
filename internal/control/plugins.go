package control

// Plugins of a server (agent: plugins.go, docs/plugins-proposal.md). The
// control panel only relays: the server's agent verifies the signature,
// installs the plugin and runs its hooks.

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/plugin"
)

var pluginRouteName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

func (c *Control) pluginRoutes(handle func(string, http.HandlerFunc)) {
	for _, pattern := range []string{
		"GET /api/control/servers/{server}/plugins",
		"PUT /api/control/servers/{server}/plugins/keys",
		"POST /api/control/servers/{server}/plugins/manifest",
		"PUT /api/control/servers/{server}/plugins/{name}/archive",
		"POST /api/control/servers/{server}/plugins/{name}/hooks/{hook}",
		"DELETE /api/control/servers/{server}/plugins/{name}",
	} {
		handle(pattern, c.serverPlugins)
	}
}

func (c *Control) serverPlugins(w http.ResponseWriter, r *http.Request) {
	target := c.serverByID(r.PathValue("server"))
	if target.ID == "" {
		http.NotFound(w, r)
		return
	}
	name, hook := r.PathValue("name"), r.PathValue("hook")
	if name != "" && !pluginRouteName.MatchString(name) {
		agent.Fail(w, 400, errors.New("invalid plugin name"))
		return
	}
	var out json.RawMessage
	reply := func(status int, err error) {
		if err != nil {
			// An agent without plugins answers 404 with no message of its own.
			var refused *agentHTTPError
			if errors.As(err, &refused) && refused.status == http.StatusNotFound && refused.message == "" {
				agent.Fail(w, http.StatusNotFound, errors.New("this server's agent is older than the control panel and has no plugins yet; update the agent on this server first"))
				return
			}
			failAgent(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(out)
	}
	switch {
	case r.Method == http.MethodGet:
		reply(200, c.agentRequest(r, target, http.MethodGet, "/v1/plugins", nil, &out))
	case r.Method == http.MethodPut && name == "":
		var in struct {
			Keys []string `json:"keys"`
		}
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		reply(200, c.agentRequest(r, target, http.MethodPut, "/v1/plugins/keys", in, &out))
	case r.Method == http.MethodPost && hook == "":
		var in struct {
			Manifest  string          `json:"manifest"`
			Signature json.RawMessage `json:"signature"`
		}
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		reply(200, c.agentRequest(r, target, http.MethodPost, "/v1/plugins/manifest", in, &out))
	case r.Method == http.MethodPut:
		if r.Header.Get("Content-Type") != "application/octet-stream" || r.ContentLength <= 0 || r.ContentLength > plugin.MaxArchiveSize {
			agent.Fail(w, 400, errors.New("upload the plugin archive as application/octet-stream with its length"))
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(20 * time.Minute))
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(21 * time.Minute))
		reply(201, c.panelStream(r.Context(), target, http.MethodPut, "/v1/plugins/"+name+"/archive", "application/octet-stream", http.MaxBytesReader(w, r.Body, r.ContentLength), r.ContentLength, 20*time.Minute, &out))
	case r.Method == http.MethodPost:
		if hook != "describe" && hook != "status" && hook != "check" {
			agent.Fail(w, 400, errors.New("only the describe, status and check hooks can be run on request"))
			return
		}
		var in json.RawMessage
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		reply(200, c.agentRequest(r, target, http.MethodPost, "/v1/plugins/"+name+"/hooks/"+hook, in, &out))
	case r.Method == http.MethodDelete:
		reply(200, c.agentRequest(r, target, http.MethodDelete, "/v1/plugins/"+name, nil, &out))
	}
}
