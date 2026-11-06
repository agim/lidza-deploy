package control

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/agim/lidza-deploy/internal/agent"
)

func init() {
	alternateApplicationProcesses = func(c *Control, w http.ResponseWriter, r *http.Request, a Application) bool {
		if a.Native == nil {
			return false
		}
		var status agent.NativeStatus
		if err := c.agentCall(r, a.ServerID, "GET", "/v1/native/apps/"+url.PathEscape(a.ID), nil, &status); err != nil {
			failAgent(w, err)
			return true
		}
		if !sameNativeApplication(status.Application, *a.Native) {
			agent.Fail(w, 409, errors.New("native service bindings changed; existing ownership retained"))
			return true
		}
		out := ApplicationProcessInventory{Processes: []ApplicationProcess{}, Note: "Each process is a systemd service. Usage comes from its cgroup accounting; existing startup policies and stopped states are preserved."}
		if len(status.Services) == 0 {
			out.Note = "Static files are served by the web server. This application has no service to start or restart."
		}
		// Usage is optional: an agent that cannot sample it still reports
		// the services' states.
		usage := map[string]agent.NativeServiceUsage{}
		var inventory agent.NativeProcessInventory
		if len(status.Services) > 0 && c.agentCall(r, a.ServerID, "GET", "/v1/native/apps/"+url.PathEscape(a.ID)+"/processes", nil, &inventory) == nil {
			for _, service := range inventory.Services {
				usage[service.Unit] = service
			}
		}
		base := "control/native-apps/" + url.PathEscape(a.ID)
		for _, service := range status.Services {
			process := ApplicationProcess{ID: service.Unit, Name: service.Unit, Kind: "Service", State: service.Active, Startup: service.UnitFileState, Runtime: "native", LogPath: base + "/logs?unit=" + url.QueryEscape(service.Unit)}
			if u, ok := usage[service.Unit]; ok {
				restarts := u.Restarts
				process.Since, process.MemoryBytes, process.MemoryLimitBytes, process.CPUPercent, process.Tasks, process.Restarts, process.Preserved = u.Since, u.MemoryBytes, u.MemoryLimitBytes, u.CPUPercent, u.Tasks, &restarts, u.Preserved
				if u.Preserved {
					process.Kind = "Service · preserved"
				}
				if u.Restartable {
					process.RestartPath = base + "/services/" + url.PathEscape(service.Unit) + "/restart"
					process.Confirm = status.Application.Name
				}
			}
			out.Processes = append(out.Processes, process)
		}
		w.Header().Set("Cache-Control", "no-store")
		agent.JSON(w, 200, out)
		return true
	}
}
