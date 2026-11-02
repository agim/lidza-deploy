package control

import (
	"errors"
	"net/http"
	"slices"

	"github.com/agim/lidza-deploy/internal/agent"
)

func (c *Control) verifyApplicationRuntime(r *http.Request, serverID string, a agent.App) error {
	if (a.Runtime == "" || a.Runtime == "go") && a.Build.SourceDirectory == "" {
		return nil
	}
	var capability struct {
		Runtimes []string `json:"runtimes"`
		Build    bool     `json:"build_configuration_supported"`
	}
	if c.agentCall(r, serverID, "GET", "/v1/application-runtimes", nil, &capability) != nil {
		return errors.New("the selected agent must be updated before using this runtime or repository directory")
	}
	runtime := a.Runtime
	if runtime == "" {
		runtime = "go"
	}
	if !slices.Contains(capability.Runtimes, runtime) || a.Build.SourceDirectory != "" && !capability.Build {
		return errors.New("the selected agent does not support this runtime or build configuration")
	}
	return nil
}
