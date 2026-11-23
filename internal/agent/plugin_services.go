package agent

// Plugin services for applications (stage 2 of docs/plugins-proposal.md): an
// application asks for a service by name; the plugin that provides it
// provisions it and answers the environment the application gets, a record
// the agent keeps for it and secrets the agent seals. The agent knows the
// service only by its name.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/agim/lidza-deploy/internal/plugin"
)

// NativePluginService asks for a service a plugin provides.
type NativePluginService struct {
	// Service is the manifest's "provides" name, such as "clickhouse".
	Service string `json:"service"`
	// Options are the plugin's own settings for this application.
	Options json.RawMessage `json:"options,omitempty"`
}

// NativeHostPlugin is what the agent keeps about a plugin service of one
// application.
type NativeHostPlugin struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	// Record is the plugin's own description of what it provisioned
	// (a database name, a port); it is handed back on every later hook.
	Record json.RawMessage `json:"record,omitempty"`
	// Secrets is the plugin's secrets for this application, sealed by the agent.
	Secrets string `json:"secrets_sealed,omitempty"`
	// Env are the variables it set for the application.
	Env []string `json:"env,omitempty"`
	// Resource is the protected resource its backups are kept under, when
	// the plugin can back the service up.
	Resource string `json:"resource,omitempty"`
}

const (
	maxPluginServices = 4
	maxPluginRecord   = 64 << 10
	maxPluginSecrets  = 16
)

var pluginServiceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// pluginAppInput is the input of the hooks that act for one application.
type pluginAppInput struct {
	App     string            `json:"app"`
	Account pluginAppAccount  `json:"account"`
	Options json.RawMessage   `json:"options,omitempty"`
	Record  json.RawMessage   `json:"record,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	// Sizes are the bytes that will arrive, for check.
	Sizes map[string]int64 `json:"sizes,omitempty"`
}

type pluginAppAccount struct {
	User string `json:"user"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	Root string `json:"root"`
}

type pluginProvisionOutput struct {
	Env     map[string]string `json:"env"`
	Record  json.RawMessage   `json:"record"`
	Secrets map[string]string `json:"secrets"`
}

func validateNativePluginServices(services []NativePluginService) error {
	if len(services) > maxPluginServices {
		return fmt.Errorf("at most %d plugin services per application", maxPluginServices)
	}
	seen := map[string]bool{}
	for _, s := range services {
		if !pluginServiceName.MatchString(s.Service) || seen[s.Service] {
			return errors.New("plugin services need distinct lowercase names")
		}
		if len(s.Options) > maxPluginRecord || len(s.Options) > 0 && !json.Valid(s.Options) {
			return fmt.Errorf("the options of the %s service are not valid", s.Service)
		}
		seen[s.Service] = true
	}
	return nil
}

// pluginProviding finds the installed plugin that provides a service.
func (m *Manager) pluginProviding(service string) (PluginRecord, plugin.Manifest, error) {
	m.mu.Lock()
	var found *PluginRecord
	for _, record := range m.data.Plugins {
		if record.Provides == service {
			copy := record
			found = &copy
		}
	}
	m.mu.Unlock()
	if found == nil {
		return PluginRecord{}, plugin.Manifest{}, fmt.Errorf("this server has no plugin that provides %q; install it on the server first", service)
	}
	manifest, err := plugin.ParseManifest(found.Manifest)
	return *found, manifest, err
}

// pluginChecks reports whether an installed plugin provides a service and
// can check the server for it: that plugin, not the agent, then answers what
// the service needs.
func (m *Manager) pluginChecks(service string) bool {
	_, manifest, err := m.pluginProviding(service)
	return err == nil && manifest.Implements("check")
}

// pluginRepairs reports the same for repairs.
func (m *Manager) pluginRepairs(service string) bool {
	_, manifest, err := m.pluginProviding(service)
	return err == nil && manifest.Implements("check") && manifest.Implements("repair")
}

func (m *Manager) pluginSecrets(sealed string) (map[string]string, error) {
	if sealed == "" {
		return nil, nil
	}
	raw, err := m.unsealHostSecret(sealed)
	if err != nil {
		return nil, err
	}
	var secrets map[string]string
	return secrets, json.Unmarshal([]byte(raw), &secrets)
}

// provisionPluginService runs the plugin's provision hook for an application
// and records what it answers. Only variables the signed manifest declares
// reach the application, and none that another service already set.
func (m *Manager) provisionPluginService(ctx context.Context, id string, account NativeAppAccount, service NativePluginService, taken map[string]string, backup BackupPolicy) (map[string]string, string, error) {
	record, manifest, err := m.pluginProviding(service.Service)
	if err != nil {
		return nil, "", err
	}
	if !manifest.Implements("provision") || !manifest.Implements("release") {
		return nil, "", fmt.Errorf("the %s plugin cannot provision a service for an application", record.Name)
	}
	resource := ""
	input := pluginAppInput{App: id, Account: pluginAppAccount{User: account.User, UID: account.UID, GID: account.GID, Root: account.Root}, Options: service.Options}
	// A retry hands back what an earlier attempt recorded.
	if existing, ok := m.hostDataRecord(id); ok {
		if previous, ok := existing.Plugins[service.Service]; ok {
			input.Record, resource = previous.Record, previous.Resource
			if input.Secrets, err = m.pluginSecrets(previous.Secrets); err != nil {
				return nil, "", err
			}
		}
	}
	raw, _ := json.Marshal(input)
	result, err := m.runPluginHook(ctx, record.Name, "provision", raw)
	if err != nil {
		return nil, "", err
	}
	if !result.Answer.OK {
		return nil, "", fmt.Errorf("the %s plugin could not provision the service: %s", record.Name, cmpMessages(result.Answer.Messages))
	}
	var out pluginProvisionOutput
	if err := json.Unmarshal(result.Answer.Output, &out); err != nil {
		return nil, "", fmt.Errorf("the %s plugin's provision answer is not understood", record.Name)
	}
	if len(out.Record) > maxPluginRecord || len(out.Secrets) > maxPluginSecrets {
		return nil, "", fmt.Errorf("the %s plugin answered more than the agent keeps for a service", record.Name)
	}
	for key, value := range out.Env {
		if !slices.Contains(manifest.Environment, key) {
			return nil, "", fmt.Errorf("the %s plugin set %s, which its manifest does not declare", record.Name, key)
		}
		if _, conflict := taken[key]; conflict {
			return nil, "", fmt.Errorf("the %s plugin set %s, which another service of this application already sets", record.Name, key)
		}
		if _, err := nativeReleaseEnvLine(key, value); err != nil {
			return nil, "", fmt.Errorf("the %s plugin set a value the environment file cannot hold: %v", record.Name, err)
		}
	}
	sealed := ""
	if len(out.Secrets) > 0 {
		secrets, _ := json.Marshal(out.Secrets)
		if sealed, err = m.sealHostSecret(string(secrets)); err != nil {
			return nil, "", err
		}
	}
	keys := slices.Sorted(maps.Keys(out.Env))
	if err := m.updateHostDataRecord(id, func(r *NativeHostDataRecord) {
		if r.Plugins == nil {
			r.Plugins = map[string]NativeHostPlugin{}
		}
		r.Plugins[service.Service] = NativeHostPlugin{Plugin: record.Name, Version: record.Version, Record: out.Record, Secrets: sealed, Env: keys, Resource: resource}
	}); err != nil {
		return nil, "", err
	}
	if !manifest.Implements("backup") {
		return out.Env, "", nil
	}
	if resource, err = m.protectPluginService(id, service.Service, backup); err != nil {
		return nil, "", err
	}
	return out.Env, resource, nil
}

// releasePluginServices runs the release hook of every plugin service the
// application has and forgets each one that was released. A service with
// backups gets a final one first (unless the application was never
// released); its backups stay.
func (m *Manager) releasePluginServices(ctx context.Context, id string, record NativeHostDataRecord, finalBackup bool) error {
	for _, service := range slices.Sorted(maps.Keys(record.Plugins)) {
		entry := record.Plugins[service]
		if entry.Resource != "" {
			m.mu.Lock()
			d, exists := m.data.Databases[entry.Resource]
			binding, bound := m.data.NativeDatabases[entry.Resource]
			m.mu.Unlock()
			if finalBackup && exists && bound && binding.Policy.AppID == id && d.Ready && !m.nativeCreatedNeverReleased(id) {
				if err := m.freshBackupBeforeRelease(ctx, []string{entry.Resource}); err != nil {
					return fmt.Errorf("the final backup of the %s service failed; the service is retained: %w", service, err)
				}
			}
			if err := m.unbindHostDatabase(id, entry.Resource); err != nil {
				return err
			}
		}
		secrets, err := m.pluginSecrets(entry.Secrets)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(pluginAppInput{App: id, Record: entry.Record, Secrets: secrets})
		result, err := m.runPluginHook(ctx, entry.Plugin, "release", raw)
		if err != nil {
			return fmt.Errorf("the %s service could not be released: %w", service, err)
		}
		if !result.Answer.OK {
			return fmt.Errorf("the %s plugin could not release the service: %s", entry.Plugin, cmpMessages(result.Answer.Messages))
		}
		if err := m.updateHostDataRecord(id, func(r *NativeHostDataRecord) { delete(r.Plugins, service) }); err != nil {
			return err
		}
	}
	return nil
}

// pluginServiceChecks rehearses the plugin services of an application: a
// missing plugin blocks; otherwise the plugin's own check hook answers.
func (m *Manager) pluginServiceChecks(ctx context.Context, id string, services []NativePluginService, sizes map[string]int64) []HostReadinessCheck {
	checks := []HostReadinessCheck{}
	for _, service := range services {
		record, manifest, err := m.pluginProviding(service.Service)
		if err != nil {
			checks = append(checks, HostReadinessCheck{ID: "plugin-" + service.Service, Title: service.Service, Detail: strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + "."})
			continue
		}
		if !manifest.Implements("check") {
			checks = append(checks, HostReadinessCheck{ID: "plugin-" + service.Service, Title: service.Service, OK: true, Detail: "Provided by the " + record.Name + " plugin, version " + record.Version + "."})
			continue
		}
		raw, _ := json.Marshal(pluginAppInput{App: id, Options: service.Options, Sizes: sizes})
		result, err := m.runPluginHook(ctx, record.Name, "check", raw)
		if err != nil {
			checks = append(checks, HostReadinessCheck{ID: "plugin-" + service.Service, Title: service.Service, Detail: "The " + record.Name + " plugin could not check this server: " + err.Error() + "."})
			continue
		}
		if len(result.Answer.Checks) == 0 {
			checks = append(checks, HostReadinessCheck{ID: "plugin-" + service.Service, Title: service.Service, OK: result.Answer.OK, Detail: cmpMessages(result.Answer.Messages)})
		}
		for _, c := range result.Answer.Checks {
			checks = append(checks, HostReadinessCheck{ID: "plugin-" + service.Service + "-" + c.ID, Title: c.Title, OK: c.OK, Detail: c.Detail, Repairable: c.Repairable, Caution: c.Caution})
		}
	}
	return checks
}

func cmpMessages(messages []string) string {
	if len(messages) == 0 {
		return "it gave no reason"
	}
	return strings.Join(messages, " ")
}
