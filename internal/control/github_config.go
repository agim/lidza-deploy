package control

import (
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/pkg/credentials"
	"github.com/agim/lidza/pkg/env"
	"net/http"
)

func githubConfigured() bool {
	values, err := env.Values(".")
	if err != nil {
		return false
	}
	connectors, warnings := auth.ConnectorsFromEnv(values)
	return len(connectors) > 0 && len(warnings) == 0
}
func (c *Control) configureGitHub(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if input.ID == "" || input.Secret == "" || len(input.ID) > 256 || len(input.Secret) > 4096 {
		agent.Fail(w, 400, errors.New("supply the GitHub OAuth client ID and secret"))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	origins, err := env.Origins(".")
	if err != nil {
		agent.Fail(w, 500, errors.New("could not read configuration"))
		return
	}
	for _, key := range []string{"AUTH_CONNECT", "AUTH_CONNECT_GITHUB_CLIENT_ID", "AUTH_CONNECT_GITHUB_CLIENT_SECRET"} {
		if origins[key] == env.OriginProcess {
			agent.Fail(w, 409, errors.New("GitHub settings are overridden by the service environment; remove those overrides to manage them here"))
			return
		}
	}
	if err = credentials.Set(".", map[string]string{"AUTH_CONNECT": "github", "AUTH_CONNECT_GITHUB_CLIENT_ID": input.ID, "AUTH_CONNECT_GITHUB_CLIENT_SECRET": input.Secret}); err != nil {
		agent.Fail(w, 500, errors.New("could not save encrypted GitHub configuration"))
		return
	}
	if err = auth.From(r.Context()).Reconfigure(r.Context()); err != nil {
		agent.Fail(w, 500, errors.New("could not reload GitHub configuration"))
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "configured"})
}
