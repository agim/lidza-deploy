package agent

import (
	"errors"
	"maps"
	"strings"
)

// Shared defaults deliberately exclude credentials and host-specific attachments.
func ValidateAppDefaults(values map[string]string) error {
	for k, v := range values {
		switch k {
		case "DB_MIGRATE", "ADMIN_USERS", "AUTH_OWNER_CLAIM", "MAIL_FROM", "MAIL_PROVIDER", "LOG_LEVEL":
		default:
			return errors.New("unsupported application default: " + k)
		}
		if len(v) > 2048 || strings.ContainsAny(v, "\r\n\x00") {
			return errors.New("invalid application default: " + k)
		}
		if (k == "DB_MIGRATE" || k == "AUTH_OWNER_CLAIM") && v != "" && v != "true" && v != "false" {
			return errors.New(k + " must be true or false")
		}
	}
	return nil
}

// Called under the manager lock; the snapshot is saved together with the queue.
func (m *Manager) initialDefaults(a App, values map[string]string) App {
	if a.DefaultsApplied {
		return a
	}
	a.DefaultsApplied = true
	if a.Current != nil {
		return a
	}
	for _, d := range m.data.Deployments {
		if d.AppID == a.ID {
			return a
		}
	}
	a.Env = maps.Clone(a.Env)
	a.EnvSources = maps.Clone(a.EnvSources)
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	if a.EnvSources == nil {
		a.EnvSources = map[string]string{}
	}
	for k, v := range values {
		if v == "" {
			continue
		}
		if _, exists := a.Env[k]; exists && a.EnvSources[k] != "built-in" {
			continue
		}
		a.Env[k] = v
		a.EnvSources[k] = "workspace"
	}
	return a
}
