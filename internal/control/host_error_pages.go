package control

import (
	"encoding/json"
	"net/http"
)

// adoptErrorPages asks a server's agent to serve the managed error and
// maintenance pages from every Caddy error route that serves pages from an
// unmanaged directory. It answers with the server's ingress status.
func (c *Control) adoptErrorPages(w http.ResponseWriter, r *http.Request) {
	var out json.RawMessage
	if err := c.agentCall(r, r.PathValue("server"), http.MethodPost, "/v1/host/ingress/error-pages", struct{}{}, &out); err != nil {
		failAgent(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
