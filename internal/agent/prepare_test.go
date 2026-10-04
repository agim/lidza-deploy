package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutomaticDeploymentFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("lidza.json", `{"name":"portal","frontend":{"template":"htmx"}}`)
	write("go.mod", "module example.com/portal\n\ngo 1.27.0\ntoolchain go1.28.1\n\nrequire github.com/agim/lidza v0.1.68\n")
	if err := prepareBuild(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"github.com/agim/lidza/cmd/lidza@v0.1.68", "FROM golang:1.28-bookworm", "lidza build --out /out/portal", "/src/db /app/db"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("generated build missing %q", want)
		}
	}
	ignore, err := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ignore), "config/master.key") || !strings.Contains(string(ignore), ".env") {
		t.Fatal("framework secret exclusions missing")
	}
	write("Dockerfile", "# custom build\nFROM scratch\n")
	if err := prepareBuild(dir); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if string(b) != "# custom build\nFROM scratch\n" {
		t.Fatal("custom build overwritten")
	}
}
func TestBuildPreparationRejectsUnsafeAndMissingConfiguration(t *testing.T) {
	for _, kind := range []string{"missing-manifest", "local-replace", "symlink-output", "symlink-directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			manifest := filepath.Join(dir, "lidza.json")
			if kind != "missing-manifest" {
				os.WriteFile(manifest, []byte(`{"name":"portal","frontend":{"template":"htmx"}}`), 0600)
			}
			mod := "module example.com/portal\ngo 1.27.0\nrequire github.com/agim/lidza v0.1.69\n"
			if kind == "local-replace" {
				mod += "replace example.com/private => ../private\n"
			}
			os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0600)
			target := filepath.Join(t.TempDir(), "untouched")
			if kind == "symlink-output" {
				if err := os.Symlink(target, filepath.Join(dir, ".dockerignore")); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink-directory" {
				if err := os.Symlink(filepath.Dir(target), filepath.Join(dir, "deploy")); err != nil {
					t.Fatal(err)
				}
			}
			if err := prepareBuild(dir); err == nil {
				t.Fatal("unsafe or incomplete preparation accepted")
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatal("wrote outside checkout")
			}
		})
	}
}

func TestFrameworkGeneratedRustBuild(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "lidza.json"), []byte(`{"name":"portal","frontend":{"template":"htmx"}}`), 0600)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/portal\ngo 1.27.1\ntoolchain go1.27.0\nrequire github.com/agim/lidza v0.1.70\n"), 0600)
	os.MkdirAll(filepath.Join(dir, "packs", "compute", "rust"), 0700)
	os.WriteFile(filepath.Join(dir, "packs", "compute", "rust", "Cargo.toml"), []byte("[package]\nname = \"compute\"\nversion = \"0.1.0\"\n"), 0600)
	if err := prepareBuild(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if !strings.Contains(string(data), "FROM golang:1.27-bookworm") || !strings.Contains(string(data), "\nRUN curl --proto") {
		t.Fatal("framework version selection/Rust generation missing")
	}
}
