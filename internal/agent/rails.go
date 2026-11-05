package agent

// Rails applications on the container runtime. Rails orchestration deliberately
// uses the existing release/rollback, encrypted environment and task machinery.
import (
	"context"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/agim/lidza/pkg/config"
)

type RailsConfig struct {
	HealthPath string `json:"health_path,omitempty"`
}

func railsHealthHost(a App) string {
	if a.isRails() {
		return a.Domain
	}
	return ""
}

func (r *Release) healthPath() string {
	if r != nil && r.HealthPath != "" {
		return r.HealthPath
	}
	return "/readyz"
}

func init() {
	additionalApplicationRuntimes["rails"] = func(a App) error { return a.validateRuntime() }
}

func (a App) isRails() bool { return a.Runtime == "rails" }
func (a App) runtimeKind() string {
	if a.Runtime == "" {
		return "go"
	}
	return a.Runtime
}
func (a App) runtimeUser() string {
	if a.isRails() {
		return "1000:1000"
	}
	return "65532:65532"
}
func (a App) healthPath() string {
	if a.Runtime == "astro" {
		return applicationHealthPath(a)
	}
	if a.isRails() {
		if a.Rails != nil && a.Rails.HealthPath != "" {
			return a.Rails.HealthPath
		}
		return "/up"
	}
	return "/readyz"
}

func (a App) validateRuntime() error {
	if a.Runtime == "astro" {
		if a.Rails != nil {
			return errors.New("Rails settings require the Rails runtime")
		}
		return validateApplicationRuntime(a)
	}
	if a.runtimeKind() != "go" && !a.isRails() {
		return errors.New("choose go or rails application runtime")
	}
	if !a.isRails() {
		if a.Rails != nil {
			return errors.New("Rails settings require the Rails runtime")
		}
		return nil
	}
	if a.RuntimeProfile != "" || a.CustomerHosts || a.Preview {
		return errors.New("Rails applications do not support Go stack profiles, framework customer-host approval or previews")
	}
	p := a.healthPath()
	u, err := url.ParseRequestURI(p)
	if err != nil || len(p) > 200 || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || u.Host != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(p, "\\\r\n\x00") || strings.Contains(p, "..") {
		return errors.New("Rails health check must be an absolute local path without a query")
	}
	return nil
}

// Never generate a Rails master key. Existing credentials must retain their
// original RAILS_MASTER_KEY (or SECRET_KEY_BASE) supplied as encrypted app env.
func prepareRailsBuild(source string) error {
	for _, name := range []string{"Gemfile", "Gemfile.lock", "Dockerfile", "bin/rails"} {
		if err := regularFile(filepath.Join(source, name), false); err != nil {
			return errors.New("Rails deployment requires regular Gemfile, Gemfile.lock, Dockerfile and bin/rails files")
		}
	}
	// Dockerfile-specific ignore files take precedence. Harden both in the
	// disposable checkout so even an application's permissive ! rules cannot put
	// deployment keys or environment files into an image layer.
	for _, name := range []string{".dockerignore", "Dockerfile.dockerignore"} {
		file := filepath.Join(source, name)
		if err := regularFile(file, true); err != nil {
			return err
		}
		data, err := os.ReadFile(file)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if name == "Dockerfile.dockerignore" && os.IsNotExist(err) {
			continue
		}
		data = append(data, []byte("\n# Deployment secrets never enter the build context\n.git\n**/.git\n.env\n.env.*\n**/.env\n**/.env.*\n.bundle\n**/master.key\n**/*.key\n")...)
		if err = os.WriteFile(file, data, 0600); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) preflightRails(ctx context.Context, a App, source string) (App, error) {
	if err := prepareRailsBuild(source); err != nil {
		return a, err
	}
	// Reuse managed volume identity and encrypted state, with the Rails UID.
	cfg := &config.Config{Packs: []string{"lidza/storage"}}
	effective := maps.Clone(a.Env)
	if effective == nil {
		effective = map[string]string{}
	}
	prepared, err := m.prepareAppStorage(ctx, a, cfg, effective)
	if err != nil {
		return a, err
	}
	return prepared, prepared.Validate()
}

func runtimeArgs(a App) []string {
	if a.Runtime == "astro" {
		return []string{"--env", "NODE_ENV=production", "--env", "HOST=0.0.0.0", "--env", "PORT=3000"}
	}
	if !a.isRails() {
		return []string{"--env", "LIDZA_ADDR=0.0.0.0:3000", "--env", "LIDZA_MODE=production"}
	}
	return []string{"--workdir", "/rails", "--env", "RAILS_ENV=production", "--env", "RACK_ENV=production", "--env", "RAILS_LOG_TO_STDOUT=true", "--env", "RAILS_SERVE_STATIC_FILES=true", "--env", "PORT=3000", "--tmpfs", "/rails/tmp:rw,nosuid,nodev,uid=1000,gid=1000,mode=0700,size=128m", "--tmpfs", "/rails/log:rw,nosuid,nodev,uid=1000,gid=1000,mode=0700,size=64m"}
}

func runtimeCommand(a App) []string {
	if a.isRails() {
		return []string{"./bin/rails", "server"}
	}
	return nil
}

func prepareAppBuild(source string, a App) error {
	if a.isRails() {
		if err := prepareRailsBuild(source); err != nil {
			return err
		}
		return secureBuildContext(source)
	}
	return prepareApplicationBuild(source, a)
}

func (a App) runtimeEnvironment() map[string]string {
	env := maps.Clone(a.Env)
	if a.isRails() && env["REDIS_URL"] == "" && env["CACHE_URL"] != "" {
		env["REDIS_URL"] = env["CACHE_URL"]
	}
	return env
}
