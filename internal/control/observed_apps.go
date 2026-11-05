package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Observations never become agent-owned applications. An explicit migration
// provisions a new managed identity later; importing inventory has no host calls.
type ObservedApplication struct {
	Automatic  bool      `json:"automatic,omitempty"`
	Missing    bool      `json:"missing,omitempty"`
	Name       string    `json:"name"`
	Source     string    `json:"source"`
	Runtime    string    `json:"runtime"`
	Domain     string    `json:"domain,omitempty"`
	Processes  []string  `json:"processes"`
	ObservedAt time.Time `json:"observed_at"`
}

func observedApplications(source string, raw json.RawMessage) (map[string]ObservedApplication, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:-]{0,100}$`).MatchString(source) {
		return nil, errors.New("enter a source server name or IP address")
	}
	var inventory struct {
		Schema   int  `json:"schema"`
		ReadOnly bool `json:"read_only"`
		Apps     []struct {
			Name      string   `json:"name"`
			Runtime   string   `json:"runtime"`
			Domain    string   `json:"domain"`
			Processes []string `json:"processes"`
			Services  []struct {
				Service string `json:"service"`
			} `json:"services"`
		} `json:"apps"`
	}
	// Only this allowlist is persisted. Extra scanner fields, command strings and
	// environment values in an uploaded file are discarded, never stored or logged.
	if json.Unmarshal(raw, &inventory) != nil || inventory.Schema != 1 || !inventory.ReadOnly || len(inventory.Apps) > 100 {
		return nil, errors.New("select a version 1 read-only host inventory")
	}
	result := map[string]ObservedApplication{}
	for _, a := range inventory.Apps {
		if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,100}$`).MatchString(a.Name) {
			return nil, errors.New("invalid observed application name")
		}
		runtime := a.Runtime
		if runtime != "go" && runtime != "rails" && runtime != "astro" {
			runtime = "unknown"
		}
		if a.Domain != "" {
			spec := agent.App{ID: "observed", Repository: "observed/app", Branch: "main", Domain: a.Domain}
			if err := spec.Validate(); err != nil {
				return nil, errors.New("invalid observed domain")
			}
		}
		processes := []string{}
		for _, name := range a.Processes {
			if regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`).MatchString(name) {
				processes = append(processes, name)
			}
		}
		for _, service := range a.Services {
			if regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,150}\.service$`).MatchString(service.Service) {
				processes = append(processes, service.Service)
			}
		}
		slices.Sort(processes)
		processes = slices.Compact(processes)
		id := "observed:" + source + ":" + a.Name
		result[id] = ObservedApplication{Name: a.Name, Source: source, Runtime: runtime, Domain: a.Domain, Processes: processes, ObservedAt: time.Now().UTC()}
	}
	return result, nil
}

func (c *Control) importObservedApplications(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Source    string          `json:"source"`
		Inventory json.RawMessage `json:"inventory"`
	}
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	observed, err := observedApplications(in.Source, in.Inventory)
	if err != nil {
		agent.Fail(w, 400, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := map[string]ObservedApplication{}
	for id, a := range c.data.ObservedApps {
		if a.Source != in.Source {
			next[id] = a
		}
	}
	for id, a := range observed {
		next[id] = a
	}
	if len(next) > 500 {
		agent.Fail(w, 409, errors.New("observed application limit reached"))
		return
	}
	old := c.data.ObservedApps
	c.data.ObservedApps = next
	if err := c.save(); err != nil {
		c.data.ObservedApps = old
		agent.Fail(w, 500, err)
		return
	}
	agent.JSON(w, 200, map[string]int{"observed": len(observed)})
}
