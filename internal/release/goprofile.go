package release

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/modfile"
)

// LidzaModule is the framework module whose command-line tool builds Līdza
// applications (frontend, embedding and the Go binary in one step).
const LidzaModule = "github.com/agim/lidza"

// SQLCVersion is the sqlc release Līdza applications with a sqlc.yaml
// generate their query code with.
const SQLCVersion = "v1.31.1"

// ToolchainDir is the per-release directory that holds links to the exact
// toolchains a release was built with and the tools installed for it
// (bin/). Processes started from the release use the same toolchains.
const ToolchainDir = ".lidza-toolchain"

var binaryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// GoOptions adjusts the Go profile.
type GoOptions struct {
	// Binary is the web binary's name under bin/; default is the last
	// element of the module path.
	Binary string
	Shared *Shared // default: storage, log
}

// GoModule is what the Go profile reads from a go.mod file.
type GoModule struct {
	Path      string
	Go        string // the go line, for example 1.24 or 1.24.3
	Toolchain string // the toolchain line without its "go" prefix, if any
	Lidza     string // required framework version, "" when not a Līdza application
}

// ReadGoModule parses dir/go.mod.
func ReadGoModule(dir string) (GoModule, error) {
	// Release trees come from the repository: read only regular files, so
	// a link cannot make the (privileged) caller read elsewhere.
	file := filepath.Join(dir, "go.mod")
	if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return GoModule{}, errors.New("release: go.mod must be a regular file under 1 MiB")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return GoModule{}, err
	}
	// Strict parsing reads the toolchain line, which lax parsing skips; lax
	// parsing still accepts files that a newer Go wrote with unknown directives.
	parsed, err := modfile.Parse(file, raw, nil)
	if err != nil {
		parsed, err = modfile.ParseLax(file, raw, nil)
	}
	if err != nil {
		return GoModule{}, fmt.Errorf("release: go.mod: %w", err)
	}
	var mod GoModule
	if parsed.Module != nil {
		mod.Path = parsed.Module.Mod.Path
	}
	if parsed.Go != nil {
		mod.Go = parsed.Go.Version
	}
	if parsed.Toolchain != nil {
		mod.Toolchain = strings.TrimPrefix(parsed.Toolchain.Name, "go")
	}
	for _, require := range parsed.Require {
		if require.Mod.Path == LidzaModule {
			mod.Lidza = require.Mod.Version
		}
	}
	return mod, nil
}

// GoVersion is the Go release the module asks for: its toolchain line when
// present, otherwise its go line.
func (m GoModule) GoVersion() string {
	if m.Toolchain != "" {
		return m.Toolchain
	}
	return m.Go
}

// DefaultBinary is the binary name the profile uses when none is given.
func (m GoModule) DefaultBinary() string {
	name := path.Base(m.Path)
	if !binaryPattern.MatchString(name) || name == "." {
		return "app"
	}
	return name
}

func lidzaApplication(dir string) bool {
	mod, err := ReadGoModule(dir)
	return err == nil && mod.Lidza != ""
}

// mainPackage reports whether dir holds Go files of package main.
func mainPackage(dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, file := range files {
		if info, err := os.Lstat(file); strings.HasSuffix(file, "_test.go") || err != nil || !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		if regexp.MustCompile(`(?m)^package main\b`).Match(raw) {
			return true
		}
	}
	return false
}

// commandPackages reports whether cmd/ holds at least one main package.
func commandPackages(dir string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, "cmd"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() && mainPackage(filepath.Join(dir, "cmd", entry.Name())) {
			return true
		}
	}
	return false
}

// cCompiler reports whether the host has a C compiler, which sqlc needs.
var cCompiler = func() bool {
	for _, name := range []string{"cc", "gcc", "clang"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	return false
}

// Go builds a Go application into bin/<Binary>: Līdza applications (go.mod
// requires the framework) with the framework's own tool at the version the
// module pins, plain Go applications with go build. Every main package
// under cmd/ is built too, so extra processes can run bin/<command>.
// Builds are static (CGO_ENABLED=0). The executor's environment must put
// the requested Go (and, for JavaScript, Node) toolchain on PATH and point
// GOPATH, GOCACHE and GOMODCACHE at the application's cache; tools the
// build installs (the lidza command, sqlc) go to the release's
// ToolchainDir/bin.
func Go(opts GoOptions) Profile {
	binary := opts.Binary
	if binary == "" {
		binary = "app"
	}
	shared := Shared{Dirs: []string{"storage", "log"}}
	if opts.Shared != nil {
		shared = *opts.Shared
	}
	if !binaryPattern.MatchString(binary) {
		return Profile{Name: "go", Shared: shared, Build: []Step{{Name: fmt.Sprintf("invalid Go binary name %q", binary), Args: []string{"false"}}}}
	}
	env := []string{"CGO_ENABLED=0", "GOFLAGS=-buildvcs=false"}
	tools := `export GOBIN="$PWD/` + ToolchainDir + `/bin"; export PATH="$GOBIN:$PATH"; `
	shell := func(script string) []string {
		return []string{"/bin/sh", "-c", "set -eu; " + tools + script}
	}
	target := "bin/" + binary
	lidza := lidzaApplication
	sqlc := func(dir string) bool { return lidza(dir) && exists(dir, "sqlc.yaml") }
	build := []Step{
		{Name: "go mod download", Args: []string{"go", "mod", "download"}, Env: env, Network: true,
			When: func(dir string) bool { return exists(dir, "go.mod") }},
		{Name: "install lidza", Env: env, Network: true, When: lidza,
			Args: shell(`v=$(go list -m -f '{{.Version}}' ` + LidzaModule + `); exec go install "` + LidzaModule + `/cmd/lidza@$v"`)},
		{Name: "install sqlc", Env: []string{"CGO_ENABLED=1", "GOFLAGS=-buildvcs=false"}, Network: true,
			When: func(dir string) bool { return sqlc(dir) && cCompiler() },
			Args: shell(`exec go install "github.com/sqlc-dev/sqlc/cmd/sqlc@` + SQLCVersion + `"`)},
	}
	build = append(build, javascriptInstall()...)
	build = append(build,
		Step{Name: "npm install", Args: []string{"npm", "install", "--no-audit", "--no-fund"}, Network: true,
			When: func(dir string) bool {
				return lidza(dir) && exists(dir, "package.json") && !exists(dir, "package-lock.json") &&
					!exists(dir, "yarn.lock") && !exists(dir, "pnpm-lock.yaml")
			}},
		Step{Name: "go build ./cmd/...", Env: env, When: commandPackages,
			Args: []string{"go", "build", "-trimpath", "-o", "bin/", "./cmd/..."}},
		Step{Name: "prepare lidza directories", Args: []string{"mkdir", "-p", "db", "mail", "admin", "config"}, When: lidza},
		// Generating query code needs sqlc, which needs a C compiler; a host
		// without one builds with the generated code the repository commits.
		Step{Name: "lidza build", Env: env, Network: true, Args: shell(`exec lidza build --out ` + target),
			When: func(dir string) bool { return lidza(dir) && (!sqlc(dir) || cCompiler()) }},
		Step{Name: "lidza build --pregenerated", Env: env, Network: true, Args: shell(`exec lidza build --pregenerated --out ` + target),
			When: func(dir string) bool { return sqlc(dir) && !cCompiler() }},
		Step{Name: "go build", Env: env, Args: []string{"go", "build", "-trimpath", "-o", target, "."},
			When: func(dir string) bool {
				return !lidza(dir) && !exists(dir, filepath.Join("cmd", binary)) && mainPackage(dir)
			}},
		Step{Name: "check " + target, Args: []string{"test", "-x", target}},
	)
	return Profile{Name: "go", Shared: shared, Build: build}
}
