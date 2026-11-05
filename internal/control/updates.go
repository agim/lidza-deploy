package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/buildinfo"
	"github.com/agim/lidza-deploy/internal/updates"
	"golang.org/x/mod/semver"
)

type UpdateTarget struct {
	ServerID string    `json:"server_id"`
	Name     string    `json:"name"`
	State    string    `json:"state"`
	Message  string    `json:"message,omitempty"`
	Started  time.Time `json:"started,omitempty"`
}
type UpdateSettings struct {
	Automatic  bool           `json:"automatic"`
	Latest     string         `json:"latest,omitempty"`
	Checked    time.Time      `json:"checked,omitempty"`
	CheckError string         `json:"check_error,omitempty"`
	Version    string         `json:"version,omitempty"`
	Targets    []UpdateTarget `json:"targets"`
	Channel    *UpdateChannel `json:"channel,omitempty"`
	Bundle     *UpdateBundle  `json:"bundle,omitempty"`
}

func localUpdateServer(s Server) bool {
	u, e := url.Parse(s.URL)
	if e != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
func (c *Control) updateSettingsSnapshot() UpdateSettings {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.data.Updates
	v.Targets = slices.Clone(v.Targets)
	if v.Targets == nil {
		v.Targets = []UpdateTarget{}
	}
	return v
}
func (c *Control) saveUpdateSettings(v UpdateSettings) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.data.Updates
	c.data.Updates = v
	if err := c.save(); err != nil {
		c.data.Updates = old
		return err
	}
	return nil
}
func updateInProgress(v UpdateSettings) bool {
	for _, t := range v.Targets {
		if t.State == "failed" {
			return false
		}
	}
	for _, t := range v.Targets {
		if t.State == "waiting" || t.State == "updating" {
			return true
		}
	}
	return false
}
func (c *Control) checkUpdateRelease(r *http.Request) error {
	latest, err := c.latestRelease(r)
	v := c.updateSettingsSnapshot() // a channel check records the verified bundle itself
	v.Checked = time.Now().UTC()
	v.CheckError = ""
	if err != nil {
		v.CheckError = err.Error()
	} else {
		v.Latest = latest
	}
	if e := c.saveUpdateSettings(v); e != nil {
		return e
	}
	return err
}
func (c *Control) updates(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" || r.Method == "PATCH" {
		c.updateMu.Lock()
		defer c.updateMu.Unlock()
	}
	switch r.Method {
	case "PATCH":
		var in struct {
			Automatic bool `json:"automatic"`
		}
		if err := agent.Decode(w, r, &in); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		if in.Automatic && !c.updatesAvailable() {
			agent.Fail(w, 409, errors.New("configure the signed update channel before enabling automatic updates"))
			return
		}
		v := c.updateSettingsSnapshot()
		v.Automatic = in.Automatic
		if err := c.saveUpdateSettings(v); err != nil {
			agent.Fail(w, 500, err)
			return
		}
		agent.JSON(w, 200, v)
		return
	case "POST":
		if r.PathValue("action") == "channel" {
			c.updateChannelRoute(w, r)
			return
		}
		if r.PathValue("action") == "check" {
			if err := c.checkUpdateRelease(r); err != nil {
				agent.Fail(w, 502, err)
				return
			}
			agent.JSON(w, 200, c.updateSettingsSnapshot())
			return
		}
		if r.PathValue("action") != "install" {
			http.NotFound(w, r)
			return
		}
		v := c.updateSettingsSnapshot()
		if updateInProgress(v) {
			agent.Fail(w, 409, errors.New("an update is already in progress"))
			return
		}
		if err := c.checkUpdateRelease(r); err != nil {
			agent.Fail(w, 502, err)
			return
		}
		v = c.updateSettingsSnapshot()
		if err := c.prepareUpdatePlan(v); err != nil {
			agent.Fail(w, 409, err)
			return
		}
		agent.JSON(w, 202, c.updateSettingsSnapshot())
		return
	}
	type host struct {
		ServerID string `json:"server_id"`
		Name     string `json:"name"`
		Local    bool   `json:"local"`
		Error    string `json:"error,omitempty"`
		agent.UpgradeStatus
	}
	hosts := []host{}
	for _, result := range c.readFleet(r, "/v1/upgrade") {
		h := host{ServerID: result.Server.ID, Name: result.Server.Name, Local: localUpdateServer(result.Server)}
		if result.Err != nil {
			h.Error = "Agent unavailable"
		} else if json.Unmarshal(result.Body, &h.UpgradeStatus) != nil {
			h.Error = "Invalid agent response"
		}
		hosts = append(hosts, h)
	}
	settings := c.updateSettingsSnapshot()
	supported := c.updatesAvailable()
	if !supported {
		settings.Automatic = false
		settings.Latest = "" // never advertise a public release to a signed distribution
	}
	if settings.Bundle != nil {
		settings.Bundle = &UpdateBundle{Version: settings.Bundle.Version, KeyID: settings.Bundle.KeyID} // manifest stays server-side
	}
	agent.JSON(w, 200, map[string]any{"settings": settings, "automatic_updates_supported": supported, "control_version": buildinfo.Version, "servers": hosts, "channel": c.channelView()})
}
func (c *Control) prepareUpdatePlan(v UpdateSettings) error {
	hosts := c.servers()
	if !slices.ContainsFunc(hosts, localUpdateServer) {
		return errors.New("connect the colocated agent on this control-panel host before updating the fleet")
	}
	if len(hosts) == 0 {
		return errors.New("connect a managed agent before updating")
	}
	slices.SortStableFunc(hosts, func(a, b Server) int {
		if localUpdateServer(a) == localUpdateServer(b) {
			return 0
		}
		if localUpdateServer(a) {
			return 1
		}
		return -1
	})
	v.Version = v.Latest
	v.Targets = []UpdateTarget{}
	for _, s := range hosts {
		v.Targets = append(v.Targets, UpdateTarget{ServerID: s.ID, Name: s.Name, State: "waiting"})
	}
	return c.saveUpdateSettings(v)
}

// The framework's durable minute schedule runs this even when the browser is closed.
func (c *Control) updateTick(ctx context.Context) error {
	// A retained public rollout plan must never dispatch after a signed
	// distribution build takes over the panel, even when its last release
	// check is still fresh.
	if !c.updatesAvailable() {
		return nil
	}
	c.updateMu.Lock()
	defer c.updateMu.Unlock()
	r, _ := http.NewRequestWithContext(ctx, "GET", c.cfg.PublicURL, nil)
	v := c.updateSettingsSnapshot()
	if !updateInProgress(v) && time.Since(v.Checked) >= time.Hour {
		if err := c.checkUpdateRelease(r); err != nil {
			return err
		}
		v = c.updateSettingsSnapshot()
		// A failed release requires an explicit retry or a newer release.
		if v.Automatic && v.Latest != "" && v.Version != v.Latest {
			if err := c.prepareUpdatePlan(v); err != nil {
				return err
			}
			v = c.updateSettingsSnapshot()
		}
	}
	for i := range v.Targets {
		t := &v.Targets[i]
		if t.State == "failed" {
			// A host that reached this release by other means (for example a
			// manual bootstrap) no longer blocks the batch.
			var current agent.UpgradeStatus
			if err := c.agentCall(r, t.ServerID, "GET", "/v1/upgrade", nil, &current); err == nil && (current.Current == v.Version ||
				semver.IsValid(current.Current) && semver.IsValid(v.Version) && semver.Compare(current.Current, v.Version) > 0) {
				t.State, t.Message = "succeeded", "Release installed"
				if err := c.saveUpdateSettings(v); err != nil {
					return err
				}
				continue
			}
			return nil // Do not update the panel after a failed remote host.
		}
		if t.State == "succeeded" {
			continue
		}
		var status agent.UpgradeStatus
		if err := c.agentCall(r, t.ServerID, "GET", "/v1/upgrade", nil, &status); err != nil {
			t.Message = "Agent unavailable; will retry"
			return c.saveUpdateSettings(v)
		}
		if status.State == "upgrading" || status.State == "queued" {
			if t.Started.IsZero() {
				t.Started = time.Now().UTC()
			}
			if time.Since(t.Started) > 20*time.Minute {
				t.State = "failed"
				t.Message = "Update health checks did not finish; inspect the server updater"
				return c.saveUpdateSettings(v)
			}
			t.State = "updating"
			t.Message = status.Message
			return c.saveUpdateSettings(v)
		}
		if t.State == "updating" && status.State == "failed" {
			t.State = "failed"
			t.Message = status.Message
			return c.saveUpdateSettings(v)
		}
		needsPanel := false
		for _, host := range c.servers() {
			if host.ID == t.ServerID && localUpdateServer(host) && semver.IsValid(buildinfo.Version) && semver.Compare(buildinfo.Version, v.Version) < 0 {
				needsPanel = true
			}
		}
		if !needsPanel && (status.Current == v.Version || (semver.IsValid(status.Current) && semver.Compare(status.Current, v.Version) > 0)) {
			t.State = "succeeded"
			t.Message = "Release installed"
			if err := c.saveUpdateSettings(v); err != nil {
				return err
			}
			continue
		}
		if !status.Supported {
			t.State = "failed"
			t.Message = "Run the current installer on this server to enable managed updates"
			return c.saveUpdateSettings(v)
		}
		if t.State == "updating" && status.State == "succeeded" {
			t.State = "waiting"
		}
		if t.State == "updating" && status.State == "idle" && time.Since(t.Started) > time.Minute {
			t.State = "waiting"
		}
		if t.State == "updating" {
			if time.Since(t.Started) > 20*time.Minute {
				t.State = "failed"
				t.Message = "Update did not complete; check server upgrade logs"
				return c.saveUpdateSettings(v)
			}
			return nil
		}
		if c.channelConfigured() {
			// Only signed bundles may be dispatched on the signed channel.
			if !updates.ValidVersion(v.Version) || v.Bundle == nil || v.Bundle.Version != v.Version {
				t.State = "failed"
				t.Message = "Release plan is not a verified signed bundle; check for releases again"
				return c.saveUpdateSettings(v)
			}
			t.Message = "Transferring verified bundle"
			if err := c.saveUpdateSettings(v); err != nil {
				return err
			}
			if err := c.transferBundle(r, c.serverByID(t.ServerID), *v.Bundle, agentArch(status)); err != nil {
				t.State = "failed"
				t.Message = err.Error()
				return c.saveUpdateSettings(v)
			}
		}
		// Persist intent before the root helper can restart this process.
		t.State = "updating"
		t.Started = time.Now().UTC()
		t.Message = "Queuing verified release"
		if err := c.saveUpdateSettings(v); err != nil {
			return err
		}
		if err := c.agentCall(r, t.ServerID, "POST", "/v1/upgrade", map[string]string{"version": v.Version}, nil); err != nil {
			t.State = "waiting"
			t.Message = "Server busy or unavailable; will retry"
			return c.saveUpdateSettings(v)
		}
		return nil // Check readiness on the next durable tick before the next server.
	}
	return nil
}

// updatesAvailable reports whether this panel has a release source. Builds
// without pinned signing keys follow the project's published releases; a
// build that pins keys installs only bundles from its configured channel.
func (c *Control) updatesAvailable() bool {
	return c.releaseCheck != nil || c.channelConfigured() || !channelTrust().Configured()
}

func (c *Control) latestRelease(r *http.Request) (string, error) {
	if c.channelConfigured() {
		return c.channelLatest(r)
	}
	if c.releaseCheck != nil {
		return c.releaseCheck(r)
	}
	return agent.LatestRelease(r)
}

func (c *Control) serverByID(id string) Server {
	for _, s := range c.servers() {
		if s.ID == id {
			return s
		}
	}
	return Server{}
}
