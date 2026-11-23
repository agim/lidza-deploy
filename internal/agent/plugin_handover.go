package agent

// Handing the host's ClickHouse to its plugin (docs/plugins-proposal.md,
// stage 3). The ClickHouse the agent's built-in support installed keeps
// running exactly as it is: the plugin is told about the server and each
// application's database, proves the credentials and records them. The
// agent then keeps each database as that plugin's service instead of its
// own. No configuration is written, no user is altered and nothing
// restarts; an application's environment does not change.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
)

const clickhousePluginService = "clickhouse"

// ClickHouseHandOver describes what a hand-over would do, or did.
type ClickHouseHandOver struct {
	// Available says the hand-over can run now; Reason says why not.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	// BuiltIn says the built-in support runs a ClickHouse on this server.
	BuiltIn bool   `json:"built_in"`
	Version string `json:"version,omitempty"`
	Plugin  string `json:"plugin,omitempty"`
	// PluginVersion is the plugin that would take it over.
	PluginVersion string                  `json:"plugin_version,omitempty"`
	Applications  []ClickHouseHandOverApp `json:"applications"`
	// Done lists what a finished hand-over changed.
	Done []string `json:"done,omitempty"`
}

type ClickHouseHandOverApp struct {
	App      string `json:"app"`
	Database string `json:"database"`
	Resource string `json:"resource"`
	Backups  int    `json:"backups"`
}

// clickhouseViaPlugin reports whether ClickHouse on this server is the
// plugin's to provide: a plugin that can provision it is installed and the
// built-in support runs no ClickHouse here.
func (m *Manager) clickhouseViaPlugin() bool {
	_, manifest, err := m.pluginProviding(clickhousePluginService)
	if err != nil || !manifest.Implements("provision") || !manifest.Implements("release") {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data.NativeClickHouse == nil
}

// clickhouseHandOverPlan looks at what a hand-over would cover.
func (m *Manager) clickhouseHandOverPlan() ClickHouseHandOver {
	plan := ClickHouseHandOver{Applications: []ClickHouseHandOverApp{}}
	record, manifest, pluginErr := m.pluginProviding(clickhousePluginService)
	if pluginErr == nil {
		plan.Plugin, plan.PluginVersion = record.Name, record.Version
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	server := m.data.NativeClickHouse
	if server == nil || !server.Installed {
		plan.Reason = "The built-in support runs no ClickHouse on this server."
		return plan
	}
	plan.BuiltIn, plan.Version = true, server.Version
	busy := ""
	for _, id := range slices.Sorted(maps.Keys(m.data.NativeHostData)) {
		ch := m.data.NativeHostData[id].ClickHouse
		if ch == nil {
			continue
		}
		d := m.data.Databases[ch.Resource]
		plan.Applications = append(plan.Applications, ClickHouseHandOverApp{App: id, Database: ch.Name, Resource: ch.Resource, Backups: len(d.Backups)})
		switch {
		case !ch.Ready || !d.Ready:
			busy = "The ClickHouse database of " + id + " is not ready."
		case d.Operation != "":
			busy = "The ClickHouse database of " + id + " has an operation running (" + d.Operation + ")."
		case m.nativeOperations[id]:
			busy = id + " is being deployed."
		}
	}
	switch {
	case pluginErr != nil:
		plan.Reason = "Install the ClickHouse plugin on this server first."
	case !manifest.Implements("provision") || !manifest.Implements("release") || !manifest.Implements("backup") || !manifest.Implements("restore"):
		plan.Reason = "The installed " + record.Name + " plugin (version " + record.Version + ") cannot run a ClickHouse server; install a newer version."
	case len(plan.Applications) == 0:
		plan.Reason = "No application uses the built-in ClickHouse, so there is nothing to hand over."
	case m.upgradePending():
		plan.Reason = "The agent is being updated."
	case busy != "":
		plan.Reason = busy
	default:
		plan.Available = true
	}
	return plan
}

// handOverClickHouse hands every application's built-in ClickHouse database
// to the plugin. The plugin is asked first, for all of them; the agent's
// own records change only after every answer matched what the applications
// already use, in one write.
func (m *Manager) handOverClickHouse(ctx context.Context) (ClickHouseHandOver, error) {
	nativeHostDataMu.Lock()
	defer nativeHostDataMu.Unlock()
	plan := m.clickhouseHandOverPlan()
	if !plan.Available {
		return plan, errors.New(plan.Reason)
	}
	m.mu.Lock()
	server := *m.data.NativeClickHouse
	m.mu.Unlock()
	admin, err := m.unsealHostSecret(server.Password)
	if err != nil {
		return plan, err
	}
	type taken struct {
		app    ClickHouseHandOverApp
		plugin NativeHostPlugin
	}
	var all []taken
	for _, app := range plan.Applications {
		existing, _ := m.hostDataRecord(app.App)
		ch := existing.ClickHouse
		if ch == nil || ch.Resource != app.Resource {
			return plan, errors.New("the ClickHouse records changed; try again")
		}
		password, err := m.unsealHostSecret(ch.Password)
		if err != nil {
			return plan, err
		}
		handed, _ := json.Marshal(map[string]any{"database": ch.Name, "http_port": ch.HTTPPort, "query_memory_mib": ch.QueryMemoryMiB, "adopt": map[string]int{"tcp_port": server.TCPPort}})
		input, _ := json.Marshal(pluginAppInput{App: app.App, Record: handed, Secrets: map[string]string{"password": password, "admin_password": admin}})
		result, err := m.runPluginHook(ctx, plan.Plugin, "provision", input)
		if err != nil {
			return plan, fmt.Errorf("the %s plugin could not take over %s: %w; nothing was changed", plan.Plugin, ch.Name, err)
		}
		if !result.Answer.OK {
			return plan, fmt.Errorf("the %s plugin did not take over %s: %s; nothing was changed", plan.Plugin, ch.Name, cmpMessages(result.Answer.Messages))
		}
		var out pluginProvisionOutput
		if err := json.Unmarshal(result.Answer.Output, &out); err != nil || len(out.Record) == 0 || len(out.Record) > maxPluginRecord {
			return plan, fmt.Errorf("the %s plugin's answer for %s is not understood; nothing was changed", plan.Plugin, ch.Name)
		}
		// The application must not notice: the plugin has to answer exactly
		// the connection it already uses.
		if !maps.Equal(out.Env, nativeClickHouseEnv(ch.Name, password, ch.HTTPPort)) || out.Secrets["password"] != password {
			return plan, fmt.Errorf("the %s plugin would change the connection of %s; nothing was changed", plan.Plugin, app.App)
		}
		secrets, _ := json.Marshal(out.Secrets)
		sealed, err := m.sealHostSecret(string(secrets))
		if err != nil {
			return plan, err
		}
		all = append(all, taken{app, NativeHostPlugin{Plugin: plan.Plugin, Version: plan.PluginVersion, Record: out.Record, Secrets: sealed, Env: slices.Sorted(maps.Keys(out.Env)), Resource: app.Resource}})
	}

	m.mu.Lock()
	previous := struct {
		host     map[string]NativeHostDataRecord
		bindings map[string]NativeDatabaseBinding
		dbs      map[string]Database
		specs    map[string]NativeCreateRequest
		server   *NativeClickHouseServer
		blocked  *NativeClickHouseBlocked
	}{maps.Clone(m.data.NativeHostData), maps.Clone(m.data.NativeDatabases), maps.Clone(m.data.Databases), maps.Clone(m.data.NativeCreatedSpecs), m.data.NativeClickHouse, m.data.NativeClickHouseBlocked}
	restore := func() {
		m.data.NativeHostData, m.data.NativeDatabases, m.data.Databases, m.data.NativeCreatedSpecs = previous.host, previous.bindings, previous.dbs, previous.specs
		m.data.NativeClickHouse, m.data.NativeClickHouseBlocked = previous.server, previous.blocked
	}
	m.data.NativeHostData, m.data.NativeDatabases, m.data.Databases, m.data.NativeCreatedSpecs = maps.Clone(previous.host), maps.Clone(previous.bindings), maps.Clone(previous.dbs), maps.Clone(previous.specs)
	for _, t := range all {
		id, resource := t.app.App, t.app.Resource
		record := m.data.NativeHostData[id]
		record.Plugins = maps.Clone(record.Plugins)
		if record.Plugins == nil {
			record.Plugins = map[string]NativeHostPlugin{}
		}
		record.Plugins[clickhousePluginService] = t.plugin
		record.ClickHouse = nil
		m.data.NativeHostData[id] = record
		binding := m.data.NativeDatabases[resource]
		policy := NativeDatabasePolicy{Enabled: true, AppID: id, PluginService: clickhousePluginService, ConfigSHA256: nativeHostPluginDigest(clickhousePluginService, id, resource),
			Backup: binding.Policy.Backup, BeforeDeploy: binding.Policy.BeforeDeploy}
		if err := validateNativeDatabasePolicy(resource, policy); err != nil {
			restore()
			m.mu.Unlock()
			return plan, fmt.Errorf("the backup policy of %s cannot be carried over: %w; nothing was changed", resource, err)
		}
		binding.Policy = policy
		m.data.NativeDatabases[resource] = binding
		d := m.data.Databases[resource]
		d.URL = nativePluginResourceURL(clickhousePluginService, id)
		m.data.Databases[resource] = d
		// A later move asks its destination for the plugin's service.
		if spec, recorded := m.data.NativeCreatedSpecs[id]; recorded {
			service := NativePluginService{Service: clickhousePluginService}
			if spec.Data.ClickHouseVersion != "" {
				service.Options, _ = json.Marshal(map[string]string{"series": spec.Data.ClickHouseVersion})
			}
			spec.Data.ClickHouse, spec.Data.ClickHouseVersion = false, ""
			spec.Data.Plugins = append(slices.DeleteFunc(slices.Clone(spec.Data.Plugins), func(p NativePluginService) bool { return p.Service == clickhousePluginService }), service)
			m.data.NativeCreatedSpecs[id] = spec
		}
	}
	m.data.NativeClickHouse, m.data.NativeClickHouseBlocked = nil, nil
	if err := m.save(); err != nil {
		restore()
		m.mu.Unlock()
		return plan, err
	}
	m.mu.Unlock()

	plan.Available, plan.BuiltIn, plan.Reason = false, false, ""
	for _, t := range all {
		plan.Done = append(plan.Done, fmt.Sprintf("%s: the database %s is now the %s plugin's service; its connection is unchanged.", t.app.App, t.app.Database, plan.Plugin))
		// The plugin cannot restore the built-in support's backups, so each
		// database gets one of its own right away. The older ones stay
		// downloadable until retention removes them.
		if err := m.databaseAction(t.app.Resource, "backup"); err != nil {
			plan.Done = append(plan.Done, fmt.Sprintf("%s: a first backup through the plugin could not be started (%v); start one from the database's page.", t.app.App, err))
		} else {
			plan.Done = append(plan.Done, fmt.Sprintf("%s: a first backup through the plugin has started.", t.app.App))
		}
	}
	return plan, nil
}

func (m *Manager) pluginHandOverRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/plugins/handover/clickhouse", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, m.clickhouseHandOverPlan()) })
	mux.HandleFunc("POST /v1/plugins/handover/clickhouse", func(w http.ResponseWriter, r *http.Request) {
		result, err := m.handOverClickHouse(r.Context())
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, result)
	})
}
