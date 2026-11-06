package agent

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agim/lidza-deploy/internal/release"
)

// Removing unused Bundler versions. Server health warns when a Ruby keeps
// Bundler versions older than its RubyGems' default (native_ruby.go); this
// is the operator action that removes them. A version is removable only when
//
//   - it is not the RubyGems default Bundler (or the one RubyGems pairs with),
//   - no kept release of any application on that Ruby runs with it: the
//     release's runtime BUNDLER_VERSION, or its Gemfile.lock's BUNDLED WITH
//     when the release sets no BUNDLER_VERSION,
//
// and only after every application on that Ruby resolves its bundle:
// `bundle exec ruby -e 1` in a scratch copy of the current release's
// Gemfile, Gemfile.lock, version files, bundle config and PATH gems, as the
// application's account, with the release's own BUNDLER_VERSION and nothing
// inherited. The same check runs after removal; if an application no longer
// resolves, the removed versions are installed again.

// BundlerKept is a stale Bundler version that stays, and why.
type BundlerKept struct {
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

// BundlerGroup is one Ruby installation: an account's Ruby version with its
// RubyGems, and the applications that use it.
type BundlerGroup struct {
	User     string        `json:"user"`
	Ruby     string        `json:"ruby"`
	RubyGems string        `json:"rubygems"`
	Default  string        `json:"default_bundler"`
	Apps     []string      `json:"apps"`
	Remove   []string      `json:"remove"`
	Keep     []BundlerKept `json:"keep,omitempty"`
}

// BundlerCleanupStatus is the removal plan and the last removal job.
type BundlerCleanupStatus struct {
	Groups []BundlerGroup `json:"groups"`
	Job    *HostJob       `json:"job,omitempty"`
}

// BundlerSelection names one version the operator confirmed.
type BundlerSelection struct {
	User    string `json:"user"`
	Ruby    string `json:"ruby"`
	Version string `json:"version"`
}

// BundlerRemoval is the confirmed list of versions to remove.
type BundlerRemoval struct {
	Remove []BundlerSelection `json:"remove"`
}

func (r BundlerRemoval) Validate() error {
	if len(r.Remove) == 0 || len(r.Remove) > 64 {
		return errors.New("select between one and 64 Bundler versions")
	}
	for _, s := range r.Remove {
		if !release.GemVersion(s.Version) || !release.GemVersion(s.Ruby) || s.User == "" || len(s.User) > 64 {
			return errors.New("invalid Bundler selection")
		}
	}
	return nil
}

const bundlerCleanupKind = "bundler-cleanup"

// bundlerRoutes: GET /v1/host/ruby/bundlers is the plan and the last job;
// POST /v1/host/ruby/bundlers/remove starts the removal of confirmed
// versions.
func (m *Manager) bundlerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/host/ruby/bundlers", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, 200, m.bundlerStatus())
	})
	mux.HandleFunc("POST /v1/host/ruby/bundlers/remove", func(w http.ResponseWriter, r *http.Request) {
		var in BundlerRemoval
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := in.Validate(); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.startBundlerCleanup(in); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, m.bundlerStatus())
	})
}

type bundlerKey struct{ user, ruby, rubygems, bundler string }

// bundlerApp is one Ruby application with what its releases need.
type bundlerApp struct {
	id      string
	policy  NativeReleasePolicy
	account nativeReleaseAccount
	layout  release.Layout
	record  release.RubyToolchain
}

// bundlerPlan groups the Ruby applications by installation and decides
// which stale versions each installation can lose.
func (m *Manager) bundlerPlan() ([]BundlerGroup, map[bundlerKey][]bundlerApp) {
	m.mu.Lock()
	records := map[string]release.RubyToolchain{}
	for id, record := range m.data.NativeRuby {
		if _, registered := m.data.NativeApps[id]; registered {
			records[id] = record
		}
	}
	m.mu.Unlock()
	apps := map[bundlerKey][]bundlerApp{}
	stale := map[bundlerKey][]string{}
	used := map[bundlerKey]map[string]string{}
	for _, id := range slices.Sorted(maps.Keys(records)) {
		record := records[id]
		_, policy, err := m.nativeReleasePolicy(id)
		if err != nil {
			continue
		}
		account, err := m.nativeReleaseAccount(policy)
		if err != nil {
			continue
		}
		key := bundlerKey{account.Name, record.Ruby, record.RubyGems, cmp.Or(record.DefaultBundler, release.PairedBundler(record.RubyGems))}
		if key.ruby == "" || key.rubygems == "" || key.bundler == "" {
			continue
		}
		app := bundlerApp{id: id, policy: policy, account: account, layout: m.nativeReleaseLayout(account, policy), record: record}
		apps[key] = append(apps[key], app)
		for _, version := range release.StaleBundlers(key.bundler, record.Installed) {
			if !slices.Contains(stale[key], version) {
				stale[key] = append(stale[key], version)
			}
		}
		if used[key] == nil {
			used[key] = map[string]string{}
		}
		releases, _ := app.layout.List()
		for _, rid := range releases {
			if version := releaseBundler(app.layout, rid); version != "" {
				if _, seen := used[key][version]; !seen {
					used[key][version] = "release " + rid + " of " + id + " runs with it"
				}
			}
		}
	}
	var groups []BundlerGroup
	for key, list := range apps {
		group := BundlerGroup{User: key.user, Ruby: key.ruby, RubyGems: key.rubygems, Default: key.bundler, Remove: []string{}}
		for _, app := range list {
			group.Apps = append(group.Apps, app.id)
		}
		for _, version := range release.StaleBundlers(key.bundler, stale[key]) {
			switch why, inUse := used[key][version]; {
			case version == key.bundler || version == release.PairedBundler(key.rubygems):
				group.Keep = append(group.Keep, BundlerKept{version, "RubyGems " + key.rubygems + " default"})
			case inUse:
				group.Keep = append(group.Keep, BundlerKept{version, why})
			default:
				group.Remove = append(group.Remove, version)
			}
		}
		if len(group.Remove) > 0 || len(group.Keep) > 0 {
			groups = append(groups, group)
		}
	}
	slices.SortFunc(groups, func(a, b BundlerGroup) int { return cmp.Or(cmp.Compare(a.User, b.User), cmp.Compare(a.Ruby, b.Ruby)) })
	return groups, apps
}

// releaseBundler is the Bundler one release runs with: its runtime
// BUNDLER_VERSION, else its Gemfile.lock's BUNDLED WITH, else "" (RubyGems'
// default).
func releaseBundler(l release.Layout, id string) string {
	for _, entry := range l.RuntimeEnv(id) {
		if value, ok := strings.CutPrefix(entry, "BUNDLER_VERSION="); ok {
			return value
		}
	}
	return release.LockedBundler(l.Release(id))
}

func (m *Manager) bundlerStatus() BundlerCleanupStatus {
	groups, _ := m.bundlerPlan()
	status := BundlerCleanupStatus{Groups: groups}
	if status.Groups == nil {
		status.Groups = []BundlerGroup{}
	}
	m.mu.Lock()
	for _, job := range m.hostUpdatesStateLocked().Jobs {
		if job.Kind == bundlerCleanupKind {
			copy := job
			status.Job = &copy
			break
		}
	}
	m.mu.Unlock()
	return status
}

// startBundlerCleanup checks the selection against the current plan and
// starts the removal as a host job.
func (m *Manager) startBundlerCleanup(in BundlerRemoval) error {
	if err := m.hostOperationsAllowed(); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	groups, apps := m.bundlerPlan()
	type work struct {
		key      bundlerKey
		versions []string
	}
	var jobs []work
	for _, group := range groups {
		var versions []string
		for _, s := range in.Remove {
			if s.User == group.User && s.Ruby == group.Ruby && slices.Contains(group.Remove, s.Version) && !slices.Contains(versions, s.Version) {
				versions = append(versions, s.Version)
			}
		}
		if len(versions) > 0 {
			jobs = append(jobs, work{bundlerKey{group.User, group.Ruby, group.RubyGems, group.Default}, versions})
		}
	}
	if len(jobs) == 0 {
		return errors.New("none of the selected Bundler versions is removable now; reload the list")
	}
	return m.startHostJob(bundlerCleanupKind, "manual", func(ctx context.Context) (string, error) {
		var out strings.Builder
		var summary []string
		var failures []error
		for _, j := range jobs {
			removed, err := m.removeBundlers(ctx, apps[j.key], j.key, j.versions, &out)
			if len(removed) > 0 {
				summary = append(summary, fmt.Sprintf("Bundler %s from Ruby %s (%s)", strings.Join(removed, ", "), j.key.ruby, j.key.user))
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("Ruby %s (%s): %w", j.key.ruby, j.key.user, err))
			}
		}
		if len(summary) > 0 {
			fmt.Fprintf(&out, "Summary: Removed %s.\n", strings.Join(summary, "; "))
		}
		return out.String(), errors.Join(failures...)
	})
}

// bundlerExecutor runs commands as one application's account in its root.
func (m *Manager) bundlerExecutor(app bundlerApp, writable ...string) nativeReleaseExecutor {
	return nativeReleaseExecutor{m: m, account: app.account, root: app.policy.Root, id: "bundler-" + newID()[:8], app: app.id, writable: writable}
}

// bundlerEnv is the application's command environment without any
// Bundler selection it might carry.
func bundlerEnv(app bundlerApp) ([]string, error) {
	env, err := nativeReleaseEnvironment(app.account, app.policy)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(env, func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return strings.HasPrefix(key, "BUNDLE")
	}), nil
}

func (m *Manager) bundlerRun(ctx context.Context, x nativeReleaseExecutor, dir, name string, env []string, network bool, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	err := x.Run(ctx, release.Command{Name: name, Dir: dir, Args: args, Env: env, Network: network, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		detail := strings.TrimSpace(stderr.String() + "\n" + stdout.String())
		return stdout.String(), fmt.Errorf("%s: %w%s", name, err, boundedDetail(detail))
	}
	return stdout.String(), nil
}

func boundedDetail(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + boundedErrorText(detail, 600)
}

// removeBundlers removes versions from one Ruby installation and returns
// the versions removed.
func (m *Manager) removeBundlers(ctx context.Context, apps []bundlerApp, key bundlerKey, versions []string, out io.Writer) ([]string, error) {
	if len(apps) == 0 {
		return nil, errors.New("no application uses this Ruby any more")
	}
	// Releases and backups of these applications wait for the removal.
	m.mu.Lock()
	for _, app := range apps {
		if m.nativeOperations[app.id] || m.busy(app.id) {
			m.mu.Unlock()
			return nil, fmt.Errorf("wait for the running operation of %s", app.id)
		}
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	for _, app := range apps {
		m.nativeOperations[app.id] = true
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		for _, app := range apps {
			delete(m.nativeOperations, app.id)
		}
		m.mu.Unlock()
	}()
	runner := apps[0]
	current, err := runner.layout.CurrentID()
	if err != nil || current == "" {
		return nil, fmt.Errorf("%s has no current release to run gem from", runner.id)
	}
	dir := runner.layout.Release(current)
	env, err := bundlerEnv(runner)
	if err != nil {
		return nil, err
	}
	x := m.bundlerExecutor(runner)
	listed, err := m.bundlerRun(ctx, x, dir, "gem list bundler", env, false, "gem", "list", "--local", "--exact", "bundler")
	if err != nil {
		return nil, err
	}
	defaultVersion, installed := release.ParseBundlerList(listed)
	var targets []string
	for _, version := range versions {
		switch {
		case version == defaultVersion:
			fmt.Fprintf(out, "Bundler %s is RubyGems' default here; kept.\n", version)
		case !slices.Contains(installed, version):
			fmt.Fprintf(out, "Bundler %s is no longer installed.\n", version)
		default:
			targets = append(targets, version)
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	for _, app := range apps {
		if err := m.bundlerCheck(ctx, app); err != nil {
			return nil, fmt.Errorf("nothing was removed: %s does not resolve its bundle before the change: %w", app.id, err)
		}
		fmt.Fprintf(out, "%s resolves its bundle.\n", app.id)
	}
	dirs, err := m.bundlerRun(ctx, x, dir, "gem directories", env, false, "ruby", "-e", `puts Gem.dir, Gem.bindir, Gem.user_dir`)
	if err != nil {
		return nil, err
	}
	var writable []string
	for _, path := range strings.Split(dirs, "\n") {
		path = filepath.Clean(strings.TrimSpace(path))
		if info, err := os.Stat(path); err == nil && info.IsDir() && filepath.IsAbs(path) && path != "/" {
			writable = append(writable, path)
		}
	}
	gem := m.bundlerExecutor(runner, writable...)
	var removed []string
	var failure error
	for _, version := range targets {
		if _, err := m.bundlerRun(ctx, gem, dir, "gem uninstall bundler "+version, env, false, "gem", "uninstall", "bundler", "--version", version, "--executables", "--ignore-dependencies"); err != nil {
			failure = fmt.Errorf("Bundler %s could not be removed (is this Ruby owned by another account?): %w", version, err)
			break
		}
		removed = append(removed, version)
		fmt.Fprintf(out, "Removed Bundler %s from Ruby %s (%s).\n", version, key.ruby, key.user)
	}
	for _, app := range apps {
		if err := m.bundlerCheck(ctx, app); err != nil {
			// Put the removed versions back; the applications ran with them.
			for _, version := range removed {
				if _, installErr := m.bundlerRun(ctx, gem, dir, "gem install bundler "+version, env, true, "gem", "install", "bundler", "--version", version, "--no-document"); installErr != nil {
					fmt.Fprintf(out, "Bundler %s could not be installed again: %v\n", version, installErr)
				}
			}
			return nil, fmt.Errorf("%s no longer resolved its bundle, so the removed versions were installed again: %w", app.id, err)
		}
	}
	m.forgetBundlers(apps, removed)
	return removed, failure
}

// forgetBundlers drops removed versions from the recorded toolchains, so the
// health warning clears without waiting for the next release.
func (m *Manager) forgetBundlers(apps []bundlerApp, removed []string) {
	if len(removed) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, app := range apps {
		record, ok := m.data.NativeRuby[app.id]
		if !ok {
			continue
		}
		record.Installed = slices.DeleteFunc(slices.Clone(record.Installed), func(v string) bool { return slices.Contains(removed, v) })
		m.data.NativeRuby[app.id] = record
	}
	_ = m.save()
}

// bundlerCheckDir is the scratch directory of the bundle check, inside the
// application root (the only tree its commands may write).
func bundlerCheckDir(root string) string { return filepath.Join(root, ".cache", "lidza-bundler-check") }

// bundlerCheck resolves the application's bundle in a scratch copy of its
// current release, so nothing in the release changes.
func (m *Manager) bundlerCheck(ctx context.Context, app bundlerApp) error {
	current, err := app.layout.CurrentID()
	if err != nil || current == "" {
		return errors.New("no current release")
	}
	source := app.layout.Release(current)
	scratch := bundlerCheckDir(app.policy.Root)
	defer os.RemoveAll(scratch)
	if err := copyBundleFiles(source, scratch, app.account); err != nil {
		return err
	}
	env, err := bundlerEnv(app)
	if err != nil {
		return err
	}
	env = append(env, "BUNDLE_GEMFILE="+filepath.Join(scratch, "Gemfile"), "BUNDLE_FROZEN=true")
	for _, entry := range app.layout.RuntimeEnv(current) {
		if strings.HasPrefix(entry, "BUNDLER_VERSION=") {
			env = append(env, entry)
		}
	}
	_, err = m.bundlerRun(ctx, m.bundlerExecutor(app), scratch, "bundle exec ruby -e 1", env, false, "bundle", "exec", "ruby", "-e", "1")
	return err
}

// bundleFiles are the release files Bundler reads to resolve a bundle.
var bundleFiles = []string{"Gemfile", "Gemfile.lock", "gems.rb", "gems.locked", ".ruby-version", ".tool-versions", ".bundle/config"}

const bundleCopyLimit = 64 << 20

// copyBundleFiles copies the files Bundler needs, and the PATH gems the
// lockfile names inside the release, into scratch. Only regular files are
// copied (no symbolic links), and nothing outside the release.
func copyBundleFiles(source, scratch string, account nativeReleaseAccount) error {
	if err := os.RemoveAll(scratch); err != nil {
		return err
	}
	if err := mkdirOwnedAll(scratch, account); err != nil {
		return err
	}
	realSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	var copied int64
	copyFile := func(rel string) error {
		from := filepath.Join(source, rel)
		info, err := os.Lstat(from)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		// A directory on the way may be a link out of the release.
		if real, err := filepath.EvalSymlinks(from); err != nil || !strings.HasPrefix(real, realSource+string(filepath.Separator)) {
			return nil
		}
		copied += info.Size()
		if copied > bundleCopyLimit {
			return errors.New("the release's bundle files are too large to check")
		}
		to := filepath.Join(scratch, rel)
		if err := mkdirOwnedAll(filepath.Dir(to), account); err != nil {
			return err
		}
		raw, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, raw, 0o600); err != nil {
			return err
		}
		return chownAccount(to, account)
	}
	for _, rel := range bundleFiles {
		if err := copyFile(rel); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(filepath.Join(source, "Gemfile")); err != nil {
		return errors.New("the current release has no Gemfile")
	}
	for _, remote := range lockfilePathGems(filepath.Join(source, "Gemfile.lock")) {
		var roots []string
		if remote == "." {
			// A gemspec in the application itself: its gemspecs and lib/.
			matches, _ := filepath.Glob(filepath.Join(source, "*.gemspec"))
			for _, match := range matches {
				roots = append(roots, filepath.Base(match))
			}
			roots = append(roots, "lib")
		} else {
			roots = append(roots, remote)
		}
		for _, root := range roots {
			start := filepath.Join(source, root)
			err := filepath.WalkDir(start, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if entry.Type().IsRegular() {
					rel, _ := filepath.Rel(source, path)
					return copyFile(rel)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// lockfilePathGems are the PATH sources of a Gemfile.lock that lie inside
// the release, as clean relative paths.
func lockfilePathGems(lockfile string) []string {
	raw, err := os.ReadFile(lockfile)
	if err != nil {
		return nil
	}
	var out []string
	inPath := false
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" && !strings.HasPrefix(line, " ") {
			inPath = strings.TrimSpace(line) == "PATH"
			continue
		}
		remote, ok := strings.CutPrefix(strings.TrimSpace(line), "remote: ")
		if !inPath || !ok {
			continue
		}
		clean := filepath.Clean(remote)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			continue
		}
		if !slices.Contains(out, clean) {
			out = append(out, clean)
		}
	}
	return out
}

func chownAccount(path string, account nativeReleaseAccount) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return os.Lchown(path, account.UID, account.GID)
}

// mkdirOwnedAll creates dir and its missing parents, owned by the account.
func mkdirOwnedAll(dir string, account nativeReleaseAccount) error {
	var missing []string
	for path := dir; ; path = filepath.Dir(path) {
		if _, err := os.Lstat(path); err == nil {
			break
		}
		missing = append(missing, path)
		if filepath.Dir(path) == path {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := chownAccount(missing[i], account); err != nil {
			return err
		}
	}
	return nil
}
