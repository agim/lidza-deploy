package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/db"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const consoleTable = `CREATE TABLE IF NOT EXISTS deploy_error_agent (server_id text PRIMARY KEY,last_seen timestamptz NOT NULL,collection_error text NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS deploy_error_scope ON app_error ((extra->>'deploy_server'),(extra->>'deploy_app'),(extra->>'deploy_generation'),created_at DESC);`

func (c *Control) reportingConfig(server Server) agent.ErrorReporting {
	c.mu.Lock()
	defer c.mu.Unlock()
	apps := map[string]string{}
	for _, a := range c.data.Apps {
		if a.ServerID == server.ID && !a.Retiring {
			apps[a.ID] = a.Generation
		}
	}
	return agent.ErrorReporting{URL: strings.TrimSuffix(c.cfg.PublicURL, "/") + "/api/agent/errors", Server: server.ID, Apps: apps}
}
func (c *Control) configureCollectors(ctx context.Context, _ json.RawMessage) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	failures := make(chan struct{}, 1)
	for _, server := range c.servers() {
		sem <- struct{}{}
		wg.Add(1)
		go func(server Server) {
			defer wg.Done()
			defer func() { <-sem }()
			pairCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(pairCtx, "GET", c.cfg.PublicURL, nil)
			if err := c.agentCall(req, server.ID, "PUT", "/v1/error-reporting", c.reportingConfig(server), nil); err != nil {
				select {
				case failures <- struct{}{}:
				default:
				}
			}
		}(server)
	}
	wg.Wait()
	if _, err := db.From(ctx).Exec(ctx, `DELETE FROM app_error WHERE extra ? 'deploy_server' AND created_at < now()-interval '30 days'`); err != nil {
		return err
	}
	if len(failures) > 0 {
		return errors.New("could not configure one or more console collectors; check agent connectivity/version")
	}
	return nil
}
func (c *Control) authenticatedAgent(r *http.Request) string {
	server := r.Header.Get("X-Lidza-Server")
	for _, s := range c.servers() {
		if s.ID == server {
			want := sha256.Sum256([]byte(s.Token))
			got := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && subtle.ConstantTimeCompare(want[:], got[:]) == 1 {
				return server
			}
		}
	}
	return ""
}
func (c *Control) ingestErrors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	server := c.authenticatedAgent(r)
	if server == "" {
		agent.Fail(w, 401, errors.New("agent authentication required"))
		return
	}
	var batch agent.ConsoleBatch
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&batch); err != nil || len(batch.Errors) > 20 || len(batch.CollectionError) > 256 {
		agent.Fail(w, 400, errors.New("invalid console report batch"))
		return
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		agent.Fail(w, 400, errors.New("expected one report batch"))
		return
	}
	for _, e := range batch.Errors {
		if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(e.ID) || e.Source != "server" || len(e.Message) > 8192 || e.Message == "" || e.CreatedAt.IsZero() || e.CreatedAt.After(time.Now().Add(5*time.Minute)) || len(e.Container) > 200 || len(e.Release) > 128 || len(e.Commit) > 128 || len(e.Generation) > 128 || len(e.Fingerprint) > 128 || e.URL != nil || e.UserID != nil {
			agent.Fail(w, 400, errors.New("invalid console error record"))
			return
		}
		for _, p := range []*string{e.Stack, e.Route, e.Method, e.RequestID} {
			if p != nil && len(*p) > 16384 {
				agent.Fail(w, 400, errors.New("console error detail too large"))
				return
			}
		}
	}
	tx, err := db.From(r.Context()).Begin(r.Context())
	if err != nil {
		agent.Fail(w, 503, errors.New("error store unavailable"))
		return
	}
	defer tx.Rollback(r.Context())
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authenticatedAgent(r) != server {
		agent.Fail(w, 401, errors.New("agent access changed"))
		return
	}
	for _, e := range batch.Errors {
		a, ok := c.data.Apps[e.AppID]
		if !ok || a.Retiring || a.ServerID != server || a.Generation != e.Generation {
			continue
		}
		sum := sha256.Sum256([]byte(server + a.ID + a.Generation + e.ID))
		v := hex.EncodeToString(sum[:16])
		id := fmt.Sprintf("%s-%s-%s-%s-%s", v[:8], v[8:12], v[12:16], v[16:20], v[20:])
		extra, _ := json.Marshal(map[string]string{"deploy_server": server, "deploy_app": a.ID, "deploy_generation": a.Generation, "container": e.Container, "release": e.Release, "commit": e.Commit})
		if _, err = tx.Exec(r.Context(), `INSERT INTO app_error(id,source,message,stack,route,method,request_id,fingerprint,extra,created_at) VALUES($1,'server',$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(id) DO NOTHING`, id, e.Message, e.Stack, e.Route, e.Method, e.RequestID, e.Fingerprint, extra, e.CreatedAt); err != nil {
			agent.Fail(w, 503, errors.New("could not persist console errors"))
			return
		}
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO deploy_error_agent(server_id,last_seen,collection_error) VALUES($1,now(),$2) ON CONFLICT(server_id) DO UPDATE SET last_seen=now(),collection_error=excluded.collection_error`, server, batch.CollectionError); err != nil {
		agent.Fail(w, 503, errors.New("could not persist collector status"))
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		agent.Fail(w, 503, errors.New("could not commit console errors"))
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "stored"})
}
func (c *Control) appErrors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	pairCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	pairErr := c.agentCall(r.WithContext(pairCtx), a.ServerID, "PUT", "/v1/error-reporting", c.reportingConfig(Server{ID: a.ServerID}), nil)
	cancel()
	rows, err := db.From(r.Context()).Query(r.Context(), `SELECT id,source,message,stack,route,method,request_id,fingerprint,created_at,extra FROM app_error WHERE extra->>'deploy_server'=$1 AND extra->>'deploy_app'=$2 AND extra->>'deploy_generation'=$3 ORDER BY created_at DESC LIMIT 500`, a.ServerID, a.ID, a.Generation)
	if err != nil {
		agent.Fail(w, 503, errors.New("console error store unavailable"))
		return
	}
	defer rows.Close()
	records := []agent.ConsoleError{}
	size := 0
	truncated := false
	for rows.Next() {
		var e agent.ConsoleError
		var extra []byte
		if err = rows.Scan(&e.ID, &e.Source, &e.Message, &e.Stack, &e.Route, &e.Method, &e.RequestID, &e.Fingerprint, &e.CreatedAt, &extra); err != nil {
			agent.Fail(w, 503, errors.New("could not read console errors"))
			return
		}
		var meta map[string]string
		json.Unmarshal(extra, &meta)
		e.AppID = a.ID
		e.Generation = a.Generation
		e.Container = meta["container"]
		e.Release = meta["release"]
		e.Commit = meta["commit"]
		encoded, _ := json.Marshal(e)
		size += len(encoded)
		if size > 2<<20 {
			truncated = true
			break
		}
		records = append(records, e)
	}
	if rows.Err() != nil {
		agent.Fail(w, 503, errors.New("console error read failed"))
		return
	}
	rows.Close()
	var seen time.Time
	var problem string
	status := "waiting_agent"
	if err = db.From(r.Context()).QueryRow(r.Context(), `SELECT last_seen,collection_error FROM deploy_error_agent WHERE server_id=$1`, a.ServerID).Scan(&seen, &problem); err == nil {
		status = "ready"
		if time.Since(seen) > time.Minute {
			status = "agent_offline"
		}
		if problem != "" {
			status = "collection_warning"
		}
	}
	if pairErr != nil {
		status = "agent_offline"
		problem = "Hosting agent is unreachable or does not support console reporting. Stored errors remain available."
	}
	agent.JSON(w, 200, map[string]any{"status": status, "errors": records, "limit": 500, "truncated": truncated, "last_received": seen, "collection_error": problem, "mode": "console"})
}
func (c *Control) analyticsErrors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var out agent.AppErrors
	if err := c.agentCall(r, a.ServerID, "GET", "/v1/apps/"+a.ID+"/analytics-errors", nil, &out); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 200, out)
}
