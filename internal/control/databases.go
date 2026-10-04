package control

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"io"
	"net/http"
	"regexp"
	"time"
)

type fleetDatabase struct {
	agent.DatabaseView
	ServerID string `json:"server_id"`
}

func (c *Control) databases(w http.ResponseWriter, r *http.Request) {
	list := []fleetDatabase{}
	unavailable := []string{}
	for _, result := range c.readFleet(r, "/v1/databases") {
		if result.Err != nil {
			unavailable = append(unavailable, result.Server.Name)
			continue
		}
		var rows []agent.DatabaseView
		if json.Unmarshal(result.Body, &rows) != nil {
			unavailable = append(unavailable, result.Server.Name)
			continue
		}
		for _, row := range rows {
			list = append(list, fleetDatabase{row, result.Server.ID})
		}
	}
	agent.JSON(w, 200, map[string]any{"databases": list, "unavailable_servers": unavailable})
}
func (c *Control) configureDatabase(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var input agent.DatabaseRequest
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if err := input.Validate(); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if err := c.syncBackupStorage(r, a.ServerID); err != nil && input.Backup.Offsite {
		agent.Fail(w, 409, err)
		return
	}
	if err := c.agentCall(r, a.ServerID, "POST", "/v1/apps/"+a.ID+"/database", input, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 202, map[string]string{"status": "provisioning"})
}
func (c *Control) databaseAction(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	action := r.PathValue("action")
	if action != "backup" && action != "provision" {
		http.NotFound(w, r)
		return
	}
	if action == "backup" {
		c.mu.Lock()
		configured := c.data.BackupStorage != nil
		c.mu.Unlock()
		if configured {
			if err := c.syncBackupStorage(r, a.ServerID); err != nil {
				agent.Fail(w, 502, err)
				return
			}
		}
	}
	if err := c.agentCall(r, a.ServerID, "POST", "/v1/apps/"+a.ID+"/database/"+action, nil, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 202, map[string]string{"status": "queued"})
}
func (c *Control) backupPolicy(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var p agent.BackupPolicy
	if err := agent.Decode(w, r, &p); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if err := c.syncBackupStorage(r, a.ServerID); err != nil && p.Offsite {
		agent.Fail(w, 409, err)
		return
	}
	if err := c.agentCall(r, a.ServerID, "PATCH", "/v1/apps/"+a.ID+"/backups", p, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "saved"})
}
func (c *Control) syncBackupStorage(r *http.Request, server string) error {
	c.mu.Lock()
	cfg := c.data.BackupStorage
	c.mu.Unlock()
	if cfg == nil {
		return errors.New("configure off-site storage in Integrations first")
	}
	return c.agentCall(r, server, "PUT", "/v1/backup-storage", cfg, nil)
}
func (c *Control) downloadBackup(w http.ResponseWriter, r *http.Request) {
	// Explicit server + app allows retrieval of retained databases after app removal.
	server, id, key := r.PathValue("server"), r.PathValue("id"), r.PathValue("backup")
	if !regexp.MustCompile(`^[a-f0-9]{24}$`).MatchString(key) || !regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`).MatchString(id) {
		http.NotFound(w, r)
		return
	}
	var target *Server
	for _, s := range c.servers() {
		if s.ID == server {
			target = &s
			break
		}
	}
	if target == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "GET", target.URL+"/v1/apps/"+id+"/backups/"+key, nil)
	if err != nil {
		agent.Fail(w, 400, errors.New("invalid download request"))
		return
	}
	request.Header.Set("Authorization", "Bearer "+target.Token)
	client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(request)
	if err != nil {
		agent.Fail(w, 502, errors.New("backup download unavailable"))
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		agent.Fail(w, 502, errors.New("backup file unavailable"))
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Minute))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", res.Header.Get("Content-Disposition"))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, res.Body)
}

func (c *Control) reloadApp(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	if err := c.syncStorageIfConfigured(r, a.ServerID); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	var out agent.Deployment
	if err := c.agentCall(r, a.ServerID, "POST", "/v1/apps/"+a.ID+"/reload", nil, &out); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 202, out)
}
func (c *Control) databaseResourceAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	id := r.PathValue("id")
	server := r.PathValue("server")
	if (action != "backup" && action != "provision") || !regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`).MatchString(id) {
		http.NotFound(w, r)
		return
	}
	if action == "backup" {
		c.mu.Lock()
		configured := c.data.BackupStorage != nil
		c.mu.Unlock()
		if configured {
			if err := c.syncBackupStorage(r, server); err != nil {
				agent.Fail(w, 502, err)
				return
			}
		}
	}
	if err := c.agentCall(r, server, "POST", "/v1/databases/"+id+"/"+action, nil, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 202, map[string]string{"status": "queued"})
}
func (c *Control) databaseResourcePolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	server := r.PathValue("server")
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`).MatchString(id) {
		http.NotFound(w, r)
		return
	}
	var p agent.BackupPolicy
	if err := agent.Decode(w, r, &p); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if p.Offsite {
		if err := c.syncBackupStorage(r, server); err != nil {
			agent.Fail(w, 409, err)
			return
		}
	}
	if err := c.agentCall(r, server, "PATCH", "/v1/databases/"+id+"/backups", p, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "saved"})
}

func (c *Control) syncStorageIfConfigured(r *http.Request, server string) error {
	c.mu.Lock()
	configured := c.data.BackupStorage != nil
	c.mu.Unlock()
	if !configured {
		return nil
	}
	return c.syncBackupStorage(r, server)
}
