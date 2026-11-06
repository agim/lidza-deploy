package detect

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"

	"github.com/agim/lidza-deploy/internal/buildmem"
)

// railsAssetGems are the gems whose presence makes a Rails build
// precompile assets (the release profile's assets:precompile step).
var railsAssetGems = regexp.MustCompile(`(?m)^    (propshaft|sprockets|vite_rails|jsbundling-rails|cssbundling-rails|tailwindcss-rails|importmap-rails) \(`)

// estimateBuildMemory estimates the memory of the build the result
// proposes, from the files of its source directory.
func (s *scanner) estimateBuildMemory(r Result) (*buildmem.Estimate, error) {
	if !r.Detected {
		return nil, nil
	}
	dir := r.SourceDirectory
	stack := buildmem.Stack{Mode: r.Mode, Runtime: r.Runtime, Framework: r.Framework}
	if r.Mode == ModeContainer && r.Runtime == "" {
		e := buildmem.EstimateStack(stack)
		return &e, nil
	}
	raw, err := s.read(path.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	var pkg packageJSON
	scripted := raw != nil && json.Unmarshal(raw, &pkg) == nil && pkg.Scripts["build"] != ""
	switch r.Runtime {
	case RuntimeGo:
		// A Līdza build bundles the front end when the application has one.
		stack.Frontend = r.Framework == "lidza" && raw != nil
	case RuntimeRails:
		lock, err := s.text(path.Join(dir, "Gemfile.lock"))
		if err != nil {
			return nil, err
		}
		stack.Assets = railsAssetGems.MatchString(lock)
		stack.Frontend = stack.Assets && scripted
	default:
		stack.Frontend = scripted
	}
	if stack.Rust, err = s.rust(dir); err != nil {
		return nil, err
	}
	e := buildmem.EstimateStack(stack)
	return &e, nil
}

// rust reports whether the build compiles Rust: a Cargo.toml at the root
// or in a pack (packs/<name>/Cargo.toml or packs/<name>/rust/Cargo.toml).
func (s *scanner) rust(dir string) (bool, error) {
	if ok, err := s.isFile(path.Join(dir, "Cargo.toml")); err != nil || ok {
		return ok, err
	}
	packs, err := s.list(path.Join(dir, "packs"))
	if err != nil {
		return false, err
	}
	for _, name := range packs {
		if !strings.HasSuffix(name, "/") || strings.HasPrefix(name, ".") {
			continue
		}
		pack := path.Join(dir, "packs", strings.TrimSuffix(name, "/"))
		for _, file := range []string{path.Join(pack, "Cargo.toml"), path.Join(pack, "rust", "Cargo.toml")} {
			if ok, err := s.isFile(file); err != nil || ok {
				return ok, err
			}
		}
	}
	return false, nil
}
