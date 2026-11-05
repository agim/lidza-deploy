package agent

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

// NativeReleasePolicy describes one application that the release engine
// (internal/release) deploys into the conventional releases/current/shared
// layout on this host and runs through the application owner's existing
// service units. It is the single reviewed source for deploys, rollbacks,
// restarts, schedules and pre-release backups of that application.
type NativeReleasePolicy struct {
	Enabled    bool   `json:"enabled"`
	Root       string `json:"root"`       // application root, for example /home/deploy/<name>
	Repository string `json:"repository"` // owner/name on GitHub
	Branch     string `json:"branch"`     // deployed branch
	Runtime    string `json:"runtime"`    // rails, node, static, go or auto
	Output     string `json:"output,omitempty"`
	Domain     string `json:"domain,omitempty"`
	Port       string `json:"port,omitempty"` // loopback port the web process (or its socket) listens on
	HealthPath string `json:"health_path,omitempty"`

	// Units lists the owner's service units in restart order: web servers
	// first, then workers and custom processes. Only units that are running
	// at deploy time are restarted.
	Units []string `json:"units"`
	// Preserve lists units the operator keeps stopped or disabled. They are
	// never restarted, started or enabled by a deploy.
	Preserve []string `json:"preserve,omitempty"`
	// EnvFiles are KEY=value files, in override order, that compose the
	// application environment for build, release and schedule commands.
	EnvFiles []string `json:"env_files,omitempty"`
	// Shared overrides the profile's shared directories.
	Shared []string `json:"shared,omitempty"`
	// Schedule is the crontab block identifier; default is the directory name of Root.
	Schedule string `json:"schedule,omitempty"`
	Keep     int    `json:"keep,omitempty"`

	// BuildMemoryMiB caps the memory of each build command (default 2048).
	// JavaScript builds get three quarters of it as their heap limit.
	BuildMemoryMiB int `json:"build_memory_mib,omitempty"`

	// Databases names the protected native database resources that are
	// backed up (and verified off-site) before any release step runs.
	Databases []string `json:"databases,omitempty"`

	// User is the account releases build and run as; empty is the native
	// host's deployment user (native.user), as before per-application
	// accounts existed.
	User string `json:"user,omitempty"`
	// UnitScope selects the service manager of Units: "user" (default) is
	// the account's own systemd user manager, "system" means hardened
	// system units that run as User. With system units a deploy starts
	// every enabled or running unit; disabled, stopped units stay stopped.
	UnitScope string `json:"unit_scope,omitempty"`
	// Toolchains chooses where builds and processes find Go, Node.js and
	// Ruby: "managed" uses the agent's verified installations, "account"
	// uses the version manager in the account's home (for example asdf).
	// Empty means managed for Go applications and applications with their
	// own account, otherwise the account's version manager.
	Toolchains string `json:"toolchains,omitempty"`
}

// SystemUnits reports whether the policy's units are system units.
func (p NativeReleasePolicy) SystemUnits() bool { return p.UnitScope == "system" }

// ManagedToolchains reports whether the agent provides the build and
// runtime toolchains (Go, Node.js) for this application from its own
// verified installations, instead of the account's version manager.
func (p NativeReleasePolicy) ManagedToolchains() bool {
	switch p.Toolchains {
	case "managed":
		return true
	case "account":
		return false
	}
	return p.User != "" || p.Runtime == "go"
}

var (
	nativeReleaseUnitPattern = regexp.MustCompile(`^[A-Za-z0-9:_.@-]+\.service$`)
	nativeReleaseRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	nativeAccountPattern     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

// ScheduleIdentifier returns the crontab block identifier for the policy.
func (p NativeReleasePolicy) ScheduleIdentifier() string {
	if p.Schedule != "" {
		return p.Schedule
	}
	return filepath.Base(p.Root)
}

// Validate reports whether the policy is complete and internally consistent.
func (p NativeReleasePolicy) Validate() error {
	if p.Toolchains != "" && p.Toolchains != "managed" && p.Toolchains != "account" {
		return errors.New("native release policy toolchains must be managed or account")
	}
	switch {
	case !filepath.IsAbs(p.Root) || filepath.Clean(p.Root) != p.Root || p.Root == "/":
		return errors.New("native release policy needs an absolute, clean root")
	case !nativeReleaseRepoPattern.MatchString(p.Repository):
		return errors.New("native release policy needs a repository in owner/name form")
	case p.Branch == "" || strings.HasPrefix(p.Branch, "-") || strings.ContainsAny(p.Branch, " \t\n"):
		return errors.New("native release policy needs a branch")
	case p.Runtime != "rails" && p.Runtime != "node" && p.Runtime != "static" && p.Runtime != "go" && p.Runtime != "auto":
		return errors.New("native release policy runtime must be rails, node, static, go or auto")
	case p.User != "" && (!nativeAccountPattern.MatchString(p.User) || p.User == "root"):
		return errors.New("native release policy user must be a non-root account name")
	case p.UnitScope != "" && p.UnitScope != "user" && p.UnitScope != "system":
		return errors.New("native release policy unit scope must be user or system")
	case p.SystemUnits() && p.User == "":
		return errors.New("native release policy system units need the account they run as")
	case p.BuildMemoryMiB != 0 && (p.BuildMemoryMiB < 512 || p.BuildMemoryMiB > 16384):
		return errors.New("native release policy build memory must be between 512 and 16384 MiB")
	case p.Keep < 0 || p.Keep == 1:
		return errors.New("native release policy must keep at least two releases")
	}
	seen := map[string]bool{}
	for _, unit := range append(append([]string{}, p.Units...), p.Preserve...) {
		if !nativeReleaseUnitPattern.MatchString(unit) || seen[unit] {
			return errors.New("native release policy units must be unique service names")
		}
		seen[unit] = true
	}
	for _, file := range p.EnvFiles {
		if !filepath.IsAbs(file) {
			return errors.New("native release policy environment files must be absolute")
		}
	}
	if p.Schedule != "" && !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(p.Schedule) {
		return errors.New("native release policy schedule identifier is invalid")
	}
	return nil
}
