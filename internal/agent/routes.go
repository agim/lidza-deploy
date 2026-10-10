package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type routeRuntime interface {
	PublishedPorts(context.Context, []string) (map[string]string, error)
}

// Inspect only published bindings; never include container environment values.
func (d *Docker) PublishedPorts(ctx context.Context, names []string) (map[string]string, error) {
	ports := map[string]string{}
	if len(names) == 0 {
		return ports, nil
	}
	args := append([]string{"inspect", "--format", `{{.Name}} {{json (index .NetworkSettings.Ports "3000/tcp")}}`}, names...)
	output, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil && len(strings.TrimSpace(string(output))) == 0 {
		// Docker may successfully inspect none because all tracked containers
		// were removed. Distinguish that from daemon/access failures.
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			missing := map[string]bool{}
			for _, line := range strings.Split(strings.TrimSpace(string(exit.Stderr)), "\n") {
				name, ok := strings.CutPrefix(line, "Error: No such object: ")
				if !ok {
					missing = nil
					break
				}
				missing[name] = true
			}
			allMissing := true
			for _, name := range names {
				if !missing[name] {
					allMissing = false
				}
				ports[name] = ""
			}
			if allMissing {
				return ports, nil
			}
		}
		return nil, errors.New("could not inspect published application ports")
	}
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
		ports[name] = ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name, raw, ok := strings.Cut(line, " ")
		if !ok {
			return nil, errors.New("invalid published port response")
		}
		name = strings.TrimPrefix(name, "/")
		if !allowed[name] {
			return nil, errors.New("unexpected container in published port response")
		}
		var bindings []struct {
			HostIP   string
			HostPort string
		}
		if err := json.Unmarshal([]byte(raw), &bindings); err != nil {
			return nil, errors.New("invalid published port bindings")
		}
		if len(bindings) != 1 || bindings[0].HostIP != "127.0.0.1" {
			continue
		}
		port, err := strconv.Atoi(bindings[0].HostPort)
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		ports[name] = strconv.Itoa(port)
	}
	return ports, nil
}

func (m *Manager) reconcileRoutes() {
	rt, ok := m.runtime.(routeRuntime)
	if !ok {
		return
	}
	m.mu.Lock()
	names := []string{}
	for _, a := range m.data.Apps {
		for _, r := range []*Release{a.Current, a.Previous} {
			if r != nil {
				names = append(names, r.Container)
			}
		}
	}
	m.mu.Unlock()
	if len(names) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	ports, err := rt.PublishedPorts(ctx, names)
	if err != nil {
		if m.ctx.Err() == nil {
			log.Printf("application route reconciliation: %v", err)
		}
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Copy release pointers: readers may still be proxying with the old snapshot.
	changed := false
	for id, a := range m.data.Apps {
		for _, slot := range []**Release{&a.Current, &a.Previous} {
			r := *slot
			if r == nil {
				continue
			}
			if port, ok := ports[r.Container]; ok && port != r.Port {
				next := *r
				next.Port = port
				*slot = &next
				changed = true
			}
		}
		m.data.Apps[id] = a
	}
	if changed {
		if err := m.save(); err != nil {
			log.Printf("could not persist refreshed application routes: %v", err)
		}
	}
}

func (m *Manager) routeLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.reconcileRoutes()
		}
	}
}
