package agent

import (
	"errors"
	"maps"
	"slices"
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
		if m.hostReserved(host) {
			return errors.New("domain is reserved for an independently managed application")
		}
	}
	return nil
}

// hostReserved reports a hostname reserved by the configuration
// (reserved_domains) and not released by deleting the application it was
// reserved for. Safe with or without m.mu held.
func (m *Manager) hostReserved(host string) bool {
	host = strings.ToLower(host)
	if !m.cfg.reservedHostname(host) {
		return false
	}
	if released := m.releasedReservations.Load(); released != nil {
		if _, ok := (*released)[host]; ok {
			return false
		}
	}
	return true
}

// syncReleasedReservations publishes State.ReleasedReservations to
// hostReserved; the caller holds m.mu (or owns the manager).
func (m *Manager) syncReleasedReservations() {
	released := maps.Clone(m.data.ReleasedReservations)
	m.releasedReservations.Store(&released)
}

// releaseArchiveReservations releases the configured reservations of a
// deleted application's hostnames, recording them in its archive.
func (m *Manager) releaseArchiveReservations(archiveID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.AppArchives[archiveID]
	if !ok {
		return nil
	}
	var hosts []string
	for _, host := range a.Hostnames {
		host = strings.ToLower(host)
		if _, released := m.data.ReleasedReservations[host]; !released && m.cfg.reservedHostname(host) && !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	previous, archive := maps.Clone(m.data.ReleasedReservations), a
	if m.data.ReleasedReservations == nil {
		m.data.ReleasedReservations = map[string]string{}
	}
	for _, host := range hosts {
		m.data.ReleasedReservations[host] = archiveID
	}
	a.ReleasedReservations = append(slices.Clone(a.ReleasedReservations), hosts...)
	a.Deleted = slices.Clone(a.Deleted)
	addArchiveItem(&a.Deleted, ArchiveItem{Kind: "reservation", Name: strings.Join(a.ReleasedReservations, ", "), Count: len(a.ReleasedReservations),
		Detail: "hostnames reserved for this application in the agent configuration are released for other applications; a restore reserves them again"})
	m.data.AppArchives[archiveID] = a
	if err := m.save(); err != nil {
		m.data.ReleasedReservations, m.data.AppArchives[archiveID] = previous, archive
		return err
	}
	m.syncReleasedReservations()
	return nil
}

// restoreArchiveReservations reserves again what the archive released,
// except hostnames the restore leaves to another application.
func (m *Manager) restoreArchiveReservations(archiveID string, plan ArchiveRestoreResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.AppArchives[archiveID]
	if !ok || len(a.ReleasedReservations) == 0 {
		return nil
	}
	previous := maps.Clone(m.data.ReleasedReservations)
	changed := false
	for _, host := range a.ReleasedReservations {
		if _, skipped := plan.Skipped[host]; skipped || m.data.ReleasedReservations[host] != archiveID {
			continue
		}
		delete(m.data.ReleasedReservations, host)
		changed = true
	}
	if !changed {
		return nil
	}
	if err := m.save(); err != nil {
		m.data.ReleasedReservations = previous
		return err
	}
	m.syncReleasedReservations()
	return nil
}
