package agent

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Runtime sections of the server health report. A host may run native
// applications, containers or both: the Docker section is present only when
// Docker is installed, the native section whenever the host has systemd.

// HostDockerHealth summarizes the container runtime.
type HostDockerHealth struct {
	Version    string `json:"version,omitempty"`
	Containers int    `json:"containers"`
	Running    int    `json:"running"`
}

// HostNativeHealth summarizes native applications and the host's systemd.
type HostNativeHealth struct {
	// Applications and Units count the registered native applications and
	// their units; Active and Inactive split the units by state.
	Applications int `json:"applications"`
	Units        int `json:"units"`
	Active       int `json:"active"`
	Inactive     int `json:"inactive"`
	// FailedUnits are failed units of native applications; FailedHostUnits
	// every failed system unit of the host.
	FailedUnits     []string `json:"failed_units"`
	FailedHostUnits []string `json:"failed_host_units"`
	// JournalBytes is the disk space of the systemd journal; zero when it
	// could not be read.
	JournalBytes int64 `json:"journal_bytes,omitempty"`
	// Releases is the disk space of each application's releases directory,
	// measured in the background and at most every releaseDiskTTL.
	Releases []NativeReleaseDisk `json:"releases"`
}

// NativeReleaseDisk is the size of one application's retained releases.
type NativeReleaseDisk struct {
	AppID    string    `json:"app_id"`
	Path     string    `json:"path"`
	Bytes    int64     `json:"bytes"`
	Releases int       `json:"releases"`
	Measured time.Time `json:"measured"`
}

// runtimeHealth adds the Docker and native sections to a health report.
func (m *Manager) runtimeHealth(ctx context.Context, out *ServerHealth) {
	out.Docker = dockerHealth(ctx)
	if native, ok := m.nativeHealth(ctx); ok {
		out.Native = &native
	}
}

var dockerInstalled = func() bool { _, err := exec.LookPath("docker"); return err == nil }

func dockerHealth(ctx context.Context) *HostDockerHealth {
	if !dockerInstalled() {
		return nil
	}
	raw, err := command(ctx, "", nil, "docker", "info", "--format", "{{.ServerVersion}} {{.Containers}} {{.ContainersRunning}}")
	health := &HostDockerHealth{}
	if err != nil {
		// Installed but not answering: the section stays, without counts.
		return health
	}
	fields := strings.Fields(raw)
	if len(fields) == 3 {
		health.Version = fields[0]
		health.Containers, _ = strconv.Atoi(fields[1])
		health.Running, _ = strconv.Atoi(fields[2])
	}
	return health
}

var systemdInstalled = func() bool { _, err := exec.LookPath("systemctl"); return err == nil }

func (m *Manager) nativeHealth(ctx context.Context) (HostNativeHealth, bool) {
	if m.nativeCommand == nil && !systemdInstalled() {
		return HostNativeHealth{}, false
	}
	health := HostNativeHealth{FailedUnits: []string{}, FailedHostUnits: []string{}, Releases: []NativeReleaseDisk{}}
	if out, err := m.nativeRootExec(ctx, "systemctl", "list-units", "--state=failed", "--type=service", "--plain", "--no-legend", "--no-pager"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if fields := strings.Fields(line); len(fields) > 0 && nativeUnitPattern.MatchString(fields[0]) {
				health.FailedHostUnits = append(health.FailedHostUnits, fields[0])
			}
		}
	}
	if out, err := m.nativeRootExec(ctx, "journalctl", "--disk-usage", "--no-pager"); err == nil {
		health.JournalBytes = journalDiskUsage(out)
	}
	m.mu.Lock()
	ids := slices.Sorted(maps.Keys(m.data.NativeApps))
	m.mu.Unlock()
	health.Applications = len(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		status, err := m.nativeStatus(ctx, id)
		if err != nil {
			continue
		}
		for _, service := range status.Services {
			health.Units++
			switch service.Active {
			case "active", "reloading", "activating":
				health.Active++
			case "failed":
				health.Inactive++
				health.FailedUnits = append(health.FailedUnits, service.Unit)
			default:
				health.Inactive++
			}
		}
		if root := m.nativeApplicationRoot(id); root != "" {
			if disk, ok := releaseDisk(id, filepath.Join(root, "releases")); ok {
				health.Releases = append(health.Releases, disk)
			}
		}
	}
	return health, true
}

// nativeApplicationRoot is the directory holding an application's
// releases: its release policy's root, or <native root>/<name> for
// applications bound through the deployment user's units.
func (m *Manager) nativeApplicationRoot(id string) string {
	if _, policy, err := m.nativeReleasePolicy(id); err == nil && filepath.IsAbs(policy.Root) {
		return filepath.Clean(policy.Root)
	}
	m.mu.Lock()
	a, ok := m.data.NativeApps[id]
	m.mu.Unlock()
	if !ok || m.cfg.Native == nil || !nativeNamePattern.MatchString(a.Name) || !filepath.IsAbs(m.cfg.Native.Root) {
		return ""
	}
	return filepath.Join(m.cfg.Native.Root, a.Name)
}

var journalUsagePattern = regexp.MustCompile(`take up ([0-9.]+)([KMGTP]?)(i?B)?\b`)

// journalDiskUsage parses "Archived and active journals take up 1.2G in
// the file system."
func journalDiskUsage(out string) int64 {
	match := journalUsagePattern.FindStringSubmatch(out)
	if match == nil {
		return 0
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0
	}
	exponent := strings.Index("KMGTP", match[2]) + 1
	if match[2] == "" {
		exponent = 0
	}
	for range exponent {
		value *= 1024
	}
	return int64(value)
}

// Release directories are measured in the background: a walk of many
// releases can take longer than a health request, and their size changes
// only with deploys.
const releaseDiskTTL = 15 * time.Minute

var (
	releaseDiskMu       sync.Mutex
	releaseDiskCache    = map[string]NativeReleaseDisk{}
	releaseDiskRunning  = map[string]bool{}
	releaseDiskMaxFiles = 2_000_000
	// releaseDiskSync measures in the caller; tests set it.
	releaseDiskSync = false
)

// releaseDisk returns the last measurement of dir and starts a new one when
// it is missing or older than releaseDiskTTL.
func releaseDisk(appID, dir string) (NativeReleaseDisk, bool) {
	releaseDiskMu.Lock()
	cached, ok := releaseDiskCache[dir]
	stale := !ok || time.Since(cached.Measured) > releaseDiskTTL
	start := stale && !releaseDiskRunning[dir]
	if start {
		releaseDiskRunning[dir] = true
	}
	releaseDiskMu.Unlock()
	if start {
		measure := func() {
			disk := measureReleases(appID, dir)
			releaseDiskMu.Lock()
			delete(releaseDiskRunning, dir)
			if disk.Path != "" {
				releaseDiskCache[dir] = disk
			}
			releaseDiskMu.Unlock()
		}
		if releaseDiskSync {
			measure()
			releaseDiskMu.Lock()
			cached, ok = releaseDiskCache[dir]
			releaseDiskMu.Unlock()
		} else {
			go measure()
		}
	}
	cached.AppID = appID
	return cached, ok
}

// measureReleases sums regular file sizes under dir without following
// symbolic links.
func measureReleases(appID, dir string) NativeReleaseDisk {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return NativeReleaseDisk{}
	}
	disk := NativeReleaseDisk{AppID: appID, Path: dir, Measured: time.Now().UTC()}
	for _, entry := range entries {
		if entry.IsDir() {
			disk.Releases++
		}
	}
	files := 0
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if files++; files > releaseDiskMaxFiles {
			return filepath.SkipAll
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				disk.Bytes += info.Size()
			}
		}
		return nil
	})
	return disk
}
