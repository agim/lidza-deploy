package control

import (
	"context"
	"errors"
)

// Adopted applications can predate GitHub App pairing. Resolve an already
// authorized repository automatically while retaining working OAuth access,
// app identity, deployment preferences and hooks. This grants no new permission.
func (c *Control) resolveApplicationInstallation(ctx context.Context, a Application) (Application, error) {
	if a.GitHubInstallation != 0 || c.cfg.GitHub == nil {
		return a, nil
	}
	cfg := c.githubApp()
	if cfg == nil {
		return a, nil
	}
	if token, err := c.token(ctx); err != nil || token != "" {
		return a, err
	}
	id, err := c.cfg.GitHub.RepositoryInstallation(ctx, cfg.App, a.Repository)
	if err != nil || cfg.Installations[id] == "" {
		return a, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.GitHubApp == nil || c.data.GitHubApp.App != cfg.App || c.data.GitHubApp.Installations[id] != cfg.Installations[id] {
		return a, errors.New("repository authorization changed; retry deployment")
	}
	current, exists := c.data.Apps[a.ID]
	if !exists || current.Generation != a.Generation || current.Repository != a.Repository || current.ServerID != a.ServerID || current.Retiring {
		return a, errors.New("application changed while resolving repository access; retry deployment")
	}
	if current.GitHubInstallation != 0 {
		return current, nil
	}
	next := current
	next.GitHubInstallation = id
	c.data.Apps[a.ID] = next
	if err := c.save(); err != nil {
		c.data.Apps[a.ID] = current
		return a, errors.New("could not retain the authorized repository pairing")
	}
	return next, nil
}
