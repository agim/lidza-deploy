// Package detect recognises how a repository is built and run. It reads a
// small number of well-known files (Gemfile.lock, go.mod, package.json,
// compose files, Procfiles) through a Source and proposes a deployment:
// a native build under systemd for frameworks the host can build itself, or
// containers for repositories made of third-party service images.
//
// Detection is pure and deterministic: it never executes repository code and
// only reads files through the Source it is given.
package detect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/agim/lidza-deploy/internal/buildmem"
)

// Deployment modes.
const (
	ModeNative    = "native"
	ModeContainer = "container"
)

// Runtimes a native application can use.
const (
	RuntimeGo     = "go"
	RuntimeRails  = "rails"
	RuntimeNode   = "node"
	RuntimeStatic = "static"
)

// MaxFileSize is the largest file detection reads. Sources should refuse
// larger files with ErrTooLarge; detection also truncates defensively.
const MaxFileSize = 256 << 10

// ErrTooLarge reports a file that exceeds MaxFileSize. Detection treats it as
// unreadable and continues.
var ErrTooLarge = errors.New("detect: file exceeds the size limit")

// Source lists and reads a repository tree. Paths are slash-separated and
// relative to the repository root; "" is the root directory.
type Source interface {
	// List returns the names of the entries directly inside dir. Directory
	// names end in "/". A missing directory returns fs.ErrNotExist.
	List(dir string) ([]string, error)
	// Read returns a file's contents. A missing file returns fs.ErrNotExist
	// and a file larger than MaxFileSize returns ErrTooLarge.
	Read(path string) ([]byte, error)
}

// Process is one long-running process of a native application.
type Process struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Web     bool   `json:"web,omitempty"`
}

// ComposeService is one service of a compose file.
type ComposeService struct {
	Name  string `json:"name"`
	Image string `json:"image,omitempty"`
	Build bool   `json:"build,omitempty"`
	// Role is "data" for services the host provides natively (PostgreSQL,
	// Redis), "development" for local tooling, "source" for services built
	// from the repository and "image" for third-party images.
	Role string `json:"role"`
}

// Result is a proposed deployment.
type Result struct {
	// Detected is false when no supported application was recognised; the
	// remaining fields are then defaults the operator must complete.
	Detected bool   `json:"detected"`
	Mode     string `json:"mode"`
	// Runtime is the native runtime: go, rails, node or static. It is also
	// set in container mode when the repository's own application was
	// recognised, so the operator can switch to a native deployment.
	Runtime          string `json:"runtime,omitempty"`
	Framework        string `json:"framework,omitempty"`
	FrameworkVersion string `json:"framework_version,omitempty"`
	SourceDirectory  string `json:"source_directory,omitempty"`
	// Processes are the long-running processes; exactly one is the web
	// process. Static sites have none.
	Processes     []Process `json:"processes,omitempty"`
	NeedsPostgres bool      `json:"needs_postgres"`
	NeedsRedis    bool      `json:"needs_redis"`
	// NeedsClickHouse reports that the application uses ClickHouse: the
	// host provides a ClickHouse database for it.
	NeedsClickHouse bool              `json:"needs_clickhouse"`
	Pins            map[string]string `json:"pins,omitempty"`
	HealthPath      string            `json:"health_path,omitempty"`
	StaticOutput    string            `json:"static_output,omitempty"`
	Dockerfile      bool              `json:"dockerfile,omitempty"`
	ComposeFile     string            `json:"compose_file,omitempty"`
	Services        []ComposeService  `json:"services,omitempty"`
	// StackManifest is the container stack definition the agent deploys
	// (deploy/lidza.stack.json); StackServices are its service names.
	StackManifest string   `json:"stack_manifest,omitempty"`
	StackServices []string `json:"stack_services,omitempty"`
	// StackManifestMissing is set when the repository needs a container
	// stack (a compose file with service images) but has no stack manifest,
	// so it cannot be deployed as it is.
	StackManifestMissing bool `json:"stack_manifest_missing,omitempty"`
	// Candidates lists directories that each contain an application when
	// the repository root does not.
	Candidates []string `json:"candidates,omitempty"`
	// Reasons explain each decision in plain language.
	Reasons []string `json:"reasons"`
	// BuildMemory estimates the memory the build needs at its largest
	// step, so a server can be checked before the first build.
	BuildMemory *buildmem.Estimate `json:"build_memory,omitempty"`
}

// Detect inspects the repository and proposes a deployment.
func Detect(src Source) (Result, error) {
	s := &scanner{src: src, lists: map[string][]string{}}
	result, err := s.detectAt("")
	if err == nil && !result.Detected {
		result, err = s.detectMonorepo(result)
	}
	return s.withBuildMemory(result, err)
}

func (s *scanner) withBuildMemory(r Result, err error) (Result, error) {
	if err != nil {
		return r, err
	}
	r.BuildMemory, err = s.estimateBuildMemory(r)
	return r, err
}

// DetectDirectory inspects one directory of the repository, for example a
// monorepo candidate the operator chose.
func DetectDirectory(src Source, dir string) (Result, error) {
	dir = strings.Trim(path.Clean("/"+dir), "/")
	if dir == "" {
		return Detect(src)
	}
	s := &scanner{src: src, lists: map[string][]string{}}
	r, err := s.detectAt(dir)
	if err == nil && !r.Detected {
		r.Reasons = append(r.Reasons, fmt.Sprintf("No supported application was found in %s/; choose the runtime and commands manually", dir))
	}
	return s.withBuildMemory(r, err)
}

// app is a recognised buildable application in one directory.
type app struct {
	runtime, framework, version string
	processes                   []Process
	postgres, redis, clickhouse bool
	pins                        map[string]string
	health, static              string
	reasons                     []string
}

func (a *app) pin(name, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if a.pins == nil {
		a.pins = map[string]string{}
	}
	if _, set := a.pins[name]; !set {
		a.pins[name] = value
	}
}

func (a *app) reason(format string, args ...any) {
	a.reasons = append(a.reasons, fmt.Sprintf(format, args...))
}

type scanner struct {
	src   Source
	lists map[string][]string
	reads int
}

// maxReads bounds how many files one detection reads.
const maxReads = 150

// list returns a directory's entries, caching the answer. Missing
// directories are empty.
func (s *scanner) list(dir string) ([]string, error) {
	dir = strings.Trim(dir, "/")
	if names, ok := s.lists[dir]; ok {
		return names, nil
	}
	names, err := s.src.List(dir)
	if errors.Is(err, fs.ErrNotExist) {
		names, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	s.lists[dir] = names
	return names, nil
}

func (s *scanner) exists(p string) (bool, error) {
	dir, name := path.Split(strings.Trim(p, "/"))
	names, err := s.list(dir)
	if err != nil {
		return false, err
	}
	return slices.Contains(names, name) || slices.Contains(names, name+"/"), nil
}

func (s *scanner) isFile(p string) (bool, error) {
	dir, name := path.Split(strings.Trim(p, "/"))
	names, err := s.list(dir)
	return slices.Contains(names, name), err
}

// read returns a file's contents, or nil when it is missing, too large or
// beyond the read budget.
func (s *scanner) read(p string) ([]byte, error) {
	if ok, err := s.isFile(p); err != nil || !ok {
		return nil, err
	}
	if s.reads >= maxReads {
		return nil, nil
	}
	s.reads++
	data, err := s.src.Read(p)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrTooLarge) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileSize {
		data = data[:MaxFileSize]
	}
	return data, nil
}

func (s *scanner) text(p string) (string, error) {
	data, err := s.read(p)
	return string(data), err
}

// firstFile returns the first of names that exists as a file in dir.
func (s *scanner) firstFile(dir string, names ...string) (string, error) {
	for _, name := range names {
		ok, err := s.isFile(path.Join(dir, name))
		if err != nil {
			return "", err
		}
		if ok {
			return name, nil
		}
	}
	return "", nil
}

// analyze recognises a buildable application in dir.
func (s *scanner) analyze(dir string) (*app, error) {
	for _, recognise := range []func(string) (*app, error){s.rails, s.golang, s.node, s.html} {
		a, err := recognise(dir)
		if err != nil || a != nil {
			if a != nil {
				if err := s.procfile(dir, a); err != nil {
					return nil, err
				}
			}
			return a, err
		}
	}
	return nil, nil
}

// detectAt decides the deployment of the application in dir.
func (s *scanner) detectAt(dir string) (Result, error) {
	r := Result{Mode: ModeNative, SourceDirectory: dir, Reasons: []string{}}
	found, err := s.analyze(dir)
	if err != nil {
		return r, err
	}
	compose, err := s.compose(dir)
	if err != nil {
		return r, err
	}
	dockerfile, err := s.isFile(path.Join(dir, "Dockerfile"))
	if err != nil {
		return r, err
	}
	r.Dockerfile = dockerfile
	if found != nil {
		r.Detected = true
		r.Runtime, r.Framework, r.FrameworkVersion = found.runtime, found.framework, found.version
		r.Processes, r.NeedsPostgres, r.NeedsRedis, r.NeedsClickHouse = found.processes, found.postgres, found.redis, found.clickhouse
		r.Pins, r.HealthPath, r.StaticOutput = found.pins, found.health, found.static
		r.Reasons = append(r.Reasons, found.reasons...)
	}
	if compose != nil {
		r.ComposeFile, r.Services = path.Join(dir, compose.file), compose.services
		var images, built, data []string
		for _, service := range compose.services {
			switch service.Role {
			case "image":
				images = append(images, service.Name)
			case "source":
				built = append(built, service.Name)
			case "data":
				data = append(data, service.Name)
			}
		}
		names := make([]string, 0, len(compose.services))
		for _, service := range compose.services {
			names = append(names, service.Name)
		}
		switch {
		case len(images) > 0:
			r.Mode, r.Detected = ModeContainer, true
			r.Reasons = append(r.Reasons, fmt.Sprintf("%s defines %s (%s) using third-party images: container", r.ComposeFile, plural(len(names), "service"), strings.Join(names, ", ")))
		case len(built) > 1:
			r.Mode, r.Detected = ModeContainer, true
			r.Reasons = append(r.Reasons, fmt.Sprintf("%s builds %s from the repository (%s): container", r.ComposeFile, plural(len(built), "service"), strings.Join(built, ", ")))
		case found == nil && len(compose.services) > 0:
			r.Mode, r.Detected = ModeContainer, true
			r.Reasons = append(r.Reasons, fmt.Sprintf("%s defines %s without a recognised framework: container", r.ComposeFile, plural(len(names), "service")))
		case found != nil && len(data) > 0:
			for _, service := range compose.services {
				if service.Role != "data" {
					continue
				}
				if isClickHouseImage(service.Image) {
					r.NeedsClickHouse = true
				} else if isRedisImage(service.Image) {
					r.NeedsRedis = true
				} else {
					r.NeedsPostgres = true
				}
			}
			r.Reasons = append(r.Reasons, fmt.Sprintf("%s services %s are provided by the host natively", r.ComposeFile, strings.Join(data, ", ")))
		}
	}
	// A container stack manifest is the deployment the repository asks for,
	// even when its own application could also build natively.
	manifest := path.Join(dir, "deploy", "lidza.stack.json")
	if raw, err := s.read(manifest); err != nil {
		return r, err
	} else if raw != nil {
		var stack struct {
			Services []struct {
				Name string `json:"name"`
			} `json:"services"`
		}
		if json.Unmarshal(raw, &stack) == nil && len(stack.Services) > 0 {
			names := make([]string, 0, len(stack.Services))
			for _, service := range stack.Services {
				names = append(names, service.Name)
			}
			r.Mode, r.Detected = ModeContainer, true
			r.StackManifest, r.StackServices = manifest, names
			r.Reasons = append(r.Reasons, fmt.Sprintf("%s defines a container stack of %s (%s): container", manifest, plural(len(names), "service"), strings.Join(names, ", ")))
			return r, nil
		}
		r.Reasons = append(r.Reasons, manifest+" is not a valid stack manifest")
	}
	if r.Mode == ModeContainer && compose != nil {
		r.StackManifestMissing = true
		r.Reasons = append(r.Reasons, "Deploying these services needs a deploy/lidza.stack.json stack manifest in the repository")
	}
	if r.Mode == ModeNative && dockerfile {
		if found != nil {
			r.Reasons = append(r.Reasons, "A Dockerfile is present, but the recognised framework builds natively")
		} else {
			r.Mode, r.Detected = ModeContainer, true
			r.Reasons = append(r.Reasons, "Dockerfile without a recognised framework: container")
		}
	}
	if r.Mode == ModeNative && found != nil {
		r.Reasons = append(r.Reasons, "Builds and runs natively under systemd")
	}
	return r, nil
}

// Directories that never hold the deployable application.
var skippedDirectories = map[string]bool{"node_modules/": true, "vendor/": true, "docs/": true, "doc/": true, "test/": true, "tests/": true, "spec/": true, "examples/": true, "example/": true, "scripts/": true, "script/": true, "tmp/": true, "bin/": true, "fixtures/": true}

// detectMonorepo looks one level down (and inside apps/ and services/) when
// the root holds no application.
func (s *scanner) detectMonorepo(root Result) (Result, error) {
	var dirs []string
	collect := func(parent string) error {
		names, err := s.list(parent)
		if err != nil {
			return err
		}
		for _, name := range names {
			if !strings.HasSuffix(name, "/") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || skippedDirectories[name] {
				continue
			}
			dirs = append(dirs, path.Join(parent, strings.TrimSuffix(name, "/")))
		}
		return nil
	}
	if err := collect(""); err != nil {
		return root, err
	}
	for _, parent := range []string{"apps", "services", "packages"} {
		if slices.Contains(dirs, parent) {
			if err := collect(parent); err != nil {
				return root, err
			}
		}
	}
	if len(dirs) > 40 {
		dirs = dirs[:40]
	}
	var native, container []Result
	for _, dir := range dirs {
		r, err := s.detectAt(dir)
		if err != nil {
			return root, err
		}
		if !r.Detected {
			continue
		}
		if r.Runtime != "" {
			native = append(native, r)
		} else {
			container = append(container, r)
		}
	}
	pick := native
	if len(pick) == 0 {
		pick = container
	}
	switch len(pick) {
	case 0:
		root.Reasons = append(root.Reasons, "No supported application was found; choose the runtime and commands manually")
		return root, nil
	case 1:
		r := pick[0]
		r.Reasons = append([]string{fmt.Sprintf("The repository root has no application; %s/ does", r.SourceDirectory)}, r.Reasons...)
		return r, nil
	}
	for _, r := range pick {
		root.Candidates = append(root.Candidates, r.SourceDirectory)
	}
	root.Reasons = append(root.Reasons, fmt.Sprintf("Several directories contain applications (%s); choose the repository directory to deploy", strings.Join(root.Candidates, ", ")))
	return root, nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
