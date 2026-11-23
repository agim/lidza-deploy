package agent

// Preflight of a native application. POST /v1/native/preflight takes the
// request that would create the application here (a move's destination spec,
// or a new application) with the sizes of the data that will arrive, and
// rehearses every step createNativeApp and the first copy would take, without
// changing the host. Each step answers ok, or what would stop it and whether
// the agent repairs that itself (host_ready.go) when the application is
// created. A move's review shows the result before anything starts.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NativePreflightRequest is the application that would be created and the
// data that would be copied into it.
type NativePreflightRequest struct {
	Request NativeCreateRequest `json:"request"`
	// PostgresBytes, ClickHouseBytes and FilesBytes are the sizes on the
	// source; zero when unknown or nothing is copied.
	PostgresBytes   int64 `json:"postgres_bytes,omitempty"`
	ClickHouseBytes int64 `json:"clickhouse_bytes,omitempty"`
	FilesBytes      int64 `json:"files_bytes,omitempty"`
}

type NativePreflight struct {
	OK     bool                 `json:"ok"`
	Checks []HostReadinessCheck `json:"checks"`
}

const (
	nativePGDataRoot = "/var/lib/postgresql"
	nativeCHDataRoot = "/var/lib/clickhouse"
)

// preflightDisk is replaced in tests.
var preflightDisk = diskHealth

// existingDir returns dir or its nearest existing ancestor: where data that
// will be written under dir lands today.
func (h nativeHostData) existingDir(dir string) string {
	for dir != "/" && dir != "." {
		if info, err := os.Stat(h.path(dir)); err == nil && info.IsDir() {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return "/"
}

func gib(bytes uint64) string { return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30)) }

// spaceCheck compares what will be written under dir with the free space of
// the file system dir is on: a database on its own disk is measured there,
// not on the root file system.
func (h nativeHostData) spaceCheck(id, title, dir string, incoming int64, factor float64) (HostReadinessCheck, bool) {
	if incoming <= 0 {
		return HostReadinessCheck{}, false
	}
	where := h.existingDir(dir)
	disk, err := preflightDisk(h.path(where))
	if err != nil {
		return HostReadinessCheck{ID: id, Title: title, Detail: "The free space under " + where + " could not be measured."}, true
	}
	needed := uint64(float64(incoming)*factor) + 1<<30
	if disk.Available < needed {
		return HostReadinessCheck{ID: id, Title: title, Detail: fmt.Sprintf("%s has %s free; the data needs about %s there (%s on the source, with room to load it).", where, gib(disk.Available), gib(needed), gib(uint64(incoming)))}, true
	}
	return HostReadinessCheck{ID: id, Title: title, OK: true, Detail: fmt.Sprintf("%s has %s free; about %s is needed.", where, gib(disk.Available), gib(needed))}, true
}

// unitState is a unit's ActiveState ("active", "failed", "inactive", …).
func (h nativeHostData) unitState(ctx context.Context, unit string) string {
	out, _ := h.run(ctx, "", "/usr/bin/systemctl", "show", "-p", "ActiveState", "--value", unit)
	return strings.TrimSpace(out)
}

func (m *Manager) nativePreflight(ctx context.Context, in NativePreflightRequest) NativePreflight {
	out := NativePreflight{OK: true, Checks: []HostReadinessCheck{}}
	add := func(c HostReadinessCheck) {
		out.Checks = append(out.Checks, c)
		out.OK = out.OK && (c.OK || c.Repairable)
	}
	host := nativeHostDataHost(m)
	req := in.Request

	// The request itself: names, runtime, processes, ports, conflicts.
	if err := m.validateNativeCreateRequest(&req); err != nil {
		add(HostReadinessCheck{ID: "application", Title: "Application", Detail: "The application cannot be created here: " + err.Error() + "."})
		return out
	}
	add(HostReadinessCheck{ID: "application", Title: "Application", OK: true, Detail: "The application's account, directories, port and service units can be created."})
	if hostNativeEUID() != 0 {
		add(HostReadinessCheck{ID: "account", Title: "Account", Detail: "The agent does not run as root on this server, so it cannot create the application's own account and service units."})
	}
	m.mu.Lock()
	_, portErr := m.allocateNativePortLocked()
	m.mu.Unlock()
	if portErr != nil {
		add(HostReadinessCheck{ID: "port", Title: "Port", Detail: "No port is free for the application: " + portErr.Error() + "."})
	}

	// PostgreSQL: a usable cluster (or none yet, then one is installed) and room for the data.
	if req.Data.Postgres {
		clusters, _ := host.listClusters(ctx)
		dataDir := nativePGDataRoot
		if problem := host.diagnosePostgres(ctx); problem != nil {
			dataDir = problem.Cluster.DataDir
			add(HostReadinessCheck{ID: "postgres", Title: "PostgreSQL", Detail: problem.Summary, Repairable: problem.Fix != ""})
		} else if cluster, err := chooseNativePGCluster(clusters, host.cfg.PostgresVersion, host.cfg.PostgresPort); err == nil {
			dataDir = cluster.DataDir
			add(HostReadinessCheck{ID: "postgres", Title: "PostgreSQL", OK: true, Detail: pgClusterLabel(cluster) + " is online; the database is created in it."})
		} else if host.fileExists("/usr/bin/apt-get") {
			add(HostReadinessCheck{ID: "postgres", Title: "PostgreSQL", OK: true, Detail: "PostgreSQL is not installed; the agent installs it when the application is created."})
		} else {
			add(HostReadinessCheck{ID: "postgres", Title: "PostgreSQL", Detail: "PostgreSQL is not installed and this server has no apt to install it with."})
		}
		// A dump is restored beside the live copy until it is verified.
		if check, ok := host.spaceCheck("postgres-disk", "PostgreSQL disk", dataDir, in.PostgresBytes, 1.3); ok {
			add(check)
		}
	}

	// Redis: installed and running, or installable.
	if req.Data.Redis {
		switch {
		case !host.fileExists(nativeRedisServer) && host.fileExists("/usr/bin/apt-get"):
			add(HostReadinessCheck{ID: "redis", Title: "Redis", OK: true, Detail: "Redis is not installed; the agent installs it when the application is created."})
		case !host.fileExists(nativeRedisServer):
			add(HostReadinessCheck{ID: "redis", Title: "Redis", Detail: "Redis is not installed and this server has no apt to install it with."})
		default:
			add(HostReadinessCheck{ID: "redis", Title: "Redis", OK: true, Detail: "Redis is installed; the application gets its own instance."})
		}
	}

	// ClickHouse: where the ClickHouse plugin is installed it is the
	// authority on its processor, server and disk rules; the built-in checks
	// below are for servers that do not have it yet.
	pluginKnowsClickHouse := false
	if req.Data.ClickHouse && m.pluginChecks("clickhouse") {
		pluginKnowsClickHouse = true
		options, _ := json.Marshal(map[string]string{"series": req.Data.ClickHouseVersion})
		for _, check := range m.pluginServiceChecks(ctx, req.ID, []NativePluginService{{Service: "clickhouse", Options: options}}, map[string]int64{"clickhouse": in.ClickHouseBytes}) {
			add(check)
		}
	}
	if req.Data.ClickHouse && !pluginKnowsClickHouse {
		installed := host.fileExists(nativeCHServerBinary)
		state := ""
		if installed {
			state = host.unitState(ctx, nativeCHUnit)
		}
		m.mu.Lock()
		blocked := m.data.NativeClickHouseBlocked
		m.mu.Unlock()
		switch {
		case blocked != nil && !(installed && state == "active"):
			// A release already proved unable to run on this processor.
			add(HostReadinessCheck{ID: "clickhouse", Title: "ClickHouse", Detail: blocked.Reason})
		case !installed && host.fileExists("/usr/bin/apt-get") && host.cpuLacksAVX2():
			detail := "ClickHouse is not installed. This server's processor (" + host.cpuSummary() + ") cannot run ClickHouse releases from " + clickhouseAVX2Series + " on, so the agent installs the newest long-term release before that."
			wanted := req.Data.ClickHouseVersion
			newer := wanted != "" && clickhouseSeriesPattern.MatchString(wanted) && compareClickHouseSeries(wanted, clickhouseAVX2Series) >= 0
			if newer {
				detail += " The application comes from ClickHouse " + wanted + ": its data is copied table by table into the older release, the whole database is copied again at the final sync with the application locked, and anything it uses that only " + wanted + " has will not work."
			}
			add(HostReadinessCheck{ID: "clickhouse", Title: "ClickHouse", OK: true, Caution: newer, Detail: detail})
		case !installed && host.fileExists("/usr/bin/apt-get"):
			series := req.Data.ClickHouseVersion
			if series == "" {
				series = "the current stable"
			}
			add(HostReadinessCheck{ID: "clickhouse", Title: "ClickHouse", OK: true, Detail: "ClickHouse is not installed; the agent installs " + series + " series when the application is created."})
		case !installed:
			add(HostReadinessCheck{ID: "clickhouse", Title: "ClickHouse", Detail: "ClickHouse is not installed and this server has no apt to install it with."})
		case state == "active":
			add(HostReadinessCheck{ID: "clickhouse", Title: "ClickHouse", OK: true, Detail: "The ClickHouse server is running; the database is created in it."})
		default:
			// Creating the database enables and starts the server in any case.
			add(HostReadinessCheck{ID: "clickhouse", Title: "ClickHouse", Detail: "The ClickHouse server is installed but not running (its service is " + state + "); the agent starts it when the application is created.", Repairable: true})
		}
		// A BACKUP is unpacked next to the tables it restores.
		if check, ok := host.spaceCheck("clickhouse-disk", "ClickHouse disk", nativeCHDataRoot, in.ClickHouseBytes, 2.1); ok {
			add(check)
		}
	}

	// Services of plugins: the plugin's own check answers for them.
	for _, check := range m.pluginServiceChecks(ctx, req.ID, req.Data.Plugins, map[string]int64{"postgres": in.PostgresBytes, "clickhouse": in.ClickHouseBytes, "files": in.FilesBytes}) {
		add(check)
	}

	// Files land under the application's root.
	if check, ok := host.spaceCheck("files-disk", "Files disk", m.nativeAppsRoot(), in.FilesBytes, 1.1); ok {
		add(check)
	}

	// The web route: the Caddy that owns this server's web ports must answer.
	if caddy, ok := m.nativeCaddy(ctx); ok {
		if status, err := caddy.call(ctx, http.MethodGet, "/config/apps/http/servers/"+caddy.cfg.Server+"/listen", nil); err != nil || status >= 400 {
			add(HostReadinessCheck{ID: "web", Title: "Web route", Detail: "Caddy's admin API at " + caddy.cfg.Admin + " does not answer for its HTTPS server, so the application's hostname cannot be published. Check that Caddy is running."})
		} else {
			add(HostReadinessCheck{ID: "web", Title: "Web route", OK: true, Detail: "The hostname is published through Caddy on this server."})
		}
	}
	return out
}

func (m *Manager) nativePreflightRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/native/preflight", func(w http.ResponseWriter, r *http.Request) {
		var in NativePreflightRequest
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		JSON(w, 200, m.nativePreflight(ctx, in))
	})
}
