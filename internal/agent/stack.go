package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

type StackBudget struct {
	MemoryMiB int    `json:"memory_mib"`
	CPUs      string `json:"cpus"`
}
type StackProfile struct {
	ID                          string `json:"id"`
	HostMiB                     int    `json:"host_mib"`
	ReserveMiB                  int    `json:"reserve_mib"`
	App, Mail, Office, Database StackBudget
}

var stackProfiles = []StackProfile{
	{"minimum", 4096, 512, StackBudget{512, "0.5"}, StackBudget{1792, "0.75"}, StackBudget{1024, "0.5"}, StackBudget{256, "0.25"}},
	{"standard", 8192, 1024, StackBudget{1024, "1"}, StackBudget{2560, "1"}, StackBudget{3072, "1.5"}, StackBudget{512, "0.5"}},
	{"performance", 16384, 2048, StackBudget{2048, "2"}, StackBudget{4096, "2"}, StackBudget{7168, "3"}, StackBudget{1024, "1"}},
}

func profileByID(id string) (StackProfile, bool) {
	for _, p := range stackProfiles {
		if p.ID == id {
			return p, true
		}
	}
	return StackProfile{}, false
}
func (p StackProfile) budget(role string) StackBudget {
	switch role {
	case "app":
		return p.App
	case "mail":
		return p.Mail
	case "office":
		return p.Office
	}
	return p.Database
}

type StackBuild struct {
	Dockerfile string `json:"dockerfile"`
	Target     string `json:"target,omitempty"`
}
type StackVolume struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}
type StackPort struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}
type StackRoute struct {
	Prefix      string `json:"prefix"`
	Port        int    `json:"port"`
	StripPrefix bool   `json:"strip_prefix"`
	Published   string `json:"published,omitempty"`
}
type StackService struct {
	Name            string            `json:"name"`
	Role            string            `json:"role"`
	Policy          string            `json:"policy"`
	Build           *StackBuild       `json:"build,omitempty"`
	Image           string            `json:"image,omitempty"`
	Command         []string          `json:"command,omitempty"`
	Aliases         []string          `json:"aliases,omitempty"`
	Environment     map[string]string `json:"environment,omitempty"`
	ShareAppNetwork bool              `json:"share_app_network,omitempty"`
	Volumes         []StackVolume     `json:"volumes,omitempty"`
	PublicPorts     []StackPort       `json:"public_ports,omitempty"`
	HTTPRoutes      []StackRoute      `json:"http_routes,omitempty"`
	HealthCommand   []string          `json:"health_command"`
}
type StackManifest struct {
	Version             int            `json:"version"`
	RequiredEnvironment []string       `json:"required_environment,omitempty"`
	GeneratedSecrets    []string       `json:"generated_secrets,omitempty"`
	Services            []StackService `json:"services"`
}
type StackContainer struct {
	Service   StackService `json:"service"`
	Container string       `json:"container"`
	Image     string       `json:"image"`
}
type StackRelease struct {
	Profile    string           `json:"profile"`
	Network    string           `json:"network"`
	Manifest   StackManifest    `json:"manifest"`
	Containers []StackContainer `json:"containers"`
	Routes     []StackRoute     `json:"routes,omitempty"`
}

var digestImagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:\-]*@sha256:[a-f0-9]{64}$`)
var stackTokenPattern = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)\}`)

func loadStackManifest(source string) (*StackManifest, error) {
	path := filepath.Join(source, "deploy", "lidza.stack.json")
	if info, err := os.Lstat(filepath.Join(source, "deploy")); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("deploy must be a regular repository directory")
	}
	if err := regularFile(path, false); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 {
		return nil, errors.New("stack manifest exceeds 64 KiB")
	}
	var manifest StackManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&manifest); err != nil {
		return nil, errors.New("invalid stack manifest or unknown field")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("stack manifest must contain one JSON object")
	}
	if err = manifest.validate(); err != nil {
		return nil, err
	}
	for _, s := range manifest.Services {
		if s.Build != nil {
			path := source
			for _, part := range strings.Split(s.Build.Dockerfile, "/") {
				path = filepath.Join(path, part)
				info, e := os.Lstat(path)
				if e != nil || info.Mode()&os.ModeSymlink != 0 {
					return nil, errors.New("stack Dockerfile path must not traverse symlinks")
				}
			}
			if err = regularFile(path, false); err != nil {
				return nil, err
			}
		}
	}
	return &manifest, nil
}
func (m StackManifest) validate() error {
	if m.Version != 1 || len(m.Services) < 1 || len(m.Services) > 3 {
		return errors.New("stack requires version 1 and one to three services")
	}
	names, roles, aliases, volumes := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, list := range [][]string{m.RequiredEnvironment, m.GeneratedSecrets} {
		if len(list) > 20 {
			return errors.New("too many stack environment requirements")
		}
		seen := map[string]bool{}
		for _, k := range list {
			if !envPattern.MatchString(k) || seen[k] {
				return errors.New("invalid or duplicate stack environment key")
			}
			seen[k] = true
		}
	}
	for _, k := range m.GeneratedSecrets {
		if k == "LIDZA_MASTER_KEY" || k == "DATABASE_URL" || environmentKind(k, "") != EnvironmentCredential {
			return errors.New("generated secrets must be credential keys; master keys and databases remain explicit")
		}
	}
	for _, s := range m.Services {
		if !idPattern.MatchString(s.Name) || len(s.Name) > 24 || names[s.Name] || roles[s.Role] {
			return errors.New("invalid or duplicate stack service name/role")
		}
		names[s.Name] = true
		roles[s.Role] = true
		if s.Role != "app" && s.Role != "mail" && s.Role != "office" {
			return errors.New("unsupported stack service role")
		}
		if s.Policy != "hardened" && !(s.Role == "app" && s.Policy == "init") && !(s.Role == "mail" && s.Policy == "mail") && !(s.Role == "office" && s.Policy == "office") {
			return errors.New("runtime policy does not match service role")
		}
		if (s.Build == nil) == (s.Image == "") {
			return errors.New("choose exactly one service build or pinned image")
		}
		if s.Image != "" && !digestImagePattern.MatchString(s.Image) {
			return errors.New("vendor images require a sha256 digest")
		}
		if s.Build != nil {
			b := s.Build
			if b.Dockerfile == "" || filepath.IsAbs(b.Dockerfile) || filepath.ToSlash(filepath.Clean(b.Dockerfile)) != b.Dockerfile || strings.HasPrefix(b.Dockerfile, "../") || strings.ContainsAny(b.Dockerfile, "\\\x00\r\n") || b.Dockerfile == ".." || b.Target != "" && !idPattern.MatchString(b.Target) {
				return errors.New("invalid repository Dockerfile or build target")
			}
		}
		if len(s.HealthCommand) == 0 || len(s.HealthCommand) > 32 || len(s.Command) > 64 {
			return errors.New("each service needs a bounded health command")
		}
		for _, cmd := range [][]string{s.Command, s.HealthCommand} {
			for _, arg := range cmd {
				if len(arg) > 2048 || strings.ContainsAny(arg, "\x00\r\n") {
					return errors.New("invalid stack command argument")
				}
			}
		}
		if s.ShareAppNetwork && s.Role != "mail" {
			return errors.New("only mail may share the app network namespace")
		}
		if len(s.Aliases) > 4 || len(s.Volumes) > 8 || len(s.Environment) > 80 {
			return errors.New("stack service resource count exceeds limit")
		}
		for _, alias := range append([]string{s.Name}, s.Aliases...) {
			if !idPattern.MatchString(alias) || aliases[alias] {
				return errors.New("invalid or duplicate network alias")
			}
			aliases[alias] = true
		}
		for _, v := range s.Volumes {
			if !idPattern.MatchString(v.Name) || len(v.Name) > 24 || volumes[v.Name] || !strings.HasPrefix(v.Target, "/var/") || filepath.Clean(v.Target) != v.Target || strings.ContainsAny(v.Target, ",\x00\r\n") {
				return errors.New("invalid, shared, or unsafe stack volume")
			}
			volumes[v.Name] = true
		}
		if s.Role == "app" && len(s.Volumes) > 0 {
			return errors.New("app storage uses the platform STORAGE_DIR attachment")
		}
		for k, v := range s.Environment {
			if !envPattern.MatchString(k) || strings.ContainsAny(v, "\x00\r\n") || len(v) > 8192 || strings.HasPrefix(k, "LIDZA_") || k == "DATABASE_URL" || k == "STORAGE_DIR" {
				return errors.New("invalid or platform-managed stack environment setting")
			}
			if environmentKind(k, v) == EnvironmentCredential && !regexp.MustCompile(`^\$\{[A-Z][A-Z0-9_]*\}$`).MatchString(v) {
				return errors.New("stack credentials must reference encrypted application environment")
			}
		}
		ports := map[string]bool{}
		for _, p := range s.PublicPorts {
			key := fmt.Sprint(p.Port, "/", p.Protocol)
			if s.Role != "app" || ports[key] || !(key == "25/tcp" || key == "7881/tcp" || key == "7882/udp") {
				return errors.New("unsupported or duplicate public stack port")
			}
			ports[key] = true
		}
		if len(s.HTTPRoutes) > 4 {
			return errors.New("too many stack HTTP routes")
		}
		for _, r := range s.HTTPRoutes {
			if s.Role != "app" || r.Prefix != "/rtc" || r.Port != 7880 || r.Published != "" {
				return errors.New("only app /rtc to private SFU port 7880 is supported")
			}
		}
	}
	if !roles["app"] {
		return errors.New("stack requires exactly one app service")
	}
	return nil
}
func stackEnvironment(a App, s StackService) (map[string]string, error) {
	env := map[string]string{}
	if s.Role == "app" {
		maps.Copy(env, a.Env)
	}
	for k, v := range s.Environment {
		var missing bool
		v = stackTokenPattern.ReplaceAllStringFunc(v, func(match string) string {
			key := stackTokenPattern.FindStringSubmatch(match)[1]
			if key == "APP_DOMAIN" {
				return a.Domain
			}
			value, ok := a.Env[key]
			missing = missing || !ok || value == ""
			return value
		})
		if missing || strings.Contains(v, "${") {
			return nil, fmt.Errorf("stack environment %s references a missing setting", k)
		}
		if _, explicit := env[k]; !explicit || s.Role != "app" {
			env[k] = v
		}
	}
	return env, nil
}
func (m *Manager) prepareStack(ctx context.Context, a App, manifest *StackManifest) (App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	saved, ok := m.data.Apps[a.ID]
	if !ok || saved.Retiring {
		return a, errors.New("application unavailable")
	}
	next := saved
	next.Env = maps.Clone(saved.Env)
	if next.Env == nil {
		next.Env = map[string]string{}
	}
	for _, k := range manifest.GeneratedSecrets {
		if next.Env[k] == "" {
			next.Env[k] = newID() + newID()
			if k == "LIVEKIT_API_KEY" {
				next.Env[k] = newID()
			}
		}
	}
	for _, k := range manifest.RequiredEnvironment {
		if next.Env[k] == "" {
			return a, fmt.Errorf("stack requires %s in encrypted application settings", k)
		}
	}
	if err := next.Validate(); err != nil {
		return a, err
	}
	oldVolumes := m.data.StackVolumes[a.ID]
	plannedVolumes := slices.Clone(oldVolumes)
	for _, service := range manifest.Services {
		for _, v := range service.Volumes {
			volume := "lidza-stack-" + a.resourceID() + "-" + v.Name
			if !slices.Contains(plannedVolumes, volume) {
				plannedVolumes = append(plannedVolumes, volume)
			}
		}
	}
	m.data.StackVolumes[a.ID] = plannedVolumes
	m.data.Apps[a.ID] = next
	if err := m.save(); err != nil {
		m.data.Apps[a.ID] = saved
		m.data.StackVolumes[a.ID] = oldVolumes
		return a, err
	}
	a.Env = maps.Clone(next.Env)
	a.StackManifest = manifest
	for _, s := range manifest.Services {
		if _, err := stackEnvironment(a, s); err != nil {
			return a, err
		}
	}
	// Reserve fixed ports against all committed live stacks. Docker also rejects
	// host-process bindings; no unrelated container is stopped to claim them.
	for _, other := range m.data.Apps {
		if other.ID == a.ID || other.Current == nil || other.Current.Stack == nil {
			continue
		}
		for _, want := range manifest.Services {
			for _, port := range want.PublicPorts {
				for _, old := range other.Current.Stack.Manifest.Services {
					if slices.Contains(old.PublicPorts, port) {
						return a, fmt.Errorf("stack port %d/%s belongs to application %s", port.Port, port.Protocol, other.ID)
					}
				}
			}
		}
	}
	return a, nil
}
