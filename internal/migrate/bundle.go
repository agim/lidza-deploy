package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// ManifestFile names the bundle description inside a bundle directory.
const ManifestFile = "manifest.json"

// DefaultExcludes are shared paths that are rebuilt on the target rather than
// copied: dependency caches and transient files.
var DefaultExcludes = []string{"bundle/", "node_modules/", "tmp/cache/", "tmp/pids/", "tmp/sockets/", "log/*.log", "maintenance/"}

// Manifest describes a bundle. It carries no secret values: environment
// files and dumps are separate files with restricted modes.
type Manifest struct {
	Version    int       `json:"version"`
	App        string    `json:"app"`
	Created    time.Time `json:"created"`
	SourceRoot string    `json:"source_root"`
	Release    string    `json:"release"`
	Commit     string    `json:"commit"`
	Domain     string    `json:"domain,omitempty"`
	Port       string    `json:"port,omitempty"`
	Units      []string  `json:"units"`
	EnvFiles   []EnvFile `json:"env_files"`
	Excludes   []string  `json:"excludes"`
	Schedule   string    `json:"schedule,omitempty"`
	Databases  []Dump    `json:"databases"`
}

// EnvFile records where an environment file lives relative to the
// application root (or absolutely when outside it) and its bundle path.
type EnvFile struct {
	Target string `json:"target"`
	Path   string `json:"path"`
}

// Dump records one database dump inside the bundle.
type Dump struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ExportOptions configure Export.
type ExportOptions struct {
	Dumper   Dumper          // required when the application has databases
	Crontab  release.Crontab // nil skips the schedule block
	Excludes []string        // nil uses DefaultExcludes
	Log      io.Writer
}

func logf(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, "==> "+format+"\n", args...)
	}
}

// Export writes a complete bundle of the application into dir, which must
// not exist yet. The bundle directory is created mode 0700.
func Export(ctx context.Context, app App, dir string, opts ExportOptions) (Manifest, error) {
	var m Manifest
	if err := app.validate(); err != nil {
		return m, err
	}
	if !filepath.IsAbs(dir) {
		return m, errors.New("migrate: bundle directory must be absolute")
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return m, fmt.Errorf("migrate: bundle directory: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()
	current, err := app.Layout.CurrentID()
	if err != nil {
		return m, err
	}
	if current == "" {
		return m, errors.New("migrate: application has no published release")
	}
	commit, err := app.Layout.Revision(current)
	if err != nil {
		return m, fmt.Errorf("migrate: current release has no revision: %w", err)
	}
	m = Manifest{Version: 1, App: app.Name, Created: time.Now().UTC(), SourceRoot: app.Layout.Root,
		Release: current, Commit: commit, Domain: app.Domain, Port: app.Port, Units: app.Units,
		Excludes: opts.Excludes, EnvFiles: []EnvFile{}, Databases: []Dump{}}
	if m.Excludes == nil {
		m.Excludes = DefaultExcludes
	}
	logf(opts.Log, "exporting %s release %s (%s)", app.Name, current, commit[:12])
	if exists(app.Layout.Shared()) {
		logf(opts.Log, "copying shared files")
		if err := rsync(ctx, syncOptions{excludes: m.Excludes, mirror: true}, app.Layout.Shared(), filepath.Join(dir, "shared")); err != nil {
			return m, err
		}
	}
	for i, file := range app.EnvFiles {
		target := file
		if rel, err := filepath.Rel(app.Layout.Root, file); err == nil && !strings.HasPrefix(rel, "..") {
			target = rel
		}
		entry := EnvFile{Target: target, Path: filepath.Join("env", fmt.Sprintf("%d-%s", i, filepath.Base(file)))}
		if err := copyFile(file, filepath.Join(dir, entry.Path), 0o600); err != nil {
			return m, fmt.Errorf("migrate: environment file %s: %w", file, err)
		}
		m.EnvFiles = append(m.EnvFiles, entry)
	}
	for _, unit := range app.Units {
		if err := copyFile(filepath.Join(app.UnitDir, unit), filepath.Join(dir, "units", unit), 0o644); err != nil {
			return m, fmt.Errorf("migrate: unit %s: %w", unit, err)
		}
	}
	if opts.Crontab != nil {
		existing, err := opts.Crontab.Read(ctx)
		if err != nil {
			return m, err
		}
		if m.Schedule, err = ScheduleBlock(existing, app.Name); err != nil {
			return m, err
		}
	}
	names := make([]string, 0, len(app.Databases))
	for name := range app.Databases {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 0 && opts.Dumper == nil {
		return m, errors.New("migrate: application has databases but no dumper was given")
	}
	for _, name := range names {
		if !namePattern.MatchString(name) {
			return m, fmt.Errorf("migrate: invalid database name %q", name)
		}
		logf(opts.Log, "dumping database %s", name)
		entry := Dump{Name: name, Path: filepath.Join("db", name+".dump")}
		if err := os.MkdirAll(filepath.Join(dir, "db"), 0o700); err != nil {
			return m, err
		}
		path := filepath.Join(dir, entry.Path)
		if err := opts.Dumper.Dump(ctx, app.Databases[name], path); err != nil {
			return m, err
		}
		if entry.SHA256, err = Digest(path); err != nil {
			return m, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return m, err
		}
		entry.Size = info.Size()
		m.Databases = append(m.Databases, entry)
	}
	if err := writeManifest(dir, m); err != nil {
		return m, err
	}
	ok = true
	return m, nil
}

func writeManifest(dir string, m Manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestFile), append(raw, '\n'), 0o600)
}

// ReadManifest loads and checks a bundle: the manifest parses, every listed
// file exists and dump digests match.
func ReadManifest(dir string) (Manifest, error) {
	var m Manifest
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return m, fmt.Errorf("migrate: bundle manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Version != 1 || !namePattern.MatchString(m.App) || len(m.Commit) != 40 {
		return m, errors.New("migrate: bundle manifest is invalid")
	}
	for _, entry := range m.EnvFiles {
		if !safeBundlePath(entry.Path, "env") || !exists(filepath.Join(dir, entry.Path)) {
			return m, fmt.Errorf("migrate: bundle is missing environment file %s", entry.Path)
		}
	}
	for _, unit := range m.Units {
		if !unitPattern.MatchString(unit) || !exists(filepath.Join(dir, "units", unit)) {
			return m, fmt.Errorf("migrate: bundle is missing unit %s", unit)
		}
	}
	for _, dump := range m.Databases {
		if !safeBundlePath(dump.Path, "db") {
			return m, fmt.Errorf("migrate: bundle dump path %q is invalid", dump.Path)
		}
		digest, err := Digest(filepath.Join(dir, dump.Path))
		if err != nil || digest != dump.SHA256 {
			return m, fmt.Errorf("migrate: dump %s is missing or does not match its digest", dump.Name)
		}
	}
	return m, nil
}

func safeBundlePath(path, prefix string) bool {
	clean := filepath.Clean(path)
	return clean == path && !filepath.IsAbs(path) && strings.HasPrefix(path, prefix+"/") && !strings.Contains(path, "..")
}

func copyFile(from, to string, mode os.FileMode) error {
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", from)
	}
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// ScheduleBlock returns the entries inside the application's crontab block,
// using the same markers that release.SyncSchedule writes and adopts.
func ScheduleBlock(crontab, identifier string) (string, error) {
	begin := "# Begin Whenever generated tasks for: " + identifier
	end := "# End Whenever generated tasks for: " + identifier
	matches := func(line, marker string) bool {
		return line == marker || strings.HasPrefix(line, marker+" at: ")
	}
	var entries []string
	inside := false
	for _, line := range strings.Split(crontab, "\n") {
		switch {
		case !inside && matches(line, begin):
			inside = true
		case inside && matches(line, end):
			return strings.Join(entries, "\n"), nil
		case inside:
			entries = append(entries, line)
		}
	}
	if inside {
		return "", fmt.Errorf("migrate: crontab block for %s is not closed", identifier)
	}
	return "", nil
}
