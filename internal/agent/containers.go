package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ContainerView exposes operational metadata only. Container environment,
// mounts and labels can contain credentials and are deliberately omitted.
type ContainerView struct {
	Ports          []PortView `json:"ports,omitempty"`
	ID             string     `json:"id,omitempty"`
	Name           string     `json:"name"`
	AppID          string     `json:"app_id"`
	Role           string     `json:"role"`
	State          string     `json:"state"`
	Status         string     `json:"status,omitempty"`
	Health         string     `json:"health,omitempty"`
	CPU            string     `json:"cpu,omitempty"`
	Memory         string     `json:"memory,omitempty"`
	MemoryPercent  string     `json:"memory_percent,omitempty"`
	NetworkIO      string     `json:"network_io,omitempty"`
	BlockIO        string     `json:"block_io,omitempty"`
	PIDs           string     `json:"pids,omitempty"`
	Restartable    bool       `json:"restartable"`
	NeedsAttention bool       `json:"needs_attention"`
}

type ContainerInventory struct {
	CheckedAt  time.Time       `json:"checked_at"`
	Containers []ContainerView `json:"containers"`
	Warning    string          `json:"warning,omitempty"`
}

type managedContainer struct {
	HealthCommand []string
	AppID         string
	Name          string
	Role          string
	Restartable   bool
}

func (m *Manager) managedContainers() []managedContainer {
	m.mu.Lock()
	defer m.mu.Unlock()
	byName := map[string]managedContainer{}
	add := func(value managedContainer) {
		if value.Name != "" {
			byName[value.Name] = value
		}
	}
	for _, a := range m.data.Apps {
		if a.Current != nil {
			add(managedContainer{AppID: a.ID, Name: a.Current.Container, Role: "Web · current release", Restartable: true})
		}
		if a.Current != nil && a.Current.Stack != nil {
			for _, c := range a.Current.Stack.Containers {
				add(managedContainer{AppID: a.ID, Name: c.Container, Role: "Stack · " + c.Service.Name, HealthCommand: c.Service.HealthCommand, Restartable: true})
			}
		}
		if a.Previous != nil && a.Previous.Stack != nil {
			for _, c := range a.Previous.Stack.Containers {
				add(managedContainer{AppID: a.ID, Name: c.Container, Role: "Stack · " + c.Service.Name + " · rollback"})
			}
		}
		if a.Previous != nil && a.Previous.Stack == nil {
			add(managedContainer{AppID: a.ID, Name: a.Previous.Container, Role: "Web · rollback release"})
		}
	}
	for _, task := range m.data.Tasks {
		role := "Scheduled command"
		restartable := false
		if task.Mode == "worker" {
			role = "Worker · " + task.ID
			restartable = true
		}
		add(managedContainer{AppID: task.AppID, Name: task.Container, Role: role, Restartable: restartable})
	}
	for _, database := range m.data.Databases {
		if database.Mode == "local" {
			add(managedContainer{AppID: database.AppID, Name: database.Network, Role: "PostgreSQL", Restartable: true})
		}
	}
	for appID, cache := range m.data.Caches {
		if cache.Mode == "local" {
			add(managedContainer{AppID: appID, Name: cache.Network, Role: "Valkey cache", Restartable: true})
		}
	}
	for appID, clickhouse := range m.data.ClickHouses {
		add(managedContainer{AppID: appID, Name: clickhouse.Network, Role: "ClickHouse", Restartable: true})
	}
	out := make([]managedContainer, 0, len(byName))
	for _, value := range byName {
		out = append(out, value)
	}
	slices.SortFunc(out, func(a, b managedContainer) int {
		if n := strings.Compare(a.AppID, b.AppID); n != 0 {
			return n
		}
		if n := strings.Compare(a.Role, b.Role); n != 0 {
			return n
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

type dockerListRow struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	State  string `json:"State"`
	Status string `json:"Status"`
}

type dockerStatsRow struct {
	Name       string `json:"Name"`
	CPU        string `json:"CPUPerc"`
	Memory     string `json:"MemUsage"`
	MemoryPerc string `json:"MemPerc"`
	NetworkIO  string `json:"NetIO"`
	BlockIO    string `json:"BlockIO"`
	PIDs       string `json:"PIDs"`
}

func decodeDockerLines[T any](raw string) map[string]T {
	out := map[string]T{}
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var value T
		if json.Unmarshal([]byte(line), &value) != nil {
			continue
		}
		var key string
		switch row := any(value).(type) {
		case dockerListRow:
			key = row.Names
		case dockerStatsRow:
			key = row.Name
		}
		if key != "" {
			out[key] = value
		}
	}
	return out
}

func healthFromStatus(status string) string {
	start := strings.LastIndex(status, "(")
	end := strings.LastIndex(status, ")")
	if start < 0 || end <= start {
		return ""
	}
	value := strings.ToLower(strings.TrimSpace(status[start+1 : end]))
	if value == "healthy" || value == "unhealthy" || value == "health: starting" {
		return strings.TrimPrefix(value, "health: ")
	}
	return ""
}

func (m *Manager) containerInventory(ctx context.Context) ContainerInventory {
	managed := m.managedContainers()
	out := ContainerInventory{CheckedAt: time.Now().UTC(), Containers: make([]ContainerView, 0, len(managed))}
	if len(managed) == 0 {
		return out
	}
	probe, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	live := map[string]dockerListRow{}
	listFailed := false
	for start := 0; start < len(managed); start += 50 {
		end := min(start+50, len(managed))
		args := []string{"container", "ls", "--all", "--no-trunc", "--format", "{{json .}}"}
		for _, item := range managed[start:end] {
			args = append(args, "--filter", "name=^/"+item.Name+"$")
		}
		raw, err := command(probe, "", nil, "docker", args...)
		if err != nil {
			listFailed = true
			continue
		}
		for name, row := range decodeDockerLines[dockerListRow](raw) {
			live[name] = row
		}
	}
	running := []string{}
	for _, item := range managed {
		if row, ok := live[item.Name]; ok && row.State == "running" {
			running = append(running, item.Name)
		}
	}
	stats := map[string]dockerStatsRow{}
	for start := 0; start < len(running); start += 50 {
		end := min(start+50, len(running))
		args := []string{"stats", "--no-stream", "--format", "{{json .}}"}
		args = append(args, running[start:end]...)
		if raw, err := command(probe, "", nil, "docker", args...); err == nil {
			for name, row := range decodeDockerLines[dockerStatsRow](raw) {
				stats[name] = row
			}
		} else if out.Warning == "" {
			out.Warning = "Live resource sampling is temporarily unavailable. Container state is still current."
		}
	}
	serviceHealth := map[string]string{}
	var healthMu sync.Mutex
	var healthWG sync.WaitGroup
	for _, item := range managed {
		if len(item.HealthCommand) == 0 || live[item.Name].State != "running" {
			continue
		}
		healthWG.Add(1)
		go func(item managedContainer) {
			defer healthWG.Done()
			bounded, stop := context.WithTimeout(probe, 3*time.Second)
			_, err := command(bounded, "", nil, "docker", append([]string{"exec", item.Name}, item.HealthCommand...)...)
			state := "healthy"
			if err != nil {
				state = "unhealthy"
			}
			if bounded.Err() != nil {
				state = "unknown"
			}
			stop()
			healthMu.Lock()
			serviceHealth[item.Name] = state
			healthMu.Unlock()
		}(item)
	}
	healthWG.Wait()
	if listFailed {
		out.Warning = "Docker inventory is temporarily unavailable. Retry in a few seconds."
	}
	portRows := map[string][]PortView{}
	if d, ok := m.runtime.(*Docker); ok {
		m.mu.Lock()
		var registrations []portRegistration
		for _, a := range m.data.Apps {
			registrations = append(registrations, releasePortRegistrations(a.Current)...)
			if a.Current != nil && a.Current.Stack == nil {
				registrations = append(registrations, portRegistration{AppID: a.ID, Release: a.Current.ID, Container: a.Current.Container, Service: "app", Ports: []StackPort{}})
			}
		}
		m.mu.Unlock()
		if len(registrations) > 0 {
			ports, err := d.portsCall(ctx, "inspect", registrations)
			if err != nil {
				out.Warning += " Public-port inventory unavailable: " + err.Error()
			} else {
				for _, row := range ports.Ports {
					portRows[row.Container] = append(portRows[row.Container], row)
				}
				if ports.Warning != "" {
					out.Warning += " " + ports.Warning
				}
			}
		}
	}
	for _, item := range managed {
		view := ContainerView{Name: item.Name, AppID: item.AppID, Role: item.Role, State: "missing", Restartable: item.Restartable}
		if row, ok := live[item.Name]; ok {
			view.ID = row.ID
			view.State = row.State
			view.Status = row.Status
			view.Health = healthFromStatus(row.Status)
		}
		if row, ok := stats[item.Name]; ok {
			view.CPU = row.CPU
			view.Memory = row.Memory
			view.MemoryPercent = row.MemoryPerc
			view.NetworkIO = row.NetworkIO
			view.BlockIO = row.BlockIO
			view.PIDs = row.PIDs
		}
		if health, ok := serviceHealth[item.Name]; ok {
			view.Health = health
			if health == "unknown" && out.Warning == "" {
				out.Warning = "Stack service health sampling timed out; unavailable evidence is not a healthy result."
			}
		}
		view.Ports = portRows[item.Name]
		view.NeedsAttention = view.State == "missing" || view.State == "dead" || view.State == "exited" || view.State == "restarting" || view.Health == "unhealthy"
		out.Containers = append(out.Containers, view)
	}
	return out
}

func (m *Manager) containerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/containers", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, m.containerInventory(r.Context()))
	})
	mux.HandleFunc("POST /v1/containers/restart", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Name string `json:"name"`
		}
		if err := Decode(w, r, &input); err != nil {
			Fail(w, http.StatusBadRequest, err)
			return
		}
		var target *managedContainer
		for _, item := range m.managedContainers() {
			if item.Name == input.Name {
				copy := item
				target = &copy
				break
			}
		}
		if target == nil {
			Fail(w, http.StatusNotFound, errors.New("managed container not found"))
			return
		}
		if !target.Restartable {
			Fail(w, http.StatusConflict, errors.New("this retained container cannot be restarted; use its application control instead"))
			return
		}
		if strings.HasPrefix(target.Role, "Stack · ") {
			deployment, err := m.Reload(target.AppID)
			if err != nil {
				Fail(w, http.StatusConflict, err)
				return
			}
			JSON(w, http.StatusAccepted, map[string]string{"status": "queued", "deployment_id": deployment.ID, "name": target.Name, "app_id": target.AppID})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if _, err := command(ctx, "", nil, "docker", "restart", "--time", "10", target.Name); err != nil {
			Fail(w, http.StatusConflict, errors.New("container restart failed"))
			return
		}
		JSON(w, http.StatusOK, map[string]string{"status": "restarted", "name": target.Name, "app_id": target.AppID, "at": strconv.FormatInt(time.Now().UTC().Unix(), 10)})
	})
}
