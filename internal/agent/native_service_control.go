package agent

import (
	"errors"
	"net/http"
)

// Service control of adopted applications.
//
// Applications this agent created always run under their own account and
// units, so the panel can always stop, start and restart them. Applications
// adopted from the existing host setup run under the shared deployment
// account; starting, stopping or restarting their systemd units from the
// panel is a per-server setting. The agent configuration's
// native.service_control is the initial value; once the panel sets it, the
// stored setting (diskState.NativeServiceControl) wins. Every check reads
// it when it runs, so a change applies without restarting the agent.

// ServiceControlStatus is the server's service control setting for adopted
// applications.
type ServiceControlStatus struct {
	// Supported is false when native management is not configured; the
	// setting then has nothing to control.
	Supported bool `json:"supported"`
	// Enabled reports that adopted applications' units can be started,
	// stopped and restarted from the panel.
	Enabled bool `json:"enabled"`
	// Source is "setting" once the panel set it, "config" otherwise.
	Source string `json:"source"`
	// ConfigDefault is the agent configuration's native.service_control.
	ConfigDefault bool `json:"config_default"`
	// Account is the shared deployment account adopted applications run as.
	Account string `json:"account,omitempty"`
}

// ServiceControlSettings is the panel's change of the setting.
type ServiceControlSettings struct {
	Enabled *bool `json:"enabled"`
}

// Validate checks a change before it is stored or forwarded.
func (s ServiceControlSettings) Validate() error {
	if s.Enabled == nil {
		return errors.New("enabled is required")
	}
	return nil
}

const serviceControlOffReason = "Service control is off for this server."

// nativeAdoptedServiceControl reports whether adopted applications' units
// may be controlled from the panel. It takes m.mu.
func (m *Manager) nativeAdoptedServiceControl() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nativeAdoptedServiceControlLocked()
}

// nativeAdoptedServiceControlLocked is nativeAdoptedServiceControl for a
// caller holding m.mu.
func (m *Manager) nativeAdoptedServiceControlLocked() bool {
	if m.cfg.Native == nil {
		return false
	}
	if m.data.NativeServiceControl != nil {
		return *m.data.NativeServiceControl
	}
	return m.cfg.Native.ServiceControl
}

// nativeServiceControlFor reports whether application id's units may be
// controlled from the panel: always for created applications, by the
// server setting for adopted ones.
func (m *Manager) nativeServiceControlFor(id string) bool {
	if _, created := m.nativeCreatedPolicy(id); created {
		return true
	}
	return m.nativeAdoptedServiceControl()
}

func (m *Manager) serviceControlStatus() ServiceControlStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg.Native == nil {
		return ServiceControlStatus{Source: "config"}
	}
	status := ServiceControlStatus{Supported: true, Enabled: m.nativeAdoptedServiceControlLocked(), Source: "config", ConfigDefault: m.cfg.Native.ServiceControl, Account: m.cfg.Native.User}
	if m.data.NativeServiceControl != nil {
		status.Source = "setting"
	}
	return status
}

// setServiceControl stores the panel's setting.
func (m *Manager) setServiceControl(s ServiceControlSettings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg.Native == nil {
		return errors.New("native management is not configured on this server")
	}
	previous := m.data.NativeServiceControl
	enabled := *s.Enabled
	m.data.NativeServiceControl = &enabled
	if err := m.save(); err != nil {
		m.data.NativeServiceControl = previous
		return errors.New("could not save the service control setting")
	}
	return nil
}

func (m *Manager) serviceControlRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/host/service-control", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, 200, m.serviceControlStatus())
	})
	mux.HandleFunc("PUT /v1/host/service-control", func(w http.ResponseWriter, r *http.Request) {
		var s ServiceControlSettings
		if err := Decode(w, r, &s); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := s.Validate(); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setServiceControl(s); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, m.serviceControlStatus())
	})
}
