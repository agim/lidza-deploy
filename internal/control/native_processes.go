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
			agent.Fail(w, 502, err)
			return true
		}
		if !sameNativeApplication(status.Application, *a.Native) {
			agent.Fail(w, 409, errors.New("native service bindings changed; existing ownership retained"))
			return true
		}
		out := ApplicationProcessInventory{Processes: []ApplicationProcess{}, Note: "Native process metadata only. Existing startup policies and stopped states are preserved."}
		if len(status.Services) == 0 {
			out.Note = "Static files are served by the existing web server. This application has no service to start or restart."
		}
		for _, service := range status.Services {
			out.Processes = append(out.Processes, ApplicationProcess{ID: service.Unit, Name: service.Unit, Kind: service.State, State: service.Active, Startup: service.UnitFileState, LogPath: "control/native-apps/" + url.PathEscape(a.ID) + "/logs?unit=" + url.QueryEscape(service.Unit)})
		}
		w.Header().Set("Cache-Control", "no-store")
		agent.JSON(w, 200, out)
		return true
	}
}
