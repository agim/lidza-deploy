package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/agim/lidza-deploy/internal/release"
)

// ImportOptions configure Import.
type ImportOptions struct {
	Services ServiceControl // installs and enables units; nil skips units
	Dumper   Dumper
	// Databases maps each dump name in the bundle to the target database URL.
	Databases map[string]string
	// Deploy builds and publishes commit on the target with the release
	// engine. It receives the exact commit from the bundle.
	Deploy func(ctx context.Context, commit string) error
	Log    io.Writer
}

// Import lays the application out on this host from a bundle: shared files,
// environment files, unit definitions (enabled, never started), restored
// databases, and finally a release of the exact source commit. The
// application's schedule is not installed; InstallSchedule does that at
// cutover so jobs never run on two hosts at once.
func Import(ctx context.Context, app App, dir string, opts ImportOptions) (Manifest, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return m, err
	}
	if err := app.validate(); err != nil {
		return m, err
	}
	if m.App != app.Name {
		return m, fmt.Errorf("migrate: bundle is for %s, not %s", m.App, app.Name)
	}
	if exists(app.Layout.Current()) {
		return m, errors.New("migrate: target already has a published release; import refused")
	}
	if err := ImportData(ctx, app, dir, m, opts); err != nil {
		return m, err
	}
	if opts.Services != nil {
		if err := installUnits(ctx, app, dir, m, opts); err != nil {
			return m, err
		}
	}
	if opts.Deploy == nil {
		return m, errors.New("migrate: import needs a deploy function for the source commit")
	}
	logf(opts.Log, "deploying commit %s on the target", m.Commit[:12])
	if err := opts.Deploy(ctx, m.Commit); err != nil {
		return m, err
	}
	return m, nil
}

// ImportData copies shared files and environment files from the bundle and
// restores its database dumps. It is the repeatable data step: Import runs
// it once, and the final cutover sync runs it again on a fresh bundle.
func ImportData(ctx context.Context, app App, dir string, m Manifest, opts ImportOptions) error {
	if err := app.validate(); err != nil {
		return err
	}
	for _, dump := range m.Databases {
		if opts.Databases[dump.Name] == "" {
			return fmt.Errorf("migrate: no target database URL for %s", dump.Name)
		}
		if opts.Dumper == nil {
			return errors.New("migrate: bundle has database dumps but no dumper was given")
		}
	}
	if err := mkdirOwned(app.Layout.Root, app.Layout.Owner); err != nil {
		return err
	}
	if exists(filepath.Join(dir, "shared")) {
		logf(opts.Log, "copying shared files")
		if err := rsync(ctx, syncOptions{excludes: m.Excludes}, filepath.Join(dir, "shared"), app.Layout.Shared()); err != nil {
			return err
		}
		if err := chownTree(app.Layout.Shared(), app.Layout.Owner); err != nil {
			return err
		}
	}
	for _, entry := range m.EnvFiles {
		target := entry.Target
		if !filepath.IsAbs(target) {
			target = filepath.Join(app.Layout.Root, target)
		}
		if err := writeOwned(target, filepath.Join(dir, entry.Path), 0o600, app.Layout.Owner, nil); err != nil {
			return fmt.Errorf("migrate: environment file %s: %w", target, err)
		}
	}
	for _, dump := range m.Databases {
		logf(opts.Log, "restoring database %s", dump.Name)
		if err := opts.Dumper.Restore(ctx, opts.Databases[dump.Name], filepath.Join(dir, dump.Path)); err != nil {
			return err
		}
	}
	return nil
}

func installUnits(ctx context.Context, app App, dir string, m Manifest, opts ImportOptions) error {
	if app.UnitDir == "" || !filepath.IsAbs(app.UnitDir) {
		return errors.New("migrate: target unit directory must be absolute")
	}
	rewrite := func(raw []byte) []byte {
		if m.SourceRoot == app.Layout.Root || m.SourceRoot == "" {
			return raw
		}
		return []byte(strings.ReplaceAll(string(raw), m.SourceRoot, app.Layout.Root))
	}
	for _, unit := range m.Units {
		target := filepath.Join(app.UnitDir, unit)
		if exists(target) {
			return fmt.Errorf("migrate: unit %s already exists on the target", unit)
		}
		if err := writeOwned(target, filepath.Join(dir, "units", unit), 0o644, app.Layout.Owner, rewrite); err != nil {
			return err
		}
	}
	if err := opts.Services.DaemonReload(ctx); err != nil {
		return err
	}
	for _, unit := range m.Units {
		logf(opts.Log, "enabling %s (not started)", unit)
		if err := opts.Services.Enable(ctx, unit); err != nil {
			return err
		}
	}
	return nil
}

// StartServices starts the application's units in order (sockets first, so
// a socket-activated server owns its port before anything connects) and
// waits for each service to run.
func StartServices(ctx context.Context, services ServiceControl, units []string, log io.Writer) error {
	ordered := append([]string{}, units...)
	for pass := 0; pass < 2; pass++ {
		for _, unit := range ordered {
			if strings.HasSuffix(unit, ".socket") != (pass == 0) {
				continue
			}
			logf(log, "starting %s", unit)
			if err := services.Start(ctx, unit); err != nil {
				return err
			}
		}
	}
	for _, unit := range ordered {
		if strings.HasSuffix(unit, ".service") {
			state, err := services.State(ctx, unit)
			if err != nil {
				return err
			}
			if state.Active == "failed" {
				return fmt.Errorf("migrate: %s failed to start", unit)
			}
		}
	}
	return nil
}

// InstallSchedule writes the bundle's crontab block on this host.
func InstallSchedule(ctx context.Context, tab release.Crontab, m Manifest) (bool, error) {
	if tab == nil {
		return false, nil
	}
	return release.SyncSchedule(ctx, tab, m.App, m.Schedule)
}

func mkdirOwned(dir string, owner release.Owner) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return chown(dir, owner)
}

func chown(path string, owner release.Owner) error {
	if os.Geteuid() != 0 || owner.UID == 0 && owner.GID == 0 {
		return nil
	}
	return os.Lchown(path, owner.UID, owner.GID)
}

func chownTree(root string, owner release.Owner) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return chown(path, owner)
	})
}

func writeOwned(target, from string, mode os.FileMode, owner release.Owner, transform func([]byte) []byte) error {
	raw, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if transform != nil {
		raw = transform(raw)
	}
	if err := mkdirOwned(filepath.Dir(target), owner); err != nil {
		return err
	}
	temporary := target + ".migrate-tmp"
	if err := os.WriteFile(temporary, raw, mode); err != nil {
		return err
	}
	if err := chown(temporary, owner); err != nil {
		os.Remove(temporary)
		return err
	}
	return os.Rename(temporary, target)
}
