// Package migrate moves a natively deployed application from one server to
// another with a single-writer cutover.
//
// A move is a sequence of explicit, reviewable operations:
//
//  1. Preflight both hosts (capacity, toolchains, free ports and unit names,
//     where DNS points today).
//  2. Export a bundle on the source: shared files, environment files, unit
//     definitions, the application's crontab block and database dumps.
//  3. Transfer the bundle with a caller-supplied Transport.
//  4. Import on the target: lay out the application, restore data, install
//     units without starting anything, then deploy the exact source commit
//     with the release engine.
//  5. Verify the target by name before DNS changes.
//  6. Cut over: freeze the source (maintenance page, stopped workers), take
//     and restore the final dump, start the target, switch DNS, and keep the
//     stopped source for rollback.
//
// Nothing here changes DNS or removes source data. Every operation that
// touches a host is a separate call, so an operator can review and sequence
// them, and the same calls run against local directories in tests.
package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agim/lidza-deploy/internal/release"
)

// App identifies one application on one host.
type App struct {
	Name     string         // crontab block identifier and bundle name
	Layout   release.Layout // root and owner on this host
	Domain   string
	Port     string   // loopback port the web socket or process listens on
	Units    []string // service and socket unit names in start order
	EnvFiles []string // absolute paths of KEY=value files
	// UnitDir holds the owner's unit files (for example ~/.config/systemd/user).
	UnitDir string
	// Databases maps a short name to a connection URL. Values never enter a bundle.
	Databases map[string]string
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (a App) validate() error {
	switch {
	case !namePattern.MatchString(a.Name):
		return fmt.Errorf("migrate: invalid application name %q", a.Name)
	case !filepath.IsAbs(a.Layout.Root):
		return errors.New("migrate: application root must be absolute")
	}
	for _, unit := range a.Units {
		if !unitPattern.MatchString(unit) {
			return fmt.Errorf("migrate: invalid unit %q", unit)
		}
	}
	for _, file := range a.EnvFiles {
		if !filepath.IsAbs(file) || filepath.Clean(file) != file {
			return fmt.Errorf("migrate: invalid environment file %q", file)
		}
	}
	return nil
}

var unitPattern = regexp.MustCompile(`^[A-Za-z0-9:_.@-]+\.(service|socket|timer)$`)

// Transport copies a bundle directory to the target host. Implementations
// must not retry or reconnect on their own: a failed transfer is reported
// and the operator decides.
type Transport interface {
	// Push copies the local directory tree at from to the target path to,
	// creating it when missing and preserving file modes. Owners are set by
	// the import step, not the transport.
	Push(ctx context.Context, from, to string) error
}

// LocalTransport copies between directories on this machine with rsync.
type LocalTransport struct{}

func (LocalTransport) Push(ctx context.Context, from, to string) error {
	return rsync(ctx, syncOptions{mirror: true}, from, to)
}

// SSHTransport pushes over an existing SSH control connection. Command is the
// exact ssh argv prefix (for example ssh -S /run/x.sock -o ControlMaster=no
// -o BatchMode=yes -o ConnectionAttempts=1 user@host). Only the options
// given are used; nothing is added that could open or reuse other sessions.
type SSHTransport struct {
	Command []string
	Host    string // user@host as rsync addresses it
}

func (t SSHTransport) Push(ctx context.Context, from, to string) error {
	if len(t.Command) == 0 || t.Host == "" || strings.ContainsAny(t.Host, " :") {
		return errors.New("migrate: ssh transport needs a command and host")
	}
	return rsync(ctx, syncOptions{ssh: t.Command, mirror: true}, from, t.Host+":"+to)
}

type syncOptions struct {
	ssh      []string
	excludes []string
	// mirror also deletes excluded paths on the receiver, so the destination
	// becomes an exact copy. Without it, excluded paths already present on
	// the receiver (such as caches rebuilt there) are left alone.
	mirror bool
}

func rsync(ctx context.Context, opts syncOptions, from, to string) error {
	info, err := os.Stat(from)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("migrate: %s is not a directory", from)
	}
	args := []string{"--archive", "--delete", "--no-owner", "--no-group", "--quiet", "--safe-links", "--mkpath"}
	if opts.mirror {
		args = append(args, "--delete-excluded")
	}
	for _, exclude := range opts.excludes {
		args = append(args, "--exclude="+exclude)
	}
	if len(opts.ssh) > 0 {
		args = append(args, "--rsh="+shellJoin(opts.ssh))
	}
	args = append(args, strings.TrimRight(from, "/")+"/", to)
	cmd := exec.CommandContext(ctx, "rsync", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("migrate: rsync: %w: %s", err, tail(string(out)))
	}
	return nil
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 600 {
		return s[len(s)-600:]
	}
	return s
}

// ServiceControl manages the owner's units beyond restarts: it is used to
// install units on the target without starting them and to stop writers on
// the source at cutover.
type ServiceControl interface {
	release.Services
	DaemonReload(ctx context.Context) error
	Enable(ctx context.Context, unit string) error
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
}

// UserSystemd controls the owner's systemd user manager. Like
// release.UserSystemd it never starts a user manager that is not running.
type UserSystemd struct {
	release.UserSystemd
}

func (u UserSystemd) systemctl(ctx context.Context, args ...string) error {
	_, err := commandOutput(ctx, u.Exec, release.Command{Name: "systemctl " + args[0], Dir: "/", Env: u.Env,
		Args: append([]string{"systemctl", "--user", "--no-pager"}, args...)})
	return err
}

// commandOutput runs c through x and returns its trimmed standard output.
func commandOutput(ctx context.Context, x release.Executor, c release.Command) (string, error) {
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := x.Run(ctx, c); err != nil {
		return "", fmt.Errorf("%s: %w: %s", c.Name, err, tail(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (u UserSystemd) DaemonReload(ctx context.Context) error {
	return u.systemctl(ctx, "daemon-reload")
}
func (u UserSystemd) Enable(ctx context.Context, unit string) error {
	return u.unitAction(ctx, "enable", unit)
}
func (u UserSystemd) Start(ctx context.Context, unit string) error {
	return u.unitAction(ctx, "start", unit)
}
func (u UserSystemd) Stop(ctx context.Context, unit string) error {
	return u.unitAction(ctx, "stop", unit)
}

func (u UserSystemd) unitAction(ctx context.Context, action, unit string) error {
	if !unitPattern.MatchString(unit) {
		return fmt.Errorf("migrate: invalid unit %q", unit)
	}
	return u.systemctl(ctx, action, unit)
}
