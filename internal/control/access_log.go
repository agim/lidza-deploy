package control

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/audit"
)

// The access log of an application (agent/access_log.go): the file of its
// own Caddy access entries that only its account reads. The hosting agent
// stores the setting and applies it at once; the panel forwards both ways.

func (c *Control) accessLog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var status agent.AppAccessLogStatus
	if err := c.agentCall(r, a.ServerID, http.MethodGet, "/v1/apps/"+url.PathEscape(a.ID)+"/access-log", nil, &status); err != nil {
		var refused *agentHTTPError
		if errors.As(err, &refused) && refused.status == http.StatusNotFound {
			// Agents before access logs have no such route.
			agent.Fail(w, http.StatusNotFound, errors.New("update the hosting agent to manage access logs"))
			return
		}
		failAgent(w, err)
		return
	}
	agent.JSON(w, 200, status)
}

func (c *Control) updateAccessLog(w http.ResponseWriter, r *http.Request) {
	var in agent.AppAccessLogSettings
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if in.Enabled == nil {
		agent.Fail(w, 400, errors.New("enabled is required"))
		return
	}
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var status agent.AppAccessLogStatus
	if err := c.agentCall(r, a.ServerID, http.MethodPut, "/v1/apps/"+url.PathEscape(a.ID)+"/access-log", in, &status); err != nil {
		failAgent(w, err)
		return
	}
	recordAccessLogAudit(r.Context(), a.ID, *in.Enabled)
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, status)
}

func recordAccessLogAudit(ctx context.Context, appID string, enabled bool) {
	log, ok := lidza.Optional[*audit.Audit](ctx)
	if !ok || log == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = log.Record(writeCtx, audit.Event{Action: "app.access_log.update", Scope: fleetScope, Resource: "app/" + appID, Meta: map[string]string{"enabled": strconv.FormatBool(enabled)}})
}
