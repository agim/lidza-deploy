package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAstroBuildProducesStaticAndStandaloneProductionImages(t *testing.T) {
	for _, mode := range []string{"static", "node"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"build":"astro build"},"dependencies":{"astro":"5.0.0","@astrojs/node":"9.0.0"}}`), 0600)
			os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte(`{}`), 0600)
			a := App{Runtime: "astro", Astro: &AstroConfig{Mode: mode, OutputDir: "public", NodeMajor: "24"}}
			if err := prepareAstroBuild(dir, a); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(dir, "Dockerfile"))
			file := string(data)
			for _, want := range []string{"npm ci", "npm run build", "node:24-alpine", "USER 65532:65532", "lidza_public_env", "/app/public"} {
				if !strings.Contains(file, want) {
					t.Fatal("production contract missing", want)
				}
			}
			if strings.Contains(file, "astro preview") || strings.Contains(file, "ARG ") {
				t.Fatal("preview server or credential-bearing build args")
			}
			if mode == "node" && !strings.Contains(file, "./dist/server/entry.mjs") {
				t.Fatal("missing standalone entry point")
			}
			if mode == "static" {
				data, _ := os.ReadFile(filepath.Join(dir, ".lidza-static.Caddyfile"))
				if !strings.Contains(string(data), "auto_https off") || !strings.Contains(string(data), "/.__lidza_health") {
					t.Fatal("static readiness or managed ingress contract missing")
				}
			}
		})
	}
}

func TestAstroBuildRejectsEscapesAndSymlinkWrites(t *testing.T) {
	for _, cfg := range []AstroConfig{{OutputDir: "../outside"}, {OutputDir: "dist;touch"}, {OutputDir: "/tmp"}, {OutputDir: "."}, {Mode: "preview"}, {NodeMajor: "$(id)"}, {HealthPath: "//evil.example"}} {
		if err := validateApplicationRuntime(App{Runtime: "astro", Astro: &cfg}); err == nil {
			t.Fatal("unsafe Astro configuration accepted", cfg)
		}
	}
	dir := t.TempDir()
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(dir, "app"))
	if _, err := applicationSourceDirectory(dir, App{Build: BuildConfig{SourceDirectory: "app"}}); err == nil {
		t.Fatal("source symlink escaped repository")
	}
	for _, source := range []string{"../outside", "/tmp", "a/../b", "dir\\other"} {
		if _, err := applicationSourceDirectory(dir, App{Build: BuildConfig{SourceDirectory: source}}); err == nil {
			t.Fatal("unsafe source directory", source)
		}
	}
}

func TestAstroOnlyBuildsExplicitPublicEnvironment(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "build.env")
	if err := astroPublicBuildEnvironment(App{Env: map[string]string{"PUBLIC_SITE": "safe'$(touch /tmp/no)", "DATABASE_URL": "private", "RAILS_MASTER_KEY": "private-key", "LIDZA_MASTER_KEY": "private-key"}}, file); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if strings.Contains(string(data), "private") || !strings.Contains(string(data), "'\\''") {
		t.Fatal("credential included or shell quote lost", string(data))
	}
	info, _ := os.Stat(file)
	if info.Mode().Perm() != 0600 {
		t.Fatal("public build file mode")
	}
}

func TestBuildContextProtectsDockerfileSpecificIgnoreAndRejectsSymlinkWrites(t *testing.T) {
	root := t.TempDir()
	specific := filepath.Join(root, "Dockerfile.dockerignore")
	os.WriteFile(specific, []byte("# Original rules\n!everything\n"), 0600)
	if err := secureBuildContext(root); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".dockerignore", "Dockerfile.dockerignore"} {
		data, _ := os.ReadFile(filepath.Join(root, name))
		for _, required := range []string{"**/.env", "**/master.key", "**/.npmrc"} {
			if !strings.Contains(string(data), required) {
				t.Fatal("credential exclusion missing", name, required)
			}
		}
	}
	out := filepath.Join(t.TempDir(), "must-not-change")
	os.WriteFile(out, []byte("original"), 0600)
	os.Remove(specific)
	os.Symlink(out, specific)
	if err := secureBuildContext(root); err == nil {
		t.Fatal("ignore file symlink was written")
	}
	data, _ := os.ReadFile(out)
	if string(data) != "original" {
		t.Fatal("external file changed")
	}
}
