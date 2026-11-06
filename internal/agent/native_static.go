package agent

// Native static releases keep the existing Caddy document root and publish only
// a complete release via an atomic current-symlink replacement. This adapter
// never reloads Caddy or starts/restarts/enables an application service.
import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type NativeStaticPolicy struct {
	Enabled                     bool     `json:"enabled"`
	ExternalDeploymentsDisabled bool     `json:"external_deployments_disabled"`
	Repository                  string   `json:"repository"`
	Domain                      string   `json:"domain"`
	OutputDirectory             string   `json:"output_directory"`
	PostDeploy                  []string `json:"post_deploy,omitempty"`
}
type NativeReleaseRef struct {
	Path     string `json:"path"`
	Commit   string `json:"commit,omitempty"`
	ID       string `json:"id"`
	BasePath string `json:"-"`
}
type NativeReleaseHistory struct {
	Current  NativeReleaseRef `json:"current"`
	Previous NativeReleaseRef `json:"previous"`
}
type NativeReleaseTransition struct {
	Before NativeReleaseHistory `json:"before"`
	After  NativeReleaseHistory `json:"after"`
	Error  string               `json:"error,omitempty"`
}

func (m *Manager) nativeStaticPolicy(a NativeApplication) (NativeStaticPolicy, error) {
	if m.cfg.Native == nil {
		return NativeStaticPolicy{}, errors.New("native deployment ownership has not been approved")
	}
	p := m.cfg.Native.StaticDeployments[a.ID]
	if a.Runtime != "astro" || a.DocumentRoot != "public" || len(a.Services) != 0 || !p.Enabled || !p.ExternalDeploymentsDisabled || p.Repository != a.Repository || p.Domain != a.Domain {
		return p, errors.New("native deployment ownership has not been approved for this exact application")
	}
	if p.OutputDirectory == "" {
		p.OutputDirectory = "dist"
	}
	if !cleanRelativeDirectory(p.OutputDirectory) || p.OutputDirectory == "." || len(p.PostDeploy) > 32 {
		return p, errors.New("native static deployment policy is invalid")
	}
	for _, arg := range p.PostDeploy {
		if len(arg) > 4096 || strings.ContainsAny(arg, "\x00\r\n") {
			return p, errors.New("native post-deploy command is invalid")
		}
	}
	// Static adoption must not silently acquire native database ownership.
	values, err := m.nativeStaticEnvironment(a)
	if err != nil {
		return p, err
	}
	for key, value := range values {
		if strings.Contains(key, "DATABASE") && strings.Contains(key, "URL") || strings.HasPrefix(value, "postgres:") || strings.HasPrefix(value, "postgresql:") || strings.HasPrefix(value, "mysql:") {
			return p, errors.New("native database backups and ownership require a reviewed binding before deployment")
		}
	}
	return p, nil
}

func (m *Manager) enqueueNativeStatic(id string, req DeployRequest) (Deployment, error) {
	a, err := m.nativeApp(id)
	if err != nil {
		return Deployment{}, err
	}
	if _, err = m.nativeStaticPolicy(a); err != nil {
		return Deployment{}, err
	}
	if err = validateCheckoutCredential(req); err != nil {
		return Deployment{}, err
	}
	values, err := m.nativeBuildEnvironment(a.Name)
	if err != nil {
		return Deployment{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	override := m.data.NativeSettings[id]
	if override.InheritedEnvironment == nil {
		override.InheritedEnvironment = values
	}
	if override.Branch != "" {
		a.Branch = override.Branch
	}
	values = applyNativeEnvironment(values, override)
	if err := m.recoverNativePublications(); err != nil {
		return Deployment{}, err
	}
	if len(req.Key) > 200 || len(req.Token) > 4096 {
		return Deployment{}, errors.New("request exceeds limit")
	}
	if req.Key != "" {
		for _, d := range m.data.Deployments {
			if d.AppID == id && d.Key == req.Key {
				return d, nil
			}
		}
	}
	if m.nativeOperations[id] || len(m.queue) == cap(m.queue) {
		return Deployment{}, errors.New("native deployment is already active or the queue is full")
	}
	if _, pending := m.data.NativeTransitions[id]; pending {
		return Deployment{}, errors.New("native publication recovery needs review before another deployment")
	}
	d := Deployment{ID: newID(), AppID: id, Status: "queued", Kind: "deploy", Key: req.Key, Branch: a.Branch, Domain: a.Domain, Created: time.Now().UTC()}
	oldOverride := m.data.NativeSettings[id]
	if m.data.NativeSettings == nil {
		m.data.NativeSettings = map[string]NativeSettingsOverride{}
	}
	m.data.NativeSettings[id] = override
	m.data.Deployments = append(m.data.Deployments, d)
	if err = m.save(); err != nil {
		m.data.NativeSettings[id] = oldOverride
		m.data.Deployments = m.data.Deployments[:len(m.data.Deployments)-1]
		return Deployment{}, err
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[id] = true
	m.queue <- job{native: true, app: App{ID: id, Native: &a, Runtime: "astro", Repository: a.Repository, Branch: a.Branch, Domain: a.Domain, Env: values}, deployment: d.ID, credentials: req, token: req.Token}
	return d, nil
}

func (m *Manager) workNativeStatic(j job) {
	defer func() { m.mu.Lock(); delete(m.nativeOperations, j.app.ID); m.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(m.ctx, 25*time.Minute)
	defer cancel()
	ctx = m.deploymentDiagnostics(ctx, j)
	m.mu.Lock()
	err := m.update(j.deployment, "building", nil, nil)
	m.mu.Unlock()
	if err != nil {
		return
	}
	if j.credentials.CredentialURL != "" {
		j.token, err = m.checkoutCredential(ctx, j.credentials)
	}
	var release NativeReleaseRef
	if err == nil {
		release, err = m.prepareNativeStatic(ctx, *j.app.Native, j.deployment, j.token)
	}
	j.token = ""
	if err == nil {
		err = m.publishNativeStatic(j.app.ID, release)
	}
	m.mu.Lock()
	if err == nil {
		err = m.update(j.deployment, "live", &Release{ID: release.ID, Commit: release.Commit}, nil)
	} else {
		_ = m.update(j.deployment, "failed", nil, err)
	}
	m.mu.Unlock()
	if err == nil {
		policy, _ := m.nativeStaticPolicy(*j.app.Native)
		if len(policy.PostDeploy) > 0 {
			if _, hookErr := m.runNativeBuild(ctx, *j.app.Native, release.Path, j.deployment+"-post", policy.PostDeploy); hookErr != nil {
				// An activated static release keeps serving if an external post hook
				// fails. Never roll back a live release merely to retry a notification.
				commandDiagnostic(ctx, "post-deploy", "Post-deploy command failed; the published release remains live.")
			}
		}
	}
}

func (m *Manager) prepareNativeStatic(ctx context.Context, a NativeApplication, id, token string) (NativeReleaseRef, error) {
	if !regexp.MustCompile(`^[a-f0-9]{24}$`).MatchString(id) {
		return NativeReleaseRef{}, errors.New("invalid native deployment identity")
	}
	p, err := m.nativeStaticPolicy(a)
	if err != nil {
		return NativeReleaseRef{}, err
	}
	override := m.nativeOverride(a.ID)
	if override.Branch != "" {
		a.Branch = override.Branch
	}
	if override.OutputDirectory != "" {
		p.OutputDirectory = override.OutputDirectory
	}
	app := filepath.Join(m.cfg.Native.Root, a.Name)
	basePath, err := filepath.EvalSymlinks(filepath.Join(app, "current"))
	if err != nil {
		return NativeReleaseRef{}, errors.New("current native release is unavailable")
	}
	var capacity syscall.Statfs_t
	if syscall.Statfs(app, &capacity) != nil || capacity.Bavail*uint64(capacity.Bsize) < 4<<30 {
		return NativeReleaseRef{}, errors.New("native builds require at least 4 GiB of free filesystem capacity")
	}
	releases := filepath.Join(app, "releases")
	if resolved, err := filepath.EvalSymlinks(releases); err != nil || resolved != releases {
		return NativeReleaseRef{}, errors.New("native releases directory redirects outside the application")
	}
	candidate := filepath.Join(releases, "lidza-"+id)
	if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
		return NativeReleaseRef{}, errors.New("candidate release already exists")
	}
	checkout := m.nativeCheckout
	if checkout == nil {
		checkout = clone
	}
	if err := checkout(ctx, App{Repository: a.Repository, Branch: a.Branch}, candidate, token); err != nil {
		return NativeReleaseRef{}, err
	}
	if err := validateNativeAstroSource(candidate); err != nil {
		return NativeReleaseRef{}, err
	}
	commit, err := command(ctx, candidate, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return NativeReleaseRef{}, err
	}
	// Keep encrypted Git credentials out of subsequent build processes.
	if err := os.RemoveAll(filepath.Join(candidate, ".git")); err != nil {
		return NativeReleaseRef{}, err
	}
	if err := os.WriteFile(filepath.Join(candidate, "REVISION"), []byte(commit+"\n"), 0644); err != nil {
		return NativeReleaseRef{}, err
	}
	account, err := user.Lookup(m.cfg.Native.User)
	if err != nil {
		return NativeReleaseRef{}, errors.New("native build user is unavailable")
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid <= 0 {
		return NativeReleaseRef{}, errors.New("native builds require a non-root user")
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return NativeReleaseRef{}, err
	}
	if err := filepath.WalkDir(candidate, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	}); err != nil {
		return NativeReleaseRef{}, err
	}
	if err := preserveNativeRuntimePins(basePath, candidate, true, uid, gid); err != nil {
		return NativeReleaseRef{}, err
	}
	if err := m.buildNativeStatic(ctx, a, candidate, id); err != nil {
		return NativeReleaseRef{}, err
	}
	if err := validateNativeAstroSource(candidate); err != nil {
		return NativeReleaseRef{}, err
	}
	if err := preserveNativeRuntimePins(basePath, candidate, false, uid, gid); err != nil {
		return NativeReleaseRef{}, err
	}
	output := filepath.Join(candidate, p.OutputDirectory)
	if real, err := filepath.EvalSymlinks(output); err != nil || real != output {
		return NativeReleaseRef{}, errors.New("static build output is unavailable or redirects outside its release")
	}
	if regularFile(filepath.Join(output, "index.html"), false) != nil {
		return NativeReleaseRef{}, errors.New("static build did not produce index.html; current release retained")
	}
	if err := validateNativeStaticArtifact(output); err != nil {
		return NativeReleaseRef{}, err
	}
	if p.OutputDirectory != "public" {
		public := filepath.Join(candidate, "public")
		if info, err := os.Lstat(public); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return NativeReleaseRef{}, errors.New("candidate public path must be a real directory")
		}
		if err := os.RemoveAll(public); err != nil {
			return NativeReleaseRef{}, err
		}
		if err := os.Rename(output, public); err != nil {
			return NativeReleaseRef{}, err
		}
	}
	if err := m.retainNativeStaticAssets(a, candidate); err != nil {
		return NativeReleaseRef{}, err
	}
	if err := os.WriteFile(filepath.Join(candidate, "REVISION"), []byte(commit+"\n"), 0644); err != nil {
		return NativeReleaseRef{}, err
	}
	return NativeReleaseRef{ID: id, Path: candidate, Commit: commit, BasePath: basePath}, nil
}

func validateNativeAstroSource(candidate string) error {
	for _, path := range []string{"package.json", "package-lock.json"} {
		if err := regularFile(filepath.Join(candidate, path), false); err != nil {
			return errors.New("native Astro deployment requires a package manifest and npm lockfile")
		}
	}
	data, err := os.ReadFile(filepath.Join(candidate, "package.json"))
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
		Scripts         map[string]string `json:"scripts"`
	}
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, &pkg) != nil || pkg.Scripts["build"] == "" || pkg.Dependencies["astro"] == "" && pkg.DevDependencies["astro"] == "" {
		return errors.New("candidate no longer identifies an Astro application with a build script")
	}
	if _, err := os.Lstat(filepath.Join(candidate, "Gemfile")); !os.IsNotExist(err) {
		return errors.New("candidate contains a Ruby runtime requiring review before deployment")
	}
	return nil
}

func (m *Manager) retainNativeStaticAssets(a NativeApplication, candidate string) error {
	old := filepath.Join(m.cfg.Native.Root, a.Name, "current/public/_astro")
	if _, err := os.Stat(old); os.IsNotExist(err) {
		return nil
	}
	destination := filepath.Join(candidate, "public/_astro")
	var bytes int64
	files := 0
	return filepath.WalkDir(old, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(old, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("static asset retention refuses symlinks and special files")
		}
		bytes += info.Size()
		files++
		if bytes > 1<<30 || files > 20000 {
			return errors.New("retained static assets exceed the reviewed build budget")
		}
		if _, err := os.Lstat(target); err == nil {
			old, err := nativeStaticFileDigest(path)
			if err != nil {
				return err
			}
			next, err := nativeStaticFileDigest(target)
			if err != nil {
				return err
			}
			if old != next {
				return errors.New("hashed asset path changed content; publication would break an existing page")
			}
			return nil
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			source.Close()
			return err
		}
		copied, copyErr := io.Copy(file, io.LimitReader(source, info.Size()+1))
		source.Close()
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if copied != info.Size() {
			return errors.New("existing static asset changed during retention")
		}
		return closeErr
	})
}

func (m *Manager) publishNativeStatic(id string, release NativeReleaseRef) error {
	a, err := m.nativeApp(id)
	if err != nil {
		return err
	}
	if _, err = m.nativeStaticPolicy(a); err != nil {
		return err
	}
	current := filepath.Join(m.cfg.Native.Root, a.Name, "current")
	before, err := filepath.EvalSymlinks(current)
	if err != nil {
		return err
	}
	if release.BasePath != "" && release.BasePath != before {
		return errors.New("an external deployment changed the native release during build; publication refused")
	}
	releases := filepath.Join(m.cfg.Native.Root, a.Name, "releases")
	real, resolveErr := filepath.EvalSymlinks(release.Path)
	if resolveErr != nil || real != release.Path || filepath.Clean(release.Path) != release.Path || !strings.HasPrefix(release.Path, releases+string(filepath.Separator)) || regularFile(filepath.Join(release.Path, "public/index.html"), false) != nil {
		return errors.New("native publication requires a complete local release")
	}
	if err := validateNativeStaticArtifact(filepath.Join(release.Path, "public")); err != nil {
		return err
	}
	if err := m.retainNativeStaticAssets(a, release.Path); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.data.NativeReleases[id]
	if old.Current.Path == "" {
		old.Current = NativeReleaseRef{ID: filepath.Base(before), Path: before}
	}
	if old.Current.Path != before {
		return errors.New("an external deployment changed the native release; ownership needs review")
	}
	after := NativeReleaseHistory{Current: release, Previous: old.Current}
	if m.data.NativeTransitions == nil {
		m.data.NativeTransitions = map[string]NativeReleaseTransition{}
	}
	m.data.NativeTransitions[id] = NativeReleaseTransition{Before: old, After: after}
	if err = m.save(); err != nil {
		delete(m.data.NativeTransitions, id)
		return err
	}
	if err = replaceNativeCurrent(current, before, release.Path); err != nil {
		return err
	}
	if m.data.NativeReleases == nil {
		m.data.NativeReleases = map[string]NativeReleaseHistory{}
	}
	m.data.NativeReleases[id] = after
	delete(m.data.NativeTransitions, id)
	if err = m.save(); err != nil {
		m.data.NativeTransitions[id] = NativeReleaseTransition{Before: old, After: after, Error: "Publication completed; inventory finalization requires recovery."}
		return err
	}
	return nil
}

func replaceNativeCurrent(link, expected, target string) error {
	actual, err := filepath.EvalSymlinks(link)
	if err != nil || actual != expected {
		return errors.New("native current release changed during build; publication refused")
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return errors.New("native current path must remain a symlink")
	}
	temporary := link + ".lidza-" + newID()
	if err = os.Symlink(target, temporary); err != nil {
		return err
	}
	defer os.Remove(temporary)
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if err = os.Lchown(temporary, int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
	}
	if err = os.Rename(temporary, link); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(link))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func nativeStaticFileDigest(path string) ([32]byte, error) {
	var result [32]byte
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(file, (2<<30)+1))
	if err != nil {
		return result, err
	}
	if count > 2<<30 {
		return result, errors.New("static asset exceeds the publication budget")
	}
	copy(result[:], hash.Sum(nil))
	return result, nil
}

// Recovery reconciles only encrypted metadata with the actual current symlink.
// It never repeats publication, runs hooks, or rolls back files automatically.
func (m *Manager) recoverNativePublications() error {
	changed := false
	for id, transition := range m.data.NativeTransitions {
		a, exists := m.data.NativeApps[id]
		if !exists || m.cfg.Native == nil {
			continue
		}
		current, err := filepath.EvalSymlinks(filepath.Join(m.cfg.Native.Root, a.Name, "current"))
		if err == nil && current == transition.After.Current.Path {
			if m.data.NativeReleases == nil {
				m.data.NativeReleases = map[string]NativeReleaseHistory{}
			}
			m.data.NativeReleases[id] = transition.After
			delete(m.data.NativeTransitions, id)
			changed = true
		} else if err == nil && current == transition.Before.Current.Path {
			if m.data.NativeReleases == nil {
				m.data.NativeReleases = map[string]NativeReleaseHistory{}
			}
			m.data.NativeReleases[id] = transition.Before
			delete(m.data.NativeTransitions, id)
			changed = true
		} else if transition.Error != "Native release changed outside the interrupted publication; manual review required." {
			transition.Error = "Native release changed outside the interrupted publication; manual review required."
			m.data.NativeTransitions[id] = transition
			changed = true
		}
	}
	if changed {
		return m.save()
	}
	return nil
}

func (m *Manager) rollbackNativeStatic(id string) error {
	a, err := m.nativeApp(id)
	if err != nil {
		return err
	}
	if _, err = m.nativeStaticPolicy(a); err != nil {
		return err
	}
	m.mu.Lock()
	if m.nativeOperations[id] {
		m.mu.Unlock()
		return errors.New("native deployment is active")
	}
	if _, pending := m.data.NativeTransitions[id]; pending {
		m.mu.Unlock()
		return errors.New("native publication recovery requires review")
	}
	history := m.data.NativeReleases[id]
	if history.Previous.Path == "" {
		m.mu.Unlock()
		return errors.New("no retained native release is available")
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[id] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.nativeOperations, id); m.mu.Unlock() }()
	previous := history.Previous
	previous.BasePath = ""
	return m.publishNativeStatic(id, previous)
}

func (m *Manager) nativeStaticDeployRoute(w http.ResponseWriter, r *http.Request) {
	var request DeployRequest
	if err := Decode(w, r, &request); err != nil {
		Fail(w, 400, err)
		return
	}
	deployment, err := m.enqueueNativeStatic(r.PathValue("id"), request)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	JSON(w, 202, deployment)
}

func validateNativeStaticArtifact(root string) error {
	var size int64
	count := 0
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("static output contains a symlink or special file")
		}
		size += info.Size()
		count++
		if size > 2<<30 || count > 100000 {
			return errors.New("static output exceeds the reviewed publication budget")
		}
		return nil
	})
}

// A host-generated pin is inherited only when the repository omits it. Explicit
// changes, symlinks, new pins and build-time mutations still require review.
func preserveNativeRuntimePins(base, candidate string, inherit bool, uid, gid int) error {
	for _, name := range []string{".node-version", ".tool-versions"} {
		beforePath, afterPath := filepath.Join(base, name), filepath.Join(candidate, name)
		before, beforeErr := os.ReadFile(beforePath)
		_, afterErr := os.Lstat(afterPath)
		if os.IsNotExist(beforeErr) && os.IsNotExist(afterErr) {
			continue
		}
		if beforeErr != nil || regularFile(beforePath, false) != nil {
			return errors.New("existing runtime pin is unavailable; review the runtime first")
		}
		if os.IsNotExist(afterErr) && inherit {
			if err := os.WriteFile(afterPath, before, 0644); err != nil {
				return err
			}
			if err := os.Chown(afterPath, uid, gid); err != nil {
				return err
			}
		}
		if regularFile(afterPath, false) != nil {
			return errors.New("requested runtime pin requires review")
		}
		after, err := os.ReadFile(afterPath)
		if err != nil || string(before) != string(after) {
			return errors.New("the requested runtime version differs from the existing release; review the runtime change first")
		}
	}
	return nil
}

// buildNativeStatic runs a static site's build steps in the host's build slot.
func (m *Manager) buildNativeStatic(ctx context.Context, a NativeApplication, candidate, id string) error {
	log := deploymentNoteWriter{ctx}
	release, err := m.acquireBuildSlot(ctx, a.ID, log)
	if err != nil {
		return err
	}
	defer release()
	for _, args := range [][]string{{"npm", "ci", "--no-audit", "--no-fund"}, {"npm", "run", "build"}} {
		buildLogf(log, "==> %s", buildStepName(args))
		if _, err := m.runNativeBuild(ctx, a, candidate, id, args); err != nil {
			return err
		}
	}
	return nil
}
