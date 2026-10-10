package control

import (
	"context"
	"encoding/json"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/jobs"
	"net/http"
	"time"
)

type fleetTask struct {
	agent.Task
	ServerID string `json:"server_id"`
}
type taskJob struct {
	Revision   string `json:"revision"`
	ServerID   string `json:"server_id"`
	AppID      string `json:"app_id"`
	TaskID     string `json:"task_id"`
	Key        string `json:"key"`
	Generation string `json:"generation"`
}

func (c *Control) tasks(w http.ResponseWriter, r *http.Request) {
	out := []fleetTask{}
	for _, result := range c.readFleet(r, "/v1/tasks") {
		var tasks []agent.Task
		if result.Err != nil || json.Unmarshal(result.Body, &tasks) != nil {
			continue
		}
		for _, t := range tasks {
			out = append(out, fleetTask{t, result.Server.ID})
		}
	}
	agent.JSON(w, 200, out)
}
func (c *Control) taskAction(w http.ResponseWriter, r *http.Request) {
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("task")
	var in any
	if r.Method == "PUT" {
		var t agent.Task
		if err := agent.Decode(w, r, &t); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		t.ID = id
		if err := t.Validate(); err != nil {
			agent.Fail(w, 400, err)
			return
		}
		in = t
	} else if r.Method == "POST" {
		in = map[string]string{"key": "manual:" + random()}
	}
	path := "/v1/apps/" + a.ID + "/tasks/" + id
	if r.PathValue("action") != "" {
		action := r.PathValue("action")
		if action != "run" && action != "logs" {
			http.NotFound(w, r)
			return
		}
		path += "/" + action
	}
	var out any
	if err := c.agentCall(r, a.ServerID, r.Method, path, in, &out); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 200, out)
}
func (c *Control) tasksTick(ctx context.Context, r *http.Request) error {
	for _, result := range c.readFleet(r, "/v1/tasks") {
		if result.Err != nil {
			continue
		}
		var tasks []agent.Task
		if json.Unmarshal(result.Body, &tasks) != nil {
			continue
		}
		for _, t := range tasks {
			a, ok := c.app(t.AppID)
			if !ok || a.Retiring || t.Paused || !t.Enabled || a.ServerID != result.Server.ID {
				continue
			}
			if t.Mode == "worker" {
				if err := c.agentCall(r, result.Server.ID, "POST", "/v1/apps/"+a.ID+"/tasks/"+t.ID+"/run", map[string]string{}, nil); err != nil {
					continue
				}
			} else if !t.Running && !t.NextRun.IsZero() && !t.NextRun.After(time.Now()) {
				key := "task:" + a.ID + ":" + t.ID + ":" + t.Revision + ":" + t.NextRun.UTC().Format("20060102T150405")
				if _, err := jobs.From(ctx).Enqueue(ctx, "deploy.task", taskJob{Revision: t.Revision, ServerID: result.Server.ID, AppID: a.ID, TaskID: t.ID, Key: key, Generation: a.Generation}, jobs.Unique(key), jobs.MaxAttempts(3)); err != nil {
					return err
				}
			}
			if err := c.observe(ctx, "task:"+result.Server.ID+":"+t.AppID+":"+t.ID, "Background task failed: "+t.AppID+"/"+t.ID, t.Error != "", 1); err != nil {
				return err
			}
		}
	}
	return nil
}
func (c *Control) dispatchTask(ctx context.Context, payload json.RawMessage) error {
	var in taskJob
	if err := json.Unmarshal(payload, &in); err != nil {
		return err
	}
	a, ok := c.app(in.AppID)
	if !ok || a.Retiring || a.Generation != in.Generation || a.ServerID != in.ServerID {
		return nil
	}
	r, _ := http.NewRequestWithContext(ctx, "POST", c.cfg.PublicURL, nil)
	var settings agent.Settings
	if err := c.agentCall(r, a.ServerID, "GET", "/v1/apps/"+a.ID+"/settings", nil, &settings); err != nil {
		return err
	}
	if settings.Stopped {
		return nil
	}
	return c.agentCall(r, in.ServerID, "POST", "/v1/apps/"+a.ID+"/tasks/"+in.TaskID+"/run", map[string]string{"key": in.Key, "revision": in.Revision}, nil)
}
