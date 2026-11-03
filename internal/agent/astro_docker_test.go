package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAstroDockerProductionDeployAndReload(t *testing.T) {
	if os.Getenv("TEST_ASTRO_DOCKER") != "1" {
		t.Skip("set TEST_ASTRO_DOCKER=1 against the isolated local Docker engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	engine, err := command(ctx, "", nil, "docker", "info", "--format", "{{.Name}}")
	if expected := os.Getenv("LIDZA_TEST_DOCKER_ENGINE"); err != nil || expected == "" || engine != expected || os.Getenv("DOCKER_HOST") != "" {
		t.Fatal("Astro acceptance requires an explicitly selected local Docker engine via LIDZA_TEST_DOCKER_ENGINE", err)
	}
	for _, mode := range []string{"static", "node"} {
		t.Run(mode, func(t *testing.T) {
			d := &Docker{Root: t.TempDir()}
			var diagnostic string
			ctx := context.WithValue(ctx, diagnosticStreamKey{}, func(name string) func(string) { return func(output string) { diagnostic = name + "\n" + output } })
			d.checkout = func(ctx context.Context, a App, source, token string) error {
				if err := os.MkdirAll(filepath.Join(source, "src/pages"), 0755); err != nil {
					return err
				}
				files := map[string]string{"package.json": `{"type":"module","scripts":{"build":"astro build"},"dependencies":{"astro":"7.4.4","@astrojs/node":"11.1.9"}}`, "src/pages/index.astro": "---\nconst site=import.meta.env.PUBLIC_SITE;\n---\n<h1>Production Astro</h1><p>{site}</p>"}
				if mode == "node" {
					files["astro.config.mjs"] = "import {defineConfig} from 'astro/config'; import node from '@astrojs/node'; export default defineConfig({output:'server',adapter:node({mode:'standalone'})});"
				}
				for name, data := range files {
					if err := os.WriteFile(filepath.Join(source, name), []byte(data), 0644); err != nil {
						return err
					}
				}
				if _, err := command(ctx, "", nil, "docker", "run", "--rm", "--mount", "type=bind,source="+source+",target=/app", "--workdir", "/app", "node:24-alpine", "npm", "install", "--package-lock-only", "--ignore-scripts", "--no-audit", "--no-fund"); err != nil {
					return err
				}
				for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "Fixture"}} {
					if _, err := command(ctx, source, nil, "git", args...); err != nil {
						return err
					}
				}
				return nil
			}
			a := App{ID: fmt.Sprintf("astro-test-%x", time.Now().UnixNano()), Runtime: "astro", Astro: &AstroConfig{Mode: mode, NodeMajor: "24"}, Domain: "astro.example.com", Repository: "fixture/site", Branch: "main", Env: map[string]string{"PUBLIC_SITE": "Public build value", "DATABASE_PASSWORD": "must-not-enter-build"}}
			var releases []*Release
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
				defer stop()
				for _, release := range releases {
					_ = d.Remove(cleanup, release)
				}
			}()
			first, err := d.Deploy(ctx, a, "first", "")
			if err != nil {
				t.Fatal("Astro production deployment", err, diagnostic)
			}
			releases = append(releases, first)
			if err := d.Ready(ctx, first); err != nil {
				t.Fatal("production readiness", err)
			}
			assertPage := func(release *Release) {
				t.Helper()
				response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/", release.Port))
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				if response.StatusCode != 200 || !strings.Contains(string(body), "Production Astro") {
					t.Fatal("production site missing", response.StatusCode, string(body))
				}
			}
			assertPage(first)
			a.Current = first
			next, err := d.Reload(ctx, a, "reload")
			if err != nil {
				t.Fatal("reload retained Astro image", err)
			}
			releases = append(releases, next)
			assertPage(next)
			assertPage(first)
			if next.HealthPath != first.HealthPath {
				t.Fatal("reload lost runtime-specific readiness")
			}
		})
	}
}
