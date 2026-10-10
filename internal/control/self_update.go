package control

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/agim/lidza-deploy/internal/agent"
	"golang.org/x/mod/semver"
)

// Self-updates use the installed root helper, never a repository build or shell input.
func (c *Control) selfUpdate(w http.ResponseWriter, r *http.Request) {
	if len(c.cfg.SelfUpdateSecret) < 32 || c.cfg.SelfUpdateServer == "" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		agent.Fail(w, 413, errors.New("webhook too large"))
		return
	}
	signature := r.Header.Get("X-Hub-Signature-256")
	raw, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	mac := hmac.New(sha256.New, []byte(c.cfg.SelfUpdateSecret))
	mac.Write(body)
	if err != nil || !strings.HasPrefix(signature, "sha256=") || !hmac.Equal(raw, mac.Sum(nil)) {
		http.Error(w, "invalid signature", 401)
		return
	}
	if r.Header.Get("X-GitHub-Event") == "ping" {
		agent.JSON(w, 200, map[string]string{"status": "ready"})
		return
	}
	var event struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Release struct {
			Tag        string `json:"tag_name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
		} `json:"release"`
	}
	if json.Unmarshal(body, &event) != nil {
		agent.Fail(w, 400, errors.New("invalid webhook"))
		return
	}
	if r.Header.Get("X-GitHub-Event") != "release" || event.Action != "published" || event.Repository.FullName != "agim/lidza-deploy" || event.Release.Draft || event.Release.Prerelease || !semver.IsValid(event.Release.Tag) {
		agent.JSON(w, 200, map[string]string{"status": "ignored"})
		return
	}
	// A remote hosting agent must never be mistaken for this control panel's host.
	local := false
	for _, server := range c.servers() {
		if server.ID == c.cfg.SelfUpdateServer {
			u, e := url.Parse(server.URL)
			if e == nil {
				h := u.Hostname()
				local = h == "127.0.0.1" || h == "localhost" || h == "::1"
			}
		}
	}
	if !local {
		agent.Fail(w, 503, errors.New("self-update requires a colocated loopback agent"))
		return
	}
	var status agent.UpgradeStatus
	if err = c.agentCall(r, c.cfg.SelfUpdateServer, "GET", "/v1/upgrade?check=1", nil, &status); err != nil {
		agent.Fail(w, 503, errors.New("updater unavailable; redeliver after recovery"))
		return
	}
	if status.Current == event.Release.Tag || (semver.IsValid(status.Current) && semver.Compare(event.Release.Tag, status.Current) <= 0) || status.Latest != event.Release.Tag {
		agent.JSON(w, 200, map[string]string{"status": "ignored"})
		return
	}
	if !status.Supported {
		agent.Fail(w, 503, errors.New("managed upgrade helper unavailable"))
		return
	}
	if status.State == "queued" || status.State == "upgrading" {
		agent.Fail(w, 503, errors.New("upgrade already pending; redeliver after completion"))
		return
	}
	if err = c.agentCall(r, c.cfg.SelfUpdateServer, "POST", "/v1/upgrade", map[string]string{"version": event.Release.Tag}, nil); err != nil {
		agent.Fail(w, 503, errors.New("upgrade not queued; redeliver when server is idle"))
		return
	}
	agent.JSON(w, 202, map[string]string{"status": "queued"})
}
