package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// BuildConfig is shared across runtimes. SourceDirectory is resolved within the
// disposable checkout, never against an operator-controlled host directory.
type BuildConfig struct {
	SourceDirectory string `json:"source_directory,omitempty"`
}

type AstroConfig struct {
	Mode       string `json:"mode,omitempty"` // static or node (standalone adapter)
	OutputDir  string `json:"output_directory,omitempty"`
	NodeMajor  string `json:"node_major,omitempty"`
	HealthPath string `json:"health_path,omitempty"`
}

// Distribution-specific adapters are registered during initialization only.
var additionalApplicationRuntimes = map[string]func(App) error{}

func ApplicationRuntimes() []string {
	runtimes := []string{"go", "astro"}
	for name := range additionalApplicationRuntimes {
		runtimes = append(runtimes, name)
	}
	slices.Sort(runtimes)
	return runtimes
}

func cleanRelativeDirectory(value string) bool {
	return value == "" || value == "." || regexp.MustCompile(`^[A-Za-z0-9_./-]+$`).MatchString(value) && !filepath.IsAbs(value) && filepath.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../")
}

func validateApplicationRuntime(a App) error {
	if !cleanRelativeDirectory(a.Build.SourceDirectory) {
		return errors.New("source directory must stay inside the repository")
	}
	if a.Runtime != "astro" {
		if a.Astro != nil {
			return errors.New("Astro settings require the Astro runtime")
		}
		// Private runtime adapters perform their own validation. The public
		// distribution accepts only its known runtimes.
		if a.Runtime != "" && a.Runtime != "go" {
			if validate, ok := additionalApplicationRuntimes[a.Runtime]; ok {
				return validate(a)
			}
			return errors.New("choose a supported application runtime")
		}
		return nil
	}
	if a.RuntimeProfile != "" || a.CustomerHosts || a.Preview {
		return errors.New("Astro cannot use Go stack profiles, framework customer hosts or previews")
	}
	if a.Astro == nil {
		return nil
	}
	cfg := a.Astro
	if cfg.Mode != "" && cfg.Mode != "static" && cfg.Mode != "node" {
		return errors.New("Astro output must be static or standalone Node")
	}
	if !cleanRelativeDirectory(cfg.OutputDir) || cfg.OutputDir == "." {
		return errors.New("Astro output must be a directory within the build checkout")
	}
	if cfg.NodeMajor != "" && cfg.NodeMajor != "22" && cfg.NodeMajor != "24" {
		return errors.New("choose Node 22 or 24 for Astro")
	}
	if cfg.HealthPath != "" {
		u, err := url.ParseRequestURI(cfg.HealthPath)
		if err != nil || len(cfg.HealthPath) > 200 || !strings.HasPrefix(cfg.HealthPath, "/") || strings.HasPrefix(cfg.HealthPath, "//") || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(cfg.HealthPath, "\\\r\n\x00") || strings.Contains(cfg.HealthPath, "..") {
			return errors.New("health path must be a local absolute URL path")
		}
	}
	return nil
}

func applicationSourceDirectory(checkout string, a App) (string, error) {
	if !cleanRelativeDirectory(a.Build.SourceDirectory) {
		return "", errors.New("invalid source directory")
	}
	dir := filepath.Join(checkout, a.Build.SourceDirectory)
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", errors.New("source directory is unavailable")
	}
	base, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("source directory escapes the repository")
	}
	return real, nil
}

func prepareApplicationBuild(source string, a App) error {
	if a.Runtime == "astro" {
		if err := prepareAstroBuild(source, a); err != nil {
			return err
		}
	} else if err := prepareBuild(source); err != nil {
		return err
	}
	return secureBuildContext(source)
}

func secureBuildContext(source string) error {
	for _, name := range []string{".dockerignore", "Dockerfile.dockerignore"} {
		path := filepath.Join(source, name)
		if err := regularFile(path, true); err != nil {
			return err
		}
		if name == "Dockerfile.dockerignore" {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				continue
			}
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(file, "\n# Deployment credentials never enter the build context\n.git\n**/.git\n.env\n.env.*\n**/.env\n**/.env.*\n.npmrc\n**/.npmrc\n**/master.key\n**/*.key\n**/*.pem\n")
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func astroOptions(a App) AstroConfig {
	cfg := AstroConfig{Mode: "static", OutputDir: "dist", NodeMajor: "22", HealthPath: "/.__lidza_health"}
	if a.Astro != nil {
		if a.Astro.Mode != "" {
			cfg.Mode = a.Astro.Mode
		}
		if a.Astro.OutputDir != "" {
			cfg.OutputDir = a.Astro.OutputDir
		}
		if a.Astro.NodeMajor != "" {
			cfg.NodeMajor = a.Astro.NodeMajor
		}
		if a.Astro.HealthPath != "" {
			cfg.HealthPath = a.Astro.HealthPath
		} else if cfg.Mode == "node" {
			cfg.HealthPath = "/"
		}
	}
	return cfg
}

func prepareAstroBuild(source string, a App) error {
	if err := validateApplicationRuntime(a); err != nil {
		return err
	}
	for _, name := range []string{"package.json", "package-lock.json"} {
		if err := regularFile(filepath.Join(source, name), false); err != nil {
			return err
		}
	}
	data, err := os.ReadFile(filepath.Join(source, "package.json"))
	if err != nil || len(data) > 1<<20 {
		return errors.New("package.json is unavailable or too large")
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
		Scripts         map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil || pkg.Scripts["build"] == "" || pkg.Dependencies["astro"] == "" && pkg.DevDependencies["astro"] == "" {
		return errors.New("Astro deployment requires an Astro dependency and a build script")
	}
	cfg := astroOptions(a)
	if cfg.Mode == "node" && pkg.Dependencies["@astrojs/node"] == "" {
		return errors.New("standalone Node output requires @astrojs/node as a production dependency")
	}
	path := filepath.Join(source, "Dockerfile")
	if err := regularFile(path, true); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_./-]+$`).MatchString(cfg.OutputDir) {
		return errors.New("Astro output directory contains unsupported characters")
	}
	// No environment file or credential is copied into the image. PUBLIC_ values
	// are supplied separately through a BuildKit secret, never Docker ARG/history.
	build := "# syntax=docker/dockerfile:1\nFROM node:" + cfg.NodeMajor + "-alpine AS build\nWORKDIR /app\nCOPY package.json package-lock.json ./\nRUN npm ci --no-audit --no-fund\nCOPY . .\nRUN --mount=type=secret,id=lidza_public_env if [ -f /run/secrets/lidza_public_env ]; then . /run/secrets/lidza_public_env; fi; npm run build\n"
	if cfg.Mode == "node" {
		build += "RUN test -f " + cfg.OutputDir + "/server/entry.mjs && npm prune --omit=dev --no-audit --no-fund\nFROM node:" + cfg.NodeMajor + "-alpine\nWORKDIR /app\nCOPY --from=build --chown=65532:65532 /app/package.json ./\nCOPY --from=build --chown=65532:65532 /app/node_modules ./node_modules\nCOPY --from=build --chown=65532:65532 /app/" + cfg.OutputDir + " ./dist\nUSER 65532:65532\nENV NODE_ENV=production HOST=0.0.0.0 PORT=3000\nEXPOSE 3000\nCMD [\"node\",\"./dist/server/entry.mjs\"]\n"
	} else {
		build += "RUN test -f " + cfg.OutputDir + "/index.html\nFROM caddy:2-alpine AS caddy-bin\nRUN setcap -r /usr/bin/caddy\nFROM alpine:3.22\nCOPY --from=caddy-bin /usr/bin/caddy /usr/bin/caddy\nCOPY --from=caddy-bin /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\nCOPY --from=build --chown=65532:65532 /app/" + cfg.OutputDir + " /srv\nCOPY --chown=65532:65532 .lidza-static.Caddyfile /etc/caddy/Caddyfile\nUSER 65532:65532\nENV HOME=/tmp XDG_CONFIG_HOME=/tmp/config XDG_DATA_HOME=/tmp/data\nEXPOSE 3000\nCMD [\"caddy\",\"run\",\"--config\",\"/etc/caddy/Caddyfile\",\"--adapter\",\"caddyfile\"]\n"
		if err := regularFile(filepath.Join(source, ".lidza-static.Caddyfile"), true); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(source, ".lidza-static.Caddyfile"), []byte("{\n admin off\n auto_https off\n persist_config off\n}\n:3000 {\n @health path /.__lidza_health\n respond @health ok 200\n root * /srv\n file_server\n}\n"), 0600); err != nil {
			return err
		}
	}
	ignore := filepath.Join(source, ".dockerignore")
	if err := regularFile(ignore, true); err != nil {
		return err
	}
	f, err := os.OpenFile(ignore, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(f, "\n.git\n**/.git\nnode_modules\n.env\n.env.*\n**/.env\n**/.env.*\n!.env.example\n.npmrc\n*.pem\n*.key\n")
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.WriteFile(path, []byte(build), 0600)
}

func applicationHealthPath(a App) string {
	if a.Runtime == "astro" {
		return astroOptions(a).HealthPath
	}
	return "/readyz"
}
func releaseHealthPath(r *Release) string {
	if r != nil && r.HealthPath != "" {
		return r.HealthPath
	}
	return "/readyz"
}

func astroPublicBuildEnvironment(a App, path string) error {
	var content strings.Builder
	for key, value := range a.Env {
		if strings.HasPrefix(key, "PUBLIC_") && envPattern.MatchString(key) {
			fmt.Fprintf(&content, "export %s='%s'\n", key, strings.ReplaceAll(value, "'", "'\\''"))
		}
	}
	return os.WriteFile(path, []byte(content.String()), 0600)
}
