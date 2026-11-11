package agent

// The systemd journal size policy. The agent owns how much disk the
// journal may use, sized to the filesystem that holds it, because a large
// journal makes every journalctl read slow: journalctl opens and indexes
// every journal file whatever the match, so a 20 GiB journal in 900 files
// costs about a second of CPU per read.
//
// With D the size of the journal's filesystem:
//
//	SystemMaxUse      = 1% of D rounded up to a whole GiB, within [1, 4] GiB
//	                    (the panel's "System log size" override replaces it)
//	SystemKeepFree    = max(5% of D, 2 GiB), the agent's disk reserve
//	SystemMaxFileSize = SystemMaxUse/8, so the journal is a few large files
//	MaxRetentionSec   = 30day
//
// The policy is written to /etc/systemd/journald.conf.d/zz-lidza-journal.conf.
// journald reads its drop-ins in lexical order of their file names and a
// later assignment wins, so the agent's file sorts after the usual
// "NN-name.conf" drop-ins. A foreign drop-in that sets one of these keys
// would still win when it sorts later, and would mislead an operator reading
// it either way, so the agent backs it up into its state directory and
// comments out only the conflicting keys; the file and its other settings
// stay. What was changed is recorded and reported in the server's health
// notes.
//
// The policy is recomputed at agent start, daily and when the override
// changes. Only when the drop-in or a foreign file changed does the agent
// restart systemd-journald (journald reads its configuration only at start;
// SIGHUP rotates and, on newer systemd, reloads, so a restart is the portable
// way to apply size limits) and run `journalctl --vacuum-size` once so the
// journal shrinks at once rather than at the next rotation. A journald
// restart keeps services' log streams (journald stores them in its file
// descriptor store); a `journalctl --follow` reader that ends is restarted
// by its owner from its cursor (journal_follow.go).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	journaldDropInDir     = "/etc/systemd/journald.conf.d"
	journaldDropInName    = "zz-lidza-journal.conf"
	journalPolicyInterval = 24 * time.Hour
	journalPolicyJobKind  = "journal-policy"
	journalRetention      = "30day"
	journalMaxUseFloor    = 1 << 30
	journalMaxUseCeiling  = 4 << 30
	// The override's bounds, in MiB.
	journalOverrideMinMiB = 256
	journalOverrideMaxMiB = 64 << 10
)

// journalOwnedKeys are the [Journal] keys the agent's drop-in sets; foreign
// drop-ins that set them are neutralised.
var journalOwnedKeys = []string{"SystemMaxUse", "SystemKeepFree", "SystemMaxFileSize", "MaxRetentionSec"}

// JournalPolicy is the journal size configuration the agent writes.
type JournalPolicy struct {
	MaxUseBytes   uint64 `json:"max_use_bytes"`
	KeepFreeBytes uint64 `json:"keep_free_bytes"`
	MaxFileBytes  uint64 `json:"max_file_bytes"`
	Retention     string `json:"retention"`
}

// JournalNeutralized is a foreign drop-in whose conflicting keys the agent
// commented out.
type JournalNeutralized struct {
	File   string    `json:"file"`
	Lines  []string  `json:"lines"` // the assignments as they were, e.g. "SystemMaxUse=100G"
	Backup string    `json:"backup"`
	At     time.Time `json:"at"`
}

// JournalPolicyState is the persisted journal policy, shown in the panel's
// Resources section.
type JournalPolicyState struct {
	DiskPath  string        `json:"disk_path,omitempty"`
	DiskBytes uint64        `json:"disk_bytes,omitempty"`
	Computed  JournalPolicy `json:"computed"`
	// OverrideMiB is the override the target was computed with (0: none).
	OverrideMiB int                  `json:"override_mib,omitempty"`
	Target      JournalPolicy        `json:"target"`
	DropIn      string               `json:"drop_in"`
	UsageBytes  uint64               `json:"usage_bytes,omitempty"`
	Neutralized []JournalNeutralized `json:"neutralized,omitempty"`
	LastRun     time.Time            `json:"last_run,omitzero"`
	LastApplied time.Time            `json:"last_applied,omitzero"`
	// RestartPending marks files written whose journald restart failed.
	RestartPending bool   `json:"restart_pending,omitempty"`
	Error          string `json:"error,omitempty"`
}

// computeJournalPolicy sizes the journal for a filesystem of total bytes;
// overrideMiB > 0 replaces the computed SystemMaxUse.
func computeJournalPolicy(total uint64, overrideMiB int) JournalPolicy {
	use := (total/100 + (1<<30 - 1)) &^ (1<<30 - 1) // 1%, rounded up to a GiB
	use = max(journalMaxUseFloor, min(journalMaxUseCeiling, use))
	if overrideMiB > 0 {
		use = uint64(overrideMiB) << 20
	}
	keep := diskReserve(DiskHealth{Total: total}) &^ (1<<20 - 1)
	return JournalPolicy{MaxUseBytes: use, KeepFreeBytes: keep, MaxFileBytes: (use / 8) &^ (1<<20 - 1), Retention: journalRetention}
}

// journaldSize renders bytes in journald's units (base 1024).
func journaldSize(bytes uint64) string {
	switch {
	case bytes >= 1<<30 && bytes%(1<<30) == 0:
		return strconv.FormatUint(bytes>>30, 10) + "G"
	case bytes >= 1<<20:
		return strconv.FormatUint(bytes>>20, 10) + "M"
	}
	return strconv.FormatUint(bytes>>10, 10) + "K"
}

func renderJournaldDropIn(p JournalPolicy, diskBytes uint64, overrideMiB int) string {
	var b strings.Builder
	b.WriteString("# Managed by Līdza Deploy (lidza-agent): the systemd journal size policy.\n")
	b.WriteString("# Changes are overwritten; set \"System log size\" in the server's Resources\n")
	b.WriteString("# section of the panel. This file sorts after other drop-ins so it wins.\n")
	if overrideMiB > 0 {
		fmt.Fprintf(&b, "# SystemMaxUse is the operator's override; the disk is %s.\n", moveHumanBytes(diskBytes))
	} else {
		fmt.Fprintf(&b, "# Sized for a %s disk: 1%% within 1–4 GiB, keep 5%% (at least 2 GiB) free.\n", moveHumanBytes(diskBytes))
	}
	b.WriteString("[Journal]\n")
	fmt.Fprintf(&b, "SystemMaxUse=%s\n", journaldSize(p.MaxUseBytes))
	fmt.Fprintf(&b, "SystemKeepFree=%s\n", journaldSize(p.KeepFreeBytes))
	fmt.Fprintf(&b, "SystemMaxFileSize=%s\n", journaldSize(p.MaxFileBytes))
	fmt.Fprintf(&b, "MaxRetentionSec=%s\n", p.Retention)
	return b.String()
}

var journalOwnedLine = regexp.MustCompile(`^\s*(` + strings.Join(journalOwnedKeys, "|") + `)\s*=`)

// neutralizeJournalKeys comments out the agent-owned assignments of the
// [Journal] section in a foreign drop-in. It returns the new content and
// the assignments it disabled (none: content unchanged).
func neutralizeJournalKeys(content string) (string, []string) {
	lines := strings.SplitAfter(content, "\n")
	section := ""
	var disabled []string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			continue
		}
		if section != "Journal" || !journalOwnedLine.MatchString(line) {
			continue
		}
		disabled = append(disabled, trimmed)
		newline := ""
		if strings.HasSuffix(line, "\n") {
			newline = "\n"
		}
		lines[i] = "# " + trimmed + "  # disabled by Līdza Deploy: the journal size is set in " + journaldDropInName + newline
	}
	return strings.Join(lines, ""), disabled
}

// journalPolicyController is runtime-only coordination of the policy.
type journalPolicyController struct {
	mu      sync.Mutex
	running bool
	booted  bool
}

var journalPolicyControllers sync.Map // *Manager -> *journalPolicyController

func (m *Manager) journalPolicy() *journalPolicyController {
	value, _ := journalPolicyControllers.LoadOrStore(m, &journalPolicyController{})
	return value.(*journalPolicyController)
}

// journalPolicyTick runs from hostTick once a minute: at agent start,
// daily, and when the panel's override changed.
func (m *Manager) journalPolicyTick(now time.Time) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || !hostFileExists("/run/systemd/system") || !hostToolAvailable("journalctl") {
		return
	}
	m.mu.Lock()
	state := m.tuningStateLocked()
	override := state.Overrides.JournalMaxUseMiB
	var lastRun time.Time
	applied := -1
	if state.Journal != nil {
		lastRun, applied = state.Journal.LastRun, state.Journal.OverrideMiB
	}
	m.mu.Unlock()
	jc := m.journalPolicy()
	jc.mu.Lock()
	trigger := ""
	switch {
	case !jc.booted:
		trigger = "boot"
	case now.Sub(lastRun) >= journalPolicyInterval:
		trigger = "schedule"
	case applied != override:
		trigger = "override"
	}
	if trigger == "" || jc.running {
		jc.mu.Unlock()
		return
	}
	jc.running, jc.booted = true, true
	jc.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() { jc.mu.Lock(); jc.running = false; jc.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
		defer cancel()
		m.applyJournalPolicy(ctx, trigger, now)
	}()
}

// journalDiskPath is the directory whose filesystem holds the journal:
// the persistent journal when it exists, else /var/log.
func journalDiskPath() string {
	if hostFileExists("/var/log/journal") {
		return "/var/log/journal"
	}
	return "/var/log"
}

// applyJournalPolicy computes the policy, writes the drop-in, neutralises
// foreign keys and, when anything changed, restarts journald and vacuums
// the journal once. The state is saved either way.
func (m *Manager) applyJournalPolicy(ctx context.Context, trigger string, now time.Time) {
	m.mu.Lock()
	override := m.tuningStateLocked().Overrides.JournalMaxUseMiB
	previous := m.tuningStateLocked().Journal
	m.mu.Unlock()
	next := JournalPolicyState{DropIn: filepath.Join(journaldDropInDir, journaldDropInName), LastRun: now.UTC(), OverrideMiB: override}
	if previous != nil {
		next.Neutralized, next.LastApplied = slices.Clone(previous.Neutralized), previous.LastApplied
	}
	var transcript strings.Builder
	changed, err := m.writeJournalPolicy(&next, now, &transcript)
	// A restart that failed earlier is retried although the files match.
	if err == nil && (changed || previous != nil && previous.RestartPending) {
		changed = true
		if err = applyJournaldChange(ctx, next.Target, &transcript); err == nil {
			next.LastApplied = now.UTC()
		}
	}
	next.RestartPending = changed && err != nil
	if out, usageErr := hostCommand(ctx, "journalctl", "--disk-usage"); usageErr == nil {
		next.UsageBytes = parseJournalUsage(out)
	}
	if err != nil {
		next.Error = err.Error()
		log.Printf("journal policy (%s): %v", trigger, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tuningStateLocked().Journal = &next
	if changed || err != nil {
		job := newHostJob(journalPolicyJobKind, trigger, now)
		finished := time.Now().UTC()
		job.Finished = &finished
		job.Output = strings.TrimSpace(transcript.String())
		job.Status, job.Summary = "succeeded", "System log size set to "+journaldSize(next.Target.MaxUseBytes)+"."
		if err != nil {
			job.Status, job.Summary = "failed", "System log size: "+err.Error()
		}
		m.recordHostJobLocked(job)
		log.Printf("journal policy (%s): %s", trigger, job.Summary)
	}
	if saveErr := m.save(); saveErr != nil {
		log.Printf("journal policy state: %v", saveErr)
	}
}

// writeJournalPolicy fills next's sizing and writes the agent's drop-in and
// the neutralised foreign drop-ins; changed reports a file write.
func (m *Manager) writeJournalPolicy(next *JournalPolicyState, now time.Time, transcript *strings.Builder) (bool, error) {
	next.DiskPath = journalDiskPath()
	disk, err := diskStat(hostPath(next.DiskPath))
	if err != nil || disk.Total == 0 {
		return false, fmt.Errorf("the journal's disk could not be measured: %v", err)
	}
	next.DiskBytes = disk.Total
	next.Computed = computeJournalPolicy(disk.Total, 0)
	next.Target = computeJournalPolicy(disk.Total, next.OverrideMiB)
	changed := false
	dir := hostPath(journaldDropInDir)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == journaldDropInName || !strings.HasSuffix(name, ".conf") || entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, name)
		// Only regular files: the agent never rewrites through a link.
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return changed, err
		}
		content, disabled := neutralizeJournalKeys(string(raw))
		if len(disabled) == 0 {
			continue
		}
		backup, err := m.backupJournalDropIn(name, raw, now)
		if err != nil {
			return changed, fmt.Errorf("backing up %s: %w", name, err)
		}
		if err := writeHostFile(path, []byte(content), info.Mode().Perm()); err != nil {
			return changed, err
		}
		changed = true
		file := filepath.Join(journaldDropInDir, name)
		fmt.Fprintf(transcript, "Commented out %s in %s (backup: %s).\n", strings.Join(disabled, ", "), file, backup)
		record := JournalNeutralized{File: file, Lines: disabled, Backup: backup, At: now.UTC()}
		next.Neutralized = slices.DeleteFunc(next.Neutralized, func(n JournalNeutralized) bool { return n.File == file })
		next.Neutralized = append(next.Neutralized, record)
	}
	content := renderJournaldDropIn(next.Target, disk.Total, next.OverrideMiB)
	path := hostPath(next.DropIn)
	if old, err := os.ReadFile(path); err != nil || string(old) != content {
		if err := writeHostFile(path, []byte(content), 0o644); err != nil {
			return changed, err
		}
		changed = true
		fmt.Fprintf(transcript, "Wrote %s: SystemMaxUse=%s SystemKeepFree=%s SystemMaxFileSize=%s MaxRetentionSec=%s.\n", next.DropIn, journaldSize(next.Target.MaxUseBytes), journaldSize(next.Target.KeepFreeBytes), journaldSize(next.Target.MaxFileBytes), next.Target.Retention)
	}
	return changed, nil
}

// backupJournalDropIn keeps a foreign drop-in's original content in the
// agent's state directory before it is changed.
func (m *Manager) backupJournalDropIn(name string, raw []byte, now time.Time) (string, error) {
	dir := filepath.Join(m.cfg.DataDir, "journald-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+"."+now.UTC().Format("20060102T150405Z"))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// applyJournaldChange restarts journald so it reads the new limits and
// vacuums the journal down to SystemMaxUse once.
func applyJournaldChange(ctx context.Context, p JournalPolicy, transcript *strings.Builder) error {
	run := func(name string, args ...string) error {
		out, err := hostCommand(ctx, name, args...)
		transcript.WriteString("$ " + name + " " + strings.Join(args, " ") + "\n")
		if out = strings.TrimSpace(out); out != "" {
			transcript.WriteString(boundedErrorText(out, 4000) + "\n")
		}
		if err != nil {
			return fmt.Errorf("%s %s failed: %w", name, args[0], err)
		}
		return nil
	}
	if err := run("systemctl", "restart", "systemd-journald.service"); err != nil {
		return err
	}
	if err := run("journalctl", "--vacuum-size="+journaldSize(p.MaxUseBytes)); err != nil {
		return errors.New("the journal limits apply, but vacuuming failed: " + err.Error())
	}
	return nil
}

var journalUsageSize = regexp.MustCompile(`take up ([0-9.]+)\s*([KMGTP]?)`)

// parseJournalUsage reads `journalctl --disk-usage` ("Archived and active
// journals take up 21.9G in the file system.").
func parseJournalUsage(out string) uint64 {
	match := journalUsageSize.FindStringSubmatch(out)
	if match == nil {
		return 0
	}
	value, _ := strconv.ParseFloat(match[1], 64)
	unit := map[string]float64{"": 1, "K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40, "P": 1 << 50}[match[2]]
	return uint64(value * unit)
}

// journalPolicyHealth reports the policy's failures as warnings and the
// foreign drop-ins it changed as notes.
func (m *Manager) journalPolicyHealth() (warnings, notes []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.ResourceTuning == nil || m.data.ResourceTuning.Journal == nil {
		return nil, nil
	}
	j := m.data.ResourceTuning.Journal
	if j.Error != "" {
		warnings = append(warnings, "System log size policy: "+j.Error)
	}
	for _, n := range j.Neutralized {
		notes = append(notes, fmt.Sprintf("The agent owns the systemd journal size (%s): it commented out %s in %s; the original is kept at %s.", j.DropIn, strings.Join(n.Lines, ", "), n.File, n.Backup))
	}
	return warnings, notes
}
