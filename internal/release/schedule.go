package release

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Crontab reads and replaces the application owner's crontab.
type Crontab interface {
	Read(ctx context.Context) (string, error)
	Write(ctx context.Context, body string) error
}

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

const (
	scheduleBegin = "# Begin Whenever generated tasks for: "
	scheduleEnd   = "# End Whenever generated tasks for: "
)

// scheduleMarker reports whether line is a Whenever block marker with the
// given prefix and returns the identifier it names. Whenever appends
// " at: <timestamp>" when it writes a block itself.
func scheduleMarker(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	identifier := strings.TrimPrefix(line, prefix)
	if at := strings.LastIndex(identifier, " at: "); at >= 0 {
		identifier = identifier[:at]
	}
	return identifier, identifier != ""
}

// adopted reports whether a block identifier belongs to a block the plan
// takes over. Each entry in adopt is an exact identifier, or a prefix when it
// ends with "/": Whenever's default identifier is the absolute path of the
// schedule file, which differs for every release directory, so a prefix such
// as "<root>/releases/" covers every block an earlier tool left behind.
func adopted(identifier string, adopt []string) bool {
	for _, entry := range adopt {
		if strings.HasSuffix(entry, "/") && strings.HasPrefix(identifier, entry) || identifier == entry {
			return true
		}
	}
	return false
}

// ReplaceScheduleBlock returns existing with the block for identifier
// replaced by entries (or removed when entries is empty). It uses the same
// markers as Whenever's --update-crontab, so a block installed by an earlier
// deploy tool under the same identifier is adopted rather than duplicated.
// Blocks whose identifier matches adopt (see adopted) are removed as well.
// Every other line is kept.
func ReplaceScheduleBlock(existing, identifier, entries string, adopt ...string) (string, error) {
	if !identifierPattern.MatchString(identifier) {
		return "", fmt.Errorf("release: invalid schedule identifier %q", identifier)
	}
	for _, entry := range adopt {
		if entry == "" || entry == "/" || strings.ContainsAny(entry, "\n\r") {
			return "", fmt.Errorf("release: invalid adopted schedule identifier %q", entry)
		}
	}
	var kept []string
	inside, found := "", false
	for _, line := range strings.Split(strings.TrimRight(existing, "\n"), "\n") {
		if inside != "" {
			if name, ok := scheduleMarker(line, scheduleEnd); ok && name == inside {
				inside = ""
			}
			continue
		}
		if name, ok := scheduleMarker(line, scheduleBegin); ok && (name == identifier || adopted(name, adopt)) {
			if name == identifier {
				if found {
					return "", fmt.Errorf("release: crontab has two schedule blocks for %s", identifier)
				}
				found = true
			}
			inside = name
			continue
		}
		kept = append(kept, line)
	}
	if inside != "" {
		return "", fmt.Errorf("release: crontab schedule block for %s is not closed", inside)
	}
	for len(kept) > 0 && kept[len(kept)-1] == "" {
		kept = kept[:len(kept)-1]
	}
	body := strings.Join(kept, "\n")
	entries = strings.TrimSpace(entries)
	if entries != "" {
		if body != "" {
			body += "\n\n"
		}
		body += scheduleBegin + identifier + "\n" + entries + "\n" + scheduleEnd + identifier
	}
	if body == "" {
		return "", nil
	}
	return body + "\n", nil
}

// SyncSchedule installs entries as the application's crontab block and
// removes adopted legacy blocks. It writes only when the crontab would change.
func SyncSchedule(ctx context.Context, tab Crontab, identifier, entries string, adopt ...string) (bool, error) {
	existing, err := tab.Read(ctx)
	if err != nil {
		return false, err
	}
	next, err := ReplaceScheduleBlock(existing, identifier, entries, adopt...)
	if err != nil {
		return false, err
	}
	if next == existing || strings.TrimSpace(next) == strings.TrimSpace(existing) {
		return false, nil
	}
	return true, tab.Write(ctx, next)
}

// wheneverSet builds the argument of Whenever's --set flag. Whenever splits
// it on "&" and "=", so neither may appear in a key or value, and the
// profile owns environment and path.
func wheneverSet(environment, path string, extra map[string]string) (string, error) {
	pairs := []string{"environment=" + environment, "path=" + path}
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sortStrings(keys)
	for _, key := range keys {
		value := extra[key]
		switch {
		case key == "environment" || key == "path":
			return "", fmt.Errorf("release: whenever variable %q is set by the profile", key)
		case !identifierPattern.MatchString(key):
			return "", fmt.Errorf("release: invalid whenever variable name %q", key)
		case strings.ContainsAny(value, "&=\n\r") || strings.ContainsAny(environment+path, "&=\n\r"):
			return "", fmt.Errorf("release: whenever variable %q must not contain '&', '=' or newlines", key)
		}
		pairs = append(pairs, key+"="+value)
	}
	return strings.Join(pairs, "&"), nil
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
