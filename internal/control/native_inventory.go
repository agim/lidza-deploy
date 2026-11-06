package control

// Inventory synchronization is read-only on connected hosts. It never
// registers services, sends service actions, probes an app port or changes an
// existing application's deployment configuration.
import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
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
		if !namePattern.MatchString(a.Name) {
			return errors.New("Agent discovery lists an application without a valid name; previous inventory retained.")
		}
		if len(a.Services) > 32 || len(a.Services) == 0 && !nativeStaticInventory(a) {
			return fmt.Errorf("%s: agent discovery lists %d services; previous inventory retained.", a.Name, len(a.Services))
		}
		if _, exists := found[a.Name]; exists {
			return fmt.Errorf("%s: agent discovery lists the application twice; previous inventory retained.", a.Name)
		}
		for _, unit := range a.Services {
			if !unitPattern.MatchString(unit) {
				return fmt.Errorf("%s: agent discovery lists an invalid service name; previous inventory retained.", a.Name)
			}
		}
		a.Services = slices.Clone(a.Services)
		slices.Sort(a.Services)
		if len(slices.Compact(slices.Clone(a.Services))) != len(a.Services) {
			return fmt.Errorf("%s: agent discovery lists a service twice; previous inventory retained.", a.Name)
		}
		found[a.Name] = a
	}
	// Each registration is compared with the discovery entry of the same
	// name. The agent derives that entry from the policy that runs the
	// application when it has one (system units), otherwise from the user
	// units bound to its directory, so the two sides describe the same
	// service manager; service order is the agent's, not a difference.
	registered := map[string]agent.NativeApplication{}
	ids := map[string]bool{}
	for _, a := range remote {
		if a.Native == nil {
			continue
		}
		n := *a.Native
		if nativeIdentityError(n) != nil {
			return errors.New("Agent returned an invalid native binding; previous inventory retained.")
		}
		if n.ID != a.ID || n.Domain != a.Domain || n.Repository != a.Repository || n.Branch != a.Branch {
			return fmt.Errorf("%s: agent application and native binding disagree; previous inventory retained.", n.ID)
		}
		if _, duplicate := registered[n.Name]; duplicate || ids[n.ID] {
			return fmt.Errorf("%s: agent returned the native binding twice; previous inventory retained.", n.ID)
		}
		d, exists := found[n.Name]
		if !exists {
			return fmt.Errorf("%s: registered on the agent but absent from its discovery; previous inventory retained.", n.ID)
		}
		if !sameServiceSet(n.Services, d.Services) {
			return fmt.Errorf("%s: registered services %s vs discovered %s; previous inventory retained.", n.ID, serviceSet(sortedServices(n.Services)), serviceSet(d.Services))
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
				return fmt.Errorf("%s: recorded in the panel but not registered on the agent; previous inventory retained.", existing.ID)
			}
		}
	}
	changed := false
	for name, n := range registered {
		if current, exists := apps[n.ID]; exists {
			if current.ServerID != server.ID || current.Native == nil {
				return fmt.Errorf("%s: owned by another server or a container application; existing ownership retained.", n.ID)
			}
			if difference := nativeBindingDifference(*current.Native, n); difference != "" {
				return fmt.Errorf("%s: panel record and agent binding differ (%s); existing ownership retained.", n.ID, difference)
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

// nativeStaticInventory reports whether an application legitimately runs no
// service: a static Astro site served from its public directory, or a static
// site the agent created and serves from its build output.
func nativeStaticInventory(a agent.NativeApplication) bool {
	return a.Runtime == "astro" && a.DocumentRoot == "public" || a.Runtime == "static" && a.DocumentRoot != ""
}

// nativeIdentityError checks the identity an agent reports for a native
// application: id, repository, branch and hostnames, plus the readiness path
// of a Rails application. The runtime itself is the agent's decision; created
// applications run runtimes the container validator does not know.
func nativeIdentityError(n agent.NativeApplication) error {
	probe := agent.App{ID: n.ID, Repository: n.Repository, Branch: n.Branch, Domain: n.Domain, AdditionalDomains: n.AdditionalDomains}
	if n.Runtime == "" || n.Runtime == "rails" {
		probe.Runtime, probe.Rails = "rails", &agent.RailsConfig{HealthPath: n.HealthPath}
	}
	return probe.Validate()
}

// sortedServices is a copy of units in a stable order.
func sortedServices(units []string) []string { return slices.Sorted(slices.Values(units)) }

// sameServiceSet compares services regardless of order: the agent lists a
// created application's units in process order and a discovered one's
// alphabetically.
func sameServiceSet(a, b []string) bool {
	return slices.Equal(sortedServices(a), sortedServices(b))
}

// serviceSet renders services for a status message.
func serviceSet(units []string) string {
	return "[" + strings.Join(units, ",") + "]"
}

// nativeBindingDifference names the first field in which the panel's record
// of a native binding and the agent's differ, for the sync status; it is
// empty exactly when sameNativeApplication accepts the pair.
func nativeBindingDifference(record, remote agent.NativeApplication) string {
	if sameNativeApplication(record, remote) {
		return ""
	}
	switch {
	case record.ID != remote.ID:
		return fmt.Sprintf("id %q vs %q", record.ID, remote.ID)
	case record.Name != remote.Name:
		return fmt.Sprintf("name %q vs %q", record.Name, remote.Name)
	case record.Domain != remote.Domain:
		return fmt.Sprintf("domain %q vs %q", record.Domain, remote.Domain)
	case !slices.Equal(record.AdditionalDomains, remote.AdditionalDomains):
		return "additional domains differ"
	case record.Repository != remote.Repository:
		return fmt.Sprintf("repository %q vs %q", record.Repository, remote.Repository)
	case record.Branch != remote.Branch:
		return fmt.Sprintf("branch %q vs %q", record.Branch, remote.Branch)
	case record.Port != remote.Port:
		return fmt.Sprintf("port %q vs %q", record.Port, remote.Port)
	case record.HealthPath != remote.HealthPath:
		return fmt.Sprintf("health path %q vs %q", record.HealthPath, remote.HealthPath)
	case record.Runtime != remote.Runtime:
		return fmt.Sprintf("runtime %q vs %q", record.Runtime, remote.Runtime)
	case record.DocumentRoot != remote.DocumentRoot:
		return fmt.Sprintf("document root %q vs %q", record.DocumentRoot, remote.DocumentRoot)
	case !slices.Equal(record.Services, remote.Services):
		return "services " + serviceSet(record.Services) + " vs " + serviceSet(remote.Services)
	}
	return "binding fields differ"
}
