package control

import (
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/pkg/router"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const serverConsolePurpose = "server.console"

func (c *Control) consoleServer(id string) (Server, bool) {
	for _, server := range c.servers() {
		if server.ID == id {
			return server, true
		}
	}
	return Server{}, false
}

func (c *Control) consoleStatus(w http.ResponseWriter, r *http.Request) {
	server, ok := c.consoleServer(r.PathValue("server"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	u := auth.CurrentUser(r.Context())
	enabled, err := auth.From(r.Context()).TwoFactorEnabled(r.Context(), u.ID)
	if err != nil {
		agent.Fail(w, http.StatusServiceUnavailable, errors.New("two-factor status unavailable"))
		return
	}
	unlocked := enabled && auth.From(r.Context()).CheckStepUp(r.Context(), serverConsolePurpose, server.ID) == nil
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, http.StatusOK, map[string]any{"two_factor_enabled": enabled, "unlocked": unlocked, "max_runtime_seconds": int(agent.ConsoleTimeout.Seconds()), "max_output_bytes": agent.ConsoleMaxOutput})
}

func (c *Control) consoleAccess(w http.ResponseWriter, r *http.Request) (Server, bool) {
	server, ok := c.consoleServer(r.PathValue("server"))
	if !ok {
		http.NotFound(w, r)
		return Server{}, false
	}
	u := auth.CurrentUser(r.Context())
	enabled, err := auth.From(r.Context()).TwoFactorEnabled(r.Context(), u.ID)
	if err != nil {
		agent.Fail(w, http.StatusServiceUnavailable, errors.New("two-factor status unavailable"))
		return Server{}, false
	}
	if !enabled {
		router.WriteError(w, r, auth.ErrFactorSetupRequired)
		return Server{}, false
	}
	if err := auth.AuthorizeStepUp(r.Context(), r, serverConsolePurpose, server.ID); err != nil {
		router.WriteError(w, r, err)
		return Server{}, false
	}
	return server, true
}

func (c *Control) runConsole(w http.ResponseWriter, r *http.Request) {
	server, ok := c.consoleAccess(w, r)
	if !ok {
		return
	}
	var input agent.ConsoleRequest
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, http.StatusBadRequest, err)
		return
	}
	var out agent.ConsoleResult
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(agent.ConsoleResponseTimeout + 15*time.Second))
	if err := c.agentCall(r, server.ID, "POST", "/v1/console", input, &out); err != nil {
		agent.Fail(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, http.StatusOK, out)
}

// Commands execute on the agent independently of the short HTTP acknowledgement.
func (c *Control) startConsoleJob(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	server, ok := c.consoleAccess(w, r)
	if !ok {
		return
	}
	var input struct {
		ID      string `json:"id"`
		Command string `json:"command"`
	}
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	request := agent.ConsoleJobRequest{ID: input.ID, Command: input.Command, Owner: auth.CurrentUser(r.Context()).ID}
	var out agent.ConsoleJobView
	if err := c.agentCall(r, server.ID, "POST", "/v1/console/jobs", request, &out); err != nil {
		consoleJobError(w, err)
		return
	}
	agent.JSON(w, 202, out)
}

func (c *Control) consoleJobOutput(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	server, ok := c.consoleAccess(w, r)
	if !ok {
		return
	}
	offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
	if err != nil || offset < 0 {
		agent.Fail(w, 400, errors.New("invalid output offset"))
		return
	}
	var out agent.ConsoleJobView
	path := "/v1/console/jobs/" + url.PathEscape(r.PathValue("job")) + "?owner=" + url.QueryEscape(auth.CurrentUser(r.Context()).ID) + "&offset=" + strconv.Itoa(offset)
	if err := c.agentCall(r, server.ID, "GET", path, nil, &out); err != nil {
		consoleJobError(w, err)
		return
	}
	agent.JSON(w, 200, out)
}

func consoleJobError(w http.ResponseWriter, err error) {
	var remote *agentHTTPError
	if errors.As(err, &remote) && (remote.status == 404 || remote.status == 409) {
		agent.Fail(w, remote.status, err)
		return
	}
	agent.Fail(w, 502, err)
}
