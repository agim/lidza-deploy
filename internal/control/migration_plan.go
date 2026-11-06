package control

import (
	"errors"
	"net/http"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

func (c *Control) migrationPlan(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var plan agent.MigrationPlan
	err := c.agentCall(r, a.ServerID, http.MethodGet, "/v1/apps/"+a.ID+"/migration-plan", nil, &plan)
	var responseError *agentHTTPError
	if errors.As(err, &responseError) && responseError.status == http.StatusNotFound && responseError.message == "" {
		plan, err = c.legacyMigrationPlan(r, a)
	}
	if err != nil {
		failAgent(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, plan)
}

// Older agents expose read-only application inventory without a migration route.
// Only report facts that can be verified from that inventory.
func (c *Control) legacyMigrationPlan(r *http.Request, app Application) (agent.MigrationPlan, error) {
	var inventory []agent.App
	if err := c.agentCall(r, app.ServerID, http.MethodGet, "/v1/apps", nil, &inventory); err != nil {
		return agent.MigrationPlan{}, err
	}
	for _, source := range inventory {
		if source.ID != app.ID || source.Retiring {
			continue
		}
		plan := agent.MigrationPlan{
			Version: 1, InventoryLimited: true, AppID: source.ID, Checked: time.Now().UTC(),
			Domains: source.Hostnames(), EnvironmentKeys: []string{}, Databases: []agent.MigrationDatabase{}, Tasks: []string{},
			Blockers: []string{"The source agent provides application inventory only. Verify its databases, persistent files, cache, workers and schedules before moving."},
			Steps: []string{
				"Review source dependencies and verify usable backups.",
				"Verify destination capacity and prepare it without routing traffic or starting workers.",
				"Plan the final data transfer, traffic switch and rollback after verifying the source dependencies.",
			},
		}
		if source.Current != nil {
			plan.Commit = source.Current.Commit
		}
		return plan, nil
	}
	return agent.MigrationPlan{}, errors.New("application is no longer in the source server inventory")
}
