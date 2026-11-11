package agent

import (
	"context"
	"errors"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// Applying a native application's saved settings (settings_apply.go). The
// environment files were rewritten by the save; a restart job restarts the
// release's units the way a deploy does after publishing (release.Restart):
// in order, each waits until it runs again, socket-activated servers keep
// their socket, then the health check. It is a job on the deployment queue
// (kind "restart") so the console follows it like a deployment.

// applyNativeSettings applies what a native settings save changed.
func (m *Manager) applyNativeSettings(id string, subject settingsSubject, changes []string) SettingsApplyResult {
	effect := subject.classify(changes)
	result := settingsResult(effect, changes)
	if effect < effectRestart {
		return result
	}
	if _, stopped := m.nativeStopped(id); stopped {
		result.Applied, result.Message = SettingsAppliedImmediate, "Settings saved. The application is stopped; starting it applies them."
		if effect == effectDeploy {
			result.Message = "Settings saved. The application is stopped; start it, then deploy to apply them."
		}
		return result
	}
	if effect == effectDeploy {
		return result
	}
	if !m.nativeReleaseManaged(id) {
		// A static publication has no process; its files change on deploy.
		return settingsResult(effectDeploy, changes)
	}
	if reason := m.nativeLifecycleAllowed(id); reason != "" {
		result.Applied = SettingsAppliedRestartRequired
		result.Message = "Settings saved. Restart the application to apply them; the panel cannot restart it: " + reason
		return result
	}
	d, err := m.enqueueNativeRestart(id, changes)
	if err != nil {
		result.Applied, result.Error = SettingsAppliedRestartRequired, err.Error()
		result.Message = "Settings saved, but the restart could not start; restart the application to apply them."
		return result
	}
	if d.ID == "" {
		result.Message = "Settings saved. This application has no process to restart."
		return result
	}
	return withDeployment(SettingsApplyResult{Applied: SettingsAppliedRestart, Message: "Settings saved. Restarting the application with them.", Changes: changes}, d)
}

// enqueueNativeRestart queues a restart of a release-managed application;
// an empty deployment when the policy names no unit.
func (m *Manager) enqueueNativeRestart(id string, changes []string) (Deployment, error) {
	a, policy, err := m.nativeReleasePolicy(id)
	if err != nil {
		return Deployment{}, err
	}
	if len(policy.Units) == 0 {
		return Deployment{}, nil
	}
	if m.upgradePending() {
		return Deployment{}, errors.New("agent upgrade in progress")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nativeOperations[id] || m.busy(id) {
		return Deployment{}, errors.New("another operation on this application is running")
	}
	if len(m.queue) == cap(m.queue) {
		return Deployment{}, errors.New("the deployment queue is full")
	}
	d := Deployment{ID: newID(), AppID: id, Status: "queued", Kind: "restart", Branch: policy.Branch, Domain: a.Domain, Created: time.Now().UTC(), Changes: changes}
	m.data.Deployments = append(m.data.Deployments, d)
	if err := m.save(); err != nil {
		m.data.Deployments = m.data.Deployments[:len(m.data.Deployments)-1]
		return Deployment{}, err
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[id] = true
	app := App{ID: id, Native: &a, Runtime: policy.Runtime, Repository: a.Repository, Branch: policy.Branch, Domain: a.Domain}
	m.queue <- job{native: true, nativeRestart: true, app: app, deployment: d.ID}
	return d, nil
}

// workNativeRestart runs a queued restart job.
func (m *Manager) workNativeRestart(j job) {
	defer func() { m.mu.Lock(); delete(m.nativeOperations, j.app.ID); m.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Minute)
	defer cancel()
	ctx = m.deploymentDiagnostics(ctx, j)
	m.mu.Lock()
	err := m.update(j.deployment, "building", nil, nil)
	m.mu.Unlock()
	if err != nil {
		return
	}
	_, policy, err := m.nativeReleasePolicy(j.app.ID)
	var plan release.Plan
	if err == nil {
		plan, err = m.nativeReleasePlan(ctx, j.app.ID, policy, j.deployment, "")
	}
	var result release.Result
	if err == nil {
		result, err = m.nativeRestart(ctx, plan)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		_ = m.update(j.deployment, "failed", nil, err)
		return
	}
	_ = m.update(j.deployment, "live", &Release{ID: result.Release, Commit: result.Commit}, nil)
}

// nativeRestart is release.Restart; tests replace it.
func (m *Manager) nativeRestart(ctx context.Context, plan release.Plan) (release.Result, error) {
	if m.nativeRestartHook != nil {
		return m.nativeRestartHook(ctx, plan)
	}
	return release.Restart(ctx, plan)
}
