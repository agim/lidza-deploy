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
	v := c.updateSettingsSnapshot()
	latest, err := c.latestRelease(r)
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
		v := c.updateSettingsSnapshot()
		v.Automatic = in.Automatic
		if err := c.saveUpdateSettings(v); err != nil {
			agent.Fail(w, 500, err)
			return
		}
		agent.JSON(w, 200, v)
		return
	case "POST":
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
	agent.JSON(w, 200, map[string]any{"settings": c.updateSettingsSnapshot(), "control_version": buildinfo.Version, "servers": hosts})
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
			return nil
		} // Do not update the panel after a failed remote host.
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

func (c *Control) latestRelease(r *http.Request) (string, error) {
	if c.releaseCheck != nil {
		return c.releaseCheck(r)
	}
	return agent.LatestRelease(r)
}
