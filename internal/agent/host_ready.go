package agent

// Server readiness: what on this host would stop an application from
// running here or being moved here, checked before anyone finds out the
// hard way. The installer asks for a repair when it finishes, the control
// panel shows the report on the server's page and a move's review lists what
// is left; the agent itself repairs the database service before it needs it.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// HostReadinessCheck is one thing applications depend on.
type HostReadinessCheck struct {
	ID    string `json:"id"` // native, postgres, docker
	Title string `json:"title"`
	OK    bool   `json:"ok"`
	// Detail says what is wrong, or what is in place.
	Detail string `json:"detail,omitempty"`
	// Repairable problems are fixed by POST /v1/host/readiness/repair, and
	// by the agent itself when an application needs the service.
	Repairable bool `json:"repairable,omitempty"`
	// Caution marks a passing check with a risk worth reading before going on.
	Caution bool `json:"caution,omitempty"`
}

type HostReadiness struct {
	OK      bool                 `json:"ok"`
	Checks  []HostReadinessCheck `json:"checks"`
	Checked time.Time            `json:"checked"`
	// Repaired lists what a repair request did.
	Repaired []string `json:"repaired,omitempty"`
	// Failed lists repairs that were tried and did not work, with the reason.
	Failed []string `json:"failed,omitempty"`
}

func (m *Manager) hostReadiness(ctx context.Context) HostReadiness {
	report := HostReadiness{OK: true, Checks: []HostReadinessCheck{}, Checked: time.Now().UTC()}
	add := func(c HostReadinessCheck) {
		report.Checks = append(report.Checks, c)
		report.OK = report.OK && c.OK
	}
	if m.cfg.IngressOnly {
		return report
	}
	native := m.hostNativeStatus()
	switch {
	case native.Configured:
		add(HostReadinessCheck{ID: "native", Title: "Native applications", OK: true, Detail: "Deployment account " + native.User + ", root " + native.Root + "."})
	case native.Supported:
		add(HostReadinessCheck{ID: "native", Title: "Native applications", Detail: "This server is not set up for native applications yet; the agent sets it up when it restarts.", Repairable: true})
	default:
		add(HostReadinessCheck{ID: "native", Title: "Native applications", Detail: native.Reason})
	}
	host := nativeHostDataHost(m)
	if clusters, _ := host.listClusters(ctx); len(clusters) > 0 {
		if problem := host.diagnosePostgres(ctx); problem != nil {
			add(HostReadinessCheck{ID: "postgres", Title: "PostgreSQL", Detail: problem.Summary, Repairable: problem.Fix != ""})
		} else if cluster, err := chooseNativePGCluster(clusters, host.cfg.PostgresVersion, host.cfg.PostgresPort); err == nil {
			add(HostReadinessCheck{ID: "postgres", Title: "PostgreSQL", OK: true, Detail: pgClusterLabel(cluster) + " is online."})
		}
	}
	if docker := observeDocker(ctx); docker.Installed {
		if docker.Running {
			add(HostReadinessCheck{ID: "docker", Title: "Docker", OK: true, Detail: "Docker " + docker.Version + " is running."})
		} else {
			add(HostReadinessCheck{ID: "docker", Title: "Docker", Detail: "Docker is installed but its daemon is not running; containers cannot start.", Repairable: true})
		}
	}
	// Services a plugin checks are that plugin's to report (above).
	covered := func(id string) bool { return id == "clickhouse" && m.pluginChecks("clickhouse") }
	for _, service := range host.readinessServices() {
		if covered(service.id) {
			continue
		}
		switch state := host.unitState(ctx, service.unit); {
		case state == "active":
			add(HostReadinessCheck{ID: service.id, Title: service.title, OK: true, Detail: service.title + " is running."})
		case state == "failed":
			add(HostReadinessCheck{ID: service.id, Title: service.title, Detail: service.title + " is down: its service (" + service.unit + ") failed.", Repairable: true})
		case service.always:
			add(HostReadinessCheck{ID: service.id, Title: service.title, Detail: service.title + " is not running (" + service.unit + " is " + state + "); this server's applications are not reachable without it.", Repairable: true})
		}
		// Otherwise it was stopped on purpose, or runs per application: not this report's business.
	}
	// What plugins manage: each one's own check, with no application in mind.
	for _, check := range m.pluginReadinessChecks(ctx) {
		add(check)
	}
	// Half-configured packages stop every later installation and update.
	if broken := host.brokenPackages(ctx); len(broken) > 0 && !(m.ownsBrokenPackages(host, broken) && covered("clickhouse")) {
		add(HostReadinessCheck{ID: "packages", Title: "Package manager", Detail: "Packages are only half installed (" + strings.Join(broken, ", ") + "); nothing can be installed or updated on this server until that is resolved.", Repairable: m.ownsBrokenPackages(host, broken)})
	}
	// A full disk stops deployments, databases and logs alike.
	paths := []string{"/"}
	if clusters, _ := host.listClusters(ctx); len(clusters) > 0 {
		for _, c := range clusters {
			paths = append(paths, host.existingDir(c.DataDir))
		}
	}
	seen := map[uint64]bool{}
	for _, path := range paths {
		disk, err := preflightDisk(host.path(path))
		if err != nil || disk.Total == 0 || seen[disk.Total^disk.Available] {
			continue
		}
		seen[disk.Total^disk.Available] = true
		if disk.Available < 2<<30 || disk.UsedPercent > 95 {
			add(HostReadinessCheck{ID: "disk", Title: "Disk space", Detail: fmt.Sprintf("%s is almost full: %s free (%.0f%% used). Free space there, or clean up under the server's Disk cleanup tab.", path, gib(disk.Available), disk.UsedPercent)})
		}
	}
	return report
}

// brokenPackages lists packages dpkg reports as unpacked or half configured.
func (h nativeHostData) brokenPackages(ctx context.Context) []string {
	if !h.fileExists("/usr/bin/dpkg-query") {
		return nil
	}
	out, err := h.run(ctx, "", "/usr/bin/dpkg-query", "-W", "-f", "${db:Status-Abbrev} ${Package}\\n")
	if err != nil {
		return nil
	}
	broken := []string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		// Desired state install, actual state unpacked (U), half-configured (F) or half-installed (H).
		if len(fields) == 2 && len(fields[0]) >= 2 && fields[0][0] == 'i' && strings.ContainsRune("UFH", rune(fields[0][1])) {
			broken = append(broken, fields[1])
		}
	}
	return broken
}

// ownsBrokenPackages reports that every broken package is of a ClickHouse
// installation this agent started and never completed: those it may remove.
func (m *Manager) ownsBrokenPackages(h nativeHostData, broken []string) bool {
	m.mu.Lock()
	completed := m.data.NativeClickHouse != nil && m.data.NativeClickHouse.Installed
	m.mu.Unlock()
	if completed || len(broken) == 0 {
		return false
	}
	for _, name := range broken {
		if !strings.HasPrefix(name, "clickhouse-") {
			return false
		}
	}
	return true
}

// readinessService is a host service applications depend on.
type readinessService struct {
	id, title, unit string
	// always is true for a service this server cannot do without: stopped
	// counts as a problem, not as a decision.
	always bool
}

const installerCaddyMarker = "# Managed by lidza-deploy installer"

// readinessServices lists the services present on this host. Caddy counts
// only where the hosting installer set it up as the server's web entrance;
// another layout's Caddy is not the agent's to start.
func (h nativeHostData) readinessServices() []readinessService {
	services := []readinessService{}
	if h.fileExists(nativeCHServerBinary) {
		services = append(services, readinessService{id: "clickhouse", title: "ClickHouse", unit: nativeCHUnit})
	}
	if h.fileExists(nativeRedisServer) {
		services = append(services, readinessService{id: "redis", title: "Redis", unit: "redis-server.service"})
	}
	if raw, err := os.ReadFile(h.path("/etc/caddy/Caddyfile")); err == nil && strings.HasPrefix(string(raw), installerCaddyMarker) {
		services = append(services, readinessService{id: "web", title: "Web server (Caddy)", unit: "caddy.service", always: true})
	}
	return services
}

// startService starts a service whose unit failed (or, for a service the
// server cannot do without, is stopped) and reports what it did.
func (h nativeHostData) startService(ctx context.Context, service readinessService) (string, error) {
	_, _ = h.run(ctx, "", "/usr/bin/systemctl", "reset-failed", service.unit)
	if out, err := h.run(ctx, "", "/usr/bin/systemctl", "start", service.unit); err != nil {
		tail, _ := h.run(ctx, "", "/usr/bin/journalctl", "-u", service.unit, "-n", "5", "--no-pager", "-o", "cat")
		return "", fmt.Errorf("%s did not start: %s", service.title, nativeTrimOutput(strings.TrimSpace(out+"\n"+tail)))
	}
	return "started " + service.title, nil
}

// repairHost fixes what the report marks repairable and returns the report
// afterwards. Native setup is the agent's own start step and is not run here.
func (m *Manager) repairHost(ctx context.Context) HostReadiness {
	repaired, failed := []string{}, []string{}
	if !m.cfg.IngressOnly {
		host := nativeHostDataHost(m)
		if problem := host.diagnosePostgres(ctx); problem != nil && problem.Fix != "" {
			if did, err := host.repairPostgres(ctx, problem); err != nil {
				failed = append(failed, err.Error())
			} else {
				repaired = append(repaired, did)
			}
		}
		if docker := observeDocker(ctx); docker.Installed && !docker.Running {
			if err := ensureDocker(ctx, m); err != nil {
				failed = append(failed, "Docker could not be started: "+err.Error())
			} else if dockerRunning(ctx) {
				repaired = append(repaired, "started Docker")
			}
		}
		pluginRepaired, pluginFailed := m.repairPlugins(ctx)
		repaired, failed = append(repaired, pluginRepaired...), append(failed, pluginFailed...)
		// With the ClickHouse plugin installed, its repair (above) owns ClickHouse.
		pluginOwnsClickHouse := m.pluginRepairs("clickhouse")
		if broken := host.brokenPackages(ctx); !pluginOwnsClickHouse && m.ownsBrokenPackages(host, broken) {
			if host.removeClickHousePackages(ctx) {
				repaired = append(repaired, "removed the packages of a ClickHouse installation that did not complete ("+strings.Join(broken, ", ")+")")
			} else {
				failed = append(failed, "the half-installed ClickHouse packages could not be removed; run: apt-get purge clickhouse-server clickhouse-client clickhouse-common-static")
			}
		}
		for _, service := range host.readinessServices() {
			if service.id == "clickhouse" && pluginOwnsClickHouse {
				continue
			}
			if state := host.unitState(ctx, service.unit); state == "failed" || service.always && state != "active" {
				if did, err := host.startService(ctx, service); err != nil {
					failed = append(failed, err.Error())
				} else {
					repaired = append(repaired, did)
				}
			}
		}
	}
	report := m.hostReadiness(ctx)
	report.Repaired, report.Failed = repaired, failed
	return report
}

// RepairHostAfterStart runs one repair pass shortly after the agent starts,
// so a server heals from what an earlier failure left behind without anyone
// asking, and logs what it did.
func (m *Manager) RepairHostAfterStart(ctx context.Context, delay time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	report := m.repairHost(bounded)
	for _, line := range report.Repaired {
		log.Printf("server readiness: repaired: %s", line)
	}
	for _, line := range report.Failed {
		log.Printf("server readiness: could not repair: %s", line)
	}
	for _, check := range report.Checks {
		if !check.OK && !check.Repairable {
			log.Printf("server readiness: needs attention: %s: %s", check.Title, check.Detail)
		}
	}
}

func (m *Manager) hostReadinessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/host/readiness", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		JSON(w, 200, m.hostReadiness(ctx))
	})
	mux.HandleFunc("POST /v1/host/readiness/repair", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		JSON(w, 200, m.repairHost(ctx))
	})
}
