package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/audit"
	"github.com/agim/lidza/pkg/webhook"
)

func (c *Control) enableGitHubAppDeploy(w http.ResponseWriter, r *http.Request, a Application) {
	if a.Retiring {
		http.NotFound(w, r)
		return
	}
	if _, err := c.tokenFor(r.Context(), a); err != nil {
		agent.Fail(w, 409, errors.New("GitHub installation access is unavailable"))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.data.Apps[a.ID]
	if !ok || current.Generation != a.Generation || current.Retiring {
		agent.Fail(w, 409, errors.New("app changed during GitHub verification"))
		return
	}
	old := current
	current.AutoDeploy = true
	c.data.Apps[a.ID] = current
	if err := c.save(); err != nil {
		c.data.Apps[a.ID] = old
		agent.Fail(w, 503, errors.New("could not enable auto-deploy"))
		return
	}
	current.Secret = ""
	current.Previews.Env = nil
	agent.JSON(w, 200, current)
}
func (c *Control) migrateGitHubApp(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	cfg := c.githubApp()
	if !ok || a.Retiring || a.PreviewParent != "" || cfg == nil {
		agent.Fail(w, 409, errors.New("connect GitHub App before switching this app"))
		return
	}
	id, err := c.cfg.GitHub.RepositoryInstallation(r.Context(), cfg.App, a.Repository)
	if err != nil || cfg.Installations[id] == "" {
		agent.Fail(w, 409, errors.New("choose this repository in a verified GitHub App installation first"))
		return
	}
	a.GitHubInstallation = id
	if _, err = c.tokenFor(r.Context(), a); err != nil {
		agent.Fail(w, 409, errors.New("GitHub repository access could not be verified"))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.data.Apps[a.ID]
	if !ok || old.Generation != a.Generation || old.Retiring {
		agent.Fail(w, 409, errors.New("app changed during verification"))
		return
	}
	oldID := old.GitHubInstallation
	previousChildren := map[string]Application{}
	for id, child := range c.data.Apps {
		if child.PreviewParent == a.ID {
			previousChildren[id] = child
			child.GitHubInstallation = a.GitHubInstallation
			c.data.Apps[id] = child
		}
	}
	old.GitHubInstallation = id
	c.data.Apps[a.ID] = old
	if err = c.save(); err != nil {
		for id, child := range previousChildren {
			c.data.Apps[id] = child
		}
		old.GitHubInstallation = oldID
		c.data.Apps[a.ID] = old
		agent.Fail(w, 503, errors.New("could not save GitHub App selection"))
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "switched", "next": "The old repository hook is now ignored; remove it in GitHub settings when convenient."})
}
func (c *Control) githubAppWebhook(w http.ResponseWriter, r *http.Request) {
	cfg := c.githubApp()
	if cfg == nil {
		http.NotFound(w, r)
		return
	}
	// Framework HMAC verifies the body and uses its durable Postgres delivery
	// store. Constructing here reads freshly changed credential settings.
	endpoint := webhook.HMAC("DEPLOY_GITHUB_APP_WEBHOOK_SECRET", "X-Hub-Signature-256", func(ctx context.Context, d *webhook.Delivery) error {
		var event struct {
			Action       string `json:"action"`
			Installation struct {
				ID int64 `json:"id"`
			} `json:"installation"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if json.Unmarshal(d.Body, &event) != nil {
			return errors.New("invalid GitHub event")
		}
		kind := d.Request.Header.Get("X-GitHub-Event")
		if kind == "ping" {
			return nil
		}
		if kind == "installation" || kind == "installation_repositories" {
			ctx = audit.System(ctx, "github-app-webhook")
			if err := audit.From(ctx).Record(ctx, audit.Event{Action: "github.installation." + kind, Scope: fleetScope, Resource: "github-installation"}); err != nil {
				return err
			}
			// Repository access is checked server-side again on every mint. A deletion
			// or suspension also removes the locally authorized installation.
			if kind == "installation" && (event.Action == "deleted" || event.Action == "suspend") {
				c.githubMu.Lock()
				defer c.githubMu.Unlock()
				c.mu.Lock()
				defer c.mu.Unlock()
				if c.data.GitHubApp != nil {
					old := c.data.GitHubApp
					next := *old
					next.Installations = make(map[int64]string)
					for id, name := range old.Installations {
						if id != event.Installation.ID {
							next.Installations[id] = name
						}
					}
					c.data.GitHubApp = &next
					if err := c.save(); err != nil {
						c.data.GitHubApp = old
						return err
					}
				}
			}
			return nil
		}
		if kind != "push" && kind != "pull_request" {
			return nil
		}
		latest := c.githubApp()
		if latest == nil || latest.App.ID != cfg.App.ID || latest.Installations[event.Installation.ID] == "" {
			return nil
		}
		c.mu.Lock()
		apps := []Application{}
		for _, a := range c.data.Apps {
			if !a.Retiring && a.GitHubInstallation == event.Installation.ID && strings.EqualFold(a.Repository, event.Repository.FullName) && a.PreviewParent == "" {
				apps = append(apps, a)
			}
		}
		c.mu.Unlock()
		for _, a := range apps {
			req := d.Request.WithContext(ctx)
			rec := &auditResponse{header: make(http.Header)}
			c.enqueueGitHubEvent(rec, req, a, d.Body)
			if rec.status >= 400 {
				return errors.New("could not persist GitHub deployment event")
			}
		}
		return nil
	}, webhook.Prefix("sha256="), webhook.IDHeader("X-GitHub-Delivery"))
	endpoint.ServeHTTP(w, r)
}
