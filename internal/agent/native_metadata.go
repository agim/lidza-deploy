package agent

// Read deployment metadata as data. No Git hooks/credential helpers, shell
// sourcing, Rails boot, application HTTP requests or Caddy writes are involved.
import (
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

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

func (m *Manager) nativeDiscoveredApplication(name string, units []string) NativeApplication {
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
	for _, path := range []string{filepath.Join(name, "repo/HEAD"), filepath.Join(name, "repo/.git/HEAD"), filepath.Join(name, "current/.git/HEAD")} {
		if branch := nativeGitBranch(m.nativeMetadataFile(path)); branch != "" {
			a.Branch = branch
			break
		}
	}
	a.Port = m.nativeConfiguredPort(name, units)
	// The domain is not guessed from web server configuration; the operator
	// confirms it when registering the application.
	// Empty/ambiguous metadata remains a discovery, never a guessed binding.
	return a
}

// nativeGitBranch returns the branch a git HEAD file points at, or "".
func nativeGitBranch(head []byte) string {
	branch, ok := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: refs/heads/")
	if !ok || branch == "" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " \t\n\\:~^?*[") || len(branch) > 200 {
		return ""
	}
	return branch
}
