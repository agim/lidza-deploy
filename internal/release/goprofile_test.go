package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// applies returns the names of the steps that run for the tree.
func applies(profile Profile, dir string) []string {
	var names []string
	for _, step := range profile.Build {
		if step.When == nil || step.When(dir) {
			names = append(names, step.Name)
		}
	}
	return names
}

func TestDetectGoBeforeJavaScript(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"go.mod":       "module github.com/acme/shop-lidza\n\ngo 1.27.0\n\ntoolchain go1.27.3\n\nrequire github.com/agim/lidza v0.1.132\n",
		"package.json": `{"scripts":{"build":"vite build"}}`,
		"main.go":      "package main\nfunc main() {}\n",
	})
	profile, err := Detect(dir)
	if err != nil || profile.Name != "go" {
		t.Fatalf("detected %q, %v", profile.Name, err)
	}
	mod, err := ReadGoModule(dir)
	if err != nil || mod.GoVersion() != "1.27.3" || mod.Lidza != "v0.1.132" || mod.DefaultBinary() != "shop-lidza" {
		t.Fatalf("module = %+v, %v", mod, err)
	}
	if !slices.ContainsFunc(profile.Build, func(step Step) bool { return step.Name == "check bin/shop-lidza" }) {
		t.Fatalf("detected profile builds %v", profile.Build[len(profile.Build)-1].Args)
	}
}

func TestGoProfileBuildsLidzaApplicationsWithTheirPinnedTool(t *testing.T) {
	old := cCompiler
	t.Cleanup(func() { cCompiler = old })
	cCompiler = func() bool { return false }
	dir := writeTree(t, map[string]string{
		"go.mod":              "module example.com/shop\n\ngo 1.26\n\nrequire (\n\tgithub.com/agim/lidza v0.1.120\n)\n",
		"package-lock.json":   "{}",
		"package.json":        "{}",
		"sqlc.yaml":           "version: 2\n",
		"cmd/migrate/main.go": "package main\nfunc main() {}\n",
	})
	got := applies(Go(GoOptions{Binary: "shop"}), dir)
	want := []string{"go mod download", "install lidza", "npm ci", "go build ./cmd/...", "prepare lidza directories", "lidza build --pregenerated", "check bin/shop", "link module binary"}
	if !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	cCompiler = func() bool { return true }
	got = applies(Go(GoOptions{Binary: "shop"}), dir)
	if !slices.Contains(got, "install sqlc") || !slices.Contains(got, "lidza build") || slices.Contains(got, "lidza build --pregenerated") {
		t.Fatalf("with a C compiler: %v", got)
	}
	for _, step := range Go(GoOptions{Binary: "shop"}).Build {
		if step.Name == "go build" || step.Name == "go mod download" {
			if !slices.Contains(step.Env, "CGO_ENABLED=0") {
				t.Fatalf("%s must build statically", step.Name)
			}
		}
	}
}

func TestGoProfileBuildsPlainGoApplications(t *testing.T) {
	root := writeTree(t, map[string]string{"go.mod": "module example.com/api\n\ngo 1.24.3\n", "main.go": "package main\nfunc main() {}\n"})
	if got := applies(Go(GoOptions{Binary: "api"}), root); !slices.Equal(got, []string{"go mod download", "go build", "check bin/api", "link module binary"}) {
		t.Fatalf("root main: %v", got)
	}
	command := writeTree(t, map[string]string{"go.mod": "module example.com/api\n\ngo 1.24.3\n", "cmd/api/main.go": "package main\nfunc main() {}\n", "lib.go": "package api\n"})
	if got := applies(Go(GoOptions{Binary: "api"}), command); !slices.Equal(got, []string{"go mod download", "go build ./cmd/...", "check bin/api", "link module binary"}) {
		t.Fatalf("cmd main: %v", got)
	}
	if profile := Go(GoOptions{Binary: "../evil"}); len(profile.Build) != 1 || profile.Build[0].Args[0] != "false" {
		t.Fatal("an unsafe binary name must fail the build")
	}
}

func TestReadGoModuleRefusesLinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(target, []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "go.mod")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGoModule(dir); err == nil {
		t.Fatal("a linked go.mod must not be read")
	}
}

func TestGoProfileLinksTheModuleNamedBinary(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	dir := writeTree(t, map[string]string{"go.mod": "module example.com/acme/shop-lidza\n\ngo 1.21\n", "bin/shop": "#!/bin/sh\n"})
	if err := os.Chmod(filepath.Join(dir, "bin", "shop"), 0o755); err != nil {
		t.Fatal(err)
	}
	var link Step
	for _, step := range Go(GoOptions{Binary: "shop"}).Build {
		if step.Name == "link module binary" {
			link = step
		}
	}
	for range 2 { // idempotent
		cmd := exec.Command(link.Args[0], link.Args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	target, err := os.Readlink(filepath.Join(dir, "bin", "shop-lidza"))
	if err != nil || target != "shop" {
		t.Fatalf("module-named binary link = %q, %v", target, err)
	}
}
