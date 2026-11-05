package detect

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

var (
	lockedGem = func(name string) *regexp.Regexp {
		return regexp.MustCompile(`(?m)^    ` + regexp.QuoteMeta(name) + ` \(([^)]+)\)`)
	}
	lockedRuby    = regexp.MustCompile(`(?m)^RUBY VERSION\s*\n\s+ruby (\d+\.\d+(?:\.\d+)?)`)
	lockedBundler = regexp.MustCompile(`(?m)^BUNDLED WITH\s*\n\s+(\S+)`)
	gemfileRuby   = regexp.MustCompile(`(?m)^\s*ruby\s+["']([^"']+)["']`)
	gemfileGem    = func(name string) *regexp.Regexp {
		return regexp.MustCompile(`(?m)^\s*gem\s+["']` + regexp.QuoteMeta(name) + `["']`)
	}
	pumaSolidQueue = regexp.MustCompile(`(?m)^\s*plugin\s+:solid_queue\s*$`)
	railsHealth    = regexp.MustCompile(`rails/health#show`)
	databaseYAML   = regexp.MustCompile(`(?m)^\s*adapter:\s*["']?(\w+)`)
	cableRedis     = regexp.MustCompile(`(?m)^\s*adapter:\s*["']?redis`)
)

// gems answers whether a gem is part of the bundle.
type gems struct{ lock, gemfile string }

func (g gems) has(name string) bool {
	if g.lock != "" {
		return lockedGem(name).MatchString(g.lock)
	}
	return gemfileGem(name).MatchString(g.gemfile)
}

func (g gems) version(name string) string {
	if m := lockedGem(name).FindStringSubmatch(g.lock); m != nil {
		return m[1]
	}
	return ""
}

func (s *scanner) rails(dir string) (*app, error) {
	gemfile, err := s.text(path.Join(dir, "Gemfile"))
	if err != nil || gemfile == "" {
		return nil, err
	}
	lock, err := s.text(path.Join(dir, "Gemfile.lock"))
	if err != nil {
		return nil, err
	}
	application, err := s.isFile(path.Join(dir, "config/application.rb"))
	if err != nil {
		return nil, err
	}
	g := gems{lock: lock, gemfile: gemfile}
	if !application && !g.has("rails") && !g.has("railties") {
		return nil, nil
	}
	a := &app{runtime: RuntimeRails, framework: "rails"}
	a.version = g.version("rails")
	if a.version == "" {
		a.version = g.version("railties")
	}
	evidence := "Gemfile"
	if application {
		evidence += " and config/application.rb"
	}
	if a.version != "" {
		a.reason("%s: Ruby on Rails %s", evidence, majorMinor(a.version))
	} else {
		a.reason("%s: Ruby on Rails", evidence)
	}

	// Readiness: Rails 7.1+ generates the /up health check route.
	routes, err := s.text(path.Join(dir, "config/routes.rb"))
	if err != nil {
		return nil, err
	}
	switch {
	case railsHealth.MatchString(routes) || atLeast(a.version, 7, 1):
		a.health = "/up"
		a.reason("Readiness check: /up (Rails health check)")
	default:
		a.health = "/"
	}

	// Processes; a Procfile replaces these defaults later.
	railsBin, err := s.isFile(path.Join(dir, "bin/rails"))
	if err != nil {
		return nil, err
	}
	web := "bundle exec rails server"
	if railsBin {
		web = "bin/rails server"
	}
	a.processes = []Process{{Name: "web", Command: web, Web: true}}
	if g.has("puma") {
		a.reason("Web process: Puma (%s)", web)
	}
	if g.has("sidekiq") {
		a.processes = append(a.processes, Process{Name: "worker", Command: "bundle exec sidekiq"})
		a.reason("sidekiq gem: a worker process runs bundle exec sidekiq")
	}
	if g.has("solid_queue") {
		puma, err := s.text(path.Join(dir, "config/puma.rb"))
		if err != nil {
			return nil, err
		}
		if pumaSolidQueue.MatchString(puma) {
			a.reason("solid_queue runs inside Puma (config/puma.rb plugin)")
		} else {
			jobs, err := s.isFile(path.Join(dir, "bin/jobs"))
			if err != nil {
				return nil, err
			}
			command := "bundle exec rake solid_queue:start"
			if jobs {
				command = "bin/jobs"
			}
			a.processes = append(a.processes, Process{Name: "jobs", Command: command})
			a.reason("solid_queue gem: a jobs process runs %s", command)
		}
	}
	if g.has("good_job") {
		a.processes = append(a.processes, Process{Name: "jobs", Command: "bundle exec good_job start"})
		a.reason("good_job gem: a jobs process runs bundle exec good_job start")
	}
	if g.has("delayed_job") {
		a.processes = append(a.processes, Process{Name: "worker", Command: "bundle exec rake jobs:work"})
		a.reason("delayed_job gem: a worker process runs bundle exec rake jobs:work")
	}

	// Data services.
	database, err := s.text(path.Join(dir, "config/database.yml"))
	if err != nil {
		return nil, err
	}
	adapters := map[string]bool{}
	for _, m := range databaseYAML.FindAllStringSubmatch(database, -1) {
		adapters[m[1]] = true
	}
	switch {
	case g.has("pg") || adapters["postgresql"] || adapters["postgis"]:
		a.postgres = true
		a.reason("pg gem: PostgreSQL")
	case g.has("mysql2") || g.has("trilogy"):
		a.reason("MySQL adapter: supply an existing DATABASE_URL; only PostgreSQL is provisioned on the host")
	case g.has("sqlite3"):
		a.reason("SQLite: the database lives in the application's shared storage")
	}
	cable, err := s.text(path.Join(dir, "config/cable.yml"))
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"redis", "redis-client", "sidekiq", "hiredis", "kredis", "resque"} {
		if g.has(name) {
			a.redis = true
			a.reason("%s gem: Redis", name)
			break
		}
	}
	if !a.redis && cableRedis.MatchString(cable) {
		a.redis = true
		a.reason("config/cable.yml uses the Redis adapter: Redis")
	}

	// Toolchain pins.
	rubyVersion, err := s.text(path.Join(dir, ".ruby-version"))
	if err != nil {
		return nil, err
	}
	if v := strings.TrimPrefix(firstLine(rubyVersion), "ruby-"); v != "" {
		a.pin("ruby", v)
		a.reason(".ruby-version pins Ruby %s", v)
	} else if m := lockedRuby.FindStringSubmatch(lock); m != nil {
		a.pin("ruby", m[1])
		a.reason("Gemfile.lock pins Ruby %s", m[1])
	} else if m := gemfileRuby.FindStringSubmatch(gemfile); m != nil {
		a.pin("ruby", m[1])
		a.reason("Gemfile pins Ruby %s", m[1])
	}
	if m := lockedBundler.FindStringSubmatch(lock); m != nil {
		a.pin("bundler", m[1])
	}
	// Rails applications that bundle JavaScript also need Node.
	if ok, err := s.isFile(path.Join(dir, "package.json")); err != nil {
		return nil, err
	} else if ok {
		if err := s.nodePins(dir, a, nil); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

func majorMinor(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, ".")
}

// atLeast compares a dotted version's major and minor numbers.
func atLeast(version string, major, minor int) bool {
	parts := strings.Split(version, ".")
	if len(parts) == 0 || parts[0] == "" {
		return false
	}
	got, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	if got != major {
		return got > major
	}
	if len(parts) < 2 {
		return minor == 0
	}
	digits := strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' })
	gotMinor, err := strconv.Atoi(digits)
	return err == nil && gotMinor >= minor
}
