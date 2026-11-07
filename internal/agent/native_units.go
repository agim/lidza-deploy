package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/agim/lidza-deploy/internal/release"
)

// Hardened system units for applications with their own account. Each
// process of an application is one unit, lidza-app-<id>-<process>.service,
// that runs as app-<id> from <root>/current with the application's
// environment file. The whole file system is read-only to it except
// <root>/shared; it has no capabilities, devices, home directories or
// kernel tunables, and its memory and CPU are capped.

// nativeSystemUnitDir is where system units are written; tests replace it.
var nativeSystemUnitDir = "/etc/systemd/system"

const nativeDefaultMemoryMiB = 1024

var nativeProcessNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)

// nativeUnitSpec is everything one rendered unit contains.
type nativeUnitSpec struct {
	App        string // application id
	Process    string // process name (unit suffix)
	User       string // account and group
	Root       string // application root
	Command    string // run with /bin/sh -c from <root>/current
	MemoryMiB  int    // 0 = default
	CPUPercent int    // 0 = no quota
	// Ruby keeps gems installed outside Bundler in the application's own
	// GEM_HOME (Rails applications).
	Ruby bool
}

// nativeUnitName is the system unit of one application process.
func nativeUnitName(app, process string) string {
	return "lidza-app-" + app + "-" + process + ".service"
}

func validNativeCommand(command string) error {
	if strings.TrimSpace(command) == "" || len(command) > 1000 {
		return errors.New("process commands must contain between 1 and 1000 characters")
	}
	for _, r := range command {
		if unicode.IsControl(r) {
			return errors.New("process commands cannot contain line breaks or control characters")
		}
	}
	return nil
}

// systemdQuote quotes one ExecStart argument: systemd unescapes C-style
// backslashes inside double quotes and expands $ and % unless doubled.
func systemdQuote(value string) string {
	value = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`).Replace(value)
	return `"` + value + `"`
}

// renderNativeUnit renders one hardened system unit. It is pure: the
// result depends only on spec.
func renderNativeUnit(spec nativeUnitSpec) (string, error) {
	switch {
	case !idPattern.MatchString(spec.App) || !nativeProcessNamePattern.MatchString(spec.Process):
		return "", errors.New("unit needs a valid application id and process name")
	case !nativeAccountPattern.MatchString(spec.User) || spec.User == "root":
		return "", errors.New("unit needs a non-root account")
	case !filepath.IsAbs(spec.Root) || filepath.Clean(spec.Root) != spec.Root || strings.ContainsAny(spec.Root, " \t\"'\\$%;"):
		return "", errors.New("unit needs a clean absolute application root")
	case spec.MemoryMiB < 0 || spec.MemoryMiB > 1<<20 || spec.CPUPercent < 0 || spec.CPUPercent > 6400:
		return "", errors.New("unit resource limits are out of range")
	}
	if err := validNativeCommand(spec.Command); err != nil {
		return "", err
	}
	memory := spec.MemoryMiB
	if memory == 0 {
		memory = nativeDefaultMemoryMiB
	}
	current := filepath.Join(spec.Root, "current")
	shared := filepath.Join(spec.Root, "shared")
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("# Managed by Līdza Deploy; changes are replaced.")
	line("[Unit]")
	line("Description=%s %s", spec.App, spec.Process)
	line("After=network-online.target")
	line("Wants=network-online.target")
	// Until the first release is published there is nothing to run.
	line("ConditionPathExists=%s", current)
	line("")
	line("[Service]")
	line("Type=simple")
	line("User=%s", spec.User)
	line("Group=%s", spec.User)
	line("WorkingDirectory=%s", current)
	line("Environment=PATH=%s", nativeManagedRuntimePath(spec.Root))
	line("Environment=HOME=%s", spec.Root)
	// The home directory is read-only to the process; caches go to its
	// private /tmp.
	line("Environment=XDG_CACHE_HOME=/tmp/.cache npm_config_cache=/tmp/.npm")
	if spec.Ruby {
		gems := nativeManagedGemEnvironment(spec.Root)
		line("Environment=GEM_HOME=%s GEM_PATH=%s", gems["GEM_HOME"], gems["GEM_PATH"])
	}
	// The release's own toolchain settings (for example BUNDLER_VERSION),
	// read through current so they follow rollbacks; the application's
	// environment, read after it, can override them.
	line("EnvironmentFile=-%s", filepath.Join(current, release.RuntimeEnvFile))
	line("EnvironmentFile=%s", filepath.Join(shared, ".env"))
	line("ExecStart=/bin/sh -c %s", systemdQuote("exec "+spec.Command))
	line("Restart=on-failure")
	line("RestartSec=5")
	line("TimeoutStopSec=30")
	line("KillMode=mixed")
	line("SyslogIdentifier=%s", strings.TrimSuffix(nativeUnitName(spec.App, spec.Process), ".service"))
	line("UMask=0027")
	line("NoNewPrivileges=yes")
	line("ProtectSystem=strict")
	line("ProtectHome=yes")
	line("ReadWritePaths=%s", shared)
	line("PrivateTmp=yes")
	line("PrivateDevices=yes")
	line("ProtectKernelTunables=yes")
	line("ProtectKernelModules=yes")
	line("ProtectKernelLogs=yes")
	line("ProtectControlGroups=yes")
	line("ProtectClock=yes")
	line("ProtectHostname=yes")
	line("RestrictSUIDSGID=yes")
	line("RestrictRealtime=yes")
	line("RestrictNamespaces=yes")
	line("LockPersonality=yes")
	line("RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK")
	line("CapabilityBoundingSet=")
	line("AmbientCapabilities=")
	line("MemoryMax=%dM", memory)
	line("MemorySwapMax=0")
	if spec.CPUPercent > 0 {
		line("CPUQuota=%d%%", spec.CPUPercent)
	}
	line("TasksMax=512")
	line("")
	line("[Install]")
	line("WantedBy=multi-user.target")
	return b.String(), nil
}

// nativeRootExec runs a host command as the root agent: systemctl,
// journalctl and account tools. Tests route it through nativeCommand.
func (m *Manager) nativeRootExec(ctx context.Context, args ...string) (string, error) {
	if m.nativeCommand != nil {
		return m.nativeCommand(ctx, args)
	}
	if len(args) == 0 {
		return "", errors.New("empty host command")
	}
	if os.Geteuid() != 0 {
		return "", errors.New("this operation requires the root agent")
	}
	return command(ctx, "", []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}, args[0], args[1:]...)
}

// writeNativeUnits writes the units of one application atomically and
// reloads systemd. Units are enabled but not started: their condition
// keeps them idle until the first release is published.
func (m *Manager) writeNativeUnits(ctx context.Context, specs []nativeUnitSpec) ([]string, error) {
	units := make([]string, 0, len(specs))
	for _, spec := range specs {
		body, err := renderNativeUnit(spec)
		if err != nil {
			return units, err
		}
		unit := nativeUnitName(spec.App, spec.Process)
		path := filepath.Join(nativeSystemUnitDir, unit)
		temporary := path + ".tmp"
		if err := os.WriteFile(temporary, []byte(body), 0o644); err != nil {
			return units, err
		}
		if err := os.Chmod(temporary, 0o644); err != nil {
			os.Remove(temporary)
			return units, err
		}
		if err := os.Rename(temporary, path); err != nil {
			os.Remove(temporary)
			return units, err
		}
		units = append(units, unit)
	}
	if _, err := m.nativeRootExec(ctx, "systemctl", "daemon-reload"); err != nil {
		return units, errors.New("systemd could not reload the application units")
	}
	if len(units) > 0 {
		if _, err := m.nativeRootExec(ctx, append([]string{"systemctl", "enable", "--quiet"}, units...)...); err != nil {
			return units, errors.New("systemd could not enable the application units")
		}
	}
	return units, nil
}

// removeNativeUnits stops, disables and deletes units.
func (m *Manager) removeNativeUnits(ctx context.Context, units []string) error {
	var failed error
	for _, unit := range units {
		if !nativeReleaseUnitPattern.MatchString(unit) || !strings.HasPrefix(unit, "lidza-app-") {
			continue
		}
		_, _ = m.nativeRootExec(ctx, "systemctl", "disable", "--now", "--quiet", unit)
		if err := os.Remove(filepath.Join(nativeSystemUnitDir, unit)); err != nil && !errors.Is(err, os.ErrNotExist) {
			failed = err
		}
	}
	if _, err := m.nativeRootExec(ctx, "systemctl", "daemon-reload"); err != nil && failed == nil {
		failed = errors.New("systemd could not reload after removing the application units")
	}
	// A removed unit that had failed stays listed as "not-found failed"
	// until its state is reset; a unit that is not loaded fails harmlessly.
	for _, unit := range units {
		if nativeReleaseUnitPattern.MatchString(unit) && strings.HasPrefix(unit, "lidza-app-") {
			_, _ = m.nativeRootExec(ctx, "systemctl", "reset-failed", unit)
		}
	}
	return failed
}

// nativeSystemServiceMetadata reads the state of system units in the same
// key=value form as the user-unit query.
func (m *Manager) nativeSystemServiceMetadata(ctx context.Context, units []string) (string, error) {
	args := []string{"systemctl", "show", "--property=Id,ActiveState,SubState,UnitFileState", "--no-pager"}
	for _, unit := range units {
		if !nativeUnitPattern.MatchString(unit) {
			return "", errors.New("native metadata services contain an invalid identity")
		}
		args = append(args, unit)
	}
	return m.nativeRootExec(ctx, args...)
}

func (m *Manager) nativeSystemServiceJournal(ctx context.Context, units ...string) (string, error) {
	args := []string{"journalctl"}
	for _, unit := range units {
		if !nativeUnitPattern.MatchString(unit) {
			return "", errors.New("invalid unit")
		}
		args = append(args, "--unit="+unit)
	}
	return m.nativeRootExec(ctx, append(args, nativeJournalFormat(units)...)...)
}

// nativeSystemServiceAction starts, stops or restarts a system unit. Start
// and stop also enable and disable it, so a later deploy keeps the
// operator's choice: deploys start enabled units and leave disabled ones.
func (m *Manager) nativeSystemServiceAction(ctx context.Context, unit, action string) error {
	var args []string
	switch action {
	case "start":
		args = []string{"systemctl", "enable", "--now", "--no-block", unit}
	case "stop":
		args = []string{"systemctl", "disable", "--now", "--no-block", unit}
	case "restart":
		args = []string{"systemctl", "restart", "--no-block", unit}
	default:
		return errors.New("unsupported service action")
	}
	_, err := m.nativeRootExec(ctx, args...)
	return err
}

// Scope-aware service access for native applications: system units are
// read and controlled as root, user units through the owner's manager.

func (m *Manager) nativeServiceMetadataFor(ctx context.Context, id string, units []string) (string, error) {
	if m.nativeCreatedSystemUnits(id) {
		return m.nativeSystemServiceMetadata(ctx, units)
	}
	return m.nativeServiceMetadata(ctx, units)
}

func (m *Manager) nativeServiceJournalFor(ctx context.Context, id string, units ...string) (string, error) {
	if len(units) == 0 {
		return "", errors.New("select a native service")
	}
	if m.nativeCreatedSystemUnits(id) {
		return m.nativeSystemServiceJournal(ctx, units...)
	}
	return m.nativeServiceJournal(ctx, units...)
}

func (m *Manager) nativeServiceActionFor(ctx context.Context, id, unit, action string) error {
	if m.nativeCreatedSystemUnits(id) {
		return m.nativeSystemServiceAction(ctx, unit, action)
	}
	_, err := m.nativeExec(ctx, "systemctl", "--user", "--no-block", action, unit)
	return err
}

// nativeSecretsFor returns the values scrubbed from an application's logs:
// its environment file for created applications, the discovered
// environment otherwise.
func (m *Manager) nativeSecretsFor(a NativeApplication) []string {
	if _, created := m.nativeCreatedPolicy(a.ID); created {
		return m.nativeCreatedSecrets(a.ID)
	}
	return m.nativeSecrets(a)
}
