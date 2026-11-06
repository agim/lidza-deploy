package agent

import (
	"net/http"
	"time"
)

// hostRoutes exposes host updates, reboots and the managed firewall.
func (m *Manager) hostRoutes(mux *http.ServeMux) {
	m.bundlerRoutes(mux)
	mux.HandleFunc("GET /v1/host/updates", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, 200, m.hostUpdatesStatus(r.Context(), r.URL.Query().Get("refresh") == "1"))
	})
	mux.HandleFunc("PATCH /v1/host/updates", func(w http.ResponseWriter, r *http.Request) {
		var p HostUpdatesPolicy
		if err := Decode(w, r, &p); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setHostUpdatesPolicy(p); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, m.hostUpdatesStatus(r.Context(), false))
	})
	mux.HandleFunc("POST /v1/host/updates/run", func(w http.ResponseWriter, r *http.Request) {
		var in HostUpdatesRun
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.runHostUpdates(in); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, m.hostUpdatesStatus(r.Context(), false))
	})
	mux.HandleFunc("POST /v1/host/reboot", func(w http.ResponseWriter, r *http.Request) {
		var in HostRebootRequest
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.rebootHost(in); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, m.hostUpdatesStatus(r.Context(), false))
	})
	mux.HandleFunc("GET /v1/host/firewall", func(w http.ResponseWriter, r *http.Request) {
		view, err := m.firewallView(r.Context())
		if err != nil {
			Fail(w, 502, err)
			return
		}
		JSON(w, 200, view)
	})
	mux.HandleFunc("PUT /v1/host/firewall", func(w http.ResponseWriter, r *http.Request) {
		var in HostFirewallModel
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		view, err := m.saveFirewallModel(r.Context(), in)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, view)
	})
	mux.HandleFunc("GET /v1/host/firewall/plan", func(w http.ResponseWriter, r *http.Request) {
		plan, err := m.firewallPlan(r.Context())
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, plan)
	})
	mux.HandleFunc("POST /v1/host/firewall/apply", func(w http.ResponseWriter, r *http.Request) {
		// Installing ufw and applying rules can outlast the default write deadline.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(11 * time.Minute))
		var in HostFirewallApply
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		view, err := m.applyFirewall(r.Context(), in)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, view)
	})
	mux.HandleFunc("POST /v1/host/firewall/confirm", func(w http.ResponseWriter, r *http.Request) {
		view, err := m.confirmFirewall(r.Context())
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, view)
	})
}
