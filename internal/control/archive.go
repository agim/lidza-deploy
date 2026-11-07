package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/audit"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/packs/db"
)

// Deleted applications. Deleting an application (DELETE
// /api/control/apps/{id}) asks its agent to archive it (agent/archive.go):
// the settings, files and databases are kept with every login disabled,
// everything else is deleted and the hostnames are free at once. The panel
// keeps its own part of the snapshot (repository, webhook, auto-deploy) in
// an archive record next to the agent's. A restore within the retention
// recreates the application from both, registers its webhook again and
// deploys it; the agents purge expired archives themselves, and the panel
// records each purge it sees in the audit log.

// archiveAgentTimeout bounds an agent's archive operation.
const archiveAgentTimeout = 10 * time.Minute

// ControlArchive is the panel's record of a deleted application.
type ControlArchive struct {
	ID        string               `json:"id"` // the agent's archive id
	AppID     string               `json:"app_id"`
	ServerID  string               `json:"server_id"`
	DeletedAt time.Time            `json:"deleted_at"`
	DeletedBy string               `json:"deleted_by,omitempty"`
	Reason    string               `json:"reason,omitempty"`
	ExpiresAt time.Time            `json:"expires_at"`
	App       Application          `json:"app"`
	Agent     agent.AppArchiveView `json:"agent"`
	// Webhook says what happened to the repository webhook.
	Webhook string `json:"webhook,omitempty"`
	// MoveCopies says what happened to the data moves left of the
	// application on other servers.
	MoveCopies string `json:"move_copies,omitempty"`
}

// ArchiveView is one row of GET /api/control/archives.
type ArchiveView struct {
	agent.AppArchiveView
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
	DaysLeft   int    `json:"days_left"`
	Webhook    string `json:"webhook,omitempty"`
	MoveCopies string `json:"move_copies,omitempty"`
	// Unavailable is set when the server did not answer: the row is the
	// panel's last copy.
	Unavailable bool `json:"unavailable,omitempty"`
}

// ArchivesView is GET /api/control/archives.
type ArchivesView struct {
	Archives      []ArchiveView      `json:"archives"`
	Purged        []ArchivePurgeView `json:"purged"`
	RetentionDays int                `json:"retention_days"`
	Unavailable   []string           `json:"unavailable_servers"`
}

// ArchivePurgeView is a purge an agent reported.
type ArchivePurgeView struct {
	agent.ArchivePurge
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
}

// RestoreView is POST /api/control/archives/{id}/restore.
type RestoreView struct {
	Result      agent.ArchiveRestoreResult `json:"result"`
	App         Application                `json:"app"`
	Webhook     string                     `json:"webhook,omitempty"`
	Deployment  *agent.Deployment          `json:"deployment,omitempty"`
	DeployError string                     `json:"deploy_error,omitempty"`
}

func daysLeft(expires, now time.Time) int {
	left := expires.Sub(now)
	if left <= 0 {
		return 0
	}
	return int((left + 24*time.Hour - 1) / (24 * time.Hour))
}

func currentUserID(ctx context.Context) string {
	if u := auth.CurrentUser(ctx); u != nil {
		if email, _ := u.Claims["email"].(string); email != "" {
			return email
		}
		return u.ID
	}
	return ""
}

// archiveGitHubToken is the operator's GitHub token; tests replace it.
var archiveGitHubToken = func(c *Control, ctx context.Context) (string, error) { return c.token(ctx) }

// recordArchiveAudit records an archive event when the audit log runs.
func recordArchiveAudit(ctx context.Context, action, appID string, meta map[string]string) {
	recorder, ok := lidza.Optional[*audit.Audit](ctx)
	if !ok || recorder == nil {
		return
	}
	if err := recorder.Record(ctx, audit.Event{Action: action, Scope: fleetScope, Resource: "app/" + appID, Meta: meta}); err != nil {
		log.Printf("audit %s %s: %v", action, appID, err)
	}
}

// deleteAppErrors forgets the console errors stored for the application.
var deleteAppErrors = func(ctx context.Context, serverID, appID string) {
	if _, ok := lidza.Optional[*db.DB](ctx); !ok {
		return
	}
	if _, err := db.From(ctx).Exec(ctx, `DELETE FROM app_error WHERE extra->>'deploy_server'=$1 AND extra->>'deploy_app'=$2`, serverID, appID); err != nil {
		log.Printf("delete %s: stored console errors not removed: %v", appID, err)
	}
}

// exclusiveBackupPrefixes lists the databases of server whose off-site
// folder no other server could share: every server answered and none has a
// database of that id. The agent deletes those folders whole.
func (c *Control) exclusiveBackupPrefixes(r *http.Request, server string) []string {
	own := map[string]bool{}
	others := map[string]bool{}
	for _, result := range c.readFleet(r, "/v1/databases") {
		var views []agent.DatabaseView
		if result.Err != nil || json.Unmarshal(result.Body, &views) != nil {
			return nil
		}
		for _, v := range views {
			if result.Server.ID == server {
				own[v.AppID] = true
			} else {
				others[v.AppID] = true
			}
		}
	}
	var out []string
	for id := range own {
		if !others[id] {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// deleteApp archives an application (DELETE /api/control/apps/{id}).
func (c *Control) deleteApp(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Confirm string `json:"confirm"`
		Reason  string `json:"reason"`
	}
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	id := r.PathValue("id")
	if input.Confirm != id {
		agent.Fail(w, 400, errors.New("type the application id to delete it"))
		return
	}
	if len(input.Reason) > 1000 {
		agent.Fail(w, 400, errors.New("the reason is limited to 1000 characters"))
		return
	}
	c.mu.Lock()
	a, ok := c.data.Apps[id]
	if !ok {
		c.mu.Unlock()
		agent.JSON(w, 200, map[string]string{"status": "deleted"})
		return
	}
	if _, moving := c.activeMoveLocked(id); moving {
		c.mu.Unlock()
		agent.Fail(w, 409, errors.New("finish or abort the application's move before deleting it"))
		return
	}
	if a.PreviewParent != "" {
		// A preview is removed outright; its pull request recreates it.
		c.mu.Unlock()
		if err := c.agentCall(r, a.ServerID, "DELETE", "/v1/previews/"+id, nil, nil); err != nil {
			failAgent(w, err)
			return
		}
		c.mu.Lock()
		delete(c.data.Apps, id)
		err := c.save()
		c.mu.Unlock()
		if err != nil {
			agent.Fail(w, 500, err)
			return
		}
		agent.JSON(w, 200, map[string]string{"status": "removed"})
		return
	}
	autoDeploy := a.AutoDeploy
	if !a.Retiring && a.Native == nil {
		old := a
		a.Retiring, a.AutoDeploy = true, false
		c.data.Apps[id] = a
		if err := c.save(); err != nil {
			c.data.Apps[id] = old
			c.mu.Unlock()
			agent.Fail(w, 500, err)
			return
		}
	}
	var previews []Application
	for _, child := range c.data.Apps {
		if child.PreviewParent == id {
			previews = append(previews, child)
		}
	}
	snapshot := c.data.Apps[id]
	c.mu.Unlock()
	for _, child := range previews {
		if err := c.agentCall(r, child.ServerID, "DELETE", "/v1/previews/"+child.ID, nil, nil); err != nil {
			failAgent(w, err)
			return
		}
		c.mu.Lock()
		delete(c.data.Apps, child.ID)
		c.mu.Unlock()
	}
	by := currentUserID(r.Context())
	request := agent.AppArchiveRequest{DeletedBy: by, Reason: strings.TrimSpace(input.Reason), ExclusiveBackupPrefixes: c.exclusiveBackupPrefixes(r, a.ServerID)}
	var archived agent.AppArchiveView
	slow := r.WithContext(withAgentTimeout(r.Context(), archiveAgentTimeout))
	if err := c.agentCall(slow, a.ServerID, "POST", "/v1/apps/"+id+"/archive", request, &archived); err != nil {
		var refused *agentHTTPError
		if errors.As(err, &refused) && refused.status == http.StatusNotFound && refused.message == "" {
			agent.Fail(w, 409, errors.New("the server's agent cannot archive applications yet; update it first"))
			return
		}
		failAgent(w, err)
		return
	}
	webhook := c.removeWebhook(r.Context(), snapshot)
	copies := c.removeMoveCopies(r, id, a.ServerID)
	deleteAppErrors(r.Context(), a.ServerID, id)
	record := ControlArchive{ID: archived.ID, AppID: id, ServerID: a.ServerID, DeletedAt: archived.DeletedAt, DeletedBy: by, Reason: request.Reason,
		ExpiresAt: archived.ExpiresAt, App: snapshot, Agent: archived, Webhook: webhook, MoveCopies: copies}
	record.App.Retiring, record.App.Secret, record.App.HookID, record.App.AutoDeploy = false, "", 0, autoDeploy
	c.mu.Lock()
	delete(c.data.Apps, id)
	for _, kind := range []string{"database:", "health:", "deploy:", "security-burst:", "security-review:"} {
		delete(c.data.Incidents, kind+a.ServerID+":"+id)
	}
	if c.data.AppArchives == nil {
		c.data.AppArchives = map[string]ControlArchive{}
	}
	c.data.AppArchives[record.ID] = record
	err := c.save()
	c.mu.Unlock()
	if err != nil {
		agent.Fail(w, 500, errors.New("the server archived the application; retry the deletion to finish saving the panel's record"))
		return
	}
	recordArchiveAudit(r.Context(), "app.delete", id, map[string]string{"archive": record.ID, "server": a.ServerID, "expires": record.ExpiresAt.Format(time.RFC3339)})
	agent.JSON(w, 200, c.archiveView(record, time.Now()))
}

// removeMoveCopies deletes the data moves left of the application on the
// other servers: copies of the data the archive keeps.
func (c *Control) removeMoveCopies(r *http.Request, appID, server string) string {
	removed, failed := 0, []string{}
	for _, result := range c.readFleet(r, "/v1/retained-resources") {
		if result.Server.ID == server {
			continue
		}
		var rows []agent.RetainedResourceView
		if result.Err != nil || json.Unmarshal(result.Body, &rows) != nil {
			continue
		}
		for _, row := range rows {
			if row.AppID != appID || row.Origin == nil || row.Origin.MoveID == "" {
				continue
			}
			body := agent.RetainedRemoveRequest{Confirm: row.ID, MoveLeftover: true}
			if err := c.agentCall(r, result.Server.ID, "DELETE", "/v1/retained-resources/"+row.ID, body, nil); err != nil {
				failed = append(failed, c.serverName(result.Server.ID)+": "+row.ID)
				continue
			}
			removed++
		}
	}
	switch {
	case removed == 0 && len(failed) == 0:
		return ""
	case len(failed) == 0:
		return fmt.Sprintf("%d move copies on other servers are being removed", removed)
	}
	return fmt.Sprintf("%d move copies are being removed; %s could not be (remove them from the server's retained resources)", removed, strings.Join(failed, ", "))
}

// removeWebhook deletes the application's repository webhook. A GitHub
// App delivers for the whole installation; its deliveries for a deleted
// application are ignored.
func (c *Control) removeWebhook(ctx context.Context, a Application) string {
	switch {
	case a.GitHubInstallation > 0:
		return "GitHub App deliveries for the repository are ignored while the application is deleted"
	case a.HookID == 0:
		return ""
	}
	token, err := archiveGitHubToken(c, ctx)
	if err != nil || token == "" || c.cfg.GitHub == nil {
		return fmt.Sprintf("the repository webhook %d could not be removed (GitHub is not connected); its deliveries are ignored", a.HookID)
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := c.cfg.GitHub.DeleteHook(bounded, token, a.Repository, a.HookID); err != nil {
		return fmt.Sprintf("the repository webhook %d could not be removed (%v); its deliveries are ignored", a.HookID, err)
	}
	return "the repository webhook was removed"
}

func (c *Control) archiveView(record ControlArchive, now time.Time) ArchiveView {
	v := ArchiveView{AppArchiveView: record.Agent, ServerID: record.ServerID, ServerName: c.serverName(record.ServerID), Webhook: record.Webhook, MoveCopies: record.MoveCopies}
	if v.DeletedBy == "" {
		v.DeletedBy = record.DeletedBy
	}
	if v.Repository == "" {
		v.Repository, v.Branch = record.App.Repository, record.App.Branch
	}
	v.DaysLeft = daysLeft(v.ExpiresAt, now)
	return v
}

// archives lists the deleted applications of every server
// (GET /api/control/archives). Purges the agents report are recorded in
// the audit log once, and the panel's record of them is dropped.
func (c *Control) archives(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	now := time.Now()
	out := ArchivesView{Archives: []ArchiveView{}, Purged: []ArchivePurgeView{}, RetentionDays: int(agent.AppArchiveRetention / (24 * time.Hour)), Unavailable: []string{}}
	answered := map[string]bool{}
	live := map[string]agent.AppArchiveView{}
	purged := map[string]ArchivePurgeView{}
	for _, result := range c.readFleet(r, "/v1/archives") {
		var list agent.AppArchivesView
		if result.Err != nil || json.Unmarshal(result.Body, &list) != nil {
			out.Unavailable = append(out.Unavailable, c.serverName(result.Server.ID))
			continue
		}
		answered[result.Server.ID] = true
		for _, a := range list.Archives {
			live[a.ID] = a
			view := ArchiveView{AppArchiveView: a, ServerID: result.Server.ID, ServerName: c.serverName(result.Server.ID), DaysLeft: daysLeft(a.ExpiresAt, now)}
			out.Archives = append(out.Archives, view)
		}
		for _, p := range list.Purged {
			view := ArchivePurgeView{ArchivePurge: p, ServerID: result.Server.ID, ServerName: c.serverName(result.Server.ID)}
			purged[p.ArchiveID] = view
			out.Purged = append(out.Purged, view)
		}
	}
	c.mu.Lock()
	changed := false
	var audits []ArchivePurgeView
	for i, row := range out.Archives {
		if record, ok := c.data.AppArchives[row.ID]; ok {
			record.Agent = live[row.ID]
			c.data.AppArchives[row.ID] = record
			out.Archives[i].Webhook = record.Webhook
			if out.Archives[i].Repository == "" {
				out.Archives[i].Repository, out.Archives[i].Branch = record.App.Repository, record.App.Branch
			}
			changed = true
		}
	}
	for id, record := range c.data.AppArchives {
		if _, ok := live[id]; ok {
			continue
		}
		if !answered[record.ServerID] {
			view := c.archiveView(record, now)
			view.Unavailable = true
			out.Archives = append(out.Archives, view)
			continue
		}
		// The agent no longer has it: purged (by expiry or on its own).
		if p, ok := purged[id]; ok {
			audits = append(audits, p)
		}
		delete(c.data.AppArchives, id)
		changed = true
	}
	if changed {
		if err := c.save(); err != nil {
			log.Printf("archives: panel records not saved: %v", err)
		}
	}
	c.mu.Unlock()
	for _, p := range audits {
		ctx := audit.System(r.Context(), "archive-"+p.Trigger)
		recordArchiveAudit(ctx, "app.purge", p.AppID, map[string]string{"archive": p.ArchiveID, "server": p.ServerID, "trigger": p.Trigger})
	}
	slices.SortFunc(out.Archives, func(a, b ArchiveView) int { return b.DeletedAt.Compare(a.DeletedAt) })
	slices.SortFunc(out.Purged, func(a, b ArchivePurgeView) int { return b.At.Compare(a.At) })
	agent.JSON(w, 200, out)
}

// archiveServer finds the server of an archive: the panel's record, else
// the server query parameter.
func (c *Control) archiveServer(r *http.Request, id string) (ControlArchive, string, bool) {
	c.mu.Lock()
	record, ok := c.data.AppArchives[id]
	c.mu.Unlock()
	if ok {
		return record, record.ServerID, true
	}
	server := r.URL.Query().Get("server")
	return ControlArchive{}, server, server != ""
}

// hostOwnerLocked names the panel application that uses host, or "".
func (c *Control) hostOwnerLocked(host string) string {
	for id, a := range c.data.Apps {
		if (agent.App{Domain: a.Domain, AdditionalDomains: a.AdditionalDomains}).HasDomain(host) {
			return id
		}
		for _, d := range a.RedirectDomains {
			if strings.EqualFold(d.Host, host) {
				return id
			}
		}
	}
	return ""
}

// restoreArchive restores a deleted application
// (POST /api/control/archives/{id}/restore) and deploys it.
func (c *Control) restoreArchive(w http.ResponseWriter, r *http.Request) {
	var input struct {
		NewID  string `json:"new_id"`
		Domain string `json:"domain"`
	}
	if r.ContentLength != 0 {
		if err := agent.Decode(w, r, &input); err != nil {
			agent.Fail(w, 400, err)
			return
		}
	}
	id := r.PathValue("id")
	record, server, ok := c.archiveServer(r, id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	target := record.AppID
	if input.NewID != "" {
		target = input.NewID
	}
	c.mu.Lock()
	if _, exists := c.data.Apps[target]; exists && target != "" {
		c.mu.Unlock()
		agent.Fail(w, 409, fmt.Errorf("an application named %s exists; restore under another id", target))
		return
	}
	taken := map[string]string{}
	for _, host := range record.Agent.Hostnames {
		if owner := c.hostOwnerLocked(host); owner != "" {
			taken[strings.ToLower(host)] = owner
		}
	}
	c.mu.Unlock()
	request := agent.ArchiveRestoreRequest{NewID: input.NewID, Domain: input.Domain, By: currentUserID(r.Context()), Taken: taken}
	var result agent.ArchiveRestoreResult
	slow := r.WithContext(withAgentTimeout(r.Context(), archiveAgentTimeout))
	if err := c.agentCall(slow, server, "POST", "/v1/archives/"+id+"/restore", request, &result); err != nil {
		failAgent(w, err)
		return
	}
	app := record.App
	if app.ID == "" {
		app = Application{ServerID: server, Repository: record.Agent.Repository, Branch: record.Agent.Branch}
	}
	app.ID, app.ServerID, app.Retiring = result.AppID, server, false
	app.Domain, app.AdditionalDomains = result.Hostnames[0], slices.Clone(result.Hostnames[1:])
	if app.Native != nil {
		native := *app.Native
		native.ID, native.Domain, native.AdditionalDomains = result.AppID, app.Domain, app.AdditionalDomains
		if !result.Adopted {
			// A created application's directory is named after its id; an
			// adopted one keeps its own.
			native.Name = result.AppID
		}
		app.Native = &native
	}
	var redirects []agent.RedirectDomain
	for _, d := range app.RedirectDomains {
		if _, skipped := result.Skipped[d.Host]; !skipped {
			redirects = append(redirects, d)
		}
	}
	app.RedirectDomains = redirects
	app.Generation = random()
	wantHook := record.App.AutoDeploy && record.App.GitHubInstallation == 0
	app.AutoDeploy = record.App.AutoDeploy && record.App.GitHubInstallation > 0
	c.mu.Lock()
	c.data.Apps[app.ID] = app
	delete(c.data.AppArchives, id)
	err := c.save()
	c.mu.Unlock()
	if err != nil {
		agent.Fail(w, 500, errors.New("the server restored the application; the panel's record was not saved, retry"))
		return
	}
	view := RestoreView{Result: result}
	if wantHook {
		view.Webhook = c.registerWebhook(r.Context(), app.ID)
	}
	// An adopted application's processes were started again as they were:
	// nothing to deploy.
	if !result.Adopted {
		if d, err := c.startDeployment(r, app.ID); err != nil {
			view.DeployError = err.Error()
		} else {
			view.Deployment = &d
		}
	}
	recordArchiveAudit(r.Context(), "app.restore", app.ID, map[string]string{"archive": id, "server": server, "skipped": strings.Join(slices.Sorted(maps.Keys(result.Skipped)), ",")})
	app, _ = c.app(app.ID)
	app.Secret = ""
	app.Previews.Env = nil
	view.App = app
	agent.JSON(w, 200, view)
}

// registerWebhook registers the restored application's repository webhook
// with a new secret.
func (c *Control) registerWebhook(ctx context.Context, id string) string {
	token, err := archiveGitHubToken(c, ctx)
	if err != nil || token == "" || c.cfg.GitHub == nil || !strings.HasPrefix(c.cfg.PublicURL, "https://") {
		return "auto-deploy is off: connect GitHub and enable it in the application's settings"
	}
	c.mu.Lock()
	a := c.data.Apps[id]
	a.Secret = random()
	c.data.Apps[id] = a
	saveErr := c.save()
	c.mu.Unlock()
	if saveErr != nil {
		return "auto-deploy is off: the webhook secret was not saved"
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	hook, err := c.cfg.GitHub.Hook(bounded, token, a.Repository, strings.TrimSuffix(c.cfg.PublicURL, "/")+"/hooks/github/"+a.ID, a.Secret, 0)
	if err != nil {
		return fmt.Sprintf("auto-deploy is off: the webhook could not be registered (%v)", err)
	}
	c.mu.Lock()
	a = c.data.Apps[id]
	a.HookID, a.AutoDeploy = hook, true
	c.data.Apps[id] = a
	saveErr = c.save()
	c.mu.Unlock()
	if saveErr != nil {
		return "the webhook was registered but not saved; enable auto-deploy again"
	}
	return "the repository webhook was registered again"
}

// purgeArchive deletes a deleted application permanently
// (DELETE /api/control/archives/{id}).
func (c *Control) purgeArchive(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Confirm string `json:"confirm"`
	}
	if err := agent.Decode(w, r, &input); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	id := r.PathValue("id")
	record, server, ok := c.archiveServer(r, id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if record.AppID != "" && input.Confirm != record.AppID {
		agent.Fail(w, 400, errors.New("type the application id to delete it permanently"))
		return
	}
	var purge agent.ArchivePurge
	slow := r.WithContext(withAgentTimeout(r.Context(), archiveAgentTimeout))
	if err := c.agentCall(slow, server, "DELETE", "/v1/archives/"+id, agent.ArchivePurgeRequest{Confirm: input.Confirm, By: currentUserID(r.Context())}, &purge); err != nil {
		failAgent(w, err)
		return
	}
	c.mu.Lock()
	delete(c.data.AppArchives, id)
	err := c.save()
	c.mu.Unlock()
	if err != nil {
		agent.Fail(w, 500, err)
		return
	}
	recordArchiveAudit(r.Context(), "app.purge", purge.AppID, map[string]string{"archive": id, "server": server, "trigger": "manual"})
	agent.JSON(w, 200, purge)
}
