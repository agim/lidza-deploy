package control

import (
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/pkg/router"
	"net/http"
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
	agent.JSON(w, http.StatusOK, map[string]bool{"two_factor_enabled": enabled, "unlocked": unlocked})
}

func (c *Control) runConsole(w http.ResponseWriter, r *http.Request) {
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
	if !enabled {
		router.WriteError(w, r, auth.ErrFactorSetupRequired)
		return
	}
	if err := auth.AuthorizeStepUp(r.Context(), r, serverConsolePurpose, server.ID); err != nil {
		router.WriteError(w, r, err)
		return
	}
	var input agent.ConsoleRequest
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, http.StatusBadRequest, err)
		return
	}
	var out agent.ConsoleResult
	if err := c.agentCall(r, server.ID, "POST", "/v1/console", input, &out); err != nil {
		agent.Fail(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, http.StatusOK, out)
}
