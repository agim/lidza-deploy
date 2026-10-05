// Adapted from monolithcms-app/agent/server.go: authenticated JSON API and
// asynchronous provisioning, now with bounded work and fail-closed auth.
package agent

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func Fail(w http.ResponseWriter, status int, err error) {
	JSON(w, status, map[string]string{"error": err.Error()})
}
func Decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
func Handler(m *Manager) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, map[string]string{"status": "ok"}) })
	// Caddy calls this loopback endpoint before issuing any certificate.
	mux.HandleFunc("GET /tls/allow", func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			w.WriteHeader(403)
			return
		}
		domain := strings.ToLower(r.URL.Query().Get("domain"))
		if m.domainAllowed(r.Context(), domain) {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(403)
	})
	private := http.NewServeMux()
	private.HandleFunc("PUT /v1/error-reporting", m.configureErrorReporting)
	private.HandleFunc("GET /v1/server-health", m.healthRoute)
	private.HandleFunc("POST /v1/apps/{id}/restore", m.restoreRoute)
	private.HandleFunc("PUT /v1/apps/{id}/maintenance", func(w http.ResponseWriter, r *http.Request) {
		var in Maintenance
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setMaintenance(r.PathValue("id"), in); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, in)
	})
	m.databaseRoutes(private)
	m.taskRoutes(private)
	m.upgradeRoutes(private)
	private.HandleFunc("DELETE /v1/previews/{id}", m.previewRoute)
	private.HandleFunc("GET /v1/apps", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, m.Apps()) })
	private.HandleFunc("PUT /v1/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		var a App
		if err := Decode(w, r, &a); err != nil {
			Fail(w, 400, err)
			return
		}
		a.ID = r.PathValue("id")
		if err := m.Upsert(a); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "saved"})
	})
	private.HandleFunc("GET /v1/apps/{id}/settings", func(w http.ResponseWriter, r *http.Request) {
		out, err := m.Settings(r.PathValue("id"))
		if err != nil {
			Fail(w, 404, err)
			return
		}
		JSON(w, 200, out)
	})
	private.HandleFunc("PATCH /v1/apps/{id}/settings", func(w http.ResponseWriter, r *http.Request) {
		var p SettingsPatch
		if err := Decode(w, r, &p); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.PatchSettings(r.PathValue("id"), p); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "saved"})
	})
	private.HandleFunc("DELETE /v1/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Retire(r.Context(), r.PathValue("id")); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "removed"})
	})
	private.HandleFunc("GET /v1/deployments", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, m.Deployments()) })
	private.HandleFunc("POST /v1/apps/{id}/reload", func(w http.ResponseWriter, r *http.Request) {
		d, err := m.Reload(r.PathValue("id"))
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, d)
	})
	private.HandleFunc("POST /v1/apps/{id}/deploy", func(w http.ResponseWriter, r *http.Request) {
		var req DeployRequest
		if err := Decode(w, r, &req); err != nil {
			Fail(w, 400, err)
			return
		}
		d, err := m.Enqueue(r.PathValue("id"), req)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, d)
	})
	private.HandleFunc("POST /v1/apps/{id}/rollback", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Rollback(r.Context(), r.PathValue("id")); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "rolled_back"})
	})
	private.HandleFunc("GET /v1/apps/{id}/analytics-errors", m.errorsRoute)
	private.HandleFunc("GET /v1/apps/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		a, ok := m.data.Apps[r.PathValue("id")]
		env := maps.Clone(a.Env)
		m.mu.Unlock()
		if !ok || a.Retiring || a.Current == nil {
			Fail(w, 409, errors.New("no running release"))
			return
		}
		s, err := m.runtime.Logs(r.Context(), a.Current)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"logs": scrubOutput(s, outputSecrets(env))})
	})
	mux.Handle("/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := sha256.Sum256([]byte(m.cfg.APIKey))
		got := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
		if len(m.cfg.APIKey) < 32 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		private.ServeHTTP(w, r)
	}))
	return mux
}
func Proxy(m *Manager) http.Handler {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 100}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if r.URL.Path == domainProbePath {
			m.serveDomainProof(w, r, host)
			return
		}
		if r.URL.Path == "/metrics" || r.URL.Path == "/readyz" || r.URL.Path == "/healthz" {
			http.NotFound(w, r)
			return
		}
		if m.maintenancePage(w, r, host) {
			return
		}
		release := m.Target(host)
		if release == nil {
			http.Error(w, "application unavailable", http.StatusServiceUnavailable)
			return
		}
		target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + release.Port}
		p := httputil.NewSingleHostReverseProxy(target)
		p.Transport = transport
		p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		}
		// Only the local TLS terminator supplies trusted forwarding headers.
		remote, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(remote); ip == nil || !ip.IsLoopback() {
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("X-Forwarded-Host")
			r.Header.Del("X-Forwarded-Proto")
		}
		p.ServeHTTP(w, r)
	})
}
