package agent

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type StackProfileResources struct {
	Services map[string]StackBudget `json:"services"`
	Database StackBudget            `json:"database"`
}
type StackRuntimeSelection struct {
	Selectors map[string][]string `json:"selectors"`
	Initial   map[string]string   `json:"initial"`
	Path      string              `json:"path"`
	TokenKey  string              `json:"token_key"`
}
type StackSelectionSnapshot struct {
	Version  int               `json:"version"`
	Revision string            `json:"revision"`
	Values   map[string]string `json:"values"`
}

var stackCPUPattern = regexp.MustCompile(`^[0-9]{1,2}(\.[0-9]{1,3})?$`)

// Old databases defaulted to a 1GiB memory cap and no CPU cap. Preserve
// that fact: a missing CPU ceiling cannot be invented for a bounded v2 plan.
func databaseResourceBudget(d Database) StackBudget {
	memory := d.MemoryMiB
	if memory == 0 {
		memory = 1024
	}
	return StackBudget{MemoryMiB: memory, CPUs: d.CPUs}
}
func budgetCPU(b StackBudget) (int, error) {
	if !stackCPUPattern.MatchString(b.CPUs) {
		return 0, errors.New("CPU budget must be a positive decimal with up to three fractional digits")
	}
	n, e := strconv.ParseFloat(b.CPUs, 64)
	if e != nil || n < 0.01 || n > 64 {
		return 0, errors.New("CPU budget outside supported bounds")
	}
	return int(n*1000 + 0.5), nil
}
func validStackBudget(b StackBudget) bool {
	_, err := budgetCPU(b)
	return err == nil && b.MemoryMiB >= 16 && b.MemoryMiB <= 65536
}
func (m StackManifest) validateV2() error {
	if m.Version == 1 {
		if m.RuntimeSelection != nil || len(m.ProfileResources) > 0 {
			return errors.New("runtime_selection/profile_resources require stack version 2")
		}
		for _, s := range m.Services {
			if s.EnabledWhen != nil || len(s.DatabaseBindings) > 0 {
				return errors.New("conditional services/database_bindings require stack version 2")
			}
		}
		return nil
	}
	if len(m.ProfileResources) == 0 || len(m.ProfileResources) > 3 {
		return errors.New("version 2 needs explicit profile_resources")
	}
	for id, p := range m.ProfileResources {
		if _, ok := profileByID(id); !ok {
			return errors.New("unknown stack profile resource ID")
		}
		if len(p.Services) != len(m.Services) || !validStackBudget(p.Database) || p.Database.MemoryMiB < 64 {
			return errors.New("profile needs budgets for every service and a database allowance of at least 64 MiB")
		}
		for _, s := range m.Services {
			if !validStackBudget(p.Services[s.Name]) {
				return fmt.Errorf("profile %s needs a valid budget for service %s", id, s.Name)
			}
		}
	}
	sel := m.RuntimeSelection
	if sel != nil {
		if len(sel.Selectors) == 0 || len(sel.Selectors) > 16 || len(sel.Initial) != len(sel.Selectors) || !validStackProbePath(sel.Path) || path.Clean(sel.Path) != sel.Path || !strings.HasPrefix(sel.Path, "/.well-known/") || sel.Path == domainProbePath || !slices.Contains(m.GeneratedSecrets, sel.TokenKey) || platformConfiguration(sel.TokenKey) {
			return errors.New("runtime_selection needs bounded selectors/initial values, a private well-known path and a purpose-specific generated token")
		}
		for name, values := range sel.Selectors {
			if !envPattern.MatchString(name) || platformConfiguration(name) || environmentKind(name, "") == EnvironmentCredential || len(values) == 0 || len(values) > 16 {
				return errors.New("invalid public runtime selector")
			}
			seen := map[string]bool{}
			for _, v := range values {
				if !configurationText(v, 80) || strings.TrimSpace(v) == "" || seen[v] {
					return errors.New("invalid or duplicate runtime selector value")
				}
				seen[v] = true
			}
			if !seen[sel.Initial[name]] {
				return errors.New("runtime selector initial value is not allowed")
			}
			for _, c := range m.Configuration {
				if c.Name == name && c.Secret {
					return errors.New("runtime selector cannot be secret")
				}
			}
		}
	}
	for _, s := range m.Services {
		if s.EnabledWhen != nil {
			c := s.EnabledWhen
			if s.Role == "app" || sel == nil || !slices.Contains(sel.Selectors[c.Name], c.Equals) {
				return errors.New("enabled_when needs a declared runtime selector; app must remain unconditional")
			}
			if s.Build != nil {
				return errors.New("conditional services require pinned images so reload can enable them without fetching repository source")
			}
		}
		if len(s.DatabaseBindings) > 8 || s.ShareAppNetwork && len(s.DatabaseBindings) > 0 {
			return errors.New("database_bindings must be bounded and cannot change a shared network namespace")
		}
		seen := map[string]bool{}
		for _, key := range s.DatabaseBindings {
			if !envPattern.MatchString(key) || seen[key] {
				return errors.New("invalid/duplicate service database binding")
			}
			seen[key] = true
		}
	}
	return nil
}
func (m StackManifest) selectedServices(selection *StackSelectionSnapshot) []StackService {
	out := []StackService{}
	for _, s := range m.Services {
		if s.EnabledWhen == nil || selection != nil && selection.Values[s.EnabledWhen.Name] == s.EnabledWhen.Equals {
			out = append(out, s)
		}
	}
	return out
}
func (m StackManifest) serviceBudget(profile StackProfile, s StackService) StackBudget {
	if m.Version == 2 {
		return m.ProfileResources[profile.ID].Services[s.Name]
	}
	return profile.budget(s.Role)
}
func (m StackManifest) validatePlan(a App, selection *StackSelectionSnapshot) error {
	if m.Version != 2 {
		return nil
	}
	profile, ok := profileByID(a.RuntimeProfile)
	if !ok {
		return errors.New("choose a stack profile")
	}
	resources, ok := m.ProfileResources[profile.ID]
	if !ok {
		return errors.New("manifest does not support the selected resource profile")
	}
	memory := resources.Database.MemoryMiB
	cpu, _ := budgetCPU(resources.Database)
	actualMemory, actualCPU := 0, 0
	for _, b := range a.DatabaseBudgets {
		n, err := budgetCPU(b)
		if err != nil {
			return errors.New("attached database has no valid recorded CPU ceiling; its configuration is preserved, explicitly attach a bounded database before claiming this profile")
		}
		actualMemory += b.MemoryMiB
		actualCPU += n
	}
	if actualMemory > memory || actualCPU > cpu {
		return errors.New("attached database ceilings exceed this manifest's database allowance; database sizing is preserved, increase the allowance/profile or change attachments explicitly")
	}
	for _, s := range m.selectedServices(selection) {
		b := resources.Services[s.Name]
		memory += b.MemoryMiB
		n, _ := budgetCPU(b)
		cpu += n
		for _, key := range s.DatabaseBindings {
			if a.Bindings[key] == "" || a.Env[key] == "" {
				return fmt.Errorf("service %s requires existing database attachment %s", s.Name, key)
			}
		}
	}
	ceilingCPU := 0
	for _, b := range []StackBudget{profile.App, profile.Mail, profile.Office, profile.Database} {
		n, _ := budgetCPU(b)
		ceilingCPU += n
	}
	if memory+profile.ReserveMiB > profile.HostMiB || cpu > ceilingCPU {
		return fmt.Errorf("selected stack services need %d MiB plus %d MiB host allowance and %.3f CPU; profile %s permits %d MiB and %.3f CPU; choose a larger supported profile or remeasure service budgets", memory, profile.ReserveMiB, float64(cpu)/1000, profile.ID, profile.HostMiB, float64(ceilingCPU)/1000)
	}
	return nil
}
func cloneStackSelection(s *StackSelectionSnapshot) *StackSelectionSnapshot {
	if s == nil {
		return nil
	}
	return &StackSelectionSnapshot{Version: s.Version, Revision: s.Revision, Values: maps.Clone(s.Values)}
}
