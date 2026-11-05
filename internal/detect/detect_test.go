package detect

import (
	"errors"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func detect(t *testing.T, files map[string]string) Result {
	t.Helper()
	r, err := Detect(Files(files))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func hasReason(r Result, fragment string) bool {
	return slices.ContainsFunc(r.Reasons, func(reason string) bool { return strings.Contains(reason, fragment) })
}

func expectReason(t *testing.T, r Result, fragment string) {
	t.Helper()
	if !hasReason(r, fragment) {
		t.Fatalf("missing reason %q in %q", fragment, r.Reasons)
	}
}

func expectProcesses(t *testing.T, r Result, want ...Process) {
	t.Helper()
	if !reflect.DeepEqual(r.Processes, want) {
		t.Fatalf("processes = %+v, want %+v", r.Processes, want)
	}
}

const railsSidekiqLock = `GEM
  remote: https://rubygems.org/
  specs:
    pg (1.5.6)
    puma (6.4.2)
    rails (7.1.3)
    railties (7.1.3)
    redis-client (0.22.1)
    sidekiq (7.2.4)

PLATFORMS
  x86_64-linux

DEPENDENCIES
  pg
  puma
  rails (~> 7.1)
  sidekiq

RUBY VERSION
   ruby 3.2.2p53

BUNDLED WITH
   2.5.6
`

func TestRailsWithSidekiq(t *testing.T) {
	r := detect(t, map[string]string{
		"Gemfile":               "source 'https://rubygems.org'\ngem 'rails', '~> 7.1'\ngem 'pg'\ngem 'sidekiq'\n",
		"Gemfile.lock":          railsSidekiqLock,
		"config/application.rb": "module Shop; class Application < Rails::Application; end; end\n",
		"config/routes.rb":      "Rails.application.routes.draw do\nend\n",
		"bin/rails":             "#!/usr/bin/env ruby\n",
		".ruby-version":         "ruby-3.3.0\n",
	})
	if !r.Detected || r.Mode != ModeNative || r.Runtime != RuntimeRails || r.Framework != "rails" || r.FrameworkVersion != "7.1.3" {
		t.Fatalf("unexpected detection %+v", r)
	}
	expectProcesses(t, r, Process{Name: "web", Command: "bin/rails server", Web: true}, Process{Name: "worker", Command: "bundle exec sidekiq"})
	if !r.NeedsPostgres || !r.NeedsRedis || r.HealthPath != "/up" || r.SourceDirectory != "" {
		t.Fatalf("unexpected needs %+v", r)
	}
	if r.Pins["ruby"] != "3.3.0" || r.Pins["bundler"] != "2.5.6" {
		t.Fatalf("pins = %v", r.Pins)
	}
	expectReason(t, r, "Gemfile and config/application.rb: Ruby on Rails 7.1")
	expectReason(t, r, "sidekiq gem")
	expectReason(t, r, ".ruby-version pins Ruby 3.3.0")
}

func TestRails8SolidQueue(t *testing.T) {
	r := detect(t, map[string]string{
		"Gemfile":               "gem \"rails\", \"~> 8.0.1\"\ngem \"solid_queue\"\ngem \"sqlite3\"\n",
		"Gemfile.lock":          "GEM\n  specs:\n    rails (8.0.1)\n    solid_queue (1.1.2)\n    sqlite3 (2.5.0)\n    puma (6.5.0)\n\nRUBY VERSION\n   ruby 3.4.1p0\n",
		"config/application.rb": "module Blog; end\n",
		"config/puma.rb":        "plugin :solid_queue if ENV[\"SOLID_QUEUE_IN_PUMA\"]\n",
		"bin/rails":             "",
		"bin/jobs":              "",
		"Dockerfile":            "FROM ruby:3.4\n",
	})
	if r.Mode != ModeNative || r.Runtime != RuntimeRails || r.FrameworkVersion != "8.0.1" || r.HealthPath != "/up" {
		t.Fatalf("unexpected detection %+v", r)
	}
	expectProcesses(t, r, Process{Name: "web", Command: "bin/rails server", Web: true}, Process{Name: "jobs", Command: "bin/jobs"})
	if r.NeedsPostgres || r.NeedsRedis || r.Pins["ruby"] != "3.4.1" || !r.Dockerfile {
		t.Fatalf("unexpected needs %+v", r)
	}
	expectReason(t, r, "Ruby on Rails 8.0")
	expectReason(t, r, "Dockerfile is present, but the recognised framework builds natively")
	expectReason(t, r, "SQLite")
}

func TestRailsSolidQueueInsidePuma(t *testing.T) {
	r := detect(t, map[string]string{
		"Gemfile":               "gem 'rails'\n",
		"Gemfile.lock":          "GEM\n  specs:\n    rails (8.0.0)\n    solid_queue (1.0.0)\n",
		"config/application.rb": "",
		"config/puma.rb":        "plugin :solid_queue\n",
	})
	expectProcesses(t, r, Process{Name: "web", Command: "bundle exec rails server", Web: true})
	expectReason(t, r, "solid_queue runs inside Puma")
}

func TestRailsProcfileAndOlderHealth(t *testing.T) {
	r := detect(t, map[string]string{
		"Gemfile":               "gem 'rails'\n",
		"Gemfile.lock":          "GEM\n  specs:\n    rails (6.1.7)\n    sidekiq (6.5.0)\n",
		"config/application.rb": "",
		"Procfile":              "# processes\nweb: bundle exec puma -C config/puma.rb\nworker: bundle exec sidekiq -C config/sidekiq.yml\nrelease: bin/rails db:migrate\n",
		"Procfile.dev":          "web: bin/rails server -p 3000\n",
	})
	expectProcesses(t, r, Process{Name: "web", Command: "bundle exec puma -C config/puma.rb", Web: true}, Process{Name: "worker", Command: "bundle exec sidekiq -C config/sidekiq.yml"})
	if r.HealthPath != "/" {
		t.Fatalf("Rails 6.1 health path = %q", r.HealthPath)
	}
	expectReason(t, r, "Procfile defines processes: web, worker")
	expectReason(t, r, "release command")
}

func TestRailsComposeDataServicesStayNative(t *testing.T) {
	r := detect(t, map[string]string{
		"Gemfile":               "gem 'rails'\n",
		"Gemfile.lock":          "GEM\n  specs:\n    rails (7.2.0)\n",
		"config/application.rb": "",
		"docker-compose.yml":    "services:\n  web:\n    build: .\n  db:\n    image: postgres:16-alpine\n  cache:\n    image: \"redis:7\" # sessions\n  mail:\n    image: axllent/mailpit\n",
	})
	if r.Mode != ModeNative || !r.NeedsPostgres || !r.NeedsRedis {
		t.Fatalf("unexpected detection %+v", r)
	}
	expectReason(t, r, "docker-compose.yml services db, cache are provided by the host natively")
}

func TestLidzaApplication(t *testing.T) {
	r := detect(t, map[string]string{
		"go.mod":                "module github.com/acme/portal\n\ngo 1.27.0\n\ntoolchain go1.27.3\n\nrequire (\n\tgithub.com/agim/lidza v0.1.132\n\tgithub.com/valkey-io/valkey-go v1.0.79 // indirect\n\tgithub.com/jackc/pgx/v5 v5.11.0 // indirect\n)\n",
		"cmd/portal/main.go":    "package main\n",
		"cmd/migrate/main.go":   "package main\n",
		"db/migrations/001.sql": "create table t();\n",
		"web/package.json":      `{"dependencies":{"astro":"^5.0.0"}}`,
	})
	if r.Mode != ModeNative || r.Runtime != RuntimeGo || r.Framework != "lidza" || r.FrameworkVersion != "v0.1.132" || r.HealthPath != "/healthz" {
		t.Fatalf("unexpected detection %+v", r)
	}
	expectProcesses(t, r, Process{Name: "web", Command: "bin/portal", Web: true})
	if !r.NeedsPostgres || r.NeedsRedis {
		t.Fatalf("indirect requirements must not count: %+v", r)
	}
	if r.Pins["go"] != "1.27.0" || r.Pins["go_toolchain"] != "go1.27.3" {
		t.Fatalf("pins = %v", r.Pins)
	}
	expectReason(t, r, "go.mod requires github.com/agim/lidza v0.1.132: Līdza application")
	expectReason(t, r, "db/migrations")
}

func TestPlainGo(t *testing.T) {
	r := detect(t, map[string]string{
		"go.mod":  "module example.com/svc\n\ngo 1.23\n\nrequire (\n\tgithub.com/redis/go-redis/v9 v9.7.0\n\tgithub.com/lib/pq v1.10.9\n)\n",
		"main.go": "package main\n",
	})
	if r.Runtime != RuntimeGo || r.Framework != "go" || r.HealthPath != "/" || !r.NeedsPostgres || !r.NeedsRedis || r.Pins["go"] != "1.23" {
		t.Fatalf("unexpected detection %+v", r)
	}
	expectProcesses(t, r, Process{Name: "web", Command: "bin/svc", Web: true})
}

func TestAstroStatic(t *testing.T) {
	r := detect(t, map[string]string{
		"package.json":     `{"scripts":{"build":"astro build"},"dependencies":{"astro":"^5.1.0"}}`,
		"astro.config.mjs": "import { defineConfig } from 'astro/config';\nexport default defineConfig({ site: 'https://example.com' });\n",
		".nvmrc":           "v22\n",
	})
	if r.Mode != ModeNative || r.Runtime != RuntimeStatic || r.Framework != "astro" || r.StaticOutput != "dist" || len(r.Processes) != 0 || r.HealthPath != "" {
		t.Fatalf("unexpected detection %+v", r)
	}
	if r.Pins["node"] != "22" {
		t.Fatalf("pins = %v", r.Pins)
	}
	expectReason(t, r, "astro.config.mjs: Astro static site published from dist/")
}

func TestAstroNodeServer(t *testing.T) {
	r := detect(t, map[string]string{
		"package.json":    `{"dependencies":{"astro":"^4.16.0","@astrojs/node":"^8.0.0","pg":"^8.0.0"},"engines":{"node":">=20"}}`,
		"astro.config.ts": "import node from '@astrojs/node';\nexport default defineConfig({\n  output: 'server',\n  adapter: node({ mode: 'standalone' }),\n});\n",
	})
	if r.Runtime != RuntimeNode || r.Framework != "astro" || r.StaticOutput != "" || r.HealthPath != "/" || !r.NeedsPostgres {
		t.Fatalf("unexpected detection %+v", r)
	}
	expectProcesses(t, r, Process{Name: "web", Command: "node dist/server/entry.mjs", Web: true})
	if r.Pins["node"] != ">=20" {
		t.Fatalf("pins = %v", r.Pins)
	}
	expectReason(t, r, "output 'server' with @astrojs/node")
}

func TestNodeExpress(t *testing.T) {
	r := detect(t, map[string]string{
		"package.json":  `{"name":"api","scripts":{"start":"node server.js","worker":"node worker.js"},"dependencies":{"express":"^4.21.0","pg":"^8.13.0","ioredis":"^5.4.0"},"packageManager":"pnpm@9.12.0"}`,
		"server.js":     "",
		".node-version": "20.18.0\n",
	})
	if r.Runtime != RuntimeNode || r.Framework != "express" || !r.NeedsPostgres || !r.NeedsRedis || r.HealthPath != "/" {
		t.Fatalf("unexpected detection %+v", r)
	}
	expectProcesses(t, r, Process{Name: "web", Command: "npm start", Web: true}, Process{Name: "worker", Command: "npm run worker"})
	if r.Pins["node"] != "20.18.0" || r.Pins["package_manager"] != "pnpm@9.12.0" {
		t.Fatalf("pins = %v", r.Pins)
	}
	expectReason(t, r, "start script (node server.js): Express server")
}

func TestComposeThirdPartyServicesUseContainers(t *testing.T) {
	compose := `name: workspace
x-common: &common
  restart: unless-stopped
services:
  synapse:
    <<: *common
    image: ghcr.io/element-hq/synapse:v1.120.0
    volumes:
      - synapse:/data
  postfix:
    image: boky/postfix
    environment:
      image: not-a-field
  livekit:
    image: livekit/livekit-server:latest
  collabora:
    image: "collabora/code"
  app:
    build:
      context: .
volumes:
  synapse: {}
`
	r := detect(t, map[string]string{
		"compose.yaml": compose,
		"package.json": `{"scripts":{"start":"node index.js"},"dependencies":{"express":"^4.0.0"}}`,
	})
	if !r.Detected || r.Mode != ModeContainer || r.ComposeFile != "compose.yaml" || len(r.Services) != 5 {
		t.Fatalf("unexpected detection %+v", r)
	}
	if r.Services[0] != (ComposeService{Name: "synapse", Image: "ghcr.io/element-hq/synapse:v1.120.0", Role: "image"}) || r.Services[4] != (ComposeService{Name: "app", Build: true, Role: "source"}) || r.Services[1].Image != "boky/postfix" {
		t.Fatalf("services = %+v", r.Services)
	}
	expectReason(t, r, "compose.yaml defines 5 services (synapse, postfix, livekit, collabora, app) using third-party images: container")
	if r.Runtime != RuntimeNode {
		t.Fatal("the repository's own application should remain visible for an override", r.Runtime)
	}
}

func TestDockerfileOnlyUsesContainer(t *testing.T) {
	r := detect(t, map[string]string{"Dockerfile": "FROM alpine\n", "README.md": "# tool\n"})
	if !r.Detected || r.Mode != ModeContainer || r.Runtime != "" || !r.Dockerfile {
		t.Fatalf("unexpected detection %+v", r)
	}
	expectReason(t, r, "Dockerfile without a recognised framework: container")
}

func TestMonorepoSubdirectory(t *testing.T) {
	r := detect(t, map[string]string{
		"README.md":                "",
		"docs/index.html":          "",
		".github/ci.yml":           "",
		"apps/web/package.json":    `{"scripts":{"start":"next start","build":"next build"},"dependencies":{"next":"15.0.0","react":"19.0.0"}}`,
		"apps/web/next.config.mjs": "export default {}\n",
	})
	if !r.Detected || r.SourceDirectory != "apps/web" || r.Runtime != RuntimeNode || r.Framework != "next" {
		t.Fatalf("unexpected detection %+v", r)
	}
	expectProcesses(t, r, Process{Name: "web", Command: "npm start", Web: true})
	expectReason(t, r, "The repository root has no application; apps/web/ does")
}

func TestMonorepoWithSeveralApplications(t *testing.T) {
	r := detect(t, map[string]string{
		"api/go.mod":        "module example.com/api\n\ngo 1.23\n",
		"api/main.go":       "package main\n",
		"site/package.json": `{"dependencies":{"astro":"5.0.0"}}`,
	})
	if r.Detected || !reflect.DeepEqual(r.Candidates, []string{"api", "site"}) {
		t.Fatalf("unexpected detection %+v", r)
	}
	expectReason(t, r, "Several directories contain applications (api, site)")
}

func TestNothingDetected(t *testing.T) {
	r := detect(t, map[string]string{"README.md": "hello"})
	if r.Detected || r.Mode != ModeNative || len(r.Reasons) == 0 {
		t.Fatalf("unexpected detection %+v", r)
	}
}

func TestStaticSites(t *testing.T) {
	r := detect(t, map[string]string{"index.html": "<!doctype html>", "style.css": ""})
	if r.Runtime != RuntimeStatic || r.StaticOutput != "." || r.Framework != "html" {
		t.Fatalf("unexpected detection %+v", r)
	}
	r = detect(t, map[string]string{"package.json": `{"scripts":{"build":"vite build"},"devDependencies":{"vite":"^6.0.0"}}`, "index.html": ""})
	if r.Runtime != RuntimeStatic || r.StaticOutput != "dist" || r.Framework != "vite" {
		t.Fatalf("unexpected detection %+v", r)
	}
	r = detect(t, map[string]string{"package.json": `{"scripts":{"build":"next build"},"dependencies":{"next":"15.0.0"}}`, "next.config.js": "module.exports = { output: 'export' }\n"})
	if r.Runtime != RuntimeStatic || r.StaticOutput != "out" || r.Framework != "next" {
		t.Fatalf("unexpected detection %+v", r)
	}
}

func TestSourceErrorsAndLimits(t *testing.T) {
	failing := Tree([]string{"go.mod"}, func(string) ([]byte, error) { return nil, errors.New("network down") })
	if _, err := Detect(failing); err == nil || !strings.Contains(err.Error(), "network down") {
		t.Fatal("read errors must be reported", err)
	}
	large := Files(map[string]string{"package.json": strings.Repeat(" ", MaxFileSize+1), "index.html": ""})
	r, err := Detect(large)
	if err != nil || r.Runtime != RuntimeStatic {
		t.Fatal("oversized files are skipped, not fatal", r, err)
	}
	src := Files(map[string]string{"a/b/c.txt": "x"})
	if names, err := src.List("a"); err != nil || !reflect.DeepEqual(names, []string{"b/"}) {
		t.Fatal(names, err)
	}
	if _, err := src.List("missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := src.Read("a/b"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestParseComposeServices(t *testing.T) {
	services := parseComposeServices("version: '3'\nservices:\n\n  db: # primary\n    image: postgis/postgis:16-3.4\n  worker:\n    image: ${REGISTRY}/worker@sha256:abc\n    build: ./worker\n  'quoted':\n    image: 'mariadb:11'\n")
	want := []ComposeService{{Name: "db", Image: "postgis/postgis:16-3.4", Role: "data"}, {Name: "worker", Image: "${REGISTRY}/worker@sha256:abc", Build: true, Role: "source"}, {Name: "quoted", Image: "mariadb:11", Role: "image"}}
	if !reflect.DeepEqual(services, want) {
		t.Fatalf("services = %+v", services)
	}
}

func TestDetectDirectory(t *testing.T) {
	src := Files(map[string]string{
		"api/go.mod":        "module example.com/api\n\ngo 1.23\n",
		"api/main.go":       "package main\n",
		"site/package.json": `{"dependencies":{"astro":"5.0.0"}}`,
	})
	r, err := DetectDirectory(src, "/api/")
	if err != nil || !r.Detected || r.SourceDirectory != "api" || r.Runtime != RuntimeGo {
		t.Fatalf("unexpected detection %+v %v", r, err)
	}
	expectProcesses(t, r, Process{Name: "web", Command: "bin/api", Web: true})
	r, err = DetectDirectory(src, "missing")
	if err != nil || r.Detected {
		t.Fatalf("unexpected detection %+v %v", r, err)
	}
	expectReason(t, r, "No supported application was found in missing/")
}

func TestStackManifestMeansContainersEvenForABuildableApplication(t *testing.T) {
	r := detect(t, map[string]string{
		"go.mod":                  "module github.com/acme/suite\n\ngo 1.27.0\n\nrequire github.com/agim/lidza v0.1.132\n",
		"main.go":                 "package main\n",
		"deploy/lidza.stack.json": `{"version":2,"services":[{"name":"app","role":"web"},{"name":"mail","role":"worker"},{"name":"office","role":"worker"}]}`,
	})
	if r.Mode != ModeContainer || r.StackManifest != "deploy/lidza.stack.json" || strings.Join(r.StackServices, ",") != "app,mail,office" || r.StackManifestMissing {
		t.Fatalf("unexpected detection %+v", r)
	}
	expectReason(t, r, "deploy/lidza.stack.json defines a container stack of 3 services (app, mail, office): container")
}

func TestComposeWithoutStackManifestIsFlagged(t *testing.T) {
	r := detect(t, map[string]string{
		"compose.yaml": "services:\n  synapse:\n    image: ghcr.io/element-hq/synapse\n  postfix:\n    image: boky/postfix\n",
	})
	if r.Mode != ModeContainer || !r.StackManifestMissing {
		t.Fatalf("unexpected detection %+v", r)
	}
	expectReason(t, r, "deploy/lidza.stack.json stack manifest")
}
