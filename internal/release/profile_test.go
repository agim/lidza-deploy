package release

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A release must never reuse another release's Vite Ruby build record:
// the SSR build would be skipped and the release would publish without
// its SSR bundle. The record lives inside the release, outside every
// shared directory, and the precompile task does not reinstall packages.
func TestRailsPrecompileKeepsViteBuildRecordInTheRelease(t *testing.T) {
	profile := Rails(RailsOptions{})
	i := slices.IndexFunc(profile.Build, func(s Step) bool { return s.Name == "assets:precompile" })
	if i < 0 {
		t.Fatal("no assets:precompile step")
	}
	env := profile.Build[i].Env
	if !slices.Contains(env, "VITE_RUBY_BUILD_CACHE_DIR="+viteBuildCacheDir) || !slices.Contains(env, "VITE_RUBY_SKIP_ASSETS_PRECOMPILE_INSTALL=true") {
		t.Fatalf("precompile env %v", env)
	}
	if filepath.IsAbs(viteBuildCacheDir) || strings.HasPrefix(viteBuildCacheDir, "public/") {
		t.Fatal("build record must be release-local and not served:", viteBuildCacheDir)
	}
	for _, dir := range profile.Shared.Dirs {
		if viteBuildCacheDir == dir || strings.HasPrefix(viteBuildCacheDir, dir+"/") {
			t.Fatalf("build record %s is under shared %s", viteBuildCacheDir, dir)
		}
	}
}
