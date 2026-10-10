package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/audit"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/packs/jobs"
	"github.com/jackc/pgx/v5"
)

// One team shares this control panel's fleet. These are framework memberships,
// not a second application role store.
const fleetScope = "deploy-fleet"

var fleetRoles = auth.Roles{
	"admin":    {"*"},
	"deployer": {"fleet.read", "deploy.*"},
	"viewer":   {"fleet.read"},
}

func heldRoles(ctx context.Context) []string {
	r, _ := fleetRoles.Of(ctx, auth.CurrentUser(ctx).ID, fleetScope)
	return r
}

func permissionError(w http.ResponseWriter, err error) {
	if errors.Is(err, auth.ErrForbidden) {
		agent.Fail(w, 403, errors.New("this action requires a different team role"))
	} else {
		agent.Fail(w, 503, errors.New("team permissions could not be checked"))
	}
}

// Explicit route policy: unknown/new mutation routes default to administrator.
// Downloads contain database contents and must not be treated as ordinary reads.
func routePermission(pattern string) string {
	switch pattern {
	case "GET /api/control/github/app/status", "GET /api/control/github/app/manifest-callback", "GET /api/control/github/app/install-callback":
		return "github.manage"
	case "GET /api/control/team", "POST /api/control/team", "DELETE /api/control/team/{subject}":
		return "team.manage"
	case "GET /api/control/servers/{server}/ssh-access":
		return "infrastructure.manage"
	case "GET /api/control/audit":
		return "audit.read"
	case "GET /api/control/servers/{server}/databases/{id}/backups/{backup}":
		return "infrastructure.manage"
	case "GET /api/control/github/repos", "GET /api/control/github/branches":
		return "deploy.repositories"
	case "POST /api/control/apps/{id}/owner-claim":
		return "deploy.secrets"
	case "PATCH /api/control/apps/{id}/settings",
		"PUT /api/control/apps/{id}/{feature}", "PUT /api/control/apps/{id}/tasks/{task}",
		"POST /api/control/apps/{id}/tasks/{task}/{action}",
		"POST /api/control/apps/{id}/deploy", "POST /api/control/apps/{id}/rollback",
		"POST /api/control/apps/{id}/reload", "POST /api/control/apps/{id}/webhook",
		"DELETE /api/control/apps/{id}/webhook":
		return "deploy.write"
	}
	if strings.HasPrefix(pattern, "GET ") {
		return "fleet.read"
	}
	return "infrastructure.manage"
}

// Capture only the small JSON response of a mutation, never its request body.
// A requested entry must persist before a side effect; a separate result entry
// records acceptance/failure, not eventual deployment completion.
type auditResponse struct {
	header http.Header
	status int
	bytes.Buffer
}

func (w *auditResponse) Header() http.Header { return w.header }
func (w *auditResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *auditResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.Buffer.Write(b)
}

func (c *Control) protectedRoute(pattern string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mutates := r.Method != "GET" && r.Method != "HEAD"
		if pattern == "POST /api/control/apps/{id}/owner-claim" {
			w.Header().Set("Cache-Control", "no-store")
		}
		// Fixed registered route only: no URL query, raw path or request data.
		action := "control." + strings.ToLower(strings.Fields(pattern)[0])
		event := audit.Event{Action: action, Resource: strings.Fields(pattern)[1], Scope: fleetScope, Meta: map[string]string{"route": pattern}}
		permission := routePermission(pattern)
		if pattern == "PUT /api/control/apps/{id}/{feature}" && r.PathValue("feature") != "maintenance" {
			permission = "infrastructure.manage"
		}
		if err := fleetRoles.Check(r.Context(), fleetScope, permission); err != nil {
			if mutates {
				event.Outcome = audit.Denied
				if e := audit.From(r.Context()).Record(r.Context(), event); e != nil {
					agent.Fail(w, 503, errors.New("audit unavailable; action refused"))
					return
				}
			}
			permissionError(w, err)
			return
		}
		if !mutates {
			next(w, r)
			return
		}
		// Identifier metadata is from matched path values, never secrets or bodies.
		for _, k := range []string{"id", "server", "task", "subject"} {
			if v := r.PathValue(k); len(v) > 0 && len(v) <= 64 && safeAuditID(v) {
				event.Meta[k] = v
			}
		}
		requestEvent := event
		requestEvent.Action += ".requested"
		if err := audit.From(r.Context()).Record(r.Context(), requestEvent); err != nil {
			agent.Fail(w, 503, errors.New("audit unavailable; action refused"))
			return
		}
		response := &auditResponse{header: make(http.Header)}
		next(response, r)
		if response.status == 0 {
			response.status = 200
		}
		event.Outcome = audit.OK
		if response.status >= 400 {
			event.Outcome = audit.Failed
		}
		event.Meta["http_status"] = strconv.Itoa(response.status)
		// Preserve services and authenticated actor after a client disconnect.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		if err := audit.From(ctx).Record(ctx, event); err != nil {
			agent.Fail(w, 503, errors.New("audit result could not be saved; action may have applied; inspect state before retrying"))
			return
		}
		for k, v := range response.header {
			w.Header()[k] = v
		}
		w.WriteHeader(response.status)
		_, _ = w.Write(response.Bytes())
	}
}

func safeAuditID(s string) bool {
	for _, ch := range s {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

type teamMember struct {
	auth.Membership
	Email string `json:"email"`
	Name  string `json:"name"`
	Owner bool   `json:"owner"`
}

func (c *Control) team(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == "GET" {
		members, cursor, err := fleetRoles.Members(ctx, fleetScope, r.URL.Query().Get("cursor"), 50)
		if err != nil {
			agent.Fail(w, 503, errors.New("could not load team members"))
			return
		}
		out := make([]teamMember, 0, len(members))
		for _, m := range members {
			p, err := auth.From(ctx).Profile(ctx, m.Subject)
			if err != nil {
				agent.Fail(w, 503, errors.New("could not load member profile"))
				return
			}
			out = append(out, teamMember{m, p.Email, p.Name, m.Subject == c.operatorID})
		}
		agent.JSON(w, 200, map[string]any{"members": out, "next": cursor})
		return
	}
	c.teamMu.Lock()
	defer c.teamMu.Unlock()
	if r.Method == "DELETE" {
		id := r.PathValue("subject")
		if id == c.operatorID {
			agent.Fail(w, 409, errors.New("the installation owner must remain an administrator"))
			return
		}
		if err := audit.From(ctx).Record(ctx, audit.Event{Action: "team.member.remove", Resource: "member/" + id, Scope: fleetScope}); err != nil {
			agent.Fail(w, 503, errors.New("audit unavailable; membership was not removed"))
			return
		}
		if err := fleetRoles.RevokeAll(ctx, id, fleetScope); err != nil {
			agent.Fail(w, 503, errors.New("could not revoke membership"))
			return
		}
		agent.JSON(w, 200, map[string]string{"status": "removed"})
		return
	}
	var in struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	in.Email = auth.NormalizeEmail(in.Email)
	address, err := mail.ParseAddress(in.Email)
	if err != nil || address.Address != in.Email || len(in.Email) > 254 || len(in.Name) > 100 {
		agent.Fail(w, 400, errors.New("enter a valid email and a name of at most 100 characters"))
		return
	}
	if _, ok := fleetRoles[in.Role]; !ok {
		agent.Fail(w, 400, errors.New("choose admin, deployer or viewer"))
		return
	}
	p, err := auth.From(ctx).ProfileByEmail(ctx, in.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		if len(in.Password) < 16 || len(in.Password) > 1024 {
			agent.Fail(w, 400, errors.New("new members need a password of 16 to 1024 characters"))
			return
		}
		p, err = auth.From(ctx).CreateUser(ctx, in.Email, in.Name, in.Password)
	}
	if err != nil {
		agent.Fail(w, 503, errors.New("could not create or find member; use the framework password requirements"))
		return
	}
	if p.Subject == c.operatorID && in.Role != "admin" {
		agent.Fail(w, 409, errors.New("the installation owner must remain an administrator"))
		return
	}
	// Existing account passwords are never reset by role changes.
	if err := audit.From(ctx).Record(ctx, audit.Event{Action: "team.member.set", Resource: "member/" + p.Subject, Scope: fleetScope, Meta: map[string]string{"role": in.Role}}); err != nil {
		agent.Fail(w, 503, errors.New("audit unavailable; role was not changed"))
		return
	}
	if err := fleetRoles.Grant(ctx, p.Subject, fleetScope, in.Role); err != nil {
		agent.Fail(w, 503, errors.New("could not grant role"))
		return
	}
	for role := range fleetRoles {
		if role != in.Role {
			if err := fleetRoles.Revoke(ctx, p.Subject, fleetScope, role); err != nil {
				agent.Fail(w, 503, errors.New("role change incomplete; inspect membership before retrying"))
				return
			}
		}
	}
	agent.JSON(w, 200, map[string]string{"status": "saved", "subject": p.Subject})
}

func (c *Control) auditLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := audit.From(r.Context()).List(r.Context(), audit.Query{Scope: fleetScope, Cursor: q.Get("cursor"), Limit: 50, Actor: q.Get("actor"), Outcome: q.Get("outcome"), Action: q.Get("action")})
	if err != nil {
		agent.Fail(w, 503, errors.New("could not load audit events; check the pagination cursor"))
		return
	}
	agent.JSON(w, 200, page)
}

func (c *Control) auditedJob(name string, next jobs.Handler) jobs.Handler {
	return func(ctx context.Context, payload json.RawMessage) error {
		ctx = audit.System(ctx, name)
		var target struct {
			AppID string `json:"app_id"`
		}
		if err := json.Unmarshal(payload, &target); err != nil {
			return err
		}
		e := audit.Event{Action: name, Resource: "fleet", Scope: fleetScope}
		if len(target.AppID) <= 64 && safeAuditID(target.AppID) {
			e.Resource = "app/" + target.AppID
		}
		requested := e
		requested.Action += ".requested"
		if err := audit.From(ctx).Record(ctx, requested); err != nil {
			return err
		}
		err := next(ctx, payload)
		if err != nil {
			e.Outcome = audit.Failed
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return errors.Join(err, audit.From(writeCtx).Record(writeCtx, e))
	}
}
