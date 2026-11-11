package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestJournalPolicySizing(t *testing.T) {
	const gib = uint64(1) << 30
	for _, tc := range []struct {
		name               string
		disk               uint64
		override           int
		use, keep, maxFile string
	}{
		{"small disk keeps the 1 GiB floor and the 2 GiB reserve", 20 * gib, 0, "1G", "2G", "128M"},
		{"100 GB rounds 1% up to a GiB", 100_000_000_000, 0, "1G", "4768M", "128M"},
		{"250 GiB", 250 * gib, 0, "3G", "12800M", "384M"},
		{"the 386 GB production host", 386_000_000_000, 0, "4G", "18405M", "512M"},
		{"2 TB stays at the 4 GiB ceiling", 2_000_000_000_000, 0, "4G", "95367M", "512M"},
		{"override replaces SystemMaxUse", 386_000_000_000, 2048, "2G", "18405M", "256M"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := computeJournalPolicy(tc.disk, tc.override)
			if got := []string{journaldSize(p.MaxUseBytes), journaldSize(p.KeepFreeBytes), journaldSize(p.MaxFileBytes), p.Retention}; !slices.Equal(got, []string{tc.use, tc.keep, tc.maxFile, "30day"}) {
				t.Fatalf("policy = %v, want %s %s %s 30day", got, tc.use, tc.keep, tc.maxFile)
			}
		})
	}
}

func TestJournalDropInSortsAfterForeignDropIns(t *testing.T) {
	names := []string{journaldDropInName, "99-zz-retention-30d.conf", "50-retention.conf", "99-zzz.conf", "10-forward.conf"}
	sort.Strings(names)
	if names[len(names)-1] != journaldDropInName {
		t.Fatal("journald applies drop-ins in lexical order; the agent's must be last", names)
	}
}

func TestJournalOverrideValidation(t *testing.T) {
	if err := (TuningOverrides{JournalMaxUseMiB: 100}).Validate(); err == nil {
		t.Fatal("a 100 MiB journal must be refused")
	}
	if err := (TuningOverrides{JournalMaxUseMiB: 2048}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// journalHost is a fake host: an etc tree under root, a fixed disk and a
// recording systemctl/journalctl.
type journalHost struct {
	root       string
	calls      []string
	restartErr error
}

func newJournalHost(t *testing.T, disk uint64) *journalHost {
	t.Helper()
	h := &journalHost{root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(h.root, "var/log/journal"), 0o755); err != nil {
		t.Fatal(err)
	}
	previousPath, previousCommand, previousStat := hostPath, hostCommand, diskStat
	hostPath = func(path string) string { return filepath.Join(h.root, path) }
	hostCommand = func(_ context.Context, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		if call == "journalctl --disk-usage" {
			return "Archived and active journals take up 21.9G in the file system.\n", nil
		}
		h.calls = append(h.calls, call)
		if name == "systemctl" {
			return "", h.restartErr
		}
		return "Vacuuming done, freed 17.9G of archived journals from /var/log/journal/abc.\n", nil
	}
	diskStat = func(path string) (DiskHealth, error) {
		if path != filepath.Join(h.root, "var/log/journal") {
			return DiskHealth{}, errors.New("unexpected path " + path)
		}
		return DiskHealth{Path: path, Total: disk, Available: disk / 2}, nil
	}
	t.Cleanup(func() { hostPath, hostCommand, diskStat = previousPath, previousCommand, previousStat })
	return h
}

func (h *journalHost) write(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(h.root, journaldDropInDir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *journalHost) read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.root, journaldDropInDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (h *journalHost) take() []string {
	calls := h.calls
	h.calls = nil
	return calls
}

const (
	retention50 = "[Journal]\nSystemMaxUse=20G\nMaxRetentionSec=1month\n"
	retention99 = "# operator: keep a month\n[Journal]\nSystemMaxUse=100G\nSystemKeepFree=50G\nSystemMaxFiles=2000\nMaxRetentionSec=30day\n"
	forward     = "[Journal]\nForwardToSyslog=no\n"
)

func TestJournalPolicyNeutralizesForeignDropInsAndShrinksOnce(t *testing.T) {
	h := newJournalHost(t, 386_000_000_000)
	h.write(t, "50-retention.conf", retention50)
	h.write(t, "99-zz-retention-30d.conf", retention99)
	h.write(t, "10-forward.conf", forward)
	m := testManager(t, &fakeRuntime{})
	now := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)

	m.applyJournalPolicy(context.Background(), "boot", now)
	if calls := h.take(); !slices.Equal(calls, []string{"systemctl restart systemd-journald.service", "journalctl --vacuum-size=4G"}) {
		t.Fatal("journald was not restarted and vacuumed exactly once", calls)
	}
	own := h.read(t, journaldDropInName)
	for _, want := range []string{"Managed by Līdza Deploy (lidza-agent)", "[Journal]\n", "SystemMaxUse=4G\n", "SystemKeepFree=18405M\n", "SystemMaxFileSize=512M\n", "MaxRetentionSec=30day\n"} {
		if !strings.Contains(own, want) {
			t.Fatalf("drop-in lacks %q:\n%s", want, own)
		}
	}
	got99 := h.read(t, "99-zz-retention-30d.conf")
	for _, want := range []string{"# operator: keep a month\n", "[Journal]\n", "\nSystemMaxFiles=2000\n", "# SystemMaxUse=100G  # disabled by Līdza Deploy", "# SystemKeepFree=50G  # disabled", "# MaxRetentionSec=30day  # disabled"} {
		if !strings.Contains(got99, want) {
			t.Fatalf("99-zz-retention-30d.conf lacks %q:\n%s", want, got99)
		}
	}
	for _, line := range strings.Split(got99+h.read(t, "50-retention.conf"), "\n") {
		if journalOwnedLine.MatchString(line) {
			t.Fatal("a conflicting key is still active:", line)
		}
	}
	if h.read(t, "10-forward.conf") != forward {
		t.Fatal("a drop-in without conflicting keys was changed")
	}
	view := m.resourceTuningView(context.Background(), false)
	j := view.Journal
	if j == nil || len(j.Neutralized) != 2 || j.UsageBytes < 21<<30 || j.Computed.MaxUseBytes != 4<<30 || j.Error != "" {
		t.Fatalf("view = %+v", j)
	}
	for _, n := range j.Neutralized {
		original := map[string]string{"/etc/systemd/journald.conf.d/50-retention.conf": retention50, "/etc/systemd/journald.conf.d/99-zz-retention-30d.conf": retention99}[n.File]
		if raw, err := os.ReadFile(n.Backup); err != nil || string(raw) != original || !strings.HasPrefix(n.Backup, m.cfg.DataDir) {
			t.Fatalf("backup of %s at %s: %q, %v", n.File, n.Backup, raw, err)
		}
	}
	if len(view.Jobs) != 1 || view.Jobs[0].Kind != journalPolicyJobKind || view.Jobs[0].Status != "succeeded" {
		t.Fatal("the change was not recorded as a job", view.Jobs)
	}
	_, notes := m.journalPolicyHealth()
	if len(notes) != 2 || !strings.Contains(strings.Join(notes, "\n"), "SystemMaxUse=100G, SystemKeepFree=50G, MaxRetentionSec=30day in /etc/systemd/journald.conf.d/99-zz-retention-30d.conf") {
		t.Fatal("health notes do not report the neutralised keys", notes)
	}

	// Idempotent: nothing changed, so no restart, vacuum or backup.
	backups, _ := os.ReadDir(filepath.Join(m.cfg.DataDir, "journald-backups"))
	m.applyJournalPolicy(context.Background(), "schedule", now.Add(25*time.Hour))
	if calls := h.take(); len(calls) != 0 {
		t.Fatal("an unchanged policy restarted journald", calls)
	}
	again, _ := os.ReadDir(filepath.Join(m.cfg.DataDir, "journald-backups"))
	if len(again) != len(backups) || len(m.resourceTuningView(context.Background(), false).Jobs) != 1 {
		t.Fatal("an unchanged policy made backups or jobs")
	}

	// The panel's override is honoured.
	if err := m.setTuningOverrides(TuningOverrides{JournalMaxUseMiB: 2048}); err != nil {
		t.Fatal(err)
	}
	m.applyJournalPolicy(context.Background(), "override", now.Add(26*time.Hour))
	if calls := h.take(); !slices.Equal(calls, []string{"systemctl restart systemd-journald.service", "journalctl --vacuum-size=2G"}) {
		t.Fatal("the override was not applied", calls)
	}
	if own := h.read(t, journaldDropInName); !strings.Contains(own, "SystemMaxUse=2G\n") || !strings.Contains(own, "SystemMaxFileSize=256M\n") || !strings.Contains(own, "override") {
		t.Fatal("drop-in ignores the override:\n" + own)
	}
	if j := m.resourceTuningView(context.Background(), false).Journal; j.OverrideMiB != 2048 || j.Target.MaxUseBytes != 2<<30 || j.Computed.MaxUseBytes != 4<<30 {
		t.Fatalf("view after override = %+v", j)
	}
}

func TestJournalPolicyRetriesAFailedRestart(t *testing.T) {
	h := newJournalHost(t, 50<<30)
	h.restartErr = errors.New("exit status 1")
	m := testManager(t, &fakeRuntime{})
	now := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	m.applyJournalPolicy(context.Background(), "boot", now)
	if calls := h.take(); !slices.Equal(calls, []string{"systemctl restart systemd-journald.service"}) {
		t.Fatal("a failed restart must not vacuum", calls)
	}
	if warnings, _ := m.journalPolicyHealth(); len(warnings) != 1 {
		t.Fatal("the failure is not a health warning", warnings)
	}
	h.restartErr = nil
	m.applyJournalPolicy(context.Background(), "schedule", now.Add(25*time.Hour))
	if calls := h.take(); !slices.Equal(calls, []string{"systemctl restart systemd-journald.service", "journalctl --vacuum-size=1G"}) {
		t.Fatal("the restart was not retried although the files already match", calls)
	}
	if warnings, _ := m.journalPolicyHealth(); len(warnings) != 0 {
		t.Fatal("the warning outlived the fix", warnings)
	}
	m.applyJournalPolicy(context.Background(), "schedule", now.Add(50*time.Hour))
	if calls := h.take(); len(calls) != 0 {
		t.Fatal("restarted again after success", calls)
	}
}

func TestNeutralizeJournalKeysOnlyInJournalSection(t *testing.T) {
	content, disabled := neutralizeJournalKeys("SystemMaxUse=1G\n[Other]\nSystemMaxUse=2G\n[Journal]\n  SystemMaxFileSize = 9G\nRuntimeMaxUse=1G\n; SystemMaxUse=3G\n")
	if !slices.Equal(disabled, []string{"SystemMaxFileSize = 9G"}) {
		t.Fatal("disabled", disabled)
	}
	if !strings.Contains(content, "SystemMaxUse=1G\n[Other]\nSystemMaxUse=2G\n") || !strings.Contains(content, "RuntimeMaxUse=1G\n; SystemMaxUse=3G\n") {
		t.Fatal("unrelated lines changed:\n" + content)
	}
}
