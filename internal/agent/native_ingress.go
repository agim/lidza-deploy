package agent

// Native ingress adapts an explicitly published native routing record to the
// existing independent proxy. It does not discover a serving target, contact
// systemd/application ports, write inventory, or authorize Rails activation.
import (
	"errors"
	"net"
	"regexp"
	"slices"
)

type NativeIngressRoute struct {
	ReleaseID string `json:"release_id"`
	Port      string `json:"port"`
}

func (c Config) validateNativeIngressTargets() error {
	if len(c.NativeIngressTargets) == 0 {
		return nil
	}
	if !c.IngressOnly || c.NativeOnly || c.Native != nil {
		return errors.New("native ingress targets require a separate read-only ingress role")
	}
	if len(c.NativeIngressTargets) > 100 {
		return errors.New("too many native ingress applications")
	}
	ports := map[string]bool{}
	for id, targets := range c.NativeIngressTargets {
		if !idPattern.MatchString(id) || len(targets) == 0 || len(targets) > 8 {
			return errors.New("native ingress requires an app identity and bounded exact target ports")
		}
		for _, port := range targets {
			if port == "" || nativePort(map[string]string{"PORT": port}) != port || ports[port] {
				return errors.New("native ingress target ports must be valid and unique")
			}
			for _, listener := range []string{c.Listen, c.ProxyListen, c.TLSListen} {
				_, listenerPort, _ := net.SplitHostPort(listener)
				if port == listenerPort {
					return errors.New("native ingress cannot target its own listeners")
				}
			}
			if c.Gateway != nil {
				_, listenerPort, _ := net.SplitHostPort(c.Gateway.Listen)
				if port == listenerPort {
					return errors.New("native ingress cannot target its own gateway")
				}
			}
			ports[port] = true
		}
	}
	return nil
}

func (m *Manager) addNativeIngressRoutes(next *diskState) error {
	if len(m.cfg.NativeIngressTargets) == 0 {
		return nil
	}
	hosts := map[string]bool{}
	for _, app := range next.Apps {
		hosts[app.Domain] = true
		for _, host := range app.AdditionalDomains {
			hosts[host] = true
		}
	}
	for id, targets := range m.cfg.NativeIngressTargets {
		route, published := next.NativeIngress[id]
		if !published {
			continue // Standby is safe: no route is inferred from discovery.
		}
		a, found := next.NativeApps[id]
		if !found || a.ID != id || a.Runtime != "rails" && a.Runtime != "" || len(a.Services) == 0 || !nativeNamePattern.MatchString(a.Name) || !slices.Contains(targets, route.Port) || !regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`).MatchString(route.ReleaseID) {
			return errors.New("native ingress route lacks a valid approved Rails binding")
		}
		if _, conflict := next.Apps[id]; conflict || len(a.AdditionalDomains) > 20 {
			return errors.New("native ingress application identity conflicts with another route")
		}
		for _, service := range a.Services {
			if !nativeUnitPattern.MatchString(service) {
				return errors.New("native ingress service binding is invalid")
			}
		}
		for _, host := range append([]string{a.Domain}, a.AdditionalDomains...) {
			canonical, valid := customerHostname(host)
			if !valid || canonical != host || hosts[host] {
				return errors.New("native ingress host binding is invalid or conflicts with another route")
			}
			hosts[host] = true
		}
		// This projection exists only inside the reader. The original sealed
		// inventory stays untouched, and no manager sees a container application.
		next.Apps[id] = App{ID: id, Domain: a.Domain, AdditionalDomains: slices.Clone(a.AdditionalDomains), Native: &a, Current: &Release{ID: route.ReleaseID, Port: route.Port}}
	}
	return nil
}

func (m *Manager) nativeIngressApplication(host string) (App, bool) {
	if !m.cfg.IngressOnly || len(m.cfg.NativeIngressTargets) == 0 {
		return App{}, false
	}
	if canonical, valid := customerHostname(host); !valid || canonical != host {
		return App{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.cfg.NativeIngressTargets {
		a, found := m.data.Apps[id]
		if found && a.Native != nil && a.HasDomain(host) {
			return a, true
		}
	}
	return App{}, false
}
