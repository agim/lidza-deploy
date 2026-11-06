package control

import (
	"cmp"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

type ApplicationProcess struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Startup string `json:"startup,omitempty"`
	LogPath string `json:"log_path,omitempty"`
	// Usage and control of native processes (systemd units); container
	// processes report theirs on the server's process page.
	Runtime          string    `json:"runtime,omitempty"`
	Since            time.Time `json:"since,omitzero"`
	MemoryBytes      *int64    `json:"memory_bytes,omitempty"`
	MemoryLimitBytes *int64    `json:"memory_limit_bytes,omitempty"`
	CPUPercent       *float64  `json:"cpu_percent,omitempty"`
	Tasks            *int64    `json:"tasks,omitempty"`
	Restarts         *int      `json:"restarts,omitempty"`
	Preserved        bool      `json:"preserved,omitempty"`
	// RestartPath is the panel route that restarts this process, present
	// only when the process may be restarted; Confirm is the confirmation
	// value the route expects.
	RestartPath string `json:"restart_path,omitempty"`
	Confirm     string `json:"confirm,omitempty"`
}
type ApplicationProcessInventory struct {
	Processes []ApplicationProcess `json:"processes"`
	Note      string               `json:"note,omitempty"`
}

// Distribution adapters supply the same process contract for other runtimes.
// Initialization-only hook; it never changes the permissions of this read route.
var alternateApplicationProcesses func(*Control, http.ResponseWriter, *http.Request, Application) bool

func (c *Control) applicationProcesses(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	if alternateApplicationProcesses != nil && alternateApplicationProcesses(c, w, r, a) {
		return
	}
	var inventory agent.ContainerInventory
	if err := c.agentCall(r, a.ServerID, "GET", "/v1/containers", nil, &inventory); err != nil {
		failAgent(w, err)
		return
	}
	var tasks []agent.Task
	// Workers are optional. A failed task lookup cannot invent their log paths.
	_ = c.agentCall(r, a.ServerID, "GET", "/v1/tasks", nil, &tasks)
	out := ApplicationProcessInventory{Processes: []ApplicationProcess{}, Note: "Process metadata only. Refreshing this view does not start, stop or restart a process."}
	for _, row := range inventory.Containers {
		if row.AppID != a.ID || !(strings.HasPrefix(row.Role, "Web") || strings.HasPrefix(row.Role, "Worker") || strings.HasPrefix(row.Role, "Stack")) {
			continue
		}
		process := ApplicationProcess{ID: row.Name, Name: row.Name, Kind: row.Role, State: row.State}
		if strings.HasPrefix(row.Role, "Web") && !strings.Contains(row.Role, "rollback") {
			process.LogPath = "control/apps/" + url.PathEscape(a.ID) + "/logs"
		}
		for _, task := range tasks {
			if task.AppID == a.ID && task.Container == row.Name {
				process.Startup = "disabled"
				if task.Enabled && !task.Paused {
					process.Startup = "enabled"
				}
				process.LogPath = "control/apps/" + url.PathEscape(a.ID) + "/tasks/" + url.PathEscape(task.ID) + "/logs"
			}
		}
		out.Processes = append(out.Processes, process)
	}
	slices.SortFunc(out.Processes, func(a, b ApplicationProcess) int { return cmp.Compare(a.ID, b.ID) })
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, out)
}
