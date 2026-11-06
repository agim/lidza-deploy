package detect

import (
	"strings"
	"testing"
)

func TestBuildMemoryEstimatePerStack(t *testing.T) {
	lidzaMod := "module example.com/shop\n\ngo 1.24\n\nrequire github.com/agim/lidza v0.9.0\n"
	cases := []struct {
		name  string
		files map[string]string
		mib   int
		basis string
	}{
		{"go", map[string]string{"go.mod": "module example.com/api\n\ngo 1.24\n", "main.go": "package main"}, 1024, "Go build"},
		{"lidza", map[string]string{"go.mod": lidzaMod}, 1536, "Līdza build"},
		{"lidza front end", map[string]string{"go.mod": lidzaMod, "package.json": `{"devDependencies":{"vite":"6"}}`}, 2560, "front end"},
		{"lidza rust packs", map[string]string{"go.mod": lidzaMod, "package.json": `{}`, "packs/search/rust/Cargo.toml": "[package]"}, 3584, "Rust packs"},
		{"rails assets", map[string]string{"Gemfile": "gem 'rails'", "Gemfile.lock": "GEM\n  specs:\n    propshaft (1.0.0)\n    rails (7.1.3)\n", "config/application.rb": ""}, 1536, "asset precompilation"},
		{"rails", map[string]string{"Gemfile": "gem 'rails'", "Gemfile.lock": "GEM\n  specs:\n    rails (7.1.3)\n", "config/application.rb": ""}, 1024, "bundle install"},
		{"vite", map[string]string{"package.json": `{"scripts":{"build":"vite build"},"devDependencies":{"vite":"6"}}`, "index.html": ""}, 2048, "JavaScript build"},
		{"containers", map[string]string{"Dockerfile": "FROM scratch"}, 2048, "Container image build"},
	}
	for _, c := range cases {
		r := detect(t, c.files)
		if r.BuildMemory == nil || r.BuildMemory.MiB != c.mib || !strings.Contains(strings.Join(r.BuildMemory.Basis, " "), c.basis) {
			t.Errorf("%s: %+v (runtime %s, framework %s), want %d MiB mentioning %q", c.name, r.BuildMemory, r.Runtime, r.Framework, c.mib, c.basis)
		}
	}
	if r := detect(t, map[string]string{"README.md": "hello"}); r.BuildMemory != nil {
		t.Fatal("no estimate without a detected application", r.BuildMemory)
	}
	// A monorepo candidate is estimated from its own directory.
	r, err := DetectDirectory(Files(map[string]string{"apps/web/package.json": `{"scripts":{"build":"next build"},"dependencies":{"next":"15"}}`, "apps/api/go.mod": "module x\n"}), "apps/web")
	if err != nil || r.BuildMemory == nil || r.BuildMemory.MiB != 2048 {
		t.Fatal(r.BuildMemory, err)
	}
}
