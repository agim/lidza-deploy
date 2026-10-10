package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

type stateRuntime struct {
	fakeRuntime
	calls    [][]string
	failStop bool
}

func (f *stateRuntime) SetRunning(_ context.Context, names []string, running bool) error {
	action := "stop"
	if running {
		action = "start"
	}
	f.calls = append(f.calls, append([]string{action}, names...))
	if !running && f.failStop {
		return errors.New("stop failed")
	}
	return nil
}
func TestApplicationStopStartPersistsAndBlocksWork(t *testing.T) {
	rt := &stateRuntime{}
	m := testManager(t, rt)
	a := testApp("portal")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	a.Current = &Release{ID: "current", Container: "web", Port: "12345"}
	a.Previous = &Release{ID: "previous", Container: "previous"}
	m.data.Apps[a.ID] = a
	m.data.Tasks["portal:worker"] = Task{ID: "worker", AppID: "portal", Enabled: true, Running: true, Mode: "worker", Container: "worker"}
	m.mu.Unlock()
	stopped := true
	if err := m.setApplicationState(a.ID, ApplicationState{&stopped}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.calls[0], []string{"stop", "previous", "web", "worker"}) {
		t.Fatal(rt.calls)
	}
	if m.Target(a.Domain) != nil {
		t.Fatal("stopped app still publicly routed")
	}
	w := httptest.NewRecorder()
	Proxy(m).ServeHTTP(w, httptest.NewRequest("GET", "http://"+a.Domain+"/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
	if _, err := m.Enqueue(a.ID, DeployRequest{}); err == nil {
		t.Fatal("deployed stopped app")
	}
	if _, err := m.Reload(a.ID); err == nil {
		t.Fatal("reloaded stopped app")
	}
	if err := m.runTask(a.ID, "worker", ""); err == nil {
		t.Fatal("ran stopped worker")
	}
	if m.applicationHealth(context.Background())[0].State != "stopped" {
		t.Fatal("reported intentional stop as failure")
	}
	if !m.taskList(a.ID)[0].Paused || !m.taskList(a.ID)[0].Enabled {
		t.Fatal("lost worker policy")
	}
	value := "changed"
	if err := m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, EnvChanges: map[string]*string{"LOG_LEVEL": &value}}); err != nil {
		t.Fatal(err)
	}
	if m.Target(a.Domain) != nil {
		t.Fatal("settings restarted app")
	}
	m.Close()
	restarted, err := NewManager(context.Background(), m.cfg, rt)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.Target(a.Domain) != nil {
		t.Fatal("restart activated app")
	}
	settings, _ := restarted.Settings(a.ID)
	if !settings.Stopped {
		t.Fatal("stop not persisted")
	}
	stopped = false
	if err := restarted.setApplicationState(a.ID, ApplicationState{&stopped}); err != nil {
		t.Fatal(err)
	}
	for _, d := range restarted.Deployments() {
		if d.Kind == "start" {
			waitDeployment(t, restarted, d.ID)
		}
	}
	if restarted.Target(a.Domain) == nil {
		t.Fatal("start did not activate route")
	}
	if len(rt.calls) != 2 {
		t.Fatal("unexpected direct container startup", rt.calls)
	}
	if restarted.taskList(a.ID)[0].Paused {
		t.Fatal("workers remain paused")
	}
}

func TestStopAddedAppAndRefuseBusyOrUnsupported(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("new-app")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	stopped := true
	if err := m.setApplicationState(a.ID, ApplicationState{&stopped}); err != nil {
		t.Fatal(err)
	}
	if err := m.setApplicationState(a.ID, ApplicationState{}); err == nil {
		t.Fatal("missing state accepted")
	}
	if err := m.Upsert(testApp(a.ID)); err != nil {
		t.Fatal(err)
	}
	settings, _ := m.Settings(a.ID)
	if !settings.Stopped {
		t.Fatal("full update cleared stopped state")
	}
	stopped = false
	if err := m.setApplicationState(a.ID, ApplicationState{&stopped}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps[a.ID]
	a.Current = &Release{ID: "one"}
	m.data.Apps[a.ID] = a
	m.mu.Unlock()
	stopped = true
	if err := m.setApplicationState(a.ID, ApplicationState{&stopped}); err == nil {
		t.Fatal("unsupported runtime claimed stop")
	}
	rt := &stateRuntime{failStop: true}
	m.runtime = rt
	if err := m.setApplicationState(a.ID, ApplicationState{&stopped}); err == nil {
		t.Fatal("hid runtime stop failure")
	}
	settings, _ = m.Settings(a.ID)
	if !settings.Stopped {
		t.Fatal("failed stop lost desired state")
	}
}

func TestFailedStartKeepsStoppedAndDoesNotBlockOtherApps(t *testing.T) {
	gate := make(chan struct{})
	rt := &stateRuntime{fakeRuntime: fakeRuntime{gate: gate, fail: true}}
	m := testManager(t, rt)
	for _, id := range []string{"one", "two"} {
		a := testApp(id)
		if err := m.Upsert(a); err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		a = m.data.Apps[id]
		a.Current = &Release{ID: id, Container: id, Port: "12345"}
		m.data.Apps[id] = a
		m.mu.Unlock()
	}
	stopped := true
	if err := m.setApplicationState("one", ApplicationState{&stopped}); err != nil {
		t.Fatal(err)
	}
	stopped = false
	if err := m.setApplicationState("one", ApplicationState{&stopped}); err != nil {
		t.Fatal(err)
	}
	var id string
	for _, d := range m.Deployments() {
		if d.Kind == "start" {
			id = d.ID
		}
	}
	done := make(chan bool, 1)
	go func() { done <- m.Target("two.example.com") != nil && len(m.Apps()) == 2 }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("other app unavailable")
		}
	case <-time.After(time.Second):
		t.Fatal("start locked the whole fleet")
	}
	stopped = true
	if err := m.setApplicationState("one", ApplicationState{&stopped}); err == nil {
		t.Fatal("allowed stop during active startup")
	}
	close(gate)
	if waitDeployment(t, m, id).Status != "failed" || m.Target("one.example.com") != nil {
		t.Fatal("failed startup exposed stopped app")
	}
}
