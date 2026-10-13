package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"
)

// ClickHouseResource is kept only in encrypted agent state. Deployment owns
// the service lifecycle; the application owns its schema and migrations.
type ClickHouseResource struct {
	Network  string `json:"network"`
	Password string `json:"password"`
	Ready    bool   `json:"ready"`
}

func (c ClickHouseResource) env() map[string]string {
	return map[string]string{"CLICKHOUSE_URL": "http://" + c.Network + ":8123", "CLICKHOUSE_DATABASE": "telemetry", "CLICKHOUSE_USER": "lidza", "CLICKHOUSE_PASSWORD": c.Password}
}

func (m *Manager) prepareClickHouse(ctx context.Context, a App, effective map[string]string) (App, error) {
	flag := strings.TrimSpace(effective["CLICKHOUSE_MANAGED"])
	if flag != "" && flag != "true" && flag != "false" {
		return a, errors.New("CLICKHOUSE_MANAGED must be true or false")
	}
	if flag != "true" {
		return a, nil
	}
	m.mu.Lock()
	c := m.data.ClickHouses[a.ID]
	m.mu.Unlock()
	// An explicit external URL always wins, including sealed app credentials.
	if u := strings.TrimSpace(effective["CLICKHOUSE_URL"]); u != "" && (c.Network == "" || u != c.env()["CLICKHOUSE_URL"]) {
		return a, nil
	}
	if c.Network == "" {
		for _, key := range []string{"CLICKHOUSE_DATABASE", "CLICKHOUSE_USER", "CLICKHOUSE_PASSWORD"} {
			if effective[key] != "" {
				return a, errors.New("partial ClickHouse connection settings: supply CLICKHOUSE_URL for an existing service or remove the partial settings for automatic provisioning")
			}
		}
	}
	_, err, _ := m.clickhouseFlights.Do(a.ID, func() (any, error) {
		m.mu.Lock()
		original, ok := m.data.Apps[a.ID]
		if !ok || original.Retiring {
			m.mu.Unlock()
			return nil, errors.New("application unavailable")
		}
		c := m.data.ClickHouses[a.ID]
		if c.Network == "" {
			password := make([]byte, 32)
			if _, err := rand.Read(password); err != nil {
				m.mu.Unlock()
				return nil, err
			}
			c = ClickHouseResource{Network: "lidza-clickhouse-" + original.resourceID(), Password: hex.EncodeToString(password)}
			m.data.ClickHouses[a.ID] = c
			if err := m.save(); err != nil {
				delete(m.data.ClickHouses, a.ID)
				m.mu.Unlock()
				return nil, errors.New("could not save ClickHouse credentials")
			}
		}
		m.mu.Unlock()
		commandDiagnostic(ctx, "clickhouse", "Preparing private authenticated ClickHouse; persistent data and credentials are retained across releases.")
		probe, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		var err error
		if m.clickhouseProvision != nil {
			err = m.clickhouseProvision(probe, c)
		} else {
			err = provisionLocalClickHouse(probe, c)
		}
		if err != nil {
			commandDiagnostic(ctx, "clickhouse", publicClickHouseFailure(err).Error())
			return nil, publicClickHouseFailure(err)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		original = m.data.Apps[a.ID]
		if original.ID == "" || original.Retiring {
			return nil, errors.New("application unavailable")
		}
		updated := original
		updated.Env = maps.Clone(original.Env)
		if updated.Env == nil {
			updated.Env = map[string]string{}
		}
		maps.Copy(updated.Env, c.env())
		if err := updated.Validate(); err != nil {
			return nil, err
		}
		old := m.data.ClickHouses[a.ID]
		c.Ready = true
		m.data.ClickHouses[a.ID] = c
		m.data.Apps[a.ID] = updated
		if err := m.save(); err != nil {
			m.data.ClickHouses[a.ID] = old
			m.data.Apps[a.ID] = original
			return nil, errors.New("could not save ClickHouse attachment; retry deployment")
		}
		return nil, nil
	})
	if err != nil {
		return a, err
	}
	m.mu.Lock()
	c = m.data.ClickHouses[a.ID]
	a.Env = maps.Clone(m.data.Apps[a.ID].Env)
	m.mu.Unlock()
	maps.Copy(effective, c.env())
	a.Networks = slices.Compact(slices.Sorted(slices.Values(append(a.Networks, c.Network))))
	commandDiagnostic(ctx, "clickhouse", "ClickHouse is ready; private connection and network attached. The app applies its schema before readiness.")
	return a, nil
}
