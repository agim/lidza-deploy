package agent

// The agent link to a control panel (internal/agentlink). A server whose
// agent no other server can reach (loopback listener, a firewall admitting
// only 80 and 443) stays in a panel's server list by connecting out to the
// panel's public HTTPS address; the panel sends its calls back through
// those connections. A panel move sets it up on the panel's old host when
// the operator keeps that server. The configuration persists and the agent
// reconnects forever.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agentlink"
)

// ControlLink is where this agent connects out to.
type ControlLink struct {
	// URL is the panel's public URL.
	URL string `json:"url"`
	// Address is dialed instead of resolving the URL's host (the panel's
	// new server while DNS may still point to the old one).
	Address string    `json:"address,omitempty"`
	Server  string    `json:"server"`
	Updated time.Time `json:"updated"`
}

// controlLinkTLS is the link's TLS client configuration; nil verifies
// against the system roots (tests replace it).
var controlLinkTLS *tls.Config

var controlLinkServerPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

func (m *Manager) controlLinkPath() string { return filepath.Join(m.cfg.DataDir, "control-link.json") }

func (m *Manager) controlLink() *ControlLink {
	var l ControlLink
	raw, err := os.ReadFile(m.controlLinkPath())
	if err != nil || json.Unmarshal(raw, &l) != nil || l.URL == "" || l.Server == "" {
		return nil
	}
	return &l
}

func validateControlLink(l ControlLink) error {
	u, err := url.Parse(l.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") {
		return errors.New("the panel URL must be https://<hostname>")
	}
	if !controlLinkServerPattern.MatchString(l.Server) {
		return errors.New("invalid server id")
	}
	if l.Address != "" {
		if _, err := netip.ParseAddr(l.Address); err != nil {
			return errors.New("invalid panel address")
		}
	}
	return nil
}

// setControlLink stores the link (nil removes it) and restarts it.
func (m *Manager) setControlLink(l *ControlLink) error {
	if l == nil {
		if err := os.Remove(m.controlLinkPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		if err := validateControlLink(*l); err != nil {
			return err
		}
		l.URL = strings.TrimSuffix(l.URL, "/")
		l.Updated = time.Now().UTC()
		raw, _ := json.MarshalIndent(l, "", "  ")
		if err := os.MkdirAll(m.cfg.DataDir, 0o700); err != nil {
			return err
		}
		if err := writeFileAtomic(m.controlLinkPath(), raw, 0o600); err != nil {
			return err
		}
	}
	m.startControlLink()
	return nil
}

// startControlLink (re)starts the link loop for the stored configuration.
func (m *Manager) startControlLink() {
	m.linkMu.Lock()
	defer m.linkMu.Unlock()
	if m.linkCancel != nil {
		m.linkCancel()
		m.linkCancel = nil
	}
	l := m.controlLink()
	if l == nil || m.ctx == nil || m.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.linkCancel = cancel
	run := agentlink.Agent{URL: l.URL, Address: l.Address, Server: l.Server, Token: m.cfg.APIKey, Handler: Handler(m), TLS: controlLinkTLS}
	m.wg.Add(1)
	go func() { defer m.wg.Done(); run.Run(ctx) }()
}

func (m *Manager) controlLinkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/host/control/link", func(w http.ResponseWriter, r *http.Request) {
		var in ControlLink
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setControlLink(&in); err != nil {
			Fail(w, 400, err)
			return
		}
		JSON(w, 200, m.controlLink())
	})
	mux.HandleFunc("DELETE /v1/host/control/link", func(w http.ResponseWriter, r *http.Request) {
		if err := m.setControlLink(nil); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "removed"})
	})
}
