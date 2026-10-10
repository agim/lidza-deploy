package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	gh "github.com/agim/lidza-deploy/internal/providers/github"
	"github.com/agim/lidza/packs/audit"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/pkg/credentials"
)

type githubAppConfig struct {
	App           gh.App           `json:"app"`
	Installations map[int64]string `json:"installations"`
}

func (c *Control) githubApp() *githubAppConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.GitHubApp == nil {
		return nil
	}
	a := *c.data.GitHubApp
	a.Installations = make(map[int64]string)
	for id, name := range c.data.GitHubApp.Installations {
		a.Installations[id] = name
	}
	return &a
}
func (c *Control) githubOwner(r *http.Request) bool {
	return auth.CurrentUser(r.Context()).ID == c.operatorID
}
func (c *Control) githubAppRoute(w http.ResponseWriter, r *http.Request) {
	if !c.githubOwner(r) {
		agent.Fail(w, 403, errors.New("only the installation owner can connect GitHub"))
		return
	}
	path := r.URL.Path
	ctx := r.Context()
	c.githubMu.Lock()
	defer c.githubMu.Unlock()
	a := c.githubApp()
	switch {
	case r.Method == "GET" && strings.HasSuffix(path, "/status"):
		out := map[string]any{"configured": a != nil, "installations": map[int64]string{}}
		if a != nil {
			out["slug"] = a.App.Slug
			out["installations"] = a.Installations
		}
		agent.JSON(w, 200, out)
	case r.Method == "POST" && strings.HasSuffix(path, "/register"):
		if a != nil {
			agent.Fail(w, 409, errors.New("GitHub App already registered; choose repositories or disconnect it first"))
			return
		}
		if !strings.HasPrefix(c.cfg.PublicURL, "https://") {
			agent.Fail(w, 409, errors.New("GitHub registration needs the public HTTPS address chosen at installation"))
			return
		}
		var input struct {
			Organization string `json:"organization"`
		}
		if err := agent.Decode(w, r, &input); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		if input.Organization != "" && !regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`).MatchString(input.Organization) {
			agent.Fail(w, 400, errors.New("enter a valid GitHub organization name"))
			return
		}
		state, err := auth.From(ctx).IssueToken(ctx, "deploy.github.manifest", c.operatorID+" "+input.Organization, 10*time.Minute)
		if err != nil {
			agent.Fail(w, 503, errors.New("could not start GitHub registration"))
			return
		}
		origin := strings.TrimSuffix(c.cfg.PublicURL, "/")
		manifest := map[string]any{"name": fmt.Sprintf("Lidza Deploy %x", sha256.Sum256([]byte(origin)))[:25], "url": origin, "public": false, "redirect_url": origin + "/api/control/github/app/manifest-callback", "setup_url": origin + "/api/control/github/app/install-callback", "hook_attributes": map[string]any{"url": origin + "/hooks/github-app", "active": true}, "default_permissions": map[string]string{"contents": "read", "pull_requests": "read"}, "default_events": []string{"push", "pull_request"}}
		action := "https://github.com/settings/apps/new?state=" + url.QueryEscape(state)
		if input.Organization != "" {
			action = "https://github.com/organizations/" + input.Organization + "/settings/apps/new?state=" + url.QueryEscape(state)
		}
		agent.JSON(w, 200, map[string]any{"action": action, "manifest": manifest})
	case r.Method == "GET" && strings.HasSuffix(path, "/manifest-callback"):
		owner, err := auth.From(ctx).ConsumeToken(ctx, "deploy.github.manifest", r.URL.Query().Get("state"))
		if err != nil || !strings.HasPrefix(owner, c.operatorID+" ") || a != nil {
			agent.Fail(w, 400, errors.New("GitHub registration expired or was already used; restart Connect GitHub"))
			return
		}
		converted, err := c.cfg.GitHub.ConvertManifest(ctx, r.URL.Query().Get("code"))
		if err != nil {
			agent.Fail(w, 502, errors.New("GitHub App registration could not be completed; retry registration"))
			return
		}
		target := strings.TrimPrefix(owner, c.operatorID+" ")
		if target != "" {
			jwt, _ := converted.JWT()
			var info struct {
				Owner struct {
					Login string `json:"login"`
				} `json:"owner"`
			}
			if e := c.cfg.GitHub.Request(ctx, jwt, "GET", "/app", nil, &info); e != nil || !strings.EqualFold(info.Owner.Login, target) {
				agent.Fail(w, 403, errors.New("GitHub App organization does not match registration"))
				return
			}
		}
		if err = audit.From(ctx).Record(ctx, audit.Event{Action: "github.app.register", Scope: fleetScope, Resource: "github-app"}); err != nil {
			agent.Fail(w, 503, errors.New("audit unavailable; registration not saved"))
			return
		}
		// Reuse framework atomic sealed credentials for webhook verification. The
		// app credential set itself is in the existing framework-sealed product state.
		if err = credentials.Set(".", map[string]string{"DEPLOY_GITHUB_APP_WEBHOOK_SECRET": converted.WebhookSecret}); err != nil {
			agent.Fail(w, 503, errors.New("could not save encrypted webhook configuration"))
			return
		}
		c.mu.Lock()
		c.data.GitHubApp = &githubAppConfig{App: converted, Installations: map[int64]string{}}
		err = c.save()
		if err != nil {
			c.data.GitHubApp = nil
		}
		c.mu.Unlock()
		if err != nil {
			agent.Fail(w, 503, errors.New("could not save GitHub App; registration must be retried"))
			return
		}
		http.Redirect(w, r, "/console.html?github=registered", http.StatusSeeOther)
	case r.Method == "POST" && strings.HasSuffix(path, "/install"):
		if a == nil {
			agent.Fail(w, 409, errors.New("connect GitHub first"))
			return
		}
		state, err := auth.From(ctx).IssueToken(ctx, "deploy.github.install", fmt.Sprintf("%s:%d", c.operatorID, a.App.ID), 10*time.Minute)
		if err != nil {
			agent.Fail(w, 503, errors.New("could not start repository selection"))
			return
		}
		agent.JSON(w, 200, map[string]string{"url": "https://github.com/apps/" + a.App.Slug + "/installations/new?state=" + url.QueryEscape(state)})
	case r.Method == "GET" && strings.HasSuffix(path, "/install-callback"):
		owner, err := auth.From(ctx).ConsumeToken(ctx, "deploy.github.install", r.URL.Query().Get("state"))
		if a == nil || err != nil || owner != fmt.Sprintf("%s:%d", c.operatorID, a.App.ID) {
			agent.Fail(w, 400, errors.New("repository selection expired or was already used; retry Choose repositories"))
			return
		}
		id, _ := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
		if id <= 0 {
			http.Redirect(w, r, "/console.html?github=pending", http.StatusSeeOther)
			return
		}
		in, err := c.cfg.GitHub.Installation(ctx, a.App, id)
		if err != nil {
			agent.Fail(w, 409, errors.New("installation could not be verified; check GitHub approval and permissions, then retry"))
			return
		}
		if err = audit.From(ctx).Record(ctx, audit.Event{Action: "github.app.install", Scope: fleetScope, Resource: "github-installation", Meta: map[string]string{"installation": strconv.FormatInt(id, 10)}}); err != nil {
			agent.Fail(w, 503, errors.New("audit unavailable; installation not saved"))
			return
		}
		a.Installations[id] = in.Account.Login
		c.mu.Lock()
		old := c.data.GitHubApp
		c.data.GitHubApp = a
		err = c.save()
		if err != nil {
			c.data.GitHubApp = old
		}
		c.mu.Unlock()
		if err != nil {
			agent.Fail(w, 503, errors.New("could not save installation"))
			return
		}
		http.Redirect(w, r, "/console.html?github=connected", http.StatusSeeOther)
	case r.Method == "DELETE" && strings.HasSuffix(path, "/app"):
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, app := range c.data.Apps {
			if app.GitHubInstallation > 0 {
				agent.Fail(w, 409, errors.New("remove GitHub App applications before disconnecting; GitHub access can also be suspended in GitHub settings"))
				return
			}
		}
		old := c.data.GitHubApp
		c.data.GitHubApp = nil
		if err := c.save(); err != nil {
			c.data.GitHubApp = old
			agent.Fail(w, 503, errors.New("could not disconnect GitHub"))
			return
		}
		agent.JSON(w, 200, map[string]string{"status": "disconnected", "next": "Delete or uninstall the old app in GitHub settings to revoke its GitHub access."})
	default:
		http.NotFound(w, r)
	}
}
func (c *Control) tokenFor(ctx context.Context, a Application) (string, error) {
	if a.GitHubInstallation == 0 {
		return c.token(ctx)
	}
	cfg := c.githubApp()
	if cfg == nil || cfg.Installations[a.GitHubInstallation] == "" {
		return "", errors.New("GitHub installation is disconnected")
	}
	return c.cfg.GitHub.InstallationToken(ctx, cfg.App, a.GitHubInstallation, a.Repository)
}
func (c *Control) deploymentCredentials(ctx context.Context, a Application, key string) (out agent.DeployRequest, err error) {
	defer func() {
		if err == nil {
			out.Defaults = c.applicationDefaultsSnapshot()
		}
	}()
	if a.GitHubInstallation == 0 {
		token, err := c.token(ctx)
		return agent.DeployRequest{Token: token, Key: key}, err
	}
	// Validate access now for feedback, but mint checkout credentials after the
	// agent's queue/backup wait. The one-use ticket carries no GitHub credentials.
	if _, err := c.tokenFor(ctx, a); err != nil {
		return agent.DeployRequest{}, err
	}
	subject, _ := json.Marshal(checkoutClaim{AppID: a.ID, Generation: a.Generation, ServerID: a.ServerID, Repository: a.Repository, Installation: a.GitHubInstallation})
	ticket, err := auth.From(ctx).IssueToken(ctx, "deploy.github.checkout", string(subject)+":"+random(), 24*time.Hour)
	return agent.DeployRequest{Key: key, CredentialURL: strings.TrimSuffix(c.cfg.PublicURL, "/") + "/api/agent/checkout-token", CredentialTicket: ticket, CredentialServer: a.ServerID}, err
}

type checkoutClaim struct {
	AppID, Generation, ServerID, Repository string
	Installation                            int64
}

func (c *Control) checkoutToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	server := r.Header.Get("X-Lidza-Server")
	valid := false
	for _, s := range c.servers() {
		if s.ID == server {
			want := sha256.Sum256([]byte(s.Token))
			got := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			valid = strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && subtle.ConstantTimeCompare(want[:], got[:]) == 1
		}
	}
	if !valid {
		agent.Fail(w, 401, errors.New("agent authentication required"))
		return
	}
	var input struct {
		Ticket string `json:"ticket"`
	}
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	subject, err := auth.From(r.Context()).ConsumeToken(r.Context(), "deploy.github.checkout", input.Ticket)
	var claim checkoutClaim
	split := strings.LastIndex(subject, ":")
	if err != nil || split < 0 || json.Unmarshal([]byte(subject[:split]), &claim) != nil {
		agent.Fail(w, 401, errors.New("checkout authorization expired or already used"))
		return
	}
	a, ok := c.app(claim.AppID)
	if !ok || a.Retiring || a.Generation != claim.Generation || a.ServerID != server || claim.ServerID != server || a.Repository != claim.Repository || a.GitHubInstallation != claim.Installation {
		agent.Fail(w, 403, errors.New("checkout authorization no longer matches the assigned app"))
		return
	}
	ctx := audit.System(r.Context(), "agent-checkout")
	if err = audit.From(ctx).Record(ctx, audit.Event{Action: "github.checkout", Scope: fleetScope, Resource: "app/" + a.ID}); err != nil {
		agent.Fail(w, 503, errors.New("audit unavailable; checkout refused"))
		return
	}
	token, err := c.tokenFor(ctx, a)
	if err != nil {
		agent.Fail(w, 409, errors.New("GitHub access unavailable; verify installation and repository access"))
		return
	}
	agent.JSON(w, 200, map[string]string{"token": token})
}
