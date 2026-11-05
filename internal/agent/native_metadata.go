package agent

// Read deployment metadata as data. No Git hooks/credential helpers, shell
// sourcing, Rails boot, application HTTP requests or Caddy writes are involved.
import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type nativeObservedRoute struct {
	Hosts []string
	Root  string
	Port  string
}

func (m *Manager) nativeObservedRoutes(ctx context.Context) []nativeObservedRoute {
	if ctx.Err() != nil {
		return nil
	}
	// Caddy's resume snapshot is a file under the configured native root. Never
	// infer an admin HTTP listener: even a supposedly administrative port could
	// belong to a socket-activated application on another host.
	root, err := os.OpenRoot(m.cfg.Native.Root)
	if err != nil {
		return nil
	}
	defer root.Close()
	file, err := root.Open(".config/caddy/autosave.json")
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return nil
	}
	var config any
	if json.NewDecoder(io.LimitReader(file, 8<<20)).Decode(&config) != nil {
		return nil
	}
	return nativeRouteMetadata(config)
}

func nativeRouteMetadata(config any) []nativeObservedRoute {
	result := []nativeObservedRoute{}
	var walk func(any, []string)
	walk = func(value any, hosts []string) {
		switch node := value.(type) {
		case []any:
			for _, child := range node {
				walk(child, hosts)
			}
		case map[string]any:
			own := []string{}
			if matches, ok := node["match"].([]any); ok {
				for _, value := range matches {
					if match, ok := value.(map[string]any); ok {
						if names, ok := match["host"].([]any); ok {
							for _, value := range names {
								if name, ok := value.(string); ok && validateHostname(name) == nil {
									own = append(own, name)
								}
							}
						}
					}
				}
			}
			if len(own) > 0 {
				hosts = own
			}
			if node["handler"] == "vars" || node["handler"] == "file_server" {
				if root, ok := node["root"].(string); ok && filepath.IsAbs(root) {
					result = append(result, nativeObservedRoute{Hosts: slices.Clone(hosts), Root: root})
				}
			}
			if node["handler"] == "reverse_proxy" {
				if upstreams, ok := node["upstreams"].([]any); ok {
					for _, value := range upstreams {
						if upstream, ok := value.(map[string]any); ok {
							if dial, ok := upstream["dial"].(string); ok {
								if port, ok := strings.CutPrefix(dial, "127.0.0.1:"); ok && nativePort(map[string]string{"PORT": port}) == port {
									result = append(result, nativeObservedRoute{Hosts: slices.Clone(hosts), Port: port})
								}
							}
						}
					}
				}
			}
			for _, child := range node {
				switch child.(type) {
				case map[string]any, []any:
					walk(child, hosts)
				}
			}
		}
	}
	walk(config, nil)
	return result
}

func (m *Manager) nativeMetadataFile(relative string) []byte {
	// Existing hosts use absolute current symlinks. Resolve only within the configured
	// root, then let OpenRoot enforce confinement again while opening the file.
	real, err := filepath.EvalSymlinks(filepath.Join(m.cfg.Native.Root, relative))
	if err != nil {
		return nil
	}
	relative, err = filepath.Rel(m.cfg.Native.Root, real)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil
	}
	root, err := os.OpenRoot(m.cfg.Native.Root)
	if err != nil {
		return nil
	}
	defer root.Close()
	file, err := root.Open(relative)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(file, 128<<10))
	if err != nil {
		return nil
	}
	return data
}

func nativeGitHubRepository(config []byte) string {
	origin := false
	for _, line := range strings.Split(string(config), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			origin = line == `[remote "origin"]`
			continue
		}
		if !origin {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "url" {
			continue
		}
		value = strings.TrimSpace(value)
		var repository string
		if suffix, ok := strings.CutPrefix(value, "git@github.com:"); ok {
			repository = suffix
		} else if u, err := url.Parse(value); err == nil && u.Host == "github.com" && (u.Scheme == "https" || u.Scheme == "ssh") {
			repository = strings.TrimPrefix(u.Path, "/")
		}
		repository = strings.TrimSuffix(repository, ".git")
		if repoPattern.MatchString(repository) && !strings.Contains(repository, "..") {
			return repository
		}
	}
	return ""
}

func (m *Manager) nativeDiscoveredApplication(name string, units []string, routes []nativeObservedRoute) NativeApplication {
	// Retain approved identities and operator-selected canonical domains.
	m.mu.Lock()
	for _, a := range m.data.NativeApps {
		if a.Name == name {
			m.mu.Unlock()
			a.Services = slices.Clone(units)
			return a
		}
	}
	m.mu.Unlock()
	a := NativeApplication{Name: name, Services: slices.Clone(units), HealthPath: "/up"}
	prefix := "rails-"
	if len(units) == 0 {
		a.Runtime = "astro"
		a.DocumentRoot = "public"
		a.HealthPath = "/"
		prefix = "astro-"
	}
	a.ID = prefix + strings.ReplaceAll(strings.ToLower(name), "_", "-")
	if !idPattern.MatchString(a.ID) {
		a.ID = ""
	}
	for _, path := range []string{filepath.Join(name, "current/.git/config"), filepath.Join(name, "repo/config"), filepath.Join(name, "repo/.git/config")} {
		if repository := nativeGitHubRepository(m.nativeMetadataFile(path)); repository != "" {
			a.Repository = repository
			break
		}
	}
	a.Port = m.nativeConfiguredPort(name, units)
	hosts := []string{}
	for _, route := range routes {
		if a.Runtime == "astro" && route.Root == filepath.Join(m.cfg.Native.Root, name, "current", "public") || a.Runtime != "astro" && a.Port != "" && route.Port == a.Port {
			for _, host := range route.Hosts {
				if validateHostname(host) == nil {
					hosts = append(hosts, host)
				}
			}
		}
	}
	slices.Sort(hosts)
	hosts = slices.Compact(hosts)
	canonical := []string{}
	for _, host := range hosts {
		if !strings.HasPrefix(host, "www.") {
			canonical = append(canonical, host)
		}
	}
	if len(canonical) == 1 {
		a.Domain = canonical[0]
		for _, host := range hosts {
			if host != a.Domain {
				a.AdditionalDomains = append(a.AdditionalDomains, host)
			}
		}
	} else if len(hosts) == 1 {
		a.Domain = hosts[0]
	}
	// Empty/ambiguous metadata remains a discovery, never a guessed binding.
	return a
}
