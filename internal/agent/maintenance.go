package agent

import (
	"errors"
	"html/template"
	"net/http"
	"strings"
)

var maintenanceTemplate = template.Must(template.New("maintenance").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Maintenance · Līdza</title><style>body{background:#171214;color:#eee7e3;font:18px system-ui;margin:0;min-height:100vh;display:grid;place-items:center}main{max-width:38rem;padding:3rem}small{color:oklch(76% 0.1 345)}h1{font-size:2rem}</style><main><small>LĪDZA</small><h1>We’ll be back soon</h1><p>{{.}}</p></main></html>`))

type Maintenance struct {
	Enabled bool   `json:"enabled"`
	Message string `json:"message"`
}

func (m *Manager) setMaintenance(id string, in Maintenance) error {
	if len(in.Message) > 500 || strings.ContainsRune(in.Message, '\x00') {
		return errors.New("maintenance message must be at most 500 characters")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[id]
	if !ok || a.Retiring {
		return errors.New("application unavailable")
	}
	old := a
	a.Maintenance = in
	m.data.Apps[id] = a
	if err := m.save(); err != nil {
		m.data.Apps[id] = old
		return err
	}
	return nil
}
func (m *Manager) maintenancePage(w http.ResponseWriter, r *http.Request, host string) bool {
	for _, a := range m.Apps() {
		if a.Domain == host && !a.Retiring && a.Stopped {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "application is stopped", http.StatusServiceUnavailable)
			return true
		}
		if a.Domain == host && !a.Retiring && a.Maintenance.Enabled {
			message := a.Maintenance.Message
			if message == "" {
				message = "We’re performing scheduled maintenance. Please try again shortly."
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "300")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = maintenanceTemplate.Execute(w, message)
			return true
		}
	}
	return false
}
