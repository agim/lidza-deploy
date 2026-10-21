package agent

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed public-ports.py
var publicPortsHelper string

type PortView struct {
	AppID         string `json:"app_id"`
	Service       string `json:"service"`
	Container     string `json:"container"`
	Release       string `json:"release"`
	Declared      bool   `json:"declared"`
	Protocol      string `json:"protocol"`
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Publication   string `json:"publication"`
	Firewall      string `json:"firewall"`
	External      string `json:"external"`
	Bindings      []struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	} `json:"bindings,omitempty"`
}
type PortInventory struct {
	Ports   []PortView `json:"ports"`
	Warning string     `json:"warning,omitempty"`
	Error   string     `json:"error,omitempty"`
}
type portRegistration struct {
	AppID     string      `json:"app_id"`
	Owner     string      `json:"owner"`
	Release   string      `json:"release"`
	Container string      `json:"container"`
	Service   string      `json:"service"`
	Ports     []StackPort `json:"ports"`
}

func releasePortRegistrations(r *Release) []portRegistration {
	if r == nil || r.Stack == nil {
		return nil
	}
	var out []portRegistration
	owner := strings.TrimSuffix(strings.TrimPrefix(r.Stack.Network, "lidza-stack-"), "-"+r.ID)
	for _, c := range r.Stack.Containers {
		if c.Service.Role != "app" {
			continue
		}
		appID := strings.TrimPrefix(strings.TrimSuffix(c.Container, "-"+r.ID+"-"+c.Service.Name), "lidza-")
		out = append(out, portRegistration{appID, owner, r.ID, c.Container, c.Service.Name, append([]StackPort{}, c.Service.PublicPorts...)})
	}
	return out
}
func (d *Docker) portsCall(ctx context.Context, action string, registrations []portRegistration) (PortInventory, error) {
	var out PortInventory
	request, _ := json.Marshal(struct {
		Action        string             `json:"action"`
		Registrations []portRegistration `json:"registrations,omitempty"`
	}{action, registrations})
	bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(bounded, "python3", "-c", publicPortsHelper, filepath.Join(d.Root, ".public-ports"))
	cmd.Stdin = bytes.NewReader(request)
	data, err := cmd.Output()
	if json.Unmarshal(data, &out) != nil {
		return out, errors.New("public-port firewall helper unavailable; python3, root privileges and Docker firewall access are required")
	}
	if err != nil || out.Error != "" {
		if out.Error != "" {
			return out, errors.New(out.Error)
		}
		return out, errors.New("public-port firewall reconciliation failed")
	}
	return out, nil
}
func hasPublicPorts(r *Release) bool {
	for _, entry := range releasePortRegistrations(r) {
		if len(entry.Ports) > 0 {
			return true
		}
	}
	return false
}
func (d *Docker) registerPublicPorts(ctx context.Context, r *Release) error {
	var entries []portRegistration
	for _, entry := range releasePortRegistrations(r) {
		if len(entry.Ports) > 0 {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	_, err := d.portsCall(ctx, "register", entries)
	return err
}
func (d *Docker) syncPublicPorts(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(d.Root, ".public-ports", "registry.json")); os.IsNotExist(err) {
		return nil
	}
	_, err := d.portsCall(ctx, "sync", nil)
	return err
}
func (m *Manager) reconcilePublicPorts() {
	d, ok := m.runtime.(*Docker)
	if !ok {
		return
	}
	m.mu.Lock()
	var releases []*Release
	for _, a := range m.data.Apps {
		releases = append(releases, a.Current, a.Previous)
	}
	for _, t := range m.data.StackTransitions {
		releases = append(releases, t.Candidate, t.Previous)
	}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(m.ctx, 45*time.Second)
	defer cancel()
	for _, r := range releases {
		if err := d.registerPublicPorts(ctx, r); err != nil {
			return
		}
	}
	// Do not prevent the control plane starting on legacy/unsupported hosts.
	// Inventory reports the actual failure; subsequent activation fails explicitly.
	_ = d.syncPublicPorts(ctx)
}
func (m *Manager) appPorts(ctx context.Context, id string) (PortInventory, error) {
	m.mu.Lock()
	a, ok := m.data.Apps[id]
	m.mu.Unlock()
	if !ok {
		return PortInventory{}, errors.New("unknown application")
	}
	d, ok := m.runtime.(*Docker)
	if !ok {
		return PortInventory{Ports: []PortView{}, Warning: "Port inventory is unavailable for this runtime."}, nil
	}
	entries := releasePortRegistrations(a.Current)
	if a.Current != nil && a.Current.Stack == nil {
		entries = []portRegistration{{AppID: id, Release: a.Current.ID, Container: a.Current.Container, Service: "app", Ports: []StackPort{}}}
	}
	out, err := d.portsCall(ctx, "inspect", entries)
	// The helper maintains other apps too. Only this app's current release is
	// exposed here, avoiding rollback/stopped registrations in the app overview.
	filtered := []PortView{}
	for _, row := range out.Ports {
		if row.AppID == id && a.Current != nil && row.Release == a.Current.ID {
			filtered = append(filtered, row)
		}
	}
	out.Ports = filtered
	return out, err
}
func (m *Manager) publicPortRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/apps/{id}/ports", func(w http.ResponseWriter, r *http.Request) {
		out, err := m.appPorts(r.Context(), r.PathValue("id"))
		if err != nil {
			Fail(w, 502, err)
			return
		}
		JSON(w, 200, out)
	})
}
