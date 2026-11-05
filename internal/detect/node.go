package detect

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

type packageJSON struct {
	Name            string            `json:"name"`
	Main            string            `json:"main"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Engines         struct {
		Node string `json:"node"`
	} `json:"engines"`
	PackageManager string `json:"packageManager"`
}

func (p packageJSON) has(name string) bool {
	_, runtime := p.Dependencies[name]
	_, dev := p.DevDependencies[name]
	return runtime || dev
}

func (p packageJSON) version(name string) string {
	v := p.Dependencies[name]
	if v == "" {
		v = p.DevDependencies[name]
	}
	return strings.TrimLeft(v, "^~>=< ")
}

var (
	astroOutput       = regexp.MustCompile(`output\s*:\s*["'](server|hybrid|static)["']`)
	astroOutDir       = regexp.MustCompile(`outDir\s*:\s*["']\.?/?([A-Za-z0-9_./-]+?)/?["']`)
	astroNodeAdapter  = regexp.MustCompile(`@astrojs/node`)
	astroAdapterMatch = regexp.MustCompile(`adapter\s*:\s*([A-Za-z_$][\w$]*)`)
	astroMiddleware   = regexp.MustCompile(`mode\s*:\s*["']middleware["']`)
	nextExport        = regexp.MustCompile(`output\s*:\s*["']export["']`)
	nextDistDir       = regexp.MustCompile(`distDir\s*:\s*["']\.?/?([A-Za-z0-9_./-]+?)/?["']`)
	prismaPostgres    = regexp.MustCompile(`provider\s*=\s*"(postgresql|postgres)"`)
)

func (s *scanner) node(dir string) (*app, error) {
	raw, err := s.read(path.Join(dir, "package.json"))
	if err != nil || raw == nil {
		return nil, err
	}
	var pkg packageJSON
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return &app{runtime: RuntimeNode, framework: "node", health: "/", processes: []Process{{Name: "web", Command: "npm start", Web: true}}, reasons: []string{"package.json could not be parsed; check the start command"}}, nil
	}
	astroConfig, err := s.firstFile(dir, "astro.config.mjs", "astro.config.ts", "astro.config.js", "astro.config.mts", "astro.config.cjs")
	if err != nil {
		return nil, err
	}
	var a *app
	switch {
	case astroConfig != "" || pkg.has("astro"):
		a, err = s.astro(dir, astroConfig, pkg)
	case pkg.has("next"):
		a, err = s.next(dir, pkg)
	case pkg.has("nuxt"):
		a = &app{runtime: RuntimeNode, framework: "nuxt", version: pkg.version("nuxt"), health: "/"}
		a.reason("package.json depends on nuxt: Nuxt server")
		a.processes = []Process{{Name: "web", Command: startCommand(pkg, "node .output/server/index.mjs"), Web: true}}
	case pkg.has("@sveltejs/kit"):
		a = &app{framework: "sveltekit", version: pkg.version("@sveltejs/kit"), health: "/"}
		if pkg.has("@sveltejs/adapter-static") {
			a.runtime, a.static, a.health = RuntimeStatic, "build", ""
			a.reason("SvelteKit with adapter-static: static site published from build/")
		} else {
			a.runtime = RuntimeNode
			a.processes = []Process{{Name: "web", Command: startCommand(pkg, "node build"), Web: true}}
			if !pkg.has("@sveltejs/adapter-node") {
				a.reason("SvelteKit without @sveltejs/adapter-node: add the Node adapter to run on a server")
			} else {
				a.reason("SvelteKit with adapter-node: Node server")
			}
		}
	default:
		a = s.nodeServer(pkg)
	}
	if err != nil || a == nil {
		return a, err
	}
	if err := s.nodePins(dir, a, &pkg); err != nil {
		return nil, err
	}
	if a.runtime == RuntimeNode {
		if worker := pkg.Scripts["worker"]; worker != "" {
			a.processes = append(a.processes, Process{Name: "worker", Command: "npm run worker"})
			a.reason("package.json worker script: a worker process runs npm run worker")
		}
		if err := s.nodeData(dir, pkg, a); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func startCommand(pkg packageJSON, fallback string) string {
	if pkg.Scripts["start"] != "" {
		return "npm start"
	}
	return fallback
}

func (s *scanner) astro(dir, config string, pkg packageJSON) (*app, error) {
	a := &app{framework: "astro", version: pkg.version("astro")}
	text := ""
	if config != "" {
		var err error
		if text, err = s.text(path.Join(dir, config)); err != nil {
			return nil, err
		}
	} else {
		config = "package.json"
	}
	output := "static"
	if m := astroOutput.FindStringSubmatch(text); m != nil {
		output = m[1]
	}
	outDir := "dist"
	if m := astroOutDir.FindStringSubmatch(text); m != nil && cleanRelative(m[1]) {
		outDir = m[1]
	}
	nodeAdapter := astroNodeAdapter.MatchString(text) && astroAdapterMatch.MatchString(text)
	explicit := astroOutput.MatchString(text)
	switch {
	case nodeAdapter && (output == "server" || output == "hybrid" || !explicit):
		a.runtime, a.health = RuntimeNode, "/"
		entry := "node " + outDir + "/server/entry.mjs"
		if astroMiddleware.MatchString(text) {
			entry = startCommand(pkg, entry)
		}
		a.processes = []Process{{Name: "web", Command: entry, Web: true}}
		if explicit {
			a.reason("%s uses output '%s' with @astrojs/node: Node server (%s)", config, output, entry)
		} else {
			a.reason("%s uses the @astrojs/node adapter: Node server (%s)", config, entry)
		}
	case output == "server" || output == "hybrid":
		a.runtime, a.health = RuntimeNode, "/"
		a.processes = []Process{{Name: "web", Command: startCommand(pkg, "node "+outDir+"/server/entry.mjs"), Web: true}}
		a.reason("%s uses output '%s' without @astrojs/node: add the Node adapter to run on a server", config, output)
	default:
		a.runtime, a.static = RuntimeStatic, outDir
		a.reason("%s: Astro static site published from %s/", config, outDir)
	}
	return a, nil
}

func (s *scanner) next(dir string, pkg packageJSON) (*app, error) {
	a := &app{framework: "next", version: pkg.version("next")}
	config, err := s.firstFile(dir, "next.config.js", "next.config.mjs", "next.config.ts", "next.config.cjs")
	if err != nil {
		return nil, err
	}
	text := ""
	if config != "" {
		if text, err = s.text(path.Join(dir, config)); err != nil {
			return nil, err
		}
	}
	if nextExport.MatchString(text) {
		out := "out"
		if m := nextDistDir.FindStringSubmatch(text); m != nil && cleanRelative(m[1]) {
			out = m[1]
		}
		a.runtime, a.static = RuntimeStatic, out
		a.reason("%s uses output 'export': static site published from %s/", config, out)
		return a, nil
	}
	a.runtime, a.health = RuntimeNode, "/"
	a.processes = []Process{{Name: "web", Command: startCommand(pkg, "npx next start"), Web: true}}
	a.reason("package.json depends on next: Next.js server")
	return a, nil
}

// Server frameworks recognised from package.json dependencies, in order.
var nodeFrameworks = []struct{ dependency, name, label string }{
	{"@nestjs/core", "nest", "NestJS"},
	{"@remix-run/serve", "remix", "Remix"},
	{"@remix-run/node", "remix", "Remix"},
	{"express", "express", "Express"},
	{"fastify", "fastify", "Fastify"},
	{"koa", "koa", "Koa"},
	{"hono", "hono", "Hono"},
	{"@hapi/hapi", "hapi", "hapi"},
}

func (s *scanner) nodeServer(pkg packageJSON) *app {
	a := &app{runtime: RuntimeNode, framework: "node", health: "/"}
	label := "Node.js"
	for _, f := range nodeFrameworks {
		if pkg.has(f.dependency) {
			a.framework, a.version, label = f.name, pkg.version(f.dependency), f.label
			break
		}
	}
	switch {
	case pkg.Scripts["start"] != "":
		a.processes = []Process{{Name: "web", Command: "npm start", Web: true}}
		a.reason("package.json start script (%s): %s server", pkg.Scripts["start"], label)
	case a.framework == "nest" && pkg.Scripts["start:prod"] != "":
		a.processes = []Process{{Name: "web", Command: "npm run start:prod", Web: true}}
		a.reason("package.json start:prod script: NestJS server")
	case pkg.Main != "" && cleanRelative(strings.TrimPrefix(pkg.Main, "./")):
		command := "node " + strings.TrimPrefix(pkg.Main, "./")
		a.processes = []Process{{Name: "web", Command: command, Web: true}}
		a.reason("package.json main (%s): %s server", pkg.Main, label)
	case a.framework == "node" && pkg.Scripts["build"] != "" && (pkg.has("vite") || pkg.has("react-scripts") || pkg.has("parcel")):
		out := "dist"
		if pkg.has("react-scripts") {
			out = "build"
		}
		a.runtime, a.framework, a.static, a.health = RuntimeStatic, "vite", out, ""
		if pkg.has("react-scripts") {
			a.framework = "create-react-app"
		} else if pkg.has("parcel") {
			a.framework = "parcel"
		}
		a.reason("package.json build script without a server: static site published from %s/", out)
	default:
		a.processes = []Process{{Name: "web", Command: "npm start", Web: true}}
		a.reason("package.json without a start script: set the web command")
	}
	return a
}

func (s *scanner) nodeData(dir string, pkg packageJSON, a *app) error {
	for _, name := range []string{"pg", "postgres", "pg-promise", "@neondatabase/serverless", "@vercel/postgres"} {
		if pkg.has(name) {
			a.postgres = true
			a.reason("%s package: PostgreSQL", name)
			break
		}
	}
	if !a.postgres && (pkg.has("prisma") || pkg.has("@prisma/client")) {
		schema, err := s.text(path.Join(dir, "prisma/schema.prisma"))
		if err != nil {
			return err
		}
		if prismaPostgres.MatchString(schema) {
			a.postgres = true
			a.reason("prisma/schema.prisma uses PostgreSQL")
		}
	}
	for _, name := range []string{"ioredis", "redis", "@redis/client", "bullmq", "bull", "connect-redis"} {
		if pkg.has(name) {
			a.redis = true
			a.reason("%s package: Redis", name)
			break
		}
	}
	return nil
}

// nodePins records the Node version and package manager a repository pins.
func (s *scanner) nodePins(dir string, a *app, pkg *packageJSON) error {
	if pkg == nil {
		raw, err := s.read(path.Join(dir, "package.json"))
		if err != nil {
			return err
		}
		var parsed packageJSON
		if json.Unmarshal(raw, &parsed) == nil {
			pkg = &parsed
		}
	}
	for _, file := range []string{".node-version", ".nvmrc"} {
		text, err := s.text(path.Join(dir, file))
		if err != nil {
			return err
		}
		if v := strings.TrimPrefix(firstLine(text), "v"); v != "" {
			a.pin("node", v)
			a.reason("%s pins Node %s", file, v)
			break
		}
	}
	if pkg != nil {
		if v := strings.TrimSpace(pkg.Engines.Node); v != "" {
			if _, set := a.pins["node"]; !set {
				a.reason("package.json engines.node: %s", v)
			}
			a.pin("node", v)
		}
		a.pin("package_manager", pkg.PackageManager)
	}
	return nil
}

// html recognises a plain static site.
func (s *scanner) html(dir string) (*app, error) {
	ok, err := s.isFile(path.Join(dir, "index.html"))
	if err != nil || !ok {
		return nil, err
	}
	a := &app{runtime: RuntimeStatic, framework: "html", static: "."}
	a.reason("index.html without a build: static site published from the directory")
	return a, nil
}

// procfile replaces the default processes with a Procfile's.
func (s *scanner) procfile(dir string, a *app) error {
	if a.runtime == RuntimeStatic {
		return nil
	}
	name, err := s.firstFile(dir, "Procfile.prod", "Procfile.production", "Procfile")
	if err != nil || name == "" {
		return err
	}
	text, err := s.text(path.Join(dir, name))
	if err != nil {
		return err
	}
	var processes []Process
	web := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, command, ok := strings.Cut(line, ":")
		key, command = strings.TrimSpace(key), strings.TrimSpace(command)
		if !ok || command == "" || !processName.MatchString(key) {
			continue
		}
		if key == "release" {
			a.reason("%s release command (%s) runs as a release step, not a process", name, command)
			continue
		}
		p := Process{Name: key, Command: command, Web: key == "web"}
		web = web || p.Web
		processes = append(processes, p)
	}
	if len(processes) == 0 {
		return nil
	}
	if !web {
		// Keep the detected web process; the Procfile adds the others.
		for _, p := range a.processes {
			if p.Web {
				processes = append([]Process{p}, processes...)
				break
			}
		}
		if len(processes) > 0 && !processes[0].Web {
			processes[0].Web = true
		}
	}
	a.processes = processes
	names := make([]string, 0, len(processes))
	for _, p := range processes {
		names = append(names, p.Name)
	}
	a.reason("%s defines processes: %s", name, strings.Join(names, ", "))
	return nil
}

var processName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func cleanRelative(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && !strings.Contains(p, "..") && path.Clean(p) == p
}
