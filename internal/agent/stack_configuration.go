package agent

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// StackConfiguration declares validation ownership, never configuration values.
// Setup-owned values may live in the app database; genuine runtime readiness
// remains authoritative after setup. Undeclared settings keep framework defaults.
type StackConfiguration struct {
	Name         string                       `json:"name"`
	Stage        string                       `json:"stage"`
	Label        string                       `json:"label,omitempty"`
	Help         string                       `json:"help,omitempty"`
	Secret       bool                         `json:"secret,omitempty"`
	RequiredWhen *StackConfigurationCondition `json:"required_when,omitempty"`
}
type StackConfigurationCondition struct {
	Name   string `json:"name"`
	Equals string `json:"equals"`
}

func platformConfiguration(name string) bool {
	return strings.HasPrefix(name, "LIDZA_") || strings.HasPrefix(name, "CLICKHOUSE_") || strings.HasPrefix(name, "STORAGE_") || slices.Contains([]string{"DATABASE_URL", "CACHE_URL", "AUTH_SECRET", "APP_URL"}, name)
}
func configurationText(s string, limit int) bool {
	return len(s) <= limit && !strings.ContainsFunc(s, unicode.IsControl)
}
func (m StackManifest) validateConfiguration() error {
	if len(m.Configuration) > 40 {
		return errors.New("too many stack configuration declarations")
	}
	entries := map[string]StackConfiguration{}
	for _, c := range m.Configuration {
		if !envPattern.MatchString(c.Name) || entries[c.Name].Name != "" || !slices.Contains([]string{"deploy", "setup", "optional"}, c.Stage) || !configurationText(c.Label, 80) || !configurationText(c.Help, 300) {
			return errors.New("invalid or duplicate stack configuration declaration")
		}
		if c.Stage != "deploy" && (platformConfiguration(c.Name) || slices.Contains(m.RequiredEnvironment, c.Name) || slices.Contains(m.GeneratedSecrets, c.Name)) {
			return fmt.Errorf("%s is required infrastructure or an unconditional stack requirement; it cannot be deferred", c.Name)
		}
		if c.Stage == "setup" && m.BootstrapReadiness == nil {
			return errors.New("setup configuration requires explicit bootstrap_readiness")
		}
		if c.RequiredWhen != nil && (c.Stage != "optional" || !envPattern.MatchString(c.RequiredWhen.Name) || c.RequiredWhen.Name == c.Name || strings.TrimSpace(c.RequiredWhen.Equals) == "" || !configurationText(c.RequiredWhen.Equals, 80) || environmentKind(c.RequiredWhen.Name, "") == EnvironmentCredential) {
			return errors.New("required_when needs an optional setting and a public unconditional setting comparison")
		}
		entries[c.Name] = c
	}
	for _, c := range m.Configuration {
		if c.RequiredWhen != nil {
			other := entries[c.RequiredWhen.Name]
			if other.Secret || other.RequiredWhen != nil || other.Stage == "setup" || other.Stage == "optional" {
				return errors.New("required_when cannot depend on a secret or deferred setting")
			}
		}
	}
	return nil
}
func (c StackConfiguration) required(values map[string]string) bool {
	return c.Stage == "deploy" || c.Stage == "optional" && c.RequiredWhen != nil && values[c.RequiredWhen.Name] == c.RequiredWhen.Equals
}
func (c StackConfiguration) guidance() string {
	text := c.Name
	if c.Label != "" {
		text += " (" + c.Label + ")"
	}
	if c.Help != "" {
		text += " — " + c.Help
	}
	return text
}
