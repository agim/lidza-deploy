package control

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/jobs"
	"net/http"
	"strings"
)

func (c *Control) hook(w http.ResponseWriter, r *http.Request) {
	if a, ok := c.app(r.PathValue("id")); ok && a.GitHubInstallation > 0 {
		c.enableGitHubAppDeploy(w, r, a)
		return
	}
	agent.Fail(w, 409, errors.New("connect this repository through the GitHub App before enabling auto-deploy"))
}
func (c *Control) enqueueGitHubEvent(w http.ResponseWriter, r *http.Request, a Application, body []byte) {
	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		w.WriteHeader(204)
		return
	}
	if event == "pull_request" {
		if !a.AutoDeploy {
			var ev struct {
				Action string `json:"action"`
			}
			if json.Unmarshal(body, &ev) != nil {
				http.Error(w, "invalid payload", 400)
				return
			}
			if ev.Action != "closed" {
				w.WriteHeader(204)
				return
			}
		}
		c.enqueuePreview(w, r, a, body)
		return
	}
	if event != "push" || !a.AutoDeploy {
		w.WriteHeader(204)
		return
	}
	var push struct {
		Ref        string `json:"ref"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &push); err != nil {
		http.Error(w, "invalid payload", 400)
		return
	}
	if push.Deleted || push.Ref != "refs/heads/"+a.Branch || !strings.EqualFold(push.Repository.FullName, a.Repository) {
		w.WriteHeader(204)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery == "" || len(delivery) > 128 {
		http.Error(w, "invalid delivery ID", 400)
		return
	}
	id, err := jobs.From(r.Context()).Enqueue(r.Context(), "deploy.push", pushJob{AppID: a.ID, Delivery: delivery, Generation: a.Generation}, jobs.Unique(a.ID+":"+delivery), jobs.MaxAttempts(12))
	if err != nil {
		agent.Fail(w, 503, errors.New("could not persist delivery"))
		return
	}
	agent.JSON(w, 202, map[string]string{"id": id, "status": "pending"})
}

type pushJob struct {
	Generation string `json:"generation,omitempty"`
	AppID      string `json:"app_id"`
	Delivery   string `json:"delivery"`
}

func (c *Control) dispatchPush(ctx context.Context, payload json.RawMessage) error {
	var job pushJob
	if err := json.Unmarshal(payload, &job); err != nil {
		return err
	}
	a, ok := c.app(job.AppID)
	if !ok || a.Retiring || !a.AutoDeploy || job.Generation != a.Generation {
		return nil
	}
	credentials, err := c.deploymentCredentials(ctx, a, "github:"+job.Delivery)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.PublicURL, nil)
	if err != nil {
		return err
	}
	var d agent.Deployment
	if err := c.syncStorageIfConfigured(req, a.ServerID); err != nil {
		return err
	}
	return c.agentCall(req, a.ServerID, "POST", "/v1/apps/"+a.ID+"/deploy", credentials, &d)
}
func (c *Control) deliveries(w http.ResponseWriter, r *http.Request) {
	rows, err := jobs.From(r.Context()).Recent(r.Context(), 100)
	if err != nil {
		agent.Fail(w, 500, err)
		return
	}
	agent.JSON(w, 200, rows)
}
