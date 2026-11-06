package agent

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Disk hygiene of the server cleanup, beside Docker's build cache and the
// system journal (server_maintenance.go). On hosts where the agent manages
// Docker it removes:
//
//   - images of hosted applications older than each application's current
//     and previous release, so a rollback keeps working; an image any
//     container uses, running or stopped, is never removed;
//   - stopped BuildKit builder containers ("buildx_buildkit_<node>") and
//     their state volumes, stopped for over a week and not part of the
//     builder in use (the agent builds with Docker's default builder);
//   - anonymous volumes no container references (named volumes, which
//     hold data, are never touched).
//
// On apt hosts it also empties the package cache (apt-get clean). Every item
// is reported with the space it frees, and the cleanup page shows the same
// list before anything runs.

// CleanupItem is one thing the cleanup removes, or would remove.
type CleanupItem struct {
	// image, builder, volume, package-cache, move-temp, native-release,
	// native-cache, native-scratch, toolchain, app-log, core-dump
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	Reason string `json:"reason,omitempty"`
	Path   string `json:"path,omitempty"` // toolchain items: the installation
	remove [][]string
	apply  func() error // native items: removes files directly
}

// CleanupPreview is what the next cleanup would remove now.
type CleanupPreview struct {
	Checked time.Time     `json:"checked"`
	Items   []CleanupItem `json:"items"`
	Bytes   int64         `json:"bytes"`
	Notes   []string      `json:"notes,omitempty"`
}

const (
	builderStoppedFor = 7 * 24 * time.Hour
	// imageMinimumAge spares an image built moments ago by a release that
	// has not been recorded yet.
	imageMinimumAge = time.Hour
	aptArchiveDir   = "/var/cache/apt/archives"
)

// hygieneHostRoot reports whether host-wide cleanup can run: it needs root;
// tests replace it.
var hygieneHostRoot = func() bool { return os.Geteuid() == 0 }

type commandRunner func(ctx context.Context, name string, args ...string) (string, error)

var anonymousVolume = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hostedImages are the application images a rollback needs, and the
// container applications that own images, longest identifier first.
func (m *Manager) hostedImages() (map[string]bool, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keep := map[string]bool{}
	var apps []string
	for id, a := range m.data.Apps {
		if a.Native != nil {
			continue
		}
		apps = append(apps, id)
		for _, r := range []*Release{a.Current, a.Previous} {
			if r == nil {
				continue
			}
			if r.Image != "" {
				keep[r.Image] = true
			}
			if r.Stack != nil {
				for _, c := range r.Stack.Containers {
					keep[c.Image] = true
				}
			}
		}
	}
	slices.SortFunc(apps, func(a, b string) int { return len(b) - len(a) })
	return keep, apps
}

// imageOwner is the hosted application a "lidza/<app>[-<service>]"
// repository belongs to, or "".
func imageOwner(repository string, apps []string) string {
	name, ok := strings.CutPrefix(repository, "lidza/")
	if !ok {
		return ""
	}
	for _, id := range apps { // longest first
		if name == id || strings.HasPrefix(name, id+"-") {
			return id
		}
	}
	return ""
}

func parseDockerTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05 -0700 MST"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func lines(out string) []string {
	var rows []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rows = append(rows, line)
		}
	}
	return rows
}

// usedImageIDs are the image IDs of every container, running or stopped.
func usedImageIDs(ctx context.Context, run commandRunner) (map[string]bool, error) {
	out, err := run(ctx, "docker", "container", "ls", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return nil, fmt.Errorf("containers could not be listed: %w", err)
	}
	used := map[string]bool{}
	ids := lines(out)
	if len(ids) == 0 {
		return used, nil
	}
	out, err = run(ctx, "docker", append([]string{"container", "inspect", "--format", "{{.Image}}"}, ids...)...)
	if err != nil {
		return nil, fmt.Errorf("containers could not be inspected: %w", err)
	}
	for _, id := range lines(out) {
		used[id] = true
	}
	return used, nil
}

// planImages lists application images older than the current and previous
// release of their application.
func (m *Manager) planImages(ctx context.Context, run commandRunner, now time.Time) ([]CleanupItem, error) {
	keep, apps := m.hostedImages()
	if len(apps) == 0 {
		return nil, nil
	}
	used, err := usedImageIDs(ctx, run)
	if err != nil {
		return nil, err
	}
	out, err := run(ctx, "docker", "image", "ls", "--no-trunc", "--filter", "reference=lidza/*", "--format", "{{.ID}}\t{{.Repository}}\t{{.Tag}}\t{{.CreatedAt}}")
	if err != nil {
		return nil, fmt.Errorf("images could not be listed: %w", err)
	}
	var items []CleanupItem
	for _, row := range lines(out) {
		fields := strings.Split(row, "\t")
		if len(fields) != 4 || fields[2] == "<none>" {
			continue
		}
		id, ref := fields[0], fields[1]+":"+fields[2]
		owner := imageOwner(fields[1], apps)
		created, ok := parseDockerTime(fields[3])
		if owner == "" || !ok || keep[ref] || used[id] || now.Sub(created) < imageMinimumAge {
			continue
		}
		var size int64
		if raw, err := run(ctx, "docker", "image", "inspect", "--format", "{{.Size}}", ref); err == nil {
			size, _ = strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		}
		items = append(items, CleanupItem{Kind: "image", Name: ref, Bytes: size, Reason: "older than " + owner + "'s current and previous release", remove: [][]string{{"docker", "image", "rm", ref}}})
	}
	return items, nil
}

// activeBuilderNodes are the node names of the builder Docker builds with.
func activeBuilderNodes(ctx context.Context, run commandRunner) map[string]bool {
	nodes := map[string]bool{}
	out, err := run(ctx, "docker", "buildx", "inspect")
	if err != nil {
		return nodes
	}
	for _, line := range lines(out) {
		if name, ok := strings.CutPrefix(line, "Name:"); ok {
			nodes[strings.TrimSpace(name)] = true
		}
	}
	return nodes
}

// volumeBytes measures a volume's directory on the host.
func volumeBytes(ctx context.Context, run commandRunner, name string) int64 {
	out, err := run(ctx, "docker", "volume", "inspect", "--format", "{{.Mountpoint}}", name)
	if err != nil || strings.TrimSpace(out) == "" {
		return 0
	}
	size, _ := treeFacts(hostPath(strings.TrimSpace(out)))
	return size
}

// planBuilders lists stopped BuildKit builder containers and their state.
func planBuilders(ctx context.Context, run commandRunner, now time.Time) ([]CleanupItem, error) {
	out, err := run(ctx, "docker", "container", "ls", "--all", "--filter", "name=buildx_buildkit_", "--filter", "status=exited", "--format", "{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("builder containers could not be listed: %w", err)
	}
	names := lines(out)
	if len(names) == 0 {
		return nil, nil
	}
	active := activeBuilderNodes(ctx, run)
	var items []CleanupItem
	for _, name := range names {
		node, ok := strings.CutPrefix(name, "buildx_buildkit_")
		if !ok || node == "" || active[node] {
			continue
		}
		finished, err := run(ctx, "docker", "container", "inspect", "--format", "{{.State.FinishedAt}}", name)
		stopped, ok := parseDockerTime(finished)
		if err != nil || !ok || now.Sub(stopped) < builderStoppedFor {
			continue
		}
		item := CleanupItem{Kind: "builder", Name: name, Reason: "BuildKit builder stopped since " + stopped.UTC().Format("2006-01-02"), remove: [][]string{{"docker", "container", "rm", name}}}
		volume := name + "_state"
		if found, err := run(ctx, "docker", "volume", "ls", "--quiet", "--filter", "name=^"+volume+"$"); err == nil && strings.TrimSpace(found) == volume {
			item.Bytes = volumeBytes(ctx, run, volume)
			item.Name += " and volume " + volume
			item.remove = append(item.remove, []string{"docker", "volume", "rm", volume})
		}
		items = append(items, item)
	}
	return items, nil
}

// planVolumes lists anonymous volumes that no container references.
func planVolumes(ctx context.Context, run commandRunner) ([]CleanupItem, error) {
	out, err := run(ctx, "docker", "volume", "ls", "--quiet", "--filter", "dangling=true")
	if err != nil {
		return nil, fmt.Errorf("volumes could not be listed: %w", err)
	}
	var items []CleanupItem
	for _, name := range lines(out) {
		if !anonymousVolume.MatchString(name) {
			continue // named volumes hold data and are kept
		}
		items = append(items, CleanupItem{Kind: "volume", Name: name, Bytes: volumeBytes(ctx, run, name), Reason: "anonymous volume no container uses", remove: [][]string{{"docker", "volume", "rm", name}}})
	}
	return items, nil
}

// planPackageCache is apt's downloaded package archive.
func planPackageCache() []CleanupItem {
	if ok, _ := aptHost(); !ok {
		return nil
	}
	size, _ := treeFacts(hostPath(aptArchiveDir))
	if size == 0 {
		return nil
	}
	return []CleanupItem{{Kind: "package-cache", Name: aptArchiveDir, Bytes: size, Reason: "downloaded package files apt no longer needs", remove: [][]string{{"apt-get", "clean"}}}}
}

// planDiskHygiene lists what the cleanup removes on this host now.
func (m *Manager) planDiskHygiene(ctx context.Context, run commandRunner, now time.Time, docker dockerAvailability) ([]CleanupItem, []string, []error) {
	if m.cfg.IngressOnly || !hygieneHostRoot() {
		return nil, nil, nil
	}
	var items []CleanupItem
	var failures []error
	if docker.ok && m.dockerManaged() {
		images, err := m.planImages(ctx, run, now)
		if err != nil {
			failures = append(failures, err)
		}
		builders, err := planBuilders(ctx, run, now)
		if err != nil {
			failures = append(failures, err)
		}
		volumes, err := planVolumes(ctx, run)
		if err != nil {
			failures = append(failures, err)
		}
		items = append(append(append(items, images...), builders...), volumes...)
	}
	m.mu.Lock()
	days := m.data.Maintenance.Policy.OlderThanDays
	m.mu.Unlock()
	native, notes := m.planNativeHygiene(now, time.Duration(max(days, 1))*24*time.Hour)
	items = append(items, native...)
	return append(items, planPackageCache()...), notes, failures
}

// dockerAvailability is whether the Docker steps of a cleanup can run.
type dockerAvailability struct {
	ok      bool
	reason  string // why the Docker steps are skipped
	warning bool   // Docker should be running but is not reachable
}

// dockerCleanupAvailability checks for the docker command and a reachable
// daemon. A server without Docker, or with its daemon stopped and disabled
// on purpose, skips the Docker steps; an enabled daemon that does not answer
// is a warning. Tests replace it.
var dockerCleanupAvailability = func(ctx context.Context) dockerAvailability {
	if !hostToolAvailable("docker") {
		return dockerAvailability{reason: "Docker: not installed on this server; skipped"}
	}
	reachable := os.Getenv("DOCKER_HOST") != "" || hostFileExists("/var/run/docker.sock")
	if reachable {
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := hostCommand(bounded, "docker", "version", "--format", "{{.Server.Version}}")
		cancel()
		reachable = err == nil
	}
	if reachable {
		return dockerAvailability{ok: true}
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	enabled, _ := hostCommand(bounded, "systemctl", "is-enabled", "docker.service")
	cancel()
	if state := strings.TrimSpace(enabled); state == "enabled" || state == "enabled-runtime" {
		return dockerAvailability{reason: "Docker: installed and enabled, but the daemon is not reachable; Docker cleanup skipped (check systemctl status docker)", warning: true}
	}
	return dockerAvailability{reason: "Docker: installed, daemon stopped and disabled; skipped"}
}

// applyCleanupItems removes the planned items and reports each one.
func applyCleanupItems(ctx context.Context, run commandRunner, items []CleanupItem) ([]string, []error) {
	var messages []string
	var failures []error
	var freed, toolchainBytes int64
	removed, toolchains := 0, 0
	for _, item := range items {
		var failed error
		if item.apply != nil {
			if err := item.apply(); err != nil {
				failed = fmt.Errorf("%s %s could not be removed: %w", item.Kind, item.Name, err)
			}
		}
		for _, args := range item.remove {
			if failed != nil {
				break
			}
			if _, err := run(ctx, args[0], args[1:]...); err != nil {
				failed = fmt.Errorf("%s %s could not be removed: %w", item.Kind, item.Name, err)
				break
			}
		}
		if failed != nil {
			failures = append(failures, failed)
			continue
		}
		removed++
		freed += item.Bytes
		if item.Kind == "toolchain" {
			toolchains++
			toolchainBytes += item.Bytes
			messages = append(messages, fmt.Sprintf("Removed %s: %s freed", item.Name, bytesText(item.Bytes)))
			continue
		}
		messages = append(messages, fmt.Sprintf("Removed %s %s (%s): %s freed", item.Kind, item.Name, item.Reason, bytesText(item.Bytes)))
	}
	if toolchains > 0 {
		messages = append(messages, toolchainTotals(toolchains, toolchainBytes))
	}
	if removed > 0 {
		messages = append(messages, fmt.Sprintf("Disk hygiene: removed %d item(s), freeing about %s.", removed, bytesText(freed)))
	}
	return messages, failures
}

// cleanupPreview is the dry run of the next cleanup: stale move transfer
// files and the disk hygiene items.
func (m *Manager) cleanupPreview(ctx context.Context) CleanupPreview {
	now := time.Now().UTC()
	preview := CleanupPreview{Checked: now, Items: []CleanupItem{}}
	m.mu.Lock()
	runner := m.maintenanceRun
	m.mu.Unlock()
	if runner == nil {
		runner = maintenanceCommand
	}
	sweep := m.sweepMoveTemps(now, true)
	for i, path := range sweep.Removed {
		preview.Items = append(preview.Items, CleanupItem{Kind: "move-temp", Name: path, Bytes: sweep.Sizes[i], Reason: "left by an interrupted or expired move transfer"})
	}
	docker := dockerCleanupAvailability(ctx)
	if !docker.ok {
		preview.Notes = append(preview.Notes, docker.reason)
	}
	items, notes, failures := m.planDiskHygiene(ctx, runner, now, docker)
	preview.Items = append(preview.Items, items...)
	preview.Notes = append(preview.Notes, notes...)
	for _, err := range failures {
		preview.Notes = append(preview.Notes, err.Error())
	}
	for _, item := range preview.Items {
		preview.Bytes += item.Bytes
	}
	return preview
}
