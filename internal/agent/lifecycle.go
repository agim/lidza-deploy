package agent

import (
	"context"
	"errors"
	"log"
	"os/exec"
	"slices"
	"strings"
	"time"
)

type ApplicationState struct {
	Stopped *bool `json:"stopped"`
}
type lifecycleRuntime interface {
	SetRunning(context.Context, []string, bool) error
}

func (d *Docker) SetRunning(ctx context.Context, names []string, running bool) error {
	action := "stop"
	if running {
		action = "start"
	}
	var first error
	for _, name := range names {
		if name == "" {
			continue
		}
		if !running {
			output, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Status}}", name).CombinedOutput()
			if err != nil {
				if strings.Contains(string(output), "Error: No such object: "+name) {
					continue
				}
				if first == nil {
					first = errors.New("could not inspect application container")
				}
				continue
			}
		}
		args := []string{action}
		if !running {
			args = append(args, "--time", "10")
		}
		args = append(args, name)
		if _, err := command(ctx, "", nil, "docker", args...); err != nil && first == nil {
			first = errors.New("could not " + action + " application container; check agent service logs")
		}
	}
	return first
}

func (m *Manager) stoppedContainers(a App) []string {
	names := []string{}
	for _, r := range []*Release{a.Current, a.Previous} {
		if r != nil {
			names = append(names, r.Container)
		}
	}
	for _, t := range m.data.Tasks {
		if t.AppID == a.ID && t.Mode == "worker" && t.Container != "" {
			names = append(names, t.Container)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(names)))
}

func (m *Manager) setApplicationState(id string, in ApplicationState) error {
	if in.Stopped == nil {
		return errors.New("stopped must be true or false")
	}
	m.mu.Lock()
	locked, owned := true, false
	unlock := func() { locked = false; m.mu.Unlock() }
	lock := func() { m.mu.Lock(); locked = true }
	defer func() {
		if !locked {
			m.mu.Lock()
		}
		if owned {
			delete(m.lifecycle, id)
		}
		m.mu.Unlock()
	}()
	a, ok := m.data.Apps[id]
	if !ok || a.Retiring {
		return errors.New("application unavailable")
	}
	if m.busy(id) || a.Restoring || m.data.Caches[id].Operation || m.upgradePending() {
		return errors.New("wait for the active application operation to finish")
	}
	for _, t := range m.data.Tasks {
		if t.AppID == id && t.Running && (t.Mode == "schedule" || t.Launching || t.Container == "") {
			return errors.New("wait for the active command or worker startup to finish")
		}
	}
	rt, supported := m.runtime.(lifecycleRuntime)
	if !supported && a.Current != nil {
		return errors.New("runtime does not support application stop/start")
	}
	m.lifecycle[id] = true
	owned = true
	ctx, cancel := context.WithTimeout(m.ctx, 90*time.Second)
	defer cancel()
	if *in.Stopped {
		old := a
		a.Stopped = true
		m.data.Apps[id] = a
		// Persist the desired state before side effects, so a crash cannot reactivate it.
		if err := m.save(); err != nil {
			m.data.Apps[id] = old
			return err
		}
		names := m.stoppedContainers(a)
		unlock()
		if supported {
			if d, ok := m.runtime.(*Docker); ok {
				extra, err := d.ownedContainers(ctx, a.ID)
				if err != nil {
					return err
				}
				names = append(names, extra...)
			}
			if err := rt.SetRunning(ctx, names, false); err != nil {
				return err
			}
		}
		lock()
		for key, t := range m.data.Tasks {
			if t.AppID == id {
				t.Running = false
				m.data.Tasks[key] = t
			}
		}
		return m.save()
	}
	if !a.Stopped {
		return nil
	}
	if a.Current != nil {
		// Recreate the current image through the normal checked reload pipeline, so
		// settings edited while stopped are applied and logs/backups stay available.
		delete(m.lifecycle, id)
		_, err := m.queueLocked(a, DeployRequest{resume: true}, true)
		return err
	}
	old := m.data.Apps[id]
	a.Stopped = false
	m.data.Apps[id] = a
	if err := m.save(); err != nil {
		m.data.Apps[id] = old
		return err
	}

	return nil
}

func (m *Manager) enforceStoppedApps() {
	rt, ok := m.runtime.(lifecycleRuntime)
	if !ok {
		return
	}
	for _, a := range m.data.Apps {
		if a.Stopped {
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			names := m.stoppedContainers(a)
			if d, ok := m.runtime.(*Docker); ok {
				extra, err := d.ownedContainers(ctx, a.ID)
				if err != nil {
					log.Printf("stopped app %s discovery: %v", a.ID, err)
				} else {
					names = append(names, extra...)
				}
			}
			err := rt.SetRunning(ctx, names, false)
			cancel()
			if err != nil {
				log.Printf("stopped app %s: %v", a.ID, err)
			}
		}
	}
}

func (d *Docker) ownedContainers(ctx context.Context, id string) ([]string, error) {
	names := []string{}
	for _, label := range []string{"io.lidza.task=", "io.lidza.app="} {
		output, err := command(ctx, "", nil, "docker", "ps", "-aq", "--filter", "label="+label+id)
		if err != nil {
			return nil, errors.New("could not discover application containers")
		}
		names = append(names, strings.Fields(output)...)
	}
	return slices.Compact(slices.Sorted(slices.Values(names))), nil
}
