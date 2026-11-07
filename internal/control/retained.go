package control

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Retained resources are what a removed application, or the source of a
// finished move, left on a server: databases, caches, ClickHouse and file
// volumes. The panel lists them per server with why they were retained, and
// removes one through the server's agent, which refuses anything still
// attached and requires a verified (or final) backup of every database.

// Retained reasons shown by the panel.
const (
	retainedReasonMoved   = "moved"
	retainedReasonRemoved = "removed"
)

type fleetRetainedResource struct {
	agent.RetainedResourceView
	ServerID string `json:"server_id"`
	// Reason is moved (a finished move retired the application here) or
	// removed (the application was deleted with its data kept).
	Reason string `json:"reason"`
	// RunsOn is the server the application now runs on, when the control
	// plane knows; RunsOnName is its display name.
	RunsOn     string `json:"runs_on,omitempty"`
	RunsOnName string `json:"runs_on_name,omitempty"`
	// MovedAt is when the move that left the data here finished.
	MovedAt    time.Time `json:"moved_at,omitzero"`
	ServerName string    `json:"server_name"`
}

// retainedFor annotates one agent row with why it was retained.
func (c *Control) retainedFor(serverID string, row agent.RetainedResourceView) fleetRetainedResource {
	out := fleetRetainedResource{RetainedResourceView: row, ServerID: serverID, Reason: retainedReasonRemoved}
	c.mu.Lock()
	var latest *MoveJob
	for _, job := range c.data.Moves {
		if row.Origin != nil && row.Origin.MoveID != "" && job.ID == row.Origin.MoveID {
			job := job
			latest = &job
			break
		}
		if job.State == moveStateFinished && job.AppID == row.AppID && job.SourceServer == serverID && (latest == nil || job.Updated.After(latest.Updated)) {
			job := job
			latest = &job
		}
	}
	if latest == nil && row.Origin != nil && row.Origin.MoveID != "" && row.Origin.Switched {
		// The move is no longer in the panel's history; the agent knows.
		out.Reason = retainedReasonMoved
		if a, live := c.data.Apps[row.AppID]; live && a.ServerID != serverID {
			out.RunsOn = a.ServerID
		}
	}
	if latest != nil {
		out.Reason, out.RunsOn, out.MovedAt = retainedReasonMoved, latest.TargetServer, latest.Updated
		if latest.SourceServer != serverID {
			// A destination that never switched holds a copy.
			out.RunsOn = latest.SourceServer
		}
		if a, live := c.data.Apps[row.AppID]; live && a.ServerID != "" {
			out.RunsOn = a.ServerID
		}
	}
	c.mu.Unlock()
	if out.RunsOn != "" {
		out.RunsOnName = c.serverName(out.RunsOn)
	}
	out.ServerName = c.serverName(serverID)
	return out
}

func sortRetained(list []fleetRetainedResource) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].ServerID != list[j].ServerID {
			return list[i].ServerID < list[j].ServerID
		}
		return list[i].ID < list[j].ID
	})
}

// serverRetained lists one server's retained resources.
func (c *Control) serverRetained(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("server")
	var rows []agent.RetainedResourceView
	if err := c.agentCall(r, serverID, http.MethodGet, "/v1/retained-resources", nil, &rows); err != nil {
		failAgent(w, err)
		return
	}
	out := make([]fleetRetainedResource, 0, len(rows))
	for _, row := range rows {
		out = append(out, c.retainedFor(serverID, row))
	}
	sortRetained(out)
	agent.JSON(w, http.StatusOK, out)
}

// removeRetained asks the server's agent to remove one retained resource.
// The confirmation must repeat the resource id; a running move of the same
// application blocks removal.
func (c *Control) removeRetained(w http.ResponseWriter, r *http.Request) {
	serverID, id := r.PathValue("server"), r.PathValue("id")
	var input agent.RetainedRemoveRequest
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, http.StatusBadRequest, err)
		return
	}
	if input.Confirm != id {
		agent.Fail(w, http.StatusBadRequest, errors.New("type the retained resource id to confirm its removal"))
		return
	}
	var rows []agent.RetainedResourceView
	if err := c.agentCall(r, serverID, http.MethodGet, "/v1/retained-resources", nil, &rows); err != nil {
		failAgent(w, err)
		return
	}
	var found *agent.RetainedResourceView
	for i := range rows {
		if rows[i].ID == id {
			found = &rows[i]
		}
	}
	if found == nil {
		agent.Fail(w, http.StatusNotFound, errors.New("no such retained resource on this server"))
		return
	}
	if job, moving := c.activeMove(found.AppID); moving && (job.SourceServer == serverID || job.TargetServer == serverID) {
		agent.Fail(w, http.StatusConflict, errors.New("a move of "+found.AppID+" involves this server; finish or abort it first"))
		return
	}
	var out map[string]string
	if err := c.agentCall(r, serverID, http.MethodDelete, "/v1/retained-resources/"+url.PathEscape(id), input, &out); err != nil {
		var refused *agentHTTPError
		if errors.As(err, &refused) && refused.status == http.StatusNotFound {
			agent.Fail(w, http.StatusNotFound, errors.New("no such retained resource on this server"))
			return
		}
		failAgent(w, err)
		return
	}
	agent.JSON(w, http.StatusAccepted, map[string]string{"status": "removing"})
}
