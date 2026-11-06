package agent

import (
	"slices"
	"testing"

	"github.com/agim/lidza-deploy/internal/release"
)

func TestRubyToolchainHealthClassifiesBundlerStates(t *testing.T) {
	host := func(pinned string, installed ...string) release.RubyToolchain {
		return release.RubyToolchain{Ruby: "3.4.4", RubyGems: "4.0.23", DefaultBundler: "4.0.23", Pinned: pinned, Installed: installed}
	}
	warnings, notes := rubyToolchainHealth([]release.RubyToolchain{
		host("2.6.9", "2.6.9"), host("2.6.9", "2.7.1"), host("2.6.9"), host("4.0.10"),
		host("4.1.0"), // newer than the host ships
		{Ruby: "3.3.6", RubyGems: "3.6.9", Pinned: "2.6.9"},  // matches its RubyGems
		{Ruby: "3.3.6", RubyGems: "3.6.9", Pinned: "2.5.23"}, // older on another Ruby
	})
	// Older pins are one note per Ruby: releases already use the host's Bundler.
	wantNotes := []string{
		"Rails releases on Ruby 3.3.6 use the host's Bundler 2.6.9; 1 application pins older versions in their Gemfile.lock",
		"Rails releases on Ruby 3.4.4 use the host's Bundler 4.0.23; 3 applications pin older versions in their Gemfile.lock",
	}
	if !slices.Equal(notes, wantNotes) {
		t.Fatalf("notes:\n%q", notes)
	}
	// Only actionable states warn: stale installed versions and a newer pin.
	wantWarnings := []string{
		"Ruby 3.4.4: Bundler 2.7.1, 2.6.9 installed beside RubyGems 4.0.23's default Bundler 4.0.23; older Bundler versions print constant redefinition warnings on this RubyGems (remove them with the server's Remove unused Bundler versions action)",
		"Ruby 3.4.4: Bundler 4.1.0 pinned by 1 application is newer than the host's Bundler 4.0.23 (RubyGems 4.0.23); releases cannot use it until the host RubyGems is updated or the Gemfile.lock pins Bundler 4.0.23",
	}
	if !slices.Equal(warnings, wantWarnings) {
		t.Fatalf("warnings:\n%q", warnings)
	}
	for _, w := range warnings {
		if slices.Contains(notes, w) {
			t.Fatal("a note repeated as a warning", w)
		}
	}
	if warnings, notes := rubyToolchainHealth([]release.RubyToolchain{host("4.0.10")}); len(warnings)+len(notes) != 0 {
		t.Fatalf("matching toolchain reported: %q %q", warnings, notes)
	}
	// Older pins alone: a note and no warning.
	if warnings, notes := rubyToolchainHealth([]release.RubyToolchain{host("2.6.9"), host("2.5.0")}); len(warnings) != 0 || len(notes) != 1 {
		t.Fatalf("older pins warned: %q %q", warnings, notes)
	}
}

func TestNativeRubyHealthOnlyCoversRegisteredApplications(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.data.NativeApps = map[string]NativeApplication{"shop": {ID: "shop"}}
	m.data.NativeRuby = map[string]release.RubyToolchain{
		"shop": {Ruby: "3.4.4", RubyGems: "4.0.23", Pinned: "2.6.9"},
		"gone": {Ruby: "3.4.4", RubyGems: "4.0.23", Pinned: "2.5.0", Installed: []string{"2.5.0"}},
	}
	warnings, notes := m.nativeRubyHealth()
	if len(warnings) != 0 || !slices.Equal(notes, []string{"Rails releases on Ruby 3.4.4 use the host's Bundler 4.0.23; 1 application pins older versions in their Gemfile.lock"}) {
		t.Fatalf("warnings %q notes %q", warnings, notes)
	}
	m.recordNativeReleaseLocked("shop", release.Layout{Root: t.TempDir()}, release.Result{Release: "r2", Ruby: &release.RubyToolchain{Ruby: "3.4.4", RubyGems: "4.0.23", Pinned: "4.0.20"}})
	if warnings, notes := m.nativeRubyHealth(); len(warnings)+len(notes) != 0 {
		t.Fatalf("a release with a matching pin still reported: %q %q", warnings, notes)
	}
}
