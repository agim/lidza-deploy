package release

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Step is one command a profile runs inside a release directory.
type Step struct {
	Name     string
	Args     []string
	Env      []string // added to the application environment
	Network  bool
	Database bool
	// When decides from the release tree whether the step applies; nil means always.
	When func(dir string) bool
	// Do, when set, replaces Args: the step does its own work through the
	// executor, reports to the log itself and never fails the deploy
	// (housekeeping such as removing unused gems after an install).
	Do func(ctx context.Context, x Executor, dir string, env []string, logf func(string, ...any))
}

// Profile describes how one kind of application is built and released.
type Profile struct {
	Name   string
	Shared Shared
	// Prepare writes per-release configuration before any step runs.
	Prepare func(l Layout, id string, env []string) error
	// Toolchain inspects the release's toolchain once the release is
	// prepared, before the build. Its environment entries apply to build,
	// release and schedule steps and are recorded for the release's
	// processes (RuntimeEnvFile). It never fails a deploy.
	Toolchain func(ctx context.Context, x Executor, dir string, env []string, logf func(string, ...any)) ToolchainCheck
	// Build turns source into a runnable tree. It must not change shared state.
	Build []Step
	// Release runs against shared state (databases) just before publication.
	Release []Step
	// Schedule returns the step that prints the application's crontab
	// entries for this layout, or nil when it defines none.
	Schedule func(l Layout) *Step
	// Output is the directory, relative to the release, that a web server
	// publishes for static sites ("" for applications that run processes).
	Output string
}

func exists(dir, path string) bool {
	_, err := os.Stat(filepath.Join(dir, path))
	return err == nil
}

func lockfileMentions(dir string, gems ...string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "Gemfile.lock"))
	if err != nil {
		return false
	}
	for _, gem := range gems {
		if regexp.MustCompile(`(?m)^    ` + regexp.QuoteMeta(gem) + ` \(`).Match(raw) {
			return true
		}
	}
	return false
}

func packageScript(dir, name string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	return json.Unmarshal(raw, &manifest) == nil && manifest.Scripts[name] != ""
}

// javascriptInstall installs exact locked dependencies with whichever
// package manager the repository is locked to.
func javascriptInstall() []Step {
	return []Step{
		{Name: "npm ci", Args: []string{"npm", "ci", "--include=dev", "--no-audit", "--no-fund"}, Network: true,
			When: func(dir string) bool { return exists(dir, "package-lock.json") }},
		{Name: "yarn install", Args: []string{"yarn", "install", "--frozen-lockfile", "--non-interactive"}, Network: true,
			When: func(dir string) bool { return exists(dir, "yarn.lock") && !exists(dir, "package-lock.json") }},
		{Name: "pnpm install", Args: []string{"pnpm", "install", "--frozen-lockfile"}, Network: true,
			When: func(dir string) bool {
				return exists(dir, "pnpm-lock.yaml") && !exists(dir, "package-lock.json") && !exists(dir, "yarn.lock")
			}},
	}
}

// viteBuildCacheDir is where Vite Ruby records its last build during a
// release build, relative to the release.
const viteBuildCacheDir = "node_modules/.cache/vite_ruby"

// RailsOptions adjusts the Rails profile.
type RailsOptions struct {
	Environment string   // RAILS_ENV; default production
	Shared      *Shared  // default: log, storage, tmp, public/system, public/uploads
	Migrate     *bool    // default true when db/migrate or a db/*schema.rb exists
	Precompile  *bool    // default true when an asset pipeline gem is locked
	Without     []string // bundle groups to skip; default development and test
	// ScheduleRoles limits Whenever jobs to these roles (its --roles flag);
	// jobs without roles are always included.
	ScheduleRoles []string
	// ScheduleVariables are extra Whenever --set variables. The profile sets
	// environment and path itself; neither keys nor values may contain '&' or '='.
	ScheduleVariables map[string]string
	// Clean removes gems the lockfile no longer needs from the application's
	// own bundle path after each install (bundle clean); default true.
	Clean *bool
}

// Rails builds a Ruby on Rails application: locked gems into shared/bundle,
// locked JavaScript packages per release, precompiled assets, migrations
// before publication, and Whenever schedules rendered against current.
func Rails(opts RailsOptions) Profile {
	environment := opts.Environment
	if environment == "" {
		environment = "production"
	}
	shared := Shared{Dirs: []string{"log", "storage", "tmp", "public/system", "public/uploads"}}
	if opts.Shared != nil {
		shared = *opts.Shared
	}
	without := opts.Without
	if len(without) == 0 {
		without = []string{"development", "test"}
	}
	precompile := func(dir string) bool {
		if opts.Precompile != nil {
			return *opts.Precompile
		}
		return exists(dir, "bin/rails") && lockfileMentions(dir, "propshaft", "sprockets", "vite_rails",
			"jsbundling-rails", "cssbundling-rails", "tailwindcss-rails", "importmap-rails")
	}
	migrate := func(dir string) bool {
		if opts.Migrate != nil {
			return *opts.Migrate
		}
		if !exists(dir, "bin/rails") {
			return false
		}
		schemas, _ := filepath.Glob(filepath.Join(dir, "db", "*schema.rb"))
		return exists(dir, "db/migrate") || len(schemas) > 0
	}
	railsEnv := []string{"RAILS_ENV=" + environment, "RACK_ENV=" + environment}
	build := []Step{{Name: "bundle install", Args: []string{"bundle", "install", "--jobs", "4", "--quiet"},
		Env: railsEnv, Network: true}}
	if opts.Clean == nil || *opts.Clean {
		// Gem versions an upgrade left behind in shared/bundle make every
		// later Bundler boot warn about ambiguous specs and waste disk.
		build = append(build, Step{Name: "bundle clean", Env: railsEnv, Do: bundleClean,
			When: func(dir string) bool { return bundleDeploymentPath(dir) != "" }})
	}
	build = append(build, javascriptInstall()...)
	// Dependencies were installed with their lockfile above: the bundling
	// gems must not run their own package-manager install, which would prune
	// development dependencies such as esbuild under NODE_ENV=production.
	// Precompilation boots the application, and initializers commonly reach
	// the database, so the step runs with database access.
	//
	// Vite Ruby skips a build when its record of the last build (default
	// tmp/cache/vite, which releases share) matches the watched files and a
	// client manifest exists. Its SSR build checks the client manifest that
	// the same task has just written, so with a shared record a new release
	// would skip its SSR bundle whenever the frontend is unchanged. The
	// record therefore lives in the release's own node_modules, which every
	// release installs afresh.
	build = append(build, Step{Name: "assets:precompile", Args: []string{"bundle", "exec", "rails", "assets:precompile"},
		Env: append(railsEnv, "NODE_ENV=production", "SKIP_YARN_INSTALL=1", "SKIP_BUN_INSTALL=1",
			"VITE_RUBY_SKIP_ASSETS_PRECOMPILE_INSTALL=true", "VITE_RUBY_BUILD_CACHE_DIR="+viteBuildCacheDir), Database: true, When: precompile})
	return Profile{
		Name:   "rails",
		Shared: shared,
		Prepare: func(l Layout, id string, env []string) error {
			// Gems are installed once into shared/bundle and reused by every
			// release; deployment mode refuses an unlocked Gemfile.
			groups := without
			if value, ok := envValue(env, "BUNDLE_WITHOUT"); ok && value != "" {
				groups = strings.FieldsFunc(value, func(r rune) bool { return r == ':' || r == ' ' || r == ',' })
			}
			body := fmt.Sprintf("---\nBUNDLE_PATH: %q\nBUNDLE_DEPLOYMENT: \"true\"\nBUNDLE_WITHOUT: %q\n",
				filepath.Join(l.Shared(), "bundle"), strings.Join(groups, ":"))
			if err := l.mkdirOwned(l.Release(id), ".bundle"); err != nil {
				return err
			}
			if err := l.mkdirOwned(l.Shared(), "bundle"); err != nil {
				return err
			}
			path := filepath.Join(l.Release(id), ".bundle", "config")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				return err
			}
			return l.chown(path)
		},
		Build: build,
		// Use the host's default Bundler when the lockfile pins an older one.
		Toolchain: checkRubyToolchain,
		// db:prepare migrates existing databases and creates and loads the
		// schema of databases that do not exist yet, so a first deploy and
		// every later one use the same step. Rollbacks never reverse it.
		Release: []Step{{Name: "db:prepare", Args: []string{"bundle", "exec", "rails", "db:prepare"},
			Env: railsEnv, Database: true, When: migrate}},
		// Jobs run from current, not from the release directory, so they keep
		// working after the release is pruned and follow every rollback.
		Schedule: func(l Layout) *Step {
			applies := func(dir string) bool {
				return exists(dir, "config/schedule.rb") && lockfileMentions(dir, "whenever")
			}
			set, err := wheneverSet(environment, l.Current(), opts.ScheduleVariables)
			if err != nil {
				// Fail the deploy before publication instead of installing a
				// schedule rendered from rejected variables.
				return &Step{Name: "whenever: " + err.Error(), Args: []string{"false"}, When: applies}
			}
			args := []string{"bundle", "exec", "whenever", "--load-file", "config/schedule.rb", "--set", set}
			if len(opts.ScheduleRoles) > 0 {
				for _, role := range opts.ScheduleRoles {
					if !identifierPattern.MatchString(role) {
						return &Step{Name: fmt.Sprintf("whenever: invalid role %q", role), Args: []string{"false"}, When: applies}
					}
				}
				args = append(args, "--roles", strings.Join(opts.ScheduleRoles, ","))
			}
			return &Step{Name: "whenever", Env: railsEnv, Args: args, When: applies}
		},
	}
}

// Node builds a JavaScript server application that runs its own processes.
func Node() Profile {
	build := javascriptInstall()
	build = append(build, Step{Name: "npm run build", Args: []string{"npm", "run", "build"}, Env: []string{"NODE_ENV=production"},
		When: func(dir string) bool { return packageScript(dir, "build") }})
	return Profile{Name: "node", Shared: Shared{Dirs: []string{"log"}}, Build: build}
}

// Static builds a static site (for example Astro) whose output directory is
// served directly by the web server.
func Static(output string) Profile {
	if output == "" {
		output = "dist"
	}
	profile := Node()
	profile.Name, profile.Shared, profile.Output = "static", Shared{}, output
	return profile
}

// Detect chooses a profile from the files in a release tree.
func Detect(dir string) (Profile, error) {
	switch {
	case exists(dir, "Gemfile") && exists(dir, "config/application.rb"):
		return Rails(RailsOptions{}), nil
	case exists(dir, "go.mod"):
		// Līdza applications carry a package.json for their frontend; the
		// Go module decides the type.
		mod, err := ReadGoModule(dir)
		if err != nil {
			return Profile{}, err
		}
		return Go(GoOptions{Binary: mod.DefaultBinary()}), nil
	case exists(dir, "astro.config.mjs") || exists(dir, "astro.config.ts") || exists(dir, "astro.config.js"):
		return Static("dist"), nil
	case exists(dir, "package.json"):
		return Node(), nil
	}
	return Profile{}, fmt.Errorf("release: cannot detect an application type in %s", dir)
}
