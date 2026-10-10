package agent

import (
	"context"
	"github.com/prometheus/procfs"
	"golang.org/x/sys/unix"
	"net/http"
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
	Errors           []string         `json:"errors,omitempty"`
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
		if d, e := diskHealth(path); e == nil {
			out.Disks = append(out.Disks, d)
		} else {
			out.Errors = append(out.Errors, "disk metrics unavailable: "+path)
		}
	}
	m.mu.Lock()
	var dbs []Database
	for _, d := range m.data.Databases {
		if d.Ready && d.Operation == "" {
			dbs = append(dbs, d)
		}
	}
	m.mu.Unlock()
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, d := range dbs {
		wg.Add(1)
		go func(d Database) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			env, flags := postgresEnvironment(d.URL)
			name := "lidza-size-" + newID()
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, _ = command(cleanup, "", nil, "docker", "rm", "-f", name)
			}()
			args := []string{"run", "--rm", "--name", name, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "128m"}
			if d.Network != "" {
				args = append(args, "--network", d.Network)
			}
			args = append(args, flags...)
			args = append(args, databaseImage, "psql", "-X", "-tAc", "SELECT pg_database_size(current_database())")
			var b limitedBuffer
			err := dockerStream(ctx, nil, &b, env, args...)
			n, e := strconv.ParseInt(strings.TrimSpace(b.String()), 10, 64)
			mu.Lock()
			defer mu.Unlock()
			if err == nil && e == nil {
				out.DatabaseBytes[d.AppID] = n
			} else {
				out.Errors = append(out.Errors, "database size unavailable: "+d.AppID)
			}
		}(d)
	}
	wg.Wait()
	return out
}
func (m *Manager) healthRoute(w http.ResponseWriter, r *http.Request) {
	JSON(w, 200, m.serverHealth(r.Context()))
}
