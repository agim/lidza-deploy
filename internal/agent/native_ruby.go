package agent

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/agim/lidza-deploy/internal/release"
)

// Server health about Ruby toolchains, from what each native Rails
// application's latest release found (release.RubyToolchain). Nothing is
// installed or removed automatically.
//
// A lockfile that pins a Bundler older than the host's RubyGems pairs with
// needs nothing from the operator: the release engine uses the host's default
// Bundler for such releases, so that is a note. Warnings are the actionable
// states: older Bundler versions still installed beside the default one
// (removed with the server's "Remove unused Bundler versions" action), and a
// pinned Bundler newer than the host's RubyGems ships, which the release
// cannot activate.

// nativeRubyHealth reports the warnings and notes of registered applications'
// Ruby toolchains.
func (m *Manager) nativeRubyHealth() (warnings, notes []string) {
	m.mu.Lock()
	records := make([]release.RubyToolchain, 0, len(m.data.NativeRuby))
	for id, record := range m.data.NativeRuby {
		if _, registered := m.data.NativeApps[id]; registered {
			records = append(records, record)
		}
	}
	m.mu.Unlock()
	return rubyToolchainHealth(records)
}

func rubyToolchainHealth(records []release.RubyToolchain) (warnings, notes []string) {
	type host struct{ ruby, rubygems, bundler string }
	older := map[host]int{}
	newer := map[host]map[string]int{}
	installed := map[host][]string{}
	for _, r := range records {
		h := host{r.Ruby, r.RubyGems, cmp.Or(r.DefaultBundler, release.PairedBundler(r.RubyGems))}
		if h.ruby == "" || h.rubygems == "" || h.bundler == "" {
			continue
		}
		switch {
		case release.ChooseBundler(h.rubygems, h.bundler, r.Pinned) != "":
			older[h]++
		case release.NewerBundler(h.rubygems, h.bundler, r.Pinned):
			if newer[h] == nil {
				newer[h] = map[string]int{}
			}
			newer[h][r.Pinned]++
		}
		for _, version := range release.StaleBundlers(h.bundler, r.Installed) {
			if !slices.Contains(installed[h], version) {
				installed[h] = append(installed[h], version)
			}
		}
	}
	for h, count := range older {
		notes = append(notes, fmt.Sprintf("Rails releases on Ruby %s use the host's Bundler %s; %s older versions in their Gemfile.lock",
			h.ruby, h.bundler, applicationsPin(count)))
	}
	for h, versions := range newer {
		for version, count := range versions {
			warnings = append(warnings, fmt.Sprintf("Ruby %s: Bundler %s pinned by %s is newer than the host's Bundler %s (RubyGems %s); releases cannot use it until the host RubyGems is updated or the Gemfile.lock pins Bundler %s",
				h.ruby, version, applications(count), h.bundler, h.rubygems, h.bundler))
		}
	}
	for h, versions := range installed {
		versions = release.StaleBundlers(h.bundler, versions) // newest first
		warnings = append(warnings, fmt.Sprintf("Ruby %s: Bundler %s installed beside RubyGems %s's default Bundler %s; older Bundler versions print constant redefinition warnings on this RubyGems (remove them with the server's Remove unused Bundler versions action)",
			h.ruby, strings.Join(versions, ", "), h.rubygems, h.bundler))
	}
	slices.Sort(warnings)
	slices.Sort(notes)
	return warnings, notes
}

func applications(count int) string {
	if count == 1 {
		return "1 application"
	}
	return fmt.Sprintf("%d applications", count)
}

func applicationsPin(count int) string {
	if count == 1 {
		return "1 application pins"
	}
	return fmt.Sprintf("%d applications pin", count)
}
