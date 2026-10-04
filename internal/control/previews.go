package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/jobs"
	"net/http"
	"strings"
)

type PreviewConfig struct {
	Enabled    bool              `json:"enabled"`
	BaseDomain string            `json:"base_domain"`
	Database   bool              `json:"database"`
	Env        map[string]string `json:"env,omitempty"`
}
type previewJob struct {
	AppID      string `json:"app_id"`
	Number     int    `json:"number"`
	Action     string `json:"action"`
	Delivery   string `json:"delivery"`
	Generation string `json:"generation"`
}

func previewID(parent string, number int) string {
	if len(parent) > 24 {
		parent = parent[:24]
	}
	return fmt.Sprintf("%s-pr-%d", strings.TrimRight(parent, "-"), number)
}
func (c *Control) previewConfig(w http.ResponseWriter, r *http.Request) {
	var input PreviewConfig
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if input.Enabled {
		spec := agent.App{ID: "preview", Domain: "pr-1." + input.BaseDomain, Repository: "test/repo", Branch: "main", Env: input.Env}
		if err := spec.Validate(); err != nil {
			agent.Fail(w, 400, err)
			return
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.data.Apps[r.PathValue("id")]
	if !ok || a.Retiring || a.PreviewParent != "" {
		http.NotFound(w, r)
		return
	}
	old := a
	if input.Env == nil {
		input.Env = a.Previews.Env
	}
	a.Previews = input
	c.data.Apps[a.ID] = a
	if err := c.save(); err != nil {
		c.data.Apps[a.ID] = old
		agent.Fail(w, 500, err)
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "saved", "next": "Enable or refresh Auto-deploy to register pull-request events"})
}
func (c *Control) enqueuePreview(w http.ResponseWriter, r *http.Request, a Application, body []byte) {
	var event struct {
		Action     string `json:"action"`
		Number     int    `json:"number"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Pull struct {
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_request"`
	}
	if json.Unmarshal(body, &event) != nil {
		http.Error(w, "invalid payload", 400)
		return
	}
	if (!a.Previews.Enabled && event.Action != "closed") || !strings.EqualFold(event.Repository.FullName, a.Repository) || (event.Action != "closed" && event.Pull.Base.Ref != a.Branch) || event.Number < 1 || event.Number > 999999999 {
		w.WriteHeader(204)
		return
	}
	switch event.Action {
	case "opened", "reopened", "synchronize", "closed":
	default:
		w.WriteHeader(204)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery == "" || len(delivery) > 128 {
		http.Error(w, "invalid delivery", 400)
		return
	}
	id, err := jobs.From(r.Context()).Enqueue(r.Context(), "deploy.preview", previewJob{a.ID, event.Number, event.Action, delivery, a.Generation}, jobs.Unique("preview:"+a.ID+":"+delivery), jobs.MaxAttempts(12))
	if err != nil {
		agent.Fail(w, 503, err)
		return
	}
	agent.JSON(w, 202, map[string]string{"id": id})
}
func (c *Control) dispatchPreview(ctx context.Context, payload json.RawMessage) error {
	var in previewJob
	if err := json.Unmarshal(payload, &in); err != nil {
		return err
	}
	parent, ok := c.app(in.AppID)
	if !ok || parent.Generation != in.Generation {
		return nil
	}
	r, _ := http.NewRequestWithContext(ctx, "POST", c.cfg.PublicURL, nil)
	id := previewID(parent.ID, in.Number)
	if in.Action == "closed" {
		return c.deletePreview(r, parent.ServerID, id)
	}
	if !parent.Previews.Enabled || parent.Retiring {
		return nil
	}
	// Revalidate the current PR state so an old queued open/sync cannot resurrect a closed PR.
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	if c.cfg.GitHub == nil {
		return errors.New("GitHub unavailable")
	}
	var pr struct {
		State string `json:"state"`
		Base  struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err = c.cfg.GitHub.Request(ctx, token, "GET", fmt.Sprintf("/repos/%s/pulls/%d", parent.Repository, in.Number), nil, &pr); err != nil {
		return err
	}
	if pr.State != "open" || pr.Base.Ref != parent.Branch {
		return c.deletePreview(r, parent.ServerID, id)
	}
	c.mu.Lock()
	a, exists := c.data.Apps[id]
	if exists && a.PreviewParent != parent.ID {
		c.mu.Unlock()
		return errors.New("preview ID conflict")
	}
	if !exists {
		a = Application{ID: id, ServerID: parent.ServerID, Repository: parent.Repository, Branch: fmt.Sprintf("pr-%d", in.Number), Domain: id + "." + parent.Previews.BaseDomain, Generation: random(), PreviewParent: parent.ID}
		c.data.Apps[id] = a
		if err = c.save(); err != nil {
			delete(c.data.Apps, id)
			c.mu.Unlock()
			return err
		}
	}
	c.mu.Unlock()
	spec := agent.App{ID: id, Repository: parent.Repository, Branch: a.Branch, Domain: a.Domain, Env: parent.Previews.Env, Preview: true, PullRequest: in.Number}
	var remoteApps []agent.App
	if err = c.agentCall(r, parent.ServerID, "GET", "/v1/apps", nil, &remoteApps); err != nil {
		return err
	}
	for _, remote := range remoteApps {
		if remote.ID == id {
			if !remote.Preview {
				return errors.New("remote preview ID conflict")
			}
			spec.Env = nil
		}
	}
	if err = c.agentCall(r, parent.ServerID, "PUT", "/v1/apps/"+id, spec, nil); err != nil {
		return err
	}
	if parent.Previews.Database {
		var dbs []agent.DatabaseView
		if err = c.agentCall(r, parent.ServerID, "GET", "/v1/databases", nil, &dbs); err != nil {
			return err
		}
		found := false
		ready := false
		for _, db := range dbs {
			if db.AppID == id {
				found = true
				ready = db.Ready
				if db.Error != "" {
					return errors.New("preview database provisioning failed")
				}
			}
		}
		if !found {
			if err = c.agentCall(r, parent.ServerID, "POST", "/v1/apps/"+id+"/database", agent.DatabaseRequest{Mode: "local", ID: id, EnvKey: "DATABASE_URL", Backup: agent.BackupPolicy{Keep: 1}}, nil); err != nil {
				return err
			}
			return errors.New("preview database provisioning; retry")
		}
		if !ready {
			return errors.New("preview database not ready; retry")
		}
	}
	return c.agentCall(r, parent.ServerID, "POST", "/v1/apps/"+id+"/deploy", agent.DeployRequest{Token: token, Key: "preview:" + in.Delivery}, nil)
}
func (c *Control) deletePreview(r *http.Request, server, id string) error {
	if err := c.agentCall(r, server, "DELETE", "/v1/previews/"+id, nil, nil); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old, exists := c.data.Apps[id]
	delete(c.data.Apps, id)
	if err := c.save(); err != nil {
		if exists {
			c.data.Apps[id] = old
		}
		return err
	}
	return nil
}
