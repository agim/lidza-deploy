package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/prometheus/procfs"
	"golang.org/x/sys/unix"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DiskHealth struct {
	Path        string  `json:"path"`
	Total       uint64  `json:"total"`
	Available   uint64  `json:"available"`
	UsedPercent float64 `json:"used_percent"`
}
type ServerHealth struct {
	Checked          time.Time        `json:"checked"`
	CPUs             int              `json:"cpus"`
	CPUPercent       *float64         `json:"cpu_percent,omitempty"`
	Load1            float64          `json:"load1"`
	MemoryTotal      uint64           `json:"memory_total"`
	MemoryAvailable  uint64           `json:"memory_available"`
	SwapTotal        uint64           `json:"swap_total"`
	SwapFree         uint64           `json:"swap_free"`
	SwapProvisioning SwapStatus       `json:"swap_provisioning"`
	Disks            []DiskHealth     `json:"disks"`
	DatabaseBytes    map[string]int64 `json:"database_bytes"`
	// RetainedDatabases are the databases no application uses any more, by
	// id: their data stays until the operator removes them under the
	// server's retained resources. One that still answers is measured in
	// DatabaseBytes as well; otherwise its last backup stands in.
	RetainedDatabases map[string]RetainedDatabase `json:"retained_databases,omitempty"`
	Errors            []string                    `json:"errors,omitempty"`
	// Warnings are host conditions that need the operator's attention but
	// do not stop deployments, for example stale Bundler versions.
	Warnings []string `json:"warnings,omitempty"`
	// Notes are host conditions the agent already handles and the operator
	// may want to know about; nothing is required of them.
	Notes []string `json:"notes,omitempty"`
	// DiskLow marks a disk at least 85% full or within its free-space
	// reserve: new backups, builds and move transfers that would take it
	// below the reserve are refused (disk_reserve.go).
	DiskLow bool `json:"disk_low,omitempty"`
	// ClickHouseSystemLogs is the disk space of each managed ClickHouse's
	// system log tables, by application ("host" for the host server), from
	// the hourly retention check.
	ClickHouseSystemLogs map[string]int64 `json:"clickhouse_system_logs,omitempty"`
	// Ingress is the host's web ingress: the agent proxy or an admin-API
	// Caddy (host_ingress.go).
	Ingress *HostIngressStatus `json:"ingress,omitempty"`
	// Docker is present only when Docker is installed; Native whenever the
	// host has systemd (host_runtime_health.go).
	Docker *HostDockerHealth `json:"docker,omitempty"`
	Native *HostNativeHealth `json:"native,omitempty"`
}

// RetainedDatabase is a retained database's last backup, when it has one.
type RetainedDatabase struct {
	BackupBytes   int64     `json:"backup_bytes,omitempty"`
	BackupCreated time.Time `json:"backup_created,omitzero"`
}

func diskHealth(path string) (DiskHealth, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return DiskHealth{}, err
	}
	total := st.Blocks * uint64(st.Bsize)
	available := st.Bavail * uint64(st.Bsize)
	used := float64(0)
	if total > 0 {
		used = 100 * float64(st.Blocks-st.Bfree) / float64(st.Blocks)
	}
	return DiskHealth{path, total, available, used}, nil
}
func (m *Manager) serverHealth(parent context.Context) ServerHealth {
	ctx, cancel := context.WithTimeout(parent, 6*time.Second)
	defer cancel()
	out := ServerHealth{Checked: time.Now().UTC(), CPUs: runtime.NumCPU(), SwapProvisioning: m.swapStatus(), DatabaseBytes: map[string]int64{}, Disks: []DiskHealth{}}
	ingress := m.hostIngress(ctx, false)
	out.Ingress = &ingress
	fs, err := procfs.NewFS("/proc")
	if err == nil {
		mem, e := fs.Meminfo()
		if e == nil && mem.MemTotalBytes != nil && mem.MemAvailableBytes != nil {
			out.MemoryTotal = *mem.MemTotalBytes
			out.MemoryAvailable = *mem.MemAvailableBytes
			if mem.SwapTotalBytes != nil {
				out.SwapTotal = *mem.SwapTotalBytes
			}
			if mem.SwapFreeBytes != nil {
				out.SwapFree = *mem.SwapFreeBytes
			}
		} else {
			out.Errors = append(out.Errors, "memory metrics unavailable")
		}
		if stat, e := fs.Stat(); e == nil {
			cpu := stat.CPUTotal
			total := cpu.User + cpu.Nice + cpu.System + cpu.Idle + cpu.Iowait + cpu.IRQ + cpu.SoftIRQ + cpu.Steal
			idle := cpu.Idle + cpu.Iowait
			m.mu.Lock()
			if total > m.cpuTotal && m.cpuTotal > 0 {
				value := 100 * (1 - (idle-m.cpuIdle)/(total-m.cpuTotal))
				if value < 0 {
					value = 0
				}
				if value > 100 {
					value = 100
				}
				out.CPUPercent = &value
			}
			m.cpuTotal = total
			m.cpuIdle = idle
			m.mu.Unlock()
		}
		load, e := fs.LoadAvg()
		if e == nil {
			out.Load1 = load.Load1
		}
	} else {
		out.Errors = append(out.Errors, "host metrics unavailable")
	}
	paths := []string{m.cfg.DataDir}
	if root, e := command(ctx, "", nil, "docker", "info", "--format", "{{.DockerRootDir}}"); e == nil && root != "" && root != m.cfg.DataDir {
		paths = append(paths, root)
	}
	for _, path := range paths {
		if d, e := diskStat(path); e == nil {
			out.Disks = append(out.Disks, d)
			if diskLow(d) {
				out.DiskLow = true
				out.Warnings = append(out.Warnings, fmt.Sprintf("Disk %s is %.0f%% full with %s free. New backups, builds and move transfers are refused when they would leave less than %s free; run Disk cleanup or add disk.", path, d.UsedPercent, moveHumanBytes(d.Available), moveHumanBytes(diskReserve(d))))
			}
		} else {
			out.Errors = append(out.Errors, "disk metrics unavailable: "+path)
		}
	}
	m.databaseSizes(ctx, &out)
	m.runtimeHealth(ctx, &out)
	warnings, notes := m.nativeRubyHealth()
	out.Warnings = append(out.Warnings, warnings...)
	out.Notes = append(out.Notes, notes...)
	out.ClickHouseSystemLogs = m.clickhouseLogSizes()
	out.Warnings = append(out.Warnings, clickhouseLogWarnings(out.ClickHouseSystemLogs)...)
	return out
}
func (m *Manager) healthRoute(w http.ResponseWriter, r *http.Request) {
	JSON(w, 200, m.serverHealth(r.Context()))
}

// measuredDatabase is one database the health check sizes: a live one, an
// archive measured by its archive_database name on the host cluster, or a
// retained one that may no longer answer.
type measuredDatabase struct {
	Database
	archive  NativeDatabasePolicy // ArchiveDatabase set for archives
	retained bool
}

// databaseSizes measures every ready database, eight at a time. A live
// database that cannot be measured is an error with the probe's reason; a
// retained one falls back to its last backup without an error, since its
// live database may be gone.
func (m *Manager) databaseSizes(ctx context.Context, out *ServerHealth) {
	m.mu.Lock()
	var dbs []measuredDatabase
	for id, d := range m.data.Databases {
		if d.Ready && d.Operation == "" {
			dbs = append(dbs, measuredDatabase{Database: d, archive: m.data.NativeDatabases[id].Policy, retained: !m.databaseInUseLocked(id)})
		}
	}
	m.mu.Unlock()
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, d := range dbs {
		wg.Add(1)
		go func(d measuredDatabase) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			n, err := m.measureDatabase(ctx, d)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				out.DatabaseBytes[d.AppID] = n
			}
			if d.retained {
				if out.RetainedDatabases == nil {
					out.RetainedDatabases = map[string]RetainedDatabase{}
				}
				out.RetainedDatabases[d.AppID] = lastBackup(d.Backups)
				return
			}
			if err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("database size unavailable: %s (%s)", d.AppID, probeReason(err)))
			}
		}(d)
	}
	wg.Wait()
}

func (m *Manager) measureDatabase(ctx context.Context, d measuredDatabase) (int64, error) {
	if strings.HasPrefix(d.URL, "clickhouse://") {
		return m.hostClickHouseSize(ctx, d.AppID)
	}
	if d.archive.ArchiveDatabase != "" {
		raw, err := nativeArchiveQuery(ctx, d.archive, "SELECT pg_database_size(current_database())")
		if err != nil {
			return 0, err
		}
		return parseDatabaseSize(raw)
	}
	if n, ok, err := measureHostDatabase(ctx, d.Database); ok {
		return n, err
	}
	env, flags := postgresEnvironment(d.URL)
	name := "lidza-size-" + newID()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = command(cleanup, "", nil, "docker", "rm", "-f", name)
	}()
	args := []string{"run", "--rm", "--name", name, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges"}
	if d.Network != "" {
		args = append(args, "--network", d.Network)
	}
	args = append(args, flags...)
	args = append(args, databaseImage, "psql", "-X", "-tAc", "SELECT pg_database_size(current_database())")
	var b limitedBuffer
	if err := dockerStream(ctx, nil, &b, env, args...); err != nil {
		return 0, err
	}
	return parseDatabaseSize(b.String())
}

func parseDatabaseSize(raw string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, errors.New("no size returned")
	}
	return n, nil
}

// lastBackup is the newest backup record's size and time, or zero.
func lastBackup(backups []BackupRecord) RetainedDatabase {
	var last RetainedDatabase
	for _, b := range backups {
		if last.BackupCreated.IsZero() || b.Created.After(last.BackupCreated) {
			last = RetainedDatabase{BackupBytes: b.Size, BackupCreated: b.Created}
		}
	}
	return last
}

// probeReason is a probe failure in one line: a failed command's first
// stderr line when it has one (psql names the refused connection or missing
// database there), else the error itself.
func probeReason(err error) string {
	reason := err.Error()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		for _, line := range strings.Split(string(exit.Stderr), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				reason = line
				break
			}
		}
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) > 200 {
		reason = reason[:200] + "…"
	}
	return reason
}

// measureHostDatabase is hostDatabaseSize; tests replace it.
var measureHostDatabase = hostDatabaseSize

// hostDatabaseSize measures databases the host itself serves: SQLite files by
// their size on disk (with the write-ahead log), and external PostgreSQL with
// the host's psql using the database's own connection settings. It reports
// false for container databases, which are measured inside their network.
func hostDatabaseSize(ctx context.Context, d Database) (int64, bool, error) {
	switch {
	case strings.HasPrefix(d.URL, "/"):
		var total int64
		for _, suffix := range []string{"", "-wal"} {
			info, err := os.Stat(d.URL + suffix)
			if err != nil {
				if suffix == "" {
					return 0, true, err
				}
				continue
			}
			total += info.Size()
		}
		return total, true, nil
	case d.Mode == "external" && (strings.HasPrefix(d.URL, "postgres://") || strings.HasPrefix(d.URL, "postgresql://")):
		psql, err := exec.LookPath("psql")
		if err != nil {
			return 0, true, err
		}
		env, _ := postgresEnvironment(d.URL)
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, psql, "-X", "-tAc", "SELECT pg_database_size(current_database())")
		cmd.Env = append(env, "PATH=/usr/bin:/bin", "PGAPPNAME=lidza-size")
		out, err := cmd.Output()
		if err != nil {
			return 0, true, err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		return n, true, err
	}
	return 0, false, nil
}
