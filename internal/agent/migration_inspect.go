package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// InspectMigrationPlans reads a retained container agent inventory without
// starting its management runtime, listeners, jobs, maintenance or reconcilers.
// It is useful when the running source predates the migration-review endpoint.
func InspectMigrationPlans(ctx context.Context, configPath, appID string) ([]MigrationPlan, error) {
	f, err := os.Open(configPath)
	if err != nil {
		return nil, errors.New("agent configuration unavailable")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (512<<10)+1))
	if err != nil || len(data) > 512<<10 {
		return nil, errors.New("agent configuration is unavailable or oversized")
	}
	cfg := Config{Listen: "127.0.0.1:9090", ProxyListen: "127.0.0.1:8081", DataDir: "/var/lib/lidza-agent"}
	if json.Unmarshal(data, &cfg) != nil || !filepath.IsAbs(cfg.DataDir) {
		return nil, errors.New("agent configuration is invalid")
	}
	cfg.IngressOnly = true
	m, err := NewIngressReader(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer m.Close()
	plans := []MigrationPlan{}
	if appID != "" {
		plan, err := m.MigrationPlan(appID)
		if err != nil {
			return nil, err
		}
		return append(plans, plan), nil
	}
	for _, a := range m.Apps() {
		plan, err := m.MigrationPlan(a.ID)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}
