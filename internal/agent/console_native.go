package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Native applications report through the same console collector as
// container applications. Instead of Docker console output the agent reads
// the journal of each registered service unit (system units as root, user
// units through the owner's manager), converts the entries to the
// collector's timestamped line form and runs the same parsers, cursors,
// outbox and delivery. Static sites have no process and nothing to read.

// nativeConsoleTarget is one native application the control panel assigned
// to the collector, with the release its units currently run.
type nativeConsoleTarget struct {
	id      string
	release Release
}

// nativeConsoleTargetsLocked lists the assigned native applications that
// run services. The caller holds m.mu.
func (m *Manager) nativeConsoleTargetsLocked(assigned map[string]string) []nativeConsoleTarget {
	targets := []nativeConsoleTarget{}
	for id, a := range m.data.NativeApps {
		if assigned[id] == "" || len(a.Services) == 0 {
			continue
		}
		current := m.data.NativeReleases[id].Current
		targets = append(targets, nativeConsoleTarget{id: id, release: Release{ID: current.ID, Commit: current.Commit}})
	}
	slices.SortFunc(targets, func(a, b nativeConsoleTarget) int { return strings.Compare(a.id, b.id) })
	return targets
}

// nativeConsoleLogs reads one unit's journal since a time, as "RFC3339Nano
// message" lines. The read starts at the cursor's second; the parser's
// cursor skips the entries it has already reported.
func (m *Manager) nativeConsoleLogs(ctx context.Context, id, unit string, since time.Time) (string, error) {
	if !nativeUnitPattern.MatchString(unit) {
		return "", errors.New("invalid unit")
	}
	args := []string{"journalctl", "--unit=" + unit, "--no-pager", "--quiet", "--output=json", "--lines=1000", "--since=@" + strconv.FormatInt(since.Unix(), 10)}
	var out string
	var err error
	if m.nativeCreatedSystemUnits(id) {
		out, err = m.nativeRootExec(ctx, args...)
	} else {
		args[1] = "--user-unit=" + unit
		out, err = m.nativeExec(ctx, args...)
	}
	if err != nil {
		return "", err
	}
	return journalConsoleLines(out), nil
}

// journalConsoleLines converts journalctl JSON export entries to the
// collector's line form. A multi-line message becomes one line per text
// line at the entry's time; binary (non-UTF-8) messages are skipped.
func journalConsoleLines(raw string) string {
	var b strings.Builder
	for _, entry := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(entry, "{") {
			continue
		}
		var fields struct {
			Stamp   string          `json:"__REALTIME_TIMESTAMP"`
			Message json.RawMessage `json:"MESSAGE"`
		}
		var message string
		if json.Unmarshal([]byte(entry), &fields) != nil || json.Unmarshal(fields.Message, &message) != nil {
			continue
		}
		micros, err := strconv.ParseInt(fields.Stamp, 10, 64)
		if err != nil || micros <= 0 {
			continue
		}
		at := time.UnixMicro(micros).UTC().Format(time.RFC3339Nano)
		for _, part := range strings.Split(strings.TrimRight(message, "\n"), "\n") {
			b.WriteString(at)
			b.WriteByte(' ')
			b.WriteString(part)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// collectNativeConsole reads the journals of assigned native applications
// into the outbox. It returns the records, the updated cursors (keyed like
// container cursors: incarnation:unit) and the collection warning, if any.
func (m *Manager) collectNativeConsole(ctx context.Context, cfg ErrorReporting, targets []nativeConsoleTarget, cursors map[string]ConsoleCursor, active map[string]bool, room int) ([]ConsoleError, string) {
	records := []ConsoleError{}
	warning := ""
	for _, t := range targets {
		if room-len(records) <= 0 {
			return records, "Error forwarding backlog is full; collection paused until delivery succeeds"
		}
		a, err := m.nativeApp(t.id)
		if err != nil {
			warning = "Native service bindings changed; review the application before collection resumes"
			continue
		}
		secrets := m.nativeSecretsFor(a)
		for _, unit := range a.Services {
			key := cfg.Apps[t.id] + ":" + unit
			active[key] = true
			cursor := cursors[key]
			if cursor.Time.IsZero() {
				cursor.Time = time.Now().UTC().Add(-15 * time.Minute)
			}
			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			logs, err := m.nativeConsoleLogs(readCtx, t.id, unit, cursor.Time)
			cancel()
			if err != nil {
				warning = "Native service log collection failed; existing reports remain available"
				continue
			}
			logs, limited := boundConsoleRead(logs)
			if limited {
				warning = "Service log read reached its size limit; some output may be missing"
			}
			release := t.release
			release.Container = unit
			parsed, next := parseConsoleLines(logs, t.id, secrets, release, cfg.Apps[t.id], cursor, room-len(records))
			records = append(records, parsed...)
			cursors[key] = next
		}
	}
	return records, warning
}
