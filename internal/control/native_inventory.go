package control

// Inventory synchronization is read-only on connected hosts. It never
// registers services, sends service actions, probes an app port or changes an
// existing application's deployment configuration.
import (
	"context"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

type NativeInventoryStatus struct {
	ServerID string    `json:"server_id"`
	State    string    `json:"state"`
	Message  string    `json:"message,omitempty"`
	Checked  time.Time `json:"checked"`
	Apps     int       `json:"apps"`
}

func (c *Control) nativeInventoryStatus(w http.ResponseWriter, r *http.Request) {
	agent.JSON(w, http.StatusOK, c.nativeInventoryStates())
}

func (c *Control) nativeInventoryStates() []NativeInventoryStatus {
	c.mu.Lock()
	statuses := maps.Clone(c.nativeSync)
	c.mu.Unlock()
	out := make([]NativeInventoryStatus, 0, len(c.servers()))
	for _, server := range c.servers() {
		if status, ok := statuses[server.ID]; ok {
			out = append(out, status)
		} else {
			out = append(out, NativeInventoryStatus{ServerID: server.ID, State: "pending"})
		}
	}
	return out
}

func (c *Control) syncNativeInventory(ctx context.Context) {
	c.nativeSyncMu.Lock()
	defer c.nativeSyncMu.Unlock()
	slots := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, server := range c.servers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			c.syncNativeServer(ctx, server)
		}()
	}
	wg.Wait()
}

func (c *Control) syncNativeServer(ctx context.Context, server Server) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1", nil)
	status := NativeInventoryStatus{ServerID: server.ID, State: "error", Message: "Inventory sync unavailable; existing applications retained."}
	defer func() {
		status.Checked = time.Now().UTC()
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, current := range c.servers() {
			if current == server {
				if c.nativeSync == nil {
					c.nativeSync = map[string]NativeInventoryStatus{}
				}
				c.nativeSync[server.ID] = status
			}
		}
	}()
	var capability struct {
		MetadataOnly          bool `json:"status_metadata_only"`
		HTTPProbes            bool `json:"http_readiness_probes"`
		AutomaticRegistration bool `json:"automatic_registration_metadata_only"`
	}
	if err := c.agentRequest(r, server, "GET", "/v1/native/capabilities", nil, &capability); err != nil {
		var remote *agentHTTPError
		if errors.As(err, &remote) && (remote.status == 404 || remote.status == 409) {
			status.State, status.Message = "unsupported", "This agent does not provide native Rails discovery."
		}
		return
	}
	if !capability.MetadataOnly || capability.HTTPProbes {
		status.Message = "Native inventory requires metadata-only status without application HTTP probes."
		return
	}
	var discovery []agent.NativeApplication
	var remote []agent.App
	if c.agentRequest(r, server, "GET", "/v1/native/discovery", nil, &discovery) != nil || len(discovery) > 100 {
		return
	}
	if c.agentRequest(r, server, "GET", "/v1/apps", nil, &remote) != nil || len(remote) > 500 {
		return
	}
	if capability.AutomaticRegistration {
		for _, candidate := range discovery {
			registered := false
			for _, app := range remote {
				if app.Native != nil && app.Native.Name == candidate.Name {
					registered = true
					break
				}
			}
			if registered || candidate.ID == "" || candidate.Repository == "" || candidate.Branch == "" || candidate.Domain == "" {
				continue
			}
			runtime := candidate.Runtime
			if runtime == "" {
				runtime = "rails"
			}
			probe := agent.App{ID: candidate.ID, Runtime: runtime, Repository: candidate.Repository, Branch: candidate.Branch, Domain: candidate.Domain, AdditionalDomains: candidate.AdditionalDomains}
			if probe.Validate() != nil {
				continue
			}
			if !c.nativeInventoryCandidateAvailable(server, candidate) {
				status.Message = "Discovered application conflicts with existing inventory; existing ownership retained."
				return
			}
			var binding agent.NativeApplication
			if c.agentRequest(r, server, "POST", "/v1/native/apps", candidate, &binding) != nil || !sameNativeApplication(candidate, binding) {
				status.Message = "Automatic metadata registration could not be verified; existing applications retained."
				return
			}
			remote = append(remote, agent.App{ID: binding.ID, Runtime: runtime, Native: &binding, Repository: binding.Repository, Branch: binding.Branch, Domain: binding.Domain, AdditionalDomains: slices.Clone(binding.AdditionalDomains)})
		}
	}
	if err := c.mergeNativeInventory(server, discovery, remote); err != nil {
		status.Message = err.Error()
		return
	}
	status.State, status.Message, status.Apps = "synced", "", len(discovery)
}

func (c *Control) nativeInventoryCandidateAvailable(server Server, candidate agent.NativeApplication) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, current := range c.data.Apps {
		if current.ID == candidate.ID {
			return current.Native != nil && current.ServerID == server.ID && sameNativeApplication(*current.Native, candidate)
		}
		if (agent.App{Domain: current.Domain, AdditionalDomains: current.AdditionalDomains}).DomainsOverlap(agent.App{Domain: candidate.Domain, AdditionalDomains: candidate.AdditionalDomains}) || current.Native != nil && current.ServerID == server.ID && current.Native.Name == candidate.Name {
			return false
		}
	}
	return true
}

func (c *Control) mergeNativeInventory(server Server, discovery []agent.NativeApplication, remote []agent.App) error {
	// Treat incomplete or ambiguous responses as a failed snapshot, never as a
	// reason to drop existing records or silently rebind an application.
	namePattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,100}$`)
	unitPattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{0,149}\.service$`)
	found := map[string]agent.NativeApplication{}
	for _, a := range discovery {
		if !namePattern.MatchString(a.Name) || len(a.Services) == 0 && (a.Runtime != "astro" || a.DocumentRoot != "public") || len(a.Services) > 32 {
			return errors.New("Agent returned incomplete native inventory; previous inventory retained.")
		}
		if _, exists := found[a.Name]; exists {
			return errors.New("Agent returned duplicate native applications; previous inventory retained.")
		}
		for _, unit := range a.Services {
			if !unitPattern.MatchString(unit) {
				return errors.New("Agent returned invalid native services; previous inventory retained.")
			}
		}
		a.Services = slices.Clone(a.Services)
		slices.Sort(a.Services)
		if len(slices.Compact(slices.Clone(a.Services))) != len(a.Services) {
			return errors.New("Agent returned duplicate native services; previous inventory retained.")
		}
		found[a.Name] = a
	}
	registered := map[string]agent.NativeApplication{}
	ids := map[string]bool{}
	for _, a := range remote {
		if a.Native == nil {
			continue
		}
		n := *a.Native
		d, exists := found[n.Name]
		runtime := n.Runtime
		if runtime == "" {
			runtime = "rails"
		}
		probe := agent.App{ID: n.ID, Repository: n.Repository, Branch: n.Branch, Domain: n.Domain, AdditionalDomains: n.AdditionalDomains, Runtime: runtime}
		if runtime == "rails" {
			probe.Rails = &agent.RailsConfig{HealthPath: n.HealthPath}
		}
		if !exists || n.ID != a.ID || n.Domain != a.Domain || n.Repository != a.Repository || n.Branch != a.Branch || probe.Validate() != nil || !slices.Equal(n.Services, d.Services) || ids[n.ID] {
			return errors.New("Agent native bindings do not match discovery; previous inventory retained.")
		}
		if _, duplicate := registered[n.Name]; duplicate {
			return errors.New("Agent returned duplicate native bindings; previous inventory retained.")
		}
		ids[n.ID] = true
		registered[n.Name] = n
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	paired := false
	for _, current := range c.servers() {
		if current == server {
			paired = true
		}
	}
	if !paired {
		return errors.New("Server connection changed during inventory sync; retrying next cycle.")
	}
	// The non-live side of a move shares the application id; it is the
	// move's to manage, not inventory's.
	for name, n := range registered {
		if c.moveShadowLocked(server.ID, n.ID) {
			delete(registered, name)
			delete(found, name)
		}
	}
	apps, observed := maps.Clone(c.data.Apps), maps.Clone(c.data.ObservedApps)
	if apps == nil {
		apps = map[string]Application{}
	}
	if observed == nil {
		observed = map[string]ObservedApplication{}
	}
	for _, existing := range apps {
		if existing.Native != nil && existing.ServerID == server.ID {
			if _, exists := registered[existing.Native.Name]; !exists {
				return errors.New("An existing Rails app is missing from agent inventory; previous inventory retained.")
			}
		}
	}
	changed := false
	for name, n := range registered {
		if current, exists := apps[n.ID]; exists {
			if current.ServerID != server.ID || current.Native == nil || !sameNativeApplication(*current.Native, n) {
				return errors.New("Native inventory conflicts with an existing application; existing ownership retained.")
			}
		} else {
			for _, current := range apps {
				if current.Domain == n.Domain || slices.Contains(current.AdditionalDomains, n.Domain) || current.ServerID == server.ID && current.Native != nil && current.Native.Name == name {
					return errors.New("Native inventory conflicts with an existing name or domain; existing ownership retained.")
				}
			}
			runtime := n.Runtime
			if runtime == "" {
				runtime = "rails"
			}
			apps[n.ID] = Application{ID: n.ID, ServerID: server.ID, Native: &n, Runtime: runtime, Repository: n.Repository, Branch: n.Branch, Domain: n.Domain, AdditionalDomains: slices.Clone(n.AdditionalDomains), Generation: random()}
			changed = true
		}
	}
	for id, observation := range observed {
		if !observation.Automatic || observation.Source != server.ID {
			continue
		}
		if _, promoted := registered[observation.Name]; promoted {
			delete(observed, id)
			changed = true
			continue
		}
		_, exists := found[observation.Name]
		if observation.Missing != !exists {
			observation.Missing = !exists
			observed[id] = observation
			changed = true
		}
	}
	for name, a := range found {
		if _, exists := registered[name]; exists {
			continue
		}
		// Unknown hostnames/repositories are left empty, never invented. These
		// services still appear automatically and keep their current manager.
		id := "discovered:" + server.ID + ":" + name
		previous, exists := observed[id]
		if exists && previous.Automatic && !previous.Missing && slices.Equal(previous.Processes, a.Services) {
			continue
		}
		runtime := a.Runtime
		if runtime == "" {
			runtime = "rails"
		}
		observed[id] = ObservedApplication{Automatic: true, Source: server.ID, Name: name, Runtime: runtime, Processes: slices.Clone(a.Services), ObservedAt: time.Now().UTC()}
		changed = true
	}
	if len(apps)+len(observed) > 500 {
		return errors.New("Application inventory limit reached; existing inventory retained.")
	}
	if !changed {
		return nil
	}
	oldApps, oldObserved := c.data.Apps, c.data.ObservedApps
	c.data.Apps, c.data.ObservedApps = apps, observed
	if err := c.save(); err != nil {
		c.data.Apps, c.data.ObservedApps = oldApps, oldObserved
		return errors.New("Could not save synchronized inventory; existing inventory retained.")
	}
	return nil
}
