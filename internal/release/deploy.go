package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Plan is everything one deploy or rollback needs.
type Plan struct {
	App     string // identifier used for the schedule block (letters, digits, . _ -)
	Layout  Layout
	Source  Source
	Ref     string // branch or exact commit
	Profile Profile
	Env     []string // application environment for build and release steps
	Exec    Executor

	Services Services // nil leaves processes alone
	Units    []string // restart order: web servers first, then workers
	Settle   time.Duration

	Crontab Crontab // nil leaves the crontab alone
	// AdoptSchedules names crontab blocks installed by an earlier deploy tool
	// that the first deploy replaces with this application's block: exact
	// Whenever identifiers, or prefixes ending in "/" such as
	// "<root>/releases/" for blocks keyed by a release's schedule file path.
	AdoptSchedules []string

	// Backup runs before release steps that change databases. A plan with
	// database steps and no Backup is refused unless SkipBackup is set.
	Backup     func(ctx context.Context) error
	SkipBackup bool
	// Prepare runs after the profile's Prepare, for host-specific files.
	Prepare func(l Layout, id, commit string) error
	// Health verifies the published release; failure republishes the previous one.
	Health func(ctx context.Context) error

	Keep int // releases kept for rollback; default 5
	Log  io.Writer
	Now  func() time.Time
}

// Result describes what a deploy or rollback did.
type Result struct {
	Release         string
	Commit          string
	Previous        string
	Restarted       []string
	Skipped         map[string]ServiceState
	ScheduleChanged bool
	Migrated        bool
	RolledBack      bool // the new release failed and the previous one was republished
	Pruned          []string
}

func (p *Plan) logf(format string, args ...any) {
	if p.Log != nil {
		fmt.Fprintf(p.Log, "==> "+format+"\n", args...)
	}
}

func (p *Plan) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Plan) validate() error {
	if !identifierPattern.MatchString(p.App) || p.Layout.Root == "" || !filepath.IsAbs(p.Layout.Root) || p.Exec == nil {
		return errors.New("release: plan needs an app identifier, absolute root and executor")
	}
	if p.Keep == 0 {
		p.Keep = 5
	}
	return nil
}

// lock takes an exclusive per-application lock for the whole operation.
func (p *Plan) lock() (func(), error) {
	path := filepath.Join(p.Layout.Root, ".release.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("release: another deploy of %s is running", p.App)
	}
	return func() { file.Close() }, nil
}

func (p *Plan) stepEnv(dir string, step Step) []string {
	env := withEnv(p.Env, "PWD="+dir)
	env = withEnv(env, step.Env...)
	// Always resolve gems from the release being built, never from an
	// inherited setting that still points at the running release.
	if exists(dir, "Gemfile") {
		env = withEnv(env, "BUNDLE_GEMFILE="+filepath.Join(dir, "Gemfile"))
	}
	return env
}

func (p *Plan) run(ctx context.Context, dir string, step Step, stdout io.Writer) (bool, error) {
	if step.When != nil && !step.When(dir) {
		return false, nil
	}
	p.logf("%s", step.Name)
	if stdout == nil {
		stdout = p.Log
	}
	err := p.Exec.Run(ctx, Command{Name: step.Name, Dir: dir, Args: step.Args, Env: p.stepEnv(dir, step),
		Network: step.Network, Database: step.Database, Stdout: stdout, Stderr: p.Log})
	if err != nil {
		return true, fmt.Errorf("release: %s failed: %w", step.Name, err)
	}
	return true, nil
}

// renderSchedule returns the crontab entries the release defines.
func (p *Plan) renderSchedule(ctx context.Context, dir string) (string, bool, error) {
	if p.Crontab == nil || p.Profile.Schedule == nil {
		return "", false, nil
	}
	step := p.Profile.Schedule(p.Layout)
	if step == nil {
		return "", true, nil
	}
	var out strings.Builder
	ran, err := p.run(ctx, dir, *step, &out)
	if err != nil || !ran {
		return "", true, err
	}
	var lines []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "## [message]") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), true, nil
}

// Deploy builds ref into a new release and publishes it.
func Deploy(ctx context.Context, p Plan) (result Result, err error) {
	if err = p.validate(); err != nil {
		return result, err
	}
	unlock, err := p.lock()
	if err != nil {
		return result, err
	}
	defer unlock()
	l := p.Layout
	if result.Previous, err = l.CurrentID(); err != nil {
		return result, err
	}
	p.logf("fetching %s", p.Ref)
	if result.Commit, err = p.Source.Fetch(ctx, p.Exec, l, p.Ref); err != nil {
		return result, err
	}
	if result.Release, err = l.Create(p.now()); err != nil {
		return result, err
	}
	dir := l.Release(result.Release)
	published := false
	defer func() {
		if err != nil && !published {
			os.RemoveAll(dir)
		}
	}()
	p.logf("release %s from %s", result.Release, shortCommit(result.Commit))
	if err = p.Source.Export(ctx, p.Exec, l, result.Release, result.Commit); err != nil {
		return result, err
	}
	if p.Profile.Prepare != nil {
		if err = p.Profile.Prepare(l, result.Release, p.Env); err != nil {
			return result, err
		}
	}
	if p.Prepare != nil {
		if err = p.Prepare(l, result.Release, result.Commit); err != nil {
			return result, err
		}
	}
	if err = l.LinkShared(result.Release, p.Profile.Shared); err != nil {
		return result, err
	}
	for _, step := range p.Profile.Build {
		if _, err = p.run(ctx, dir, step, nil); err != nil {
			return result, err
		}
	}
	if p.Profile.Output != "" {
		if info, statErr := os.Stat(filepath.Join(dir, p.Profile.Output)); statErr != nil || !info.IsDir() {
			return result, fmt.Errorf("release: build produced no %s directory", p.Profile.Output)
		}
	}
	schedule, manageSchedule, err := p.renderSchedule(ctx, dir)
	if err != nil {
		return result, err
	}
	if err = l.Complete(result.Release); err != nil {
		return result, err
	}
	var release []Step
	for _, step := range p.Profile.Release {
		if step.When == nil || step.When(dir) {
			release = append(release, step)
		}
	}
	if len(release) > 0 {
		switch {
		case p.Backup != nil:
			p.logf("backing up before release steps")
			if err = p.Backup(ctx); err != nil {
				return result, fmt.Errorf("release: backup before release failed: %w", err)
			}
		case !p.SkipBackup:
			return result, errors.New("release: release steps change databases; a backup is required")
		}
		for _, step := range release {
			if _, err = p.run(ctx, dir, step, nil); err != nil {
				return result, err
			}
		}
		result.Migrated = true
	}
	p.logf("publishing %s", result.Release)
	if err = l.Activate(result.Release, result.Previous); err != nil {
		return result, err
	}
	published = true
	if err = p.verify(ctx, &result); err != nil {
		if result.Previous == "" {
			return result, err
		}
		p.logf("new release failed (%v); republishing %s", err, result.Previous)
		if restoreErr := p.restore(ctx, result.Previous, result.Release); restoreErr != nil {
			return result, fmt.Errorf("%w; restoring %s also failed: %v", err, result.Previous, restoreErr)
		}
		result.RolledBack = true
		return result, err
	}
	if manageSchedule {
		if result.ScheduleChanged, err = SyncSchedule(ctx, p.Crontab, p.App, schedule, p.AdoptSchedules...); err != nil {
			return result, err
		}
	}
	result.Pruned, err = l.Prune(p.Keep, result.Previous)
	return result, err
}

// verify restarts running processes and checks health.
func (p *Plan) verify(ctx context.Context, result *Result) error {
	if p.Services != nil && len(p.Units) > 0 {
		p.logf("restarting %s", strings.Join(p.Units, ", "))
		restarted, err := RestartRunning(ctx, p.Services, p.Units, p.Settle)
		result.Restarted, result.Skipped = restarted.Restarted, restarted.Skipped
		if err != nil {
			return err
		}
	}
	if p.Health != nil {
		p.logf("checking health")
		if err := p.Health(ctx); err != nil {
			return fmt.Errorf("release: health check failed: %w", err)
		}
	}
	return nil
}

func (p *Plan) restore(ctx context.Context, target, expected string) error {
	if err := p.Layout.Activate(target, expected); err != nil {
		return err
	}
	var ignored Result
	return p.verify(ctx, &ignored)
}

// Rollback republishes an earlier release (the previous one when target is
// ""), restarts running processes, and restores that release's schedule.
// Database migrations are not reversed.
func Rollback(ctx context.Context, p Plan, target string) (result Result, err error) {
	if err = p.validate(); err != nil {
		return result, err
	}
	unlock, err := p.lock()
	if err != nil {
		return result, err
	}
	defer unlock()
	l := p.Layout
	if result.Previous, err = l.CurrentID(); err != nil {
		return result, err
	}
	if target == "" {
		if target, err = l.Previous(); err != nil {
			return result, err
		}
	}
	if target == result.Previous {
		return result, fmt.Errorf("release: %s is already current", target)
	}
	result.Release = target
	if result.Commit, err = l.Revision(target); err != nil {
		return result, err
	}
	schedule, manageSchedule, err := p.renderSchedule(ctx, l.Release(target))
	if err != nil {
		return result, err
	}
	p.logf("rolling back to %s (%s)", target, shortCommit(result.Commit))
	if err = l.Activate(target, result.Previous); err != nil {
		return result, err
	}
	if err = p.verify(ctx, &result); err != nil {
		return result, err
	}
	if manageSchedule {
		result.ScheduleChanged, err = SyncSchedule(ctx, p.Crontab, p.App, schedule, p.AdoptSchedules...)
	}
	return result, err
}
