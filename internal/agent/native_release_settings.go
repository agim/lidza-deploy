package agent

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

// Settings for applications deployed by the release engine. The application
// environment lives in the policy's environment files on the host, which the
// owner's service units and scheduled jobs already load; the panel edits those
// files directly and the next deploy applies them. The branch is kept as a
// settings override that the release policy then follows. Hostnames stay
// with the existing web server configuration.

var (
	nativeReleaseBareValue   = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,=!-]*$`)
	nativeReleaseBranchValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

// nativeReleaseHostKey reports environment keys the host owns: they are shown
// but never edited from the panel.
func nativeReleaseHostKey(key string) bool {
	if key == "LIDZA_MASTER_KEY" {
		return false // the application's own sealed-credentials key
	}
	return nativeReleaseReservedEnvironment(key) || key == "PORT" || key == "STORAGE_DIR" || strings.HasPrefix(key, "LIDZA_")
}

// nativeReleaseEnvValues reads the policy's environment files in override order.
func nativeReleaseEnvValues(policy NativeReleasePolicy) (map[string]string, error) {
	values := map[string]string{}
	for _, path := range policy.EnvFiles {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("environment file %s is unreadable", path)
		}
		if err := parseNativeLiteralEnvironment(data, values); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return values, nil
}

func nativeReleaseEnvLine(key, value string) (string, error) {
	switch {
	case nativeReleaseBareValue.MatchString(value):
		return key + "=" + value, nil
	case strings.ContainsAny(value, "'\r\n\x00"):
		return "", fmt.Errorf("%s cannot contain single quotes or line breaks", key)
	default:
		return key + "='" + value + "'", nil
	}
}

// rewriteNativeReleaseEnvFiles applies changes (nil deletes) to the policy's
// environment files. Existing assignments are updated in place in every file
// that defines them; new keys are appended to the last file, which has the
// highest precedence. Each file is replaced atomically with its owner and
// mode preserved, and re-read to prove it still parses.
func rewriteNativeReleaseEnvFiles(files []string, changes map[string]*string) error {
	if len(files) == 0 {
		return errors.New("the release policy names no environment file")
	}
	lines := map[string]string{}
	for key, value := range changes {
		if value == nil {
			continue
		}
		line, err := nativeReleaseEnvLine(key, *value)
		if err != nil {
			return err
		}
		lines[key] = line
	}
	written := map[string]bool{}
	for index, path := range files {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && index == len(files)-1 {
			info = nil
		} else if err != nil {
			return fmt.Errorf("environment file %s is unavailable", path)
		} else if !info.Mode().IsRegular() || info.Size() > 128<<10 {
			return fmt.Errorf("environment file %s must be a regular file under 128 KiB", path)
		}
		var original []byte
		if info != nil {
			if original, err = os.ReadFile(path); err != nil {
				return err
			}
		}
		var out []string
		changed := false
		for _, line := range strings.Split(strings.TrimRight(string(original), "\n"), "\n") {
			trimmed := strings.TrimSpace(line)
			key, _, ok := strings.Cut(strings.TrimPrefix(trimmed, "export "), "=")
			if trimmed == "" || strings.HasPrefix(trimmed, "#") || !ok {
				if line != "" || len(out) > 0 {
					out = append(out, line)
				}
				continue
			}
			value, change := changes[key]
			switch {
			case !change:
				out = append(out, line)
			case value == nil:
				changed = true
			default:
				out = append(out, lines[key])
				written[key] = true
				changed = true
			}
		}
		if index == len(files)-1 {
			keys := slices.Sorted(maps.Keys(lines))
			for _, key := range keys {
				if !written[key] {
					out = append(out, lines[key])
					written[key] = true
					changed = true
				}
			}
		}
		if !changed {
			continue
		}
		body := []byte(strings.Join(out, "\n") + "\n")
		if err := parseNativeLiteralEnvironment(body, map[string]string{}); err != nil {
			return fmt.Errorf("refusing to write %s: %w", path, err)
		}
		if err := replaceNativeReleaseFile(path, body, info); err != nil {
			return err
		}
	}
	return nil
}

func replaceNativeReleaseFile(path string, body []byte, info os.FileInfo) error {
	mode, uid, gid := os.FileMode(0o640), -1, -1
	if info != nil {
		mode = info.Mode().Perm()
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(stat.Uid), int(stat.Gid)
		}
	} else if stat, err := os.Stat(filepath.Dir(path)); err == nil {
		if sys, ok := stat.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(sys.Uid), int(sys.Gid)
		}
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".env-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(body); err == nil {
		err = temp.Chmod(mode)
	}
	if err == nil && uid >= 0 && os.Geteuid() == 0 {
		err = temp.Chown(uid, gid)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// nativeReleaseSettings fills settings for a release-managed application.
func (m *Manager) nativeReleaseSettings(settings *Settings, policy NativeReleasePolicy, override NativeSettingsOverride) error {
	values, err := nativeReleaseEnvValues(policy)
	if err != nil {
		return err
	}
	kinds := normalizeEnvironmentKinds(values, override.Kinds)
	keys := slices.Sorted(maps.Keys(values))
	settings.EnvKeys = keys
	settings.Environment = settings.Environment[:0]
	settings.ReadonlyEnvKeys = nil
	for _, key := range keys {
		value, kind := values[key], kinds[key]
		entry := EnvironmentEntry{Key: key, Kind: kind, Source: "application environment file"}
		if kind == EnvironmentCredential {
			entry.Length = len([]rune(value))
		} else {
			entry.Value = value
		}
		settings.Environment = append(settings.Environment, entry)
		if nativeReleaseHostKey(key) {
			settings.ReadonlyEnvKeys = append(settings.ReadonlyEnvKeys, key)
		}
	}
	settings.Branch = policy.Branch
	settings.ReleaseRuntime = policy.Runtime
	settings.Toolchains = "account"
	if policy.ManagedToolchains() {
		settings.Toolchains = "managed"
	}
	settings.SettingsApplyOnDeploy = true
	settings.BackupBeforeDeploy = true
	return nil
}

// patchNativeReleaseSettings saves environment and branch changes for a
// release-managed application.
func (m *Manager) patchNativeReleaseSettings(a NativeApplication, p SettingsPatch) error {
	_, policy, err := m.nativeReleasePolicy(a.ID)
	if err != nil {
		return err
	}
	if p.Domain != "" && p.Domain != a.Domain || p.AdditionalDomains != nil && !slices.Equal(*p.AdditionalDomains, a.AdditionalDomains) || p.CustomerHosts != nil && *p.CustomerHosts {
		return errors.New("hostnames for this application are served by the existing web server configuration and cannot be changed here yet")
	}
	if p.Build != nil && p.Build.SourceDirectory != "" || p.RuntimeProfile != nil && *p.RuntimeProfile != "" || p.Astro != nil && *p.Astro != (AstroConfig{}) {
		return errors.New("runtime changes are not available for this application")
	}
	if p.BackupBeforeDeploy != nil && !*p.BackupBeforeDeploy {
		return errors.New("deploys of this application always take a verified backup first")
	}
	if p.Branch != "" && !nativeReleaseBranchValue.MatchString(p.Branch) {
		return errors.New("invalid branch name")
	}
	if p.Toolchains != nil {
		m.mu.Lock()
		_, created := m.data.NativeCreated[a.ID]
		m.mu.Unlock()
		switch {
		case *p.Toolchains != "managed" && *p.Toolchains != "account":
			return errors.New("toolchains must be managed or account")
		case created && *p.Toolchains == "account":
			return errors.New("applications created here have no version manager of their own; they use managed toolchains")
		}
	}
	for key, value := range p.EnvChanges {
		if !envPattern.MatchString(key) {
			return fmt.Errorf("invalid environment variable name %q", key)
		}
		if nativeReleaseHostKey(key) {
			return fmt.Errorf("%s is managed by the host", key)
		}
		if value != nil {
			if _, err := nativeReleaseEnvLine(key, *value); err != nil {
				return err
			}
		}
	}
	if p.FromDefaults {
		values := map[string]string{}
		for key, value := range p.EnvChanges {
			if value == nil {
				return errors.New("defaults cannot delete variables")
			}
			values[key] = *value
		}
		if err := ValidateRuntimeDefaults(nativeReleaseDefaultsRuntime(policy), values); err != nil {
			return err
		}
	}
	m.mu.Lock()
	if m.nativeOperations[a.ID] {
		m.mu.Unlock()
		return errors.New("a deployment of this application is running; save settings after it finishes")
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[a.ID] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.nativeOperations, a.ID)
		m.mu.Unlock()
	}()
	changes := p.EnvChanges
	if p.FromDefaults && p.OnlyMissing {
		// Workspace defaults fill gaps only: any key already assigned in an
		// environment file, even to an empty value, is preserved.
		current, err := nativeReleaseEnvValues(policy)
		if err != nil {
			return err
		}
		changes = map[string]*string{}
		for key, value := range p.EnvChanges {
			if _, exists := current[key]; !exists {
				changes[key] = value
			}
		}
	}
	if len(changes) > 0 {
		if err := rewriteNativeReleaseEnvFiles(policy.EnvFiles, changes); err != nil {
			return err
		}
	}
	values, err := nativeReleaseEnvValues(policy)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.data.NativeSettings[a.ID]
	next := old
	next.Kinds = maps.Clone(old.Kinds)
	if next.Kinds == nil {
		next.Kinds = map[string]string{}
	}
	for key, kind := range p.EnvKindChanges {
		if _, ok := values[key]; !ok {
			return errors.New("environment classification references a missing variable")
		}
		if kind != EnvironmentSetting && kind != EnvironmentCredential {
			return errors.New("invalid environment classification")
		}
		next.Kinds[key] = kind
	}
	for key := range next.Kinds {
		if _, ok := values[key]; !ok {
			delete(next.Kinds, key)
		}
	}
	if p.Branch != "" {
		next.Branch = p.Branch
		if p.Branch == a.Branch {
			next.Branch = ""
		}
	}
	if p.Toolchains != nil {
		// Switching to managed toolchains installs the pinned Go, Node.js
		// and Ruby on the next deploy, which then builds with them; nothing
		// changes for the serving release until then.
		next.Toolchains = *p.Toolchains
	}
	if m.data.NativeSettings == nil {
		m.data.NativeSettings = map[string]NativeSettingsOverride{}
	}
	m.data.NativeSettings[a.ID] = next
	if err := m.save(); err != nil {
		m.data.NativeSettings[a.ID] = old
		return err
	}
	return nil
}

// nativeReleaseDefaultsRuntime names the workspace defaults set that applies
// to a release-managed application.
func nativeReleaseDefaultsRuntime(policy NativeReleasePolicy) string {
	switch policy.Runtime {
	case "node":
		return "node"
	case "go":
		return "go"
	case "static":
		return "astro"
	}
	return "rails"
}

// nativeReleaseCredential returns one credential value for an authorized reveal.
func (m *Manager) nativeReleaseCredential(a NativeApplication, key string) (string, error) {
	_, policy, err := m.nativeReleasePolicy(a.ID)
	if err != nil {
		return "", err
	}
	values, err := nativeReleaseEnvValues(policy)
	if err != nil {
		return "", err
	}
	value, ok := values[key]
	if !ok || normalizeEnvironmentKinds(values, m.nativeOverride(a.ID).Kinds)[key] != EnvironmentCredential {
		return "", errors.New("no revealable application credential")
	}
	return value, nil
}
