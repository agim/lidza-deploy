package agent

import (
	"errors"
	"strings"
)

func validateReservedDomains(domains []string) error {
	if len(domains) > 500 {
		return errors.New("at most 500 reserved domains")
	}
	seen := map[string]bool{}
	for _, domain := range domains {
		if seen[domain] || validateHostname(strings.TrimPrefix(domain, "*.")) != nil {
			return errors.New("reserved domains require unique lowercase DNS names or leading wildcard subtrees")
		}
		seen[domain] = true
	}
	return nil
}

func (c Config) reservedHostname(host string) bool {
	for _, reserved := range c.ReservedDomains {
		if strings.HasPrefix(reserved, "*.") {
			if strings.HasSuffix(host, "."+strings.TrimPrefix(reserved, "*.")) {
				return true
			}
		} else if host == reserved {
			return true
		}
	}
	return false
}

func (m *Manager) checkReservedDomains(a App) error {
	for _, host := range a.Hostnames() {
		if m.cfg.reservedHostname(host) {
			return errors.New("domain is reserved for an independently managed application")
		}
	}
	return nil
}
