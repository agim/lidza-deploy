package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// nativeReleaseIntent is the queued work for one release job.
type nativeReleaseIntent struct {
	Ref      string // branch or commit to deploy
	Release  string // release to roll back to; "" is the previous one
	Rollback bool
}

// nativeReleaseRequest is the body of the release and rollback routes. It
// carries the same checkout credentials as other deployments.
type nativeReleaseRequest struct {
	DeployRequest
	Ref     string `json:"ref,omitempty"`
	Release string `json:"release,omitempty"`
}

type nativeReleaseEntry struct {
	ID      string `json:"id"`
	Commit  string `json:"commit,omitempty"`
	Current bool   `json:"current,omitempty"`
}

type nativeReleaseView struct {
	ApplicationID string               `json:"application_id"`
	Current       string               `json:"current,omitempty"`
	Previous      string               `json:"previous,omitempty"`
	Branch        string               `json:"branch"`
	Releases      []nativeReleaseEntry `json:"releases"`
}

var nativeReleaseCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// nativeReleasePolicy returns the registered application and its enabled,
// consistent release policy. The policy must describe the registered
// application exactly: same root, repository and deployed branch.
func (m *Manager) nativeReleasePolicy(id string) (NativeApplication, NativeReleasePolicy, error) {
	var policy NativeReleasePolicy
	m.mu.Lock()
	a, found := m.data.NativeApps[id]
	override := m.data.NativeSettings[id]
	m.mu.Unlock()
	if !found {
		return a, policy, errors.New("native application is not registered")
	}
	if m.cfg.Native == nil {
		return a, policy, errors.New("native management is not configured")
	}
	// Reviewed configuration first, then applications this agent created.
	policy, configured := m.cfg.Native.Releases[id]
	created := false
	if !configured {
		m.mu.Lock()
		policy, created = m.data.NativeCreated[id]
		m.mu.Unlock()
	}
	if !configured && !created || !policy.Enabled {
		return a, policy, errors.New("native release management is not configured for this application")
	}
	if err := policy.Validate(); err != nil {
		return a, policy, err
	}
	branch := a.Branch
	if override.Branch != "" {
		// A branch saved from the panel replaces the policy's branch.
		branch = override.Branch
		policy.Branch = override.Branch
	}
	root := filepath.Join(m.cfg.Native.Root, a.Name)
	if created {
		root = filepath.Join(m.nativeAppsRoot(), id)
	}
	switch {
	case created && (policy.User != nativeAppUser(id) || !policy.SystemUnits()):
		return a, policy, errors.New("created application policy must run as its own account with system units")
	case policy.Root != root:
		return a, policy, errors.New("native release policy root differs from the registered application directory")
	case !strings.EqualFold(policy.Repository, a.Repository):
		return a, policy, errors.New("native release policy repository differs from the registered application")
	case policy.Branch != branch:
		return a, policy, errors.New("native release policy branch differs from the application's deployed branch")
	case policy.Domain != "" && policy.Domain != a.Domain:
		return a, policy, errors.New("native release policy domain differs from the registered application")
	}
	return a, policy, nil
}

func (m *Manager) nativeReleaseLayout(account nativeReleaseAccount, policy NativeReleasePolicy) release.Layout {
	return release.Layout{Root: policy.Root, Owner: release.Owner{UID: account.UID, GID: account.GID}}
}

func (m *Manager) enqueueNativeRelease(id string, req nativeReleaseRequest, rollback bool) (Deployment, error) {
	a, policy, err := m.nativeReleasePolicy(id)
	if err != nil {
		return Deployment{}, err
	}
	intent := nativeReleaseIntent{Rollback: rollback}
	if rollback {
		if req.Release != "" && !release.ValidID(req.Release) {
			return Deployment{}, errors.New("invalid release identifier")
		}
		intent.Release = req.Release
	} else {
		if err = validateCheckoutCredential(req.DeployRequest); err != nil {
			return Deployment{}, err
		}
		intent.Ref = policy.Branch
		if req.Ref != "" && req.Ref != policy.Branch {
			if !nativeReleaseCommitPattern.MatchString(req.Ref) {
				return Deployment{}, errors.New("release ref must be the deployed branch or an exact commit")
			}
			intent.Ref = req.Ref
		}
	}
	if len(req.Key) > 200 || len(req.Token) > 4096 {
		return Deployment{}, errors.New("request exceeds limit")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
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
	kind := "deploy"
	if rollback {
		kind = "rollback"
	}
	d := Deployment{ID: newID(), AppID: id, Status: "queued", Kind: kind, Key: req.Key, Branch: policy.Branch, Domain: a.Domain, Created: time.Now().UTC()}
	m.data.Deployments = append(m.data.Deployments, d)
	if err = m.save(); err != nil {
		m.data.Deployments = m.data.Deployments[:len(m.data.Deployments)-1]
		return Deployment{}, err
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[id] = true
	app := App{ID: id, Native: &a, Runtime: policy.Runtime, Repository: a.Repository, Branch: policy.Branch, Domain: a.Domain}
	m.queue <- job{native: true, nativeRelease: &intent, app: app, deployment: d.ID, credentials: req.DeployRequest, token: req.Token}
	return d, nil
}

func (m *Manager) workNativeRelease(j job) {
	defer func() { m.mu.Lock(); delete(m.nativeOperations, j.app.ID); m.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(m.ctx, 60*time.Minute)
	defer cancel()
	ctx = m.deploymentDiagnostics(ctx, j)
	m.mu.Lock()
	err := m.update(j.deployment, "building", nil, nil)
	m.mu.Unlock()
	if err != nil {
		return
	}
	_, policy, err := m.nativeReleasePolicy(j.app.ID)
	if err == nil && j.credentials.CredentialURL != "" && !j.nativeRelease.Rollback {
		j.token, err = m.checkoutCredential(ctx, j.credentials)
	}
	var plan release.Plan
	if err == nil {
		plan, err = m.nativeReleasePlan(ctx, j.app.ID, policy, j.deployment, j.token)
	}
	j.token = ""
	var result release.Result
	if err == nil {
		if j.nativeRelease.Rollback {
			result, err = release.Rollback(ctx, plan, j.nativeRelease.Release)
		} else {
			plan.Ref = j.nativeRelease.Ref
			result, err = release.Deploy(ctx, plan)
		}
	}
	if result.RolledBack {
		commandDiagnostic(ctx, "release", "The new release failed its checks; the previous release was republished and is serving.")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if result.Release != "" && !result.RolledBack && err == nil {
		m.recordNativeReleaseLocked(j.app.ID, plan.Layout, result)
	}
	if err == nil {
		err = m.update(j.deployment, "live", &Release{ID: result.Release, Commit: result.Commit}, nil)
	} else {
		_ = m.update(j.deployment, "failed", nil, err)
	}
}

// recordNativeReleaseLocked keeps the dashboard's current/previous view in
// step with the published release. The release tree itself is authoritative.
func (m *Manager) recordNativeReleaseLocked(id string, l release.Layout, result release.Result) {
	if m.data.NativeReleases == nil {
		m.data.NativeReleases = map[string]NativeReleaseHistory{}
	}
	history := NativeReleaseHistory{Current: NativeReleaseRef{ID: result.Release, Commit: result.Commit, Path: l.Release(result.Release)}}
	if result.Previous != "" {
		commit, _ := l.Revision(result.Previous)
		history.Previous = NativeReleaseRef{ID: result.Previous, Commit: commit, Path: l.Release(result.Previous)}
	}
	m.data.NativeReleases[id] = history
}

// nativeReleaseProfile selects the engine profile for the policy's runtime.
func (m *Manager) nativeReleaseProfile(policy NativeReleasePolicy, l release.Layout) (release.Profile, error) {
	var shared *release.Shared
	if len(policy.Shared) > 0 {
		shared = &release.Shared{Dirs: policy.Shared}
	}
	rails := func() (release.Profile, error) {
		template, err := nativeReleaseJobTemplate(policy.EnvFiles)
		if err != nil {
			return release.Profile{}, err
		}
		return release.Rails(release.RailsOptions{Shared: shared, ScheduleVariables: map[string]string{"job_template": template}}), nil
	}
	switch policy.Runtime {
	case "rails":
		return rails()
	case "node":
		profile := release.Node()
		if shared != nil {
			profile.Shared = *shared
		}
		return profile, nil
	case "go":
		return release.Go(release.GoOptions{Binary: filepath.Base(policy.Root), Shared: shared}), nil
	case "static":
		return release.Static(policy.Output), nil
	case "auto":
		current, err := l.CurrentID()
		if err != nil || current == "" {
			return release.Profile{}, errors.New("automatic runtime detection needs a published current release")
		}
		profile, err := release.Detect(l.Release(current))
		if err != nil {
			return profile, err
		}
		if profile.Name == "rails" {
			return rails()
		}
		if profile.Name == "go" {
			return release.Go(release.GoOptions{Binary: filepath.Base(policy.Root), Shared: shared}), nil
		}
		if shared != nil {
			profile.Shared = *shared
		}
		return profile, nil
	}
	return release.Profile{}, fmt.Errorf("unsupported native release runtime %q", policy.Runtime)
}

var nativeReleaseEnvFilePattern = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)

// nativeReleaseJobTemplate is the Whenever job template for scheduled jobs:
// a login shell that loads the application's environment files, as the
// owner's service units and console commands do, before running the job.
// Cron itself provides no application environment.
func nativeReleaseJobTemplate(envFiles []string) (string, error) {
	if len(envFiles) == 0 {
		return "/bin/bash -l -c ':job'", nil
	}
	parts := []string{"set -a"}
	for _, file := range envFiles {
		if !nativeReleaseEnvFilePattern.MatchString(file) {
			return "", fmt.Errorf("environment file path %q cannot be used in scheduled jobs", file)
		}
		parts = append(parts, ". "+file)
	}
	parts = append(parts, "set +a", ":job")
	return "/bin/bash -l -c '" + strings.Join(parts, "; ") + "'", nil
}

func (m *Manager) nativeReleasePlan(ctx context.Context, id string, policy NativeReleasePolicy, deployment, token string) (release.Plan, error) {
	var plan release.Plan
	account, err := m.nativeReleaseAccount(policy)
	if err != nil {
		return plan, err
	}
	env, err := nativeReleaseEnvironment(account, policy)
	if err != nil {
		return plan, err
	}
	layout := m.nativeReleaseLayout(account, policy)
	profile, err := m.nativeReleaseProfile(policy, layout)
	if err != nil {
		return plan, err
	}
	source := release.Source{URL: "https://github.com/" + policy.Repository + ".git", Token: token}
	if m.nativeReleaseSource != nil {
		source = m.nativeReleaseSource(policy, token)
	}
	plan = release.Plan{
		App:      policy.ScheduleIdentifier(),
		Layout:   layout,
		Source:   source,
		Ref:      policy.Branch,
		Profile:  profile,
		Env:      env,
		Exec:     nativeReleaseExecutor{m: m, account: account, root: policy.Root, id: deployment, memory: policy.BuildMemoryMiB},
		Prepare:  nativeReleasePinToolchains(account.Home),
		Services: m.nativeReleaseServices(account, policy),
		Units:    policy.Units,
		Settle:   45 * time.Second,
		Crontab:  nativeReleaseCrontab{m: m, user: account.Name, identifier: policy.ScheduleIdentifier()},
		Backup:   m.nativeReleaseBackup(id, policy),
		Health:   m.nativeReleaseHealth(policy.Domain, policy.HealthPath),
		Keep:     policy.Keep,
		Log:      nativeReleaseLog(ctx),
	}
	if policy.ManagedToolchains() {
		plan.Prepare = m.nativeReleaseManagedToolchains(account, policy, plan.Log)
	}
	if policy.SystemUnits() {
		m.nativeReleaseSystemPlan(&plan, id, policy)
	}
	return plan, nil
}

// nativeReleaseSystemPlan adapts a plan to system units: the agent controls
// them as root, starts enabled units on the first release, and checks the
// web process on its loopback port when the policy names one.
func (m *Manager) nativeReleaseSystemPlan(plan *release.Plan, id string, policy NativeReleasePolicy) {
	services := m.nativeSystemServices(policy)
	health := plan.Health
	if policy.Port != "" {
		health = nativeLoopbackHealth(policy.Port, policy.Domain, policy.HealthPath)
	}
	if policy.Runtime == "static" {
		health = nil
	}
	units, preserve, log := slices.Clone(policy.Units), slices.Clone(policy.Preserve), plan.Log
	plan.Services, plan.Units = nil, nil
	plan.Health = func(ctx context.Context) error {
		if _, err := nativeSystemRestart(ctx, services, units, preserve, 45*time.Second, log); err != nil {
			return err
		}
		if health == nil {
			return nil
		}
		return health(ctx)
	}
	m.mu.Lock()
	_, created := m.data.NativeCreated[id]
	m.mu.Unlock()
	if created && len(policy.Databases) == 0 {
		// A new application without provisioned databases has nothing the
		// agent could back up (an external database, or none yet).
		plan.Backup, plan.SkipBackup = nil, true
	}
}

func (m *Manager) nativeReleases(id string) (nativeReleaseView, error) {
	_, policy, err := m.nativeReleasePolicy(id)
	if err != nil {
		return nativeReleaseView{}, err
	}
	account, err := m.nativeReleaseAccount(policy)
	if err != nil {
		return nativeReleaseView{}, err
	}
	l := m.nativeReleaseLayout(account, policy)
	view := nativeReleaseView{ApplicationID: id, Branch: policy.Branch, Releases: []nativeReleaseEntry{}}
	if view.Current, err = l.CurrentID(); err != nil {
		return view, err
	}
	if previous, err := l.Previous(); err == nil {
		view.Previous = previous
	}
	ids, err := l.List()
	if err != nil {
		return view, err
	}
	for i := len(ids) - 1; i >= 0; i-- {
		commit, _ := l.Revision(ids[i])
		view.Releases = append(view.Releases, nativeReleaseEntry{ID: ids[i], Commit: commit, Current: ids[i] == view.Current})
	}
	return view, nil
}

// nativeReleaseManaged reports whether release management applies to the app.
func (m *Manager) nativeReleaseManaged(id string) bool {
	_, _, err := m.nativeReleasePolicy(id)
	return err == nil
}

func (m *Manager) nativeReleaseRoute(rollback bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request nativeReleaseRequest
		if r.ContentLength != 0 {
			if err := Decode(w, r, &request); err != nil {
				Fail(w, 400, err)
				return
			}
		}
		deployment, err := m.enqueueNativeRelease(r.PathValue("id"), request, rollback)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, deployment)
	}
}

func (m *Manager) nativeReleasesRoute(w http.ResponseWriter, r *http.Request) {
	view, err := m.nativeReleases(r.PathValue("id"))
	if err != nil {
		Fail(w, 409, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, 200, view)
}
