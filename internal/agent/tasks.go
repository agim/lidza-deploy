package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/agim/lidza/packs/jobs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Task struct {
	Revision     string     `json:"revision,omitempty"`
	SeenKeys     []string   `json:"seen_keys,omitempty"`
	ID           string     `json:"id"`
	AppID        string     `json:"app_id"`
	Mode         string     `json:"mode"`
	Command      []string   `json:"command"`
	Enabled      bool       `json:"enabled"`
	EveryMinutes int        `json:"every_minutes,omitempty"`
	Daily        string     `json:"daily,omitempty"`
	Timezone     string     `json:"timezone,omitempty"`
	NextRun      time.Time  `json:"next_run,omitempty"`
	LastRun      *time.Time `json:"last_run,omitempty"`
	Container    string     `json:"container,omitempty"`
	Release      string     `json:"release,omitempty"`
	Running      bool       `json:"running"`
	Error        string     `json:"error,omitempty"`
	Log          string     `json:"log,omitempty"`
	RunKey       string     `json:"run_key,omitempty"`
}

func (t Task) schedule() (jobs.Schedule, error) {
	if t.Daily != "" {
		if _, err := time.Parse("15:04", t.Daily); err != nil {
			return nil, errors.New("daily time must be HH:MM")
		}
		if t.Timezone != "" {
			if _, err := time.LoadLocation(t.Timezone); err != nil {
				return nil, errors.New("invalid timezone")
			}
		}
		return jobs.Daily(t.Daily, t.Timezone), nil
	}
	if t.EveryMinutes < 1 || t.EveryMinutes > 10080 {
		return nil, errors.New("interval must be between 1 minute and one week")
	}
	return jobs.Every(time.Duration(t.EveryMinutes) * time.Minute), nil
}
func (t Task) Validate() error {
	if !idPattern.MatchString(t.ID) || len(t.Command) == 0 || len(t.Command) > 32 {
		return errors.New("task needs a valid ID and command argument list")
	}
	if t.Command[0] == "" {
		return errors.New("command executable required")
	}
	for _, arg := range t.Command {
		if len(arg) > 4096 || strings.ContainsRune(arg, '\x00') {
			return errors.New("invalid command argument")
		}
	}
	if t.Mode != "worker" && t.Mode != "schedule" {
		return errors.New("choose worker or schedule")
	}
	if t.Mode == "schedule" {
		_, err := t.schedule()
		return err
	}
	return nil
}
func taskKey(app, id string) string { return app + ":" + id }
func (m *Manager) taskList(app string) []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Task{}
	for _, t := range m.data.Tasks {
		if app == "" || t.AppID == app {
			t.Command = slices.Clone(t.Command)
			t.SeenKeys = nil
			out = append(out, t)
		}
	}
	return out
}
func (m *Manager) putTask(appID string, t Task) error {
	if err := t.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[appID]
	if !ok || a.Retiring || a.Restoring || m.busy(appID) {
		return errors.New("application unavailable or busy")
	}
	key := taskKey(appID, t.ID)
	old, exists := m.data.Tasks[key]
	if old.Running && old.Mode == "schedule" {
		return errors.New("wait for the running command")
	}
	if !exists && len(m.data.Tasks) >= 500 {
		return errors.New("task limit reached")
	}
	if old.Container != "" {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		if _, err := command(ctx, "", nil, "docker", "rm", "-f", old.Container); err != nil {
			return errors.New("could not stop previous task")
		}
	}
	t.AppID = appID
	t.Revision = newID()
	t.SeenKeys = slices.Clone(old.SeenKeys)
	t.Container = ""
	t.Release = ""
	t.Running = false
	t.Log = ""
	t.Error = ""
	t.RunKey = ""
	t.LastRun = nil
	if t.Mode == "schedule" {
		schedule, _ := t.schedule()
		t.NextRun = schedule.Next(time.Now().UTC())
	}
	m.data.Tasks[key] = t
	if err := m.save(); err != nil {
		if exists {
			m.data.Tasks[key] = old
		} else {
			delete(m.data.Tasks, key)
		}
		return err
	}
	return nil
}
func (m *Manager) runTask(appID, id, key string, revision ...string) error {
	if m.upgradePending() {
		return errors.New("agent upgrade in progress")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.Apps[appID]
	t, found := m.data.Tasks[taskKey(appID, id)]
	if !ok || !found || a.Retiring || a.Restoring || a.Current == nil || m.busy(appID) || !t.Enabled {
		return errors.New("task or running app unavailable")
	}
	if len(revision) > 0 && revision[0] != "" && t.Revision != revision[0] {
		return nil
	}
	if key != "" && (key == t.RunKey || slices.Contains(t.SeenKeys, key)) {
		return nil
	}
	if len(key) > 200 {
		return errors.New("invalid run key")
	}
	if t.Mode == "worker" && t.Running && t.Release == a.Current.ID && key == "" {
		if t.Container == "" {
			return nil
		}
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		status, err := command(ctx, "", nil, "docker", "inspect", "--format", "{{.State.Running}} {{.State.Restarting}} {{.RestartCount}}", t.Container)
		cancel()
		if err == nil {
			fields := strings.Fields(status)
			nextError := ""
			if len(fields) != 3 || fields[0] != "true" || fields[1] == "true" {
				nextError = "worker is stopped or restarting; inspect logs"
			} else if count, _ := strconv.Atoi(fields[2]); count >= 5 {
				nextError = "worker has restarted repeatedly; inspect logs and restart after fixing the cause"
			}
			if t.Error != nextError {
				t.Error = nextError
				m.data.Tasks[taskKey(appID, id)] = t
				return m.save()
			}
			return nil
		}
		t.Running = false
	}
	if t.Mode == "schedule" && t.Running {
		return errors.New("scheduled command is already running")
	}
	if key == "" {
		key = newID()
	}
	now := time.Now().UTC()
	old := t
	t.Running = true
	t.Error = ""
	t.Log = ""
	t.LastRun = &now
	t.RunKey = key
	t.SeenKeys = append(slices.Clone(t.SeenKeys), key)
	if len(t.SeenKeys) > 128 {
		t.SeenKeys = t.SeenKeys[len(t.SeenKeys)-128:]
	}
	t.Release = a.Current.ID
	if t.Mode == "schedule" {
		schedule, _ := t.schedule()
		t.NextRun = schedule.Next(now)
	}
	m.data.Tasks[taskKey(appID, id)] = t
	if err := m.save(); err != nil {
		m.data.Tasks[taskKey(appID, id)] = old
		return err
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Minute)
		defer cancel()
		name, log, err := m.startTaskContainer(ctx, a, t, old.Container)
		log = scrubOutput(log, outputSecrets(a.Env))
		m.mu.Lock()
		defer m.mu.Unlock()
		current := m.data.Tasks[taskKey(appID, id)]
		if current.RunKey != t.RunKey {
			if name != "" {
				cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, _ = command(cleanup, "", nil, "docker", "rm", "-f", name)
			}
			return
		}
		current.Container = name
		current.Log = log
		current.Running = t.Mode == "worker" && err == nil
		if err != nil {
			current.Error = "command failed; inspect task logs"
		}
		m.data.Tasks[taskKey(appID, id)] = current
		_ = m.save()
	}()
	return nil
}
func (m *Manager) startTaskContainer(ctx context.Context, a App, t Task, old string) (string, string, error) {
	if old != "" {
		_, _ = command(ctx, "", nil, "docker", "rm", "-f", old)
	}
	name := "lidza-task-" + a.ID + "-" + t.ID + "-" + newID()
	dir, err := os.MkdirTemp(m.cfg.DataDir, ".task-env-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	var env strings.Builder
	for k, v := range a.Env {
		fmt.Fprintf(&env, "%s=%s\n", k, v)
	}
	if _, ok := a.Env["APP_URL"]; !ok {
		fmt.Fprintf(&env, "APP_URL=https://%s\n", a.Domain)
	}
	envfile := filepath.Join(dir, "env")
	if err = os.WriteFile(envfile, []byte(env.String()), 0600); err != nil {
		return "", "", err
	}
	args := []string{"create", "--name", name, "--label", "io.lidza.task=" + a.ID, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65532:65532", "--memory", "512m", "--cpus", "1", "--pids-limit", "256", "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m", "--log-opt", "max-size=10m", "--log-opt", "max-file=3", "--env-file", envfile, "--entrypoint", t.Command[0]}
	if t.Mode == "worker" {
		args = append(args, "--restart", "unless-stopped")
	}
	networks := []string{}
	m.mu.Lock()
	a.StorageVolume = m.data.StorageVolumes[a.ID]
	if c := m.data.Caches[a.ID]; c.Ready && c.Mode == "local" {
		networks = append(networks, c.Network)
	}
	for _, id := range a.Bindings {
		if d := m.data.Databases[id]; d.Network != "" {
			networks = append(networks, d.Network)
		}
	}
	m.mu.Unlock()
	networks = slices.Compact(slices.Sorted(slices.Values(networks)))
	if len(networks) > 0 {
		args = append(args, "--network", networks[0])
	}
	// Workers and scheduled commands share the app's lasting storage volume.
	args = append(args, storageMountArgs(a)...)
	args = append(args, a.Current.Image)
	args = append(args, t.Command[1:]...)
	if _, err = command(ctx, "", nil, "docker", args...); err != nil {
		return "", "", err
	}
	success := false
	defer func() {
		if !success || t.Mode == "schedule" {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = command(cleanup, "", nil, "docker", "rm", "-f", name)
		}
	}()
	for i := 1; i < len(networks); i++ {
		network := networks[i]
		if _, err = command(ctx, "", nil, "docker", "network", "connect", network, name); err != nil {
			return "", "", err
		}
	}
	if t.Mode == "worker" {
		_, err = command(ctx, "", nil, "docker", "start", name)
		success = err == nil
		return name, "", err
	}
	// Docker start -a exits with the process status. Preserve bounded output even on failure.
	var output limitedBuffer
	err = dockerStream(ctx, nil, &output, nil, "start", "-a", name)
	success = err == nil
	return "", output.String(), err
}
func (m *Manager) taskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tasks", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, m.taskList("")) })
	mux.HandleFunc("PUT /v1/apps/{id}/tasks/{task}", func(w http.ResponseWriter, r *http.Request) {
		var t Task
		if err := Decode(w, r, &t); err != nil {
			Fail(w, 400, err)
			return
		}
		t.ID = r.PathValue("task")
		if err := m.putTask(r.PathValue("id"), t); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, t)
	})
	mux.HandleFunc("POST /v1/apps/{id}/tasks/{task}/run", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Key      string `json:"key"`
			Revision string `json:"revision"`
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.runTask(r.PathValue("id"), r.PathValue("task"), in.Key, in.Revision); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, map[string]string{"status": "running"})
	})
	mux.HandleFunc("GET /v1/apps/{id}/tasks/{task}/logs", func(w http.ResponseWriter, r *http.Request) {
		for _, t := range m.taskList(r.PathValue("id")) {
			if t.ID == r.PathValue("task") {
				if t.Container != "" {
					ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
					defer cancel()
					log, err := command(ctx, "", nil, "docker", "logs", "--tail", "200", t.Container)
					if err == nil {
						m.mu.Lock()
						env := m.data.Apps[t.AppID].Env
						m.mu.Unlock()
						log = scrubOutput(log, outputSecrets(env))
						t.Log = log
					}
				}
				JSON(w, 200, t)
				return
			}
		}
		http.NotFound(w, r)
	})
}
