package agent

import (
	"encoding/json"
	"errors"
	"github.com/agim/lidza-deploy/internal/buildinfo"
	"github.com/agim/lidza-deploy/internal/platform/state"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

type UpgradeStatus struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	Supported bool   `json:"supported"`
}

func (m *Manager) upgradeStatus() UpgradeStatus {
	status := UpgradeStatus{Current: buildinfo.Version, State: "idle"}
	_, err := os.Stat("/usr/local/libexec/lidza-agent-upgrade")
	status.Supported = err == nil && m.cfg.DataDir == "/var/lib/lidza-agent" && runtime.GOOS == "linux"
	var saved UpgradeStatus
	if state.Load(filepath.Join(m.cfg.DataDir, "upgrade", "status.json"), &saved) == nil && saved.State != "" {
		status.State = saved.State
		status.Message = saved.Message
	}
	if _, err := os.Stat(filepath.Join(m.cfg.DataDir, "upgrade", "request")); err == nil {
		status.State = "queued"
	}
	return status
}
func LatestRelease(r *http.Request) (string, error) {
	req, err := http.NewRequestWithContext(r.Context(), "GET", "https://api.github.com/repos/agim/lidza-deploy/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: 10e9}
	res, err := client.Do(req)
	if err != nil {
		return "", errors.New("release service unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", errors.New("no published stable release available")
	}
	var data struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&data) != nil || !releaseVersion.MatchString(data.Tag) || data.Draft || data.Prerelease {
		return "", errors.New("invalid stable release")
	}
	return data.Tag, nil
}
func (m *Manager) upgradeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/upgrade", func(w http.ResponseWriter, r *http.Request) {
		status := m.upgradeStatus()
		if r.URL.Query().Get("check") == "1" {
			latest, err := LatestRelease(r)
			if err != nil {
				status.Message = err.Error()
			} else {
				status.Latest = latest
			}
		}
		JSON(w, 200, status)
	})
	mux.HandleFunc("POST /v1/upgrade", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Version string `json:"version"`
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if !releaseVersion.MatchString(in.Version) {
			Fail(w, 400, errors.New("select a published stable version"))
			return
		}
		if !m.upgradeStatus().Supported {
			Fail(w, 409, errors.New("install the managed upgrade helper on this server first"))
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.upgradePending() {
			Fail(w, 409, errors.New("upgrade already queued or running"))
			return
		}
		for _, a := range m.data.Apps {
			if a.Restoring || m.busy(a.ID) {
				Fail(w, 409, errors.New("wait for deployments and restores before upgrading"))
				return
			}
		}
		for _, d := range m.data.Databases {
			if d.Operation != "" {
				Fail(w, 409, errors.New("wait for database operations"))
				return
			}
		}
		for _, t := range m.data.Tasks {
			if t.Running && t.Mode == "schedule" {
				Fail(w, 409, errors.New("wait for scheduled commands"))
				return
			}
		}
		dir := filepath.Join(m.cfg.DataDir, "upgrade")
		if err := os.MkdirAll(dir, 0700); err != nil {
			Fail(w, 500, errors.New("cannot prepare upgrade"))
			return
		}
		if _, err := os.Stat(filepath.Join(dir, "request")); err == nil {
			Fail(w, 409, errors.New("upgrade already queued"))
			return
		}
		file, err := os.CreateTemp(dir, ".request-")
		if err != nil {
			Fail(w, 500, err)
			return
		}
		defer os.Remove(file.Name())
		_, err = io.WriteString(file, in.Version+"\n")
		if err == nil {
			err = file.Sync()
		}
		file.Close()
		if err == nil {
			err = os.Rename(file.Name(), filepath.Join(dir, "request"))
		}
		if err != nil {
			Fail(w, 500, errors.New("cannot queue upgrade"))
			return
		}
		JSON(w, 202, map[string]string{"status": "queued", "version": strings.TrimSpace(in.Version)})
	})
}

func (m *Manager) upgradePending() bool {
	status := m.upgradeStatus()
	ssh := m.sshAccess()
	swap := m.swapStatus()
	return status.State == "queued" || status.State == "upgrading" || ssh.State == "queued" || ssh.State == "applying" || swap.State == "queued" || swap.State == "applying"
}
