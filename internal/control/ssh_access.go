package control

import (
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
	"net/url"
)

func (c *Control) sshAccess(w http.ResponseWriter, r *http.Request) {
	path := "/v1/ssh-access"
	var body any
	switch r.Method {
	case http.MethodPost:
		var in struct {
			Label     string `json:"label"`
			PublicKey string `json:"public_key"`
		}
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		body = in
		path += "/keys"
	case http.MethodDelete:
		path += "/keys/" + url.PathEscape(r.PathValue("key"))
	case http.MethodPatch:
		var in struct {
			Sudo bool `json:"sudo"`
		}
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		body = in
	}
	if r.Method == http.MethodGet {
		var out agent.SSHAccess
		if err := c.agentCall(r, r.PathValue("server"), r.Method, path, body, &out); err != nil {
			agent.Fail(w, 502, err)
			return
		}
		agent.JSON(w, 200, out)
		return
	}
	if err := c.agentCall(r, r.PathValue("server"), r.Method, path, body, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 202, map[string]string{"status": "queued"})
}
