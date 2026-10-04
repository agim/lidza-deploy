package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/jobs"
	"io"
	"net/http"
	"strings"
)

func (c *Control) hook(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.data.Apps[r.PathValue("id")]
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	token, tokenErr := c.token(r.Context())
	if tokenErr != nil {
		agent.Fail(w, 409, tokenErr)
		return
	}
	if token == "" || c.cfg.GitHub == nil {
		agent.Fail(w, 409, errors.New("connect GitHub first"))
		return
	}
	if !strings.HasPrefix(c.cfg.PublicURL, "https://") {
		agent.Fail(w, 409, errors.New("webhooks require a public HTTPS control-panel URL"))
		return
	}
	// Persist the secret before making the hook reachable. A retry uses it again.
	if a.Secret == "" {
		a.Secret = random()
		c.data.Apps[a.ID] = a
		if err := c.save(); err != nil {
			agent.Fail(w, 500, err)
			return
		}
	}
	id, err := c.cfg.GitHub.Hook(r.Context(), token, a.Repository, strings.TrimSuffix(c.cfg.PublicURL, "/")+"/hooks/github/"+a.ID, a.Secret, a.HookID)
	if err != nil {
		agent.Fail(w, 502, err)
		return
	}
	a.HookID = id
	a.AutoDeploy = true
	c.data.Apps[a.ID] = a
	if err = c.save(); err != nil {
		agent.Fail(w, 500, err)
		return
	}
	a.Secret = ""
	a.Previews.Env = nil
	agent.JSON(w, 200, a)
}
func (c *Control) webhook(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring || a.Secret == "" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "payload too large", 413)
		return
	}
	signature := r.Header.Get("X-Hub-Signature-256")
	raw, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	mac := hmac.New(sha256.New, []byte(a.Secret))
	_, _ = mac.Write(body)
	if err != nil || !strings.HasPrefix(signature, "sha256=") || !hmac.Equal(raw, mac.Sum(nil)) {
		http.Error(w, "invalid signature", 401)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		w.WriteHeader(204)
		return
	}
	if event == "pull_request" {
		c.enqueuePreview(w, r, a, body)
		return
	}
	if event != "push" || !a.AutoDeploy {
		w.WriteHeader(204)
		return
	}
	var push struct {
		Ref        string `json:"ref"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err = json.Unmarshal(body, &push); err != nil {
		http.Error(w, "invalid payload", 400)
		return
	}
	if push.Deleted || push.Ref != "refs/heads/"+a.Branch || !strings.EqualFold(push.Repository.FullName, a.Repository) {
		w.WriteHeader(204)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery == "" || len(delivery) > 128 {
		http.Error(w, "invalid delivery ID", 400)
		return
	}
	id, err := jobs.From(r.Context()).Enqueue(r.Context(), "deploy.push", pushJob{AppID: a.ID, Delivery: delivery, Generation: a.Generation}, jobs.Unique(a.ID+":"+delivery), jobs.MaxAttempts(12))
	if err != nil {
		agent.Fail(w, 503, errors.New("could not persist delivery"))
		return
	}
	agent.JSON(w, 202, map[string]string{"id": id, "status": "pending"})
}

type pushJob struct {
	Generation string `json:"generation,omitempty"`
	AppID      string `json:"app_id"`
	Delivery   string `json:"delivery"`
}

func (c *Control) dispatchPush(ctx context.Context, payload json.RawMessage) error {
	var job pushJob
	if err := json.Unmarshal(payload, &job); err != nil {
		return err
	}
	a, ok := c.app(job.AppID)
	if !ok || a.Retiring || !a.AutoDeploy || job.Generation != a.Generation {
		return nil
	}
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.PublicURL, nil)
	if err != nil {
		return err
	}
	var d agent.Deployment
	if err := c.syncStorageIfConfigured(req, a.ServerID); err != nil {
		return err
	}
	return c.agentCall(req, a.ServerID, "POST", "/v1/apps/"+a.ID+"/deploy", agent.DeployRequest{Token: token, Key: "github:" + job.Delivery}, &d)
}
func (c *Control) deliveries(w http.ResponseWriter, r *http.Request) {
	rows, err := jobs.From(r.Context()).Recent(r.Context(), 100)
	if err != nil {
		agent.Fail(w, 500, err)
		return
	}
	agent.JSON(w, 200, rows)
}
